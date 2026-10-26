package core

import (
	"math"
	"strings"
)

// Loop containers are the first composite nodes Cloud executes
// (specs decisions/cloud/workflow/20261025-loop-and-loop-exit-execution.md). This file owns the
// structural rules that make a loop a loop: its body is a separate DAG whose membership is declared by
// `data.containerId`, only the body may read the loop's carried variables, and only the outer graph may
// read its published outputs. The publish gate and the scheduler read these rules, so a graph Cloud
// publishes is a graph Cloud can run.
//
// A loop's body start is a second `start` node in the document. Every reader that used to assume one
// flat graph (the run's launcher fields, the reserved-field walk, the start-node count) therefore has
// to look at the outer scope only; the helpers below are the one place that distinction is made.

const (
	// workflowLoopMinIterations and workflowLoopMaxIterations mirror desktop's 1..=100 ceiling
	// (desktop `loop_config.rs`), so a loop Cloud accepts can produce the same round budget.
	workflowLoopMinIterations = 1
	workflowLoopMaxIterations = 100
)

// workflowLoopRoots separates the two root groups a loop publishes, because their visibility differs.
// A carried variable is visible only inside the loop's body; a named output only outside it. Keeping
// them apart is what lets the gate refuse `[loop, draft]` from the outer graph and `[loop, result]`
// from the body without knowing what the runtime did.
type workflowLoopRoots struct {
	carried map[string]bool
	outputs map[string]bool
}

// workflowNodeContainers maps every node id to the id of the container it belongs to, or "" for the
// outer graph. A `containerId` naming a node that does not exist is still returned: the structure
// validator reports it rather than silently reading the node as outer.
func workflowNodeContainers(nodes []any) map[string]string {
	containers := make(map[string]string, len(nodes))
	for _, raw := range nodes {
		node, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		if id := node.S("id"); id != "" {
			containers[id] = workflowContainerID(node)
		}
	}
	return containers
}

// workflowLoopRootsByName reads each loop's declared carried and output names. It reads names only,
// so a malformed config still declares nothing here while validateLoopConfig reports the shape error;
// the two functions must not both report the same fault.
func workflowLoopRootsByName(nodes []any) map[string]workflowLoopRoots {
	out := map[string]workflowLoopRoots{}
	for _, raw := range nodes {
		node, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		data := node.O("data")
		if data.S("kind") != "loop" {
			continue
		}
		id := node.S("id")
		if id == "" {
			continue
		}
		config := data.O("loopConfig")
		roots := workflowLoopRoots{carried: map[string]bool{}, outputs: map[string]bool{}}
		for _, variable := range workflowObjectEntries(config["variables"]) {
			if name := variable.S("name"); name != "" {
				roots.carried[name] = true
			}
		}
		for _, output := range workflowObjectEntries(config["outputs"]) {
			if name := output.S("name"); name != "" {
				roots.outputs[name] = true
			}
		}
		out[id] = roots
	}
	return out
}

// workflowVisibleRoots returns the declared roots visible to a node in containerID, copying the base
// map so the caller's declarations are never mutated. A loop's carried roots are removed outside its
// body, and its named outputs are removed inside it: those are the two roots the other scope cannot
// read. Selectors are validated against this projection rather than the flat declaration set, so an
// outer selector on a carried variable is refused the same way desktop's Undeclared pool answers.
func workflowVisibleRoots(
	declared map[string]map[string]bool,
	loops map[string]workflowLoopRoots,
	containerID string,
) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(declared))
	for writer, roots := range declared {
		set := make(map[string]bool, len(roots))
		for root := range roots {
			set[root] = true
		}
		out[writer] = set
	}
	for loopID, groups := range loops {
		set, ok := out[loopID]
		if !ok {
			continue
		}
		if loopID == containerID {
			for name := range groups.outputs {
				if !groups.carried[name] {
					delete(set, name)
				}
			}
			continue
		}
		for name := range groups.carried {
			if !groups.outputs[name] {
				delete(set, name)
			}
		}
	}
	return out
}

// workflowLoopBodyContainers returns the ids whose body belongs to a loop node, in document order.
func workflowLoopBodyContainers(nodes []any) []string {
	out := []string{}
	for _, raw := range nodes {
		node, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		data := node.O("data")
		if data.S("kind") == "loop" && node.S("id") != "" && workflowContainerID(node) == "" {
			out = append(out, node.S("id"))
		}
	}
	return out
}

// validateWorkflowContainers checks the structure of every loop container: membership, the one body
// start, nested composites and outputs, cross-scope edges and loopExit terminality. It reports one
// problem per violated rule and node, in document order, so a client renders a stable list.
func validateWorkflowContainers(nodes, edges []any) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	type nodeInfo struct {
		kind      string
		container string
	}
	info := map[string]nodeInfo{}
	order := []string{}
	for _, raw := range nodes {
		node, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		id := node.S("id")
		if id == "" {
			continue
		}
		data := node.O("data")
		info[id] = nodeInfo{kind: data.S("kind"), container: workflowContainerID(node)}
		order = append(order, id)
	}
	// Membership rules: a body node names a loop that exists, and the kinds with their own scope or
	// terminal semantics cannot be nested in v1. A loopExit outside any body has no loop to exit.
	for _, id := range order {
		node := info[id]
		if node.kind == "loopExit" && node.container == "" {
			problems = append(problems, workflowGraphProblem{Code: "loop_exit_outside_body", NodeID: id})
		}
		if node.container == "" {
			continue
		}
		owner, known := info[node.container]
		if !known || (owner.kind != "loop" && owner.kind != "iteration") {
			problems = append(problems, workflowGraphProblem{Code: "loop_member_container_mismatch", NodeID: id})
			continue
		}
		if node.kind == "loopExit" && owner.kind != "loop" {
			problems = append(problems, workflowGraphProblem{Code: "loop_exit_outside_body", NodeID: id})
		}
		switch node.kind {
		case "loop", "iteration", "subflow":
			problems = append(problems, workflowGraphProblem{Code: "loop_nested_composite_unsupported", NodeID: id, Kind: node.kind})
		case "output":
			problems = append(problems, workflowGraphProblem{Code: "loop_body_output_unsupported", NodeID: id})
		}
	}
	// Edge rules: an edge may not cross a container boundary. Both endpoints are known here because
	// validateWorkflowEdges reports unknown endpoints separately; skipping them keeps one mistake
	// from being reported as a container error too.
	crossContainer := map[string]bool{}
	for _, raw := range edges {
		edge, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		source, target := edge.S("source"), edge.S("target")
		sourceNode, sourceOK := info[source]
		targetNode, targetOK := info[target]
		if !sourceOK || !targetOK {
			continue
		}
		if sourceNode.container != targetNode.container {
			problems = append(problems, workflowGraphProblem{Code: "loop_cross_container_edge", NodeID: source})
			crossContainer[source] = true
			crossContainer[target] = true
		}
	}
	// loopExit is a terminal marker: it has no outgoing edge in any container.
	for _, raw := range edges {
		edge, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		if info[edge.S("source")].kind == "loopExit" {
			problems = append(problems, workflowGraphProblem{Code: "loop_exit_has_outgoing_edge", NodeID: edge.S("source")})
		}
	}
	// Per-loop body rules: exactly one start, and every member reachable from it. Reachability is
	// skipped when the start count is wrong because there is no defined place to begin.
	for _, loopID := range workflowLoopBodyContainers(nodes) {
		members := []string{}
		starts := []string{}
		for _, id := range order {
			if info[id].container != loopID {
				continue
			}
			members = append(members, id)
			if info[id].kind == "start" {
				starts = append(starts, id)
			}
		}
		if len(starts) != 1 {
			problems = append(problems, workflowGraphProblem{Code: "loop_body_start_count", NodeID: loopID})
			continue
		}
		reachable := map[string]bool{starts[0]: true}
		queue := []string{starts[0]}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for _, raw := range edges {
				edge, ok := asWorkflowObject(raw)
				if !ok || edge.S("source") != current {
					continue
				}
				next := edge.S("target")
				if info[next].container != loopID || reachable[next] {
					continue
				}
				reachable[next] = true
				queue = append(queue, next)
			}
		}
		for _, member := range members {
			// A nested composite or an Output member is already refused for what it is; reporting it
			// as unreachable too would ask the author to fix a node they have to move or delete.
			switch info[member].kind {
			case "loop", "iteration", "subflow", "output":
				continue
			}
			if !reachable[member] && !crossContainer[member] {
				problems = append(problems, workflowGraphProblem{Code: "loop_body_unreachable", NodeID: member})
			}
		}
	}
	return problems
}

// validateLoopConfig checks `data.loopConfig` against the shape the scheduler executes. `declared` is
// the flat declaration set and `loops` the visibility groups: the initial value is resolved in the
// outer scope at entry, while feedback, until and outputs are resolved against the round pool, which
// adds the carried roots and drops the loop's own outputs.
func validateLoopConfig(
	nodeID string,
	data Object,
	declared map[string]map[string]bool,
	loops map[string]workflowLoopRoots,
	nodes, edges []any,
) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	raw, present := data["loopConfig"]
	if !present || raw == nil {
		return append(problems, workflowGraphProblem{Code: "loop_config_missing", NodeID: nodeID, Field: "loopConfig"})
	}
	config, ok := asWorkflowObject(raw)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "loop_config_missing", NodeID: nodeID, Field: "loopConfig"})
	}
	outer := workflowScopeRoots(nodes, edges, declared, nodeID)
	body := workflowVisibleRoots(declared, loops, nodeID)
	containers := workflowNodeContainers(nodes)
	for id := range body {
		if _, isNode := containers[id]; isNode && id != nodeID && containers[id] != nodeID && outer[id] == nil {
			delete(body, id)
		}
	}

	problems = append(problems, validateLoopMaxIterations(nodeID, config)...)
	problems = append(problems, validateLoopVariables(nodeID, config, outer, body)...)
	problems = append(problems, validateLoopUntil(nodeID, config, body)...)
	problems = append(problems, validateLoopOutputs(nodeID, config, body)...)
	return problems
}

// validateLoopMaxIterations checks the round budget and the exhaustion policy. Both decide whether a
// run stops with a failure or publishes its last round, so a value the scheduler would have to guess
// at is refused rather than defaulted.
func validateLoopMaxIterations(nodeID string, config Object) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	maxIterations, numeric := workflowNumberOf(config["maxIterations"])
	if !numeric || maxIterations != math.Trunc(maxIterations) ||
		maxIterations < workflowLoopMinIterations || maxIterations > workflowLoopMaxIterations {
		problems = append(problems, workflowGraphProblem{Code: "loop_max_iterations_invalid", NodeID: nodeID, Field: "loopConfig.maxIterations"})
	}
	if value, present := config["maxIterationsBehavior"]; present && value != nil {
		behavior, text := value.(string)
		if !text || (behavior != "fail" && behavior != "succeed") {
			problems = append(problems, workflowGraphProblem{Code: "loop_max_iterations_behavior_invalid", NodeID: nodeID, Field: "loopConfig.maxIterationsBehavior"})
		}
	}
	return problems
}

// validateLoopVariables checks the carried variables: names that address a selector segment, a
// supported type, an initial value that type accepts, and a feedback selector the round pool can
// resolve. Initial is resolved in the outer scope, feedback in the body.
func validateLoopVariables(nodeID string, config Object, outer, body map[string]map[string]bool) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	raw, present := config["variables"]
	if !present || raw == nil {
		return append(problems, workflowGraphProblem{Code: "loop_variable_invalid", NodeID: nodeID, Field: "loopConfig.variables"})
	}
	variables, ok := raw.([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "loop_variable_invalid", NodeID: nodeID, Field: "loopConfig.variables"})
	}
	names := map[string]bool{}
	for _, item := range variables {
		variable, ok := asWorkflowObject(item)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "loop_variable_invalid", NodeID: nodeID, Field: "loopConfig.variables"})
			continue
		}
		name := variable.S("name")
		if !validWorkflowLoopName(name) {
			problems = append(problems, workflowGraphProblem{Code: "loop_variable_invalid", NodeID: nodeID, Field: "loopConfig.variables.name"})
		} else if names[name] {
			problems = append(problems, workflowGraphProblem{Code: "loop_variable_invalid", NodeID: nodeID, Field: "loopConfig.variables.name"})
		}
		names[name] = true
		valueType := variable.S("valueType")
		if !workflowSupportedVariableType(valueType) {
			problems = append(problems, workflowGraphProblem{Code: "loop_variable_invalid", NodeID: nodeID, Field: "loopConfig.variables.valueType"})
			continue
		}
		problems = append(problems, validateLoopInitial(nodeID, variable, valueType, outer)...)
		problems = append(problems, validateLoopSelector(nodeID, variable["feedback"], "loopConfig.variables.feedback", "loop_feedback_invalid", body)...)
	}
	return problems
}

// validateLoopInitial checks one carried variable's first-round value. A constant is checked against
// the declared type (the scheduler normalizes it the same way at entry); a variable reference is
// resolved once in the outer scope, so it may name an ancestor's root but not a carried one.
func validateLoopInitial(nodeID string, variable Object, valueType string, outer map[string]map[string]bool) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	initial, ok := asWorkflowObject(variable["initial"])
	if !ok {
		return append(problems, workflowGraphProblem{Code: "loop_initial_invalid", NodeID: nodeID, Field: "loopConfig.variables.initial"})
	}
	switch initial.S("kind") {
	case "constant":
		value, present := initial["value"]
		if !present {
			problems = append(problems, workflowGraphProblem{Code: "loop_initial_invalid", NodeID: nodeID, Field: "loopConfig.variables.initial.value"})
			break
		}
		if _, ok := normalizeWorkflowValue(value, valueType); !ok {
			problems = append(problems, workflowGraphProblem{Code: "loop_initial_invalid", NodeID: nodeID, Field: "loopConfig.variables.initial.value"})
		}
	case "variable":
		problems = append(problems, validateLoopSelector(nodeID, initial["selector"], "loopConfig.variables.initial.selector", "loop_initial_invalid", outer)...)
	default:
		problems = append(problems, workflowGraphProblem{Code: "loop_initial_invalid", NodeID: nodeID, Field: "loopConfig.variables.initial.kind"})
	}
	return problems
}

// validateLoopUntil checks the round-end condition. The comparison rules are the condition node's own
// validator, so an operator or selector this gate admits is one the evaluator can apply; only the
// envelope (logic and list shape) has a loop-specific code.
func validateLoopUntil(nodeID string, config Object, body map[string]map[string]bool) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	until, ok := asWorkflowObject(config["until"])
	if !ok {
		return append(problems, workflowGraphProblem{Code: "loop_until_invalid", NodeID: nodeID, Field: "loopConfig.until"})
	}
	switch until.S("logic") {
	case "and", "or":
	default:
		problems = append(problems, workflowGraphProblem{Code: "loop_until_invalid", NodeID: nodeID, Field: "loopConfig.until.logic"})
	}
	switch conditions := until["conditions"].(type) {
	case nil:
		problems = append(problems, workflowGraphProblem{Code: "loop_until_invalid", NodeID: nodeID, Field: "loopConfig.until.conditions"})
	case []any:
		for _, rule := range conditions {
			problems = append(problems, validateConditionRule(nodeID, rule, body)...)
		}
	default:
		problems = append(problems, workflowGraphProblem{Code: "loop_until_invalid", NodeID: nodeID, Field: "loopConfig.until.conditions"})
	}
	return problems
}

// validateLoopOutputs checks the names a loop publishes. Each selector is resolved against the final
// round's pool, so it may name a body node or a carried root but not the loop's own outputs.
func validateLoopOutputs(nodeID string, config Object, body map[string]map[string]bool) []workflowGraphProblem {
	problems := []workflowGraphProblem{}
	raw, present := config["outputs"]
	if !present || raw == nil {
		return append(problems, workflowGraphProblem{Code: "loop_output_invalid", NodeID: nodeID, Field: "loopConfig.outputs"})
	}
	outputs, ok := raw.([]any)
	if !ok {
		return append(problems, workflowGraphProblem{Code: "loop_output_invalid", NodeID: nodeID, Field: "loopConfig.outputs"})
	}
	names := map[string]bool{}
	for _, item := range outputs {
		output, ok := asWorkflowObject(item)
		if !ok {
			problems = append(problems, workflowGraphProblem{Code: "loop_output_invalid", NodeID: nodeID, Field: "loopConfig.outputs"})
			continue
		}
		name := output.S("name")
		if !validWorkflowLoopName(name) {
			problems = append(problems, workflowGraphProblem{Code: "loop_output_invalid", NodeID: nodeID, Field: "loopConfig.outputs.name"})
		} else if names[name] {
			problems = append(problems, workflowGraphProblem{Code: "loop_output_invalid", NodeID: nodeID, Field: "loopConfig.outputs.name"})
		}
		names[name] = true
		problems = append(problems, validateLoopSelector(nodeID, output["variableSelector"], "loopConfig.outputs.variableSelector", "loop_output_invalid", body)...)
	}
	return problems
}

// validateLoopSelector checks one selector against a visibility projection. A malformed selector gets
// the caller's shape code; a well-formed one that names an undeclared writer or root gets the shared
// undeclared code, so the author learns the same thing whichever field carried the reference.
func validateLoopSelector(
	nodeID string,
	value any,
	field string,
	shapeCode string,
	visible map[string]map[string]bool,
) []workflowGraphProblem {
	parts, ok := workflowSelectorParts(value)
	if !ok {
		return []workflowGraphProblem{{Code: shapeCode, NodeID: nodeID, Field: field}}
	}
	if !visible[parts[0]][parts[1]] {
		return []workflowGraphProblem{{Code: "loop_selector_undeclared", NodeID: nodeID, Field: field}}
	}
	return nil
}

// workflowSelectorParts reads a Dify-style selector as its string segments, desktop's
// `VariableSelector::try_from_parts`: at least a node and a root, every segment non-empty.
func workflowSelectorParts(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok || len(raw) < 2 {
		return nil, false
	}
	parts := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil, false
		}
		parts = append(parts, text)
	}
	return parts, true
}

// validWorkflowLoopName reports whether a carried-variable or output name can address a selector
// segment: non-blank, no surrounding whitespace and no dot (desktop's loop `validate_name`).
func validWorkflowLoopName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && !strings.Contains(name, ".")
}

// workflowObjectEntries reads a JSON list of objects, dropping entries that are not objects. It is for
// readers that only collect names, where a malformed entry is reported by the shape validator.
func workflowObjectEntries(raw any) []Object {
	list, _ := raw.([]any)
	out := make([]Object, 0, len(list))
	for _, item := range list {
		if entry, ok := asWorkflowObject(item); ok {
			out = append(out, entry)
		}
	}
	return out
}

// workflowContainerID returns the container a node belongs to: the explicit `data.containerId` that
// loop bodies use, or React Flow's top-level `parentId` that iteration regions persist. Graph decode
// normalises both here so every membership decision — validation, visibility, scheduling — reads one
// value instead of re-deriving it (iteration ADR D1).
func workflowContainerID(node Object) string {
	if id := node.O("data").S("containerId"); id != "" {
		return id
	}
	return node.S("parentId")
}
