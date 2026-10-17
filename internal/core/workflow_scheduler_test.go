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
	states["s"] = executeWorkflowNode(node, Object{"flag": true}, states)

	p = projectWorkflowNodes(graph, states)
	node, ok = p.readyNode()
	if !ok || node.ID != "o" {
		t.Fatalf("second ready node = %q, %v; want output o", node.ID, ok)
	}
	states["o"] = executeWorkflowNode(node, nil, states)

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
		Object{"variableSelector": []any{"s", "input", "flag"}, "operator": "equals", "value": true},
	}}
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfConditionNode("c", []any{yes}), wfNode("y", "output", ""), wfNode("e", "output", "")},
		[][3]string{{"s", "c", ""}, {"c", "y", "yes"}, {"c", "e", "else"}},
	))
	states := map[string]workflowNodeState{}
	states["s"] = executeWorkflowNode(graph.ByNode["s"], Object{"flag": true}, states)

	branch := evaluateWorkflowCondition(graph.ByNode["c"], states)
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
		Object{"variableSelector": []any{"s", "input", "flag"}, "operator": "equals", "value": false},
	}}
	graph := readWorkflowGraph(wfGraph(
		[]Object{wfNode("s", "start", ""), wfConditionNode("c", []any{miss}), wfNode("y", "output", ""), wfNode("e", "output", "")},
		[][3]string{{"s", "c", ""}, {"c", "y", "yes"}, {"c", "e", ""}},
	))
	states := map[string]workflowNodeState{}
	states["s"] = executeWorkflowNode(graph.ByNode["s"], Object{"flag": true}, states)

	if branch := evaluateWorkflowCondition(graph.ByNode["c"], states); branch != workflowBranchElse {
		t.Fatalf("all-miss condition selected %q; want else", branch)
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
// directly, returning whether the rule matches.
func wfRuleEvaluate(t *testing.T, selector []any, operator string, value any, output Object) bool {
	t.Helper()
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: output}}
	return workflowConditionRule(Object{"variableSelector": selector, "operator": operator, "value": value}, states)
}

// The operator table, evaluated against a start node whose output is
// {"flag": true, "count": 2, "text": "hello world", "list": [], "nothing": null, "emptyObj": {},
// "fullObj": {"a": 1}, "plainObj": {}}. The three object keys are deliberately three different Go
// types for the same JSON: a start node's output nests the run input as the package's named Object,
// while a jsonb-decoded row nests a plain map. Both must classify as empty the same way.
func TestWorkflowConditionOperators(t *testing.T) {
	output := Object{
		"flag": true, "count": 2.0, "text": "hello world", "list": []any{}, "nothing": nil,
		"emptyObj": Object{}, "fullObj": Object{"a": 1.0}, "plainObj": map[string]any{},
	}
	cases := []struct {
		name      string
		selector  []any
		operator  string
		value     any
		wantMatch bool
	}{
		{"equals match", []any{"s", "flag"}, "equals", true, true},
		{"is is equals", []any{"s", "flag"}, "is", true, true},
		{"equals mismatch", []any{"s", "flag"}, "equals", false, false},
		{"not_equals mismatch", []any{"s", "flag"}, "not_equals", true, false},
		{"not_equals match", []any{"s", "flag"}, "not_equals", false, true},
		{"contains match", []any{"s", "text"}, "contains", "lo wo", true},
		{"not_contains match", []any{"s", "text"}, "not_contains", "zz", true},
		{"starts_with match", []any{"s", "text"}, "starts_with", "hello", true},
		{"ends_with match", []any{"s", "text"}, "ends_with", "world", true},
		{"empty on empty list", []any{"s", "list"}, "empty", nil, true},
		{"empty on null", []any{"s", "nothing"}, "empty", nil, true},
		{"not_empty on text", []any{"s", "text"}, "not_empty", nil, true},
		// An empty object is empty whichever Go type carries it. Before asWorkflowObject was used
		// here, the named Object fell through the type switch and an empty run input read as
		// not-empty, so a `start -> condition` graph on input {} took the opposite branch from
		// desktop's is_empty.
		{"empty on empty Object", []any{"s", "emptyObj"}, "empty", nil, true},
		{"not_empty on empty Object", []any{"s", "emptyObj"}, "not_empty", nil, false},
		{"not_empty on full Object", []any{"s", "fullObj"}, "not_empty", nil, true},
		{"empty on full Object", []any{"s", "fullObj"}, "empty", nil, false},
		{"empty on empty plain map", []any{"s", "plainObj"}, "empty", nil, true},
		{"exists on present", []any{"s", "flag"}, "exists", nil, true},
		{"not_exists on absent", []any{"s", "missing"}, "not_exists", nil, true},
		{"exists on absent", []any{"s", "missing"}, "exists", nil, false},
		{"empty on absent", []any{"s", "missing"}, "empty", nil, true},
		{"not_empty on absent", []any{"s", "missing"}, "not_empty", nil, false},
		{"comparator on absent", []any{"s", "missing"}, "equals", nil, false},
		// Absence is a no-match for every comparator, negated ones included: not_contains must not
		// select its branch on a variable no prior node ever produced, or it disagrees with
		// not_equals about the same absent variable.
		{"not_contains on absent", []any{"s", "missing"}, "not_contains", "zz", false},
		{"not_contains mismatch", []any{"s", "text"}, "not_contains", "hello", false},
		{"greater_than match", []any{"s", "count"}, "greater_than", 1.0, true},
		{"greater_than no match", []any{"s", "count"}, "greater_than", 2.0, false},
		{"greater_than_or_equal boundary", []any{"s", "count"}, "greater_than_or_equal", 2.0, true},
		{"less_than match", []any{"s", "count"}, "less_than", 3.0, true},
		{"less_than_or_equal boundary", []any{"s", "count"}, "less_than_or_equal", 2.0, true},
		{"numeric against text", []any{"s", "text"}, "greater_than", 1.0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wfRuleEvaluate(t, tc.selector, tc.operator, tc.value, output)
			if got != tc.wantMatch {
				t.Fatalf("rule %q %v got %v; want %v", tc.operator, tc.value, got, tc.wantMatch)
			}
		})
	}
}

// Case logic: "and" requires every rule, "or" any rule, the first matching case wins; a case with
// no rules is an authored shell and falls through; a malformed rule, an unknown operator and a
// selector naming a node that never produced output are all no-matches.
func TestWorkflowConditionCaseLogic(t *testing.T) {
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"flag": true, "count": 2.0}}}
	pick := func(cases any) string {
		return evaluateWorkflowCondition(workflowNodeView{ID: "c", Kind: "condition", Data: Object{"cases": cases}}, states)
	}
	rule := func(key string, value any) Object {
		return Object{"variableSelector": []any{"s", key}, "operator": "equals", "value": value}
	}
	and := []any{Object{"id": "both", "logic": "and", "conditions": []any{rule("flag", true), rule("count", 3.0)}}}
	if got := pick(and); got != workflowBranchElse {
		t.Fatalf("and-case with one failed rule selected %q; want else", got)
	}
	or := []any{Object{"id": "either", "logic": "or", "conditions": []any{rule("flag", false), rule("count", 2.0)}}}
	if got := pick(or); got != "either" {
		t.Fatalf("or-case with one matching rule selected %q; want either", got)
	}
	first := []any{
		Object{"id": "first", "logic": "or", "conditions": []any{rule("flag", true)}},
		Object{"id": "second", "logic": "or", "conditions": []any{rule("count", 2.0)}},
	}
	if got := pick(first); got != "first" {
		t.Fatalf("first matching case selected %q; want first", got)
	}
	if got := pick([]any{Object{"id": "shell", "logic": "and"}}); got != workflowBranchElse {
		t.Fatalf("case with no rules selected %q; want else", got)
	}
	malformed := []any{Object{"id": "bad", "logic": "or", "conditions": []any{Object{"operator": "equals"}}}}
	if got := pick(malformed); got != workflowBranchElse {
		t.Fatalf("malformed rule selected %q; want else", got)
	}
	badOp := []any{Object{"id": "unk", "logic": "or", "conditions": []any{
		Object{"variableSelector": []any{"s", "flag"}, "operator": "frobnicate", "value": true},
	}}}
	if got := pick(badOp); got != workflowBranchElse {
		t.Fatalf("unknown operator selected %q; want else", got)
	}
	ghost := []any{Object{"id": "ghost", "logic": "or", "conditions": []any{
		Object{"variableSelector": []any{"nope", "flag"}, "operator": "exists", "value": nil},
	}}}
	if got := pick(ghost); got != workflowBranchElse {
		t.Fatalf("rule on a node that never ran selected %q; want else", got)
	}
}
