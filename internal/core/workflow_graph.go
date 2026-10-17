package core

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Canonical workflow graph validation: the server-executable subset of the graph schema specs owns.
//
// The editor owns the document and specs owns the canonical schema; Cloud owns exactly the subset it
// can be asked to execute, and this file is that subset written down once. The publish gate, the run
// admission gate and the @ form's schema projection all read it, so a graph Cloud publishes is a
// graph Cloud has agreed to run, and the reason a graph is refused names the node or slot at fault
// instead of silently ignoring the part Cloud does not understand.
//
// Only publishing validates the graph. Saving is deliberately unchecked: an unfinished draft is a
// legitimate document — the author is mid-edit and the workflow is not runnable yet — so create and
// update accept any JSON object and only publish asks whether the document is complete. That split is
// the whole point of the draft/published distinction, and moving an install or completeness check
// into the save path is the mistake this file exists to prevent.

// workflowExecutableNodeKinds is the subset of the editor's node kinds Cloud executes in its first
// phase (#67 结论 4): start, agent, condition and output. Everything else stays editable and
// saveable, but publishing refuses it rather than freezing a graph whose nodes would be dropped.
var workflowExecutableNodeKinds = map[string]bool{
	"start":     true,
	"agent":     true,
	"condition": true,
	"output":    true,
}

// workflowUnsupportedAgentConfigFields are the agent-node configuration fields the first phase does
// not execute (#67 D2). The schema knows all of them and the editor may write them, but nothing in
// Cloud reads them, so publishing refuses them by name instead of freezing a document whose author
// believes a setting is in effect when no runner will ever apply it.
//
// They are listed rather than derived so the refusal stays a decision about *this* field, and in this
// order so the problem list a client renders is deterministic. `executorSlot`, `roleId` and `prompt`
// are the first phase's agent configuration and are deliberately absent here; the pre-slot
// `agentConfig.executor` is absent too, because specs#98 D5 makes it a field to accept and drop
// rather than refuse.
var workflowUnsupportedAgentConfigFields = []string{
	"skills",
	"mcps",
	"interactive",
	"outputContract",
	"retry",
	"promptInactivity",
}

// workflowDefaultSlot is the executor slot every graph may reference without declaring it, and the
// slot an agent node uses when it names none (specs#98 D2). It is reserved: a graph may not declare
// it, because declaring it would let an author redefine the one name every runner already has.
const workflowDefaultSlot = "default"

// workflowSlotNamePattern is the grammar of an executor slot name (specs#98 D1). The name is the key
// a runner's personal mapping is looked up by, so it stays a stable, easily compared identifier: a
// name with case or whitespace variants would look configured and still miss.
var workflowSlotNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Bounds on the author-written slot description, which is the only thing a runner sees when deciding
// what to map the slot to (specs#98 D1).
//
// They count characters, not bytes: the description is written for a human and D1 says so explicitly,
// and a byte count is a different limit in every language — 500 bytes is 500 Latin letters but about
// 166 Chinese ones, so a byte count would refuse a description the decision allows. Rune count is the
// unit `gitIdentityName` already uses for the same reason.
const (
	minSlotDescription = 1
	maxSlotDescription = 500
)

// maxSlotHintField bounds one `hint` string. specs#98 D3 fixes the hint's shape but no length, so this
// is Cloud's own ceiling on a value it only ever displays — kept, and counted in characters for the
// same reason the description is: a model id is an opaque string that may be written in any script,
// and a byte count would bound the Chinese ones four times tighter for no reason anyone could name.
const maxSlotHintField = 200

// workflowGraphProblem is one canonical-schema violation: a stable code plus the safe parameters that
// say where it is. It carries no prose because the wire contract is codes and parameters — a client
// localizes them, and a message written here would be a second, untranslated source of truth.
type workflowGraphProblem struct {
	Code   string
	NodeID string
	Slot   string
	Kind   string
	Field  string
}

// Object renders one problem for the fault's `problems` list, omitting the fields that do not apply.
// It takes its receiver by pointer because a problem is five strings and is rendered in a loop.
func (p *workflowGraphProblem) Object() Object {
	out := Object{"code": p.Code}
	if p.NodeID != "" {
		out["nodeId"] = p.NodeID
	}
	if p.Slot != "" {
		out["slot"] = p.Slot
	}
	if p.Kind != "" {
		out["kind"] = p.Kind
	}
	if p.Field != "" {
		out["field"] = p.Field
	}
	return out
}

// workflowNodes reads the graph's node list. The graph is a document the API never interprets, so a
// document whose nodes are not a list contributes no nodes rather than failing the read; publishing is
// what turns that into a problem.
func workflowNodes(graph Object) []any {
	nodes, _ := graph["nodes"].([]any)
	return nodes
}

// validateWorkflowGraph reports every way a graph violates the canonical schema, in document order so
// a client can render them next to the nodes they name. It is pure and total: a graph it cannot
// interpret produces problems rather than an error, because a malformed document is exactly what it
// is here to describe.
func validateWorkflowGraph(graph Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	nodes, ok := graph["nodes"].([]any)
	if !ok {
		problems = append(problems, workflowGraphProblem{Code: "nodes_not_a_list"})
		nodes = nil
	}
	if _, ok := graph["edges"].([]any); !ok {
		problems = append(problems, workflowGraphProblem{Code: "edges_not_a_list"})
	}
	problems = append(problems, validateWorkflowNodes(nodes)...)
	problems = append(problems, validateWorkflowEdges(graph, nodes)...)
	problems = append(problems, validateWorkflowAcyclic(graph)...)
	problems = append(problems, validateWorkflowSlots(graph, nodes)...)
	return problems
}

// validateWorkflowAcyclic refuses a graph that contains a cycle. The first phase executes a DAG and
// nothing else (#67 结论 4, D1 条件④): the projection that decides what runs is a topological read of
// committed state, so a node on a cycle either waits for an upstream that waits for it or goes round
// once and drops its back edge, and neither is what its author drew. Publishing would freeze a
// document whose run cannot mean what it says, which is exactly what this gate exists to prevent.
// Loops and iterations are not part of the first phase, so a graph that wants one is refused rather
// than executed as something else.
//
// The reported node is the source of the back edge the walk found, which is on a cycle by
// construction; a node merely downstream of one is not reported, because the author has nothing to
// fix there. The walk itself is shared with the run's advance pass — see workflowGraphCycle — so a
// graph Cloud refuses to publish and a graph Cloud refuses to run are the same graph.
//
// The graph is read by the same reader the run uses, so a document whose nodes are not a list is
// simply empty here: validateWorkflowNodes has already said so, and this pass must not pile a second,
// less useful complaint on top of it.
func validateWorkflowAcyclic(graph Object) []workflowGraphProblem {
	source := workflowGraphCycle(readWorkflowGraph(graph))
	if source == "" {
		return nil
	}
	return []workflowGraphProblem{{Code: "graph_cycle", NodeID: source}}
}

// validateWorkflowNodes checks node identity, kind, agent configuration and the start-node count. The
// editor allows at most one Start node, and a run needs exactly one entry point, so zero and two are
// both refused: a graph with none is unfinished, and one with two has no defined place to begin.
func validateWorkflowNodes(nodes []any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	seen := map[string]bool{}
	starts := 0
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "node_not_an_object"})
			continue
		}
		o := Object(node)
		id := o.S("id")
		if id == "" {
			problems = append(problems, workflowGraphProblem{Code: "node_id_missing"})
			continue
		}
		if seen[id] {
			problems = append(problems, workflowGraphProblem{Code: "node_id_duplicate", NodeID: id})
			continue
		}
		seen[id] = true
		kind := o.O("data").S("kind")
		switch {
		case kind == "":
			problems = append(problems, workflowGraphProblem{Code: "node_kind_missing", NodeID: id})
		case !workflowExecutableNodeKinds[kind]:
			problems = append(problems, workflowGraphProblem{Code: "node_kind_unsupported", NodeID: id, Kind: kind})
		case kind == "start":
			starts++
		case kind == "agent":
			// Only an agent node is asked about its configuration, and only once its kind is known to
			// be one Cloud executes: a `tool` node carrying `skills` is already refused for the kind,
			// and reporting its config too would ask the author to fix a field of a node they have to
			// delete anyway.
			problems = append(problems, validateAgentConfig(id, o.O("data"))...)
		case kind == "condition":
			problems = append(problems, validateConditionConfig(id, o.O("data"))...)
		}
	}
	if starts != 1 {
		problems = append(problems, workflowGraphProblem{Code: "start_node_count"})
	}
	return problems
}

// validateAgentConfig refuses the agent-node configuration the first phase does not execute, naming
// both the node and the field (#67 D2). The alternative — ignoring the field — is the one outcome the
// decision rules out: an author who set `retry` would read a publish that succeeded as "my retry
// policy is in effect", and nothing would ever apply it.
//
// A key present with a JSON null is not a refusal. The decision's reason for refusing these fields is
// that they change execution behavior, and a null changes none; refusing it would be the same
// mistake in the other direction, rejecting a document for a value that asks for nothing. The same
// reading is what `validSlotHint` applies to an absent-or-null `hint`.
func validateAgentConfig(nodeID string, data Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	config := data.O("agentConfig")
	for _, field := range workflowUnsupportedAgentConfigFields {
		value, present := config[field]
		if !present || value == nil {
			continue
		}
		problems = append(problems, workflowGraphProblem{Code: "node_config_unsupported", NodeID: nodeID, Field: field})
	}
	return problems
}

// workflowUnsupportedConditionConfigs is the list of condition-node configuration Cloud cannot
// evaluate the way desktop's condition.rs does, and which publishing therefore refuses. It is
// written out rather than derived because it is a statement about the two implementations, not about
// Cloud's own code: each entry is a place where the evaluator would otherwise answer silently —
// an unknown operator is a no-match, an unknown logic falls back to `and`, a missing case id becomes
// the else branch, a one-segment selector becomes the whole node output, and an expected value
// carrying `{{#selector#}}` is compared as literal text where desktop resolves it and keeps the
// bound type (circus#202 A 条件 2, #141 结论 5). Every one of them is decidable from the document
// alone, which is what makes refusing at publish possible; the differences that depend on a
// *resolved* value's type at run time are not, and are named in the ADR instead.
//
// A condition node with no cases, or a case with no rules, is deliberately NOT on this list: falling
// through to else is what an unfinished node is meant to do, and both sides already agree on it.
//
// When the parity work lands (circus#210, split out by circus#202 A 条件 3) these refusals come off
// one at a time, each with the cross-check case that proves the two sides now answer the same thing.
var workflowUnsupportedConditionConfigs = []string{
	"condition_cases_not_a_list",
	"condition_case_not_an_object",
	"condition_case_id_missing",
	"condition_logic_unsupported",
	"condition_rules_not_a_list",
	"condition_rule_not_an_object",
	"condition_selector_unsupported",
	"condition_operator_unsupported",
	"condition_value_template_unsupported",
}

// validateConditionConfig refuses the condition configuration the first phase does not evaluate the
// way desktop does. Every problem names the node and the field, so the author is told which branch
// of which node to fix rather than that the graph is invalid somewhere.
func validateConditionConfig(nodeID string, data Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	raw, present := data["cases"]
	if !present || raw == nil {
		return problems
	}
	cases, ok := raw.([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "condition_cases_not_a_list", NodeID: nodeID, Field: "cases"})
	}
	for _, item := range cases {
		c, ok := asWorkflowObject(item)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "condition_case_not_an_object", NodeID: nodeID, Field: "cases"})
			continue
		}
		if c.S("id") == "" {
			problems = append(problems, workflowGraphProblem{Code: "condition_case_id_missing", NodeID: nodeID, Field: "cases.id"})
		}
		// An absent logic is `and`, which is what the evaluator applies and what the editor omits;
		// only a value neither side recognizes is refused.
		switch c.S("logic") {
		case "", "and", "or":
		default:
			problems = append(problems, workflowGraphProblem{Code: "condition_logic_unsupported", NodeID: nodeID, Field: "cases.logic"})
		}
		rules, ok := c["conditions"].([]any)
		if !ok {
			// An absent or null rule list is the same unfinished case as an empty one, and is left
			// to fall through; a list of some other type is a document this gate cannot read.
			if value, present := c["conditions"]; present && value != nil {
				problems = append(problems, workflowGraphProblem{Code: "condition_rules_not_a_list", NodeID: nodeID, Field: "cases.conditions"})
			}
			continue
		}
		for _, rawRule := range rules {
			problems = append(problems, validateConditionRule(nodeID, rawRule)...)
		}
	}
	return problems
}

// validateConditionRule checks one rule of one case. The operator set is read from the evaluator's
// own table rather than restated, so the gate cannot admit an operator the evaluator would then
// no-match — the two lists are the same list.
func validateConditionRule(nodeID string, raw any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	r, ok := asWorkflowObject(raw)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "condition_rule_not_an_object", NodeID: nodeID, Field: "cases.conditions"})
	}
	selector, ok := r["variableSelector"].([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "condition_selector_unsupported", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
	}
	// The selector needs the node AND a path into its output. The editor hands a new comparison an
	// empty list, and one element names the whole output, which desktop refuses and Cloud would read
	// as everything the node produced.
	if len(selector) < 2 {
		problems = append(problems, workflowGraphProblem{Code: "condition_selector_unsupported", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
	}
	for _, element := range selector {
		if text, ok := element.(string); !ok || text == "" {
			problems = append(problems, workflowGraphProblem{Code: "condition_selector_unsupported", NodeID: nodeID, Field: "cases.conditions.variableSelector"})
			break
		}
	}
	if _, ok := workflowConditionOperators[r.S("operator")]; !ok {
		problems = append(problems, workflowGraphProblem{Code: "condition_operator_unsupported", NodeID: nodeID, Field: "cases.conditions.operator"})
	}
	if text, ok := r["value"].(string); ok && strings.Contains(text, "{{#") && strings.Contains(text, "#}}") {
		problems = append(problems, workflowGraphProblem{Code: "condition_value_template_unsupported", NodeID: nodeID, Field: "cases.conditions.value"})
	}
	return problems
}

// validateWorkflowEdges checks that every edge joins two nodes that exist. An edge to a node the
// document does not contain is a dangling reference the run would have to guess at, and the editor
// only produces one when the document was edited outside it.
func validateWorkflowEdges(graph Object, nodes []any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	edges, _ := graph["edges"].([]any)
	known := map[string]bool{}
	for _, raw := range nodes {
		if node, ok := raw.(map[string]any); ok {
			if id := Object(node).S("id"); id != "" {
				known[id] = true
			}
		}
	}
	for _, raw := range edges {
		edge, ok := raw.(map[string]any)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "edge_not_an_object"})
			continue
		}
		o := Object(edge)
		for _, endpoint := range []string{o.S("source"), o.S("target")} {
			if endpoint == "" || !known[endpoint] {
				problems = append(problems, workflowGraphProblem{Code: "edge_endpoint_unknown", NodeID: endpoint})
			}
		}
	}
	return problems
}

// validateWorkflowSlots checks the graph's executor-slot declarations and the agent nodes' references
// to them (specs#98 D1/D2). A reference to an undeclared slot is refused here rather than at run
// time: the author is the only one who can say what the slot means, and a run created against an
// undeclared slot could never resolve an executor for it.
//
// A declared slot no node references is *not* a problem: an author may be mid-edit or keeping one for
// later, and refusing it would block publishing for a mistake with no runtime consequence.
func validateWorkflowSlots(graph Object, nodes []any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	declared := map[string]bool{}
	for _, slot := range workflowSlotDeclarations(graph) {
		name := slot.S("name")
		switch {
		case name == "":
			problems = append(problems, workflowGraphProblem{Code: "slot_name_missing"})
		case !workflowSlotNamePattern.MatchString(name):
			problems = append(problems, workflowGraphProblem{Code: "slot_name_invalid", Slot: name})
		case name == workflowDefaultSlot:
			problems = append(problems, workflowGraphProblem{Code: "slot_name_reserved", Slot: name})
		case declared[name]:
			problems = append(problems, workflowGraphProblem{Code: "slot_name_duplicate", Slot: name})
		}
		if name != "" {
			declared[name] = true
		}
		description := strings.TrimSpace(slot.S("description"))
		if n := utf8.RuneCountInString(description); n < minSlotDescription || n > maxSlotDescription {
			problems = append(problems, workflowGraphProblem{Code: "slot_description_invalid", Slot: name})
		}
		if hint, ok := slot["hint"]; ok && hint != nil && !validSlotHint(hint) {
			problems = append(problems, workflowGraphProblem{Code: "slot_hint_invalid", Slot: name})
		}
	}
	for _, reference := range workflowReferencedSlots(nodes) {
		if reference.Name == workflowDefaultSlot || declared[reference.Name] {
			continue
		}
		for _, nodeID := range reference.Nodes {
			problems = append(problems, workflowGraphProblem{Code: "slot_not_declared", NodeID: nodeID, Slot: reference.Name})
		}
	}
	return problems
}

// validSlotHint reports whether a hint is the shape specs#98 D3 declares: an object whose `agentCli`
// and `modelId`, when present, are non-empty strings. A hint never resolves an executor, so it is
// checked only far enough to keep the document well formed for the runner that reads it.
func validSlotHint(hint any) bool {
	h, ok := asWorkflowObject(hint)
	if !ok {
		return false
	}
	for _, key := range []string{"agentCli", "modelId"} {
		value, present := h[key]
		if !present || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" || utf8.RuneCountInString(text) > maxSlotHintField {
			return false
		}
	}
	return true
}

// workflowSlotDeclarations reads the graph's executor-slot declarations. The graph is a document the
// API never interprets, so an entry that is not an object contributes nothing rather than failing the
// request; publishing is what turns the rest into problems.
func workflowSlotDeclarations(graph Object) []Object {
	declared, _ := graph["executorSlots"].([]any)
	out := []Object{}
	for _, item := range declared {
		if slot, ok := asWorkflowObject(item); ok {
			out = append(out, slot)
		}
	}
	return out
}

// workflowSlotReference is one executor slot an agent node asks for, together with the nodes that ask
// for it. Both travel together because every diagnostic and every runner-facing message has to name
// the slot *and* the node: "strong is not configured" is only actionable once the author knows which
// step depends on it.
type workflowSlotReference struct {
	Name  string
	Nodes []string
}

// workflowReferencedSlots collects the executor slots a graph's agent nodes reference, in the order
// the nodes appear so a message lists them the way the canvas draws them. An agent node that names no
// slot references `default` (specs#98 D2), which is also how a node still carrying the pre-slot
// `agentConfig.executor` field is read: cloud does not migrate its stored documents, so an old graph
// keeps running as the one-slot graph it always was (specs#98 D5).
//
// The node list is a slice because a node id may be repeated in a malformed document, and an entry
// with no id is dropped: an unnamed node cannot be pointed at in a message.
func workflowReferencedSlots(nodes []any) []workflowSlotReference {
	index := map[string]int{}
	out := []workflowSlotReference{}
	for _, raw := range nodes {
		o, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		data := o.O("data")
		if data.S("kind") != "agent" {
			continue
		}
		id := o.S("id")
		if id == "" {
			continue
		}
		name := data.O("agentConfig").S("executorSlot")
		if name == "" {
			name = workflowDefaultSlot
		}
		if i, ok := index[name]; ok {
			out[i].Nodes = append(out[i].Nodes, id)
			continue
		}
		index[name] = len(out)
		out = append(out, workflowSlotReference{Name: name, Nodes: []string{id}})
	}
	return out
}

// requireValidWorkflowGraph refuses a publish whose graph violates the canonical schema. The
// violations travel as parameters so the editor can mark each node, and the fault stays one stable
// code so a client cannot accidentally treat one kind of violation as another.
func requireValidWorkflowGraph(graph Object) {
	problems := validateWorkflowGraph(graph)
	if len(problems) == 0 {
		return
	}
	list := make([]any, 0, len(problems))
	for _, problem := range problems {
		list = append(list, problem.Object())
	}
	rejectWith(400, "workflow_graph_invalid", Object{"problems": list})
}
