package workflowhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

const loopGraph = `{"schemaVersion":2,"nodes":[
 {"id":"start","data":{"kind":"start"}},
 {"id":"loop","data":{"kind":"loop","loopConfig":{"maxIterations":2,"variables":[{"name":"draft","valueType":"string","initial":{"kind":"constant","value":"seed"},"feedback":["writer","output"]}],"until":{"logic":"and","conditions":[{"variableSelector":["writer","output"],"operator":"equals","value":"done"}]},"outputs":[{"name":"result","variableSelector":["writer","output"]}]}}},
 {"id":"entry","data":{"kind":"start","containerId":"loop"}},
 {"id":"writer","data":{"kind":"agent","containerId":"loop","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"Improve {{#loop.draft#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"result","variableSelector":["loop","result"]}]}}
 ],"edges":[{"source":"start","target":"loop"},{"source":"entry","target":"writer"},{"source":"loop","target":"out"}]} `

func TestLoopFeedbackScopesAndExports(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, loopGraph, str("kickoff"))
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(first).Prompt); !strings.Contains(text, "Improve seed") {
		t.Fatal(text)
	}
	f.exec.finish(first, completed("draft"))
	second := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(second).Prompt); !strings.Contains(text, "Improve draft") {
		t.Fatal(text)
	}
	f.exec.finish(second, completed("done"))
	run, roots := f.await(t, id, terminal)
	if run.Status != "succeeded" || run.Output == nil || *run.Output != `{"result":"done"}` {
		t.Fatalf("result: %+v", run)
	}
	var parent map[string]any
	_ = json.Unmarshal(node(roots, "loop").Payload, &parent)
	if parent["stop_reason"] != "loop_succeeded" {
		t.Fatalf("parent payload: %v", parent)
	}
	p, _, err := workflow.DecodeRunPayload(run.RuntimePayload)
	if err != nil {
		t.Fatal(err)
	}
	if p.VariablePool.Values["loop.result"] != "done" {
		t.Fatal(p.VariablePool)
	}
	if _, leaked := p.VariablePool.Catalog["writer.output"]; leaked {
		t.Fatal("child pool leaked to root")
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 2 {
		t.Fatalf("rounds %v %v", rounds, err)
	}
	for i, round := range rounds {
		if *round.RoundIndex != i+1 || *round.Status != "succeeded" {
			t.Fatal(round)
		}
		rows, err := f.s.WorkflowEngineScopeNodes(ctx, id, round.ID)
		if err != nil {
			t.Fatal(err)
		}
		writer := node(rows, "writer")
		if writer.Iteration != nil || writer.SessionID == nil || *writer.SessionID != []string{first, second}[i] {
			t.Fatalf("writer %v", writer)
		}
		var state struct {
			VariablePool workflow.VariablePool `json:"variablePool"`
		}
		_ = json.Unmarshal(round.State, &state)
		if state.VariablePool.Values["loop.draft"] != []string{"seed", "draft"}[i] {
			t.Fatalf("round pool %v", state)
		}
	}
}

func TestLoopLimitBehaviorAndLastRoundUntil(t *testing.T) {
	for _, tc := range []struct{ name, behavior, answer, status, reason string }{
		{"fails", "fail", "draft", "failed", ""},
		{"exhausts successfully", "succeed", "draft", "succeeded", "loop_max_iterations"},
		{"until wins at cap", "fail", "done", "succeeded", "loop_succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			graph := strings.Replace(loopGraph, `"maxIterations":2`, `"maxIterations":1,"maxIterationsBehavior":"`+tc.behavior+`"`, 1)
			id := f.run(t, graph, nil)
			if _, err := f.host.StartRun(context.Background(), f.scope, id); err != nil {
				t.Fatal(err)
			}
			f.exec.finish(f.exec.awaitStart(t), completed(tc.answer))
			run, rows := f.await(t, id, terminal)
			if run.Status != tc.status {
				t.Fatalf("%+v", run)
			}
			if tc.status == "failed" && (run.Error == nil || !strings.Contains(*run.Error, "last observed writer.output=draft")) {
				t.Fatalf("error %v", run.Error)
			}
			var p map[string]any
			_ = json.Unmarshal(node(rows, "loop").Payload, &p)
			if tc.reason != "" && p["stop_reason"] != tc.reason {
				t.Fatal(p)
			}
		})
	}
}

func iterationGraph(strategy string, items []string, max int) string {
	raw, _ := json.Marshal(items)
	return fmt.Sprintf(`{"nodes":[
 {"id":"start","data":{"kind":"start","inputVariables":[{"name":"items","valueType":"array[string]","value":%s}]}},
 {"id":"iter","data":{"kind":"iteration","iterationConfig":{"iteratorSelector":["start","items"],"collectSelector":["worker","output"],"errorStrategy":%q,"maxIterations":%d}}},
 {"id":"gate","parentId":"iter","data":{"kind":"condition","cases":[{"id":"process","logic":"and","conditions":[{"variableSelector":["iter","item"],"operator":"not_equals","value":"skip"}]}]}},
 {"id":"worker","parentId":"iter","data":{"kind":"agent","agentConfig":{"executor":{"agentCli":"official/ora-space.opencode","modelId":"fixture"},"prompt":"item {{#iter.index#}}: {{#iter.item#}}"}}},
 {"id":"out","data":{"kind":"output","outputs":[{"name":"items","variableSelector":["iter","output"]},{"name":"failed","variableSelector":["iter","failed_count"]}]}}
 ],"edges":[{"source":"start","target":"iter"},{"source":"iter","target":"gate"},{"source":"gate","sourceHandle":"process","target":"worker"},{"source":"iter","target":"out"}]}`, string(raw), strategy, max)
}

func TestIterationContinuesFailuresWithoutCollectingStaleValues(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, iterationGraph("continue", []string{"one", "skip", "bad", "last"}, 50), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	first := f.exec.awaitStart(t)
	f.exec.finish(first, completed("first"))
	bad := f.exec.awaitStart(t)
	if text := promptText(t, f.exec.request(bad).Prompt); !strings.Contains(text, "item 2: bad") {
		t.Fatal(text)
	}
	refused := completed("refused")
	refused.StopReason = "refusal"
	f.exec.finish(bad, refused)
	last := f.exec.awaitStart(t)
	f.exec.finish(last, completed("final"))
	run, rows := f.await(t, id, terminal)
	if run.Status != "succeeded" || run.Output == nil || *run.Output != `{"failed":2,"items":["first","final"]}` {
		t.Fatalf("%+v", run)
	}
	p, _, err := workflow.DecodeRunPayload(run.RuntimePayload)
	if err != nil {
		t.Fatal(err)
	}
	ledger := p.IterationLedger["iter"]
	if len(ledger) != 4 || ledger[1].Error == nil || *ledger[1].Error != "collect target did not run this round" || ledger[2].Status != "failed" {
		t.Fatalf("ledger %+v", ledger)
	}
	if p.ConditionDecisions["gate"] != "" || p.IterationConditionDecisions["gate#0"] != "process" || p.IterationConditionDecisions["gate#1"] != "else" {
		t.Fatalf("decisions %+v", p)
	}
	workers := 0
	for _, r := range rows {
		if r.NodeID == "worker" {
			workers++
			if r.Iteration == nil || *r.Iteration == 1 {
				t.Fatalf("unexpected worker %+v", r)
			}
		}
	}
	if workers != 3 {
		t.Fatalf("workers %d", workers)
	}
	// Duplicate callbacks and repeated scheduling do not append ledger entries or re-dispatch.
	f.host.complete(workflow.AgentDispatch{RunID: id, NodeRunID: f.exec.request(first).Caller.AttemptID}, workflow.Completion{Output: str("late")})
	if err := f.host.engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	f.exec.mu.Lock()
	count := len(f.exec.started)
	f.exec.mu.Unlock()
	if count != 3 {
		t.Fatalf("replayed %d starts", count)
	}
}

func TestIterationEmptyLimitAndFailStrategy(t *testing.T) {
	for _, tc := range []struct {
		name          string
		items         []string
		max           int
		status        string
		expectedNodes int
	}{
		{"empty", []string{}, 50, "succeeded", 0},
		{"limit", []string{"one", "two"}, 1, "failed", 0},
		{"collect skipped", []string{"skip", "one"}, 50, "failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			id := f.run(t, iterationGraph("fail", tc.items, tc.max), nil)
			if _, err := f.host.StartRun(context.Background(), f.scope, id); err != nil {
				t.Fatal(err)
			}
			run, rows := f.await(t, id, terminal)
			if run.Status != tc.status {
				t.Fatalf("%+v", run)
			}
			members := 0
			for _, r := range rows {
				if r.Iteration != nil {
					members++
				}
			}
			if members != tc.expectedNodes {
				t.Fatalf("round members %d", members)
			}
			if tc.name == "empty" && (run.Output == nil || *run.Output != `{"failed":0,"items":[]}`) {
				t.Fatalf("%+v", run)
			}
		})
	}
}

func exitGraph() string {
	graph := strings.Replace(loopGraph, `"variableSelector":["writer","output"]}]}}}`, `"variableSelector":["loop","draft"]}]}}}`, 1)
	return strings.Replace(graph, `{"id":"out"`, `{"id":"exit","data":{"kind":"loopExit","containerId":"loop"}},{"id":"out"`, 1)
}

func TestLoopExitSkipsSameWaveAgentAndUnusedFeedback(t *testing.T) {
	f := newFixture(t)
	graph := strings.Replace(exitGraph(), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"entry","target":"exit"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(context.Background(), f.scope, id); err != nil {
		t.Fatal(err)
	}
	run, rows := f.await(t, id, terminal)
	if run.Status != "succeeded" || run.Output == nil || *run.Output != `{"result":"seed"}` {
		t.Fatalf("%+v", run)
	}
	var p map[string]any
	_ = json.Unmarshal(node(rows, "loop").Payload, &p)
	if p["stop_reason"] != "loop_exit" {
		t.Fatal(p)
	}
	f.exec.mu.Lock()
	count := len(f.exec.started)
	f.exec.mu.Unlock()
	if count != 0 {
		t.Fatalf("same-wave Agent started %d times", count)
	}
}

func TestLoopExitDrainsActiveSessionAndIgnoresLateCallbacks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	graph := strings.Replace(exitGraph(), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"writer","target":"exit"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	session := f.exec.awaitStart(t)
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 {
		t.Fatal(rounds, err)
	}
	awaitScopeSession(t, f, id, rounds[0].ID, "writer")
	// Inject the persisted break-ready boundary while the sibling is executing, covering the
	// same race as a swift branch reaching Exit before the run's serialized Agent finishes.
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "exit", NodeType: "loopExit", ScopeID: rounds[0].ID}}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	f.exec.onCancel = func(ref execution.Ref) error {
		close(entered)
		<-release
		f.exec.finish(ref.SessionID, execution.Result{Status: execution.StatusCancelled})
		return nil
	}
	if err := f.host.engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("LoopExit did not request cancellation")
	}
	run, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	if err != nil || run.Status != "running" {
		t.Fatalf("advanced before cleanup %+v %v", run, err)
	}
	close(release)
	run, _ = f.await(t, id, terminal)
	if run.Status != "succeeded" || run.Output == nil || *run.Output != `{"result":"seed"}` {
		t.Fatalf("%+v", run)
	}
	request := f.exec.request(session)
	f.host.complete(workflow.AgentDispatch{RunID: id, NodeRunID: request.Caller.AttemptID}, workflow.Completion{Output: str("late")})
	if err := f.host.engine.FinishLoopExit(ctx, id, *rounds[0].ParentLoopNodeRunID, rounds[0].ID, errors.New("late cleanup error")); err != nil {
		t.Fatal(err)
	}
	again, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	if again.Status != "succeeded" || *again.Output != *run.Output {
		t.Fatalf("late mutation %+v", again)
	}
	children, err := f.s.WorkflowEngineScopeNodes(ctx, id, rounds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if node(children, "writer").Status != "cancelled" || node(children, "exit").Status != "succeeded" {
		t.Fatalf("children %+v", children)
	}
}

func awaitScopeSession(t *testing.T, f *fixture, runID, scopeID, nodeID string) {
	t.Helper()
	events, unsubscribe := f.host.Hub().Subscribe(runID)
	defer unsubscribe()
	timeout := time.After(5 * time.Second)
	for {
		rows, err := f.s.WorkflowEngineScopeNodes(context.Background(), runID, scopeID)
		if err != nil {
			t.Fatal(err)
		}
		if node(rows, nodeID).SessionID != nil {
			return
		}
		select {
		case <-events:
		case <-timeout:
			t.Fatal("session was not bound")
		}
	}
}

func TestLoopExitCannotAdvanceAfterUnconfirmedSessionEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec.EndFunc = func(context.Context, execution.Scope, string) error { return execution.ErrStopUnconfirmed }
	graph := strings.Replace(exitGraph(), `{"source":"entry","target":"writer"}`, `{"source":"entry","target":"writer"},{"source":"writer","target":"exit"}`, 1)
	id := f.run(t, graph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	_ = f.exec.awaitStart(t)
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	awaitScopeSession(t, f, id, rounds[0].ID, "writer")
	if _, err := f.s.StartWorkflowEngineNodes(ctx, id, []store.WorkflowEngineStart{{NodeID: "exit", NodeType: "loopExit", ScopeID: rounds[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.host.engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	run, rows := f.await(t, id, terminal)
	if run.Status != "failed" || run.Error == nil || !strings.Contains(*run.Error, "could not confirm session cleanup") || node(rows, "out").ID != "" {
		t.Fatalf("unsafe advance %+v %+v", run, rows)
	}
}

func TestLoopInterruptedByRestartDoesNotReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, loopGraph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	_ = f.exec.awaitStart(t)
	f.host.Close()
	f.host = f.newHost(t)
	if err := f.host.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	run, _ := f.await(t, id, terminal)
	if run.Status != "failed" {
		t.Fatalf("%+v", run)
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 1 || *rounds[0].Status != "failed" {
		t.Fatal(rounds, err)
	}
	if err := f.host.engine.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	f.exec.mu.Lock()
	count := len(f.exec.started)
	f.exec.mu.Unlock()
	if count != 1 {
		t.Fatalf("replayed %d starts", count)
	}
}

func TestLoopDoesNotReuseAnInactiveBranchFromThePreviousRound(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var graph map[string]any
	_ = json.Unmarshal([]byte(loopGraph), &graph)
	nodes := graph["nodes"].([]any)
	nodes[1].(map[string]any)["data"].(map[string]any)["loopConfig"].(map[string]any)["until"] = map[string]any{"logic": "and", "conditions": []any{}}
	nodes = append(nodes, map[string]any{"id": "gate", "data": map[string]any{"kind": "condition", "containerId": "loop", "cases": []any{map[string]any{"id": "write", "logic": "and", "conditions": []any{map[string]any{"variableSelector": []string{"loop", "draft"}, "operator": "not_equals", "value": "skip"}}}}}})
	graph["nodes"] = nodes
	graph["edges"] = []any{map[string]any{"source": "start", "target": "loop"}, map[string]any{"source": "loop", "target": "out"}, map[string]any{"source": "entry", "target": "gate"}, map[string]any{"source": "gate", "sourceHandle": "write", "target": "writer"}}
	raw, _ := json.Marshal(graph)
	id := f.run(t, string(raw), nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	f.exec.finish(f.exec.awaitStart(t), completed("skip"))
	run, _ := f.await(t, id, terminal)
	if run.Status != "failed" || run.Error == nil || !strings.Contains(*run.Error, "no value in this round: writer.output") {
		t.Fatalf("stale feedback %+v", run)
	}
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil || len(rounds) != 2 {
		t.Fatal(rounds, err)
	}
	var state struct {
		VariablePool workflow.VariablePool `json:"variablePool"`
		Decisions    map[string]string     `json:"conditionDecisions"`
	}
	_ = json.Unmarshal(rounds[1].State, &state)
	if _, present := state.VariablePool.Values["writer.output"]; present || state.Decisions["gate"] != "else" {
		t.Fatalf("stale state %+v", state)
	}
}

func TestLoopAdvancementRollsBackScopeAndPayloadTogether(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.run(t, loopGraph, nil)
	if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	_ = f.exec.awaitStart(t)
	rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := f.s.AdvanceWorkflowEngineContainer(ctx, id, *rounds[0].ParentLoopNodeRunID, store.WorkflowEngineContainerAdvance{
		ScopeID: rounds[0].ID, ScopeStatus: "succeeded",
		Fold: func([]byte) ([]byte, error) { return []byte(`{"mustRollBack":true}`), nil },
		Next: &store.WorkflowEngineRoundStart{Index: 2, State: []byte(`invalid JSON`), Entry: store.WorkflowEngineStart{NodeID: "entry", NodeType: "start"}},
	})
	if err == nil || changed {
		t.Fatal("invalid next scope committed")
	}
	after, _ := f.s.WorkflowRun(ctx, f.scope.TenantID, id)
	rounds, _ = f.s.WorkflowEngineLoopRounds(ctx, id)
	if string(after.RuntimePayload) != string(before.RuntimePayload) || len(rounds) != 1 || *rounds[0].Status != "running" {
		t.Fatalf("partial advancement %+v %+v", after, rounds)
	}
}

func TestLoopChildFailureAndCancellationCloseScope(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint(cancel), func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			id := f.run(t, loopGraph, nil)
			if _, err := f.host.StartRun(ctx, f.scope, id); err != nil {
				t.Fatal(err)
			}
			session := f.exec.awaitStart(t)
			rounds, err := f.s.WorkflowEngineLoopRounds(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			awaitScopeSession(t, f, id, rounds[0].ID, "writer")
			status := "failed"
			if cancel {
				status = "cancelled"
				if _, err := f.host.CancelRun(ctx, f.scope, id); err != nil {
					t.Fatal(err)
				}
			} else {
				r := completed("no")
				r.StopReason = "refusal"
				f.exec.finish(session, r)
			}
			run, roots := f.await(t, id, terminal)
			rounds, _ = f.s.WorkflowEngineLoopRounds(ctx, id)
			if run.Status != status || node(roots, "loop").Status != status || *rounds[0].Status != status {
				t.Fatalf("run %s parent %s scope %s", run.Status, node(roots, "loop").Status, *rounds[0].Status)
			}
		})
	}
}
