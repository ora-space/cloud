package core

// workflowPredecessorStates filters a round pool for a body consumer. The owner input and ambient
// imports are already scoped; sibling members still need the same strict predecessor check.
func workflowPredecessorStates(graph workflowGraphView, node workflowNodeView, pool map[string]workflowNodeState) map[string]workflowNodeState {
	ancestors := workflowAncestorIDs(graph, node.ID)
	out := map[string]workflowNodeState{}
	for id := range pool {
		producer, known := graph.ByNode[id]
		if known && producer.Container == node.Container && !ancestors[id] {
			continue
		}
		out[id] = pool[id]
	}
	return out
}

// workflowScopeRoots projects declarations using only edges within one region. Ambient roots
// have no graph node; body inputs are imported explicitly, never through cross-region edges.
func workflowScopeRoots(nodes, edges []any, declared map[string]map[string]bool, nodeID string) map[string]map[string]bool {
	containers := workflowNodeContainers(nodes)
	ancestors := workflowRegionAncestors(containers, edges, nodeID)
	owner := containers[nodeID]
	if owner != "" {
		for id := range workflowRegionAncestors(containers, edges, owner) {
			ancestors[id] = true
		}
	}
	visible := workflowVisibleRoots(declared, workflowLoopRootsByName(nodes), owner)
	for id := range visible {
		if _, isNode := containers[id]; isNode && !ancestors[id] && id != owner {
			delete(visible, id)
		}
	}
	if owner != "" {
		for _, raw := range nodes {
			node, ok := asWorkflowObject(raw)
			if ok && node.S("id") == owner && node.O("data").S("kind") == "iteration" {
				visible[owner] = map[string]bool{"item": true, "index": true}
			}
		}
	}
	return visible
}

func workflowRegionAncestors(containers map[string]string, edges []any, nodeID string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{nodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, raw := range edges {
			edge, ok := asWorkflowObject(raw)
			if !ok || edge.S("target") != current {
				continue
			}
			source := edge.S("source")
			region, known := containers[source]
			if !known || region != containers[nodeID] || source == nodeID || seen[source] {
				continue
			}
			seen[source] = true
			queue = append(queue, source)
		}
	}
	return seen
}

// workflowAggregatorScopeProblems keeps an undeclared root distinct from a declared but
// inaccessible producer, including private container inputs/outputs and other bodies.
func workflowAggregatorScopeProblems(id string, data Object, declared, visible map[string]map[string]bool) []workflowGraphProblem {
	problems := validateAggregatorConfig(id, data, declared)
	selectors, err := parseAggregatorSelectors(data)
	if err != nil {
		return problems
	}
	for _, selector := range selectors {
		if declared[selector.nodeID][selector.root] && !visible[selector.nodeID][selector.root] {
			problems = append(problems, workflowGraphProblem{Code: "aggregator_selector_not_upstream", NodeID: id, Field: "aggregatorConfig.variables"})
		}
	}
	return problems
}
