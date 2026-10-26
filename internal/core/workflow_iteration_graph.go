package core

// Iteration is Cloud's foreach container: the length of one array is the round count, each round
// binds `{iteration}.item` / `{iteration}.index` and runs the region once, and the node exposes
// `output` (collected successful values), `entries` (the per-round ledger) and `failed_count`.
//
// This validator is shared by publication and frozen-graph runtime admission.

// validateIterationConfig checks the executable contract of one `iteration` node. Every problem
// names the node and the field so the editor can point at the offending control.
func validateIterationConfig(
	nodeID string,
	data Object,
	nodes, edges []any,
	declaredRoots map[string]map[string]bool,
) []workflowGraphProblem {
	raw, present := data["iterationConfig"]
	config, ok := asWorkflowObject(raw)
	if !present || raw == nil || !ok {
		return []workflowGraphProblem{{Code: "iteration_config_missing", NodeID: nodeID, Field: "iterationConfig"}}
	}
	members := workflowIterationMembers(nodes, nodeID)
	problems := []workflowGraphProblem{}
	iterator, ok := workflowIterationSelector(config["iteratorSelector"])
	visible := workflowScopeRoots(nodes, edges, declaredRoots, nodeID)
	if !ok {
		problems = append(problems, workflowGraphProblem{Code: "iteration_iterator_unsupported", NodeID: nodeID, Field: "iterationConfig.iteratorSelector"})
	} else if !visible[iterator.nodeID][iterator.root] {
		// The iterator must resolve before the region runs, so a member-declared root is not
		// visible here: the body has not produced anything yet in this round.
		problems = append(problems, workflowGraphProblem{Code: "iteration_iterator_undeclared", NodeID: nodeID, Field: "iterationConfig.iteratorSelector"})
	}
	collect, ok := workflowIterationSelector(config["collectSelector"])
	switch {
	case !ok:
		problems = append(problems, workflowGraphProblem{Code: "iteration_collect_unsupported", NodeID: nodeID, Field: "iterationConfig.collectSelector"})
	case !declaredRoots[collect.nodeID][collect.root]:
		problems = append(problems, workflowGraphProblem{Code: "iteration_collect_unsupported", NodeID: nodeID, Field: "iterationConfig.collectSelector"})
	case !members[collect.nodeID]:
		problems = append(problems, workflowGraphProblem{Code: "iteration_collect_outside_region", NodeID: nodeID, Field: "iterationConfig.collectSelector"})
	}
	if strategy, present := config["errorStrategy"]; present && strategy != nil {
		if text, isString := strategy.(string); !isString || (text != "fail" && text != "continue") {
			problems = append(problems, workflowGraphProblem{Code: "iteration_error_strategy_unknown", NodeID: nodeID, Field: "iterationConfig.errorStrategy"})
		}
	}
	if ceiling, present := config["maxIterations"]; present && ceiling != nil {
		if !workflowIterationCeilingValid(ceiling) {
			problems = append(problems, workflowGraphProblem{Code: "iteration_max_iterations_invalid", NodeID: nodeID, Field: "iterationConfig.maxIterations"})
		}
	}
	if len(members) == 0 {
		return append(problems, workflowGraphProblem{Code: "iteration_body_empty", NodeID: nodeID})
	}
	if !workflowIterationRegionReachable(members, edges) {
		problems = append(problems, workflowGraphProblem{Code: "iteration_body_unreachable", NodeID: nodeID})
	}
	return problems
}

// workflowIterationSelector parses a root selector: exactly `[nodeID, root]` with no nesting.
func workflowIterationSelector(raw any) (workflowSelector, bool) {
	selector, ok := parseWorkflowSelector(raw)
	if !ok || len(selector.nested) != 0 {
		return workflowSelector{}, false
	}
	return selector, true
}

// workflowIterationCeilingValid enforces Cloud's 1..100 whole-number budget.
func workflowIterationCeilingValid(raw any) bool {
	switch value := raw.(type) {
	case float64:
		return value >= 1 && value <= 100 && value == float64(int64(value))
	case int:
		return value >= 1 && value <= 100
	case int64:
		return value >= 1 && value <= 100
	default:
		return false
	}
}

// workflowIterationMembers returns the region's member IDs keyed for membership tests.
func workflowIterationMembers(nodes []any, iterationID string) map[string]bool {
	members := map[string]bool{}
	for _, raw := range nodes {
		node, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		if id := node.S("id"); id != "" && workflowContainerID(node) == iterationID {
			members[id] = true
		}
	}
	return members
}

// workflowIterationRegionReachable reports whether every member is reachable from an entry member,
// where an entry is a member with no predecessor inside the region. A region without entries, or with
// a member no entry can reach, would leave part of the body silently unexecuted in every round.
func workflowIterationRegionReachable(members map[string]bool, edges []any) bool {
	successors := map[string][]string{}
	indegree := map[string]int{}
	for id := range members {
		successors[id] = nil
		indegree[id] = 0
	}
	for _, raw := range edges {
		edge, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		source, target := edge.S("source"), edge.S("target")
		if !members[source] || !members[target] {
			continue
		}
		successors[source] = append(successors[source], target)
		indegree[target]++
	}
	queue := make([]string, 0, len(members))
	for id, degree := range indegree {
		if degree == 0 {
			queue = append(queue, id)
		}
	}
	if len(queue) == 0 {
		return false
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, successors[id]...)
	}
	return len(seen) == len(members)
}
