package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// ScheduledTaskStartPrefix cannot occur in a client Idempotency-Key HTTP header.
const ScheduledTaskStartPrefix = "\nschedule:"

// DemoSeedSessionPrefix identifies nonexecuting Dashboard history examples, not fixture-backed runs.
const DemoSeedSessionPrefix = "demo-dashboard-"

// Task run end reasons; the empty reason is a live run.
const (
	TaskRunStopped     = "stopped"
	TaskRunReplaced    = "replaced"
	TaskRunClosed      = "closed"
	TaskRunInterrupted = "interrupted"
)

// Task run states as pages read them: a turn is running, the session waits for the next turn, or
// the session can take no more turns.
const (
	TaskRunRunning = "running"
	TaskRunIdle    = "idle"
	TaskRunEnded   = "ended"
)

var (
	// ErrRunActive reports a change that a running Agent turn forbids.
	ErrRunActive = errors.New("task run turn active")
	// ErrInvalidMention reports an @ target that is not a live member or installed Agent.
	ErrInvalidMention = errors.New("invalid mention")
)

// TaskRun is one execution session of a task's Agent. The whole workspace reads it; the session
// belongs to StartedBy, the only person who can send it another turn.
type TaskRun struct {
	ID            string     `json:"id"`
	TaskID        string     `json:"taskId"`
	TenantID      string     `json:"-"`
	StartedBy     string     `json:"startedBy"`
	StartedByName string     `json:"startedByName"`
	Scheduled     bool       `json:"scheduled,omitempty"`
	DemoSeed      bool       `json:"demoSeed"`
	StartKey      string     `json:"-"`
	ExecutorID    string     `json:"executorId"`
	ExecutorName  string     `json:"executorName"`
	SessionID     string     `json:"-"`
	LastTurnID    string     `json:"-"`
	State         string     `json:"state"`
	LastResult    string     `json:"lastResult"`
	FailureCode   string     `json:"failureCode"`
	EndReason     string     `json:"endReason"`
	CreatedAt     time.Time  `json:"createdAt"`
	EndedAt       *time.Time `json:"endedAt"`
	turnActive    bool
}

// Live reports whether the run's session can still take turns.
func (r TaskRun) Live() bool { return r.EndedAt == nil }

// TurnActive reports whether one of the run's turns is still running.
func (r TaskRun) TurnActive() bool { return r.EndedAt == nil && r.turnActive }

// TaskMention is one typed @ target of a comment. The name is resolved when the comment is written.
type TaskMention struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// TaskCommentWorkflowRun summarizes the workflow run started by a @ workflow comment.
type TaskCommentWorkflowRun struct {
	ID           string    `json:"id"`
	WorkflowID   string    `json:"workflowId"`
	WorkflowName string    `json:"workflowName"`
	Version      string    `json:"version"`
	Status       string    `json:"status"`
	Output       *string   `json:"output"`
	Error        *string   `json:"error"`
	Deleted      bool      `json:"deleted"`
	CreatedAt    time.Time `json:"createdAt"`
}

// TaskComment is a person's entry in a task's timeline. RunID is set when it was sent to the Agent,
// and WorkflowRun is set when the comment launched a workflow via @ workflow.
type TaskComment struct {
	ID          string                  `json:"id"`
	TaskID      string                  `json:"taskId"`
	AuthorID    string                  `json:"authorId"`
	AuthorName  string                  `json:"authorName"`
	Body        string                  `json:"body"`
	Mentions    []TaskMention           `json:"mentions"`
	RunID       string                  `json:"runId"`
	WorkflowRun *TaskCommentWorkflowRun `json:"workflowRun,omitempty"`
	CreatedAt   time.Time               `json:"createdAt"`
}

const taskRunColumns = `r.id, r.task_id, r.tenant_id::text, r.started_by::text, u.display_name, r.start_key,
	r.executor_id, r.executor_name, COALESCE(r.session_id, ''), r.last_turn_id, r.turn_active,
	r.last_result, r.failure_code, r.end_reason, r.created_at, r.ended_at`

const taskRunJoins = `FROM v2_task_runs r JOIN users u ON u.id = r.started_by`

func scanTaskRun(row pgx.Row) (TaskRun, error) {
	var r TaskRun
	e := row.Scan(&r.ID, &r.TaskID, &r.TenantID, &r.StartedBy, &r.StartedByName, &r.StartKey,
		&r.ExecutorID, &r.ExecutorName, &r.SessionID, &r.LastTurnID, &r.turnActive,
		&r.LastResult, &r.FailureCode, &r.EndReason, &r.CreatedAt, &r.EndedAt)
	if errors.Is(e, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if e != nil {
		return r, e
	}
	r.Scheduled = strings.HasPrefix(r.StartKey, ScheduledTaskStartPrefix)
	r.DemoSeed = isDemoSeedRun(r.StartKey, r.SessionID)
	switch {
	case r.EndedAt != nil:
		r.State = TaskRunEnded
	case r.turnActive:
		r.State = TaskRunRunning
	default:
		r.State = TaskRunIdle
	}
	return r, nil
}

// Both persisted identities must match: a caller-chosen start key alone is not seed provenance.
func isDemoSeedRun(startKey, sessionID string) bool {
	return startKey == sessionID && strings.HasPrefix(sessionID, DemoSeedSessionPrefix) && len(sessionID) > len(DemoSeedSessionPrefix)
}

func taskRuns(ctx context.Context, q dbTX, where string, args ...any) ([]TaskRun, error) {
	rows, e := q.Query(ctx, `SELECT `+taskRunColumns+` `+taskRunJoins+` WHERE `+where+` ORDER BY r.created_at, r.id`, args...)
	if e != nil {
		return nil, fmt.Errorf("list task runs: %w", e)
	}
	defer rows.Close()
	out := []TaskRun{}
	for rows.Next() {
		r, e := scanTaskRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const taskCommentSelect = `
	SELECT c.id, c.task_id, c.author_id::text, u.display_name, c.body, c.mentions,
		COALESCE(c.run_id::text, ''), c.created_at,
		COALESCE(c.workflow_run_id::text, ''), COALESCE(r.workflow_id::text, ''),
		COALESCE(w.name, ''), COALESCE(s.version, ''),
		CASE WHEN r.id IS NULL THEN '' ELSE ` + derivedRunStatus + ` END,
		CASE WHEN r.deleted_at IS NULL THEN r.output END,
		CASE WHEN r.deleted_at IS NULL THEN r.error END,
		COALESCE(r.deleted_at IS NOT NULL, false), r.created_at
	FROM v2_task_comments c
	JOIN users u ON u.id = c.author_id
	LEFT JOIN v2_workflow_runs r ON r.id = c.workflow_run_id AND r.tenant_id = c.tenant_id
	LEFT JOIN v2_workflows w ON w.id = r.workflow_id
	LEFT JOIN v2_workflow_snapshots s ON s.id = r.snapshot_id`

// TaskTimeline returns a task's runs and comments, oldest first, to a current workspace member.
func (s *Store) TaskTimeline(ctx context.Context, tenant, taskID string) ([]TaskRun, []TaskComment, error) {
	if _, e := s.Task(ctx, tenant, taskID); e != nil {
		return nil, nil, e
	}
	runs, e := taskRuns(ctx, s.pool, `r.tenant_id = $1 AND r.task_id = $2`, tenant, taskID)
	if e != nil {
		return nil, nil, e
	}
	rows, e := s.pool.Query(ctx, taskCommentSelect+`
		WHERE c.tenant_id = $1 AND c.task_id = $2 ORDER BY c.created_at, c.id`, tenant, taskID)
	if e != nil {
		return nil, nil, fmt.Errorf("list task comments: %w", e)
	}
	defer rows.Close()
	comments := []TaskComment{}
	for rows.Next() {
		c, e := scanTaskComment(rows)
		if e != nil {
			return nil, nil, e
		}
		comments = append(comments, c)
	}
	return runs, comments, rows.Err()
}

func scanTaskComment(row pgx.Row) (TaskComment, error) {
	var c TaskComment
	var mentions []byte
	var wfRunID, wfID, wfName, wfVersion, wfStatus string
	var wfOutput, wfErr *string
	var wfDeleted bool
	var wfCreatedAt *time.Time
	if e := row.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.AuthorName, &c.Body, &mentions, &c.RunID, &c.CreatedAt,
		&wfRunID, &wfID, &wfName, &wfVersion, &wfStatus, &wfOutput, &wfErr, &wfDeleted, &wfCreatedAt); e != nil {
		if errors.Is(e, pgx.ErrNoRows) {
			return c, ErrNotFound
		}
		return c, e
	}
	c.Mentions = []TaskMention{}
	if e := json.Unmarshal(mentions, &c.Mentions); e != nil {
		return c, fmt.Errorf("decode comment mentions: %w", e)
	}
	if wfRunID != "" && wfCreatedAt != nil {
		c.WorkflowRun = &TaskCommentWorkflowRun{
			ID:           wfRunID,
			WorkflowID:   wfID,
			WorkflowName: wfName,
			Version:      wfVersion,
			Status:       wfStatus,
			Output:       wfOutput,
			Error:        wfErr,
			Deleted:      wfDeleted,
			CreatedAt:    *wfCreatedAt,
		}
	}
	return c, nil
}

// TaskRun returns one run of the tenant's task without a membership check; callers check first.
func (s *Store) TaskRun(ctx context.Context, tenant, taskID, runID string) (TaskRun, error) {
	return scanTaskRun(s.pool.QueryRow(ctx, `SELECT `+taskRunColumns+` `+taskRunJoins+`
		WHERE r.tenant_id = $1 AND r.task_id = $2 AND r.id = $3`, tenant, taskID, runID))
}

// LiveTaskRun returns the task's live run, if any.
func (s *Store) LiveTaskRun(ctx context.Context, tenant, taskID string) (TaskRun, bool, error) {
	r, e := scanTaskRun(s.pool.QueryRow(ctx, `SELECT `+taskRunColumns+` `+taskRunJoins+`
		WHERE r.tenant_id = $1 AND r.task_id = $2 AND r.ended_at IS NULL`, tenant, taskID))
	if errors.Is(e, ErrNotFound) {
		return r, false, nil
	}
	return r, e == nil, e
}

// TaskRunByKey finds the run a person's start request with this key created.
func (s *Store) TaskRunByKey(ctx context.Context, tenant, taskID, user, key string) (TaskRun, error) {
	return scanTaskRun(s.pool.QueryRow(ctx, `SELECT `+taskRunColumns+` `+taskRunJoins+`
		WHERE r.tenant_id = $1 AND r.task_id = $2 AND r.started_by = $3 AND r.start_key = $4`, tenant, taskID, user, key))
}

// NewTaskRun is a run about to ask the execution service for its first turn.
type NewTaskRun struct {
	ExpectedVersion                                                 int64
	TenantID, TaskID, StartedBy, StartKey, ExecutorID, ExecutorName string
}

// CreateTaskRun records a run before its first turn starts. Another live run of the task returns
// ErrRunActive: the caller ends it first.
func (s *Store) CreateTaskRun(ctx context.Context, in NewTaskRun) (TaskRun, error) {
	var id string
	query := `INSERT INTO v2_task_runs (task_id,tenant_id,started_by,start_key,executor_id,executor_name) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`
	args := []any{in.TaskID, in.TenantID, in.StartedBy, in.StartKey, in.ExecutorID, in.ExecutorName}
	if in.ExpectedVersion != 0 {
		// Reserve against the version whose assignment/status was checked. FOR UPDATE
		// serializes this admission with reassignment and manual state transitions.
		query = `INSERT INTO v2_task_runs (task_id,tenant_id,started_by,start_key,executor_id,executor_name)
 SELECT $1::uuid,$2::uuid,$3::uuid,$4,$5,$6 FROM
 (SELECT id FROM v2_tasks WHERE id=$1 AND tenant_id=$2 AND version=$7 FOR UPDATE) eligible RETURNING id`
		args = append(args, in.ExpectedVersion)
	}
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var locked string
		err := tx.QueryRow(ctx, `SELECT id::text FROM v2_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, in.TenantID, in.TaskID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM v2_task_workflow_runs WHERE tenant_id=$1 AND task_id=$2 AND status='running')`, in.TenantID, in.TaskID).Scan(&live); err != nil {
			return err
		}
		if live {
			return ErrRunActive
		}
		return tx.QueryRow(ctx, query, args...).Scan(&id)
	})
	if errors.Is(e, pgx.ErrNoRows) {
		return TaskRun{}, ErrConflict
	}
	var pg *pgconn.PgError
	if errors.As(e, &pg) && pg.Code == "23505" {
		return TaskRun{}, ErrRunActive
	}
	if e != nil {
		return TaskRun{}, fmt.Errorf("insert task run: %w", e)
	}
	return s.TaskRun(ctx, in.TenantID, in.TaskID, id)
}

// DeleteTaskRun removes a run whose first turn the execution service refused.
func (s *Store) DeleteTaskRun(ctx context.Context, id string) error {
	_, e := s.pool.Exec(ctx, `DELETE FROM v2_task_runs WHERE id = $1 AND session_id IS NULL`, id)
	return e
}

// RecordTaskTurn notes an accepted turn: the run's session and its now running last turn.
func (s *Store) RecordTaskTurn(ctx context.Context, runID, sessionID, turnID string) error {
	tag, e := s.pool.Exec(ctx, `
		UPDATE v2_task_runs SET session_id = $2, last_turn_id = $3, turn_active = (ended_at IS NULL),
            last_result = CASE WHEN ended_at IS NULL THEN '' ELSE last_result END,
            failure_code = CASE WHEN ended_at IS NULL THEN '' ELSE failure_code END
		WHERE id = $1 AND (session_id IS NULL OR session_id = $2)`, runID, sessionID, turnID)
	if e != nil {
		return fmt.Errorf("record task turn: %w", e)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SettleTaskTurn records a turn's result once; it reports false when the turn was already settled
// or is no longer the run's latest.
func (s *Store) SettleTaskTurn(ctx context.Context, runID, turnID, result, failureCode string) (bool, error) {
	tag, e := s.pool.Exec(ctx, `
		UPDATE v2_task_runs SET turn_active = false, last_result = $3, failure_code = $4
		WHERE id = $1 AND last_turn_id = $2 AND turn_active`, runID, turnID, result, failureCode)
	if e != nil {
		return false, fmt.Errorf("settle task turn: %w", e)
	}
	return tag.RowsAffected() == 1, nil
}

// EndTaskRun records that the run's session can take no more turns. A reason already written wins,
// so a stop is not later reported as an interruption.
func (s *Store) EndTaskRun(ctx context.Context, runID, reason string) error {
	_, e := s.pool.Exec(ctx, `
		UPDATE v2_task_runs SET ended_at = now(), turn_active = false,
			end_reason = CASE WHEN end_reason = '' THEN $2 ELSE end_reason END
		WHERE id = $1 AND ended_at IS NULL`, runID, reason)
	if e != nil {
		return fmt.Errorf("end task run: %w", e)
	}
	return nil
}

// MarkTaskRunEnding writes the reason before the session is ended, so a page reading the run on
// session_ended already sees why. A failed end withdraws it with ClearTaskRunEnding.
func (s *Store) MarkTaskRunEnding(ctx context.Context, runID, reason string) error {
	_, e := s.pool.Exec(ctx, `UPDATE v2_task_runs SET end_reason = $2 WHERE id = $1 AND ended_at IS NULL`, runID, reason)
	return e
}

// ClearTaskRunEnding withdraws a reason whose end did not happen.
func (s *Store) ClearTaskRunEnding(ctx context.Context, runID string) error {
	_, e := s.pool.Exec(ctx, `UPDATE v2_task_runs SET end_reason = '' WHERE id = $1 AND ended_at IS NULL`, runID)
	return e
}

// InterruptTaskRuns ends every run a previous process left live and returns those whose turn was
// still running. Their tasks return to pending in the same transaction.
func (s *Store) InterruptTaskRuns(ctx context.Context) ([]TaskRun, error) {
	var out []TaskRun
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		live, e := taskRuns(ctx, tx, `r.ended_at IS NULL`)
		if e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_task_runs SET ended_at = now(), end_reason = 'interrupted',
				last_result = CASE WHEN turn_active THEN 'interrupted' ELSE last_result END, turn_active = false
			WHERE ended_at IS NULL`); e != nil {
			return fmt.Errorf("interrupt task runs: %w", e)
		}
		for _, r := range live {
			if !r.turnActive {
				continue
			}
			var status string
			if e := tx.QueryRow(ctx, `SELECT status FROM v2_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, r.TenantID, r.TaskID).Scan(&status); e != nil {
				return e
			}
			if status == TaskRunning {
				if _, e := writeTaskStatus(ctx, tx, r.TaskID, TaskPending, ""); e != nil {
					return e
				}
			}
			out = append(out, r)
		}
		return nil
	})
	return out, e
}

// NewTaskComment is a comment to record; Mentions carry typed ids only.
type NewTaskComment struct {
	TenantID, TaskID, AuthorID, CreateKey, Body string
	Mentions                                    []TaskMention
	RunID, TurnID                               string
}

type commentQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// TaskCommentByKey finds the comment a person's request with this key created.
func (s *Store) TaskCommentByKey(ctx context.Context, tenant, taskID, author, key string) (TaskComment, error) {
	return taskCommentByKey(ctx, s.pool, tenant, taskID, author, key)
}

func taskCommentByKey(ctx context.Context, q commentQueryer, tenant, taskID, author, key string) (TaskComment, error) {
	return scanTaskComment(q.QueryRow(ctx, taskCommentSelect+`
		WHERE c.tenant_id = $1 AND c.task_id = $2 AND c.author_id = $3 AND c.create_key = $4`, tenant, taskID, author, key))
}

// ResolveTaskMentions checks each @ target against the workspace and fills in its current name.
func (s *Store) ResolveTaskMentions(ctx context.Context, tenant string, in []TaskMention) ([]TaskMention, error) {
	out := make([]TaskMention, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		key := m.Type + "/" + m.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		var name string
		var e error
		switch m.Type {
		case "user":
			if e = requireActiveUser(ctx, s.pool, tenant, m.ID); e == nil {
				e = s.pool.QueryRow(ctx, `SELECT display_name FROM users WHERE id = $1`, m.ID).Scan(&name)
			}
		case "agent":
			if e = requireActiveAgent(ctx, s.pool, tenant, m.ID); e == nil {
				e = s.pool.QueryRow(ctx, `SELECT name FROM v2_space_agents WHERE id = $1`, m.ID).Scan(&name)
			}
		case "workflow":
			e = s.pool.QueryRow(ctx, `
				SELECT name FROM v2_workflows
				WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL AND published_snapshot_id IS NOT NULL`,
				tenant, m.ID).Scan(&name)
			if errors.Is(e, pgx.ErrNoRows) {
				return nil, ErrInvalidMention
			}
		default:
			return nil, ErrInvalidMention
		}
		if errors.Is(e, ErrUserUnavailable) || errors.Is(e, ErrAgentUnavailable) {
			return nil, ErrInvalidMention
		}
		if e != nil {
			return nil, fmt.Errorf("resolve mention: %w", e)
		}
		out = append(out, TaskMention{Type: m.Type, ID: m.ID, DisplayName: name})
	}
	return out, nil
}

// CreateTaskComment records a comment once per key; a retry returns the comment it made.
func (s *Store) CreateTaskComment(ctx context.Context, in NewTaskComment) (TaskComment, error) {
	mentions, e := marshalTaskMentions(in.Mentions)
	if e != nil {
		return TaskComment{}, e
	}
	var out TaskComment
	e = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `
			INSERT INTO v2_task_comments (task_id, tenant_id, author_id, create_key, body, mentions, run_id, turn_id)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8)
			ON CONFLICT (task_id, author_id, create_key) DO NOTHING`,
			in.TaskID, in.TenantID, in.AuthorID, in.CreateKey, in.Body, mentions, in.RunID, in.TurnID)
		if e != nil {
			return fmt.Errorf("insert task comment: %w", e)
		}
		out, e = taskCommentByKey(ctx, tx, in.TenantID, in.TaskID, in.AuthorID, in.CreateKey)
		if e != nil || tag.RowsAffected() == 0 {
			return e
		}
		return notifyMentioned(ctx, tx, in.TenantID, in.TaskID, out)
	})
	return out, e
}

func marshalTaskMentions(in []TaskMention) ([]byte, error) {
	if in == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(in)
}

// NewTaskWorkflowComment carries a comment and validated published snapshot to create atomically.
type NewTaskWorkflowComment struct {
	TenantID   string
	TaskID     string
	AuthorID   string
	CreateKey  string
	Body       string
	Mentions   []TaskMention
	WorkflowID string
	SnapshotID string
	Input      *string
}

// CreateTaskWorkflowComment atomically creates a pending workflow run as the commenter and binds
// a new comment to it. Retries with the same key return the existing comment (fresh=false).
// Unlike CreateTaskWorkflowRun, this leaves task status and handling locks untouched.
func (s *Store) CreateTaskWorkflowComment(ctx context.Context, in NewTaskWorkflowComment) (TaskComment, bool, error) {
	var comment TaskComment
	var fresh bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var taskStatus, projectID string
		err := tx.QueryRow(ctx, `
			SELECT status, project_id::text FROM v2_tasks
			WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, in.TenantID, in.TaskID).Scan(&taskStatus, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock task for comment workflow: %w", err)
		}
		if existing, err := taskCommentByKey(ctx, tx, in.TenantID, in.TaskID, in.AuthorID, in.CreateKey); err == nil {
			comment = existing
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		if taskStatus == TaskDone || taskStatus == TaskCancelled {
			return ErrInvalidTransition
		}
		run, err := createWorkflowRun(ctx, tx, in.TenantID, in.WorkflowID, in.SnapshotID, WorkflowRunInput{
			ProjectID: projectID,
			Input:     in.Input,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE v2_workflow_runs SET started_by = $2::uuid WHERE tenant_id = $1 AND id = $3`,
			in.TenantID, in.AuthorID, run.ID); err != nil {
			return fmt.Errorf("bind comment workflow starter: %w", err)
		}
		mentions, err := marshalTaskMentions(in.Mentions)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO v2_task_comments (task_id, tenant_id, author_id, create_key, body, mentions, workflow_run_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			in.TaskID, in.TenantID, in.AuthorID, in.CreateKey, in.Body, mentions, run.ID); err != nil {
			return fmt.Errorf("insert workflow comment: %w", err)
		}
		comment, err = taskCommentByKey(ctx, tx, in.TenantID, in.TaskID, in.AuthorID, in.CreateKey)
		if err != nil {
			return err
		}
		fresh = true
		return notifyMentioned(ctx, tx, in.TenantID, in.TaskID, comment)
	})
	return comment, fresh, err
}

// TaskAgentIdentifier returns the plugin identity (namespace/identifier) of a Space Agent.
func (s *Store) TaskAgentIdentifier(ctx context.Context, tenant, agentID string) (string, error) {
	var id string
	e := s.pool.QueryRow(ctx, `SELECT source_namespace || '/' || identifier FROM v2_space_agents
		WHERE id = $1 AND tenant_id = $2`, agentID, tenant).Scan(&id)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, e
}

// CollaborationAgents lists the workspace's installed, system-provided Space Agents.
func (s *Store) CollaborationAgents(ctx context.Context, tenant string) ([]TaskAssignee, error) {
	rows, e := s.pool.Query(ctx, `
		SELECT id::text, name, source_namespace || '/' || identifier FROM v2_space_agents
		WHERE tenant_id = $1 AND kind = 'agent' AND status = 'active' AND observed_state = 'installed'
			AND requested_by_user_id IS NULL
		ORDER BY name, id`, tenant)
	if e != nil {
		return nil, fmt.Errorf("list collaboration agents: %w", e)
	}
	defer rows.Close()
	out := []TaskAssignee{}
	for rows.Next() {
		a := TaskAssignee{Type: AssigneeAgent}
		if e := rows.Scan(&a.ID, &a.DisplayName, &a.Identifier); e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PendingMention is the latest comment that @-mentioned someone on an open task they have not
// commented on since. It is how a mention reaches a person: the task waits on them until they reply.
type PendingMention struct {
	TaskID     string    `json:"taskId"`
	CommentID  string    `json:"commentId"`
	AuthorID   string    `json:"authorId"`
	AuthorName string    `json:"authorName"`
	CreatedAt  time.Time `json:"createdAt"`
}

// PendingMentions lists, one per task, the mentions of the caller still waiting on their reply.
// A mention of oneself, and mentions on done or cancelled tasks, wait on nobody.
func (s *Store) PendingMentions(ctx context.Context, tenant, caller string) ([]PendingMention, error) {
	rows, e := s.pool.Query(ctx, `
		SELECT DISTINCT ON (c.task_id) c.task_id, c.id, c.author_id::text, u.display_name, c.created_at
		FROM v2_task_comments c
		JOIN v2_tasks t ON t.id = c.task_id
		JOIN users u ON u.id = c.author_id
		WHERE c.tenant_id = $1 AND t.status NOT IN ('done', 'cancelled') AND c.author_id <> $2::uuid
			AND c.mentions @> jsonb_build_array(jsonb_build_object('type', 'user', 'id', $2::text))
			AND NOT EXISTS (SELECT 1 FROM v2_task_comments r
				WHERE r.task_id = c.task_id AND r.author_id = $2::uuid AND r.created_at > c.created_at)
		ORDER BY c.task_id, c.created_at DESC, c.id DESC`, tenant, caller)
	if e != nil {
		return nil, fmt.Errorf("list pending mentions: %w", e)
	}
	defer rows.Close()
	out := []PendingMention{}
	for rows.Next() {
		var m PendingMention
		if e := rows.Scan(&m.TaskID, &m.CommentID, &m.AuthorID, &m.AuthorName, &m.CreatedAt); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InSquad reports whether the user is a current human member of the squad.
func (s *Store) InSquad(ctx context.Context, tenant, squad, user string) (bool, error) {
	return inSquad(ctx, s.pool, tenant, squad, user)
}

func inSquad(ctx context.Context, q dbTX, tenant, squad, user string) (bool, error) {
	var member bool
	e := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM v2_squad_members m JOIN v2_squads q ON q.id = m.squad_id
			WHERE q.tenant_id = $1 AND m.squad_id = $2 AND m.user_id = $3
				AND m.member_type = 'human' AND m.ended_at IS NULL)`, tenant, squad, user).Scan(&member)
	if e != nil {
		return false, fmt.Errorf("look up squad member: %w", e)
	}
	return member, nil
}

// MySquadTask is a task assigned to a squad the caller belongs to. Waiting says the caller has yet
// to act on it: the task is still being worked (待处理 or 进行中), someone else created it, and they
// have neither commented nor started a run. It is how a squad task reaches each member: it waits on
// them until they do. A task in 待验收 waits on whoever can confirm it, never on the squad.
type MySquadTask struct {
	TaskID    string `json:"taskId"`
	SquadID   string `json:"squadId"`
	SquadName string `json:"squadName"`
	Waiting   bool   `json:"waiting"`
}

// MySquadTasks lists the tasks of the caller's squads, each with whether it still waits on them.
func (s *Store) MySquadTasks(ctx context.Context, tenant, caller string) ([]MySquadTask, error) {
	rows, e := s.pool.Query(ctx, `
		SELECT t.id, q.id, q.name,
			t.status IN ('pending', 'running') AND t.created_by <> $2::uuid
				AND NOT EXISTS (SELECT 1 FROM v2_task_comments c WHERE c.task_id = t.id AND c.author_id = $2::uuid)
				AND NOT EXISTS (SELECT 1 FROM v2_task_runs r WHERE r.task_id = t.id AND r.started_by = $2::uuid)
		FROM v2_tasks t
		JOIN v2_squads q ON q.id = t.assignee_id
		JOIN v2_squad_members m ON m.squad_id = q.id
		WHERE t.tenant_id = $1 AND t.assignee_type = 'squad' AND m.user_id = $2::uuid
			AND m.member_type = 'human' AND m.ended_at IS NULL
		ORDER BY t.updated_at DESC, t.id DESC`, tenant, caller)
	if e != nil {
		return nil, fmt.Errorf("list my squad tasks: %w", e)
	}
	defer rows.Close()
	out := []MySquadTask{}
	for rows.Next() {
		var m MySquadTask
		if e := rows.Scan(&m.TaskID, &m.SquadID, &m.SquadName, &m.Waiting); e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SettleDeliveredTaskTurn advances the task and releases the run's active turn atomically.
// Only the owning active turn can change the task, and only after its delivery is durable.
func (s *Store) SettleDeliveredTaskTurn(ctx context.Context, tenant, taskID, runID, turnID, result, failureCode string) (bool, error) {
	var settled bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var from string
		if err := tx.QueryRow(ctx, `SELECT status FROM v2_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, tenant, taskID).Scan(&from); err != nil {
			return err
		}
		var ending, session string
		err := tx.QueryRow(ctx, `SELECT end_reason,COALESCE(session_id,'') FROM v2_task_runs
			WHERE id=$1 AND tenant_id=$2 AND task_id=$3 AND last_turn_id=$4 AND turn_active AND ended_at IS NULL
			FOR UPDATE`, runID, tenant, taskID, turnID).Scan(&ending, &session)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var delivery DeliveryStatus
		err = tx.QueryRow(ctx, `SELECT status FROM v2_turn_deliveries WHERE
			tenant_id=$1 AND source_kind='task' AND source_id=$2 AND turn_id=$3 AND session_id=$4`,
			tenant, runID, turnID, session).Scan(&delivery)
		if errors.Is(err, pgx.ErrNoRows) {
			// The runtime can retain a terminal even when the delivery write itself fails.
			// Only that failure may release a task without a delivery record.
			if result != string(execution.StatusFailed) || failureCode != execution.FailureDeliveryUnavailable {
				return ErrRunActive
			}
			var ended bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM v2_execution_turns
				WHERE id=$1 AND session_id=$2 AND tenant_id=$3 AND ended_at IS NOT NULL
				AND data#>>'{Result,Status}'=$4 AND data#>>'{Result,FailureCode}'=$5)`,
				turnID, session, tenant, result, failureCode).Scan(&ended); err != nil {
				return err
			}
			if !ended {
				return ErrRunActive
			}
			delivery = DeliveryFailed
		} else if err != nil {
			return err
		}
		to := TaskReview
		if delivery == DeliveryFailed || result == "cancelled" || result == "interrupted" || ending != "" {
			to = TaskPending
		}
		if _, err := tx.Exec(ctx, `UPDATE v2_task_runs SET turn_active=false,last_result=$2,failure_code=$3 WHERE id=$1`, runID, result, failureCode); err != nil {
			return err
		}
		if CanRunTransitionTask(from, to) {
			if _, err := writeTaskStatus(ctx, tx, taskID, to, ""); err != nil {
				return err
			}
		}
		settled = true
		return nil
	})
	return settled, err
}
