package core

// The variable aggregator: an ordered pass-through that collapses mutually exclusive branch outputs
// into one variable (desktop `workflow_run/engine/aggregator.rs`, V1 contract). Selectors are an
// ordered non-empty list of root selectors; the node publishes the first **assigned** candidate as
// `{aggregator}.output`, and zero assigned candidates is a stable failure.
//
// "Assigned" is key existence in the committed pool, never truthiness: `null`, `false`, `0`, `""`, an
// empty list and an empty object all count, exactly as they do on desktop. The node never reads branch
// decisions; which branch ran is expressed by the committed states the selector resolves against.

// aggregatorSelector is one parsed candidate: a node and a root, with no nested path. Nested paths are
// refused at publish because a root selector is the only shape the aggregator can statically type.
type aggregatorSelector struct {
	nodeID string
	root   string
}

// validateAggregatorConfig checks `data.aggregatorConfig` against the executable contract. Every
// problem names the node and the field, so the editor can point at the list rather than at the graph.
func validateAggregatorConfig(nodeID string, data Object, declaredRoots map[string]map[string]bool) []workflowGraphProblem {
	raw, present := data["aggregatorConfig"]
	if !present || raw == nil {
		return []workflowGraphProblem{{Code: "aggregator_config_missing", NodeID: nodeID, Field: "aggregatorConfig"}}
	}
	config, ok := asWorkflowObject(raw)
	if !ok {
		return []workflowGraphProblem{{Code: "aggregator_config_missing", NodeID: nodeID, Field: "aggregatorConfig"}}
	}
	rawSelectors, present := config["variables"]
	if !present || rawSelectors == nil {
		return []workflowGraphProblem{{Code: "aggregator_variables_not_a_list", NodeID: nodeID, Field: "aggregatorConfig.variables"}}
	}
	selectors, ok := rawSelectors.([]any)
	if !ok {
		return []workflowGraphProblem{{Code: "aggregator_variables_not_a_list", NodeID: nodeID, Field: "aggregatorConfig.variables"}}
	}
	if len(selectors) == 0 {
		return []workflowGraphProblem{{Code: "aggregator_variables_empty", NodeID: nodeID, Field: "aggregatorConfig.variables"}}
	}
	problems := []workflowGraphProblem{}
	seen := map[string]bool{}
	for _, item := range selectors {
		selector, ok := parseWorkflowSelector(item)
		if !ok || len(selector.nested) != 0 {
			problems = append(problems, workflowGraphProblem{Code: "aggregator_selector_unsupported", NodeID: nodeID, Field: "aggregatorConfig.variables"})
			continue
		}
		qualified := selector.qualified()
		if seen[qualified] {
			problems = append(problems, workflowGraphProblem{Code: "aggregator_selector_duplicate", NodeID: nodeID, Field: "aggregatorConfig.variables"})
			continue
		}
		seen[qualified] = true
		if !declaredRoots[selector.nodeID][selector.root] {
			problems = append(problems, workflowGraphProblem{Code: "aggregator_selector_undeclared", NodeID: nodeID, Field: "aggregatorConfig.variables"})
		}
	}
	return problems
}

// parseAggregatorSelectors compiles the wire list the way the scheduler executes it. It runs on frozen
// snapshots, so it reports a document the gate would have refused instead of assuming the shape.
func parseAggregatorSelectors(data Object) ([]aggregatorSelector, error) {
	config, ok := asWorkflowObject(data["aggregatorConfig"])
	if !ok {
		return nil, errString("aggregator node has no config")
	}
	raw, ok := config["variables"].([]any)
	if !ok {
		return nil, errString("aggregator variables must be a list")
	}
	selectors := make([]aggregatorSelector, 0, len(raw))
	for _, item := range raw {
		selector, ok := parseWorkflowSelector(item)
		if !ok || len(selector.nested) != 0 {
			return nil, errString("aggregator selector must be a node id and a root")
		}
		selectors = append(selectors, aggregatorSelector{nodeID: selector.nodeID, root: selector.root})
	}
	if len(selectors) == 0 {
		return nil, errString("aggregator must declare at least one selector")
	}
	return selectors, nil
}

// errString is a tiny error type so the parser stays dependency-free and its messages reach the node
// state unchanged.
type errString string

func (e errString) Error() string { return string(e) }

// evaluateWorkflowAggregator resolves the declared candidates in order and returns the first
// assigned value. Presence is key existence in the committed pool, so null, false, 0, "" and empty
// containers all count as assigned — desktop's V1 contract, not a truthiness test. Unassigned
// candidates are skipped; an empty result is reported as not assigned so the caller can fail with
// the stable no-match reason.
func evaluateWorkflowAggregator(node workflowNodeView, states map[string]workflowNodeState) (value any, assigned bool, err error) {
	selectors, err := parseAggregatorSelectors(node.Data)
	if err != nil {
		return nil, false, err
	}
	for _, candidate := range selectors {
		value, present, err := resolveWorkflowSelector(workflowSelector{nodeID: candidate.nodeID, root: candidate.root}, states)
		if err != nil {
			return nil, false, err
		}
		if present {
			return value, true, nil
		}
	}
	return nil, false, nil
}
