package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Task statuses. A person moves a task by hand; the execution service moves it when a run starts
// and when the run ends. done and cancelled are terminal.
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskReview    = "review"
	TaskDone      = "done"
	TaskCancelled = "cancelled"
)

// Assignee kinds. A squad assignee means the squad owns the work; it never dispatches to a member.
const (
	AssigneeHuman = "human"
	AssigneeAgent = "agent"
	AssigneeSquad = "squad"
)

// Task mutation errors. The HTTP layer maps each one to a stable error code.
var (
	// ErrInvalidTransition reports a status change the task lifecycle does not allow.
	ErrInvalidTransition = errors.New("invalid task status transition")
	// ErrInvalidAssignee reports an assignee that is not typed or not a live workspace reference.
	ErrInvalidAssignee = errors.New("invalid task assignee")
	// ErrInvalidProject reports a project that does not belong to this workspace.
	ErrInvalidProject = errors.New("unknown task project")
)

// TaskAssignee is one typed reference. Identifier carries the plugin identity for agents, in the
// same namespace/identifier form the plugin routes use, and stays empty for other kinds.
type TaskAssignee struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Identifier  string `json:"identifier,omitempty"`
}

// Task is v2's minimal work item. The whole workspace sees every task, including whose it is;
// a private conversation stays private.
type Task struct {
	ID            string        `json:"id"`
	TenantID      string        `json:"tenantId"`
	Title         string        `json:"title"`
	Description   string        `json:"description"`
	ProjectID     string        `json:"projectId"`
	Assignee      *TaskAssignee `json:"assignee"`
	Status        string        `json:"status"`
	CreatedBy     string        `json:"createdBy"`
	CreatedByName string        `json:"createdByName"`
	Version       int64         `json:"version"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
}

// TaskAssigneeRef is the typed identity a write assigns. Display values come from the join, never
// from the request.
type TaskAssigneeRef struct {
	Type string
	ID   string
}

// TaskInput carries the editable fields. A nil Assignee leaves the task unassigned on create and
// is not part of an edit; assignment has its own write.
type TaskInput struct {
	Title       string
	Description string
	ProjectID   string
	Assignee    *TaskAssigneeRef
	Version     int64
}

// CanTransitionTask reports whether the task lifecycle allows moving from one status to another.
// The execution service uses the same rule, so a run cannot write a status a person could not.
func CanTransitionTask(from, to string) bool {
	switch from {
	case TaskPending:
		return to == TaskRunning || to == TaskCancelled
	case TaskRunning:
		return to == TaskReview || to == TaskCancelled
	case TaskReview:
		return to == TaskDone || to == TaskCancelled
	}
	return false
}

// taskColumns resolves the assignee's current name from its own table rather than a copied label,
// so a renamed member or agent shows through.
const taskColumns = `t.id, t.tenant_id::text, t.title, t.description, t.project_id,
	COALESCE(t.assignee_type, ''), COALESCE(t.assignee_id::text, ''),
	CASE t.assignee_type
		WHEN 'human' THEN COALESCE(u.display_name, '')
		WHEN 'agent' THEN COALESCE(a.name, '')
		WHEN 'squad' THEN COALESCE(sq.name, '')
		ELSE ''
	END,
	CASE WHEN t.assignee_type = 'agent' THEN COALESCE(a.source_namespace || '/' || a.identifier, '') ELSE '' END,
	t.status, t.created_by::text, COALESCE(c.display_name, ''), t.version, t.created_at, t.updated_at`

// taskJoins resolves the typed assignee and the creator. FOR UPDATE OF t keeps a lock usable
// beside these outer joins.
const taskJoins = `FROM v2_tasks t
	LEFT JOIN users u ON t.assignee_type = 'human' AND u.id = t.assignee_id
	LEFT JOIN v2_space_agents a ON t.assignee_type = 'agent' AND a.id = t.assignee_id
	LEFT JOIN v2_squads sq ON t.assignee_type = 'squad' AND sq.id = t.assignee_id
	JOIN users c ON c.id = t.created_by`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	var kind, id, name, identifier string
	e := row.Scan(&t.ID, &t.TenantID, &t.Title, &t.Description, &t.ProjectID, &kind, &id, &name,
		&identifier, &t.Status, &t.CreatedBy, &t.CreatedByName, &t.Version, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if e != nil {
		return t, e
	}
	if kind != "" {
		t.Assignee = &TaskAssignee{Type: kind, ID: id, DisplayName: name, Identifier: identifier}
	}
	return t, nil
}

// requireProject keeps a task inside its workspace: a project id from another tenant is rejected
// exactly like an unknown one.
func requireProject(ctx context.Context, q dbTX, tenant, id string) error {
	if id == "" {
		return ErrInvalidProject
	}
	var exists bool
	e := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM v2_projects WHERE tenant_id = $1 AND id = $2)`, tenant, id).Scan(&exists)
	if e != nil {
		return fmt.Errorf("look up project: %w", e)
	}
	if !exists {
		return ErrInvalidProject
	}
	return nil
}

// requireAssignee validates the typed target against current workspace state: an active human
// member, an installed Space Agent, or a squad that is not archived.
func requireAssignee(ctx context.Context, q dbTX, tenant string, ref TaskAssigneeRef) error {
	switch ref.Type {
	case AssigneeHuman:
		return requireActiveUser(ctx, q, tenant, ref.ID)
	case AssigneeAgent:
		return requireActiveAgent(ctx, q, tenant, ref.ID)
	case AssigneeSquad:
		var live bool
		e := q.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM v2_squads WHERE id = $1 AND tenant_id = $2 AND archived_at IS NULL)`,
			ref.ID, tenant).Scan(&live)
		if e != nil {
			return fmt.Errorf("look up squad: %w", e)
		}
		if !live {
			return ErrInvalidAssignee
		}
		return nil
	}
	return ErrInvalidAssignee
}

// lockTask opens a mutation: it checks the caller's current workspace membership, locks the task
// row and rejects a stale version before any write.
func lockTask(ctx context.Context, tx pgx.Tx, tenant, user, id string, version int64) (Task, error) {
	if _, e := tenantRole(ctx, tx, tenant, user); e != nil {
		return Task{}, e
	}
	task, e := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+`
		WHERE t.tenant_id = $1 AND t.id = $2 FOR UPDATE OF t`, tenant, id))
	if e != nil {
		return task, e
	}
	if task.Version != version {
		return task, ErrConflict
	}
	return task, nil
}

// writeTaskStatus applies a transition that the caller already validated and returns the row.
func writeTaskStatus(ctx context.Context, tx pgx.Tx, id, status string) (Task, error) {
	if _, e := tx.Exec(ctx, `UPDATE v2_tasks SET status = $2, version = version + 1, updated_at = now()
		WHERE id = $1`, id, status); e != nil {
		return Task{}, fmt.Errorf("update task status: %w", e)
	}
	return scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+` WHERE t.id = $1`, id))
}

// Tasks lists the workspace's tasks, most recently changed first.
func (s *Store) Tasks(ctx context.Context, tenant, caller string) ([]Task, error) {
	if _, e := tenantRole(ctx, s.pool, tenant, caller); e != nil {
		return nil, e
	}
	rows, e := s.pool.Query(ctx, `SELECT `+taskColumns+` `+taskJoins+`
		WHERE t.tenant_id = $1 ORDER BY t.updated_at DESC, t.id DESC`, tenant)
	if e != nil {
		return nil, fmt.Errorf("list tasks: %w", e)
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		task, e := scanTask(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

// Task returns one task to a current workspace member.
func (s *Store) Task(ctx context.Context, tenant, caller, id string) (Task, error) {
	if _, e := tenantRole(ctx, s.pool, tenant, caller); e != nil {
		return Task{}, e
	}
	return scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+`
		WHERE t.tenant_id = $1 AND t.id = $2`, tenant, id))
}

// CreateTask records a task for the caller's workspace. Its project and assignee must already
// belong to that workspace.
func (s *Store) CreateTask(ctx context.Context, tenant, user string, in TaskInput) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := tenantRole(ctx, tx, tenant, user); e != nil {
			return e
		}
		if e := requireProject(ctx, tx, tenant, in.ProjectID); e != nil {
			return e
		}
		var kind, id *string
		if in.Assignee != nil {
			if e := requireAssignee(ctx, tx, tenant, *in.Assignee); e != nil {
				return e
			}
			kind, id = &in.Assignee.Type, &in.Assignee.ID
		}
		var taskID string
		if e := tx.QueryRow(ctx, `
			INSERT INTO v2_tasks (tenant_id, title, description, project_id, assignee_type, assignee_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			tenant, in.Title, in.Description, in.ProjectID, kind, id, user).Scan(&taskID); e != nil {
			return fmt.Errorf("insert task: %w", e)
		}
		task, e := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+`
			WHERE t.id = $1`, taskID))
		out = task
		return e
	})
	return out, e
}

// UpdateTask replaces the title, description and project under a version check. The status and
// assignee have their own writes so a stale form cannot silently undo them.
func (s *Store) UpdateTask(ctx context.Context, tenant, user, id string, in TaskInput) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockTask(ctx, tx, tenant, user, id, in.Version); e != nil {
			return e
		}
		if e := requireProject(ctx, tx, tenant, in.ProjectID); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_tasks SET title = $2, description = $3, project_id = $4,
				version = version + 1, updated_at = now() WHERE id = $1`,
			id, in.Title, in.Description, in.ProjectID); e != nil {
			return fmt.Errorf("update task: %w", e)
		}
		task, e := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+` WHERE t.id = $1`, id))
		out = task
		return e
	})
	return out, e
}

// SetTaskStatus moves a task by hand under a version check. Only the lifecycle's legal moves are
// accepted, so 待验收 cannot be skipped and a finished task cannot silently reopen.
func (s *Store) SetTaskStatus(ctx context.Context, tenant, user, id, status string, version int64) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		task, e := lockTask(ctx, tx, tenant, user, id, version)
		if e != nil {
			return e
		}
		if !CanTransitionTask(task.Status, status) {
			return ErrInvalidTransition
		}
		task, e = writeTaskStatus(ctx, tx, id, status)
		out = task
		return e
	})
	return out, e
}

// TransitionTaskStatus moves a task on behalf of the execution service, which owns the status
// while a run is live. from guards against a stale run overwriting a person's change.
func (s *Store) TransitionTaskStatus(ctx context.Context, tenant, id, from, to string) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		task, e := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+`
			WHERE t.tenant_id = $1 AND t.id = $2 FOR UPDATE OF t`, tenant, id))
		if e != nil {
			return e
		}
		if task.Status != from {
			return ErrConflict
		}
		if !CanTransitionTask(from, to) {
			return ErrInvalidTransition
		}
		task, e = writeTaskStatus(ctx, tx, id, to)
		out = task
		return e
	})
	return out, e
}

// SetTaskAssignee replaces or clears the typed assignee under a version check. Assigning a squad
// only records that the squad owns the task; it dispatches nothing.
func (s *Store) SetTaskAssignee(ctx context.Context, tenant, user, id string, ref *TaskAssigneeRef, version int64) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockTask(ctx, tx, tenant, user, id, version); e != nil {
			return e
		}
		var kind, assignee string
		if ref != nil {
			if e := requireAssignee(ctx, tx, tenant, *ref); e != nil {
				return e
			}
			kind, assignee = ref.Type, ref.ID
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_tasks SET assignee_type = NULLIF($2, ''), assignee_id = NULLIF($3, '')::uuid,
				version = version + 1, updated_at = now() WHERE id = $1`, id, kind, assignee); e != nil {
			return fmt.Errorf("update task assignee: %w", e)
		}
		task, e := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+` WHERE t.id = $1`, id))
		out = task
		return e
	})
	return out, e
}
