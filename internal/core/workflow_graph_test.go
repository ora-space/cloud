package core

import (
	"reflect"
	"strings"
	"testing"
)

// faultOf runs fn and returns the Fault it refused with. A refusal that is not a Fault would reach a
// client as a 500, so it fails the test rather than being tolerated.
func faultOf(t *testing.T, fn func()) *Fault {
	t.Helper()
	var fault *Fault
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			f, ok := r.(*Fault)
			if !ok {
				t.Fatalf("panic is not a Fault: %v", r)
			}
			fault = f
		}()
		fn()
	}()
	if fault == nil {
		t.Fatal("expected a refusal, got none")
	}
	return fault
}

// noFault fails the test when fn refuses. It is how a case asserts that a legal document is admitted,
// which is the half of a validator that a matrix of violations cannot cover.
func noFault(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected refusal: %v", r)
		}
	}()
	fn()
}

// graphStart, graphAgent, graphEdge and graphSlot build the document shapes the validator reads. They
// are deliberately built by hand rather than decoded: the validator reads a document the API never
// interprets, so it has to be exercised against the shapes a hand-edited or imported document can
// actually have, not only against what the editor writes.
func graphStart(id string) Object {
	return Object{"id": id, "type": "workflow", "data": Object{"kind": "start"}}
}

// graphAgent builds an agent node. An empty slot leaves `executorSlot` off, which is the implicit
// `default`; `legacy` adds the pre-slot `agentConfig.executor` field a desktop-imported graph still
// carries.
func graphAgent(id, slot string, legacy bool) Object {
	config := Object{"schemaVersion": 1, "prompt": "review"}
	if slot != "" {
		config["executorSlot"] = slot
	}
	if legacy {
		config["executor"] = Object{"agentCli": "open_code", "modelId": "vendor/model"}
	}
	return Object{"id": id, "type": "workflow", "data": Object{"kind": "agent", "agentConfig": config}}
}

// graphAgentConfig builds an agent node carrying the given agentConfig keys beside the first phase's
// own, so a case can state the configuration it is about without rebuilding the node around it.
func graphAgentConfig(id string, extra Object) Object {
	config := Object{"schemaVersion": 1, "prompt": "review"}
	for key, value := range extra {
		config[key] = value
	}
	return Object{"id": id, "type": "workflow", "data": Object{"kind": "agent", "agentConfig": config}}
}

func graphEdge(source, target string) Object {
	return Object{"id": source + "->" + target, "source": source, "target": target}
}

func graphSlot(name, description string) Object {
	return Object{"name": name, "description": description}
}

// graphCondition builds a condition node whose cases the test authors directly.
func graphCondition(id string, cases any) Object {
	return Object{"id": id, "type": "workflow", "data": Object{"kind": "condition", "cases": cases}}
}

// graphConditionCase is one branch: its id, its logic and its comparison rules.
func graphConditionCase(id, logic string, rules []any) Object {
	return Object{"id": id, "logic": logic, "conditions": rules}
}

// graphRule is one comparison inside a case.
func graphRule(selector []any, operator string, value any) Object {
	return Object{"variableSelector": selector, "operator": operator, "value": value}
}

// workflowGraph assembles a document. A nil `slots` leaves `executorSlots` off entirely, the way a
// graph authored before slots existed is stored.
func workflowGraph(nodes, edges, slots []any) Object {
	g := Object{"nodes": nodes, "edges": edges}
	if slots != nil {
		g["executorSlots"] = slots
	}
	return g
}

// problemObjects renders the validator's output the way the fault carries it, so a case compares the
// complete wire value rather than a count.
func problemObjects(problems []workflowGraphProblem) []any {
	out := []any{}
	for _, p := range problems {
		out = append(out, p.Object())
	}
	return out
}

// TestValidateWorkflowGraph is the canonical-schema matrix: every rule the publish gate enforces has a
// case that violates it, and the cases that must *not* be refused — the legacy field, an explicit
// `default`, a slot declared for later — are here too, because a validator that refuses a legal
// document is worse than one that misses a rule.
func TestValidateWorkflowGraph(t *testing.T) {
	// specs#98 D1 states the description's bound as a literal 500 characters, so the cases below spell it
	// out instead of deriving it from `maxSlotDescription`: a limit read back from the implementation
	// agrees with the implementation by construction, and would keep passing if the constant drifted away
	// from the decision. The hint's ceiling is Cloud's own — D3 fixes the field's shape, not a length — so
	// that pair stays derived from `maxSlotHintField`, where the constant *is* the decision.
	longDescription := strings.Repeat("x", 501)
	// The limits count characters, so a description written in Chinese is the case that separates a
	// character count from a byte count: 500 of these characters are 1500 bytes, and the byte-counting
	// implementation this replaced refused them from the 167th character on.
	chineseDescription := strings.Repeat("深", 500)
	chineseDescriptionOverLimit := strings.Repeat("深", 501)
	chineseModelID := strings.Repeat("模", maxSlotHintField)

	type graphCase struct {
		name  string
		graph Object
		want  []any
	}
	cases := []graphCase{
		{
			name:  "a start node and an agent node",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "", false)}, []any{graphEdge("s", "a")}, nil),
		},
		{
			name: "a declared and referenced slot",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "strong", false)}, []any{graphEdge("s", "a")},
				[]any{graphSlot("strong", "深度推理")}),
		},
		{
			// An author may be mid-edit or keeping a slot for later; refusing it would block publishing
			// for a mistake with no runtime consequence.
			name: "a declared but unreferenced slot",
			graph: workflowGraph([]any{graphStart("s")}, []any{},
				[]any{graphSlot("strong", "深度推理")}),
		},
		{
			// specs#98 D5: cloud accepts a desktop-imported graph and reads the node as `default`.
			name:  "the pre-slot executor field is accepted",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "", true)}, []any{graphEdge("s", "a")}, nil),
		},
		{
			name:  "an explicit default reference",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", workflowDefaultSlot, false)}, []any{graphEdge("s", "a")}, nil),
		},
		{
			name: "a valid hint",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "strong", false)}, []any{graphEdge("s", "a")},
				[]any{Object{"name": "strong", "description": "深度推理", "hint": Object{"agentCli": "official/ora-space.opencode", "modelId": "vendor/model"}}}),
		},
		{
			name:  "nodes is not a list",
			graph: workflowGraph(nil, []any{}, nil),
			want:  []any{Object{"code": "nodes_not_a_list"}, Object{"code": "start_node_count"}},
		},
		{
			name:  "edges is not a list",
			graph: Object{"nodes": []any{graphStart("s")}, "edges": "no"},
			want:  []any{Object{"code": "edges_not_a_list"}},
		},
		{
			name:  "a node that is not an object",
			graph: workflowGraph([]any{graphStart("s"), "not a node"}, []any{}, nil),
			want:  []any{Object{"code": "node_not_an_object"}},
		},
		{
			name:  "a node with no id",
			graph: workflowGraph([]any{graphStart("s"), Object{"data": Object{"kind": "output"}}}, []any{}, nil),
			want:  []any{Object{"code": "node_id_missing"}},
		},
		{
			name:  "a duplicated node id",
			graph: workflowGraph([]any{graphStart("s"), graphStart("s")}, []any{}, nil),
			want:  []any{Object{"code": "node_id_duplicate", "nodeId": "s"}},
		},
		{
			name:  "a node with no kind",
			graph: workflowGraph([]any{graphStart("s"), Object{"id": "a", "data": Object{}}}, []any{}, nil),
			want:  []any{Object{"code": "node_kind_missing", "nodeId": "a"}},
		},
		{
			// A node kind outside the first-phase executable subset is refused by name rather than
			// silently dropped from the frozen graph.
			name:  "an unsupported node kind",
			graph: workflowGraph([]any{graphStart("s"), Object{"id": "t", "data": Object{"kind": "tool"}}}, []any{}, nil),
			want:  []any{Object{"code": "node_kind_unsupported", "nodeId": "t", "kind": "tool"}},
		},
		{
			// The node is refused for its kind, and its configuration is not also reported: the author
			// has to delete this node, so a field of it is not theirs to fix.
			name: "a node of an unsupported kind carrying an unused configuration field",
			graph: workflowGraph([]any{
				graphStart("s"),
				Object{"id": "t", "data": Object{"kind": "tool", "agentConfig": Object{"retry": 3}}},
			}, []any{}, nil),
			want: []any{Object{"code": "node_kind_unsupported", "nodeId": "t", "kind": "tool"}},
		},
		{
			// A key written as null asks for nothing, so it is not the "configuration the author
			// believes is in effect" the decision refuses; only a value is.
			name: "an agent node whose unused configuration field is null",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"retry": nil})},
				[]any{graphEdge("s", "a")}, nil),
		},
		{
			name:  "no start node",
			graph: workflowGraph([]any{Object{"id": "a", "data": Object{"kind": "output"}}}, []any{}, nil),
			want:  []any{Object{"code": "start_node_count"}},
		},
		{
			name:  "two start nodes",
			graph: workflowGraph([]any{graphStart("s1"), graphStart("s2")}, []any{}, nil),
			want:  []any{Object{"code": "start_node_count"}},
		},
		{
			name:  "an edge that is not an object",
			graph: workflowGraph([]any{graphStart("s")}, []any{"not an edge"}, nil),
			want:  []any{Object{"code": "edge_not_an_object"}},
		},
		{
			name:  "an edge to a node the document does not contain",
			graph: workflowGraph([]any{graphStart("s")}, []any{graphEdge("s", "gone")}, nil),
			want:  []any{Object{"code": "edge_endpoint_unknown", "nodeId": "gone"}},
		},
		{
			// A DAG that reconverges: two paths lead into `c`, which is a join and not a cycle. It is
			// the shape a cycle test most easily refuses by accident, so it is here as the legal half
			// of that pair.
			name: "a diamond",
			graph: workflowGraph(
				[]any{graphStart("s"), graphAgent("a", "", false), graphAgent("b", "", false), graphAgent("c", "", false)},
				[]any{graphEdge("s", "a"), graphEdge("s", "b"), graphEdge("a", "c"), graphEdge("b", "c")},
				nil,
			),
		},
		{
			// The shortest cycle there is, and one the editor can produce by dragging an edge back onto
			// its own source.
			name:  "a self-loop",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "", false)}, []any{graphEdge("s", "a"), graphEdge("a", "a")}, nil),
			want:  []any{Object{"code": "graph_cycle", "nodeId": "a"}},
		},
		{
			// The back edge is `b→a`, so `b` is the node reported: it is on the cycle by construction,
			// and it is the one whose outgoing edge the author has to delete.
			name:  "a cycle behind the start node",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "", false), graphAgent("b", "", false)}, []any{graphEdge("s", "a"), graphEdge("a", "b"), graphEdge("b", "a")}, nil),
			want:  []any{Object{"code": "graph_cycle", "nodeId": "b"}},
		},
		{
			// Nodes downstream of a cycle are not reported: they are casualties, not faults, and the
			// author would have nothing to fix at them.
			name: "a cycle with nodes downstream of it",
			graph: workflowGraph(
				[]any{graphStart("s"), graphAgent("a", "", false), graphAgent("b", "", false), graphAgent("c", "", false), graphAgent("o", "", false)},
				[]any{graphEdge("s", "a"), graphEdge("a", "b"), graphEdge("b", "a"), graphEdge("b", "c"), graphEdge("c", "o")},
				nil,
			),
			want: []any{Object{"code": "graph_cycle", "nodeId": "b"}},
		},
		{
			// `v` sits between two cycles without being on either. A detector that peels nodes with no
			// incoming or no outgoing edge leaves `v` in the remainder — nothing ever drains it — so a
			// report built that way cannot tell the node the author must fix from one they must not
			// touch. The walk names a back edge's source instead, which is on a cycle by construction.
			name: "a node between two cycles",
			graph: workflowGraph(
				[]any{graphStart("s"), graphAgent("a", "", false), graphAgent("u", "", false), graphAgent("v", "", false), graphAgent("w", "", false), graphAgent("x", "", false)},
				[]any{graphEdge("s", "a"), graphEdge("a", "u"), graphEdge("u", "a"), graphEdge("u", "v"), graphEdge("v", "w"), graphEdge("w", "x"), graphEdge("x", "w")},
				nil,
			),
			want: []any{Object{"code": "graph_cycle", "nodeId": "u"}},
		},
		{
			// The nameless slot's description is fine and is not also reported: a problem list the user
			// has to read is worth keeping to the violations that are actually there.
			name:  "a slot with no name",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("", "深度推理")}),
			want:  []any{Object{"code": "slot_name_missing"}},
		},
		{
			name:  "a slot name that is not a lowercase identifier",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("Strong", "深度推理")}),
			want:  []any{Object{"code": "slot_name_invalid", "slot": "Strong"}},
		},
		{
			// `default` is the one name every runner already has; declaring it would let an author
			// redefine it.
			name:  "a slot named default",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("default", "深度推理")}),
			want:  []any{Object{"code": "slot_name_reserved", "slot": "default"}},
		},
		{
			name: "a duplicated slot name",
			graph: workflowGraph([]any{graphStart("s")}, []any{},
				[]any{graphSlot("strong", "深度推理"), graphSlot("strong", "另一个")}),
			want: []any{Object{"code": "slot_name_duplicate", "slot": "strong"}},
		},
		{
			name:  "a slot with no description",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("strong", "   ")}),
			want:  []any{Object{"code": "slot_description_invalid", "slot": "strong"}},
		},
		{
			name:  "a slot description past the limit",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("strong", longDescription)}),
			want:  []any{Object{"code": "slot_description_invalid", "slot": "strong"}},
		},
		{
			// specs#98 D1 counts characters, not bytes: this document is 1500 bytes and legal. The
			// case is the regression the byte-counting implementation failed — it refused every
			// Chinese description past the 166th character.
			name:  "a slot description at the limit in Chinese",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("strong", chineseDescription)}),
		},
		{
			name:  "a slot description one character past the limit in Chinese",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{graphSlot("strong", chineseDescriptionOverLimit)}),
			want:  []any{Object{"code": "slot_description_invalid", "slot": "strong"}},
		},
		{
			// The same unit question for the hint: a model id is an opaque string, so its ceiling is
			// counted the same way the description's is.
			name: "a hint at the limit in Chinese",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{
				Object{"name": "strong", "description": "深度推理", "hint": Object{"modelId": chineseModelID}},
			}),
		},
		{
			name: "a hint past the limit",
			graph: workflowGraph([]any{graphStart("s")}, []any{}, []any{
				Object{"name": "strong", "description": "深度推理", "hint": Object{"modelId": chineseModelID + "模"}},
			}),
			want: []any{Object{"code": "slot_hint_invalid", "slot": "strong"}},
		},
		{
			name: "a hint of the wrong shape",
			graph: workflowGraph([]any{graphStart("s")}, []any{},
				[]any{Object{"name": "strong", "description": "深度推理", "hint": Object{"agentCli": 7}}}),
			want: []any{Object{"code": "slot_hint_invalid", "slot": "strong"}},
		},
		{
			// A reference to an undeclared slot is refused at publish rather than at run time: only the
			// author can say what the slot means, and a run against it could never resolve an executor.
			name:  "a reference to an undeclared slot",
			graph: workflowGraph([]any{graphStart("s"), graphAgent("a", "strong", false)}, []any{graphEdge("s", "a")}, nil),
			want:  []any{Object{"code": "slot_not_declared", "nodeId": "a", "slot": "strong"}},
		},
	}

	// Each first-phase-unused agent configuration field is refused by name (#67 D2). The cases are
	// generated from the list the validator reads, so a field added there cannot go untested here.
	for _, field := range workflowUnsupportedAgentConfigFields {
		cases = append(cases, graphCase{
			name:  "an agent node carrying " + field,
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{field: "set"})}, []any{graphEdge("s", "a")}, nil),
			want:  []any{Object{"code": "node_config_unsupported", "nodeId": "a", "field": field}},
		})
	}

	// All six at once, in the order the list declares: that order is what the editor renders, so it
	// must come from the list rather than from the document's map iteration.
	everyUnusedField := Object{}
	wantEveryField := []any{}
	for _, field := range workflowUnsupportedAgentConfigFields {
		everyUnusedField[field] = true
		wantEveryField = append(wantEveryField, Object{"code": "node_config_unsupported", "nodeId": "a", "field": field})
	}
	cases = append(cases, graphCase{
		name:  "an agent node carrying every unused configuration field",
		graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", everyUnusedField)}, []any{graphEdge("s", "a")}, nil),
		want:  wantEveryField,
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// decodeGraph renders the hand-built document the way the jsonb column hands it back:
			// every nested object a plain map rather than the named Object type it was built from.
			// The validator reads stored documents, and that is the shape it actually sees.
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, tc.graph)))
			if tc.want == nil {
				tc.want = []any{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
		})
	}
}

// The publish gate refuses every condition configuration Cloud cannot evaluate the way desktop's
// condition.rs does (circus#202 A 条件 2). Each one is a place the evaluator would otherwise answer
// silently — a no-match, an `and`, an else branch, a whole node output, a literal where desktop
// resolves — so a graph would publish and then take a branch nothing in the document asked for.
//
// The cases and workflowUnsupportedConditionConfigs are checked against each other in both
// directions: a declared code with no case here fails, and a case producing a code the list does not
// declare fails too. That is what keeps the ADR's list, the PR's list and the gate's behavior the
// same list, and what makes circus#210's job — deleting one entry at a time as each difference
// is closed — a change this test refuses to let pass unnoticed.
func TestWorkflowPublishRefusesUnsupportedConditionConfigs(t *testing.T) {
	selector := []any{"s", "input", "flag"}
	filled := []any{graphRule(selector, "equals", true)}
	cases := []struct {
		name string
		node Object
		want []any
	}{
		{
			name: "cases of the wrong shape",
			node: graphCondition("c", "not a list"),
			want: []any{Object{"code": "condition_cases_not_a_list", "nodeId": "c", "field": "cases"}},
		},
		{
			name: "a case that is not an object",
			node: graphCondition("c", []any{"nope"}),
			want: []any{Object{"code": "condition_case_not_an_object", "nodeId": "c", "field": "cases"}},
		},
		{
			name: "a case with no id",
			node: graphCondition("c", []any{graphConditionCase("", "and", filled)}),
			want: []any{Object{"code": "condition_case_id_missing", "nodeId": "c", "field": "cases.id"}},
		},
		{
			name: "a logic neither side knows",
			node: graphCondition("c", []any{graphConditionCase("yes", "xor", filled)}),
			want: []any{Object{"code": "condition_logic_unsupported", "nodeId": "c", "field": "cases.logic"}},
		},
		{
			name: "rules of the wrong shape",
			node: graphCondition("c", []any{Object{"id": "yes", "logic": "and", "conditions": "nope"}}),
			want: []any{Object{"code": "condition_rules_not_a_list", "nodeId": "c", "field": "cases.conditions"}},
		},
		{
			name: "a rule that is not an object",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{"nope"})}),
			want: []any{Object{"code": "condition_rule_not_an_object", "nodeId": "c", "field": "cases.conditions"}},
		},
		{
			name: "a selector that is not a list",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{Object{"variableSelector": "s.input.flag", "operator": "equals"}})}),
			want: []any{Object{"code": "condition_selector_unsupported", "nodeId": "c", "field": "cases.conditions.variableSelector"}},
		},
		{
			// The editor hands a new comparison an empty selector; this is that state reaching publish.
			name: "a selector with no path",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{}, "equals", true)})}),
			want: []any{Object{"code": "condition_selector_unsupported", "nodeId": "c", "field": "cases.conditions.variableSelector"}},
		},
		{
			// One element names the whole node output on Cloud and is refused by desktop, so the two
			// sides disagree about what the rule even reads.
			name: "a single-segment selector",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"s"}, "equals", true)})}),
			want: []any{Object{"code": "condition_selector_unsupported", "nodeId": "c", "field": "cases.conditions.variableSelector"}},
		},
		{
			name: "a path element that is not a name",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"s", 0}, "equals", true)})}),
			want: []any{Object{"code": "condition_selector_unsupported", "nodeId": "c", "field": "cases.conditions.variableSelector"}},
		},
		{
			name: "an operator neither side knows",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, "matches", true)})}),
			want: []any{Object{"code": "condition_operator_unsupported", "nodeId": "c", "field": "cases.conditions.operator"}},
		},
		{
			name: "an operator the editor has not filled in yet",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, "", true)})}),
			want: []any{Object{"code": "condition_operator_unsupported", "nodeId": "c", "field": "cases.conditions.operator"}},
		},
		{
			// desktop resolves this and keeps the bound type; Cloud would compare the literal text.
			name: "an expected value carrying a selector template",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, "equals", "{{#s.input.name#}}")})}),
			want: []any{Object{"code": "condition_value_template_unsupported", "nodeId": "c", "field": "cases.conditions.value"}},
		},
	}

	exercised := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := workflowGraph([]any{graphStart("s"), tc.node}, []any{graphEdge("s", "c")}, nil)
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, graph)))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
			for _, problem := range got {
				exercised[problem.(Object)["code"].(string)] = true
			}
		})
	}
	declared := map[string]bool{}
	for _, code := range workflowUnsupportedConditionConfigs {
		declared[code] = true
	}
	if !reflect.DeepEqual(exercised, declared) {
		t.Fatalf("these cases exercise %v; the declared list is %v — the two must name the same set", exercised, declared)
	}

	// Refusing is not the same as refusing everything. The two documents the editor legitimately
	// publishes — a filled-in node, and one the author has added but not configured yet, which falls
	// through to else — must both pass.
	for name, node := range map[string]Object{
		"a filled-in condition node":               graphCondition("c", []any{graphConditionCase("yes", "and", filled)}),
		"a condition node with no cases yet":       graphCondition("c", []any{}),
		"a condition node with an unfinished case": graphCondition("c", []any{graphConditionCase("yes", "and", []any{})}),
	} {
		got := problemObjects(validateWorkflowGraph(decodeGraph(t, workflowGraph([]any{graphStart("s"), node}, []any{graphEdge("s", "c")}, nil))))
		if len(got) != 0 {
			t.Fatalf("%s was refused: %v", name, got)
		}
	}
}

// TestRequireValidWorkflowGraphIsOneStableFault covers the wire contract of the publish refusal: one
// code whatever went wrong, and every problem carried as a parameter so the editor can mark each
// node. A client that branched on a per-violation code would break the day a new rule was added.
func TestRequireValidWorkflowGraphIsOneStableFault(t *testing.T) {
	noFault(t, func() {
		requireValidWorkflowGraph(decodeGraph(t, workflowGraph([]any{graphStart("s"), graphAgent("a", "", false)}, []any{graphEdge("s", "a")}, nil)))
	})

	fault := faultOf(t, func() {
		requireValidWorkflowGraph(decodeGraph(t, workflowGraph([]any{graphStart("s"), Object{"id": "t", "data": Object{"kind": "tool"}}}, []any{}, nil)))
	})
	if fault.Code != "workflow_graph_invalid" || fault.Status != 400 {
		t.Fatalf("fault = %s/%d, want workflow_graph_invalid/400", fault.Code, fault.Status)
	}
	want := []any{Object{"code": "node_kind_unsupported", "nodeId": "t", "kind": "tool"}}
	if !reflect.DeepEqual(fault.Params["problems"], want) {
		t.Fatalf("problems = %v, want %v", fault.Params["problems"], want)
	}
}
