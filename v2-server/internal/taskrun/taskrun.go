// Package taskrun runs a task's Agent for the collaboration page. A run is one execution session,
// started by one person under their own scope; the whole workspace reads it, but only that person
// can send it another turn, because the turn would run on their identity. Others start their own
// run instead, which ends the previous idle one.
//
// A task assigned to an Agent runs that Agent, and any member may start it. A task assigned to a
// person runs the Agent that person picks, and only they start it: the work is theirs.
//
// The run moves its task: starting or following up puts it in 进行中, a finished turn (completed or
// failed) hands it to 待验收, and a stopped or interrupted turn returns it to 待处理. While a turn
// runs, nobody moves the task by hand. Runs never outlive the process: Interrupt ends what a
// previous process left live before the host serves requests.
package taskrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Errors the HTTP layer maps to stable codes.
var (
	// ErrNotAgentTask reports a run on a task assigned to no Agent, person or squad.
	ErrNotAgentTask = errors.New("task not assigned to an agent, a person or a squad")
	// ErrNotAssignee reports a run on a person's or squad's task by someone outside it.
	ErrNotAssignee = errors.New("task assigned to someone else")
	// ErrNoExecutor reports an Agent with no runnable executor, or an executor of another Agent.
	ErrNoExecutor = errors.New("no executor for the task's agent")
	// ErrTaskClosed reports a run on a task that is done or cancelled.
	ErrTaskClosed = errors.New("task closed")
	// ErrRunOwned reports a follow-up to a run someone else started.
	ErrRunOwned = errors.New("run started by someone else")
	// ErrNoLiveRun reports a stop with no live run to act on.
	ErrNoLiveRun = errors.New("no live run")
)

// Store is the persistence a Host needs; *store.Store implements it.
type Store interface {
	Task(ctx context.Context, tenant, caller, id string) (store.Task, error)
	InSquad(ctx context.Context, tenant, squad, user string) (bool, error)
	TransitionTaskStatus(ctx context.Context, tenant, id, from, to string) (store.Task, error)
	TaskRun(ctx context.Context, tenant, taskID, runID string) (store.TaskRun, error)
	LiveTaskRun(ctx context.Context, tenant, taskID string) (store.TaskRun, bool, error)
	TaskRunByKey(ctx context.Context, tenant, taskID, user, key string) (store.TaskRun, error)
	CreateTaskRun(ctx context.Context, in store.NewTaskRun) (store.TaskRun, error)
	DeleteTaskRun(ctx context.Context, id string) error
	RecordTaskTurn(ctx context.Context, runID, sessionID, turnID string) error
	SettleTaskTurn(ctx context.Context, runID, turnID, result, failureCode string) (bool, error)
	EndTaskRun(ctx context.Context, runID, reason string) error
	MarkTaskRunEnding(ctx context.Context, runID, reason string) error
	ClearTaskRunEnding(ctx context.Context, runID string) error
	InterruptTaskRuns(ctx context.Context) ([]store.TaskRun, error)
	TaskCommentByKey(ctx context.Context, tenant, taskID, author, key string) (store.TaskComment, error)
	ResolveTaskMentions(ctx context.Context, tenant string, in []store.TaskMention) ([]store.TaskMention, error)
	CreateTaskComment(ctx context.Context, in store.NewTaskComment) (store.TaskComment, error)
}

// Host runs task Agents on the shared execution service.
type Host struct {
	store   Store
	exec    execution.Service
	catalog execution.Catalog
	log     *slog.Logger

	ctx     context.Context
	stop    context.CancelFunc
	workers sync.WaitGroup

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns a Host. Call Interrupt before serving requests and Close on shutdown.
func New(s Store, exec execution.Service, catalog execution.Catalog, log *slog.Logger) *Host {
	if log == nil {
		log = slog.Default()
	}
	h := &Host{store: s, exec: exec, catalog: catalog, log: log, locks: map[string]*sync.Mutex{}}
	h.ctx, h.stop = context.WithCancel(context.Background())
	return h
}

// Close stops observing turns and waits for the observers. It does not stop the turns: the
// execution service ends them on its own shutdown, and the next start interrupts the runs.
func (h *Host) Close() {
	h.stop()
	h.workers.Wait()
}

// Interrupt ends every run a previous process left live; a task whose turn was running goes back
// to 待处理. It must run before the host serves requests.
func (h *Host) Interrupt(ctx context.Context) error {
	runs, e := h.store.InterruptTaskRuns(ctx)
	if e != nil {
		return fmt.Errorf("interrupt task runs: %w", e)
	}
	for _, r := range runs {
		h.moveTask(ctx, r.TenantID, r.TaskID, store.TaskRunning, store.TaskPending)
	}
	if len(runs) != 0 {
		h.log.Info("interrupted task runs left by the previous process", "runs", len(runs))
	}
	return nil
}

// lock serializes admission on one task, so two starts cannot both find no live run. v2-server is
// one process, which is what makes an in-memory lock enough; the entries stay as small as tasks.
func (h *Host) lock(taskID string) func() {
	h.mu.Lock()
	l := h.locks[taskID]
	if l == nil {
		l = &sync.Mutex{}
		h.locks[taskID] = l
	}
	h.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Executors lists the executors that can run the task: for an Agent's task the catalog entries of
// its plugin, for a person's or squad's task every executor the caller can run, since they pick
// the Agent.
func (h *Host) Executors(ctx context.Context, scope execution.Scope, taskID string) ([]execution.Executor, error) {
	task, e := h.store.Task(ctx, scope.TenantID, scope.UserID, taskID)
	if e != nil {
		return nil, e
	}
	return h.executorsFor(ctx, scope, task)
}

func (h *Host) executorsFor(ctx context.Context, scope execution.Scope, task store.Task) ([]execution.Executor, error) {
	if task.Assignee == nil {
		return nil, ErrNotAgentTask
	}
	switch task.Assignee.Type {
	case store.AssigneeHuman:
		// A person's task runs on that person's identity, so only they start it.
		if task.Assignee.ID != scope.UserID {
			return nil, ErrNotAssignee
		}
	case store.AssigneeSquad:
		// A squad's task runs on the identity of whichever member picks it up.
		member, e := h.store.InSquad(ctx, scope.TenantID, task.Assignee.ID, scope.UserID)
		if e != nil {
			return nil, e
		}
		if !member {
			return nil, ErrNotAssignee
		}
	case store.AssigneeAgent:
	default:
		return nil, ErrNotAgentTask
	}
	all, e := h.catalog.ListExecutors(ctx, scope)
	if e != nil {
		return nil, e
	}
	if task.Assignee.Type != store.AssigneeAgent {
		return all, nil
	}
	out := []execution.Executor{}
	for _, x := range all {
		if x.AgentID == task.Assignee.Identifier {
			out = append(out, x)
		}
	}
	return out, nil
}

// executor picks the requested executor of the task's Agent, or by default its configured model
// before the fixture.
func (h *Host) executor(ctx context.Context, scope execution.Scope, task store.Task, id string) (execution.Executor, error) {
	list, e := h.executorsFor(ctx, scope, task)
	if e != nil {
		return execution.Executor{}, e
	}
	for _, x := range list {
		if id != "" && x.ID == id {
			return x, nil
		}
	}
	if id == "" {
		for _, x := range list {
			if !strings.HasSuffix(x.ID, ":fixture") {
				return x, nil
			}
		}
		if len(list) > 0 {
			return list[0], nil
		}
	}
	return execution.Executor{}, ErrNoExecutor
}

// ScheduledTarget is a binding to a personal collaboration task. The project is
// inherited at save; a changed owner, status or project invalidates the binding.
type ScheduledTarget struct {
	Kind       string `json:"kind"`
	TaskID     string `json:"taskId"`
	ExecutorID string `json:"executorId"`
	AgentName  string `json:"agentName,omitempty"`
	ModelName  string `json:"modelName,omitempty"`
	Note       string `json:"note"`
}

var ErrScheduleTaskChanged = errors.New("schedule task changed")

// ErrScheduledStartUnknown retains the provisional task run until its original attempt can be read.
var ErrScheduledStartUnknown = errors.New("scheduled task start needs lookup")

func (h *Host) scheduledTask(ctx context.Context, scope execution.Scope, target ScheduledTarget, project string) (store.Task, error) {
	task, err := h.store.Task(ctx, scope.TenantID, scope.UserID, target.TaskID)
	if err != nil {
		return task, err
	}
	if task.Assignee == nil || task.Assignee.Type != store.AssigneeHuman || task.Assignee.ID != scope.UserID {
		return task, ErrNotAssignee
	}
	if task.ProjectID != project || (task.Status != store.TaskPending && task.Status != store.TaskRunning) {
		return task, ErrScheduleTaskChanged
	}
	return task, nil
}

// ResolveScheduled validates the binding and freezes catalog labels at save time.
func (h *Host) ResolveScheduled(ctx context.Context, scope execution.Scope, target ScheduledTarget, project string) (ScheduledTarget, error) {
	task, err := h.scheduledTask(ctx, scope, target, project)
	if err != nil {
		return target, err
	}
	executor, err := h.executor(ctx, scope, task, target.ExecutorID)
	target.AgentName = executor.AgentName
	target.ModelName = executor.Model.Name
	return target, err
}

// StartScheduled serializes with manual starts. Replays retain their accepted identity.
func (h *Host) StartScheduled(ctx context.Context, scope execution.Scope, target ScheduledTarget, project, key string) (store.TaskRun, error) {
	defer h.lock(target.TaskID)()
	if r, err := h.store.TaskRunByKey(ctx, scope.TenantID, target.TaskID, scope.UserID, key); !errors.Is(err, store.ErrNotFound) {
		return r, err
	}
	task, err := h.scheduledTask(ctx, scope, target, project)
	if err != nil {
		return store.TaskRun{}, err
	}
	note := "由定时计划触发。"
	if target.Note != "" {
		note += "\n" + target.Note
	}
	return h.start(ctx, scope, task, key, target.ExecutorID, brief(task, note), task.Version)
}

// StartInput starts a run. ExecutorID is empty for the Agent's default; Note adds to the brief.
type StartInput struct {
	ExecutorID string
	Note       string
}

// Start starts a new run of the task's Agent under the caller's scope. Key is the request's
// idempotency key: a retry returns the run it started. A live idle run is ended first; a running
// turn returns store.ErrRunActive.
func (h *Host) Start(ctx context.Context, scope execution.Scope, taskID, key string, in StartInput) (store.TaskRun, error) {
	defer h.lock(taskID)()
	task, e := h.store.Task(ctx, scope.TenantID, scope.UserID, taskID)
	if e != nil {
		return store.TaskRun{}, e
	}
	if run, e := h.store.TaskRunByKey(ctx, scope.TenantID, taskID, scope.UserID, key); e == nil {
		return run, nil
	}
	return h.start(ctx, scope, task, key, in.ExecutorID, brief(task, in.Note), 0)
}

// start runs the task's Agent in a new session. The caller holds the task's lock.
func (h *Host) start(ctx context.Context, scope execution.Scope, task store.Task, key, executorID, prompt string, expectedVersion int64) (store.TaskRun, error) {
	if task.Status == store.TaskDone || task.Status == store.TaskCancelled {
		return store.TaskRun{}, ErrTaskClosed
	}
	ex, e := h.executor(ctx, scope, task, executorID)
	if e != nil {
		return store.TaskRun{}, e
	}
	if live, ok, e := h.store.LiveTaskRun(ctx, task.TenantID, task.ID); e != nil {
		return store.TaskRun{}, e
	} else if ok {
		if live.TurnActive() {
			return store.TaskRun{}, store.ErrRunActive
		}
		if e := h.end(ctx, live, store.TaskRunReplaced); e != nil {
			return store.TaskRun{}, fmt.Errorf("end previous run: %w", e)
		}
	}
	run, e := h.store.CreateTaskRun(ctx, store.NewTaskRun{
		TenantID: task.TenantID, TaskID: task.ID, StartedBy: scope.UserID, StartKey: key,
		ExecutorID: ex.ID, ExecutorName: ex.Name, ExpectedVersion: expectedVersion,
	})
	if e != nil {
		return run, e
	}
	caller := execution.Caller{Kind: execution.CallerTask, ID: run.ID, AttemptID: key}
	ref, e := h.exec.Start(ctx, scope, execution.StartRequest{
		Caller: caller, Target: execution.Target{ProjectID: task.ProjectID}, ExecutorID: ex.ID, Prompt: execution.TextPrompt(prompt),
	})
	if e != nil && (uncertain(e) || expectedVersion != 0 && errors.Is(e, execution.ErrConflict)) {
		// The outcome is unknown: if the service did accept the turn, the run is its owner.
		if turn, re := h.exec.ReadAttempt(context.WithoutCancel(ctx), scope, caller); re == nil {
			ref, e = turn.Ref, nil
		} else if expectedVersion != 0 && !errors.Is(re, execution.ErrNotFound) {
			return run, errors.Join(ErrScheduledStartUnknown, e, re)
		}
	}
	if e != nil {
		return run, errors.Join(e, h.store.DeleteTaskRun(context.WithoutCancel(ctx), run.ID))
	}
	// The turn runs now whether or not the request is still there: a run left without its session
	// would hold the task with nothing to stop.
	ctx = context.WithoutCancel(ctx)
	if e := h.store.RecordTaskTurn(ctx, run.ID, ref.SessionID, ref.TurnID); e != nil {
		if expectedVersion != 0 {
			return run, errors.Join(ErrScheduledStartUnknown, e)
		}
		return run, e
	}
	h.moveTask(ctx, task.TenantID, task.ID, task.Status, store.TaskRunning)
	h.watch(scope, run, ref)
	return h.store.TaskRun(ctx, task.TenantID, task.ID, run.ID)
}

// CommentInput is a comment. ToAgent sends it to the task's Agent as the next turn: of the
// caller's own live run, or of a new run when none is live. ExecutorID applies to a new run.
type CommentInput struct {
	Body       string
	Mentions   []store.TaskMention
	ToAgent    bool
	ExecutorID string
}

// Comment records a comment, and with ToAgent sends it to the Agent first, so a refused turn
// leaves nothing to retry around. Key is the request's idempotency key.
func (h *Host) Comment(ctx context.Context, scope execution.Scope, taskID, key string, in CommentInput) (store.TaskComment, error) {
	defer h.lock(taskID)()
	if c, e := h.store.TaskCommentByKey(ctx, scope.TenantID, taskID, scope.UserID, key); e == nil {
		return c, nil
	}
	task, e := h.store.Task(ctx, scope.TenantID, scope.UserID, taskID)
	if e != nil {
		return store.TaskComment{}, e
	}
	mentions, e := h.store.ResolveTaskMentions(ctx, scope.TenantID, in.Mentions)
	if e != nil {
		return store.TaskComment{}, e
	}
	var runID, turnID string
	if in.ToAgent {
		if runID, turnID, e = h.followUp(ctx, scope, task, key, in); e != nil {
			return store.TaskComment{}, e
		}
	}
	return h.store.CreateTaskComment(ctx, store.NewTaskComment{
		TenantID: scope.TenantID, TaskID: taskID, AuthorID: scope.UserID, CreateKey: key,
		Body: in.Body, Mentions: mentions, RunID: runID, TurnID: turnID,
	})
}

// followUp sends a comment to the Agent. The caller holds the task's lock.
func (h *Host) followUp(ctx context.Context, scope execution.Scope, task store.Task, key string, in CommentInput) (string, string, error) {
	live, ok, e := h.store.LiveTaskRun(ctx, task.TenantID, task.ID)
	if e != nil {
		return "", "", e
	}
	switch {
	case ok && live.TurnActive():
		return "", "", store.ErrRunActive
	case ok && live.StartedBy != scope.UserID:
		return "", "", ErrRunOwned
	case ok:
		if task.Status == store.TaskDone || task.Status == store.TaskCancelled {
			return "", "", ErrTaskClosed
		}
		caller := execution.Caller{Kind: execution.CallerTask, ID: live.ID, AttemptID: key}
		ref, e := h.exec.Append(ctx, scope, execution.AppendRequest{SessionID: live.SessionID, Caller: caller, Prompt: execution.TextPrompt(in.Body)})
		if errors.Is(e, execution.ErrSessionEnded) {
			// Ended outside the page (a restart): record it and start afresh below.
			if e := h.store.EndTaskRun(ctx, live.ID, store.TaskRunInterrupted); e != nil {
				return "", "", e
			}
			break
		}
		if e != nil {
			return "", "", e
		}
		// Likewise, the accepted turn is recorded even if the request is gone.
		ctx = context.WithoutCancel(ctx)
		if e := h.store.RecordTaskTurn(ctx, live.ID, ref.SessionID, ref.TurnID); e != nil {
			return "", "", e
		}
		h.moveTask(ctx, task.TenantID, task.ID, task.Status, store.TaskRunning)
		h.watch(scope, live, ref)
		return live.ID, ref.TurnID, nil
	}
	run, e := h.start(ctx, scope, task, key, in.ExecutorID, brief(task, in.Body), 0)
	if e != nil {
		return "", "", e
	}
	return run.ID, run.LastTurnID, nil
}

// Stop ends a live run: a running turn is cancelled, which on the fixed Node ends the session,
// and an idle session is ended. Any member can stop a run; it spends nobody's identity.
func (h *Host) Stop(ctx context.Context, scope execution.Scope, taskID, runID string) (store.TaskRun, error) {
	defer h.lock(taskID)()
	if _, e := h.store.Task(ctx, scope.TenantID, scope.UserID, taskID); e != nil {
		return store.TaskRun{}, e
	}
	run, e := h.store.TaskRun(ctx, scope.TenantID, taskID, runID)
	if e != nil {
		return run, e
	}
	if !run.Live() || run.SessionID == "" {
		return run, ErrNoLiveRun
	}
	if e := h.end(ctx, run, store.TaskRunStopped); e != nil {
		return run, e
	}
	return h.store.TaskRun(ctx, scope.TenantID, taskID, runID)
}

// RecoverScheduledStart reconnects a provisional run to its already accepted
// turn, without calling Start. A definite miss releases the provisional task slot.
func (h *Host) RecoverScheduledStart(ctx context.Context, scope execution.Scope, taskID, key string) (store.TaskRun, error) {
	defer h.lock(taskID)()
	r, e := h.store.TaskRunByKey(ctx, scope.TenantID, taskID, scope.UserID, key)
	if e != nil || r.SessionID != "" {
		return r, e
	}
	turn, e := h.exec.ReadAttempt(ctx, scope, execution.Caller{Kind: execution.CallerTask, ID: r.ID, AttemptID: key})
	if errors.Is(e, execution.ErrNotFound) {
		if err := h.store.DeleteTaskRun(ctx, r.ID); err != nil {
			return r, err
		}
		return r, store.ErrNotFound
	}
	if e != nil {
		return r, e
	}
	if e = h.store.RecordTaskTurn(ctx, r.ID, turn.Ref.SessionID, turn.Ref.TurnID); e != nil {
		return r, e
	}
	if r.Live() {
		h.moveTask(ctx, r.TenantID, taskID, store.TaskPending, store.TaskRunning)
		h.watch(scope, r, turn.Ref)
	}
	return h.store.TaskRun(ctx, r.TenantID, taskID, r.ID)
}

// StopScheduledTurn stops the original turn only. A follow-up that started
// between the scheduler's history lookup and cancellation belongs to the user.
func (h *Host) StopScheduledTurn(ctx context.Context, scope execution.Scope, taskID, runID, key, turnID string) error {
	defer h.lock(taskID)()
	r, e := h.store.TaskRunByKey(ctx, scope.TenantID, taskID, scope.UserID, key)
	if e != nil {
		return e
	}
	if r.ID != runID {
		return store.ErrConflict
	}
	if !r.TurnActive() || r.LastTurnID != turnID {
		return nil
	}
	return h.end(ctx, r, store.TaskRunStopped)
}

// Release ends the task's idle run once the task is done or cancelled; nobody can follow it up.
// Failing to end it is logged: the task's status is already decided.
func (h *Host) Release(ctx context.Context, tenant, taskID string) {
	defer h.lock(taskID)()
	live, ok, e := h.store.LiveTaskRun(ctx, tenant, taskID)
	if e != nil || !ok || live.SessionID == "" {
		return
	}
	if e := h.end(ctx, live, store.TaskRunClosed); e != nil {
		h.log.Warn("end task run of a closed task", "run", live.ID, "error", e)
	}
}

// end ends a live run's session under its owner's scope, with the reason recorded first so a page
// reading the run on session_ended sees why. A running turn is cancelled and settled here, since
// the session ends with it. The caller holds the task's lock.
func (h *Host) end(ctx context.Context, run store.TaskRun, reason string) error {
	owner := execution.Scope{TenantID: run.TenantID, UserID: run.StartedBy}
	if e := h.store.MarkTaskRunEnding(ctx, run.ID, reason); e != nil {
		return e
	}
	e := func() error {
		if run.TurnActive() {
			result, e := h.exec.Cancel(ctx, owner, execution.Ref{SessionID: run.SessionID, TurnID: run.LastTurnID})
			if e != nil {
				return e
			}
			h.settle(ctx, run, run.LastTurnID, result)
		}
		return h.exec.End(ctx, owner, run.SessionID)
	}()
	if e != nil {
		return errors.Join(e, h.store.ClearTaskRunEnding(context.WithoutCancel(ctx), run.ID))
	}
	return h.store.EndTaskRun(ctx, run.ID, reason)
}

// watch follows an accepted turn until its result and settles it. Observing never stops the turn.
func (h *Host) watch(scope execution.Scope, run store.TaskRun, ref execution.Ref) {
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		result, e := h.await(scope, ref)
		if e != nil {
			if h.ctx.Err() == nil {
				h.log.Warn("observe task turn", "run", run.ID, "turn", ref.TurnID, "error", e)
			}
			return
		}
		h.settle(h.ctx, run, ref.TurnID, result)
	}()
}

// await follows the session until the turn's terminal event. If the stream ends or breaks first,
// the recorded turn is the authority: it carries the result once written.
func (h *Host) await(scope execution.Scope, ref execution.Ref) (execution.Result, error) {
	stream, e := h.exec.Subscribe(h.ctx, scope, ref.SessionID, 0)
	if e != nil {
		return h.recorded(scope, ref, e)
	}
	defer func() { _ = stream.Close() }()
	for {
		event, e := stream.Next(h.ctx)
		if e != nil {
			if errors.Is(e, io.EOF) {
				e = execution.ErrSessionEnded
			}
			return h.recorded(scope, ref, e)
		}
		if event.Kind == execution.EventTurnEnded && event.Ref.TurnID == ref.TurnID && event.Result != nil {
			return *event.Result, nil
		}
	}
}

func (h *Host) recorded(scope execution.Scope, ref execution.Ref, cause error) (execution.Result, error) {
	if h.ctx.Err() != nil {
		return execution.Result{}, h.ctx.Err()
	}
	for cursor := ""; ; {
		page, e := h.exec.ReadTurns(h.ctx, scope, ref.SessionID, cursor, 100)
		if e != nil {
			return execution.Result{}, errors.Join(cause, e)
		}
		for _, t := range page.Items {
			if t.Ref.TurnID == ref.TurnID && t.Result != nil {
				return *t.Result, nil
			}
		}
		if page.NextCursor == "" {
			return execution.Result{}, cause
		}
		cursor = page.NextCursor
	}
}

// settle records a turn's result once and moves the task: a completed or failed turn hands it to
// 待验收, a cancelled or interrupted one returns it to 待处理.
func (h *Host) settle(ctx context.Context, run store.TaskRun, turnID string, r execution.Result) {
	ctx = context.WithoutCancel(ctx)
	settled, e := h.store.SettleTaskTurn(ctx, run.ID, turnID, string(r.Status), r.FailureCode)
	if e != nil {
		h.log.Warn("settle task turn", "run", run.ID, "turn", turnID, "error", e)
		return
	}
	if !settled {
		return
	}
	to := store.TaskReview
	if r.Status == execution.StatusCancelled || r.Status == execution.StatusInterrupted {
		to = store.TaskPending
	}
	h.moveTask(ctx, run.TenantID, run.TaskID, store.TaskRunning, to)
}

// moveTask applies a run's status move. A task a person already moved elsewhere stays where it is.
func (h *Host) moveTask(ctx context.Context, tenant, taskID, from, to string) {
	if from == to {
		return
	}
	if _, e := h.store.TransitionTaskStatus(context.WithoutCancel(ctx), tenant, taskID, from, to); e != nil &&
		!errors.Is(e, store.ErrConflict) && !errors.Is(e, store.ErrInvalidTransition) {
		h.log.Warn("move task with its run", "task", taskID, "to", to, "error", e)
	}
}

// Events follows a run's session for any member of the task's workspace. The session belongs to
// the person who started the run, so it is read under their scope after the reader's check.
func (h *Host) Events(ctx context.Context, scope execution.Scope, taskID, runID string, after uint64) (execution.Stream, store.TaskRun, error) {
	if _, e := h.store.Task(ctx, scope.TenantID, scope.UserID, taskID); e != nil {
		return nil, store.TaskRun{}, e
	}
	run, e := h.store.TaskRun(ctx, scope.TenantID, taskID, runID)
	if e != nil {
		return nil, run, e
	}
	if run.SessionID == "" {
		return nil, run, store.ErrNotFound
	}
	stream, e := h.exec.Subscribe(ctx, Owner(run), run.SessionID, after)
	return stream, run, e
}

// Run reads one run of a task the caller can see.
func (h *Host) Run(ctx context.Context, scope execution.Scope, taskID, runID string) (store.TaskRun, error) {
	if _, e := h.store.Task(ctx, scope.TenantID, scope.UserID, taskID); e != nil {
		return store.TaskRun{}, e
	}
	return h.store.TaskRun(ctx, scope.TenantID, taskID, runID)
}

// Owner is the scope a run's session belongs to.
func Owner(run store.TaskRun) execution.Scope {
	return execution.Scope{TenantID: run.TenantID, UserID: run.StartedBy}
}

// TurnPrompt reads the text a turn of the run was asked.
func (h *Host) TurnPrompt(ctx context.Context, run store.TaskRun, turnID string) (string, error) {
	for cursor := ""; ; {
		page, e := h.exec.ReadTurns(ctx, Owner(run), run.SessionID, cursor, 100)
		if e != nil {
			return "", e
		}
		for _, t := range page.Items {
			if t.Ref.TurnID == turnID {
				return t.Prompt.Text()
			}
		}
		if page.NextCursor == "" {
			return "", execution.ErrNotFound
		}
		cursor = page.NextCursor
	}
}

// brief is the first prompt of a run: the task itself, and what the person added.
func brief(task store.Task, note string) string {
	var b strings.Builder
	b.WriteString("请完成下面这个任务。\n\n任务：")
	b.WriteString(task.Title)
	if d := strings.TrimSpace(task.Description); d != "" {
		b.WriteString("\n\n")
		b.WriteString(d)
	}
	if n := strings.TrimSpace(note); n != "" {
		b.WriteString("\n\n补充说明：")
		b.WriteString(n)
	}
	return b.String()
}

// uncertain reports a call whose outcome is unknown.
func uncertain(e error) bool {
	return errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded)
}
