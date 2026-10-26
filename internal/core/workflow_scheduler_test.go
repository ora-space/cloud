package core

import "testing"

// The workflow scheduler's pure core: the projection (ready-set recompute from committed state) and
// the branch semantics. These test the parts that decide WHAT happens next; the DB tests
// (workflow_scheduler_db_test.go) cover that the pass commits them.

// wfNode builds one node document: id, kind, title and the data fields tests need.
func wfNode(id, kind, title string) Object {
	data := Object{"kind": kind}
	if title != "" {
		data["title"] = title
	}
	return Object{"id": id, "data": data}
}

// wfGraph builds a graph document from nodes and source→target(→handle) edge triples.
func wfGraph(nodes []Object, edges [][3]string) Object {
	nodeList := make([]any, 0, len(nodes))
	for _, node := range nodes {
		nodeList = append(nodeList, map[string]any(node))
	}
	edgeList := make([]any, 0, len(edges))
	for _, e := range edges {
		edge := Object{"source": e[0], "target": e[1]}
		if e[2] != "" {
			edge["sourceHandle"] = e[2]
		}
		edgeList = append(edgeList, map[string]any(edge))
	}
	return Object{"nodes": nodeList, "edges": edgeList}
}

// wfSlot declares one executor slot for a graph under test.
func wfSlot(name, description string) Object {
	return Object{"name": name, "description": description}
}

// wfAgentNode is an agent node whose agentConfig the test authors directly.
func wfAgentNode(id string, config Object) Object {
	node := wfNode(id, "agent", "")
	node["data"].(Object)["agentConfig"] = config
	return node
}

// A start→output graph runs to drained in two waves: start readies, then output, then nothing.
func TestWorkflowProjectionLinearGraph(t *testing.T) {
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "")},
		[][3]string{{"s", "o", ""}},
	))
	states := map[string]workflowNodeState{}

	p := projectWorkflowNodes(graph, states)
	node, ok := p.readyNode()
	if !ok || node.ID != "s" {
		t.Fatalf("first ready node = %q, %v; want start s", node.ID, ok)
	}
	states["s"] = executeWorkflowNode(node, Object{"flag": true}, states, nil)

	p = projectWorkflowNodes(graph, states)
	node, ok = p.readyNode()
	if !ok || node.ID != "o" {
		t.Fatalf("second ready node = %q, %v; want output o", node.ID, ok)
	}
	states["o"] = executeWorkflowNode(node, nil, states, nil)

	p = projectWorkflowNodes(graph, states)
	if _, ok := p.readyNode(); ok {
		t.Fatal("drained linear graph still reports a ready node")
	}
	if p.hasFailure() {
		t.Fatal("drained linear graph reported a failure")
	}
}

// wfConditionNode is a condition node whose cases the test authors directly.
func wfConditionNode(id string, cases any) Object {
	node := wfNode(id, "condition", "")
	node["data"].(Object)["cases"] = cases
	return node
}

// A condition commits its branch: the matched branch's edge goes active, every other outgoing edge
// goes inactive, and downstream nodes resolve accordingly.
func TestWorkflowProjectionBranchSelection(t *testing.T) {
	yes := Object{"id": "yes", "logic": "and", "conditions": []any{
		Object{"variableSelector": []any{"s", "flag"}, "operator": "equals", "value": true},
	}}
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfConditionNode("c", []any{yes}), wfNode("y", "output", ""), wfNode("e", "output", "")},
		[][3]string{{"s", "c", ""}, {"c", "y", "yes"}, {"c", "e", "else"}},
	))
	states := map[string]workflowNodeState{}
	states["s"] = executeWorkflowNode(graph.ByNode["s"], Object{"flag": true}, states, nil)

	branch, err := evaluateWorkflowCondition(graph.ByNode["c"], states)
	if err != nil {
		t.Fatalf("condition evaluation: %v", err)
	}
	if branch != "yes" {
		t.Fatalf("condition selected %q; want yes", branch)
	}
	states["c"] = workflowNodeState{Status: "succeeded", Output: Object{"selectedBranch": branch}}

	p := projectWorkflowNodes(graph, states)
	if node, ok := p.readyNode(); !ok || node.ID != "y" {
		t.Fatalf("ready node = %q, %v; want taken branch y", node.ID, ok)
	}
	if p.projected["e"] != "inactive" {
		t.Fatalf("untaken branch e = %q; want inactive", p.projected["e"])
	}
}

// An edge with no sourceHandle is the else edge: when no case matches, it is the one that goes
// active (desktop branch_projection: an unlabeled edge defaults to else).
func TestWorkflowProjectionUnlabeledEdgeIsElse(t *testing.T) {
	miss := Object{"id": "yes", "logic": "and", "conditions": []any{
		Object{"variableSelector": []any{"s", "flag"}, "operator": "equals", "value": false},
	}}
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfConditionNode("c", []any{miss}), wfNode("y", "output", ""), wfNode("e", "output", "")},
		[][3]string{{"s", "c", ""}, {"c", "y", "yes"}, {"c", "e", ""}},
	))
	states := map[string]workflowNodeState{}
	states["s"] = executeWorkflowNode(graph.ByNode["s"], Object{"flag": true}, states, nil)

	if branch, err := evaluateWorkflowCondition(graph.ByNode["c"], states); err != nil || branch != workflowBranchElse {
		t.Fatalf("all-miss condition = %q, %v; want else", branch, err)
	}
	states["c"] = workflowNodeState{Status: "succeeded", Output: Object{"selectedBranch": workflowBranchElse}}

	p := projectWorkflowNodes(graph, states)
	if node, ok := p.readyNode(); !ok || node.ID != "e" {
		t.Fatalf("ready node = %q, %v; want unlabeled else edge e", node.ID, ok)
	}
	if p.projected["y"] != "inactive" {
		t.Fatalf("labeled branch y = %q; want inactive", p.projected["y"])
	}
}

// A node with no incoming edges never runs unless it is the start node (desktop branch_projection:
// rootless non-start nodes resolve inactive, so an authoring leftover cannot hijack execution).
func TestWorkflowProjectionRootlessNodeIsInactive(t *testing.T) {
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", ""), wfNode("x", "output", "orphan")},
		[][3]string{{"s", "o", ""}},
	))
	p := projectWorkflowNodes(graph, map[string]workflowNodeState{})
	if node, ok := p.readyNode(); !ok || node.ID != "s" {
		t.Fatalf("ready node = %q, %v; want start s", node.ID, ok)
	}
	if p.projected["x"] != "inactive" {
		t.Fatalf("rootless non-start x = %q; want inactive", p.projected["x"])
	}
}

// An upstream node without a committed result leaves downstream unresolved — not ready, not failed,
// not inactive (invariant 5: nothing resolves on missing state; the pass waits for the commit).
func TestWorkflowProjectionPendingUpstreamWaits(t *testing.T) {
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "")},
		[][3]string{{"s", "o", ""}},
	))
	states := map[string]workflowNodeState{"s": {Status: "running", DisplayName: "start"}}
	p := projectWorkflowNodes(graph, states)
	if node, ok := p.readyNode(); ok {
		t.Fatalf("downstream of a running node readied %q", node.ID)
	}
	if resolved, ok := p.projected["o"]; ok {
		t.Fatalf("downstream of a running node resolved as %q", resolved)
	}
}

// A failed node fails its downstream edges: the branch resolves failed (not ready, not inactive),
// and hasFailure — read from committed state — is true, which is what settles the run failed.
func TestWorkflowProjectionFailurePropagates(t *testing.T) {
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "")},
		[][3]string{{"s", "o", ""}},
	))
	states := map[string]workflowNodeState{
		"s": {Status: "failed", DisplayName: "start", Output: Object{"reason": "boom"}},
	}
	p := projectWorkflowNodes(graph, states)
	if p.projected["o"] != "failed" {
		t.Fatalf("downstream of a failed node = %q; want failed", p.projected["o"])
	}
	if !p.hasFailure() {
		t.Fatal("committed failure not reported")
	}
}

// A run's recorded failure reason must come from the first failure in node order, not from
// whichever failure the map happened to yield first: the states are a map, Go randomizes map
// iteration, and a reason that changes between two replays of the same trace is not evidence of
// anything. One iteration would pass half the time under the old implementation, so the loop turns
// that coin flip into a near-certain failure.
func TestWorkflowRunFailureReasonFollowsNodeOrder(t *testing.T) {
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("first", "agent", ""), wfNode("second", "agent", "")},
		nil,
	))
	states := map[string]workflowNodeState{
		"first":  {Status: "failed", Output: Object{"reason": "first failed"}},
		"second": {Status: "failed", Output: Object{"reason": "second failed"}},
	}
	for i := 0; i < 200; i++ {
		if reason := workflowRunFailureReason(graph, states); reason != "first failed" {
			t.Fatalf("iteration %d: reason = %q; want the first failure in node order", i, reason)
		}
	}
}

// A failed node that carries no reason of its own still has to say something, and a run whose only
// committed failures are non-failing states reports the generic phrase rather than an empty one.
func TestWorkflowRunFailureReasonFallbacks(t *testing.T) {
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("a", "agent", ""), wfNode("b", "agent", "")},
		nil,
	))
	if reason := workflowRunFailureReason(graph, map[string]workflowNodeState{
		"a": {Status: "failed"},
	}); reason != "node failed" {
		t.Fatalf("reasonless failure = %q; want node failed", reason)
	}
	if reason := workflowRunFailureReason(graph, map[string]workflowNodeState{
		"a": {Status: "succeeded", Output: Object{"reason": "not a failure"}},
	}); reason != "run failed" {
		t.Fatalf("no committed failure = %q; want run failed", reason)
	}
}

// wfRuleEvaluate runs one condition rule against a start node whose output the test authors
// directly, returning whether the rule matches and any evaluation error.
func wfRuleEvaluate(t *testing.T, selector []any, operator string, value any, output Object) (bool, error) {
	t.Helper()
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: output}}
	return workflowConditionRule(Object{"variableSelector": selector, "operator": operator, "value": value}, states)
}

// The operator table, evaluated against a start node whose output is
// {"flag": true, "count": 2, "text": "hello world", "list": [], "tags": ["a","b"],
// "nothing": null, "emptyObj": {}, "fullObj": {"a": 1}, "plainObj": {}}. The three object keys are
// deliberately three different Go types for the same JSON: a start node's output nests the run input
// as the package's named Object, while a jsonb-decoded row nests a plain map. Both must classify as
// empty the same way.
//
// The expected behavior comes from desktop's condition.rs: contains is substring-or-array-element,
// starts_with/ends_with require strings on both sides, numeric ordering requires two numbers, and
// every comparator other than exists/not_exists/empty/not_empty fails on an unassigned variable.
func TestWorkflowConditionOperators(t *testing.T) {
	output := Object{
		"flag": true, "count": 2.0, "text": "hello world", "list": []any{}, "tags": []any{"a", "b"},
		"nothing": nil, "emptyObj": Object{}, "fullObj": Object{"a": 1.0}, "plainObj": map[string]any{},
		"nested": Object{"tags": []any{"x"}}, "label": "n=2",
	}
	cases := []struct {
		name      string
		selector  []any
		operator  string
		value     any
		wantMatch bool
		wantErr   bool
	}{
		{"equals match", []any{"s", "flag"}, "equals", true, true, false},
		{"is is equals", []any{"s", "flag"}, "is", true, true, false},
		{"is_not is not_equals", []any{"s", "flag"}, "is_not", false, true, false},
		{"equals mismatch", []any{"s", "flag"}, "equals", false, false, false},
		{"equals null is a type error", []any{"s", "nothing"}, "equals", nil, false, true},
		{"not_equals mismatch", []any{"s", "flag"}, "not_equals", true, false, false},
		{"not_equals match", []any{"s", "flag"}, "not_equals", false, true, false},
		{"contains string match", []any{"s", "text"}, "contains", "lo wo", true, false},
		{"contains array element", []any{"s", "tags"}, "contains", "b", true, false},
		{"contains array miss", []any{"s", "tags"}, "contains", "z", false, false},
		{"contains nested array", []any{"s", "nested", "tags"}, "contains", "x", true, false},
		{"contains object is a type error", []any{"s", "fullObj"}, "contains", "a", false, true},
		{"contains non-string needle is a type error", []any{"s", "text"}, "contains", 1.0, false, true},
		{"not_contains match", []any{"s", "text"}, "not_contains", "zz", true, false},
		{"not_contains mismatch", []any{"s", "text"}, "not_contains", "hello", false, false},
		{"starts_with match", []any{"s", "text"}, "starts_with", "hello", true, false},
		{"ends_with match", []any{"s", "text"}, "ends_with", "world", true, false},
		{"starts_with non-string actual is a type error", []any{"s", "count"}, "starts_with", "2", false, true},
		{"starts_with non-string expected is a type error", []any{"s", "text"}, "starts_with", 2.0, false, true},
		{"ends_with non-string actual is a type error", []any{"s", "count"}, "ends_with", "2", false, true},
		{"ends_with non-string expected is a type error", []any{"s", "text"}, "ends_with", 2.0, false, true},
		{"empty on empty list", []any{"s", "list"}, "empty", nil, true, false},
		{"empty on null", []any{"s", "nothing"}, "empty", nil, true, false},
		{"not_empty on text", []any{"s", "text"}, "not_empty", nil, true, false},
		// An empty object is empty whichever Go type carries it. Before asWorkflowObject was used
		// here, the named Object fell through the type switch and an empty run input read as
		// not-empty, so a `start -> condition` graph on input {} took the opposite branch from
		// desktop's is_empty.
		{"empty on empty Object", []any{"s", "emptyObj"}, "empty", nil, true, false},
		{"not_empty on empty Object", []any{"s", "emptyObj"}, "not_empty", nil, false, false},
		{"not_empty on full Object", []any{"s", "fullObj"}, "not_empty", nil, true, false},
		{"empty on full Object", []any{"s", "fullObj"}, "empty", nil, false, false},
		{"empty on empty plain map", []any{"s", "plainObj"}, "empty", nil, true, false},
		{"exists on present", []any{"s", "flag"}, "exists", nil, true, false},
		{"not_exists on absent", []any{"s", "missing"}, "not_exists", nil, true, false},
		{"exists on absent", []any{"s", "missing"}, "exists", nil, false, false},
		{"empty on absent", []any{"s", "missing"}, "empty", nil, true, false},
		{"not_empty on absent", []any{"s", "missing"}, "not_empty", nil, false, false},
		// desktop's exists/not_exists/empty/not_empty are the only operators that answer absence;
		// every comparator, negated ones included, reports the unassigned variable instead of
		// silently choosing else.
		{"equals on absent is an error", []any{"s", "missing"}, "equals", nil, false, true},
		{"not_equals on absent is an error", []any{"s", "missing"}, "not_equals", nil, false, true},
		{"not_contains on absent is an error", []any{"s", "missing"}, "not_contains", "zz", false, true},
		{"greater_than on absent is an error", []any{"s", "missing"}, "greater_than", 1.0, false, true},
		{"invalid nested path is an error", []any{"s", "text", "deeper"}, "exists", nil, false, true},
		{"missing nested path is an error", []any{"s", "nested", "absent"}, "exists", nil, false, true},
		{"one-segment selector is an error", []any{"s"}, "exists", nil, false, true},
		{"greater_than match", []any{"s", "count"}, "greater_than", 1.0, true, false},
		{"greater_than no match", []any{"s", "count"}, "greater_than", 2.0, false, false},
		{"greater_than_or_equal boundary", []any{"s", "count"}, "greater_than_or_equal", 2.0, true, false},
		{"less_than match", []any{"s", "count"}, "less_than", 3.0, true, false},
		{"less_than_or_equal boundary", []any{"s", "count"}, "less_than_or_equal", 2.0, true, false},
		{"numeric against text is a type error", []any{"s", "text"}, "greater_than", 1.0, false, true},
		// Expected-value templates: exclusive keeps the bound JSON type, mixed renders a string, and
		// an unset or malformed selector fails rather than comparing the placeholder text.
		{"exclusive template keeps number", []any{"s", "count"}, "equals", "{{#s.count#}}", true, false},
		{"exclusive template keeps boolean", []any{"s", "flag"}, "equals", "{{#s.flag#}}", true, false},
		{"exclusive template numeric compare", []any{"s", "count"}, "greater_than", "{{#s.count#}}", false, false},
		{"mixed template renders string", []any{"s", "label"}, "equals", "n={{#s.count#}}", true, false},
		{"mixed template resolves text verbatim", []any{"s", "text"}, "contains", "{{#s.text#}}", true, false},
		{"template on unset variable is an error", []any{"s", "text"}, "equals", "{{#s.missing#}}", false, true},
		{"unterminated template is an error", []any{"s", "text"}, "equals", "{{#s.count", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wfRuleEvaluate(t, tc.selector, tc.operator, tc.value, output)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("rule %q %v returned %v; want an evaluation error", tc.operator, tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("rule %q %v: %v", tc.operator, tc.value, err)
			}
			if got != tc.wantMatch {
				t.Fatalf("rule %q %v got %v; want %v", tc.operator, tc.value, got, tc.wantMatch)
			}
		})
	}
}

// A rule with no expected value and one whose expected value is an explicit JSON null behave the
// same: desktop's WireConditionRule.value is Option<Value>, serde reads null as None, and a
// comparison operator reports the missing operand rather than comparing against null.
func TestWorkflowConditionMissingExpectedValue(t *testing.T) {
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"flag": true}}}
	rule := Object{"variableSelector": []any{"s", "flag"}, "operator": "equals"}
	if _, err := workflowConditionRule(rule, states); err == nil {
		t.Fatal("rule with no value matched; want a type-mismatch error")
	}
}

// Case logic: "and" requires every rule, "or" any rule, the first matching case wins; a case with no
// rules is an authored shell and falls through; a malformed rule, an unknown operator and an unknown
// logic are errors rather than silent else branches. An exists rule on a node the states do not
// contain matches nothing: the evaluator has no declaration set, so this is the runtime answer for
// snapshots frozen before the publish gate. A new publish naming that node is refused as
// condition_selector_undeclared instead.
func TestWorkflowConditionCaseLogic(t *testing.T) {
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"flag": true, "count": 2.0}}}
	pick := func(cases any) (string, error) {
		return evaluateWorkflowCondition(workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": cases}}, states)
	}
	rule := func(key string, value any) Object {
		return Object{"variableSelector": []any{"s", key}, "operator": "equals", "value": value}
	}
	and := []any{Object{"id": "both", "logic": "and", "conditions": []any{rule("flag", true), rule("count", 3.0)}}}
	if got, err := pick(and); err != nil || got != workflowBranchElse {
		t.Fatalf("and-case with one failed rule = %q, %v; want else", got, err)
	}
	or := []any{Object{"id": "either", "logic": "or", "conditions": []any{rule("flag", false), rule("count", 2.0)}}}
	if got, err := pick(or); err != nil || got != "either" {
		t.Fatalf("or-case with one matching rule = %q, %v; want either", got, err)
	}
	first := []any{
		Object{"id": "first", "logic": "or", "conditions": []any{rule("flag", true)}},
		Object{"id": "second", "logic": "or", "conditions": []any{rule("count", 2.0)}},
	}
	if got, err := pick(first); err != nil || got != "first" {
		t.Fatalf("first matching case = %q, %v; want first", got, err)
	}
	if got, err := pick([]any{Object{"id": "shell", "logic": "and"}}); err != nil || got != workflowBranchElse {
		t.Fatalf("case with no rules = %q, %v; want else", got, err)
	}
	malformed := []any{Object{"id": "bad", "logic": "or", "conditions": []any{Object{"operator": "equals"}}}}
	if _, err := pick(malformed); err == nil {
		t.Fatal("malformed rule selected a branch; want an error")
	}
	badOp := []any{Object{"id": "unk", "logic": "or", "conditions": []any{
		Object{"variableSelector": []any{"s", "flag"}, "operator": "frobnicate", "value": true},
	}}}
	if _, err := pick(badOp); err == nil {
		t.Fatal("unknown operator selected a branch; want an error")
	}
	badLogic := []any{Object{"id": "xor", "logic": "xor", "conditions": []any{rule("flag", true)}}}
	if _, err := pick(badLogic); err == nil {
		t.Fatal("unknown logic selected a branch; want an error")
	}
	// `nope` is not a node in the states. Cloud answers absent and the rule matches nothing; desktop
	// would call the selector Undeclared and fail the node. Snapshots frozen before the publish gate
	// still reach this path, so the runtime answer is pinned here; a new publish is refused instead.
	ghost := []any{Object{"id": "ghost", "logic": "or", "conditions": []any{
		Object{"variableSelector": []any{"nope", "flag"}, "operator": "exists", "value": nil},
	}}}
	if got, err := pick(ghost); err != nil || got != workflowBranchElse {
		t.Fatalf("exists rule on an undeclared node = %q, %v; want else", got, err)
	}
	// Desktop resolves every rule before combining them, so an unset variable in a later rule of an
	// already-matching or-case still fails instead of being short-circuited away.
	unsetLater := []any{Object{"id": "partial", "logic": "or", "conditions": []any{
		rule("flag", true),
		Object{"variableSelector": []any{"s", "missing"}, "operator": "equals", "value": 1.0},
	}}}
	if _, err := pick(unsetLater); err == nil {
		t.Fatal("or-case with an unset later rule selected a branch; want an error")
	}
}

// The start node's output is the variable pool desktop declares: `input` is the kickoff prompt and
// each run-form variable is its own root, so selector [start, name] resolves the value the author
// selected in the editor instead of walking the whole form under [start, input, name]. The run's
// submitted kickoff wins over the authored instruction; a run that named none falls back to the
// authored text (which is also what every run created before the kickoff was persisted has), and a
// graph with neither leaves the root unset. Every value is kept verbatim, whitespace included:
// desktop's start.input is the text as written, and trimming it here would let the same selector
// compare differently on the two sides.
func TestWorkflowStartOutputMatchesDesktopVariableRoots(t *testing.T) {
	startData := func(authored any) Object {
		data := Object{"kind": "start"}
		if authored != nil {
			data["input"] = authored
		}
		return data
	}
	cases := []struct {
		name  string
		data  Object
		input Object
		want  Object
	}{
		{
			name:  "the run's submitted kickoff wins",
			data:  startData("  review the diff  "),
			input: Object{"flag": true, "count": 2.0, "input": "run override"},
			want:  Object{"flag": true, "count": 2.0, "input": "run override"},
		},
		{
			// Historical rows carry no kickoff key; the authored instruction in the frozen snapshot
			// is what they executed with, so that is what their pool must keep resolving.
			name:  "a run with no submitted kickoff falls back to the authored prompt",
			data:  startData("  review the diff  "),
			input: Object{"flag": true, "count": 2.0},
			want:  Object{"flag": true, "count": 2.0, "input": "  review the diff  "},
		},
		{
			name:  "an explicit null override falls back to the authored prompt",
			data:  startData("review the diff"),
			input: Object{"input": nil},
			want:  Object{"input": "review the diff"},
		},
		{
			// An empty submitted kickoff is a value, not an absence: it must not silently become
			// the authored prompt the user cleared.
			name:  "an empty submitted kickoff is kept",
			data:  startData("review the diff"),
			input: Object{"input": ""},
			want:  Object{"input": ""},
		},
		{
			// Desktop leaves {start}.input unset when the run supplied no kickoff and the graph has
			// no instruction, so exists/not_exists must see the same absence here.
			name:  "neither source leaves the root unset",
			data:  startData(nil),
			input: Object{"flag": true},
			want:  Object{"flag": true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := workflowStartOutput(workflowNodeView{ID: "s", Kind: "start", Data: tc.data}, tc.input)
			if canonicalJSON(output) != canonicalJSON(tc.want) {
				t.Fatalf("start output = %s; want %s", canonicalJSON(output), canonicalJSON(tc.want))
			}
		})
	}
}

// TestWorkflowConditionReadsRuntimeGlobals runs a condition against a desktop-exported graph that
// carries globalVariables: the runtime-owned sys.* pair and a frozen user global must resolve the
// selectors (no more condition_selector_undeclared), and none of the ambient pseudo-nodes may leak
// into node_states.
func TestWorkflowConditionReadsRuntimeGlobals(t *testing.T) {
	run := Object{"workflowId": "wf-1", "createdAtMs": int64(1700000000000)}
	yes := Object{"id": "yes", "logic": "and", "conditions": []any{
		Object{"variableSelector": []any{"sys", "workflow_id"}, "operator": "equals", "value": "wf-1"},
		Object{"variableSelector": []any{"sys", "timestamp"}, "operator": "equals", "value": float64(1700000000000)},
		Object{"variableSelector": []any{"g", "region"}, "operator": "equals", "value": "eu"},
	}}
	graphDoc := wfGraph(
		[]Object{wfNode("s", "start", ""), wfConditionNode("c", []any{yes}), wfNode("o", "output", "")},
		[][3]string{{"s", "c", ""}, {"c", "o", "yes"}},
	)
	// Desktop exports globals beside nodes/edges: the name, its declared type and the value frozen
	// with the snapshot.
	graphDoc["globalVariables"] = []any{
		Object{"name": "g.region", "valueType": "string", "value": "eu"},
	}
	graph := readWorkflowGraph(graphDoc)

	ambient := workflowAmbientStates(run, graphDoc)
	if got := canonicalJSON(ambient["sys"].Output); got != canonicalJSON(Object{"workflow_id": "wf-1", "timestamp": float64(1700000000000)}) {
		t.Fatalf("sys ambient = %s", got)
	}
	if got := canonicalJSON(ambient["g"].Output); got != canonicalJSON(Object{"region": "eu"}) {
		t.Fatalf("user global ambient = %s", got)
	}

	states := map[string]workflowNodeState{}
	states["s"] = executeWorkflowNode(graph.ByNode["s"], nil, states, ambient)
	state := executeWorkflowNode(graph.ByNode["c"], nil, states, ambient)
	if state.Status != "succeeded" || state.Output.S("selectedBranch") != "yes" {
		t.Fatalf("condition state = %v; want selectedBranch yes", state.object())
	}
	states["c"] = state
	for id := range states {
		if id == "sys" || id == "g" {
			t.Fatalf("ambient root %q leaked into node_states", id)
		}
	}
}
