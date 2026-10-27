package core

// assignWorkflowAgentOutput reads only durable, settled history from this node's current
// execution attempt. History updates already contain assembled messages; concatenating them
// would mix tool preambles into the final answer. The initial turn identity excludes later
// turns, and the successful turn boundary excludes interrupted or still-streaming output.
// Session terminal evidence remains required: callers invoke this only while settling it.
func assignWorkflowAgentOutput(t *transaction, runID, nodeID string, states map[string]workflowNodeState) {
	state := states[nodeID]
	if state.Status != "succeeded" {
		return
	}
	execution := t.one(`SELECT execution_id, input FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3
		AND kind='agent_session' AND result IS NOT NULL`, runID, nodeID, state.attemptNumber())
	if execution == nil {
		return
	}
	turnID := execution.O("input").O("initialTurn").S("turnId")
	if turnID == "" {
		return
	}
	ended := t.one(workflowOutputReceiptsSQL+`SELECT sequence, (event->>'record')::jsonb AS record
		FROM receipts WHERE event->>'turnId'=$2
		AND (event->>'record')::jsonb->>'type'='turnEnded'
		ORDER BY sequence DESC LIMIT 1`, execution.S("executionId"), turnID)
	if ended == nil || ended.O("record").S("stop_reason") == "cancelled" {
		return
	}
	// A retried prompt in the same execution starts a fresh answer. Never reuse a message
	// before its latest user input, even when that attempt produced no assistant text.
	message := t.one(workflowOutputReceiptsSQL+`SELECT event, (event->>'record')::jsonb AS record
		FROM receipts WHERE event->>'turnId'=$2
		AND sequence<$3 AND sequence>COALESCE((
			SELECT MAX(sequence) FROM receipts WHERE sequence<$3
			AND (event->>'record')::jsonb->'update'->>'sessionUpdate'='user_message_chunk'
		),0)
		AND (((event->>'record')::jsonb->'update'->>'sessionUpdate'='agent_message_chunk'
			AND (event->>'record')::jsonb->'update'->'content'->>'type'='text')
			OR event ? 'visibleText')
		ORDER BY sequence DESC LIMIT 1`, execution.S("executionId"), turnID, ended.N("sequence"))
	if message == nil {
		return
	}
	content := message.O("record").O("update").O("content")
	if !message.O("event").B("truncated") && content.S("type") == "text" {
		if text := content.S("text"); text != "" {
			state.Output = Object{"output": text}
		}
	} else if message.O("event").B("truncated") {
		// The visibleText projection is bounded independently of the history record. It is
		// not a substitute for a complete deliverable; never publish clipped text as success.
		state.Status = "failed"
		state.Error = "agent_output_truncated"
	}
	states[nodeID] = state
}

// Terminal receipts are opaque bytes, not necessarily JSON. Guard their cast before
// inspecting history, whose record field was validated by takeOverThreadEvents.
const workflowOutputReceiptsSQL = `WITH receipts AS MATERIALIZED (
	SELECT sequence, CASE WHEN event IS JSON OBJECT THEN event::jsonb ELSE '{}'::jsonb END AS event
	FROM node_event_receipts WHERE execution_id=$1
) `
