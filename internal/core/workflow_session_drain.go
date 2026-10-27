package core

import "time"

// drainWorkflowTimedOutSession keeps the running attempt as the serial dispatch barrier until
// its terminal evidence arrives. Command acknowledgement alone does not prove the session ended.
func drainWorkflowTimedOutSession(t *transaction, runID string, version int64, node workflowNodeView, states map[string]workflowNodeState) (map[string]workflowNodeState, bool) {
	state := states[node.ID]
	attempt := state.attemptNumber()
	ended := t.one(`SELECT execution_id FROM node_executions WHERE operation_id=$1
		AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='agent_session' AND result IS NOT NULL`, runID, node.ID, attempt)
	if ended != nil {
		state.Status, state.FinishedAt = "failed", nowRFC3339()
		state.Output = nil
		state.SessionEndReason = workflowSessionTurnDeadlineExceeded
		states[node.ID] = state
		settleWorkflowAgentFailure(t, runID, version, node, states, workflowSessionTurnDeadlineExceeded)
		return states, false
	}
	cmd := enqueueWorkflowThreadCommand(t, runID, node.ID, attempt, "end_session", Object{"reason": "cancelled"})
	window := startupDuration(t.store.AgentEndConfirmTimeout, 5*time.Minute)
	if t.one(`SELECT id FROM thread_commands WHERE id=$1
		AND created_at<=clock_timestamp()-make_interval(secs=>$2)`, cmd.S("id"), window.Seconds()) != nil {
		state.Status, state.FinishedAt = "failed", nowRFC3339()
		state.Error = "session_end_deadline_exceeded"
		state.Output = nil
		state.SessionEndReason = "session_end_deadline_exceeded"
		states[node.ID] = state
		finishWorkflowUndrainedSession(t, runID, version, node, states, "session_end_deadline_exceeded")
	}
	return states, false
}

// finishWorkflowUndrainedSession preserves the failed round and releases the Workspace. A bounded
// wait is not proof of termination: continuing in this Workspace would overlap the old session.
func finishWorkflowUndrainedSession(t *transaction, runID string, version int64, node workflowNodeView, states map[string]workflowNodeState, reason string) {
	snapshot := t.one(`SELECT s.graph FROM workflow_runs r JOIN workflow_snapshots s ON s.id=r.snapshot_id WHERE r.id=$1`, runID)
	graph := readWorkflowGraph(snapshot.O("graph"))
	container := graph.ByNode[node.Container]
	state := states[container.ID]
	body := workflowBodyGraph(graph, container.ID)
	if state.Iteration != nil {
		iteration := state.Iteration
		iteration.Failed++
		iteration.Ledger = append(iteration.Ledger, Object{"item": iteration.Items[iteration.Round], "status": "failed", "output": nil, "error": reason})
		workflowInactiveStates(body, states)
		archiveWorkflowIterationRound(iteration, body, states)
		failWorkflowIteration(states, container, reason, reason)
	} else if state.Loop != nil {
		archiveWorkflowLoopRound(state.Loop, body, states, true)
		failWorkflowLoop(states, container, reason, reason)
	}
	// D4 distinguishes the stable node code from the human-readable run error.
	finishWorkflowRun(t, runID, version, "failed", "session end deadline exceeded", states)
}
