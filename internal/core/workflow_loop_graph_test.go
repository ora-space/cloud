package core

import (
	"reflect"
	"testing"
)

// The loop container is the first graph scope Cloud publishes and executes. These tests are the
// publish gate's side of the while-loop decision: a well-formed loop passes, and every structural or
// configuration fault the scheduler would otherwise have to guess at is refused with a code, a node
// and a field.

// loopTestGraph builds the smallest executable loop: an outer start → loop → output, and a body
// start → agent inside the loop. The loop's config is the argument so a test can replace or damage it.
func loopTestGraph(loopConfig Object) Object {
	loopData := Object{"kind": "loop", "title": "循环 1"}
	if loopConfig != nil {
		loopData["loopConfig"] = loopConfig
	}
	return Object{
		"nodes": []any{
			Object{"id": "start-1", "type": "workflow", "data": Object{"kind": "start", "title": "开始 1", "inputVariables": []any{}}},
			Object{"id": "loop-1", "type": "workflow", "data": loopData},
			Object{"id": "entry", "type": "workflow", "data": Object{"kind": "start", "title": "入口", "containerId": "loop-1", "inputVariables": []any{}}},
			Object{"id": "worker", "type": "workflow", "data": Object{"kind": "agent", "title": "写手", "containerId": "loop-1", "agentConfig": Object{"schemaVersion": 3, "prompt": "write"}}},
			Object{"id": "out", "type": "workflow", "data": Object{"kind": "output", "title": "输出 1"}},
		},
		"edges": []any{
			Object{"source": "start-1", "target": "loop-1"},
			Object{"source": "entry", "target": "worker"},
			Object{"source": "loop-1", "target": "out"},
		},
	}
}

// validLoopConfig is the configuration the scheduler executes: one carried string, an until that
// reads the body agent, and one published output.
func validLoopConfig() Object {
	return Object{
		"maxIterations": 3,
		"variables": []any{
			Object{
				"name":      "draft",
				"valueType": "string",
				"initial":   Object{"kind": "constant", "value": ""},
				"feedback":  []any{"worker", "output"},
			},
		},
		"until": Object{
			"logic": "and",
			"conditions": []any{
				Object{"variableSelector": []any{"worker", "output"}, "operator": "equals", "value": "done"},
			},
		},
		"outputs": []any{
			Object{"name": "result", "variableSelector": []any{"worker", "output"}},
		},
	}
}

// loopProblemStrings projects problems as stable `code@node#field` strings so a case can state the
// complete expected list without depending on the problem struct's unused fields.
func loopProblemStrings(t *testing.T, graph Object) []string {
	t.Helper()
	out := []string{}
	for _, problem := range validateWorkflowGraph(decodeGraph(t, graph)) {
		entry := problem.Code
		if problem.NodeID != "" {
			entry += "@" + problem.NodeID
		}
		if problem.Field != "" {
			entry += "#" + problem.Field
		}
		out = append(out, entry)
	}
	return out
}

// TestWorkflowPublishAcceptsAWellFormedLoop is the positive control: without it every refusal below
// could pass on a graph that never validates.
func TestWorkflowPublishAcceptsAWellFormedLoop(t *testing.T) {
	if got := loopProblemStrings(t, loopTestGraph(validLoopConfig())); len(got) != 0 {
		t.Fatalf("a well-formed loop must publish, got problems %v", got)
	}
}

// TestWorkflowPublishAcceptsTheEditorStarterGroup pins the contract between the palette and the gate:
// the exact shape `createWorkflowLoopGroup` emits — including the default agent contract the editor
// starts from — must publish without a single change. A drift here would make every Loop added from
// the palette unpublishable while the editor still looked fine.
func TestWorkflowPublishAcceptsTheEditorStarterGroup(t *testing.T) {
	agent := Object{
		"id": "loop-1-agent",
		"data": Object{
			"kind":        "agent",
			"title":       "循环 Agent 1",
			"containerId": "loop-1",
			"agentConfig": Object{
				"schemaVersion": 3,
				"executor":      Object{"agentCli": "claude-code", "modelId": "claude-sonnet-4-5"},
				"roleId":        "member",
				"skills":        []any{},
				"mcps":          []any{},
				"prompt":        "改进当前值并输出新值：{{#loop-1.value#}}",
			},
		},
	}
	graph := Object{
		"nodes": []any{
			Object{"id": "start-1", "data": Object{"kind": "start", "title": "开始", "inputVariables": []any{}}},
			Object{"id": "loop-1", "data": Object{"kind": "loop", "title": "循环 1", "loopConfig": Object{
				"maxIterations": 3,
				"variables": []any{Object{
					"name": "value", "valueType": "string",
					"initial":  Object{"kind": "constant", "value": ""},
					"feedback": []any{"loop-1-agent", "output"},
				}},
				"until": Object{"logic": "and", "conditions": []any{
					Object{"variableSelector": []any{"loop-1-agent", "output"}, "operator": "not_empty"},
				}},
				"outputs": []any{Object{"name": "result", "variableSelector": []any{"loop-1-agent", "output"}}},
			}}},
			Object{"id": "loop-1-start", "data": Object{"kind": "start", "title": "轮次开始", "containerId": "loop-1", "inputVariables": []any{}}},
			agent,
			Object{"id": "output-1", "data": Object{"kind": "output", "title": "输出"}},
		},
		"edges": []any{
			Object{"source": "start-1", "target": "loop-1"},
			Object{"source": "loop-1", "target": "output-1"},
			Object{"source": "loop-1-start", "target": "loop-1-agent"},
		},
	}
	if got := loopProblemStrings(t, graph); len(got) != 0 {
		t.Fatalf("the palette's starter group must publish, got problems %v", got)
	}
}

// TestWorkflowPublishRefusesInvalidLoopContainers is the structural matrix: membership, body start,
// nested composites, body output, cross-scope edges, loopExit terminality and reachability.
func TestWorkflowPublishRefusesInvalidLoopContainers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(graph Object)
		want   []string
	}{
		{
			name: "body without a start",
			mutate: func(graph Object) {
				graph["nodes"] = removeNode(graph["nodes"], "entry")
				graph["edges"] = removeEdge(graph["edges"], "entry", "worker")
			},
			want: []string{"loop_body_start_count@loop-1"},
		},
		{
			name: "body with two starts",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "entry-2", "data": Object{"kind": "start", "containerId": "loop-1"}})
			},
			want: []string{"loop_body_start_count@loop-1"},
		},
		{
			name: "nested loop inside a body",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "loop-2", "data": Object{"kind": "loop", "containerId": "loop-1"}})
			},
			want: []string{"loop_nested_composite_unsupported@loop-2"},
		},
		{
			name: "output inside a body",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "out-2", "data": Object{"kind": "output", "containerId": "loop-1"}})
				graph["edges"] = appendEdge(graph["edges"], "worker", "out-2")
			},
			want: []string{"loop_body_output_unsupported@out-2"},
		},
		{
			name: "edge crossing the container boundary",
			mutate: func(graph Object) {
				graph["edges"] = appendEdge(graph["edges"], "loop-1", "worker")
			},
			want: []string{"loop_cross_container_edge@loop-1"},
		},
		{
			name: "loopExit outside a body",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "exit-1", "data": Object{"kind": "loopExit"}})
			},
			want: []string{"loop_exit_outside_body@exit-1"},
		},
		{
			name: "loopExit with an outgoing edge",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "exit-1", "data": Object{"kind": "loopExit", "containerId": "loop-1"}})
				graph["edges"] = appendEdge(graph["edges"], "entry", "exit-1")
				graph["edges"] = appendEdge(graph["edges"], "exit-1", "worker")
			},
			want: []string{"loop_exit_has_outgoing_edge@exit-1"},
		},
		{
			name: "unreachable body member",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "stray", "data": Object{"kind": "agent", "containerId": "loop-1", "agentConfig": Object{"schemaVersion": 3, "prompt": "x"}}})
			},
			want: []string{"loop_body_unreachable@stray"},
		},
		{
			name: "member naming a container that is not a loop",
			mutate: func(graph Object) {
				graph["nodes"] = appendNode(graph["nodes"], Object{"id": "lost", "data": Object{"kind": "agent", "containerId": "out", "agentConfig": Object{"schemaVersion": 3, "prompt": "x"}}})
			},
			want: []string{"loop_member_container_mismatch@lost"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := loopTestGraph(validLoopConfig())
			tc.mutate(graph)
			if got := loopProblemStrings(t, graph); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkflowPublishRefusesInvalidLoopConfig is the configuration matrix: the round budget, the
// exhaustion policy, carried names and types, the initial value, feedback, until and outputs.
func TestWorkflowPublishRefusesInvalidLoopConfig(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(config Object)
		want   []string
	}{
		{
			name:   "missing loopConfig",
			mutate: nil,
			want:   []string{"loop_config_missing@loop-1#loopConfig"},
		},
		{
			name:   "non-integer maxIterations",
			mutate: func(config Object) { config["maxIterations"] = 2.5 },
			want:   []string{"loop_max_iterations_invalid@loop-1#loopConfig.maxIterations"},
		},
		{
			name:   "maxIterations above the ceiling",
			mutate: func(config Object) { config["maxIterations"] = 101 },
			want:   []string{"loop_max_iterations_invalid@loop-1#loopConfig.maxIterations"},
		},
		{
			name:   "unknown exhaustion policy",
			mutate: func(config Object) { config["maxIterationsBehavior"] = "ignore" },
			want:   []string{"loop_max_iterations_behavior_invalid@loop-1#loopConfig.maxIterationsBehavior"},
		},
		{
			name: "variable name with a dot",
			mutate: func(config Object) {
				config["variables"].([]any)[0].(Object)["name"] = "draft.value"
			},
			want: []string{"loop_variable_invalid@loop-1#loopConfig.variables.name"},
		},
		{
			name: "duplicate variable names",
			mutate: func(config Object) {
				config["variables"] = append(config["variables"].([]any), Object{"name": "draft", "valueType": "string", "initial": Object{"kind": "constant", "value": ""}, "feedback": []any{"worker", "output"}})
			},
			want: []string{"loop_variable_invalid@loop-1#loopConfig.variables.name"},
		},
		{
			name: "unsupported variable type",
			mutate: func(config Object) {
				config["variables"].([]any)[0].(Object)["valueType"] = "date"
			},
			want: []string{"loop_variable_invalid@loop-1#loopConfig.variables.valueType"},
		},
		{
			name: "constant that does not match its type",
			mutate: func(config Object) {
				config["variables"].([]any)[0].(Object)["initial"] = Object{"kind": "constant", "value": 3}
			},
			want: []string{"loop_initial_invalid@loop-1#loopConfig.variables.initial.value"},
		},
		{
			name: "initial selector on an undeclared root",
			mutate: func(config Object) {
				config["variables"].([]any)[0].(Object)["initial"] = Object{"kind": "variable", "selector": []any{"missing", "value"}}
			},
			want: []string{"loop_selector_undeclared@loop-1#loopConfig.variables.initial.selector"},
		},
		{
			name: "feedback on an undeclared root",
			mutate: func(config Object) {
				config["variables"].([]any)[0].(Object)["feedback"] = []any{"missing", "value"}
			},
			want: []string{"loop_selector_undeclared@loop-1#loopConfig.variables.feedback"},
		},
		{
			name: "until logic that is neither and nor or",
			mutate: func(config Object) {
				config["until"].(Object)["logic"] = "xor"
			},
			want: []string{"loop_until_invalid@loop-1#loopConfig.until.logic"},
		},
		{
			name: "until comparison with an unknown operator",
			mutate: func(config Object) {
				config["until"].(Object)["conditions"].([]any)[0].(Object)["operator"] = "bogus"
			},
			want: []string{"condition_operator_unsupported@loop-1#cases.conditions.operator"},
		},
		{
			name: "duplicate output names",
			mutate: func(config Object) {
				config["outputs"] = append(config["outputs"].([]any), Object{"name": "result", "variableSelector": []any{"worker", "output"}})
			},
			want: []string{"loop_output_invalid@loop-1#loopConfig.outputs.name"},
		},
		{
			name: "output selector on the loop's own output",
			mutate: func(config Object) {
				config["outputs"].([]any)[0].(Object)["variableSelector"] = []any{"loop-1", "result"}
			},
			want: []string{"loop_selector_undeclared@loop-1#loopConfig.outputs.variableSelector"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var config Object
			if tc.mutate != nil {
				config = validLoopConfig()
				tc.mutate(config)
			}
			if got := loopProblemStrings(t, loopTestGraph(config)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkflowPublishResolvesLoopRootsPerScope is the visibility rule: a carried variable is readable
// inside the body and refused outside it, and a published output is the reverse.
func TestWorkflowPublishResolvesLoopRootsPerScope(t *testing.T) {
	t.Run("body reads a carried value", func(t *testing.T) {
		graph := loopTestGraph(validLoopConfig())
		graph["nodes"] = appendNode(graph["nodes"], Object{
			"id":   "check",
			"data": Object{"kind": "condition", "containerId": "loop-1", "cases": []any{Object{"id": "c1", "conditions": []any{Object{"variableSelector": []any{"loop-1", "draft"}, "operator": "not_empty"}}}}},
		})
		graph["edges"] = appendEdge(graph["edges"], "worker", "check")
		if got := loopProblemStrings(t, graph); len(got) != 0 {
			t.Fatalf("a body node may read its loop's carried variable, got %v", got)
		}
	})
	t.Run("outer graph cannot read a carried value", func(t *testing.T) {
		graph := loopTestGraph(validLoopConfig())
		graph["nodes"] = appendNode(graph["nodes"], Object{
			"id":   "check",
			"data": Object{"kind": "condition", "cases": []any{Object{"id": "c1", "conditions": []any{Object{"variableSelector": []any{"loop-1", "draft"}, "operator": "not_empty"}}}}},
		})
		graph["edges"] = appendEdge(graph["edges"], "loop-1", "check")
		want := []string{"condition_selector_undeclared@check#cases.conditions.variableSelector"}
		if got := loopProblemStrings(t, graph); !reflect.DeepEqual(got, want) {
			t.Fatalf("problems = %v, want %v", got, want)
		}
	})
	t.Run("outer graph reads the loop's published output", func(t *testing.T) {
		graph := loopTestGraph(validLoopConfig())
		graph["nodes"] = appendNode(graph["nodes"], Object{
			"id":   "check",
			"data": Object{"kind": "condition", "cases": []any{Object{"id": "c1", "conditions": []any{Object{"variableSelector": []any{"loop-1", "result"}, "operator": "not_empty"}}}}},
		})
		graph["edges"] = appendEdge(graph["edges"], "loop-1", "check")
		if got := loopProblemStrings(t, graph); len(got) != 0 {
			t.Fatalf("the outer graph may read a loop's published output, got %v", got)
		}
	})
}

// TestWorkflowStartNodeIgnoresABodyStart keeps the launcher fields reading the outer graph: a body
// start appears first in the document here, which is exactly the order that would have been read as
// the run's entry point before containers existed.
func TestWorkflowStartNodeIgnoresABodyStart(t *testing.T) {
	graph := Object{
		"nodes": []any{
			Object{"id": "entry", "data": Object{"kind": "start", "containerId": "loop-1", "input": "body prompt", "inputVariables": []any{Object{"name": "inner"}}}},
			Object{"id": "start-1", "data": Object{"kind": "start", "input": "run prompt", "inputVariables": []any{Object{"name": "outer"}}}},
		},
	}
	decoded := decodeGraph(t, graph)
	if got := workflowStartPrompt(decoded); got != "run prompt" {
		t.Fatalf("start prompt = %q, want the outer start's prompt", got)
	}
	variables := workflowStartVariables(decoded)
	if len(variables) != 1 || variables[0].S("name") != "outer" {
		t.Fatalf("start variables = %v, want only the outer declaration", variables)
	}
	if got := readWorkflowGraph(decoded).Start; got != "start-1" {
		t.Fatalf("run entry point = %q, want the outer start-1", got)
	}
}

// appendNode and appendEdge keep the structural cases readable without rebuilding the graph.
func appendNode(nodes any, node Object) []any {
	list, _ := nodes.([]any)
	return append(list, node)
}

// appendEdge appends one edge, preserving the document's list type.
func appendEdge(edges any, source, target string) []any {
	list, _ := edges.([]any)
	return append(list, Object{"source": source, "target": target})
}

// removeNode drops one node by id from a document list.
func removeNode(nodes any, id string) []any {
	list, _ := nodes.([]any)
	out := make([]any, 0, len(list))
	for _, item := range list {
		node, ok := asWorkflowObject(item)
		if ok && node.S("id") == id {
			continue
		}
		out = append(out, item)
	}
	return out
}

// removeEdge drops the edge with the given endpoints.
func removeEdge(edges any, source, target string) []any {
	list, _ := edges.([]any)
	out := make([]any, 0, len(list))
	for _, item := range list {
		edge, ok := asWorkflowObject(item)
		if ok && edge.S("source") == source && edge.S("target") == target {
			continue
		}
		out = append(out, item)
	}
	return out
}

// TestWorkflowContainerIDReadsBothMembershipKeys pins the iteration ADR's D1 normalization: an
// explicit body container wins, React Flow's parentId is the fallback, and a loose node has none.
func TestWorkflowContainerIDReadsBothMembershipKeys(t *testing.T) {
	explicit := Object{"id": "member", "parentId": "iteration-1", "data": Object{"containerId": "loop-1"}}
	if got := workflowContainerID(explicit); got != "loop-1" {
		t.Fatalf("explicit containerId = %q; want loop-1", got)
	}
	inherited := Object{"id": "member", "parentId": "iteration-1", "data": Object{}}
	if got := workflowContainerID(inherited); got != "iteration-1" {
		t.Fatalf("parentId fallback = %q; want iteration-1", got)
	}
	loose := Object{"id": "member", "data": Object{}}
	if got := workflowContainerID(loose); got != "" {
		t.Fatalf("loose node container = %q; want empty", got)
	}
}
