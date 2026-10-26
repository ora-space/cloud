package core

// validWorkflowIterationProgress validates a running document before any indexing or dispatch.
// Terminal documents are never resumed. A running round has exactly its preceding prefix settled.
func validWorkflowIterationProgress(s *workflowIterationRunState, config *workflowIterationConfig, graph workflowGraphView, id string) bool {
	if s == nil || s.invalid || s.Strategy != config.Strategy || len(s.Items) > config.Ceiling ||
		s.Round < 0 || s.Round >= len(s.Items) || len(s.Ledger) != s.Round || len(s.Completed) != s.Round {
		return false
	}
	body := workflowBodyGraph(graph, id)
	output := []any{}
	failed := 0
	for i, raw := range s.Ledger {
		entry, ok := asWorkflowObject(raw)
		_, hasItem := entry["item"]
		if !ok || !hasItem || len(entry) != 4 || jsonText(entry["item"]) != jsonText(s.Items[i]) {
			return false
		}
		value, hasOutput := entry["output"]
		detail, hasError := entry["error"]
		if !hasOutput || !hasError {
			return false
		}
		switch entry.S("status") {
		case "succeeded":
			if detail != nil {
				return false
			}
			output = append(output, value)
		case "failed":
			if _, ok := detail.(string); !ok || value != nil || s.Strategy == "fail" {
				return false
			}
			failed++
		default:
			return false
		}
		archive, ok := asWorkflowObject(s.Completed[i])
		index, numeric := workflowNumberOf(archive["index"])
		members, object := asWorkflowObject(archive["states"])
		if !ok || !numeric || index != float64(i) || !object {
			return false
		}
		for memberID, rawState := range members {
			if _, known := body.ByNode[memberID]; !known {
				return false
			}
			member, ok := asWorkflowObject(rawState)
			if !ok {
				return false
			}
			switch member.S("status") {
			case "succeeded", "failed", "inactive":
			default:
				return false
			}
		}
		archivedStates := workflowStatesFromJSON(members)
		projection := projectWorkflowNodes(body, archivedStates)
		collector := archivedStates[config.Collect.nodeID]
		_, collected := collector.Output[config.Collect.root]
		if entry.S("status") == "succeeded" {
			if projection.hasFailure() || projection.hasPending() || len(members) != len(body.Nodes) {
				return false
			}
			member, ok := asWorkflowObject(members[config.Collect.nodeID])
			if !ok || member.S("status") != "succeeded" {
				return false
			}
			collected, exists := member.O("output")[config.Collect.root]
			if !exists || jsonText(collected) != jsonText(value) {
				return false
			}
		} else if !projection.hasFailure() && collector.Status == "succeeded" && collected {
			// A failure entry must be justified by the archived body or an unset collect target.
			return false
		}
	}
	return failed == s.Failed && jsonText(output) == jsonText(s.Output)
}
