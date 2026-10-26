package core

import (
	"reflect"
	"testing"
)

func TestWorkflowOutputResolvesNamedBindingsAtomically(t *testing.T) {
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{
		"input": "hello", "zero": float64(0), "null": nil, "false": false,
		"empty": "", "object": Object{"count": float64(2)},
	}}}
	bindings := []any{}
	for _, root := range []string{"input", "zero", "null", "false", "empty"} {
		bindings = append(bindings, Object{"name": root, "variableSelector": []any{"s", root}})
	}
	bindings = append(bindings, Object{"name": "nested", "variableSelector": []any{"s", "object", "count"}})
	node := workflowNodeView{ID: "out", Kind: "output", Data: Object{"outputs": bindings}}
	got := executeWorkflowNode(node, nil, states, nil)
	want := Object{"input": "hello", "zero": float64(0), "null": nil, "false": false, "empty": "", "nested": float64(2)}
	if got.Status != "succeeded" || !reflect.DeepEqual(got.Output, want) {
		t.Fatalf("output = %#v; want succeeded with %#v", got, want)
	}
	bindings = append(bindings, Object{"name": "missing", "variableSelector": []any{"unselected", "output"}})
	node.Data["outputs"] = bindings
	got = executeWorkflowNode(node, nil, states, nil)
	if got.Status != "failed" || !reflect.DeepEqual(got.Output, Object{"reason": "output_value_unset"}) {
		t.Fatalf("missing binding leaked a partial successful output: %#v", got)
	}
}

func TestWorkflowOutputRejectsMalformedBindingsAndInvalidPaths(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		bindings     any
	}{
		{"wrong list", "output_invalid_config", "not a list"},
		{"empty name", "output_invalid_config", []any{Object{"name": "", "variableSelector": []any{"s", "input"}}}},
		{"invalid path", "output_invalid_path", []any{Object{"name": "value", "variableSelector": []any{"s", "input", "missing"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := executeWorkflowNode(workflowNodeView{ID: "out", Kind: "output", Data: Object{"outputs": test.bindings}}, nil,
				map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"input": "text"}}}, nil)
			if got.Status != "failed" || got.Output.S("reason") != test.reason {
				t.Fatalf("state = %#v; want failed/%s", got, test.reason)
			}
		})
	}
	got := executeWorkflowNode(workflowNodeView{ID: "out", Kind: "output", Data: Object{}}, nil, nil, nil)
	if got.Status != "succeeded" || !reflect.DeepEqual(got.Output, Object{}) {
		t.Fatalf("legacy output without bindings = %#v", got)
	}
}

func TestWorkflowPublishValidatesOutputBindings(t *testing.T) {
	valid := Object{"name": "summary", "variableSelector": []any{"s", "input"}}
	for _, test := range []struct {
		name, code string
		bindings   any
	}{
		{"valid", "", []any{valid}},
		{"empty", "", []any{}},
		{"wrong list", "output_bindings_not_a_list", true},
		{"wrong row", "output_binding_invalid", []any{"bad"}},
		{"duplicate", "output_name_duplicate", []any{valid, valid}},
		{"invalid selector", "output_selector_unsupported", []any{Object{"name": "result", "variableSelector": []any{"s"}}}},
		{"undeclared", "output_selector_undeclared", []any{Object{"name": "result", "variableSelector": []any{"unknown", "output"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := wfNode("out", "output", "")
			node["data"].(Object)["outputs"] = test.bindings
			graph := wfGraph([]Object{wfNode("s", "start", ""), node}, [][3]string{{"s", "out", ""}})
			problems := validateWorkflowGraph(graph)
			if test.code == "" {
				if len(problems) != 0 {
					t.Fatalf("valid bindings rejected: %#v", problems)
				}
			} else if len(problems) != 1 || problems[0].Code != test.code || problems[0].NodeID != "out" {
				t.Fatalf("problems = %#v; want one %s at out", problems, test.code)
			}
		})
	}
}
