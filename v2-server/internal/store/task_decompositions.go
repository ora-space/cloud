package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Task decomposition statuses: the workflow run is still going, its subtasks were written, or it
// ended without any.
const (
	DecompositionRunning   = "running"
	DecompositionSucceeded = "succeeded"
	DecompositionFailed    = "failed"
)

// ErrDecompositionActive reports a second decomposition while one of the task's is still running.
var ErrDecompositionActive = errors.New("task decomposition running")

// TaskDecomposition is one workflow run that splits a task into subtasks. The whole workspace
// reads it, like the task.
type TaskDecomposition struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"taskId"`
	TenantID      string     `json:"-"`
	StartedBy     string     `json:"startedBy"`
	StartedByName string     `json:"startedByName"`
	StartKey      string     `json:"-"`
	WorkflowID    string     `json:"workflowId"`
	WorkflowName  string     `json:"workflowName"`
	WorkflowRunID string     `json:"workflowRunId"`
	Status        string     `json:"status"`
	Failure       string     `json:"failure"`
	CreatedCount  int        `json:"createdCount"`
	CreatedAt     time.Time  `json:"createdAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
}

// NewTaskDecomposition is the row written once its workflow run exists.
type NewTaskDecomposition struct {
	TaskID        string
	TenantID      string
	StartedBy     string
	StartKey      string
	WorkflowID    string
	WorkflowName  string
	WorkflowRunID string
}

// Subtask is one item of a decomposition's output, already checked and trimmed to the task limits.
type Subtask struct {
	Title       string
	Description string
}

const decompositionColumns = `d.id, d.task_id, d.tenant_id::text, d.started_by::text, u.display_name, d.start_key,
	d.workflow_id::text, d.workflow_name, d.workflow_run_id::text, d.status, d.failure, d.created_count,
	d.created_at, d.finished_at`

const decompositionJoins = `FROM v2_task_decompositions d JOIN users u ON u.id = d.started_by`

func scanDecomposition(row pgx.Row) (TaskDecomposition, error) {
	var d TaskDecomposition
	e := row.Scan(&d.ID, &d.TaskID, &d.TenantID, &d.StartedBy, &d.StartedByName, &d.StartKey,
		&d.WorkflowID, &d.WorkflowName, &d.WorkflowRunID, &d.Status, &d.Failure, &d.CreatedCount,
		&d.CreatedAt, &d.FinishedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, e
}

func decompositions(ctx context.Context, q dbTX, where string, args ...any) ([]TaskDecomposition, error) {
	rows, e := q.Query(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+` WHERE `+where+
		` ORDER BY d.created_at DESC, d.id DESC`, args...)
	if e != nil {
		return nil, fmt.Errorf("list task decompositions: %w", e)
	}
	defer rows.Close()
	out := []TaskDecomposition{}
	for rows.Next() {
		d, e := scanDecomposition(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TaskDecompositions lists a task's decompositions to a current workspace member, newest first.
func (s *Store) TaskDecompositions(ctx context.Context, tenant, taskID string) ([]TaskDecomposition, error) {
	if _, e := s.Task(ctx, tenant, taskID); e != nil {
		return nil, e
	}
	return decompositions(ctx, s.pool, `d.tenant_id = $1 AND d.task_id = $2`, tenant, taskID)
}

// TaskDecompositionByKey finds the decomposition a person already started with this key.
func (s *Store) TaskDecompositionByKey(ctx context.Context, tenant, taskID, user, key string) (TaskDecomposition, error) {
	return scanDecomposition(s.pool.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+`
		WHERE d.tenant_id = $1 AND d.task_id = $2 AND d.started_by = $3 AND d.start_key = $4`, tenant, taskID, user, key))
}

// LiveTaskDecomposition returns the task's running decomposition, if any.
func (s *Store) LiveTaskDecomposition(ctx context.Context, tenant, taskID string) (TaskDecomposition, bool, error) {
	d, e := scanDecomposition(s.pool.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+`
		WHERE d.tenant_id = $1 AND d.task_id = $2 AND d.status = 'running'`, tenant, taskID))
	if errors.Is(e, ErrNotFound) {
		return d, false, nil
	}
	return d, e == nil, e
}

// RunningTaskDecompositions lists every decomposition still waiting on its run, across workspaces,
// for the process that recovers them at startup.
func (s *Store) RunningTaskDecompositions(ctx context.Context) ([]TaskDecomposition, error) {
	return decompositions(ctx, s.pool, `d.status = 'running'`)
}

// CreateTaskDecomposition records a decomposition whose workflow run was just created. A second
// running decomposition of the same task returns ErrDecompositionActive.
func (s *Store) CreateTaskDecomposition(ctx context.Context, in NewTaskDecomposition) (TaskDecomposition, error) {
	var id string
	e := s.pool.QueryRow(ctx, `
		INSERT INTO v2_task_decompositions (task_id, tenant_id, started_by, start_key, workflow_id, workflow_name, workflow_run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		in.TaskID, in.TenantID, in.StartedBy, in.StartKey, in.WorkflowID, in.WorkflowName, in.WorkflowRunID).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(e, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "v2_task_decompositions_live" {
			return TaskDecomposition{}, ErrDecompositionActive
		}
		return TaskDecomposition{}, ErrConflict
	}
	if e != nil {
		return TaskDecomposition{}, fmt.Errorf("insert task decomposition: %w", e)
	}
	return scanDecomposition(s.pool.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+` WHERE d.id = $1`, id))
}

// RefuseTaskDecomposition withdraws a decomposition whose run the workflow host refused to start:
// the never-started run is cancelled and the decomposition failed with the reason in one
// transaction, as RefuseTaskWorkflow does for a handling run. Either both are persisted or
// neither, so no page reads a failed decomposition beside a live run, no restart can find a
// pending run whose decomposition already let go of it, and no later report overwrites the
// reason: a decomposition already settled is returned as it is with false, its run untouched. A
// run that already began executing is refused with ErrWorkflowRunState and nothing changes:
// stopping it is the workflow host's, and its real outcome settles the decomposition.
func (s *Store) RefuseTaskDecomposition(ctx context.Context, id, failure string) (TaskDecomposition, bool, error) {
	if failure == "" {
		return TaskDecomposition{}, false, errors.New("refuse task decomposition: no failure")
	}
	var out TaskDecomposition
	settled := false
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		d, e := scanDecomposition(tx.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+`
			WHERE d.id = $1 FOR UPDATE OF d`, id))
		if e != nil {
			return e
		}
		out = d
		if d.Status != DecompositionRunning {
			return nil
		}
		if run, e := cancelWorkflowRun(ctx, tx, d.TenantID, d.WorkflowRunID); e != nil {
			if !errors.Is(e, ErrWorkflowRunNotFound) {
				return e
			}
			// A deleted run never started either; the decomposition still fails below.
		} else if run.StartedAt != nil {
			return ErrWorkflowRunState
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_task_decompositions SET status = $2, failure = $3, created_count = 0, finished_at = now()
			WHERE id = $1`, id, DecompositionFailed, failure); e != nil {
			return fmt.Errorf("refuse task decomposition: %w", e)
		}
		settled = true
		out, e = scanDecomposition(tx.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+` WHERE d.id = $1`, id))
		return e
	})
	return out, settled, e
}

// SettleTaskDecomposition ends a running decomposition: with subtasks it succeeds and writes them
// under the task, with a failure it fails. Ending it and writing the subtasks share one transaction
// guarded by the running status, so however often a run's end is reported, only the first report
// writes anything; a later one returns the settled row and false.
//
// Subtasks belong to the task's project, are created by the person who started the decomposition,
// stay unassigned and pending, and list in their output order.
func (s *Store) SettleTaskDecomposition(ctx context.Context, id string, subtasks []Subtask, failure string) (TaskDecomposition, bool, error) {
	if failure == "" && len(subtasks) == 0 {
		return TaskDecomposition{}, false, errors.New("settle task decomposition: no subtasks and no failure")
	}
	var out TaskDecomposition
	settled := false
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		d, e := scanDecomposition(tx.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+`
			WHERE d.id = $1 FOR UPDATE OF d`, id))
		if e != nil {
			return e
		}
		out = d
		if d.Status != DecompositionRunning {
			return nil
		}
		status, created := DecompositionFailed, 0
		if failure == "" {
			status, created = DecompositionSucceeded, len(subtasks)
			var project string
			if e := tx.QueryRow(ctx, `SELECT project_id FROM v2_tasks WHERE id = $1`, d.TaskID).Scan(&project); e != nil {
				return fmt.Errorf("read decomposed task: %w", e)
			}
			// The list orders by the last change, newest first: the first subtask gets the latest stamp.
			for i, sub := range subtasks {
				if _, e := tx.Exec(ctx, `
					INSERT INTO v2_tasks (tenant_id, title, description, project_id, created_by, parent_id, created_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, now() - make_interval(secs => $7), now() - make_interval(secs => $7))`,
					d.TenantID, sub.Title, sub.Description, project, d.StartedBy, d.TaskID, float64(i)/1000); e != nil {
					return fmt.Errorf("insert subtask: %w", e)
				}
			}
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_task_decompositions SET status = $2, failure = $3, created_count = $4, finished_at = now()
			WHERE id = $1`, id, status, failure, created); e != nil {
			return fmt.Errorf("settle task decomposition: %w", e)
		}
		settled = true
		out, e = scanDecomposition(tx.QueryRow(ctx, `SELECT `+decompositionColumns+` `+decompositionJoins+` WHERE d.id = $1`, id))
		return e
	})
	return out, settled, e
}
