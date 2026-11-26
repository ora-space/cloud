package workflowhost

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/completiontool"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// startHook is the real execution service with something done right after a Start returns, while
// the attempt has not bound its session yet, and optionally before each Cancel.
type startHook struct {
	*executionruntime.Service
	after        func(context.Context, execution.Scope, execution.StartRequest, execution.Ref)
	beforeCancel func(execution.Ref)
}

func (h startHook) Cancel(ctx context.Context, scope execution.Scope, ref execution.Ref) (execution.Result, error) {
	if h.beforeCancel != nil {
		h.beforeCancel(ref)
	}
	return h.Service.Cancel(ctx, scope, ref)
}

func (h startHook) Start(ctx context.Context, scope execution.Scope, req execution.StartRequest) (execution.Ref, error) {
	ref, err := h.Service.Start(ctx, scope, req)
	if err == nil && h.after != nil {
		h.after(ctx, scope, req, ref)
	}
	return ref, err
}

// releaseFixture is a host over the real execution service, whose Agent reports the step done as
// soon as it has its prompt, with hook run after the first Start returns.
func releaseFixture(t *testing.T, f *fixture, hook func(context.Context, execution.Scope, execution.StartRequest, execution.Ref), beforeCancel func(execution.Ref)) (*reportingDriver, *executionruntime.Service) {
	t.Helper()
	f.host.Close()
	ctx := t.Context()
	driver := &reportingDriver{t: t, db: f.s, agents: map[string]*fakeAgent{}}
	runtime, err := executionruntime.New(ctx, executionruntime.Config{Store: f.s, Models: executionruntime.WithFixtureModels(nil), Agent: driver})
	if err != nil {
		t.Fatal(err)
	}
	tools := completiontool.NewRegistry(f.s, runtime.HostExecutionEnding)
	served, err := completiontool.Serve(tools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(served.Close)
	var once sync.Once
	exec := startHook{Service: runtime, beforeCancel: beforeCancel, after: func(ctx context.Context, scope execution.Scope, req execution.StartRequest, ref execution.Ref) {
		once.Do(func() { hook(ctx, scope, req, ref) })
	}}
	f.host, err = NewHost(Config{Store: f.s, Execution: exec, Catalog: runtime, CompletionTool: tools,
		CheckHostTools: func(context.Context, execution.Scope, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.host.Close()
		if err := runtime.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return driver, runtime
}

// The first prompt reaches the Agent only once the node is bound to its session: an Agent that
// reports at once is accepted even when the binding waits for the run's row lock.
func TestTheFirstReportIsAcceptedWhileTheBindingWaits(t *testing.T) {
	f, admin := newFixtureWithAdmin(t)
	ctx := t.Context()
	var driver *reportingDriver
	unlocked := make(chan error, 1)
	driver, _ = releaseFixture(t, f, func(ctx context.Context, _ execution.Scope, req execution.StartRequest, _ execution.Ref) {
		// The Start has returned; the attempt binds next, behind this lock on the run's row.
		tx, err := admin.Begin(ctx)
		if err != nil {
			unlocked <- err
			return
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM v2_workflow_runs WHERE id = $1 FOR UPDATE`, req.Caller.ID); err != nil {
			_ = tx.Rollback(ctx)
			unlocked <- err
			return
		}
		go func() { unlocked <- holdUntilBlocked(ctx, tx, driver) }()
	}, nil)
	runID := f.run(t, agentChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if err := <-unlocked; err != nil {
		t.Fatal(err)
	}
	f.settledTurns(t, runID, 1)
	f.await(t, runID, parked("a"))
	driver.mu.Lock()
	answers := driver.answers
	driver.mu.Unlock()
	if len(answers) != 1 || !strings.HasPrefix(answers[0], "accepted:") {
		t.Fatalf("answers = %v, want the first report accepted", answers)
	}
}

// holdUntilBlocked keeps tx's row lock until the binding waits for it, then a while longer for an
// Agent that might report meanwhile (the first prompt must not reach it before the binding).
func holdUntilBlocked(ctx context.Context, tx pgx.Tx, driver *reportingDriver) error {
	defer tx.Rollback(context.WithoutCancel(ctx))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pg_backend_pid() = ANY(pg_blocking_pids(pid)))`).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		driver.mu.Lock()
		reported := len(driver.answers) != 0
		driver.mu.Unlock()
		if reported {
			break
		}
	}
	return nil
}

// A refused binding ends the session before its first prompt reached an Agent: none is started,
// the turn never started and the node fails.
func TestARefusedBindingStartsNoAgent(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var session string
	driver, runtime := releaseFixture(t, f, func(ctx context.Context, scope execution.Scope, req execution.StartRequest, ref execution.Ref) {
		session = ref.SessionID
		// Another session took the attempt meanwhile, so its own binding is refused.
		if _, err := f.s.BindWorkflowNodeSession(ctx, scope.TenantID, req.Caller.ID, req.Caller.AttemptID, "another-session"); err != nil {
			t.Error(err)
		}
	}, nil)
	runID := f.run(t, agentChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed || node(nodes, "a").Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s, node %s, want both failed", run.Status, node(nodes, "a").Status)
	}
	assertNeverStarted(t, driver, runtime, f, session)
}

// A run cancelled while its first prompt waits for the binding ends like an attempt abandoned
// before its Start: no Agent sees the prompt, the session ends and the run is cancelled. The
// cancellation's stop of the turn reaches execution only after the binding, so the attempt itself
// must not let the prompt through.
func TestACancellationWhileTheFirstPromptWaitsStartsNoAgent(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var session string
	cancelled := make(chan error, 1)
	runID := f.run(t, agentChain, str("支付重构"))
	driver, runtime := releaseFixture(t, f, func(ctx context.Context, _ execution.Scope, _ execution.StartRequest, ref execution.Ref) {
		session = ref.SessionID
		go func() {
			_, err := f.host.CancelRun(context.WithoutCancel(ctx), f.scope, runID)
			cancelled <- err
		}()
		// Return once the cancellation is under way: it waits for this Start, then for the turn.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if st := f.host.state(runID); st != nil {
				st.mu.Lock()
				cancelling := st.cancelling
				st.mu.Unlock()
				if cancelling {
					return
				}
			}
		}
		t.Error("the cancellation never started")
	}, func(ref execution.Ref) {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			nodes, err := f.s.WorkflowEngineNodeRuns(context.Background(), runID)
			if err != nil {
				t.Error(err)
				return
			}
			if n := node(nodes, "a"); n.SessionID != nil && *n.SessionID == ref.SessionID {
				time.Sleep(100 * time.Millisecond) // past the binding: a released prompt reaches the driver
				return
			}
		}
		t.Error("the attempt never bound its session")
	})
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").Status != store.WorkflowStatusCancelled {
		t.Fatalf("run %s, node %s, want both cancelled", run.Status, node(nodes, "a").Status)
	}
	assertNeverStarted(t, driver, runtime, f, session)
}

// assertNeverStarted checks that session's first turn reached no Agent and the session ended.
func assertNeverStarted(t *testing.T, driver *reportingDriver, runtime *executionruntime.Service, f *fixture, session string) {
	t.Helper()
	driver.mu.Lock()
	runs := driver.runs
	driver.mu.Unlock()
	if runs != 0 {
		t.Errorf("%d turns reached the Agent driver", runs)
	}
	scope := executionScope(f.scope)
	s, err := runtime.ReadSession(t.Context(), scope, session)
	if err != nil {
		t.Fatal(err)
	}
	if s.EndedAt == nil {
		t.Error("the session is still open")
	}
	turns, err := runtime.ReadTurns(t.Context(), scope, session, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns.Items) != 1 || turns.Items[0].Result == nil || !turns.Items[0].Result.NotStarted {
		t.Errorf("turns = %+v, want one that never started", turns.Items)
	}
}

// A refused binding abandons the held-back first turn before the session is ended, so the turn
// ends as never started even when that End cannot read the session.
func TestARefusedBindingEndsTheFirstTurnWhenTheSessionCannotBeEnded(t *testing.T) {
	f, admin := newFixtureWithAdmin(t)
	ctx := t.Context()
	var session string
	locked := make(chan pgx.Tx, 1)
	driver, runtime := releaseFixture(t, f, func(ctx context.Context, scope execution.Scope, req execution.StartRequest, ref execution.Ref) {
		session = ref.SessionID
		// Nothing reads execution sessions while this lock is held: the End after the refused
		// binding below gives up.
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Error(err)
			close(locked)
			return
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE v2_execution_sessions IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Error(err)
			_ = tx.Rollback(ctx)
			close(locked)
			return
		}
		locked <- tx
		if _, err := f.s.BindWorkflowNodeSession(ctx, scope.TenantID, req.Caller.ID, req.Caller.AttemptID, "another-session"); err != nil {
			t.Error(err)
		}
	}, nil)
	f.host.stopTimeout = time.Second
	runID := f.run(t, agentChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	tx, ok := <-locked
	if !ok {
		t.FailNow()
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusFailed || node(nodes, "a").Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s, node %s, want both failed", run.Status, node(nodes, "a").Status)
	}
	// The End gave up and nothing ends the session again for a failed run; only the abandoned
	// release has ended the turn.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	scope := executionScope(f.scope)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		turns, err := runtime.ReadTurns(ctx, scope, session, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns.Items) == 1 && turns.Items[0].Result != nil {
			if !turns.Items[0].Result.NotStarted {
				t.Fatalf("first turn = %+v, want one that never started", turns.Items[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first turn still waits: %+v", turns.Items)
		}
	}
	driver.mu.Lock()
	runs := driver.runs
	driver.mu.Unlock()
	if runs != 0 {
		t.Errorf("%d turns reached the Agent driver", runs)
	}
}
