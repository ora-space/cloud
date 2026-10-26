package core

// workflowArchivedAgentAttempt keeps execution identity unique when a body starts another round.
// Archives are authoritative; clearing the active projection must not reuse an earlier receipt.
func workflowArchivedAgentAttempt(states map[string]workflowNodeState, node workflowNodeView) int64 {
	owner := states[node.Container]
	var archives []any
	if owner.Loop != nil {
		archives = owner.Loop.Completed
	}
	if owner.Iteration != nil {
		archives = owner.Iteration.Completed
	}
	var last int64
	for _, raw := range archives {
		archive, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		member, ok := asWorkflowObject(archive.O("states")[node.ID])
		if !ok || member.S("status") == "inactive" {
			continue
		}
		attempt := member.N("attempt")
		if attempt < 1 {
			attempt = 1
		}
		if attempt > last {
			last = attempt
		}
	}
	return last
}
