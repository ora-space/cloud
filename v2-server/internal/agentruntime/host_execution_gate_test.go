//go:build linux

package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/completiontool"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

// hostedTurn is an interactive workflow node whose first turn runs: its Agent, held by a fake Node,
// was given the completion tool over the real driver, store and endpoint. The stored turn never ends,
// nor does anything else that would refuse the tool besides its execution.
type hostedTurn struct {
	t        *testing.T
	h        *ipcHarness
	s        *store.Store
	db       *pgx.Conn
	tools    *completiontool.Registry
	f        *fakeNodeIPC
	start    node.Message
	node     string
	url      string
	token    string
	done     <-chan ipcRunOutcome
	failing  atomic.Bool // the store cannot write ends
	blocking atomic.Pointer[chan struct{}]
}

func startHostedTurn(t *testing.T, emit Emit, ending func(*Driver) completiontool.Ending) *hostedTurn {
	t.Helper()
	ctx := context.Background()
	s, db := storetest.OpenWithConn(t)
	user, tenant := storetest.Login(t, s, "hosted-turn")
	scope := execution.Scope{TenantID: tenant, UserID: user}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	project, err := s.CreateProject(ctx, scope, store.Project{ID: "project", Name: "演示项目", RepositoryURL: "https://example.invalid/demo.git", Branch: "main",
		Commit: "0123456789abcdef0123456789abcdef01234567", MirrorPath: "/tmp/project", SnapshotRef: "refs/ora/snapshots/project"})
	must(err)
	w, _, err := s.CreateWorkflow(ctx, tenant, "完成工具", `{"nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]}`)
	must(err)
	published, err := s.PublishWorkflow(ctx, tenant, w.ID, "v1", nil)
	must(err)
	run, err := s.CreateWorkflowRun(ctx, tenant, w.ID, published.ID, store.WorkflowRunInput{ProjectID: project.ID})
	must(err)
	_, err = s.StartWorkflowRun(ctx, tenant, run.ID)
	must(err)
	row, err := s.CreateWorkflowNodeRun(ctx, tenant, run.ID, store.WorkflowNodeRunInput{NodeID: "agent-1", NodeType: "agent"})
	must(err)
	x := &hostedTurn{t: t, h: newIPCHarness(t, nil), s: s, db: db, node: row.ID}
	session := x.h.session("hosted", "workspace")
	turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: "first"}, Prompt: execution.TextPrompt("起草"),
		Caller: execution.Caller{Kind: execution.CallerWorkflow, ID: run.ID, NodeID: "agent-1", AttemptID: row.ID}}
	must(s.CreateExecutionSession(ctx, scope, session, turn))
	_, err = s.BindWorkflowNodeSession(ctx, tenant, run.ID, row.ID, session.ID)
	must(err)

	x.h.driver.hostExecutionRetry = 10 * time.Millisecond
	x.h.driver.opts.RecordHostExecution = s.RecordHostExecution
	x.h.driver.opts.EndHostExecution = func(ctx context.Context, id, reason string) error {
		if release := x.blocking.Load(); release != nil {
			<-*release
		}
		if x.failing.Load() {
			return errors.New("database unavailable")
		}
		return s.EndHostExecution(ctx, id, reason)
	}
	x.tools = completiontool.NewRegistry(s, ending(x.h.driver))
	served, err := completiontool.Serve(x.tools)
	must(err)
	t.Cleanup(served.Close)
	x.done = runHosted(x.h, ipcContext(t), session, "first", x.tools.Attempt(row.ID, row.ID), emit)
	x.f = receiveIPC(t, x.h.created)
	x.start = nextIPCCommand(t, x.f, "start_agent_session")
	var payload struct {
		Spec struct {
			Servers []struct{ URL string } `json:"host_mcp_servers"`
		}
	}
	must(json.Unmarshal(x.start.Payload, &payload))
	if len(payload.Spec.Servers) != 1 {
		t.Fatalf("the Agent was given %v", payload.Spec.Servers)
	}
	x.url = payload.Spec.Servers[0].URL
	texts := sentText(t, x.start)
	match := regexp.MustCompile(`unchanged: ([0-9a-f]{32})`).FindStringSubmatch(texts[len(texts)-1])
	if match == nil {
		t.Fatal("the Agent was given no token")
	}
	x.token = match[1]
	return x
}

func (x *hostedTurn) ask(method, params string) (int, string) {
	x.t.Helper()
	resp, err := http.Post(x.url, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+params+`}`))
	if err != nil {
		x.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		x.t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func (x *hostedTurn) report() string {
	x.t.Helper()
	status, body := x.ask("tools/call", `{"name":"`+completiontool.ToolName+`","arguments":{"attemptToken":"`+x.token+`"}}`)
	if status != http.StatusOK {
		x.t.Fatalf("tools/call = %d", status)
	}
	return body
}

// refused fails unless every request of the Agent is refused and none of them leaves anything behind.
func (x *hostedTurn) refused(when string) {
	x.t.Helper()
	for _, method := range []string{"initialize", "tools/list", "ping"} {
		if status, _ := x.ask(method, "{}"); status != http.StatusUnauthorized {
			x.t.Fatalf("%s: %s = %d", when, method, status)
		}
	}
	if body := x.report(); !strings.Contains(body, string(completiontool.RefusedNotAwaiting)) || !strings.Contains(body, `"isError":true`) {
		x.t.Fatalf("%s: tools/call = %s", when, body)
	}
	if unconnected, known := x.tools.Unconnected(x.node); !unconnected || !known {
		x.t.Fatalf("%s: unconnected = %v, known %v", when, unconnected, known)
	}
	if signal := x.tools.Take(x.node, x.node); signal != nil {
		x.t.Fatalf("%s: a refused call left a signal", when)
	}
}

// recorded is the stored end of the execution, or "" while it runs.
func (x *hostedTurn) recorded() string {
	x.t.Helper()
	rows := storetest.HostExecutions(x.t, x.db, "hosted")
	if len(rows) != 1 || rows[0].ID != x.start.ExecutionID {
		x.t.Fatalf("executions = %+v", rows)
	}
	return rows[0].Reason
}

func storedOnly(*Driver) completiontool.Ending {
	return func(string, string) bool { return false }
}

func withDriver(d *Driver) completiontool.Ending { return d.HostExecutionEnding }

// Nothing but the execution decides here: while it runs the Agent is served.
func TestARunningExecutionMayUseItsTool(t *testing.T) {
	x := startHostedTurn(t, discardIPCEvent, withDriver)
	for _, method := range []string{"initialize", "tools/list", "ping"} {
		if status, _ := x.ask(method, "{}"); status != http.StatusOK {
			t.Fatalf("%s = %d", method, status)
		}
	}
	if body := x.report(); !strings.Contains(body, "accepted:") {
		t.Fatalf("tools/call = %s", body)
	}
	if x.tools.Take(x.node, x.node) == nil {
		t.Fatal("the report was not kept")
	}
	x.f.finishTurn(t, x.start, "first", "done")
	receiveIPC(t, x.done)
}

// Each way an execution stops running voids its credential at once, by the stored record alone, while
// the turn it ran is still stored as running: what decides that this server ends it, the end it asks
// Node for, Node's own report, the loss that interrupts it, and an end Node never confirmed.
// (v2-server does not ask Node for GetExecutionStatus; that last one is its StopUnconfirmed.)
func TestAnExecutionThatStoppedRunningVoidsItsCredential(t *testing.T) {
	end := func(x *hostedTurn) <-chan error {
		ended := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ended <- x.h.driver.End(ctx, "hosted")
		}()
		return ended
	}
	for _, tc := range []struct {
		name, reason string
		// happen returns once the execution stopped running, and how the test goes on after it.
		happen func(t *testing.T, x *hostedTurn) (then func())
	}{
		{"this server decides to end it", "agent_identity_mismatch", func(t *testing.T, x *hostedTurn) func() {
			sequence := uint64(1)
			x.f.in <- node.Message{Type: "session_turn_failed", Version: 1, OperationID: "op-other", ExecutionID: x.start.ExecutionID, Sequence: &sequence, Payload: json.RawMessage(`{}`)}
			nextIPCCommand(t, x.f, "end_session")
			return func() { x.f.terminal(x.start); receiveIPC(t, x.done) }
		}},
		{"EndSession is sent", execution.HostSessionEnded, func(t *testing.T, x *hostedTurn) func() {
			ended := end(x)
			nextIPCCommand(t, x.f, "end_session")
			return func() {
				x.f.terminal(x.start)
				if err := receiveIPC(t, ended); err != nil {
					t.Error(err)
				}
				receiveIPC(t, x.done)
			}
		}},
		{"Node reports its end", "idle_timeout", nil}, // held in its turn's end, below
		{"its Node is lost", execution.FailureNodeLost, func(t *testing.T, x *hostedTurn) func() {
			_ = x.f.Close()
			receiveIPC(t, x.f.project.dead)
			return func() { receiveIPC(t, x.done) }
		}},
		{"its stop is unconfirmed", execution.HostSessionEnded, func(t *testing.T, x *hostedTurn) func() {
			unsent := make(chan struct{}, 1)
			fail := func(m node.Message) error {
				if m.Type != "end_session" {
					return nil
				}
				unsent <- struct{}{}
				return net.ErrClosed
			}
			x.f.failSend.Store(&fail)
			ended := end(x)
			receiveIPC(t, unsent)
			return func() {
				receiveIPC(t, x.done)
				receiveIPC(t, ended)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emit, held, release := Emit(discardIPCEvent), make(chan struct{}, 1), make(chan struct{})
			proceed := sync.OnceFunc(func() { close(release) })
			if tc.happen == nil {
				emit = func(_ context.Context, e execution.Event) error {
					if e.Kind == execution.EventTurnEnded {
						held <- struct{}{}
						<-release
					}
					return nil
				}
				tc.happen = func(t *testing.T, x *hostedTurn) func() {
					x.f.terminalAs(x.start, tc.reason, "")
					receiveIPC(t, held)
					return func() { proceed(); receiveIPC(t, x.done) }
				}
			}
			x := startHostedTurn(t, emit, storedOnly)
			t.Cleanup(proceed) // a failure must not leave the receive loop held while the driver closes
			then := tc.happen(t, x)
			if got := x.recorded(); got != tc.reason {
				t.Errorf("recorded %q, want %q", got, tc.reason)
			}
			x.refused(tc.name)
			then()
			if got := x.recorded(); got != tc.reason {
				t.Errorf("in the end recorded %q, want %q", got, tc.reason)
			}
		})
	}
	// The start that follows a crash: the record alone says so, whatever the endpoint remembers.
	t.Run("this server restarts", func(t *testing.T) {
		x := startHostedTurn(t, discardIPCEvent, storedOnly)
		if _, err := x.s.RecoverExecutions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := x.recorded(); got != execution.FailureServerRestarted {
			t.Fatalf("recorded %q", got)
		}
		x.refused("after the restart")
		x.f.finishTurn(t, x.start, "first", "done")
		receiveIPC(t, x.done)
	})
}

// The end is written before Node is asked for it, and the credential is void from the decision on:
// while the write is under way, and while it fails, which does not keep the Agent from being stopped.
func TestAnEndIsWrittenBeforeNodeIsAskedForIt(t *testing.T) {
	t.Run("while it is written", func(t *testing.T) {
		x := startHostedTurn(t, discardIPCEvent, withDriver)
		release := make(chan struct{})
		proceed := sync.OnceFunc(func() { close(release) })
		t.Cleanup(proceed)
		x.blocking.Store(&release)
		ended := make(chan error, 1)
		go func() { ended <- x.h.driver.End(ipcContext(t), "hosted") }()
		for deadline := time.Now().Add(5 * time.Second); len(x.h.driver.HostExecutionEnds("hosted")) == 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the end was not decided")
			}
		}
		x.refused("while the end is written")
		if x.recorded() != "" || sentCommand(x.f, "end_session") {
			t.Fatal("Node was asked to end the session before its end was written")
		}
		x.blocking.Store(nil)
		proceed()
		nextIPCCommand(t, x.f, "end_session")
		if got := x.recorded(); got != execution.HostSessionEnded {
			t.Fatalf("recorded %q", got)
		}
		x.f.terminal(x.start)
		if err := receiveIPC(t, ended); err != nil {
			t.Fatal(err)
		}
		receiveIPC(t, x.done)
	})
	t.Run("while it cannot be", func(t *testing.T) {
		x := startHostedTurn(t, discardIPCEvent, withDriver)
		x.failing.Store(true)
		ended := make(chan error, 1)
		go func() { ended <- x.h.driver.End(ipcContext(t), "hosted") }()
		nextIPCCommand(t, x.f, "end_session")
		if got := x.recorded(); got != "" {
			t.Fatalf("recorded %q", got)
		}
		x.refused("while the end cannot be written")
		x.f.terminal(x.start)
		if err := receiveIPC(t, ended); err != nil {
			t.Fatal(err)
		}
		receiveIPC(t, x.done)
		x.failing.Store(false)
		for deadline := time.Now().Add(5 * time.Second); x.recorded() == ""; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the end was not written once it could be")
			}
		}
		if got := x.recorded(); got != execution.HostSessionEnded {
			t.Fatalf("recorded %q", got)
		}
	})
}

// An execution whose end is not written yet stays ending when its session no longer has it: another one
// replaced it, or the session is gone.
func TestAnUnwrittenEndOutlivesItsSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	records := recordExecutions(h)
	f, start := parkHosted(t, h, h.session("hosted", "workspace"), nil)
	records.mu.Lock()
	records.failures = 1 << 30
	records.mu.Unlock()
	f.terminalAs(start, "idle_timeout", "")
	for deadline := time.Now().Add(5 * time.Second); len(h.driver.HostExecutionEnds("hosted")) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the end was not decided")
		}
	}
	ending := func(when string) {
		t.Helper()
		if !h.driver.HostExecutionEnding("hosted", start.ExecutionID) {
			t.Fatalf("%s: an unwritten end is not ending", when)
		}
		if h.driver.HostExecutionEnding("hosted", "x-next") {
			t.Fatalf("%s: the next execution is ending", when)
		}
	}
	h.driver.mu.Lock()
	ended := h.driver.sessions["hosted"]
	h.driver.sessions["hosted"] = &agentSession{execution: "x-next"}
	h.driver.mu.Unlock()
	t.Cleanup(func() { // before the driver closes, which waits for its sessions
		h.driver.mu.Lock()
		h.driver.sessions["hosted"] = ended
		h.driver.mu.Unlock()
	})
	ending("replaced")
	h.driver.mu.Lock()
	delete(h.driver.sessions, "hosted")
	h.driver.mu.Unlock()
	ending("gone")
}

// Only an execution whose end was decided is said to be ending, and only under its own session.
func TestOnlyADecidedEndIsEnding(t *testing.T) {
	h := newIPCHarness(t, nil)
	records := recordExecutions(h)
	f, one := parkHosted(t, h, h.session("one", "workspace"), nil)
	_, two := parkHosted(t, h, h.session("two", "workspace-2"), f)
	f.terminalAs(one, "idle_timeout", "")
	receiveIPC(t, records.ended)
	for _, tc := range []struct {
		session, execution string
		want               bool
	}{
		{"one", one.ExecutionID, true}, {"two", two.ExecutionID, false},
		{"two", one.ExecutionID, false}, {"one", two.ExecutionID, false}, {"none", "none", false},
	} {
		if got := h.driver.HostExecutionEnding(tc.session, tc.execution); got != tc.want {
			t.Errorf("%s in %s: ending = %v", tc.execution, tc.session, got)
		}
	}
}

// While the end of an execution is decided but not written yet, it is ending for its own session
// only: another session asking about the same execution ID is told it is not.
func TestAnUnwrittenEndIsEndingOnlyForItsSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	records := recordExecutions(h)
	f, one := parkHosted(t, h, h.session("one", "workspace"), nil)
	_, two := parkHosted(t, h, h.session("two", "workspace-2"), f)
	writing, written := make(chan struct{}, 1), make(chan struct{})
	end := h.driver.opts.EndHostExecution
	h.driver.opts.EndHostExecution = func(ctx context.Context, id, reason string) error {
		signal(writing)
		<-written // the end stays unwritten until the checks below are done
		return end(ctx, id, reason)
	}
	f.terminalAs(one, "idle_timeout", "")
	receiveIPC(t, writing)
	for _, tc := range []struct {
		session, execution string
		want               bool
	}{
		{"one", one.ExecutionID, true}, {"two", one.ExecutionID, false}, {"two", two.ExecutionID, false},
	} {
		if got := h.driver.HostExecutionEnding(tc.session, tc.execution); got != tc.want {
			t.Errorf("%s in %s while its end is unwritten: ending = %v", tc.execution, tc.session, got)
		}
	}
	close(written)
	receiveIPC(t, records.ended)
}
