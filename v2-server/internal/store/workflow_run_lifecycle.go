package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrWorkflowScopeExists reports a Loop round that is already open (or numbered twice).
	ErrWorkflowScopeExists = errors.New("workflow scope exists")
	// ErrWorkflowScopeNotFound reports a round scope that is missing or owned elsewhere.
	ErrWorkflowScopeNotFound = errors.New("workflow scope not found")
	// ErrWorkflowScopeState reports a round transition its current status does not allow.
	ErrWorkflowScopeState = errors.New("workflow scope is not in a transitionable state")
)

// WorkflowNodeResult carries one attempt's public answer and the payload the domain wants stored.
type WorkflowNodeResult struct {
	Output  *string
	Payload []byte
}

// WorkflowNodeFailure is what the bridge records when one attempt fails. The error text is public;
// the detail payload is built by the domain, which needs the attempt number the store assigns.
type WorkflowNodeFailure struct {
	Error  string
	Output *string
}

// lockWorkflowRun serializes every transition of one run on its row. Deleting a run takes the same
// lock, so no transition can add or resurrect work under a row that is being removed.
func lockWorkflowRun(ctx context.Context, tx pgx.Tx, tenant, runID string) error {
	var locked bool
	err := tx.QueryRow(ctx, `
		SELECT true FROM v2_workflow_runs
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenant, runID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkflowRunNotFound
	}
	if err != nil {
		return fmt.Errorf("lock workflow run: %w", err)
	}
	return nil
}

// lockWorkflowNodeRun reads one live attempt for update; callers hold the run lock already.
func lockWorkflowNodeRun(ctx context.Context, tx pgx.Tx, tenant, runID, nodeRunID string) (WorkflowNodeRun, error) {
	return scanNodeRun(tx.QueryRow(ctx, `
		SELECT `+workflowNodeRunColumns+` FROM v2_workflow_node_runs
		WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND deleted_at IS NULL FOR UPDATE`,
		tenant, runID, nodeRunID))
}

// BindWorkflowNodeSession records the execution session that serves one node attempt. Binding is
// one-time while the attempt runs; repeating the same session is a no-op, and a different session is
// refused so a retry cannot silently inherit an earlier transcript.
func (s *Store) BindWorkflowNodeSession(ctx context.Context, tenant, runID, nodeRunID, sessionID string) (WorkflowNodeRun, error) {
	var node WorkflowNodeRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		current, err := lockWorkflowNodeRun(ctx, tx, tenant, runID, nodeRunID)
		if err != nil {
			return err
		}
		if current.SessionID != nil {
			if *current.SessionID == sessionID {
				node = current
				return nil
			}
			return ErrWorkflowNodeRunState
		}
		if current.Status != WorkflowStatusRunning {
			return ErrWorkflowNodeRunState
		}
		node, err = scanNodeRun(tx.QueryRow(ctx, `
			UPDATE v2_workflow_node_runs SET session_id = $4, updated_at = now()
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND deleted_at IS NULL
			RETURNING `+workflowNodeRunColumns, tenant, runID, nodeRunID, sessionID))
		return err
	})
	return node, err
}

// CompleteWorkflowNodeRun records a successful attempt with its public answer and optional payload.
// A late completion against an attempt that already finished is a no-op returning the current row,
// because a retried or cancelled attempt may still race with its own result.
func (s *Store) CompleteWorkflowNodeRun(ctx context.Context, tenant, runID, nodeRunID string, in WorkflowNodeResult) (WorkflowNodeRun, error) {
	var node WorkflowNodeRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		current, err := lockWorkflowNodeRun(ctx, tx, tenant, runID, nodeRunID)
		if err != nil {
			return err
		}
		if current.Status == WorkflowStatusSucceeded || current.Status == WorkflowStatusCancelled {
			node = current
			return nil
		}
		if current.Status != WorkflowStatusRunning && current.Status != WorkflowStatusPending {
			return ErrWorkflowNodeRunState
		}
		payload := current.Payload
		if in.Payload != nil {
			payload = in.Payload
		}
		node, err = scanNodeRun(tx.QueryRow(ctx, `
			UPDATE v2_workflow_node_runs
			SET status = 'succeeded', output = COALESCE($4, output), payload = $5,
				finished_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND deleted_at IS NULL
			RETURNING `+workflowNodeRunColumns, tenant, runID, nodeRunID, in.Output, payload))
		return err
	})
	return node, err
}

// FailWorkflowNodeRun records a failed attempt and returns the attempt number it assigned. The
// caller builds the failure detail (desktop keeps it under payload.error_detail) inside the
// transaction, so the number it writes is the number this row actually received. Counting matches
// desktop: soft-deleted rows of the same node and iteration; inside a Loop round only that round.
func (s *Store) FailWorkflowNodeRun(
	ctx context.Context,
	tenant, runID, nodeRunID string,
	failure WorkflowNodeFailure,
	buildPayload func(attempt int) ([]byte, error),
) (WorkflowNodeRun, error) {
	var node WorkflowNodeRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		current, err := lockWorkflowNodeRun(ctx, tx, tenant, runID, nodeRunID)
		if err != nil {
			return err
		}
		if current.Status != WorkflowStatusRunning {
			return ErrWorkflowNodeRunState
		}
		var attempt int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*)::int + 1 FROM v2_workflow_node_runs failed
			WHERE failed.run_id = $1 AND failed.node_id = $2
				AND failed.iteration IS NOT DISTINCT FROM $3
				AND failed.deleted_at IS NOT NULL
				AND (failed.scope_id = $4 OR NOT EXISTS (
					SELECT 1 FROM v2_workflow_scopes round
					WHERE round.id = $4 AND round.parent_loop_node_run_id IS NOT NULL))`,
			runID, current.NodeID, current.Iteration, current.ScopeID).Scan(&attempt); err != nil {
			return fmt.Errorf("count attempts: %w", err)
		}
		payload := current.Payload
		if buildPayload != nil {
			payload, err = buildPayload(attempt)
			if err != nil {
				return err
			}
		}
		node, err = scanNodeRun(tx.QueryRow(ctx, `
			UPDATE v2_workflow_node_runs
			SET status = 'failed', error = $4, output = COALESCE($5, output), payload = $6,
				finished_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND deleted_at IS NULL
			RETURNING `+workflowNodeRunColumns, tenant, runID, nodeRunID, failure.Error, failure.Output, payload))
		return err
	})
	return node, err
}

// FinishWorkflowRun closes a run that reached a terminal outcome. Only succeeded and failed are
// accepted here; cancellation has its own transition and any other status is a caller bug.
func (s *Store) FinishWorkflowRun(ctx context.Context, tenant, runID, status string, output, runError *string) (WorkflowRun, error) {
	if status != WorkflowStatusSucceeded && status != WorkflowStatusFailed {
		return WorkflowRun{}, fmt.Errorf("finish workflow run: unsupported status %q", status)
	}
	var run WorkflowRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		current, err := readWorkflowRunStatus(ctx, tx, tenant, runID)
		if err != nil {
			return err
		}
		if current != WorkflowStatusPending && current != WorkflowStatusRunning {
			return ErrWorkflowRunState
		}
		run, err = scanWorkflowRun(tx.QueryRow(ctx, `
			UPDATE v2_workflow_runs
			SET status = $3, output = $4, error = $5, state = '{"current_nodes":[]}'::jsonb,
				finished_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+workflowRunColumns, tenant, runID, status, output, runError))
		return err
	})
	return run, err
}

// CancelWorkflowRun closes a run and every non-terminal attempt as cancelled once the bridge has
// confirmed the stop. Cancelling an already cancelled run is idempotent; a succeeded or failed run
// is refused because its outcome is authoritative.
func (s *Store) CancelWorkflowRun(ctx context.Context, tenant, runID string) (WorkflowRun, error) {
	var run WorkflowRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		status, err := readWorkflowRunStatus(ctx, tx, tenant, runID)
		if err != nil {
			return err
		}
		switch status {
		case WorkflowStatusCancelled:
			run, err = scanWorkflowRun(tx.QueryRow(ctx, `
				SELECT `+workflowRunColumns+` FROM v2_workflow_runs
				WHERE tenant_id = $1 AND id = $2`, tenant, runID))
			return err
		case WorkflowStatusPending, WorkflowStatusRunning:
		default:
			return ErrWorkflowRunState
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs
			SET status = 'cancelled', finished_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND run_id = $2
				AND status IN ('pending', 'running') AND deleted_at IS NULL`, tenant, runID); err != nil {
			return fmt.Errorf("cancel node runs: %w", err)
		}
		run, err = scanWorkflowRun(tx.QueryRow(ctx, `
			UPDATE v2_workflow_runs
			SET status = 'cancelled', state = '{"current_nodes":[]}'::jsonb,
				finished_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+workflowRunColumns, tenant, runID))
		return err
	})
	return run, err
}

func readWorkflowRunStatus(ctx context.Context, tx pgx.Tx, tenant, runID string) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `
		SELECT status FROM v2_workflow_runs WHERE tenant_id = $1 AND id = $2`, tenant, runID).Scan(&status)
	if err != nil {
		return "", fmt.Errorf("read run status: %w", err)
	}
	return status, nil
}

// StartWorkflowLoopRound opens one Loop iteration. The parent must be a live Loop node of the same
// run, and the partial unique index keeps a Loop from having two active rounds at once.
func (s *Store) StartWorkflowLoopRound(ctx context.Context, tenant, runID, parentLoopNodeRunID string, roundIndex int, state []byte) (WorkflowScope, error) {
	var scope WorkflowScope
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		var parentType string
		err := tx.QueryRow(ctx, `
			SELECT node_type FROM v2_workflow_node_runs
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND deleted_at IS NULL FOR UPDATE`,
			tenant, runID, parentLoopNodeRunID).Scan(&parentType)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowNodeRunNotFound
		}
		if err != nil {
			return fmt.Errorf("lock loop node: %w", err)
		}
		if parentType != "loop" {
			return ErrWorkflowNodeRunState
		}
		scopeID := fmt.Sprintf("scope:%s:%d", parentLoopNodeRunID, roundIndex)
		scope, err = scanWorkflowScope(tx.QueryRow(ctx, `
			INSERT INTO v2_workflow_scopes
				(id, tenant_id, run_id, parent_loop_node_run_id, round_index, status, state)
			VALUES ($1, $2, $3, $4, $5, 'running', $6)
			RETURNING `+workflowScopeColumns,
			scopeID, tenant, runID, parentLoopNodeRunID, roundIndex, state))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrWorkflowScopeExists
		}
		return err
	})
	return scope, err
}

// SettleWorkflowLoopRound closes one Loop round with its outcome. Settling a round with the status it
// already has is a no-op so a duplicated callback cannot rewrite history; a different status after
// settlement is refused.
func (s *Store) SettleWorkflowLoopRound(ctx context.Context, tenant, runID, scopeID, status string, state []byte) (WorkflowScope, error) {
	var scope WorkflowScope
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkflowRun(ctx, tx, tenant, runID); err != nil {
			return err
		}
		current, err := scanWorkflowScope(tx.QueryRow(ctx, `
			SELECT `+workflowScopeColumns+` FROM v2_workflow_scopes
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND parent_loop_node_run_id IS NOT NULL
			FOR UPDATE`, tenant, runID, scopeID))
		if err != nil {
			return err
		}
		if current.Status != nil && *current.Status != WorkflowStatusPending && *current.Status != WorkflowStatusRunning {
			if *current.Status == status {
				scope = current
				return nil
			}
			return ErrWorkflowScopeState
		}
		if state == nil {
			// A round always carries its state (the schema enforces it); an omitted update keeps the
			// accumulated ledger instead of clearing it.
			state = current.State
		}
		scope, err = scanWorkflowScope(tx.QueryRow(ctx, `
			UPDATE v2_workflow_scopes SET status = $4, state = $5, updated_at = now()
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3
			RETURNING `+workflowScopeColumns, tenant, runID, scopeID, status, state))
		return err
	})
	return scope, err
}

func scanWorkflowScope(row pgx.Row) (WorkflowScope, error) {
	var scope WorkflowScope
	err := row.Scan(&scope.ID, &scope.TenantID, &scope.RunID, &scope.ParentLoopNodeRunID,
		&scope.RoundIndex, &scope.Status, &scope.State, &scope.CreatedAt, &scope.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowScope{}, ErrWorkflowScopeNotFound
	}
	if err != nil {
		return WorkflowScope{}, fmt.Errorf("scan workflow scope: %w", err)
	}
	return scope, nil
}
