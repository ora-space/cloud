package core

// validWorkflowLoopProgress rejects damaged round prefixes before a body session can be replayed.
func validWorkflowLoopProgress(s *workflowLoopRunState, config *workflowLoopConfig, graph workflowGraphView, id string) bool {
	if s == nil || s.invalid || s.Round < 1 || s.Round > config.MaxIterations || s.MaxIterations != config.MaxIterations || int64(len(s.Completed)) != s.Round-1 {
		return false
	}
	validCarried := func(carried Object) bool {
		if len(carried) != len(config.Variables) {
			return false
		}
		for i := range config.Variables {
			variable := &config.Variables[i]
			value, exists := carried[variable.Name]
			if !exists {
				return false
			}
			if _, valid := normalizeWorkflowValue(value, variable.ValueType); !valid {
				return false
			}
		}
		return true
	}
	if !validCarried(Object(s.Carried)) {
		return false
	}
	body := workflowBodyGraph(graph, id)
	for i, raw := range s.Completed {
		archive, ok := asWorkflowObject(raw)
		index, numeric := workflowNumberOf(archive["index"])
		members, object := asWorkflowObject(archive["states"])
		if !ok || !numeric || index != float64(i+1) || !object || len(members) != len(body.Nodes) || !validCarried(archive.O("carried")) {
			return false
		}
		for memberID, rawState := range members {
			if _, known := body.ByNode[memberID]; !known {
				return false
			}
			member, ok := asWorkflowObject(rawState)
			if !ok || (member.S("status") != "succeeded" && member.S("status") != "inactive") {
				return false
			}
		}
		prior := workflowStatesFromJSON(members)
		projection := projectWorkflowNodes(body, prior)
		if projection.hasPending() || workflowLoopExitSucceeded(body, prior) {
			return false
		}
	}
	return true
}
