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
	config := Object{"schemaVersion": 3, "prompt": "review"}
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
	config := Object{"schemaVersion": 3, "prompt": "review"}
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
			// The editor's default for a node with nothing selected is an empty list, which enables
			// nothing; refusing it would block every real desktop export (circus#204).
			name: "an agent node with empty skills and mcps",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"skills": []any{}, "mcps": []any{}})},
				[]any{graphEdge("s", "a")}, nil),
		},
		{
			// A non-empty list does enable behavior the first phase will not execute, so it stays a
			// refusal, reported in the order the list declares.
			name: "an agent node with non-empty skills and mcps",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"skills": []any{"review"}, "mcps": []any{"todo"}})},
				[]any{graphEdge("s", "a")}, nil),
			want: []any{
				Object{"code": "node_config_unsupported", "nodeId": "a", "field": "skills"},
				Object{"code": "node_config_unsupported", "nodeId": "a", "field": "mcps"},
			},
		},
		{
			// interactive: false is the editor's "not interactive" default and asks for nothing.
			name: "an agent node with interactive false",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"interactive": false})},
				[]any{graphEdge("s", "a")}, nil),
		},
		{
			// interactive: true does enable behavior the first phase will not execute.
			name: "an agent node with interactive true",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"interactive": true})},
				[]any{graphEdge("s", "a")}, nil),
			want: []any{Object{"code": "node_config_unsupported", "nodeId": "a", "field": "interactive"}},
		},
		{
			// The only envelope version Cloud executes.
			name:  "an agent node carrying schema version 3",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{})}, []any{graphEdge("s", "a")}, nil),
		},
		{
			// Version 4 introduces the interactive completion declaration Cloud refuses. The envelope is
			// reported first, so the author fixes the version rather than one field of it.
			name:  "an agent node carrying schema version 4",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"schemaVersion": 4})}, []any{graphEdge("s", "a")}, nil),
			want:  []any{Object{"code": "agent_schema_version_unsupported", "nodeId": "a", "field": "agentConfig.schemaVersion"}},
		},
		{
			// A value Cloud cannot read as the version it executes is refused, not guessed.
			name:  "an agent node carrying schema version as a string",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"schemaVersion": "4"})}, []any{graphEdge("s", "a")}, nil),
			want:  []any{Object{"code": "agent_schema_version_unsupported", "nodeId": "a", "field": "agentConfig.schemaVersion"}},
		},
		{
			// Cloud never executed version 1, so a draft still on it is refused instead of being read
			// with the current field list; that is the compatibility change the ADR records.
			name:  "an agent node carrying schema version 1",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"schemaVersion": 1})}, []any{graphEdge("s", "a")}, nil),
			want:  []any{Object{"code": "agent_schema_version_unsupported", "nodeId": "a", "field": "agentConfig.schemaVersion"}},
		},
		{
			// Null asks for nothing, like the other absent-or-null configuration values in this matrix.
			name:  "an agent node carrying a null schema version",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{"schemaVersion": nil})}, []any{graphEdge("s", "a")}, nil),
		},
		{
			// A mis-versioned document that also carries the completion declaration reports both, the
			// envelope first and the field after it, in the order the validator appends them.
			name: "an agent node carrying version 4 and completion",
			graph: workflowGraph([]any{graphStart("s"), graphAgentConfig("a", Object{
				"schemaVersion": 4,
				"completion":    Object{"files": []any{}},
			})}, []any{graphEdge("s", "a")}, nil),
			want: []any{
				Object{"code": "agent_schema_version_unsupported", "nodeId": "a", "field": "agentConfig.schemaVersion"},
				Object{"code": "node_config_unsupported", "nodeId": "a", "field": "completion"},
			},
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

// The publish gate validates the retry policy Cloud executes (retry decision D1). Absent and null are
// the default policy and are admitted; a present value must be a complete object, because a partial
// one would silently run defaults for the fields the author left out. Every invalid field is reported
// at once, and unknown keys are ignored like everywhere else in the document, so a newer editor's
// extra key cannot block publishing. The runtime parser is deliberately more forgiving
// (TestWorkflowRetryPolicyOf): a frozen snapshot is never re-gated.
func TestWorkflowPublishValidatesAgentRetry(t *testing.T) {
	valid := func(enabled, maxRetries, delay any) Object {
		return Object{"enabled": enabled, "maxRetries": maxRetries, "initialDelaySeconds": delay}
	}
	cases := []struct {
		name   string
		absent bool
		retry  any
		want   []any
	}{
		{name: "no retry policy", absent: true},
		{name: "retry null", retry: nil},
		{name: "a complete policy", retry: valid(true, 2, 10)},
		{name: "the lower bounds", retry: valid(false, 0, 0)},
		{name: "the upper bounds", retry: valid(true, 5, 300)},
		{
			name:  "a policy with an unknown key",
			retry: Object{"enabled": true, "maxRetries": 2, "initialDelaySeconds": 10, "future": 1},
		},
		{
			name:  "retry not an object",
			retry: true,
			want:  []any{Object{"code": "agent_retry_not_an_object", "nodeId": "a", "field": "agentConfig.retry"}},
		},
		{
			name:  "retry a list",
			retry: []any{true, 2, 10},
			want:  []any{Object{"code": "agent_retry_not_an_object", "nodeId": "a", "field": "agentConfig.retry"}},
		},
		{
			// A partial object is refused as a whole rather than filled from the defaults: "I only
			// changed the delay" must not also change the enabled flag or the budget.
			name:  "an empty object reports every missing field",
			retry: Object{},
			want: []any{
				Object{"code": "agent_retry_enabled_invalid", "nodeId": "a", "field": "agentConfig.retry.enabled"},
				Object{"code": "agent_retry_max_retries_invalid", "nodeId": "a", "field": "agentConfig.retry.maxRetries"},
				Object{"code": "agent_retry_initial_delay_invalid", "nodeId": "a", "field": "agentConfig.retry.initialDelaySeconds"},
			},
		},
		{
			name:  "enabled of the wrong type",
			retry: valid("true", 2, 10),
			want:  []any{Object{"code": "agent_retry_enabled_invalid", "nodeId": "a", "field": "agentConfig.retry.enabled"}},
		},
		{
			name:  "maxRetries below the range",
			retry: valid(true, -1, 10),
			want:  []any{Object{"code": "agent_retry_max_retries_invalid", "nodeId": "a", "field": "agentConfig.retry.maxRetries"}},
		},
		{
			name:  "maxRetries above the range",
			retry: valid(true, 6, 10),
			want:  []any{Object{"code": "agent_retry_max_retries_invalid", "nodeId": "a", "field": "agentConfig.retry.maxRetries"}},
		},
		{
			name:  "a fractional maxRetries",
			retry: valid(true, 1.5, 10),
			want:  []any{Object{"code": "agent_retry_max_retries_invalid", "nodeId": "a", "field": "agentConfig.retry.maxRetries"}},
		},
		{
			name:  "initialDelaySeconds below the range",
			retry: valid(true, 2, -1),
			want:  []any{Object{"code": "agent_retry_initial_delay_invalid", "nodeId": "a", "field": "agentConfig.retry.initialDelaySeconds"}},
		},
		{
			name:  "initialDelaySeconds above the range",
			retry: valid(true, 2, 301),
			want:  []any{Object{"code": "agent_retry_initial_delay_invalid", "nodeId": "a", "field": "agentConfig.retry.initialDelaySeconds"}},
		},
		{
			name:  "every field invalid",
			retry: Object{"enabled": 1, "maxRetries": "two", "initialDelaySeconds": false},
			want: []any{
				Object{"code": "agent_retry_enabled_invalid", "nodeId": "a", "field": "agentConfig.retry.enabled"},
				Object{"code": "agent_retry_max_retries_invalid", "nodeId": "a", "field": "agentConfig.retry.maxRetries"},
				Object{"code": "agent_retry_initial_delay_invalid", "nodeId": "a", "field": "agentConfig.retry.initialDelaySeconds"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := Object{}
			if !tc.absent {
				extra["retry"] = tc.retry
			}
			graph := workflowGraph(
				[]any{graphStart("s"), graphAgentConfig("a", extra)},
				[]any{graphEdge("s", "a")}, nil,
			)
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, graph)))
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
			// Desktop builds its pool from the graph, so a root the Start node does not declare is
			// Undeclared there and fails the node. Cloud's evaluator has no declaration set and would
			// answer "absent" instead — a branch the author could not have written — so publishing
			// refuses it.
			name: "a selector on an undeclared root",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"s", "missing"}, "exists", nil)})}),
			want: []any{Object{"code": "condition_selector_undeclared", "nodeId": "c", "field": "cases.conditions.variableSelector"}},
		},
		{
			// A selector on a node the graph does not contain. System globals and user globals are now
			// declared too, so `sys.workflow_id` is no longer the absent node it once was; a genuinely
			// absent id is still refused.
			name: "a selector on a node the graph does not contain",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"ghost", "output"}, "exists", nil)})}),
			want: []any{Object{"code": "condition_selector_undeclared", "nodeId": "c", "field": "cases.conditions.variableSelector"}},
		},
		{
			// An expected-value template names the same variable pool as variableSelector, so its
			// placeholder must hold to the same declarations. `selectedBranch` is exactly what Cloud's
			// condition node writes at run time, but desktop declares no root on a condition node, so a
			// template reading it would resolve on Cloud and fail desktop.
			name: "an expected-value template on an undeclared root",
			node: graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"s", "input"}, "equals", "{{#c.selectedBranch#}}")})}),
			want: []any{Object{"code": "condition_selector_undeclared", "nodeId": "c", "field": "cases.conditions.value"}},
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

	// Refusing is not the same as refusing everything. The documents the editor legitimately
	// publishes — a filled-in node, one the author has added but not configured yet (falls through to
	// else), an expected value carrying a selector template (circus#210 now evaluates it the way
	// desktop does), and a selector on each root a node's kind declares — must all pass.
	startWithVariables := Object{"id": "s", "type": "workflow", "data": Object{
		"kind":           "start",
		"inputVariables": []any{Object{"name": "limit", "valueType": "number"}},
	}}
	agentNode := graphAgent("a", "", false)
	outputNode := Object{"id": "o", "type": "workflow", "data": Object{"kind": "output"}}
	oneRule := func(selector []any, operator string, value any) Object {
		return graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, operator, value)})})
	}
	positive := []struct {
		name    string
		nodes   []any
		edges   []any
		globals []any
	}{
		{
			name:  "an expected value carrying a selector template",
			nodes: []any{graphStart("s"), graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, "equals", "{{#s.input.name#}}")})})},
			edges: []any{graphEdge("s", "c")},
		},
		{
			name:  "a mixed expected-value template",
			nodes: []any{graphStart("s"), graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, "equals", "branch {{#s.input.name#}}")})})},
			edges: []any{graphEdge("s", "c")},
		},
		{
			name:  "a filled-in condition node",
			nodes: []any{graphStart("s"), graphCondition("c", []any{graphConditionCase("yes", "and", filled)})},
			edges: []any{graphEdge("s", "c")},
		},
		{
			name:  "a condition node with no cases yet",
			nodes: []any{graphStart("s"), graphCondition("c", []any{})},
			edges: []any{graphEdge("s", "c")},
		},
		{
			name:  "a condition node with an unfinished case",
			nodes: []any{graphStart("s"), graphCondition("c", []any{graphConditionCase("yes", "and", []any{})})},
			edges: []any{graphEdge("s", "c")},
		},
		{
			// Start declares every run-form variable by name, so [s, limit] is a root the pool has.
			name:  "a declared start form variable",
			nodes: []any{startWithVariables, oneRule([]any{"s", "limit"}, "greater_than", 90.0)},
			edges: []any{graphEdge("s", "c")},
		},
		{
			// Every non-start, non-condition node declares `output`; the agent's value arrives with
			// cloud#70, and until then the root is declared but unassigned.
			name:  "a declared agent output root",
			nodes: []any{graphStart("s"), agentNode, oneRule([]any{"a", "output"}, "not_empty", nil)},
			edges: []any{graphEdge("s", "a"), graphEdge("a", "c")},
		},
		{
			name:  "a declared output node root",
			nodes: []any{graphStart("s"), outputNode, oneRule([]any{"o", "output"}, "not_empty", nil)},
			edges: []any{graphEdge("s", "o"), graphEdge("o", "c")},
		},
		{
			// The system globals are always declared, so a selector on them is never refused even
			// though the graph carries no `sys` node.
			name:  "a selector on a system global",
			nodes: []any{graphStart("s"), oneRule([]any{"sys", "workflow_id"}, "equals", "wf-1")},
			edges: []any{graphEdge("s", "c")},
		},
		{
			// A user global declares its writer's second segment, so [g, region] resolves the same
			// way desktop's pool resolves it.
			name:    "a selector on a declared user global",
			nodes:   []any{graphStart("s"), oneRule([]any{"g", "region"}, "equals", "eu")},
			edges:   []any{graphEdge("s", "c")},
			globals: []any{Object{"name": "g.region", "valueType": "string", "value": "eu"}},
		},
	}
	for _, tc := range positive {
		t.Run(tc.name, func(t *testing.T) {
			graph := workflowGraph(tc.nodes, tc.edges, nil)
			if tc.globals != nil {
				graph["globalVariables"] = tc.globals
			}
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, graph)))
			if len(got) != 0 {
				t.Fatalf("refused: %v", got)
			}
		})
	}
}

// TestWorkflowPublishRefusesInvalidGlobals is the global-declaration matrix: every rule the publish
// gate enforces on `globalVariables`, mirroring desktop's into_global_variables, has a case that
// violates it, and the documents desktop accepts are admitted. The cases pin the wire codes so a
// client can rely on them.
func TestWorkflowPublishRefusesInvalidGlobals(t *testing.T) {
	nodes := []any{graphStart("s"), graphCondition("c", []any{})}
	edges := []any{graphEdge("s", "c")}
	graphWithGlobals := func(globals any) Object {
		g := workflowGraph(nodes, edges, nil)
		g["globalVariables"] = globals
		return g
	}
	cases := []struct {
		name    string
		globals any
		want    []any
	}{
		{
			name:    "a non-list declaration",
			globals: "not a list",
			want:    []any{Object{"code": "globals_not_a_list"}},
		},
		{
			name:    "an entry that is not an object",
			globals: []any{7},
			want:    []any{Object{"code": "global_not_an_object"}},
		},
		{
			name:    "an unqualified name",
			globals: []any{Object{"name": "region", "valueType": "string", "value": "eu"}},
			want:    []any{Object{"code": "global_name_invalid", "name": "region"}},
		},
		{
			name:    "a name with a leading dot",
			globals: []any{Object{"name": ".region", "valueType": "string", "value": "eu"}},
			want:    []any{Object{"code": "global_name_invalid", "name": ".region"}},
		},
		{
			name: "a duplicated name",
			globals: []any{
				Object{"name": "g.region", "valueType": "string", "value": "eu"},
				Object{"name": "g.region", "valueType": "string", "value": "us"},
			},
			want: []any{Object{"code": "global_name_duplicate", "name": "g.region"}},
		},
		{
			name:    "an unsupported type",
			globals: []any{Object{"name": "g.region", "valueType": "datetime", "value": "2024-01-01"}},
			want:    []any{Object{"code": "global_type_unsupported", "name": "g.region"}},
		},
		{
			name:    "a non-system global without an initial value",
			globals: []any{Object{"name": "g.region", "valueType": "string"}},
			want:    []any{Object{"code": "global_value_missing", "name": "g.region"}},
		},
		{
			name:    "a global claiming the platform's reserved form key",
			globals: []any{Object{"name": "snapshotId", "valueType": "string", "value": "v1"}},
			want:    []any{Object{"code": "reserved_field_name", "name": "snapshotId"}},
		},
		{
			// The form keys a global by its declared name, so a surrounding-whitespace variant is a
			// different key that never enters the form; it is refused for its syntax, not for the name.
			name:    "a whitespace variant is not the reserved form key",
			globals: []any{Object{"name": " snapshotId ", "valueType": "string", "value": "v1"}},
			want:    []any{Object{"code": "global_name_invalid", "name": "snapshotId"}},
		},
		{
			name:    "a value that does not match the declared type",
			globals: []any{Object{"name": "g.count", "valueType": "integer", "value": "many"}},
			want:    []any{Object{"code": "global_value_mismatch", "name": "g.count"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, graphWithGlobals(tc.globals))))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
		})
	}

	// A system global needs no initial value, and a matching value (or none) is admitted.
	admitted := []struct {
		name    string
		globals any
	}{
		{
			name:    "system globals without values",
			globals: []any{Object{"name": "sys.workflow_id", "valueType": "string"}, Object{"name": "sys.timestamp", "valueType": "number"}},
		},
		{
			name:    "a user global with a matching value",
			globals: []any{Object{"name": "g.region", "valueType": "string", "value": "eu"}},
		},
	}
	for _, tc := range admitted {
		t.Run(tc.name, func(t *testing.T) {
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, graphWithGlobals(tc.globals))))
			if len(got) != 0 {
				t.Fatalf("refused: %v", got)
			}
		})
	}
}

// TestWorkflowPublishRefusesReservedFieldDeclarations covers the reserved `snapshotId` gate
// (20261021-snapshot-id-is-a-reserved-field.md D1) at the two positions that reach the `@` form. The
// declaration is refused wherever it is written; everything that merely mentions the name without
// declaring it — a `launchFields` withdrawal, the pre-rename `version` key, an ordinary variable — is
// admitted, because refusing those would refuse documents that cannot collide with the cursor.
func TestWorkflowPublishRefusesReservedFieldDeclarations(t *testing.T) {
	reserved := []any{Object{"name": "snapshotId", "displayName": "Release", "valueType": "string"}}
	startNode := func(variables []any) Object {
		return Object{"id": "s", "data": Object{"kind": "start", "inputVariables": variables}}
	}
	graph := func(node, extra Object) Object {
		g := workflowGraph([]any{node}, []any{}, nil)
		for key, value := range extra {
			g[key] = value
		}
		return g
	}
	cases := []struct {
		name  string
		graph Object
		want  []any
	}{
		{
			name:  "a Start variable claims the key",
			graph: graph(startNode(reserved), nil),
			want:  []any{Object{"code": "reserved_field_name", "nodeId": "s", "field": "snapshotId"}},
		},
		{
			name:  "a global claims the key",
			graph: graph(graphStart("s"), Object{"globalVariables": []any{Object{"name": "snapshotId", "valueType": "string", "value": "v1"}}}),
			want:  []any{Object{"code": "reserved_field_name", "name": "snapshotId"}},
		},
		{
			name:  "both positions claim the key",
			graph: graph(startNode(reserved), Object{"globalVariables": []any{Object{"name": "snapshotId", "valueType": "string", "value": "v1"}}}),
			want: []any{
				Object{"code": "reserved_field_name", "nodeId": "s", "field": "snapshotId"},
				Object{"code": "reserved_field_name", "name": "snapshotId"},
			},
		},
		{
			name:  "withdrawing the platform field is not a declaration",
			graph: graph(graphStart("s"), Object{"launchFields": []any{Object{"key": "snapshotId", "enabled": false}}}),
			want:  []any{},
		},
		{
			name:  "the pre-rename version key is not reserved",
			graph: graph(startNode([]any{Object{"name": "version", "displayName": "Version", "valueType": "string"}}), nil),
			want:  []any{},
		},
		{
			name:  "an ordinary variable is not reserved",
			graph: graph(startNode([]any{Object{"name": "releaseId", "displayName": "Release", "valueType": "string"}}), nil),
			want:  []any{},
		},
		{
			// The form keys a Start variable by its declared name, so a whitespace variant is a
			// different key that never enters the form; it is not a claim on the platform's key.
			name:  "a whitespace variant is not the reserved form key",
			graph: graph(startNode([]any{Object{"name": " snapshotId ", "displayName": "Release", "valueType": "string"}}), nil),
			want:  []any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoded := decodeGraph(t, tc.graph)
			got := problemObjects(validateWorkflowGraph(decoded))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems = %v, want %v", got, tc.want)
			}
			if declared := workflowDeclaresReservedField(decoded); declared != (len(tc.want) > 0) {
				t.Fatalf("workflowDeclaresReservedField = %t, want %t", declared, len(tc.want) > 0)
			}
		})
	}
}

// TestNormalizeWorkflowGlobals pins the one rewrite the publish gate performs before freezing: a file
// or array[file] global whose value is a legacy path string is replaced in place by its canonical
// {kind: "workspace_file", path} reference, so a condition reading the frozen snapshot sees the same
// shape desktop's into_global_variables produces. Every other declaration is left as the author wrote
// it, and a value that is already canonical normalizes to itself.
func TestNormalizeWorkflowGlobals(t *testing.T) {
	cases := []struct {
		name string
		in   Object
		want Object
	}{
		{
			name: "a file global whose value is a legacy path string",
			in:   Object{"globalVariables": []any{Object{"name": "f.report", "valueType": "file", "value": "docs/report.md"}}},
			want: Object{"globalVariables": []any{Object{"name": "f.report", "valueType": "file", "value": Object{"kind": "workspace_file", "path": "docs/report.md"}}}},
		},
		{
			name: "an array[file] global whose values are legacy path strings",
			in:   Object{"globalVariables": []any{Object{"name": "f.reports", "valueType": "array[file]", "value": []any{"docs/a.md", "docs/b.md"}}}},
			want: Object{"globalVariables": []any{Object{"name": "f.reports", "valueType": "array[file]", "value": []any{
				Object{"kind": "workspace_file", "path": "docs/a.md"},
				Object{"kind": "workspace_file", "path": "docs/b.md"},
			}}}},
		},
		{
			name: "a file global whose value is already a canonical reference",
			in:   Object{"globalVariables": []any{Object{"name": "f.report", "valueType": "file", "value": Object{"kind": "workspace_file", "path": "docs/report.md"}}}},
			want: Object{"globalVariables": []any{Object{"name": "f.report", "valueType": "file", "value": Object{"kind": "workspace_file", "path": "docs/report.md"}}}},
		},
		{
			name: "a non-file global is left as authored",
			in:   Object{"globalVariables": []any{Object{"name": "g.region", "valueType": "string", "value": "eu"}}},
			want: Object{"globalVariables": []any{Object{"name": "g.region", "valueType": "string", "value": "eu"}}},
		},
		{
			name: "a graph without globalVariables",
			in:   Object{"nodes": []any{}},
			want: Object{"nodes": []any{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeWorkflowGlobals(decodeGraph(t, tc.in))
			// Compare the canonical JSON: normalizeWorkflowGlobals builds the rewritten values as typed
			// Object maps while the round-trip decoder produces plain maps, so the two spellings of the
			// same document are equal as JSON but not under reflect.DeepEqual.
			if gotJSON, wantJSON := string(encodeGraph(t, got)), string(encodeGraph(t, tc.want)); gotJSON != wantJSON {
				t.Fatalf("normalizeWorkflowGlobals = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestNormalizedSnapshotGraphText pins the importer's byte-for-byte contract: the snapshot text is
// the caller's raw document unless normalizeWorkflowGlobals rewrote a legacy file or array[file]
// value, and only that rewrite is re-encoded.
func TestNormalizedSnapshotGraphText(t *testing.T) {
	cases := []struct {
		name    string
		graph   Object
		raw     string
		rewrite bool
	}{
		{
			name: "a legacy file value is re-encoded",
			graph: Object{"globalVariables": []any{
				Object{"name": "f.report", "valueType": "file", "value": "docs/report.md"},
			}},
			raw:     `{"globalVariables":[{"name":"f.report","valueType":"file","value":"docs/report.md"}]}`,
			rewrite: true,
		},
		{
			name: "a legacy array[file] value is re-encoded",
			graph: Object{"globalVariables": []any{
				Object{"name": "f.reports", "valueType": "array[file]", "value": []any{"docs/a.md", "docs/b.md"}},
			}},
			raw:     `{"globalVariables":[{"name":"f.reports","valueType":"array[file]","value":["docs/a.md","docs/b.md"]}]}`,
			rewrite: true,
		},
		{
			name: "an already-canonical file value keeps the raw text",
			graph: Object{"globalVariables": []any{
				Object{"name": "f.report", "valueType": "file", "value": Object{"kind": "workspace_file", "path": "docs/report.md"}},
			}},
			raw:     `{"globalVariables":[{"name":"f.report","valueType":"file","value":{"kind":"workspace_file","path":"docs/report.md"}}]}`,
			rewrite: false,
		},
		{
			name: "a non-file global keeps the raw text",
			graph: Object{"globalVariables": []any{
				Object{"name": "g.region", "valueType": "string", "value": "eu"},
			}},
			raw:     `{"globalVariables":[{"name":"g.region","valueType":"string","value":"eu"}]}`,
			rewrite: false,
		},
		{
			name:    "a graph without globals keeps the raw text",
			graph:   Object{"nodes": []any{}},
			raw:     `{"nodes":[]}`,
			rewrite: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizedSnapshotGraphText(decodeGraph(t, tc.graph), tc.raw)
			if !tc.rewrite {
				if got != tc.raw {
					t.Fatalf("normalizedSnapshotGraphText = %s, want the raw text unchanged", got)
				}
				return
			}
			if got == tc.raw {
				t.Fatal("a legacy file value was not rewritten")
			}
			if !strings.Contains(got, `"kind":"workspace_file"`) {
				t.Fatalf("normalizedSnapshotGraphText = %s, want a workspace_file reference", got)
			}
		})
	}
}

// TestWorkflowPublishAcceptsOnlyDesktopsOperatorSet pins the accepted set to desktop's
// `ComparisonOperator::parse` from both sides. A spelling only one side knows is the divergence the
// publish gate exists to prevent: Cloud would admit a document desktop then reads as an operator it
// does not have, and the two would branch differently on the same rule.
//
// The direction labels the comparator helpers are built with — "starts", "ends", "greater",
// "greater_eq", "less", "less_eq" — read like operator spellings but are Go-side names for one
// comparison, never wire operators, so a document that writes one is refused like any other
// unknown operator. The map keys are checked directly as well: the gate reads that map rather than
// restating the list, so a key added there reaches the gate with nothing else to stop it.
func TestWorkflowPublishAcceptsOnlyDesktopsOperatorSet(t *testing.T) {
	selector := []any{"s", "input", "flag"}
	// desktop's wire spellings, in the order `ComparisonOperator::parse` lists them.
	desktop := []string{
		"equals", "not_equals", "contains", "not_contains", "starts_with", "ends_with",
		"empty", "not_empty", "greater_than", "greater_than_or_equal", "less_than",
		"less_than_or_equal", "is", "is_not", "exists", "not_exists",
	}

	for _, operator := range desktop {
		t.Run("accepts "+operator, func(t *testing.T) {
			node := graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, operator, true)})})
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, workflowGraph([]any{graphStart("s"), node}, []any{graphEdge("s", "c")}, nil))))
			if len(got) != 0 {
				t.Fatalf("%q is desktop's operator but was refused: %v", operator, got)
			}
		})
	}

	// The comparison names the evaluator builds its operators from, which no document may write.
	for _, operator := range []string{"starts", "ends", "greater", "greater_eq", "less", "less_eq"} {
		t.Run("refuses "+operator, func(t *testing.T) {
			node := graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule(selector, operator, true)})})
			got := problemObjects(validateWorkflowGraph(decodeGraph(t, workflowGraph([]any{graphStart("s"), node}, []any{graphEdge("s", "c")}, nil))))
			want := []any{Object{"code": "condition_operator_unsupported", "nodeId": "c", "field": "cases.conditions.operator"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("problems = %v, want %v", got, want)
			}
		})
	}

	known := map[string]bool{}
	for _, operator := range desktop {
		known[operator] = true
	}
	if len(workflowConditionOperators) != len(known) {
		t.Fatalf("the evaluator's table has %d operators; desktop's parse has %d", len(workflowConditionOperators), len(known))
	}
	for operator := range workflowConditionOperators {
		if !known[operator] {
			t.Fatalf("the evaluator's table accepts %q, which desktop's parse does not", operator)
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
