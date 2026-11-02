package workflow

import (
	"encoding/json"
	"testing"
)

func TestLoopFeedbackUsesOneSnapshotBeforeUntil(t *testing.T) {
	n := Node{data: object{"loopConfig": object{
		"maxIterations": json.Number("2"), "variables": []any{
			object{"name": "x", "valueType": "number", "initial": object{"kind": "constant", "value": json.Number("1")}, "feedback": []any{"loop", "y"}},
			object{"name": "y", "valueType": "number", "initial": object{"kind": "constant", "value": json.Number("2")}, "feedback": []any{"loop", "x"}},
		}, "until": object{"logic": "and", "conditions": []any{}}, "outputs": []any{},
	}}}
	cfg, err := n.loopConfig()
	if err != nil {
		t.Fatal(err)
	}
	pool := newPool()
	pool.Declare("loop.x", "number", "loop")
	pool.Declare("loop.y", "number", "loop")
	_ = pool.Set("loop.x", "loop", json.Number("1"))
	_ = pool.Set("loop.y", "loop", json.Number("2"))
	carried, _, reason, err := cfg.complete(1, &pool)
	if err != nil || reason != "" || carried["x"] != json.Number("2") || carried["y"] != json.Number("1") {
		t.Fatalf("%v %q %v", carried, reason, err)
	}
	if pool.Values["loop.x"] != json.Number("1") {
		t.Fatal("feedback mutated its snapshot")
	}
	cfg.until = []conditionCase{{id: "until", logic: "and", rules: []conditionRule{{selector: []string{"loop", "x"}, operator: "exists"}}}}
	delete(pool.Values, "loop.y")
	if _, _, _, err := cfg.complete(1, &pool); err == nil {
		t.Fatal("until skipped invalid feedback")
	}
	if _, err := cfg.outputs(&pool); err != nil {
		t.Fatalf("exit required unused feedback: %v", err)
	}
}
