package core

import (
	"context"
	"reflect"
	"testing"
)

// Both branches join the same terminal through distinct handles. The scheduler must activate only
// the selected edge, resolve the binding and persist the same named result the run view returns.
func TestWorkflowAdvanceConditionBranchesJoinNamedOutput(t *testing.T) {
	for _, test := range []struct{ kickoff, branch string }{{"yes", "approved"}, {"no", "else"}} {
		t.Run(test.branch, func(t *testing.T) {
			store := dispatcherDB(t)
			condition := wfNode("c", "condition", "route")
			condition["data"].(Object)["cases"] = []any{Object{
				"id": "approved", "conditions": []any{Object{
					"variableSelector": []any{"s", "input"}, "operator": "equals", "value": "yes",
				}},
			}}
			output := wfNode("out", "output", "result")
			output["data"].(Object)["outputs"] = []any{Object{"name": "answer", "variableSelector": []any{"s", "input"}}}
			graph := wfGraph([]Object{wfNode("s", "start", ""), condition, output},
				[][3]string{{"s", "c", ""}, {"c", "out", "approved"}, {"c", "out", "else"}})
			runID := seedWorkflowRunSceneWithInput(t, store, graph, Object{"input": test.kickoff})
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			status, runErr, states, _ := readWorkflowRun(t, store, runID)
			if status != "succeeded" || runErr != "" || states.O("c").O("output").S("selectedBranch") != test.branch {
				t.Fatalf("run = %s/%s; states = %#v", status, runErr, states)
			}
			if got := states.O("out").O("output"); !reflect.DeepEqual(got, Object{"answer": test.kickoff}) {
				t.Fatalf("named output = %#v", got)
			}
			if workflowIssueOutputSummary(graph, states, "") == "" {
				t.Fatal("named output was lost from the issue result projection")
			}
		})
	}
}
