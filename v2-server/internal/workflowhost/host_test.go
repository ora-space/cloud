package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// twoAgents is the demo chain: B reads A's answer through the variable pool.
const twoAgents = `{"nodes":[
 {"id":"start","data":{"kind":"start","title":"开始"}},
 {"id":"a","data":{"kind":"agent","title":"起草","description":"写一段初稿","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"Other Model"},"prompt":"起草：{{#start.input#}}","roleId":"reviewer"}}},
 {"id":"b","data":{"kind":"agent","title":"审阅","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"missing-model"},"prompt":"审阅这段初稿：{{#a.output#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"review","variableSelector":["b","output"]}]}}
],"edges":[{"source":"start","target":"a"},{"source":"a","target":"b"},{"source":"b","target":"out"}]}`

func str(s string) *string { return &s }

func promptText(t *testing.T, p execution.Prompt) string {
	t.Helper()
	text, err := p.Text()
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestTwoAgentChainHandsTheAnswerDownstream(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("支付重构"))

	run, err := f.host.StartRun(ctx, f.scope, runID)
	if err != nil || run.Status != store.WorkflowStatusRunning {
		t.Fatalf("start = %+v %v", run, err)
	}
	a := f.exec.awaitStart(t)
	reqA := f.exec.request(a)
	if c := reqA.Caller; c.Kind != execution.CallerWorkflow || c.ID != runID || c.NodeID != "a" || c.AttemptID == "" || len(c.Rounds) != 0 {
		t.Fatalf("caller = %+v", reqA.Caller)
	}
	if reqA.ExecutorID != "opencode:other" || reqA.Target.ProjectID != f.project {
		t.Fatalf("A request = %+v", reqA)
	}
	textA := promptText(t, reqA.Prompt)
	for _, want := range []string{
		"<workspace_root>\n你的工作区根目录是：\n.\n", "当前步骤: \"起草\" (`a`)", "步骤说明:\n写一段初稿", "任务要求:\n起草：支付重构",
		"直接前置节点: \"开始\" (`start`)", "- [已成功] \"开始\" (`start`) -> \"起草\" (`a`)", "- [运行中，当前节点] \"起草\" (`a`)",
		"- [未开始] \"审阅\" (`b`)", "## 工作流原始请求\n", "</current_workflow_step>\n\n<workflow_context>",
	} {
		if !strings.Contains(textA, want) {
			t.Fatalf("A prompt lacks %q:\n%s", want, textA)
		}
	}
	if strings.Contains(textA, "reviewer") || strings.Contains(textA, "system_instructions") {
		t.Fatalf("A prompt applied the role:\n%s", textA)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").SessionID != nil
	})
	if *node(nodes, "a").SessionID != a {
		t.Fatalf("A bound to %v", *node(nodes, "a").SessionID)
	}

	f.exec.finish(a, completed("初稿：先拆网关"))
	b := f.exec.awaitStart(t)
	reqB := f.exec.request(b)
	if reqB.ExecutorID != "opencode:fixture" {
		t.Fatalf("B executor = %s, want the Agent's default", reqB.ExecutorID)
	}
	if textB := promptText(t, reqB.Prompt); !strings.Contains(textB, "任务要求:\n审阅这段初稿：初稿：先拆网关") {
		t.Fatalf("B did not read A's answer:\n%s", textB)
	}
	f.exec.finish(b, completed("可以合并"))

	run, nodes = f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusSucceeded || run.Output == nil || *run.Output != `{"review":"可以合并"}` {
		t.Fatalf("run = %+v", run)
	}
	var payloadA, payloadB map[string]any
	_ = json.Unmarshal(node(nodes, "a").Payload, &payloadA)
	_ = json.Unmarshal(node(nodes, "b").Payload, &payloadB)
	if payloadA["stop_reason"] != "end_turn" || payloadA["executor"].(map[string]any)["id"] != "opencode:other" ||
		payloadA["model"].(map[string]any)["fallback"] != false || len(payloadA["unapplied"].([]any)) != 1 || payloadA["unapplied"].([]any)[0] != "role" {
		t.Fatalf("A payload = %v", payloadA)
	}
	if payloadB["task"] != "审阅这段初稿：初稿：先拆网关" {
		t.Fatalf("B task = %v, want the rendered template", payloadB["task"])
	}
	if m := payloadB["model"].(map[string]any); m["fallback"] != true || m["requested"] != "missing-model" || m["id"] != "fixture" {
		t.Fatalf("B model = %v", m)
	}
	if len(f.exec.ended) != 2 {
		t.Fatalf("sessions ended = %v", f.exec.ended)
	}
	// A late duplicate terminal event changes nothing.
	f.host.complete(workflow.AgentDispatch{RunID: runID, NodeRunID: node(nodes, "b").ID}, workflow.Completion{Output: str("迟到")})
	if again, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); *again.Output != *run.Output {
		t.Fatalf("duplicate completion rewrote the run: %+v", again)
	}
}

func errorKind(t *testing.T, n store.WorkflowNodeRun) string {
	t.Helper()
	var p struct {
		ErrorDetail struct {
			Kind string `json:"kind"`
		} `json:"error_detail"`
	}
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", n.Payload, err)
	}
	return p.ErrorDetail.Kind
}

func TestAgentOutcomesFailTheRun(t *testing.T) {
	cases := map[string]struct {
		result execution.Result
		kind   string
	}{
		"turn failed":       {execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"}, "session"},
		"answer incomplete": {execution.Result{Status: execution.StatusCompleted, Answer: "半截", StopReason: "end_turn"}, "session"},
		"refusal":           {execution.Result{Status: execution.StatusCompleted, Answer: "不行", AnswerComplete: true, StopReason: "refusal"}, "agent_refusal"},
		"no stop reason":    {execution.Result{Status: execution.StatusCompleted, AnswerComplete: true}, "session_ended_without_stop_reason"},
		"unknown reason":    {execution.Result{Status: execution.StatusCompleted, AnswerComplete: true, StopReason: "paused"}, "unknown_stop_reason"},
	}
	f := newFixture(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			runID := f.run(t, twoAgents, str("输入"))
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(f.exec.awaitStart(t), c.result)
			run, nodes := f.await(t, runID, terminal)
			a := node(nodes, "a")
			if run.Status != store.WorkflowStatusFailed || a.Status != store.WorkflowStatusFailed || errorKind(t, a) != c.kind {
				t.Fatalf("run %s, node %+v kind %s", run.Status, a, errorKind(t, a))
			}
			if node(nodes, "b").ID != "" {
				t.Fatal("the run advanced past a failed node")
			}
			if c.result.Answer != "" && (a.Output == nil || *a.Output != c.result.Answer) {
				t.Fatalf("failed attempt lost its raw answer: %+v", a)
			}
		})
	}
}

func TestNodesFailBeforeStartingAnAgent(t *testing.T) {
	cases := map[string]struct{ agent, kind string }{
		"unknown agent":       {`{"executor":{"agentCli":"official/ora-space.codex","modelId":"x"},"prompt":"hi"}`, "missing_agent_ref"},
		"no agent":            {`{"prompt":"hi"}`, "missing_agent_ref"},
		"unassigned template": {`{"executor":{"agentCli":"official/ora-space.claude"},"prompt":"{{#start.topic#}}"}`, "prompt_template"},
	}
	f := newFixture(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string"}]}},{"id":"a","data":{"kind":"agent","agentConfig":` + c.agent + `}}],"edges":[{"source":"start","target":"a"}]}`
			runID := f.run(t, graph, nil)
			if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
				t.Fatal(err)
			}
			run, nodes := f.await(t, runID, terminal)
			if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != c.kind {
				t.Fatalf("run %s, node %+v", run.Status, node(nodes, "a"))
			}
		})
	}
	if len(f.exec.started) != 0 {
		t.Fatalf("an Agent started: %+v", f.exec.started)
	}
}

func TestCancelStopsTheActiveTurnFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	run, err := f.host.CancelRun(ctx, f.scope, runID)
	if err != nil || run.Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancel = %+v %v", run, err)
	}
	if len(f.exec.cancels) != 1 || f.exec.cancels[0].SessionID != a {
		t.Fatalf("cancelled refs = %+v", f.exec.cancels)
	}
	_, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "a").Status == store.WorkflowStatusCancelled
	})
	if node(nodes, "b").ID != "" {
		t.Fatal("a cancelled run dispatched its successor")
	}
	// Cancelling again is idempotent and asks execution for nothing new.
	if again, err := f.host.CancelRun(ctx, f.scope, runID); err != nil || again.Status != store.WorkflowStatusCancelled || len(f.exec.cancels) != 1 {
		t.Fatalf("repeat cancel = %+v %v (%d cancels)", again, err, len(f.exec.cancels))
	}
}

func TestUnconfirmedStopKeepsTheRunOpenUntilTheTurnEnds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec.onCancel = func(execution.Ref) error { return execution.ErrStopUnconfirmed }
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunStopUnconfirmed) {
		t.Fatalf("cancel = %v", err)
	}
	if run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID); run.Status != store.WorkflowStatusRunning {
		t.Fatalf("an unconfirmed stop closed the run: %s", run.Status)
	}
	// The turn later succeeds anyway: the cancellation request still wins and nothing advances.
	f.exec.finish(a, completed("赶在取消前完成"))
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").Status != store.WorkflowStatusCancelled || node(nodes, "b").ID != "" {
		t.Fatalf("run %s nodes %+v", run.Status, nodes)
	}
}

func TestCancelWaitsForAnInFlightStart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	entered, release := make(chan struct{}), make(chan struct{})
	f.exec.beforeStart = func(execution.StartRequest) error {
		close(entered)
		<-release
		return nil
	}
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	<-entered
	var wg sync.WaitGroup
	var cancelled store.WorkflowRun
	var cancelErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		cancelled, cancelErr = f.host.CancelRun(ctx, f.scope, runID)
	}()
	// The cancellation cannot finish while Start has not returned the turn to stop.
	for !f.host.state(runID).isCancelling() {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if cancelErr != nil || cancelled.Status != store.WorkflowStatusCancelled {
		t.Fatalf("cancel = %+v %v", cancelled, cancelErr)
	}
	a := f.exec.awaitStart(t)
	if len(f.exec.cancels) != 1 || f.exec.cancels[0].SessionID != a {
		t.Fatalf("cancel did not stop the turn Start returned: %+v", f.exec.cancels)
	}
}

func (st *runState) isCancelling() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cancelling
}

func TestRefusedBindingEndsTheSession(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("输入"))
	// Something outside the host closes the run while the Agent starts, so the attempt is no
	// longer running when its session arrives.
	f.exec.beforeStart = func(execution.StartRequest) error {
		_, err := f.s.CancelWorkflowRun(ctx, f.scope.TenantID, runID)
		return err
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	a := f.exec.awaitStart(t)
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool {
		f.exec.mu.Lock()
		defer f.exec.mu.Unlock()
		return len(f.exec.ended) == 1
	})
	if f.exec.ended[0] != a {
		t.Fatalf("ended %v, want %s", f.exec.ended, a)
	}
	run, nodes := f.await(t, runID, terminal)
	if run.Status != store.WorkflowStatusCancelled || node(nodes, "a").SessionID != nil {
		t.Fatalf("run %s node %+v", run.Status, node(nodes, "a"))
	}
}

func TestBusyWorkspaceIsWaitedOut(t *testing.T) {
	f := newFixture(t)
	busy := 2
	f.exec.beforeStart = func(execution.StartRequest) error {
		if busy > 0 {
			busy--
			return execution.ErrWorkspaceBusy
		}
		return nil
	}
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("A"))
	f.exec.finish(f.exec.awaitStart(t), completed("B"))
	if run, _ := f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded || len(f.exec.started) != 2 {
		t.Fatalf("run %s after %d starts", run.Status, len(f.exec.started))
	}
}

func TestRestartInterruptsExecutingRuns(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	runID := f.run(t, twoAgents, str("输入"))
	if _, err := f.host.StartRun(ctx, f.scope, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool { return node(n, "a").SessionID != nil })
	f.host.Close() // the process stops while A executes

	next := f.newHost(t)
	if err := next.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	run, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, runID)
	nodes, _ := f.s.WorkflowEngineNodeRuns(ctx, runID)
	if run.Status != store.WorkflowStatusFailed || errorKind(t, node(nodes, "a")) != "interrupted_by_restart" {
		t.Fatalf("run %s node %+v", run.Status, node(nodes, "a"))
	}
	if _, err := next.StartRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunNotStartable) {
		t.Fatalf("restart an interrupted run = %v", err)
	}
}

func TestLifecycleAuthorizesAndTranslatesRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string","required":true}]}},{"id":"out","data":{"kind":"output"}}],"edges":[{"source":"start","target":"out"}]}`
	runID := f.run(t, graph, nil)
	stranger := server.WorkflowRunScope{TenantID: "00000000-0000-0000-0000-000000000000", UserID: f.scope.UserID}
	if _, err := f.host.StartRun(ctx, stranger, runID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("foreign start = %v", err)
	}
	if _, err := f.host.CancelRun(ctx, stranger, runID); !errors.Is(err, store.ErrWorkflowRunNotFound) {
		t.Fatalf("foreign cancel = %v", err)
	}

	var invalid *server.WorkflowRunInputInvalid
	if _, err := f.host.UpdateRunInput(ctx, f.scope, runID, nil, map[string]json.RawMessage{"nope": json.RawMessage(`"x"`)}); !errors.As(err, &invalid) || invalid.Variable != "nope" || invalid.Reason != "undeclared" {
		t.Fatalf("undeclared input = %v", err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.As(err, &invalid) || invalid.Variable != "topic" {
		t.Fatalf("missing required input = %v", err)
	}
	if _, err := f.host.UpdateRunInput(ctx, f.scope, runID, str("开工"), map[string]json.RawMessage{"topic": json.RawMessage(`"支付"`)}); err != nil {
		t.Fatal(err)
	}
	run, err := f.host.StartRun(ctx, f.scope, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run, _ = f.await(t, runID, terminal); run.Status != store.WorkflowStatusSucceeded {
		t.Fatalf("swift-only run = %+v", run)
	}
	if _, err := f.host.UpdateRunInput(ctx, f.scope, runID, str("太晚"), nil); !errors.Is(err, server.ErrWorkflowRunNotEditable) {
		t.Fatalf("late input = %v", err)
	}
	if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.Is(err, server.ErrWorkflowRunNotStartable) {
		t.Fatalf("second start = %v", err)
	}
	if _, err := f.host.CompleteNode(ctx, f.scope, runID, "out", false, "human"); !errors.Is(err, server.ErrWorkflowNodeNotAwaitingInput) {
		t.Fatalf("complete = %v", err)
	}
	if _, err := f.host.CancelRun(ctx, f.scope, runID); !errors.Is(err, store.ErrWorkflowRunState) {
		t.Fatalf("cancel a succeeded run = %v", err)
	}
	if f.host.state(runID) != nil {
		t.Fatal("a finished run kept its process state")
	}
}

// parallelAgents fans Start out to two Agents; v2 executes them one at a time.
const parallelAgents = `{"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode"},"prompt":"A"}}},
 {"id":"b","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode"},"prompt":"B"}}}
],"edges":[{"source":"start","target":"a"},{"source":"start","target":"b"}]}`

func TestQueuedSiblingOfAFailedNodeNeverStarts(t *testing.T) {
	f := newFixture(t)
	runID := f.run(t, parallelAgents, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, runID); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	f.exec.finish(first, execution.Result{Status: execution.StatusFailed, FailureCode: "execution_failed"})
	run, nodes := f.await(t, runID, func(_ store.WorkflowRun, n []store.WorkflowNodeRun) bool {
		return node(n, "b").Status == store.WorkflowStatusCancelled
	})
	if run.Status != store.WorkflowStatusFailed || node(nodes, "a").Status != store.WorkflowStatusFailed {
		t.Fatalf("run %s, a %+v", run.Status, node(nodes, "a"))
	}
	if b := node(nodes, "b"); b.SessionID != nil || b.FinishedAt == nil {
		t.Fatalf("queued sibling = %+v", b)
	}
	select {
	case id := <-f.exec.startedCh:
		t.Fatalf("the sibling of a failed node started session %s", id)
	case <-time.After(100 * time.Millisecond):
	}
	f.await(t, runID, func(store.WorkflowRun, []store.WorkflowNodeRun) bool { return f.host.state(runID) == nil })
}

func TestRefusedStartForgetsTheStarter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := `{"nodes":[{"id":"start","data":{"kind":"start","inputVariables":[{"name":"topic","valueType":"string","required":true}]}},` +
		`{"id":"a","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode"},"prompt":"{{#start.topic#}}"}}}],` +
		`"edges":[{"source":"start","target":"a"}]}`
	runID := f.run(t, graph, nil)
	var invalid *server.WorkflowRunInputInvalid
	if _, err := f.host.StartRun(ctx, f.scope, runID); !errors.As(err, &invalid) {
		t.Fatalf("start without the required input = %v", err)
	}
	if f.host.state(runID) != nil {
		t.Fatal("a refused start kept its starter as the run's owner")
	}

	// A teammate fixes the input and starts the run: the Agent executes under their scope.
	teammate := server.WorkflowRunScope{TenantID: f.scope.TenantID, UserID: "00000000-0000-0000-0000-0000000000aa"}
	if _, err := f.host.UpdateRunInput(ctx, teammate, runID, nil, map[string]json.RawMessage{"topic": json.RawMessage(`"支付"`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.StartRun(ctx, teammate, runID); err != nil {
		t.Fatal(err)
	}
	f.exec.awaitStart(t)
	f.exec.mu.Lock()
	scopes := append([]execution.Scope{}, f.exec.scopes...)
	f.exec.mu.Unlock()
	if want := (execution.Scope{TenantID: teammate.TenantID, UserID: teammate.UserID}); len(scopes) != 1 || scopes[0] != want {
		t.Fatalf("Agent scopes = %+v, want %+v", scopes, want)
	}
}
