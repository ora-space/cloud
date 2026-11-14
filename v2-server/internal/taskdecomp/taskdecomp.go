// Package taskdecomp splits a task into subtasks with a workflow, for the Tech Lead's 「拆分」 on
// the collaboration page. A decomposition is one run of a published workflow, started by one
// person with the task's title and description as input and the task's project as its project.
// When the run succeeds, its output's `tasks` array (items with `title` and `description`) becomes
// the task's subtasks; any other end is a failure the task page shows.
//
// Writing the subtasks and ending the decomposition share one transaction guarded by the
// decomposition still running, so a run reported finished twice, by the watcher, a page read and a
// restart, writes its subtasks once. A decomposition never outlives its run: Recover settles what a
// previous process left running, after the workflow host has interrupted its runs.
package taskdecomp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Errors the HTTP layer maps to stable codes.
var (
	// ErrInvalidWorkflow reports a workflow that does not exist in the workspace or has no published version.
	ErrInvalidWorkflow = errors.New("workflow cannot decompose")
	// ErrWorkflowOutputRequired reports a published workflow without the tasks output Subtasks consumes.
	ErrWorkflowOutputRequired = errors.New("workflow output does not support decomposition")
	// ErrTaskClosed reports a decomposition of a task that is done or cancelled.
	ErrTaskClosed = errors.New("task closed")
	// ErrSubtask reports a decomposition of a subtask: only one level exists.
	ErrSubtask = errors.New("subtask cannot be decomposed")
)

// Limits on what a run's output may become: the task routes' title and description limits, and a
// bound on how many subtasks one decomposition writes.
const (
	maxTitle       = 200
	maxDescription = 4000
	maxSubtasks    = 20
)

// Store is the persistence a Host needs; *store.Store implements it.
type Store interface {
	Task(ctx context.Context, tenant, id string) (store.Task, error)
	PublishedWorkflowSnapshot(ctx context.Context, tenant, workflowID string) (store.WorkflowSnapshot, error)
	CreateWorkflowRun(ctx context.Context, tenant, workflowID, snapshotID string, in store.WorkflowRunInput) (store.WorkflowRun, error)
	WorkflowRun(ctx context.Context, tenant, runID string) (store.WorkflowRun, error)
	TaskDecompositions(ctx context.Context, tenant, taskID string) ([]store.TaskDecomposition, error)
	TaskDecompositionByKey(ctx context.Context, tenant, taskID, user, key string) (store.TaskDecomposition, error)
	LiveTaskDecomposition(ctx context.Context, tenant, taskID string) (store.TaskDecomposition, bool, error)
	RunningTaskDecompositions(ctx context.Context) ([]store.TaskDecomposition, error)
	CreateTaskDecomposition(ctx context.Context, in store.NewTaskDecomposition) (store.TaskDecomposition, error)
	SettleTaskDecomposition(ctx context.Context, id string, subtasks []store.Subtask, failure string) (store.TaskDecomposition, bool, error)
}

// Runs starts a workflow run as a person and tells when one changes; main wires the workflow host
// and its hub. A change carries no data: the watcher reads the run again.
type Runs interface {
	Start(ctx context.Context, tenant, user, runID string) error
	Subscribe(runID string) (<-chan struct{}, func())
}

// Host starts decompositions and settles them when their runs end.
type Host struct {
	store Store
	runs  Runs
	log   *slog.Logger
	// poll re-reads a watched run even without a change event, in case one was missed.
	poll time.Duration

	ctx     context.Context
	stop    context.CancelFunc
	workers sync.WaitGroup

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns a Host. Call Recover before serving requests and Close on shutdown.
func New(s Store, runs Runs, log *slog.Logger) *Host {
	if log == nil {
		log = slog.Default()
	}
	h := &Host{store: s, runs: runs, log: log, poll: 5 * time.Second, locks: map[string]*sync.Mutex{}}
	h.ctx, h.stop = context.WithCancel(context.Background())
	return h
}

// Close stops watching runs and waits for the watchers. The runs themselves belong to the
// workflow host; the next Recover settles what is left.
func (h *Host) Close() {
	h.stop()
	h.workers.Wait()
}

func (h *Host) lock(taskID string) func() {
	h.mu.Lock()
	l, ok := h.locks[taskID]
	if !ok {
		l = &sync.Mutex{}
		h.locks[taskID] = l
	}
	h.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Start runs the workflow over the task as the caller and returns the running decomposition. The
// same key returns the decomposition it already started. A published snapshot that fails
// CanDecompose is rejected before any run is created. A run the workflow host refuses to start
// fails the decomposition at once, with the reason, rather than leaving it running.
func (h *Host) Start(ctx context.Context, tenant, user, taskID, workflowID, key string) (store.TaskDecomposition, error) {
	task, e := h.store.Task(ctx, tenant, taskID)
	if e != nil {
		return store.TaskDecomposition{}, e
	}
	defer h.lock(taskID)()
	if d, e := h.store.TaskDecompositionByKey(ctx, tenant, taskID, user, key); e == nil {
		return d, nil
	} else if !errors.Is(e, store.ErrNotFound) {
		return d, e
	}
	switch {
	case task.Status == store.TaskDone || task.Status == store.TaskCancelled:
		return store.TaskDecomposition{}, ErrTaskClosed
	case task.ParentID != nil:
		return store.TaskDecomposition{}, ErrSubtask
	}
	if _, live, e := h.store.LiveTaskDecomposition(ctx, tenant, taskID); e != nil || live {
		if e == nil {
			e = store.ErrDecompositionActive
		}
		return store.TaskDecomposition{}, e
	}
	snapshot, e := h.store.PublishedWorkflowSnapshot(ctx, tenant, workflowID)
	switch {
	case errors.Is(e, store.ErrNotFound), errors.Is(e, store.ErrWorkflowNoPublishedSnapshot),
		errors.Is(e, store.ErrWorkflowSnapshotNotFound):
		return store.TaskDecomposition{}, ErrInvalidWorkflow
	case e != nil:
		return store.TaskDecomposition{}, fmt.Errorf("read decomposition workflow: %w", e)
	}
	if !CanDecompose(snapshot.Graph) {
		return store.TaskDecomposition{}, ErrWorkflowOutputRequired
	}
	input := task.Title
	if desc := strings.TrimSpace(task.Description); desc != "" {
		input += "\n\n" + desc
	}
	// Freeze exactly the snapshot checked above, even if another version is published now.
	run, e := h.store.CreateWorkflowRun(ctx, tenant, workflowID, snapshot.ID, store.WorkflowRunInput{ProjectID: task.ProjectID, Input: &input})
	switch {
	case errors.Is(e, store.ErrNotFound), errors.Is(e, store.ErrWorkflowNoPublishedSnapshot),
		errors.Is(e, store.ErrWorkflowSnapshotNotFound), errors.Is(e, store.ErrWorkflowDraftSnapshot):
		return store.TaskDecomposition{}, ErrInvalidWorkflow
	case errors.Is(e, store.ErrProjectNotFound):
		return store.TaskDecomposition{}, store.ErrInvalidProject
	case e != nil:
		return store.TaskDecomposition{}, fmt.Errorf("create decomposition run: %w", e)
	}
	d, e := h.store.CreateTaskDecomposition(ctx, store.NewTaskDecomposition{
		TaskID: taskID, TenantID: tenant, StartedBy: user, StartKey: key,
		WorkflowID: workflowID, WorkflowName: run.Name, WorkflowRunID: run.ID,
	})
	if e != nil {
		return d, e
	}
	// Subscribe before starting, so the run's end cannot slip between the start and the watch.
	events, cancel := h.runs.Subscribe(run.ID)
	// The run outlives the request once it starts.
	if e := h.runs.Start(context.WithoutCancel(ctx), tenant, user, run.ID); e != nil {
		cancel()
		h.log.Warn("decomposition run did not start", "task", taskID, "run", run.ID, "error", e)
		settled, _, e := h.store.SettleTaskDecomposition(context.WithoutCancel(ctx), d.ID, nil, "工作流没有启动："+e.Error())
		return settled, e
	}
	h.workers.Add(1)
	go h.watch(d, events, cancel)
	return d, nil
}

// List returns the task's decompositions, newest first, settling any whose run has ended in case
// its watcher missed it.
func (h *Host) List(ctx context.Context, tenant, taskID string) ([]store.TaskDecomposition, error) {
	items, e := h.store.TaskDecompositions(ctx, tenant, taskID)
	if e != nil {
		return nil, e
	}
	for i, d := range items {
		if d.Status != store.DecompositionRunning {
			continue
		}
		if settled, done := h.settle(ctx, d); done {
			items[i] = settled
		}
	}
	return items, nil
}

// Recover settles the decompositions a previous process left running. It must run after the
// workflow host has interrupted that process's runs: a run that still has not ended then never
// will, so its decomposition fails.
func (h *Host) Recover(ctx context.Context) error {
	items, e := h.store.RunningTaskDecompositions(ctx)
	if e != nil {
		return fmt.Errorf("recover task decompositions: %w", e)
	}
	for _, d := range items {
		if _, done := h.settle(ctx, d); done {
			continue
		}
		if _, _, e := h.store.SettleTaskDecomposition(ctx, d.ID, nil, "服务重启，拆分已中断，请重新拆分。"); e != nil {
			return fmt.Errorf("interrupt task decomposition: %w", e)
		}
	}
	return nil
}

// watch settles the decomposition once its run ends.
func (h *Host) watch(d store.TaskDecomposition, events <-chan struct{}, cancel func()) {
	defer h.workers.Done()
	defer cancel()
	tick := time.NewTicker(h.poll)
	defer tick.Stop()
	for {
		if _, done := h.settle(h.ctx, d); done {
			return
		}
		select {
		case <-h.ctx.Done():
			return
		case <-events:
		case <-tick.C:
		}
	}
}

// settle reads the decomposition's run and, once it has ended, ends the decomposition with the
// run's subtasks or the reason there are none. It reports whether the decomposition is settled.
func (h *Host) settle(ctx context.Context, d store.TaskDecomposition) (store.TaskDecomposition, bool) {
	run, e := h.store.WorkflowRun(ctx, d.TenantID, d.WorkflowRunID)
	var subtasks []store.Subtask
	failure := ""
	switch {
	case errors.Is(e, store.ErrWorkflowRunNotFound):
		failure = "工作流运行已被删除。"
	case e != nil:
		if ctx.Err() != nil {
			return d, false
		}
		h.log.Warn("read decomposition run", "decomposition", d.ID, "run", d.WorkflowRunID, "error", e)
		return d, false
	case run.Status == store.WorkflowStatusSucceeded:
		subtasks, failure = Subtasks(run.Output)
	case run.Status == store.WorkflowStatusFailed:
		failure = "工作流运行失败"
		if run.Error != nil && strings.TrimSpace(*run.Error) != "" {
			failure += "：" + truncate(strings.TrimSpace(*run.Error), 500)
		}
		failure += "。"
	case run.Status == store.WorkflowStatusCancelled:
		failure = "工作流运行已取消。"
	default:
		return d, false
	}
	settled, _, e := h.store.SettleTaskDecomposition(ctx, d.ID, subtasks, failure)
	if e != nil {
		h.log.Warn("settle task decomposition", "decomposition", d.ID, "error", e)
		return d, false
	}
	return settled, true
}

// Subtasks reads a run's output as subtasks: an object whose `tasks` is an array of items with a
// non-empty `title` and an optional `description`, trimmed to the task limits. The array may also
// arrive as a JSON string, as an Agent's unstructured answer does. Otherwise it returns no subtasks
// and the reason, in the page's words, followed by the workflow's declared-output requirement.
func Subtasks(output *string) ([]store.Subtask, string) {
	if output == nil || strings.TrimSpace(*output) == "" {
		return nil, outputFailure("工作流没有输出。")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(*output), &fields) != nil || fields["tasks"] == nil {
		return nil, outputFailure("工作流的输出里没有 tasks 数组。")
	}
	raw := fields["tasks"]
	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = json.RawMessage(text)
	}
	var items []struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return nil, outputFailure("工作流输出的 tasks 不是数组。")
	}
	if len(items) == 0 {
		return nil, outputFailure("工作流没有拆出子任务。")
	}
	if len(items) > maxSubtasks {
		return nil, outputFailure(fmt.Sprintf("工作流拆出了 %d 个子任务，一次最多 %d 个。", len(items), maxSubtasks))
	}
	out := make([]store.Subtask, 0, len(items))
	for i, item := range items {
		if item.Title == nil || strings.TrimSpace(*item.Title) == "" {
			return nil, outputFailure(fmt.Sprintf("第 %d 个子任务没有标题。", i+1))
		}
		sub := store.Subtask{Title: truncate(strings.TrimSpace(*item.Title), maxTitle)}
		if item.Description != nil {
			sub.Description = truncate(strings.TrimSpace(*item.Description), maxDescription)
		}
		out = append(out, sub)
	}
	return out, ""
}

func outputFailure(reason string) string {
	return reason + OutputRequirement
}

func truncate(s string, runes int) string {
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	return string([]rune(s)[:runes])
}
