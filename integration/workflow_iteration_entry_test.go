package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// The editor saves imported Desktop graphs through the same draft API. Publication must keep
// parentId and iteration-entry intact instead of requiring the author to strip the entry edge.
func TestWorkflowIterationEntrySurvivesDraftAndPublication(t *testing.T) {
	f := setup(t)
	var graph core.Object
	must(t, json.Unmarshal([]byte(`{
		"nodes": [
			{"id":"s","data":{"kind":"start","inputVariables":[{"name":"items","valueType":"array"}]}},
			{"id":"it","data":{"kind":"iteration","iterationConfig":{
				"iteratorSelector":["s","items"],"collectSelector":["agg","output"],"maxIterations":10,"errorStrategy":"fail"
			}}},
			{"id":"agg","parentId":"it","data":{"kind":"aggregator","aggregatorConfig":{"variables":[["it","item"]]}}},
			{"id":"o","data":{"kind":"output"}}
		],
		"edges": [
			{"id":"s-it","source":"s","target":"it"},
			{"id":"it-agg","source":"it","sourceHandle":"iteration-entry","target":"agg"},
			{"id":"it-o","source":"it","target":"o"}
		]
	}`), &graph))
	draft := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Iteration entry compatibility", "graph": graph,
	}, "iteration-entry-draft", 200).O("resource")
	path := f.path("/workflows/" + draft.S("id"))
	live := f.call("GET", path, nil, "", 200)
	snapshot := f.call("POST", path+"/publish", core.Object{}, "iteration-entry-publish", 200).O("resource")
	for name, got := range map[string]core.Object{"draft": live.O("graph"), "snapshot": snapshot.O("graph")} {
		for _, key := range []string{"nodes", "edges"} {
			if !reflect.DeepEqual(got[key], graph[key]) {
				t.Fatalf("%s rewrote %s: got %#v, want %#v", name, key, got[key], graph[key])
			}
		}
	}
	var stored core.Object
	var raw []byte
	must(t, f.store.Pool.QueryRow("SELECT graph FROM workflow_snapshots WHERE id=$1", snapshot.S("id")).Scan(&raw))
	must(t, json.Unmarshal(raw, &stored))
	if !reflect.DeepEqual(stored["edges"], graph["edges"]) {
		t.Fatalf("persisted snapshot lost entry edge: %v", stored)
	}
}
