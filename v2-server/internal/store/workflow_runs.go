package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrWorkflowRunNotFound reports a run that is missing, deleted, or owned elsewhere.
	ErrWorkflowRunNotFound = errors.New("workflow run not found")
	// ErrWorkflowNoPublishedSnapshot reports a definition without a version a run could freeze.
	ErrWorkflowNoPublishedSnapshot = errors.New("workflow has no published snapshot")
	// ErrWorkflowDraftSnapshot reports an attempt to run the mutable draft.
	ErrWorkflowDraftSnapshot = errors.New("workflow run cannot use the draft snapshot")
	// ErrProjectNotFound reports a project outside the tenant or missing.
	ErrProjectNotFound = errors.New("project not found")
	// ErrWorkflowNodeRunExists reports a live node row already occupying the same identity.
	ErrWorkflowNodeRunExists = errors.New("workflow node run exists")
	// ErrWorkflowNodeRunNotFound reports a node row that is missing, deleted, or owned elsewhere.
	ErrWorkflowNodeRunNotFound = errors.New("workflow node run not found")
	// ErrWorkflowNodeRunState reports a transition the row's current status does not allow.
	ErrWorkflowNodeRunState = errors.New("workflow node run is not in a transitionable state")
	// ErrWorkflowRunState reports a run transition its current status does not allow.
	ErrWorkflowRunState = errors.New("workflow run is not in a transitionable state")
	// ErrWorkflowSnapshotInUse reports a snapshot a live run froze.
	ErrWorkflowSnapshotInUse = errors.New("workflow snapshot is in use")
	// ErrWorkflowActiveRuns reports a definition a live run still references.
	ErrWorkflowActiveRuns = errors.New("workflow has live runs")
	// ErrWorkflowRunActive reports a run that still has work in flight.
	ErrWorkflowRunActive = errors.New("workflow run is active")
)

// WorkflowRunStatuses are the states a run and its node rows share. Awaiting input is deliberately
// absent: it is derived from a running run plus a pending node, never stored.
const (
	WorkflowStatusPending   = "pending"
	WorkflowStatusRunning   = "running"
	WorkflowStatusSucceeded = "succeeded"
	WorkflowStatusFailed    = "failed"
	WorkflowStatusCancelled = "cancelled"
)

// WorkflowRunInput is what a caller freezes when creating a run. The graph itself is never copied:
// the run points at the published snapshot, and the schedule seam passes its stable trigger identity
// instead of duplicating engine state.
type WorkflowRunInput struct {
	ProjectID   string
	Name        string
	Input       *string
	TriggerKind string
	TriggerRef  string
}

// WorkflowRun is one run row without its nodes. State and RuntimePayload stay opaque JSON: the
// workflow engine owns their shape.
type WorkflowRun struct {
	ID             string
	TenantID       string
	WorkflowID     string
	SnapshotID     string
	ProjectID      string
	Name           string
	Status         string
	State          []byte
	Input          *string
	Output         *string
	Error          *string
	RuntimePayload []byte
	StartedAt      *time.Time
	FinishedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// WorkflowRunSummary is a run list row: identity, frozen version and lifecycle, no graph or payload.
type WorkflowRunSummary struct {
	ID         string
	Name       string
	ProjectID  string
	WorkflowID string
	Version    string
	Status     string
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
}

// WorkflowNodeRun is one node attempt. Failed attempts are soft-deleted rows, so a detail read sees
// only the latest live attempt of each identity.
type WorkflowNodeRun struct {
	ID         string
	TenantID   string
	RunID      string
	ScopeID    string
	NodeID     string
	NodeType   string
	SessionID  *string
	Status     string
	Input      *string
	Output     *string
	Error      *string
	Payload    []byte
	Iteration  *int
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// WorkflowScope is a run's root or one started Loop round. Root rows have no status, state or round.
type WorkflowScope struct {
	ID                  string
	TenantID            string
	RunID               string
	ParentLoopNodeRunID *string
	RoundIndex          *int
	Status              *string
	State               []byte
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

const workflowRunColumns = `id, tenant_id, workflow_id, snapshot_id, project_id, name, status, state,
	input, output, error, payload, started_at, finished_at, created_at, updated_at`

const workflowNodeRunColumns = `id, tenant_id, run_id, scope_id, node_id, node_type, session_id,
	status, input, output, error, payload, iteration, started_at, finished_at, created_at, updated_at`

const workflowScopeColumns = `id, tenant_id, run_id, parent_loop_node_run_id, round_index, status,
	state, created_at, updated_at`

// derivedRunStatus projects a stored status plus pending nodes into the public status. `awaitingInput`
// is not a lifecycle state the engine can commit; it is how a paused interactive run is read.
const derivedRunStatus = `CASE WHEN r.status = 'running' AND EXISTS (
		SELECT 1 FROM v2_workflow_node_runs n
		WHERE n.run_id = r.id AND n.deleted_at IS NULL AND n.status = 'pending')
	THEN 'awaitingInput' ELSE r.status END`

func scanWorkflowRun(row pgx.Row) (WorkflowRun, error) {
	var run WorkflowRun
	err := row.Scan(&run.ID, &run.TenantID, &run.WorkflowID, &run.SnapshotID, &run.ProjectID,
		&run.Name, &run.Status, &run.State, &run.Input, &run.Output, &run.Error, &run.RuntimePayload,
		&run.StartedAt, &run.FinishedAt, &run.CreatedAt, &run.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowRun{}, ErrWorkflowRunNotFound
	}
	if err != nil {
		return WorkflowRun{}, fmt.Errorf("scan workflow run: %w", err)
	}
	return run, nil
}

// CreateWorkflowRun freezes a published version and the project into a pending run, and opens the
// run's root scope in the same transaction so no run ever exists without the scope its first node
// needs. The definition row is locked so a concurrent delete or publish cannot move the pointer
// between choosing the snapshot and inserting the run.
func (s *Store) CreateWorkflowRun(ctx context.Context, tenant, workflowID, snapshotID string, in WorkflowRunInput) (WorkflowRun, error) {
	var run WorkflowRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var publishedSnapshotID *string
		var workflowName string
		err := tx.QueryRow(ctx, `
			SELECT published_snapshot_id, name FROM v2_workflows
			WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenant, workflowID).
			Scan(&publishedSnapshotID, &workflowName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock workflow: %w", err)
		}

		if snapshotID == "" {
			if publishedSnapshotID == nil {
				return ErrWorkflowNoPublishedSnapshot
			}
			snapshotID = *publishedSnapshotID
		}
		var version string
		err = tx.QueryRow(ctx, `
			SELECT version FROM v2_workflow_snapshots
			WHERE tenant_id = $1 AND workflow_id = $2 AND id = $3 AND deleted_at IS NULL`,
			tenant, workflowID, snapshotID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowSnapshotNotFound
		}
		if err != nil {
			return fmt.Errorf("read frozen snapshot: %w", err)
		}
		if version == WorkflowDraftVersion {
			return ErrWorkflowDraftSnapshot
		}

		var projectExists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM v2_projects WHERE id = $1 AND tenant_id = $2)`,
			in.ProjectID, tenant).Scan(&projectExists); err != nil {
			return fmt.Errorf("check project: %w", err)
		}
		if !projectExists {
			return ErrProjectNotFound
		}

		name := in.Name
		if name == "" {
			name = workflowName
		}
		trigger := in.TriggerKind
		if trigger == "" {
			trigger = "manual"
		}
		run, err = scanWorkflowRun(tx.QueryRow(ctx, `
			INSERT INTO v2_workflow_runs
				(tenant_id, workflow_id, snapshot_id, project_id, name, status, state, input,
				 trigger_kind, trigger_ref)
			VALUES ($1, $2, $3, $4, $5, 'pending', '{"current_nodes":[]}'::jsonb, $6, $7, $8)
			RETURNING `+workflowRunColumns,
			tenant, workflowID, snapshotID, in.ProjectID, name, in.Input, trigger, in.TriggerRef))
		if err != nil {
			return err
		}
		rootScope := "root:" + run.ID
		if _, err := tx.Exec(ctx, `
			INSERT INTO v2_workflow_scopes (id, tenant_id, run_id) VALUES ($1, $2, $3)`,
			rootScope, tenant, run.ID); err != nil {
			return fmt.Errorf("insert root scope: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO v2_workflow_run_roots (run_id, scope_id) VALUES ($1, $2)`,
			run.ID, rootScope); err != nil {
			return fmt.Errorf("record root scope: %w", err)
		}
		return nil
	})
	return run, err
}

// WorkflowNodeRunInput is what the engine freezes when it dispatches one node attempt.
type WorkflowNodeRunInput struct {
	ScopeID   string
	NodeID    string
	NodeType  string
	Iteration *int
	Input     *string
}

// CreateWorkflowNodeRun inserts one started node attempt. An empty scope attaches to the run's
// current root scope, and the partial unique index refuses a second live row for the same
// (scope, node, iteration), so two concurrent dispatches cannot both create the attempt.
func (s *Store) CreateWorkflowNodeRun(ctx context.Context, tenant, runID string, in WorkflowNodeRunInput) (WorkflowNodeRun, error) {
	var node WorkflowNodeRun
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock the run row: deleting a run takes the same lock, so a dispatch cannot slip a live node
		// into a run that is being deleted and leave an orphan attempt behind.
		var active bool
		err := tx.QueryRow(ctx, `
			SELECT true FROM v2_workflow_runs
			WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenant, runID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowRunNotFound
		}
		if err != nil {
			return fmt.Errorf("lock run: %w", err)
		}
		scopeID := in.ScopeID
		if scopeID == "" {
			if err := tx.QueryRow(ctx, `
				SELECT scope_id FROM v2_workflow_run_roots WHERE run_id = $1`, runID).Scan(&scopeID); err != nil {
				return fmt.Errorf("read root scope: %w", err)
			}
		}
		var scopeBelongs bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM v2_workflow_scopes
				WHERE tenant_id = $1 AND run_id = $2 AND id = $3)`, tenant, runID, scopeID).Scan(&scopeBelongs); err != nil {
			return fmt.Errorf("check scope: %w", err)
		}
		if !scopeBelongs {
			return ErrWorkflowRunNotFound
		}
		node, err = scanNodeRun(tx.QueryRow(ctx, `
			INSERT INTO v2_workflow_node_runs
				(tenant_id, run_id, scope_id, node_id, node_type, status, input, iteration, started_at)
			VALUES ($1, $2, $3, $4, $5, 'running', $6, $7, now())
			RETURNING `+workflowNodeRunColumns,
			tenant, runID, scopeID, in.NodeID, in.NodeType, in.Input, in.Iteration))
		return err
	})
	return node, err
}

// StartWorkflowRun moves a pending run into its running life and stamps the start instant. The
// engine still checks its own scheduling anchor before dispatching; this line only enforces that a
// run never starts twice.
func (s *Store) StartWorkflowRun(ctx context.Context, tenant, runID string) (WorkflowRun, error) {
	run, err := scanWorkflowRun(s.pool.QueryRow(ctx, `
		UPDATE v2_workflow_runs SET status = 'running', started_at = now(), updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND status = 'pending' AND deleted_at IS NULL
		RETURNING `+workflowRunColumns, tenant, runID))
	if err == nil || !errors.Is(err, ErrWorkflowRunNotFound) {
		return run, err
	}
	// The update matched nothing: tell "gone" apart from "not pending".
	var exists bool
	if qerr := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM v2_workflow_runs
			WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL)`, tenant, runID).Scan(&exists); qerr != nil {
		return WorkflowRun{}, fmt.Errorf("check run: %w", qerr)
	}
	if !exists {
		return WorkflowRun{}, ErrWorkflowRunNotFound
	}
	return WorkflowRun{}, ErrWorkflowRunState
}

// ParkWorkflowNodeRun marks a running node as waiting for human input. A parked node is not
// computing, so a restart must not fail it; only the status moves here, and the completion snapshot
// the caller records travels with the payload update that owns those keys.
func (s *Store) ParkWorkflowNodeRun(ctx context.Context, tenant, runID, nodeRunID string) (WorkflowNodeRun, error) {
	node, err := scanNodeRun(s.pool.QueryRow(ctx, `
		UPDATE v2_workflow_node_runs SET status = 'pending', updated_at = now()
		WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND status = 'running' AND deleted_at IS NULL
		RETURNING `+workflowNodeRunColumns, tenant, runID, nodeRunID))
	if err == nil || !errors.Is(err, ErrWorkflowNodeRunNotFound) {
		return node, err
	}
	// The update matched nothing: tell "gone" apart from "not in a parkable state".
	var exists bool
	if qerr := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM v2_workflow_node_runs
			WHERE tenant_id = $1 AND run_id = $2 AND id = $3 AND deleted_at IS NULL)`,
		tenant, runID, nodeRunID).Scan(&exists); qerr != nil {
		return WorkflowNodeRun{}, fmt.Errorf("check node run: %w", qerr)
	}
	if !exists {
		return WorkflowNodeRun{}, ErrWorkflowNodeRunNotFound
	}
	return WorkflowNodeRun{}, ErrWorkflowNodeRunState
}

// DeleteWorkflowRun soft-deletes a finished run with its node rows. A running run or one with a
// pending/running node is refused, because deleting it would abandon work a live session still
// answers; the attempt history stays recoverable as soft-deleted rows.
func (s *Store) DeleteWorkflowRun(ctx context.Context, tenant, runID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM v2_workflow_runs
			WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`, tenant, runID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrWorkflowRunNotFound
		}
		if err != nil {
			return fmt.Errorf("lock workflow run: %w", err)
		}
		var active bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM v2_workflow_node_runs
				WHERE run_id = $1 AND deleted_at IS NULL AND status IN ('pending', 'running'))`, runID).Scan(&active); err != nil {
			return fmt.Errorf("check active nodes: %w", err)
		}
		if status == WorkflowStatusRunning || active {
			return ErrWorkflowRunActive
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_node_runs SET deleted_at = now(), updated_at = now()
			WHERE run_id = $1 AND deleted_at IS NULL`, runID); err != nil {
			return fmt.Errorf("delete node runs: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_runs SET deleted_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenant, runID); err != nil {
			return fmt.Errorf("delete workflow run: %w", err)
		}
		return nil
	})
}

// WorkflowRuns lists live runs of one definition, one project, or the whole tenant, oldest first as
// desktop's run list orders them. Empty filters mean "no filter".
func (s *Store) WorkflowRuns(ctx context.Context, tenant, workflowID, projectID string) ([]WorkflowRunSummary, error) {
	var workflowFilter, projectFilter any
	if workflowID != "" {
		workflowFilter = workflowID
	}
	if projectID != "" {
		projectFilter = projectID
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.id, r.name, r.project_id, r.workflow_id, COALESCE(s.version, ''), `+derivedRunStatus+`,
			r.started_at, r.finished_at, r.created_at
		FROM v2_workflow_runs r
		LEFT JOIN v2_workflow_snapshots s ON s.id = r.snapshot_id
		WHERE r.tenant_id = $1 AND r.deleted_at IS NULL
			AND ($2::uuid IS NULL OR r.workflow_id = $2::uuid)
			AND ($3::text IS NULL OR r.project_id = $3)
		ORDER BY r.created_at, r.id`, tenant, workflowFilter, projectFilter)
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	summaries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowRunSummary, error) {
		var summary WorkflowRunSummary
		err := row.Scan(&summary.ID, &summary.Name, &summary.ProjectID, &summary.WorkflowID,
			&summary.Version, &summary.Status, &summary.StartedAt, &summary.FinishedAt, &summary.CreatedAt)
		return summary, err
	})
	if err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	return summaries, nil
}

// WorkflowRun reads one live run in the tenant.
func (s *Store) WorkflowRun(ctx context.Context, tenant, runID string) (WorkflowRun, error) {
	return scanWorkflowRun(s.pool.QueryRow(ctx, `
		SELECT r.id, r.tenant_id, r.workflow_id, r.snapshot_id, r.project_id, r.name, `+derivedRunStatus+`,
			r.state, r.input, r.output, r.error, r.payload, r.started_at, r.finished_at, r.created_at, r.updated_at
		FROM v2_workflow_runs r
		WHERE r.tenant_id = $1 AND r.id = $2 AND r.deleted_at IS NULL`, tenant, runID))
}

// WorkflowRunNodes lists the live (latest attempt) rows of one run in commit order.
func (s *Store) WorkflowRunNodes(ctx context.Context, tenant, runID string) ([]WorkflowNodeRun, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+workflowNodeRunColumns+`
		FROM v2_workflow_node_runs
		WHERE tenant_id = $1 AND run_id = $2 AND deleted_at IS NULL
		ORDER BY created_at, id`, tenant, runID)
	if err != nil {
		return nil, fmt.Errorf("list node runs: %w", err)
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowNodeRun, error) {
		return scanNodeRun(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list node runs: %w", err)
	}
	return nodes, nil
}

// WorkflowRunSnapshot returns the snapshot a run froze. The run keeps its own pointer, so history
// still renders the executed graph after the definition publishes a newer version.
func (s *Store) WorkflowRunSnapshot(ctx context.Context, tenant, runID string) (WorkflowSnapshot, error) {
	var snapshotID string
	err := s.pool.QueryRow(ctx, `
		SELECT snapshot_id FROM v2_workflow_runs
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenant, runID).Scan(&snapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowSnapshot{}, ErrWorkflowRunNotFound
	}
	if err != nil {
		return WorkflowSnapshot{}, fmt.Errorf("read run snapshot: %w", err)
	}
	return scanWorkflowSnapshot(s.pool.QueryRow(ctx, "SELECT "+workflowSnapshotColumns+`
		FROM v2_workflow_snapshots
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenant, snapshotID))
}

func scanNodeRun(row pgx.Row) (WorkflowNodeRun, error) {
	var node WorkflowNodeRun
	err := row.Scan(&node.ID, &node.TenantID, &node.RunID, &node.ScopeID, &node.NodeID,
		&node.NodeType, &node.SessionID, &node.Status, &node.Input, &node.Output, &node.Error,
		&node.Payload, &node.Iteration, &node.StartedAt, &node.FinishedAt, &node.CreatedAt, &node.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "v2_workflow_node_runs_live_identity" {
		return WorkflowNodeRun{}, ErrWorkflowNodeRunExists
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowNodeRun{}, ErrWorkflowNodeRunNotFound
	}
	if err != nil {
		return WorkflowNodeRun{}, fmt.Errorf("scan node run: %w", err)
	}
	return node, nil
}

// WorkflowRunScopes lists the Loop rounds of one run; the root scope is never exposed to clients.
func (s *Store) WorkflowRunScopes(ctx context.Context, tenant, runID string) ([]WorkflowScope, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+workflowScopeColumns+`
		FROM v2_workflow_scopes
		WHERE tenant_id = $1 AND run_id = $2 AND parent_loop_node_run_id IS NOT NULL
		ORDER BY parent_loop_node_run_id, round_index`, tenant, runID)
	if err != nil {
		return nil, fmt.Errorf("list run scopes: %w", err)
	}
	scopes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (WorkflowScope, error) {
		var scope WorkflowScope
		err := row.Scan(&scope.ID, &scope.TenantID, &scope.RunID, &scope.ParentLoopNodeRunID,
			&scope.RoundIndex, &scope.Status, &scope.State, &scope.CreatedAt, &scope.UpdatedAt)
		return scope, err
	})
	if err != nil {
		return nil, fmt.Errorf("list run scopes: %w", err)
	}
	return scopes, nil
}

// WorkflowRunRootScope returns the scope new outer node rows attach to, so the migration from a
// user restart can rotate it in one place later.
func (s *Store) WorkflowRunRootScope(ctx context.Context, tenant, runID string) (string, error) {
	var scopeID string
	err := s.pool.QueryRow(ctx, `
		SELECT scope_id FROM v2_workflow_run_roots WHERE run_id = $1
			AND EXISTS (SELECT 1 FROM v2_workflow_runs r
				WHERE r.id = $1 AND r.tenant_id = $2 AND r.deleted_at IS NULL)`, runID, tenant).Scan(&scopeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrWorkflowRunNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read root scope: %w", err)
	}
	return scopeID, nil
}
