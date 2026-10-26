package core

import (
	"context"
	"reflect"
	"testing"
)

func TestWorkflowAggregatorRetainsAssignedEmptyValues(t *testing.T) {
	node := workflowNodeView{ID: "a", Kind: "aggregator", Data: Object{"aggregatorConfig": Object{"variables": []any{[]any{"s", "value"}}}}}
	for _, value := range []any{false, 0, "", []any{}, Object{}} {
		state := executeWorkflowNode(node, nil, map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"value": value}}}, nil)
		if state.Status != "succeeded" || !reflect.DeepEqual(state.Output["output"], value) {
			t.Fatalf("assigned empty value lost: %#v -> %#v", value, state)
		}
	}
	node.Data = Object{}
	if got := executeWorkflowNode(node, nil, nil, nil); got.Output.S("reason") != "aggregator_invalid_config" {
		t.Fatalf("invalid configuration: %+v", got)
	}
}

func TestWorkflowLoopLastRoundAndExitOutputs(t *testing.T) {
	for _, scenario := range []string{"succeed exhaustion", "until at ceiling", "exit missing output", "body aggregator"} {
		t.Run(scenario, func(t *testing.T) {
			store := dispatcherDB(t)
			config := Object{"maxIterations": 2, "variables": []any{loopVar("a", "string", "a", "l", "b"), loopVar("b", "string", "b", "l", "a")}, "until": loopUntilEquals([]any{"l", "a"}, "never"), "outputs": []any{Object{"name": "result", "variableSelector": []any{"l", "a"}}}}
			members, edges := []Object{loopBodyStart("entry")}, [][3]string{}
			want, result := "succeeded", "b"
			switch scenario {
			case "succeed exhaustion":
				config["maxIterationsBehavior"] = "succeed"
			case "until at ceiling":
				config["until"] = loopUntilEquals([]any{"l", "a"}, "b")
			case "exit missing output":
				exit := wfNode("exit", "loopExit", "exit")
				exit.O("data")["containerId"] = "l"
				members = append(members, exit)
				edges = append(edges, [3]string{"entry", "exit", ""})
				config["outputs"] = []any{Object{"name": "result", "variableSelector": []any{"absent", "output"}}}
				want = "failed"
			case "body aggregator":
				agg := wfNode("agg", "aggregator", "agg")
				agg.O("data")["containerId"] = "l"
				agg.O("data")["aggregatorConfig"] = Object{"variables": []any{[]any{"l", "a"}}}
				members = append(members, agg)
				edges = append(edges, [3]string{"entry", "agg", ""})
				config["until"] = loopUntilEquals([]any{"agg", "output"}, "a")
				config["outputs"] = []any{Object{"name": "result", "variableSelector": []any{"agg", "output"}}}
				result = "a"
			}
			id := seedWorkflowRunScene(t, store, loopSceneGraph(config, members, edges))
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			status, detail, raw, _ := readWorkflowRun(t, store, id)
			loop := workflowStatesFromJSON(raw)["l"]
			if status != want {
				t.Fatalf("%s: %s %s", scenario, status, detail)
			}
			if want == "failed" {
				if detail != "loop_exit_output_unset" {
					t.Fatal(detail)
				}
				return
			}
			if loop.Output.S("result") != result {
				t.Fatalf("last round output: %v", loop.Output)
			}
		})
	}
}

func TestWorkflowIterationContinueRejectsInvalidSourceBeforeFirstRound(t *testing.T) {
	for _, source := range []any{"not an array", []any{1, 2}} {
		store := dispatcherDB(t)
		id := seedWorkflowRunSceneWithInput(t, store, iterationRunGraph("continue", 1, []any{"it", "item"}), Object{"items": source})
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		status, reason, raw, _ := readWorkflowRun(t, store, id)
		if status != "failed" || (reason != "iteration_iterator_not_a_list" && reason != "iteration_over_limit") {
			t.Fatalf("invalid source accepted: %s %s", status, reason)
		}
		if _, exists := raw["agg"]; exists {
			t.Fatal("invalid source dispatched body")
		}
	}
}
