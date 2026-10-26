package core

import (
	"context"
	"encoding/json"
)

// EnqueueThreadCommand persists one user turn or end request. It becomes claimable only after this
// transaction commits, and only once the run's session execution is registered.
func (s *Store) EnqueueThreadCommand(ctx context.Context, runID, kind string, body Object) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		return enqueueThreadCommand(t, runID, kind, body)
	})
}

func enqueueThreadCommand(t *transaction, runID, kind string, body Object) Object {
	require(validID(runID), 400, "invalid_command")
	require(t.one("SELECT id FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID) != nil, 404, "not_found")
	require(kind == "submit_user_turn" || kind == "end_session", 400, "invalid_command")
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE run_id=$1", runID) == nil, 409, "dispatch_conflict")
	validateThreadCommand(kind, body)
	id := newID()
	t.exec("INSERT INTO thread_commands(id,run_id,kind,body) VALUES($1,$2,$3,$4)", id, runID, kind, jsonText(body))
	t.commandSignals = append(t.commandSignals, runID)
	return t.one("SELECT * FROM thread_commands WHERE id=$1", id)
}

// enqueueWorkflowThreadCommand releases one session command for a WorkflowRun's graph node and
// attempt. It is the WorkflowRun counterpart of enqueueThreadCommand: the row registers against
// workflow_runs (0049's discriminator), and the node scopes the command to the session it addresses —
// a run executes one agent node at a time but several over its life (D4). The attempt scopes it
// further, to that node's one session per attempt (retry decision D4): attempt 1's end request must
// never be reused for, or delivered to, attempt 2. Repeating the same request returns the original
// row; a different request for the same node and attempt conflicts.
func enqueueWorkflowThreadCommand(t *transaction, runID, nodeID string, attempt int64, kind string, body Object) Object {
	require(validID(runID), 400, "invalid_command")
	require(t.one("SELECT id FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID) != nil, 404, "not_found")
	require(kind == "end_session", 400, "invalid_command")
	validateThreadCommand(kind, body)
	if attempt < 1 {
		attempt = 1
	}
	if existing := t.one("SELECT * FROM thread_commands WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind=$4", runID, nodeID, attempt, kind); existing != nil {
		require(jsonText(existing.O("body")) == jsonText(body), 409, "dispatch_conflict")
		return existing
	}
	id := newID()
	t.exec("INSERT INTO thread_commands(id,workflow_run_id,workflow_node_id,workflow_node_attempt,kind,body) VALUES($1,$2,$3,$4,$5,$6)", id, runID, nodeID, attempt, kind, jsonText(body))
	t.commandSignals = append(t.commandSignals, runID)
	return t.one("SELECT * FROM thread_commands WHERE id=$1", id)
}

func validateThreadCommand(kind string, body Object) {
	if kind == "end_session" {
		require(body.S("reason") == "user_ended" || body.S("reason") == "idle_timeout" || body.S("reason") == "cancelled", 400, "invalid_command")
		return
	}
	require(body.S("turnId") != "" && len(body.S("turnId")) <= 200, 400, "invalid_command")
	blocks := objectsOf(body["content"])
	require(len(blocks) > 0, 400, "invalid_command")
	for _, block := range blocks {
		text := block.S("text")
		require(text != "" && len(text) <= 100000, 400, "invalid_command")
	}
}

func claimThreadCommands(t *transaction, limit int64) Object {
	require(limit >= 1 && limit <= 100, 400, "invalid_limit")
	// A command names its run in one of two columns: an IssueRun command carries run_id, a
	// WorkflowRun command carries workflow_run_id and leaves run_id NULL (0049's exactly-one-owner
	// CHECK). The Controller matches a command to its control binding by the run id it is handed and
	// echoes the same id back on delivery, so the claim projects one value. The alias is deliberate
	// rather than `c.*`: row_to_json keeps both copies of a repeated column name, and which one the
	// decoder ends up with is not something this query should depend on.
	//
	// A workflow command is scoped to the graph node and attempt whose session it addresses. The join
	// therefore carries both: one operation id has one live agent_session execution per graph node
	// over the run's life (D4), and one more per attempt since retries (retry decision D4), so a
	// command must never be claimable against another node's session or another attempt's. IssueRun
	// commands carry no node, and their executions keep the default attempt, so the node branch is
	// where the attempt predicate belongs. Ordering is by queue_sequence alone — it is the insertion
	// order the Controller's "a skipped head blocks later commands of that run" rule relies on —
	// because ordering by the owner column would sort every workflow command after every IssueRun
	// command.
	rows := t.list(`SELECT c.id AS command_id,COALESCE(c.run_id,c.workflow_run_id) AS run_id,c.kind,c.body,
			e.execution_id,e.node_id,e.workspace_id,s.id AS sandbox_instance_id
		FROM thread_commands c
		JOIN node_executions e ON e.operation_id=COALESCE(c.run_id,c.workflow_run_id)
		 AND e.kind='agent_session' AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
		 AND (c.workflow_node_id IS NULL OR (e.workflow_node_id=c.workflow_node_id AND e.workflow_node_attempt=c.workflow_node_attempt))
		JOIN workspaces w ON w.id=e.workspace_id
		JOIN sandbox_instances s ON s.workspace_id=w.id AND s.generation=w.runtime_generation AND s.terminated_at IS NULL
		WHERE c.delivered_at IS NULL
		  AND (c.run_id IS NULL OR NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.run_id=c.run_id))
		ORDER BY c.queue_sequence
		LIMIT $1`, limit)
	return Object{"commands": rows}
}

func recordThreadCommandDelivered(t *transaction, r *ControlRequest) Object {
	id, execution := r.Body.S("commandId"), r.Body.S("executionId")
	cmd := t.one("SELECT * FROM thread_commands WHERE id=$1", id)
	require(cmd != nil, 404, "not_found")
	owner := cmd.S("runId")
	if owner == "" {
		owner = cmd.S("workflowRunId")
	}
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='agent_session'", execution, owner)
	require(e != nil, 409, "dispatch_conflict")
	// A WorkflowRun command names the graph node and attempt whose session it addresses; an execution
	// of the same run under another node, or under the same node's other attempt, is not this
	// command's destination (retry decision D4).
	if node := cmd.S("workflowNodeId"); node != "" {
		require(e.S("workflowNodeId") == node && e.N("workflowNodeAttempt") == cmd.N("workflowNodeAttempt"), 409, "dispatch_conflict")
	}
	if cmd.S("deliveredExecutionId") != "" {
		require(cmd.S("deliveredExecutionId") == execution, 409, "dispatch_conflict")
		return Object{}
	}
	t.exec("UPDATE thread_commands SET delivered_at=clock_timestamp(),delivered_execution_id=$2 WHERE id=$1 AND delivered_at IS NULL", id, execution)
	return Object{}
}

// takeOverThreadEvents stores a contiguous batch of one session execution. A gap or a changed
// record stores nothing. The business hook runs only for sequences stored by this call.
func takeOverThreadEvents(t *transaction, r *ControlRequest) Object {
	execution := r.Body.S("executionId")
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='agent_session'", execution, r.Body.S("operationId"))
	require(e != nil, 404, "not_found")
	events := objectsOf(r.Body["events"])
	require(len(events) >= 1 && len(events) <= 64, 400, "invalid_event")
	last := e.N("lastEventSequence")
	var fresh []Object
	var expected int64
	for i, ev := range events {
		seq := ev.N("sequence")
		record := ev.S("record")
		require(seq >= 1 && record != "" && len(record) <= 256*1024, 400, "invalid_event")
		var content map[string]json.RawMessage
		require(json.Unmarshal([]byte(record), &content) == nil && content != nil, 400, "invalid_event")
		if i == 0 {
			expected = seq
		}
		require(seq == expected, 409, "receipt_conflict")
		expected++
		canonEvent := Object{"record": record, "sequence": seq, "truncated": ev.B("truncated"), "turnId": ev.S("turnId")}
		if _, present := ev["historySequence"]; present {
			require(ev.N("historySequence") >= 0 && ev.N("historySequence") <= 4294967295, 400, "invalid_event")
			canonEvent["historySequence"] = ev.N("historySequence")
		}
		// Optional projection metadata is added only when present: an older Node replaying an event
		// after an upgrade must compare equal to the receipt Cloud stored before this field existed.
		if text, present := ev["visibleText"]; present {
			reported, ok := text.(string)
			require(ok && len(reported) <= 65536, 400, "invalid_event")
			canonEvent["visibleText"] = reported
		}
		if ev.B("visibleTextTruncated") {
			require(ev.S("visibleText") != "", 400, "invalid_event")
			canonEvent["visibleTextTruncated"] = true
		}
		canon := jsonText(canonEvent)
		if seq <= last {
			old := t.one("SELECT event FROM node_event_receipts WHERE execution_id=$1 AND sequence=$2", execution, seq)
			require(old != nil && old.S("event") == canon, 409, "receipt_conflict")
			continue
		}
		require(seq == last+1, 409, "receipt_conflict")
		require(len(e.O("result")) == 0 && e["terminatedByForceStopId"] == nil, 409, "result_conflict")
		t.exec("INSERT INTO node_event_receipts(execution_id,sequence,event) VALUES($1,$2,$3)", execution, seq, canon)
		last = seq
		fresh = append(fresh, ev)
	}
	if len(fresh) > 0 {
		t.exec("UPDATE node_executions SET last_event_sequence=$2 WHERE execution_id=$1", execution, last)
		threadEventsTakenOver(t, e.S("operationId"), execution, fresh)
	}
	return Object{"takenOverThrough": last}
}
