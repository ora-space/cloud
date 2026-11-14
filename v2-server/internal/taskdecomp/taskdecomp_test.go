package taskdecomp_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
)

const startGraph = `{"nodes":[{"id":"start","type":"workflow","data":{"kind":"start"}},{"id":"out","type":"workflow","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["start","input"]}]}}],"edges":[{"id":"e","source":"start","target":"out"}]}`

// runs is a workflow host that starts runs in the store and lets the test end them; every change is
// published like the host's hub does.
type runs struct {
	store   *store.Store
	refuse  error
	mu      sync.Mutex
	started []string
	subs    map[string][]chan struct{}
}

func (r *runs) Start(ctx context.Context, tenant, _, runID string) error {
	if r.refuse != nil {
		return r.refuse
	}
	if _, e := r.store.StartWorkflowRun(ctx, tenant, runID); e != nil {
		return e
	}
	r.mu.Lock()
	r.started = append(r.started, runID)
	r.mu.Unlock()
	return nil
}

func (r *runs) Subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	r.subs[runID] = append(r.subs[runID], ch)
	r.mu.Unlock()
	return ch, func() {}
}

func (r *runs) publish(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subs[runID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

type fixture struct {
	ctx      context.Context
	store    *store.Store
	runs     *runs
	host     *taskdecomp.Host
	tenant   string
	owner    string
	project  string
	workflow string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := storetest.Open(t)
	ctx := context.Background()
	owner, tenant := storetest.Login(t, s, "lead")
	f := &fixture{ctx: ctx, store: s, runs: &runs{store: s, subs: map[string][]chan struct{}{}}, tenant: tenant, owner: owner, project: "project-lead"}
	if _, e := s.CreateProject(ctx, execution.Scope{TenantID: tenant, UserID: owner}, store.Project{ID: f.project, Name: "演示",
		RepositoryURL: "https://example.test/r.git", Commit: "abc", MirrorPath: "/tmp/mirror"}); e != nil {
		t.Fatal(e)
	}
	w, _, e := s.CreateWorkflow(ctx, tenant, "需求拆分", startGraph)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := s.PublishWorkflow(ctx, tenant, w.ID, "v1", nil); e != nil {
		t.Fatal(e)
	}
	f.workflow = w.ID
	f.host = taskdecomp.New(s, f.runs, nil)
	t.Cleanup(f.host.Close)
	return f
}

func (f *fixture) task(t *testing.T, title string) store.Task {
	t.Helper()
	task, e := f.store.CreateTask(f.ctx, f.tenant, f.owner, store.TaskInput{Title: title, Description: "登录、注册和找回密码", ProjectID: f.project})
	if e != nil {
		t.Fatal(e)
	}
	return task
}

// finish ends the run as the workflow engine would and tells the watchers.
func (f *fixture) finish(t *testing.T, runID, status, text string) {
	t.Helper()
	var e error
	if status == store.WorkflowStatusCancelled {
		_, e = f.store.CancelWorkflowRun(f.ctx, f.tenant, runID)
	} else {
		_, e = f.store.FinishWorkflowRun(f.ctx, f.tenant, runID, status, &text, &text)
	}
	if e != nil {
		t.Fatal(e)
	}
	f.runs.publish(runID)
}

// await polls until the decomposition settles: the run's end is handled by a background watcher.
func (f *fixture) await(t *testing.T, taskID string) store.TaskDecomposition {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		items, e := f.store.TaskDecompositions(f.ctx, f.tenant, taskID)
		if e != nil {
			t.Fatal(e)
		}
		if len(items) > 0 && items[0].Status != store.DecompositionRunning {
			return items[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("decomposition of %s never settled: %+v", taskID, items)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f *fixture) subtasks(t *testing.T, parent string) []store.Task {
	t.Helper()
	all, e := f.store.Tasks(f.ctx, f.tenant)
	if e != nil {
		t.Fatal(e)
	}
	out := []store.Task{}
	for _, task := range all {
		if task.ParentID != nil && *task.ParentID == parent {
			out = append(out, task)
		}
	}
	return out
}

const output = `{"tasks":[{"title":"登录页","description":"邮箱登录，错误时提示"},{"title":"注册页","description":"校验邮箱格式"},{"title":"找回密码"}]}`

func TestDecompositionWritesSubtasksOnce(t *testing.T) {
	f := newFixture(t)
	parent := f.task(t, "账号体系")
	d, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, f.workflow, "key-1")
	if e != nil || d.Status != store.DecompositionRunning || d.WorkflowName != "需求拆分" || d.StartedBy != f.owner {
		t.Fatalf("start = %+v %v", d, e)
	}
	run, e := f.store.WorkflowRun(f.ctx, f.tenant, d.WorkflowRunID)
	if e != nil || run.Status != store.WorkflowStatusRunning || run.ProjectID != f.project || run.Input == nil ||
		*run.Input != "账号体系\n\n登录、注册和找回密码" {
		t.Fatalf("run = %+v %v", run, e)
	}
	// The same key is the same decomposition; another one waits for this to end.
	if again, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, f.workflow, "key-1"); e != nil || again.ID != d.ID {
		t.Fatalf("same key = %+v %v", again, e)
	}
	if _, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, f.workflow, "key-2"); !errors.Is(e, store.ErrDecompositionActive) {
		t.Fatalf("second = %v", e)
	}

	f.finish(t, d.WorkflowRunID, store.WorkflowStatusSucceeded, output)
	done := f.await(t, parent.ID)
	if done.Status != store.DecompositionSucceeded || done.CreatedCount != 3 || done.FinishedAt == nil {
		t.Fatalf("settled = %+v", done)
	}
	subs := f.subtasks(t, parent.ID)
	if len(subs) != 3 || subs[0].Title != "登录页" || subs[0].Description != "邮箱登录，错误时提示" || subs[2].Title != "找回密码" {
		t.Fatalf("subtasks = %+v", subs)
	}
	for _, sub := range subs {
		if sub.Status != store.TaskPending || sub.Assignee != nil || sub.ProjectID != f.project || sub.CreatedBy != f.owner {
			t.Fatalf("subtask = %+v", sub)
		}
	}
	// Reading the run's end again, as a page read or a restart does, writes nothing more.
	if items, e := f.host.List(f.ctx, f.tenant, parent.ID); e != nil || len(items) != 1 || items[0].CreatedCount != 3 {
		t.Fatalf("list = %+v %v", items, e)
	}
	if _, settled, e := f.store.SettleTaskDecomposition(f.ctx, d.ID, []store.Subtask{{Title: "多余"}}, ""); e != nil || settled {
		t.Fatalf("settle again = %v %v", settled, e)
	}
	if e := f.host.Recover(f.ctx); e != nil {
		t.Fatal(e)
	}
	if subs := f.subtasks(t, parent.ID); len(subs) != 3 {
		t.Fatalf("subtasks after repeats = %d", len(subs))
	}

	// A subtask is not split again, and a finished task is not split at all.
	if _, e := f.host.Start(f.ctx, f.tenant, f.owner, subs[0].ID, f.workflow, "key-3"); !errors.Is(e, taskdecomp.ErrSubtask) {
		t.Fatalf("subtask = %v", e)
	}
	closed := f.task(t, "已取消的需求")
	if _, e := f.store.SetTaskStatus(f.ctx, storetest.Access(t, f.store, f.tenant, f.owner), closed.ID, store.TaskCancelled, closed.Version); e != nil {
		t.Fatal(e)
	}
	if _, e := f.host.Start(f.ctx, f.tenant, f.owner, closed.ID, f.workflow, "key-4"); !errors.Is(e, taskdecomp.ErrTaskClosed) {
		t.Fatalf("closed = %v", e)
	}
}

func TestDecompositionFailuresAreVisible(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, status, output, failure string
	}{
		{"no tasks", store.WorkflowStatusSucceeded, `{"summary":"做不了"}`, "没有 tasks 数组"},
		{"empty", store.WorkflowStatusSucceeded, `{"tasks":[]}`, "没有拆出子任务"},
		{"untitled", store.WorkflowStatusSucceeded, `{"tasks":[{"title":"一"},{"title":" "}]}`, "第 2 个子任务没有标题"},
		{"run failed", store.WorkflowStatusFailed, "模型超时", "工作流运行失败：模型超时"},
		{"cancelled", store.WorkflowStatusCancelled, "", "工作流运行已取消"},
	}
	for i, c := range cases {
		parent := f.task(t, c.name)
		d, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, f.workflow, "key")
		if e != nil {
			t.Fatalf("%s: %v", c.name, e)
		}
		f.finish(t, d.WorkflowRunID, c.status, c.output)
		done := f.await(t, parent.ID)
		if done.Status != store.DecompositionFailed || !strings.Contains(done.Failure, c.failure) || done.CreatedCount != 0 {
			t.Fatalf("case %d %s = %+v", i, c.name, done)
		}
		if subs := f.subtasks(t, parent.ID); len(subs) != 0 {
			t.Fatalf("%s wrote %d subtasks", c.name, len(subs))
		}
		// A failure frees the task for another try.
		if _, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, f.workflow, "retry"); e != nil {
			t.Fatalf("%s retry = %v", c.name, e)
		}
	}
}

func TestDecompositionStartRefusals(t *testing.T) {
	f := newFixture(t)
	parent := f.task(t, "账号体系")
	draft, _, e := f.store.CreateWorkflow(f.ctx, f.tenant, "草稿", startGraph)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{draft.ID, "00000000-0000-4000-8000-000000000999"} {
		if _, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, id, "k-"+id); !errors.Is(e, taskdecomp.ErrInvalidWorkflow) {
			t.Fatalf("workflow %s = %v", id, e)
		}
	}

	incompatible, _, e := f.store.CreateWorkflow(f.ctx, f.tenant, "缺少 tasks 输出", `{"nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]}`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.PublishWorkflow(f.ctx, f.tenant, incompatible.ID, "v1", nil); e != nil {
		t.Fatal(e)
	}
	before, e := f.store.WorkflowRuns(f.ctx, f.tenant, "", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, incompatible.ID, "incompatible"); !errors.Is(e, taskdecomp.ErrWorkflowOutputRequired) {
		t.Fatalf("incompatible workflow = %v", e)
	}
	after, e := f.store.WorkflowRuns(f.ctx, f.tenant, "", "")
	if e != nil || len(after) != len(before) {
		t.Fatalf("incompatible start created a run: before %d after %d, %v", len(before), len(after), e)
	}
	if items, e := f.store.TaskDecompositions(f.ctx, f.tenant, parent.ID); e != nil || len(items) != 0 {
		t.Fatalf("incompatible start created a decomposition: %+v, %v", items, e)
	}

	// A run the workflow host refuses fails the decomposition at once, with the reason.
	f.runs.refuse = errors.New("没有可用的 Agent")
	d, e := f.host.Start(f.ctx, f.tenant, f.owner, parent.ID, f.workflow, "refused")
	if e != nil || d.Status != store.DecompositionFailed || !strings.Contains(d.Failure, "没有可用的 Agent") {
		t.Fatalf("refused = %+v %v", d, e)
	}
}

func TestRecoverEndsWhatTheLastProcessLeft(t *testing.T) {
	f := newFixture(t)
	finished, stuck := f.task(t, "跑完了"), f.task(t, "卡住了")
	a, e := f.host.Start(f.ctx, f.tenant, f.owner, finished.ID, f.workflow, "a")
	if e != nil {
		t.Fatal(e)
	}
	b, e := f.host.Start(f.ctx, f.tenant, f.owner, stuck.ID, f.workflow, "b")
	if e != nil {
		t.Fatal(e)
	}
	f.host.Close() // the process ends before either run is settled
	text := output
	if _, e := f.store.FinishWorkflowRun(f.ctx, f.tenant, a.WorkflowRunID, store.WorkflowStatusSucceeded, &text, nil); e != nil {
		t.Fatal(e)
	}
	next := taskdecomp.New(f.store, f.runs, nil)
	t.Cleanup(next.Close)
	if e := next.Recover(f.ctx); e != nil {
		t.Fatal(e)
	}
	if got := f.await(t, finished.ID); got.Status != store.DecompositionSucceeded || len(f.subtasks(t, finished.ID)) != 3 {
		t.Fatalf("finished = %+v", got)
	}
	if got := f.await(t, stuck.ID); got.Status != store.DecompositionFailed || !strings.Contains(got.Failure, "服务重启") || got.ID != b.ID {
		t.Fatalf("stuck = %+v", got)
	}
}

func TestSubtasksReadsTheOutput(t *testing.T) {
	text := func(s string) *string { return &s }
	// An Agent's unstructured answer carries the array as a string.
	subs, failure := taskdecomp.Subtasks(text(`{"tasks":"[{\"title\":\" 登录页 \",\"description\":\"说明\"}]"}`))
	if failure != "" || len(subs) != 1 || subs[0].Title != "登录页" || subs[0].Description != "说明" {
		t.Fatalf("string array = %+v %q", subs, failure)
	}
	long := strings.Repeat("长", 300)
	subs, failure = taskdecomp.Subtasks(text(`{"tasks":[{"title":"` + long + `"}]}`))
	if failure != "" || len([]rune(subs[0].Title)) != 200 {
		t.Fatalf("long title = %d %q", len([]rune(subs[0].Title)), failure)
	}
	many := `{"tasks":[` + strings.TrimSuffix(strings.Repeat(`{"title":"一"},`, 21), ",") + `]}`
	for _, c := range []struct {
		in      *string
		failure string
	}{
		{nil, "没有输出"},
		{text("不是 JSON"), "没有 tasks 数组"},
		{text(`{"tasks":{"title":"一"}}`), "不是数组"},
		{text(many), "一次最多 20 个"},
	} {
		if subs, failure := taskdecomp.Subtasks(c.in); subs != nil || !strings.Contains(failure, c.failure) {
			t.Fatalf("%v = %+v %q", c.in, subs, failure)
		} else if !strings.HasSuffix(failure, taskdecomp.OutputRequirement) {
			t.Fatalf("%v failure lacks the workflow requirement: %q", c.in, failure)
		}
	}
}

// republishingStore deterministically moves the published pointer between validation and run creation.
type republishingStore struct {
	*store.Store
	t *testing.T
}

func (s republishingStore) CreateWorkflowRun(ctx context.Context, tenant, workflowID, snapshotID string, in store.WorkflowRunInput) (store.WorkflowRun, error) {
	s.t.Helper()
	if _, err := s.UpdateWorkflowDraft(ctx, tenant, workflowID, `{"nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]}`); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.PublishWorkflow(ctx, tenant, workflowID, "v2", nil); err != nil {
		s.t.Fatal(err)
	}
	return s.Store.CreateWorkflowRun(ctx, tenant, workflowID, snapshotID, in)
}

func TestStartFreezesCheckedSnapshot(t *testing.T) {
	f := newFixture(t)
	task := f.task(t, "Snapshot race")
	checked, err := f.store.PublishedWorkflowSnapshot(f.ctx, f.tenant, f.workflow)
	if err != nil {
		t.Fatal(err)
	}
	host := taskdecomp.New(republishingStore{Store: f.store, t: t}, f.runs, nil)
	t.Cleanup(host.Close)
	d, err := host.Start(f.ctx, f.tenant, f.owner, task.ID, f.workflow, "snapshot-race")
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.WorkflowRun(f.ctx, f.tenant, d.WorkflowRunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.SnapshotID != checked.ID {
		t.Fatalf("run snapshot = %s, checked %s", run.SnapshotID, checked.ID)
	}
}
