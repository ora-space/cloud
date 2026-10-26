package core

import (
	"context"
	"reflect"
	"testing"
)

// Historical snapshots have no execution-semantics version. Exercise both a new run from such a
// snapshot and a run recovered after its predecessor committed, without passing a publish gate.
func TestWorkflowHistoricalOutputStrictBindings(t *testing.T) {
	for _, test := range []struct {
		name     string
		bindings any
		want     Object
		reason   string
	}{
		{name: "absent bindings", want: Object{}},
		{name: "empty bindings", bindings: []any{}, want: Object{}},
		{name: "assigned", bindings: []any{Object{"name": "answer", "variableSelector": []any{"s", "input"}}}, want: Object{"answer": "committed answer"}},
		{name: "unset", bindings: []any{
			Object{"name": "answer", "variableSelector": []any{"s", "input"}},
			Object{"name": "missing", "variableSelector": []any{"s", "missing"}},
		}, reason: "output_value_unset"},
		{name: "invalid path", bindings: []any{Object{"name": "answer", "variableSelector": []any{"s", "input", "child"}}}, reason: "output_invalid_path"},
	} {
		for _, recovered := range []bool{false, true} {
			mode := "new run"
			if recovered {
				mode = "recovered run"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				store := dispatcherDB(t)
				output := wfNode("out", "output", "")
				if test.bindings != nil {
					output["data"].(Object)["outputs"] = test.bindings
				}
				graph := wfGraph([]Object{wfNode("s", "start", ""), output}, [][3]string{{"s", "out", ""}})
				runID := seedWorkflowRunSceneWithInput(t, store, graph, Object{"input": "committed answer"})
				prior := Object{"s": Object{
					"status": "succeeded", "displayName": "", "output": Object{"input": "committed answer"},
					"startedAt": "2026-01-01T00:00:00Z", "finishedAt": "2026-01-01T00:00:01Z",
				}}
				if recovered {
					// A different input proves recovery consumes the saved predecessor, not a replay.
					if _, err := store.Pool.Exec(`UPDATE workflow_runs SET status='running', started_at=now(),
						node_states=$2::jsonb, input=$3::jsonb WHERE id=$1`, runID, jsonText(prior), jsonText(Object{"input": "must not replay"})); err != nil {
						t.Fatal(err)
					}
				}
				var frozenBefore string
				const frozenQuery = `SELECT s.graph::text FROM workflow_snapshots s JOIN workflow_runs r ON r.snapshot_id=s.id WHERE r.id=$1`
				if err := store.Pool.QueryRow(frozenQuery, runID).Scan(&frozenBefore); err != nil {
					t.Fatal(err)
				}
				if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				status, runErr, states, version := readWorkflowRun(t, store, runID)
				wantStatus, wantOutput := "succeeded", test.want
				if test.reason != "" {
					wantStatus, wantOutput = "failed", Object{"reason": test.reason}
				}
				if status != wantStatus || states.O("out").S("status") != wantStatus ||
					!reflect.DeepEqual(states.O("out").O("output"), wantOutput) {
					t.Fatalf("run = %s/%s; output = %#v; want %s/%#v", status, runErr, states.O("out"), wantStatus, wantOutput)
				}
				if (test.reason == "") != (runErr == "") {
					t.Fatalf("run error = %q for reason %q", runErr, test.reason)
				}
				if recovered && jsonText(states.O("s")) != jsonText(prior.O("s")) {
					t.Fatalf("recovery rewrote committed predecessor: %#v", states.O("s"))
				}
				var frozenAfter string
				if err := store.Pool.QueryRow(frozenQuery, runID).Scan(&frozenAfter); err != nil {
					t.Fatal(err)
				}
				if frozenAfter != frozenBefore {
					t.Fatal("execution rewrote the historical snapshot")
				}
				if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				againStatus, againErr, againStates, againVersion := readWorkflowRun(t, store, runID)
				if againStatus != status || againErr != runErr || againVersion != version || !reflect.DeepEqual(againStates, states) {
					t.Fatal("repeated scheduling changed a committed terminal result")
				}
			})
		}
	}
}
