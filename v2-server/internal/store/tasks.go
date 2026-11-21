package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// ErrWorkflowActive reports a manual status move while a workflow is handling the task: the
	// workflow's settlement owns the status until it ends.
	ErrWorkflowActive = errors.New("task workflow active")
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
// a private conversation stays private. ParentID is the task it was split from, nil for a task
// created by hand.
type Task struct {
	WorkflowSources []WorkflowTaskSource `json:"workflowSources,omitempty"`
	ID              string               `json:"id"`
	TenantID        string               `json:"tenantId"`
	Title           string               `json:"title"`
	Description     string               `json:"description"`
	ProjectID       string               `json:"projectId"`
	ParentID        *string              `json:"parentId"`
	Assignee        *TaskAssignee        `json:"assignee"`
	Status          string               `json:"status"`
	CreatedBy       string               `json:"createdBy"`
	CreatedByName   string               `json:"createdByName"`
	Version         int64                `json:"version"`
	CreatedAt       time.Time            `json:"createdAt"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	// ActiveRun is the task's live Agent run, so a list can show who is working without reading
	// each timeline; nil when no run is live.
	ActiveRun *TaskRunSummary `json:"activeRun"`
}

// TaskRunSummary is the part of a live run a task list shows.
type TaskRunSummary struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	StartedBy     string `json:"startedBy"`
	StartedByName string `json:"startedByName"`
	ExecutorName  string `json:"executorName"`
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

// canCloseTask reports whether the caller may end the task, by confirming it done or cancelling it.
// That is the decision of whoever asked for the work or a workspace administrator; doing the work
// does not grant it.
func canCloseTask(a Access, task Task) bool {
	return a.Owner() || task.CreatedBy == a.UserID
}

// CanRunTransitionTask reports the moves a task's Agent run makes: starting or following up a run
// puts the task in 进行中, a finished turn hands it to 待验收, and a stopped or interrupted turn
// returns it to 待处理. A person still cannot make the last two moves by hand.
func CanRunTransitionTask(from, to string) bool {
	switch from {
	case TaskPending, TaskReview:
		return to == TaskRunning
	case TaskRunning:
		return to == TaskReview || to == TaskPending
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
	t.status, t.created_by::text, COALESCE(c.display_name, ''), t.version, t.created_at, t.updated_at,
	COALESCE(r.id::text, ''), COALESCE(r.turn_active, false), COALESCE(r.started_by::text, ''),
	COALESCE(ru.display_name, ''), COALESCE(r.executor_name, ''), t.parent_id::text`

// taskJoins resolves the typed assignee and the creator. FOR UPDATE OF t keeps a lock usable
// beside these outer joins.
const taskJoins = `FROM v2_tasks t
	LEFT JOIN users u ON t.assignee_type = 'human' AND u.id = t.assignee_id
	LEFT JOIN v2_space_agents a ON t.assignee_type = 'agent' AND a.id = t.assignee_id
	LEFT JOIN v2_squads sq ON t.assignee_type = 'squad' AND sq.id = t.assignee_id
	JOIN users c ON c.id = t.created_by
	LEFT JOIN v2_task_runs r ON r.task_id = t.id AND r.ended_at IS NULL
	LEFT JOIN users ru ON ru.id = r.started_by`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	var kind, id, name, identifier string
	var run TaskRunSummary
	var turnActive bool
	e := row.Scan(&t.ID, &t.TenantID, &t.Title, &t.Description, &t.ProjectID, &kind, &id, &name,
		&identifier, &t.Status, &t.CreatedBy, &t.CreatedByName, &t.Version, &t.CreatedAt, &t.UpdatedAt,
		&run.ID, &turnActive, &run.StartedBy, &run.StartedByName, &run.ExecutorName, &t.ParentID)
	if errors.Is(e, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if e != nil {
		return t, e
	}
	if kind != "" {
		t.Assignee = &TaskAssignee{Type: kind, ID: id, DisplayName: name, Identifier: identifier}
	}
	if run.ID != "" {
		run.State = TaskRunIdle
		if turnActive {
			run.State = TaskRunRunning
		}
		t.ActiveRun = &run
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

// lockTask opens a mutation: it locks the task row and rejects a stale version before any write.
func lockTask(ctx context.Context, tx pgx.Tx, tenant, id string, version int64) (Task, error) {
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
// Reaching 待验收 notifies the task's creator unless actor, the person making the move, is them;
// actor is empty when a run makes it.
func writeTaskStatus(ctx context.Context, tx pgx.Tx, id, status, actor string) (Task, error) {
	// Only status writers revoke handling ownership. Metadata edits and reassignment must not
	// interfere with run settlement; an internal status cycle must invalidate the old admission.
	if _, err := tx.Exec(ctx, `UPDATE v2_task_workflow_runs SET status_revision=status_revision+1 WHERE task_id=$1 AND status='running'`, id); err != nil {
		return Task{}, fmt.Errorf("invalidate handling status owner: %w", err)
	}

	if _, e := tx.Exec(ctx, `UPDATE v2_tasks SET status = $2, version = version + 1, updated_at = now()
		WHERE id = $1`, id, status); e != nil {
		return Task{}, fmt.Errorf("update task status: %w", e)
	}
	task, e := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+` WHERE t.id = $1`, id))
	if e != nil || status != TaskReview {
		return task, e
	}
	return task, notifyReview(ctx, tx, task.TenantID, task.ID, task.CreatedBy, actor, task.Version)
}

// Tasks lists the workspace's tasks, most recently changed first.
func (s *Store) Tasks(ctx context.Context, tenant string) ([]Task, error) {
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
func (s *Store) Task(ctx context.Context, tenant, id string) (Task, error) {
	task, err := scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+` `+taskJoins+`
        WHERE t.tenant_id = $1 AND t.id = $2`, tenant, id))
	if err != nil {
		return task, err
	}
	rows, err := s.pool.Query(ctx, `SELECT f.run_id::text,r.name,f.submitted_at FROM v2_workflow_forms f JOIN v2_workflow_runs r ON r.id=f.run_id
 WHERE f.tenant_id=$1 AND r.deleted_at IS NULL AND f.results @> jsonb_build_array(jsonb_build_object('taskId',$2::text))
 ORDER BY f.submitted_at DESC,f.id`, tenant, id)
	if err != nil {
		return task, err
	}
	defer rows.Close()
	for rows.Next() {
		var source WorkflowTaskSource
		if err := rows.Scan(&source.RunID, &source.Name, &source.SubmittedAt); err != nil {
			return task, err
		}
		task.WorkflowSources = append(task.WorkflowSources, source)
	}
	return task, rows.Err()
}

// CreateTask records a task for the caller's workspace. Its project and assignee must already
// belong to that workspace.
func (s *Store) CreateTask(ctx context.Context, tenant, user string, in TaskInput) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
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
		if e != nil || in.Assignee == nil {
			return e
		}
		return notifyAssigned(ctx, tx, tenant, taskID, user, *in.Assignee, task.Version)
	})
	return out, e
}

// UpdateTask replaces the title, description and project under a version check. The status and
// assignee have their own writes so a stale form cannot silently undo them.
func (s *Store) UpdateTask(ctx context.Context, tenant, id string, in TaskInput) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockTask(ctx, tx, tenant, id, in.Version); e != nil {
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
// accepted, so 待验收 cannot be skipped and a finished task cannot silently reopen. Ending the task
// is limited by canCloseTask; anyone else gets ErrForbidden. While an Agent turn runs the run owns
// the status, and a manual move returns ErrRunActive; while a workflow handles the task it returns
// ErrWorkflowActive, so the page can say which of the two to wait for.
func (s *Store) SetTaskStatus(ctx context.Context, a Access, id, status string, version int64) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		task, e := lockTask(ctx, tx, a.TenantID, id, version)
		if e != nil {
			return e
		}
		if status == TaskDone || status == TaskCancelled {
			if !canCloseTask(a, task) {
				return ErrForbidden
			}
		}
		var handling bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM v2_task_workflow_runs WHERE tenant_id=$1 AND task_id=$2 AND status='running')`, a.TenantID, id).Scan(&handling); err != nil {
			return err
		}
		if handling {
			return ErrWorkflowActive
		}
		if task.ActiveRun != nil && task.ActiveRun.State == TaskRunRunning {
			return ErrRunActive
		}
		if !CanTransitionTask(task.Status, status) {
			return ErrInvalidTransition
		}
		task, e = writeTaskStatus(ctx, tx, id, status, a.UserID)
		out = task
		return e
	})
	return out, e
}

// TransitionTaskStatus moves a task on behalf of its Agent run, by CanRunTransitionTask. from guards
// against a stale run overwriting a person's change.
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
		if !CanRunTransitionTask(from, to) {
			return ErrInvalidTransition
		}
		task, e = writeTaskStatus(ctx, tx, id, to, "")
		out = task
		return e
	})
	return out, e
}

// SetTaskAssignee replaces or clears the typed assignee under a version check. Assigning a squad
// puts the task in front of every member; none of them is picked for it. A new assignee is
// notified unless actor, the person assigning, is them; re-sending the same assignee is not new.
func (s *Store) SetTaskAssignee(ctx context.Context, tenant, id, actor string, ref *TaskAssigneeRef, version int64) (Task, error) {
	var out Task
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, e := lockTask(ctx, tx, tenant, id, version)
		if e != nil {
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
		if e != nil || ref == nil || (before.Assignee != nil && before.Assignee.Type == ref.Type && strings.EqualFold(before.Assignee.ID, ref.ID)) {
			return e
		}
		return notifyAssigned(ctx, tx, tenant, id, actor, *ref, task.Version)
	})
	return out, e
}

// WorkflowTaskSource is one persisted report association, independent of task text.
type WorkflowTaskSource struct {
	RunID       string     `json:"runId"`
	Name        string     `json:"name"`
	SubmittedAt *time.Time `json:"submittedAt"`
}
