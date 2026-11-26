package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/completiontool"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/executionruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// reportingDriver is an Agent driver whose Agent reports the step done in every turn, through the
// tool it was started with, as the real one does.
type reportingDriver struct {
	t  *testing.T
	db *store.Store

	mu      sync.Mutex
	agents  map[string]*fakeAgent
	answers []string // what the tool answered, per turn
	tokens  []string
	runs    int // turns that reached this driver
}

// execution is the one execution this driver runs a session in.
func (d *reportingDriver) execution(sid string) string { return fmt.Sprintf("%p-%s", d, sid) }

func (d *reportingDriver) Run(ctx context.Context, _ execution.Scope, s execution.Session, turn execution.Turn, _ func(context.Context, execution.Event) error) (execution.Result, error) {
	done := execution.Result{Status: execution.StatusCompleted, AnswerComplete: true, Answer: "做完了", StopReason: "end_turn"}
	d.mu.Lock()
	d.runs++
	d.mu.Unlock()
	if turn.Host == nil {
		return done, nil
	}
	d.mu.Lock()
	agent := d.agents[s.ID]
	if agent == nil {
		agent = &fakeAgent{t: d.t, url: turn.Host.Attach(s.ID, d.execution(s.ID)).Servers[0].URL}
		d.agents[s.ID] = agent
		if err := d.db.RecordHostExecution(ctx, d.execution(s.ID), s.ID); err != nil {
			d.t.Error(err)
		}
		turn.Host.Started()
	}
	d.mu.Unlock()
	instruction := turn.Host.Instruct()
	// The Agent reports as soon as it has the prompt: the first one reaches it only once the node is
	// bound to its session (execution.StartRequest.Release).
	answer := agent.report(instruction.Token)
	d.mu.Lock()
	d.answers, d.tokens = append(d.answers, answer), append(d.tokens, instruction.Token)
	d.mu.Unlock()
	return done, nil
}
func (*reportingDriver) End(context.Context, string) error { return nil }
func (*reportingDriver) Rebuild(string)                    {}
func (*reportingDriver) Reattach(string, string, string)   {}
func (*reportingDriver) Close(context.Context) error       { return nil }
func (*reportingDriver) StopTurn(execution.Ref) bool       { return false }

// Over the real execution service and stored state: the tool is live while a turn of the node's own
// session executes and at no other time, and neither the recorded prompt, the turns nor the node's
// payload carry a token.
func TestTheToolFollowsStoredExecutionState(t *testing.T) {
	f := newFixture(t)
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
	f.host, err = NewHost(Config{Store: f.s, Execution: runtime, Catalog: runtime, CompletionTool: tools,
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
	runID := f.run(t, agentChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	p := f.settledTurns(t, runID, 1)
	if !reflect.DeepEqual(blockerKinds(p), []string{blockerTurnUnconfirmed, blockerConfirmationPending}) {
		t.Fatalf("summary = %v", p)
	}
	_, nodes := f.await(t, runID, parked("a"))
	row := node(nodes, "a")
	live := func(nodeRun, session string) bool {
		t.Helper()
		ok, err := f.s.WorkflowNodeTurnLive(ctx, nodeRun, session, driver.execution(session))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if live(row.ID, *row.SessionID) {
		t.Fatal("a parked node's turn is live")
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	p = f.settledTurns(t, runID, 2)
	if !reflect.DeepEqual(blockerKinds(p), []string{blockerConfirmationPending}) {
		t.Fatalf("summary = %v", p)
	}
	driver.mu.Lock()
	answers, tokens := driver.answers, driver.tokens
	driver.mu.Unlock()
	if len(answers) != 2 || !strings.HasPrefix(answers[0], "accepted:") || !strings.HasPrefix(answers[1], "accepted:") || answers[0] == answers[1] {
		t.Fatalf("answers = %v", answers)
	}
	if live(row.ID, "another-session") || live("00000000-0000-4000-8000-000000000000", *row.SessionID) {
		t.Fatal("another node's or session's turn is live")
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{ConfirmSignalID: signalOf(p)}); err != nil {
		t.Fatal(err)
	}
	f.succeeded(t, runID)
	_, nodes = f.await(t, runID, terminal)
	turns, err := runtime.ReadTurns(ctx, executionScope(f.scope), *row.SessionID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(map[string]any{"nodes": nodes, "turns": turns})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range tokens {
		if strings.Contains(string(stored), token) {
			t.Fatal("a token was stored")
		}
	}
}

// A parked node waits through a restart while its old execution does not: it is settled, with the
// restart as its reason, before the tool is served again, and the next turn runs in a new one.
func TestARestartSettlesTheExecutionOfAParkedNode(t *testing.T) {
	f, db := newFixtureWithAdmin(t)
	f.host.Close()
	ctx := t.Context()
	// start is one server process over the stored state, in main's order.
	start := func(check func()) (stop func()) {
		t.Helper()
		driver := &reportingDriver{t: t, db: f.s, agents: map[string]*fakeAgent{}}
		runtime, err := executionruntime.New(ctx, executionruntime.Config{Store: f.s, Models: executionruntime.WithFixtureModels(nil), Agent: driver})
		if err != nil {
			t.Fatal(err)
		}
		check()
		tools := completiontool.NewRegistry(f.s, runtime.HostExecutionEnding)
		served, err := completiontool.Serve(tools)
		if err != nil {
			t.Fatal(err)
		}
		f.host, err = NewHost(Config{Store: f.s, Execution: runtime, Catalog: runtime, CompletionTool: tools,
			CheckHostTools: func(context.Context, execution.Scope, string) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.host.Interrupt(ctx); err != nil {
			t.Fatal(err)
		}
		host := f.host
		return func() {
			host.Close()
			if err := runtime.Close(context.Background()); err != nil {
				t.Error(err)
			}
			served.Close()
		}
	}
	stop := start(func() {})
	runID := f.run(t, agentChain, str("支付重构"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	signal := signalOf(f.settledTurns(t, runID, 1))
	_, nodes := f.await(t, runID, parked("a"))
	sid := *node(nodes, "a").SessionID
	session, err := f.s.ReadExecutionSession(ctx, executionScope(f.scope), sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.RecordWorkspace(ctx, session.WorkspaceID, store.WorkspaceLocation{Path: t.TempDir(), BaseCommit: "fixed", CloneExecution: "c-1"}); err != nil {
		t.Fatal(err)
	}
	executions := func(want int) []storetest.HostExecution {
		t.Helper()
		got := storetest.HostExecutions(t, db, sid)
		if len(got) != want {
			t.Fatalf("executions = %+v, want %d", got, want)
		}
		return got
	}
	if x := executions(1)[0]; x.EndedAt != nil {
		t.Fatalf("a parked node's execution = %+v", x)
	}
	stop()

	stop = start(func() {
		if x := executions(1)[0]; x.EndedAt == nil || x.Reason != execution.FailureServerRestarted {
			t.Fatalf("before the tool is served, the execution is %+v", x)
		}
		if session, err := f.s.ReadExecutionSession(ctx, executionScope(f.scope), sid); err != nil || session.EndedAt != nil {
			t.Fatalf("session = %+v %v, want it open", session, err)
		}
	})
	defer func() { stop() }()
	p := f.settledTurns(t, runID, 1)
	if signalOf(p) != signal || !reflect.DeepEqual(blockerKinds(p), []string{blockerTurnUnconfirmed, blockerConfirmationPending}) {
		t.Fatalf("summary after the restart = %v", p)
	}
	if _, err := f.host.SendNodeTurn(ctx, f.scope, runID, "a", "继续"); err != nil {
		t.Fatal(err)
	}
	p = f.settledTurns(t, runID, 2)
	if x := executions(2); x[0].Reason != execution.FailureServerRestarted || x[1].EndedAt != nil || x[0].ID == x[1].ID {
		t.Fatalf("executions = %+v", x)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "a", server.WorkflowNodeCompletion{ConfirmSignalID: signalOf(p)}); err != nil {
		t.Fatal(err)
	}
	f.succeeded(t, runID)
	if x := executions(2); x[0].Reason != execution.FailureServerRestarted || x[1].Reason != execution.HostSessionEnded {
		t.Fatalf("executions = %+v", x)
	}
}

// failingNode is an Agent runtime whose Node could not be started.
type failingNode struct{ reportingDriver }

func (*failingNode) CheckHostTools(context.Context, execution.Scope, string) error {
	return execution.ErrUnavailable
}

// A deployment without an Agent runtime can never give an Agent the tool, so its refusal does not ask
// to try again; one whose Node could not be started may be able to later, and says so.
func TestARunIsRefusedWithoutAnAgentRuntime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent executionruntime.AgentDriver
		want  error
	}{
		{"no Agent runtime", nil, server.ErrWorkflowCompletionToolUnavailable},
		{"Node not started", &failingNode{}, execution.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.host.Close()
			ctx := t.Context()
			runtime, err := executionruntime.New(ctx, executionruntime.Config{Store: f.s, Agent: tc.agent})
			if err != nil {
				t.Fatal(err)
			}
			f.host, err = NewHost(Config{Store: f.s, Execution: runtime, Catalog: runtime, CompletionTool: f.tools, CheckHostTools: runtime.CheckHostTools})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				f.host.Close()
				if err := runtime.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			runID := f.run(t, agentChain, str("支付重构"))
			if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.Is(err, tc.want) {
				t.Fatalf("start = %v, want %v", err, tc.want)
			}
			run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, runID)
			nodes, nodesErr := f.s.WorkflowEngineNodeRuns(ctx, runID)
			if err != nil || nodesErr != nil || run.Status != store.WorkflowStatusPending || len(nodes) != 0 {
				t.Fatalf("run = %+v %v, nodes = %+v %v", run, err, nodes, nodesErr)
			}
		})
	}
}
