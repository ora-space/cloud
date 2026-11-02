package workflowhost

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// fakeExecution scripts the shared execution service: every accepted Start opens a session whose
// owning turn ends when the test calls finish. Hooks inject the faults a scenario needs.
type fakeExecution struct {
	executiontest.Stub

	mu       sync.Mutex
	started  []execution.StartRequest
	scopes   []execution.Scope
	sessions map[string]*fakeSession
	ended    []string
	cancels  []execution.Ref
	next     int

	// beforeStart runs before a Start is accepted; a non-nil error refuses it.
	beforeStart func(execution.StartRequest) error
	// onCancel decides Cancel's outcome; nil confirms by ending the turn as cancelled.
	onCancel func(execution.Ref) error
	// startedCh receives each accepted Start's session ID.
	startedCh chan string
}

type fakeSession struct {
	ref    execution.Ref
	caller execution.Caller
	events chan execution.Event
	result *execution.Result
}

func newFakeExecution() *fakeExecution {
	f := &fakeExecution{sessions: map[string]*fakeSession{}, startedCh: make(chan string, 16)}
	f.ListExecutorsFunc = func(context.Context, execution.Scope) ([]execution.Executor, error) {
		return []execution.Executor{
			{ID: "opencode:fixture", AgentID: "official/ora-space.opencode", AgentName: "OpenCode", PluginVersion: "0.6.11", Model: execution.Model{ID: "fixture", Name: "Fixture"}},
			{ID: "opencode:other", AgentID: "official/ora-space.opencode", AgentName: "OpenCode", PluginVersion: "0.6.11", Model: execution.Model{ID: "other", Name: "Other Model"}},
			{ID: "claude:fixture", AgentID: "official/ora-space.claude", AgentName: "Claude Code", PluginVersion: "0.4.5", Model: execution.Model{ID: "messages:fixture", Name: "Fixture"}},
		}, nil
	}
	f.StartFunc = f.start
	f.SubscribeFunc = f.subscribe
	f.CancelFunc = f.cancel
	f.EndFunc = func(_ context.Context, _ execution.Scope, id string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ended = append(f.ended, id)
		return nil
	}
	f.ReadAttemptFunc = func(_ context.Context, _ execution.Scope, c execution.Caller) (execution.Turn, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.sessions {
			if s.caller.AttemptID == c.AttemptID {
				return execution.Turn{Ref: s.ref, Caller: s.caller, Result: s.result}, nil
			}
		}
		return execution.Turn{}, execution.ErrNotFound
	}
	return f
}

func (f *fakeExecution) start(_ context.Context, scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
	if f.beforeStart != nil {
		if err := f.beforeStart(req); err != nil {
			return execution.Ref{}, err
		}
	}
	f.mu.Lock()
	for _, s := range f.sessions {
		if s.caller.AttemptID == req.Caller.AttemptID {
			f.mu.Unlock()
			return s.ref, nil // an identical caller attempt is idempotent
		}
	}
	f.next++
	s := &fakeSession{
		ref:    execution.Ref{SessionID: fmt.Sprintf("session-%d", f.next), TurnID: fmt.Sprintf("turn-%d", f.next)},
		caller: req.Caller, events: make(chan execution.Event, 4),
	}
	f.sessions[s.ref.SessionID] = s
	f.started = append(f.started, req)
	f.scopes = append(f.scopes, scope)
	f.mu.Unlock()
	f.startedCh <- s.ref.SessionID
	return s.ref, nil
}

type fakeStream struct {
	events <-chan execution.Event
	closed chan struct{}
	once   sync.Once
}

func (s *fakeStream) Next(ctx context.Context) (execution.Event, error) {
	select {
	case e, ok := <-s.events:
		if !ok {
			return execution.Event{}, io.EOF
		}
		return e, nil
	case <-s.closed:
		return execution.Event{}, io.EOF
	case <-ctx.Done():
		return execution.Event{}, ctx.Err()
	}
}

func (s *fakeStream) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

func (f *fakeExecution) subscribe(_ context.Context, _ execution.Scope, id string, _ uint64) (execution.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s == nil {
		return nil, execution.ErrNotFound
	}
	return &fakeStream{events: s.events, closed: make(chan struct{})}, nil
}

// finish ends the session's owning turn with r. A second call is a duplicate terminal event.
func (f *fakeExecution) finish(id string, r execution.Result) {
	f.mu.Lock()
	s := f.sessions[id]
	if s.result == nil {
		s.result = &r
	}
	f.mu.Unlock()
	s.events <- execution.Event{Ref: s.ref, Kind: execution.EventTurnEnded, Result: &r}
}

func (f *fakeExecution) cancel(_ context.Context, _ execution.Scope, ref execution.Ref) (execution.Result, error) {
	f.mu.Lock()
	f.cancels = append(f.cancels, ref)
	f.mu.Unlock()
	if f.onCancel != nil {
		if err := f.onCancel(ref); err != nil {
			return execution.Result{}, err
		}
	}
	r := execution.Result{Status: execution.StatusCancelled, StopReason: "cancelled"}
	f.finish(ref.SessionID, r)
	return r, nil
}

// awaitStart returns the next accepted session, failing the test if none arrives.
func (f *fakeExecution) awaitStart(t *testing.T) string {
	t.Helper()
	select {
	case id := <-f.startedCh:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("no Agent session started")
		return ""
	}
}

func (f *fakeExecution) request(id string) execution.StartRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.started {
		if s := f.sessions[id]; s != nil && s.caller.AttemptID == r.Caller.AttemptID {
			return r
		}
	}
	return execution.StartRequest{}
}

func completed(answer string) execution.Result {
	return execution.Result{Status: execution.StatusCompleted, Answer: answer, AnswerComplete: true, StopReason: "end_turn"}
}

// fixture is one tenant with a project and a host over a real PostgreSQL store.
type fixture struct {
	s       *store.Store
	exec    *fakeExecution
	host    *Host
	scope   server.WorkflowRunScope
	project string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s := storetest.Open(t)
	user, tenant := storetest.Login(t, s, "workflow-host")
	scope := server.WorkflowRunScope{TenantID: tenant, UserID: user}
	project, err := s.CreateProject(context.Background(), execution.Scope{TenantID: tenant, UserID: user}, store.Project{
		ID: "project-host", Name: "演示项目", RepositoryURL: "https://example.invalid/demo.git", Branch: "main",
		Commit: "0123456789abcdef0123456789abcdef01234567", MirrorPath: "/tmp/project-host", SnapshotRef: "refs/ora/snapshots/project-host",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: s, exec: newFakeExecution(), scope: scope, project: project.ID}
	f.host = f.newHost(t)
	return f
}

func (f *fixture) newHost(t *testing.T) *Host {
	t.Helper()
	h, err := NewHost(Config{Store: f.s, Execution: f.exec, Catalog: f.exec, StopTimeout: time.Second, BusyRetry: time.Second,
		Now: func() time.Time { return time.UnixMilli(1_700_000_000_000) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// run publishes graph and creates a pending run of it.
func (f *fixture) run(t *testing.T, graph string, input *string) string {
	t.Helper()
	ctx := context.Background()
	w, _, err := f.s.CreateWorkflow(ctx, f.scope.TenantID, fmt.Sprintf("工作流-%d", time.Now().UnixNano()), graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PublishWorkflow(ctx, f.scope.TenantID, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	run, err := f.s.CreateWorkflowRun(ctx, f.scope.TenantID, w.ID, "", store.WorkflowRunInput{ProjectID: f.project, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// await waits until the run satisfies done, re-reading it after each invalidation.
func (f *fixture) await(t *testing.T, runID string, done func(store.WorkflowRun, []store.WorkflowNodeRun) bool) (store.WorkflowRun, []store.WorkflowNodeRun) {
	t.Helper()
	events, cancel := f.host.Hub().Subscribe(runID)
	defer cancel()
	timeout := time.After(5 * time.Second)
	for {
		run, err := f.s.WorkflowRun(context.Background(), f.scope.TenantID, runID)
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := f.s.WorkflowEngineNodeRuns(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if done(run, nodes) {
			return run, nodes
		}
		select {
		case <-events:
		case <-time.After(20 * time.Millisecond): // also catch transitions published before Subscribe
		case <-timeout:
			t.Fatalf("run %s never reached the expected state: %+v %+v", runID, run, nodes)
		}
	}
}

func terminal(run store.WorkflowRun, _ []store.WorkflowNodeRun) bool {
	return run.Status != store.WorkflowStatusPending && run.Status != store.WorkflowStatusRunning
}

func node(nodes []store.WorkflowNodeRun, id string) store.WorkflowNodeRun {
	for _, n := range nodes {
		if n.NodeID == id {
			return n
		}
	}
	return store.WorkflowNodeRun{}
}
