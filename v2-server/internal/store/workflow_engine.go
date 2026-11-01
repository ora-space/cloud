package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The engine transactions below give the workflow domain desktop's repository semantics:
// completing a node folds its result into the run payload and maintains state.current_nodes in
// the same transaction, and a root-scope failure fails the run (only while it is still running,
// so a late sibling cannot overwrite a terminal run). They are keyed by run ID alone because the
// engine acts for runs it already admitted; HTTP callers keep using the tenant-scoped functions.
// Payload changes arrive as pure functions so this layer never interprets the payload.

// WorkflowEngineRun is one run as the engine reads it, with its frozen graph.
type WorkflowEngineRun struct {
	Run   WorkflowRun
	Graph string
}

// WorkflowEngineStart is one node attempt a scheduling wave starts.
type WorkflowEngineStart struct {
	NodeID, NodeType string
	Input            *string
}

// WorkflowEngineContext loads a run and its frozen snapshot graph.
func (s *Store) WorkflowEngineContext(ctx context.Context, runID string) (WorkflowEngineRun, error) {
	var out WorkflowEngineRun
	row := s.pool.QueryRow(ctx, `
		SELECT `+prefixed("r", workflowRunColumns)+`, snap.graph
		FROM v2_workflow_runs r JOIN v2_workflow_snapshots snap ON snap.id = r.snapshot_id
		WHERE r.id = $1 AND r.deleted_at IS NULL`, runID)
	run := &out.Run
	err := row.Scan(&run.ID, &run.TenantID, &run.WorkflowID, &run.SnapshotID, &run.ProjectID,
		&run.Name, &run.Status, &run.State, &run.Input, &run.Output, &run.Error, &run.RuntimePayload,
		&run.StartedAt, &run.FinishedAt, &run.CreatedAt, &run.UpdatedAt, &out.Graph)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowEngineRun{}, ErrWorkflowRunNotFound
	}
	if err != nil {
		return WorkflowEngineRun{}, fmt.Errorf("read workflow engine context: %w", err)
	}
	return out, nil
}

// WorkflowEngineNodeRuns lists a run's live root-scope attempts in creation order.
func (s *Store) WorkflowEngineNodeRuns(ctx context.Context, runID string) ([]WorkflowNodeRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+prefixed("n", workflowNodeRunColumns)+`
		FROM v2_workflow_node_runs n JOIN v2_workflow_run_roots root ON root.run_id = n.run_id AND root.scope_id = n.scope_id
		WHERE n.run_id = $1 AND n.deleted_at IS NULL
		ORDER BY n.created_at, n.id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list engine node runs: %w", err)
	}
	defer rows.Close()
	result := []WorkflowNodeRun{}
	for rows.Next() {
		node, err := scanNodeRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

type engineRunRow struct {
	tenant, status, root string
	current              []string
	payload              []byte
}

// lockEngineRun locks a run row and reads what every engine transition checks.
func lockEngineRun(ctx context.Context, tx pgx.Tx, runID string) (engineRunRow, error) {
	var r engineRunRow
	var state []byte
	err := tx.QueryRow(ctx, `
		SELECT r.tenant_id, r.status, r.state, r.payload, root.scope_id
		FROM v2_workflow_runs r JOIN v2_workflow_run_roots root ON root.run_id = r.id
		WHERE r.id = $1 AND r.deleted_at IS NULL FOR UPDATE OF r`, runID).
		Scan(&r.tenant, &r.status, &state, &r.payload, &r.root)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrWorkflowRunNotFound
	}
	if err != nil {
		return r, fmt.Errorf("lock workflow run: %w", err)
	}
	var decoded struct {
		CurrentNodes []string `json:"current_nodes"`
	}
	if len(state) != 0 {
		if err := json.Unmarshal(state, &decoded); err != nil {
			return r, fmt.Errorf("decode run state: %w", err)
		}
	}
	r.current = decoded.CurrentNodes
	if r.current == nil {
		r.current = []string{}
	}
	return r, nil
}

func writeCurrentNodes(ctx context.Context, tx pgx.Tx, runID string, current []string) error {
	state, err := json.Marshal(map[string][]string{"current_nodes": current})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE v2_workflow_runs SET state = $2, updated_at = now() WHERE id = $1`, runID, state)
	return err
}

// insertEngineNode stamps created_at with clock_timestamp() rather than the column default now():
// a wave inserts several attempts in one transaction, where now() is constant, and the engine
// lists attempts by (created_at, id) as their creation order.
func insertEngineNode(ctx context.Context, tx pgx.Tx, r engineRunRow, runID string, in WorkflowEngineStart) (WorkflowNodeRun, error) {
	return scanNodeRun(tx.QueryRow(ctx, `
		INSERT INTO v2_workflow_node_runs
			(tenant_id, run_id, scope_id, node_id, node_type, status, input, started_at, created_at)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, clock_timestamp(), clock_timestamp())
		RETURNING `+workflowNodeRunColumns, r.tenant, runID, r.root, in.NodeID, in.NodeType, in.Input))
}

// StartWorkflowEngineRun moves a pending run that has started no node to running, applies
// prepare to its payload and inserts the entry attempt. It returns false for any other run, so
// a repeated start is idempotent.
func (s *Store) StartWorkflowEngineRun(ctx context.Context, runID string, entry WorkflowEngineStart, prepare func([]byte) ([]byte, error)) (bool, error) {
	started := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM v2_workflow_node_runs WHERE run_id = $1 AND deleted_at IS NULL)`, runID).Scan(&live); err != nil {
			return fmt.Errorf("check started nodes: %w", err)
		}
		if r.status != WorkflowStatusPending || len(r.current) != 0 || live {
			return nil
		}
		payload, err := prepare(r.payload)
		if err != nil {
			return err
		}
		if _, err := insertEngineNode(ctx, tx, r, runID, entry); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_runs SET status = 'running', payload = $2, started_at = now(), updated_at = now()
			WHERE id = $1`, runID, payload); err != nil {
			return fmt.Errorf("start run: %w", err)
		}
		started = true
		return writeCurrentNodes(ctx, tx, runID, []string{entry.NodeID})
	})
	return started, err
}

// StartWorkflowEngineNodes inserts one ready wave into the root scope and anchors it.
func (s *Store) StartWorkflowEngineNodes(ctx context.Context, runID string, wave []WorkflowEngineStart) ([]WorkflowNodeRun, error) {
	var created []WorkflowNodeRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.status != WorkflowStatusRunning {
			return ErrWorkflowRunState
		}
		created = created[:0]
		for _, in := range wave {
			node, err := insertEngineNode(ctx, tx, r, runID, in)
			if err != nil {
				return err
			}
			created = append(created, node)
			r.current = append(r.current, in.NodeID)
		}
		return writeCurrentNodes(ctx, tx, runID, r.current)
	})
	return created, err
}

// CompleteWorkflowEngineNode marks a running attempt succeeded, merges keys into its payload
// via merge, folds the result into the run payload and drops it from current_nodes. It returns
// false when the attempt is no longer running (a late or duplicate callback).
func (s *Store) CompleteWorkflowEngineNode(
	ctx context.Context, runID, nodeRunID string, output *string,
	merge func(node []byte) ([]byte, error), fold func(run []byte, node WorkflowNodeRun) ([]byte, error),
) (bool, error) {
	advanced := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		node, err := lockWorkflowNodeRun(ctx, tx, r.tenant, runID, nodeRunID)
		if errors.Is(err, ErrWorkflowNodeRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if node.Status != WorkflowStatusRunning && node.Status != WorkflowStatusPending {
			return nil
		}
		nodePayload, err := merge(node.Payload)
		if err != nil {
			return err
		}
		runPayload, err := fold(r.payload, node)
		if err != nil {
			return err
		}
		// Condition decisions are scheduler state and never become public node output.
		persisted := output
		if node.NodeType == "condition" {
			persisted = nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET status = 'succeeded', output = $2, payload = $3,
				finished_at = now(), updated_at = now()
			WHERE id = $1`, nodeRunID, persisted, nodePayload); err != nil {
			return fmt.Errorf("complete node: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE v2_workflow_runs SET payload = $2 WHERE id = $1`, runID, runPayload); err != nil {
			return fmt.Errorf("fold run payload: %w", err)
		}
		advanced = true
		return writeCurrentNodes(ctx, tx, runID, without(r.current, node.NodeID))
	})
	return advanced, err
}

func without(items []string, drop string) []string {
	result := []string{}
	for _, item := range items {
		if item != drop {
			result = append(result, item)
		}
	}
	return result
}

// FailWorkflowEngineNode marks a running attempt failed with the payload detail built for the
// attempt number it receives, anchors current_nodes on it and fails the run if it is still
// running. It returns false when the attempt is no longer running.
func (s *Store) FailWorkflowEngineNode(
	ctx context.Context, runID, nodeRunID, message string, output *string,
	detail func(attempt int, node, run []byte) ([]byte, error),
) (bool, error) {
	advanced := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		node, err := lockWorkflowNodeRun(ctx, tx, r.tenant, runID, nodeRunID)
		if errors.Is(err, ErrWorkflowNodeRunNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if node.Status != WorkflowStatusRunning {
			return nil
		}
		attempt, err := workflowNodeAttempt(ctx, tx, runID, node)
		if err != nil {
			return err
		}
		payload, err := detail(attempt, node.Payload, r.payload)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET status = 'failed', error = $2, output = COALESCE($3, output),
				payload = $4, finished_at = now(), updated_at = now()
			WHERE id = $1`, nodeRunID, message, output, payload); err != nil {
			return fmt.Errorf("fail node: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_runs SET status = 'failed', error = $2, finished_at = now(), updated_at = now()
			WHERE id = $1 AND status = 'running'`, runID, message); err != nil {
			return fmt.Errorf("fail run: %w", err)
		}
		advanced = true
		return writeCurrentNodes(ctx, tx, runID, []string{node.NodeID})
	})
	return advanced, err
}

// FinishWorkflowEngineRun marks a drained running run succeeded with its output.
func (s *Store) FinishWorkflowEngineRun(ctx context.Context, runID string, output *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE v2_workflow_runs SET status = 'succeeded', output = $2, state = '{"current_nodes":[]}'::jsonb,
			finished_at = now(), updated_at = now()
		WHERE id = $1 AND status = 'running' AND deleted_at IS NULL`, runID, output)
	if err != nil {
		return fmt.Errorf("finish workflow run: %w", err)
	}
	return nil
}

// UpdateWorkflowEngineRunInput replaces the kickoff input of a run that has not started any
// node and applies fold to its payload. Any other run returns ErrWorkflowRunState.
func (s *Store) UpdateWorkflowEngineRunInput(ctx context.Context, runID string, input *string, fold func([]byte) ([]byte, error)) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := lockEngineRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.status != WorkflowStatusPending || len(r.current) != 0 {
			return ErrWorkflowRunState
		}
		payload, err := fold(r.payload)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE v2_workflow_runs SET input = $2, payload = $3, updated_at = now() WHERE id = $1`, runID, input, payload)
		return err
	})
}

// InterruptWorkflowRuns fails every attempt and run a previous process left executing, before
// the process serves requests. v2 has no interactive nodes and never replays an Agent, so every
// non-terminal attempt fails with the detail built for it and its run fails with it.
func (s *Store) InterruptWorkflowRuns(ctx context.Context, message string, detail func(attempt int, node []byte) ([]byte, error)) ([]string, error) {
	var interrupted []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+prefixed("n", workflowNodeRunColumns)+`
			FROM v2_workflow_node_runs n JOIN v2_workflow_runs r ON r.id = n.run_id
			WHERE n.deleted_at IS NULL AND n.status IN ('pending', 'running') AND r.deleted_at IS NULL
			ORDER BY n.run_id, n.created_at
			FOR UPDATE OF n`)
		if err != nil {
			return fmt.Errorf("list interrupted nodes: %w", err)
		}
		nodes := []WorkflowNodeRun{}
		for rows.Next() {
			node, err := scanNodeRun(rows)
			if err != nil {
				rows.Close()
				return err
			}
			nodes = append(nodes, node)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, node := range nodes {
			attempt, err := workflowNodeAttempt(ctx, tx, node.RunID, node)
			if err != nil {
				return err
			}
			payload, err := detail(attempt, node.Payload)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE v2_workflow_node_runs SET status = 'failed', error = $2, payload = $3,
					finished_at = now(), updated_at = now()
				WHERE id = $1`, node.ID, message, payload); err != nil {
				return fmt.Errorf("interrupt node: %w", err)
			}
		}
		runRows, err := tx.Query(ctx, `
			UPDATE v2_workflow_runs SET status = 'failed', error = $1, finished_at = now(), updated_at = now()
			WHERE deleted_at IS NULL AND (status = 'running' OR (status = 'pending' AND state->'current_nodes' <> '[]'::jsonb))
			RETURNING id`, message)
		if err != nil {
			return fmt.Errorf("interrupt runs: %w", err)
		}
		for runRows.Next() {
			var id string
			if err := runRows.Scan(&id); err != nil {
				runRows.Close()
				return err
			}
			interrupted = append(interrupted, id)
		}
		runRows.Close()
		if err := runRows.Err(); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_scopes SET status = 'failed', updated_at = now()
			WHERE status IN ('pending', 'running')`); err != nil {
			return fmt.Errorf("interrupt loop rounds: %w", err)
		}
		return nil
	})
	return interrupted, err
}

// prefixed qualifies a column list with a table alias.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
