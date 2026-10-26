package core

import (
	"context"
	"strings"
	"testing"
)

func TestWorkflowHistoricalFailureDoesNotUnlockReserve(t *testing.T) {
	for _, historical := range []string{"failed", "cancelled"} {
		for _, transition := range []string{"succeeded", "failed", "cancelled"} {
			t.Run(historical+"/"+transition, func(t *testing.T) {
				store := dispatcherDB(t)
				g := iterationRunGraph("continue", 10, []any{"it", "item"})
				id := seedWorkflowRunScene(t, store, g)
				states := map[string]workflowNodeState{
					"it": {Status: "succeeded", Output: Object{"output": []any{}, "failed_count": 1}, Iteration: &workflowIterationRunState{
						Ledger: []any{Object{"item": "old", "status": "failed", "output": nil, "error": "old failure"}}, Failed: 1,
					}},
					"agg":     {Status: historical, Attempt: 1},
					"later":   {Status: "running", Attempt: 1},
					"padding": {Status: "succeeded", Output: Object{"value": ""}},
				}
				n := workflowStateLimit - workflowFailureReserve - 100 - len(jsonText(workflowEncodedStates(states)))
				states["padding"] = workflowNodeState{Status: "succeeded", Output: Object{"value": strings.Repeat("x", n)}}
				commitWorkflowRunMidpoint(t, store, id, workflowEncodedStates(states))
				_, _, _, version := readWorkflowRun(t, store, id)
				states["later"] = workflowNodeState{Status: transition, Attempt: 1, Error: strings.Repeat("e", 1024)}
				_, err := store.transact(context.Background(), func(tx *transaction) Object {
					settleWorkflowRunStates(tx, id, version, "running", "", states)
					return Object{}
				})
				if err != nil {
					t.Fatal(err)
				}
				status, detail, raw, _ := readWorkflowRun(t, store, id)
				got := workflowStatesFromJSON(raw)
				if transition == "succeeded" {
					if status != "failed" || detail != "workflow_state_size_exceeded" || got["later"].Status != "running" {
						t.Fatalf("historical %s unlocked ordinary write: %s %s", historical, status, detail)
					}
				} else if status != "running" || got["later"].Status != transition || got["it"].Status != "succeeded" || got["later"].Error != strings.Repeat("e", 1024) {
					t.Fatalf("new %s lost reserve or changed completed container: %s %s", transition, status, detail)
				}
			})
		}
	}
}

func TestWorkflowReservedBodyFailureSurvivesRescheduling(t *testing.T) {
	for _, strategy := range []string{"fail", "continue", "loop"} {
		t.Run(strategy, func(t *testing.T) {
			store := dispatcherDB(t)
			graph := bodyAgentIterationGraph()
			for _, raw := range graph["nodes"].([]any) {
				node, _ := asWorkflowObject(raw)
				if node.S("id") == "it" {
					node.O("data").O("iterationConfig")["errorStrategy"] = strategy
				}
			}
			containerID := "it"
			if strategy == "loop" {
				graph, containerID = bodyAgentLoopGraph(), "l"
			}
			id, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, graph)
			if _, err := store.Pool.Exec(`UPDATE workflow_runs SET input=$2 WHERE id=$1`, id, jsonText(Object{"items": []any{"a"}})); err != nil {
				t.Fatal(err)
			}
			advance := func() {
				t.Helper()
				if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			advance()
			ws := settleSessionWorkspace(t, store, id)
			service, spec := startSessionNode(t, store, id, ws, "agg")
			execution := dispatchSession(t, store, id, "agg", service, spec)
			_, _, raw, _ := readWorkflowRun(t, store, id)
			states := workflowStatesFromJSON(raw)
			states["padding"] = workflowNodeState{Status: "succeeded", Output: Object{"value": ""}}
			n := workflowStateLimit - workflowFailureReserve - 40 - len(jsonText(workflowEncodedStates(states)))
			states["padding"] = workflowNodeState{Status: "succeeded", Output: Object{"value": strings.Repeat("x", n)}}
			if _, err := store.Pool.Exec(`UPDATE workflow_runs SET node_states=$2, version=version+1 WHERE id=$1`, id, jsonText(workflowEncodedStates(states))); err != nil {
				t.Fatal(err)
			}
			reportSessionEnd(t, store, id, execution, service, "interrupted")
			advance()
			status, detail, raw, version := readWorkflowRun(t, store, id)
			want := "failed"
			if strategy == "continue" {
				want = "succeeded"
			}
			if status != want || (strategy != "continue" && detail != "interrupted") {
				t.Fatalf("reserved failure changed at next schedule: status=%s error=%s", status, detail)
			}
			if size := len(jsonText(raw)); size <= workflowStateLimit-workflowFailureReserve || size > workflowStateLimit {
				t.Fatalf("failure did not use bounded reserve: %d", size)
			}
			container := workflowStatesFromJSON(raw)[containerID]
			if strategy == "loop" {
				if container.Loop == nil || len(container.Loop.Completed) != 1 {
					t.Fatal("loop failure archive lost")
				}
			} else if it := container.Iteration; it.Failed != 1 || len(it.Ledger) != 1 || len(it.Completed) != 1 {
				t.Fatal("iteration failure ledger or archive lost")
			}
			advance()
			_, _, after, afterVersion := readWorkflowRun(t, store, id)
			if afterVersion != version || jsonText(after) != jsonText(raw) {
				t.Fatal("terminal replay changed committed failure")
			}
		})
	}
}

func TestWorkflowReservedStateReplayDoesNotPermitGrowth(t *testing.T) {
	for _, scenario := range []string{"unchanged", "growth", "hard-limit"} {
		t.Run(scenario, func(t *testing.T) {
			store := dispatcherDB(t)
			id := seedWorkflowRunScene(t, store, iterationRunGraph("continue", 10, []any{"it", "item"}))
			states := map[string]workflowNodeState{
				"it": {Status: "succeeded"}, "agg": {Status: "failed", Attempt: 1},
				"padding": {Status: "succeeded", Output: Object{"value": ""}},
			}
			size := workflowStateLimit - workflowFailureReserve + 1024
			if scenario == "hard-limit" {
				size = workflowStateLimit + 1
			}
			padding := strings.Repeat("x", size-len(jsonText(workflowEncodedStates(states))))
			states["padding"] = workflowNodeState{Status: "succeeded", Output: Object{"value": padding}}
			commitWorkflowRunMidpoint(t, store, id, workflowEncodedStates(states))
			_, _, _, version := readWorkflowRun(t, store, id)
			if scenario == "growth" {
				states["padding"] = workflowNodeState{Status: "succeeded", Output: Object{"value": padding + "x"}}
			}
			_, err := store.transact(context.Background(), func(tx *transaction) Object {
				settleWorkflowRunStates(tx, id, version, "running", "", states)
				return Object{}
			})
			if err != nil {
				t.Fatal(err)
			}
			status, detail, _, _ := readWorkflowRun(t, store, id)
			if scenario == "unchanged" {
				if status != "running" {
					t.Fatalf("committed failure rejected: %s %s", status, detail)
				}
			} else if status != "failed" || detail != "workflow_state_size_exceeded" {
				t.Fatalf("%s bypassed budget: %s %s", scenario, status, detail)
			}
		})
	}
}

func bodyAgentLoopGraph() Object {
	agent := wfNode("agg", "agent", "work")
	agent.O("data")["containerId"] = "l"
	agent.O("data")["agentConfig"] = Object{"prompt": "work"}
	return loopSceneGraph(Object{
		"maxIterations": 2, "variables": []any{}, "outputs": []any{},
		"until": Object{"logic": "and", "conditions": []any{}},
	}, []Object{loopBodyStart("body-start"), agent}, [][3]string{{"body-start", "agg", ""}})
}
