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
	rows := t.list(`SELECT c.id AS command_id,c.run_id,c.kind,c.body,e.execution_id,e.node_id,e.workspace_id,s.id AS sandbox_instance_id
		FROM thread_commands c
		JOIN node_executions e ON e.operation_id=c.run_id AND e.kind='agent_session' AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
		JOIN workspaces w ON w.id=e.workspace_id
		JOIN sandbox_instances s ON s.workspace_id=w.id AND s.generation=w.runtime_generation AND s.terminated_at IS NULL
		WHERE c.delivered_at IS NULL AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.run_id=c.run_id)
		ORDER BY c.run_id,c.queue_sequence
		LIMIT $1`, limit)
	return Object{"commands": rows}
}

func recordThreadCommandDelivered(t *transaction, r *ControlRequest) Object {
	id, execution := r.Body.S("commandId"), r.Body.S("executionId")
	cmd := t.one("SELECT * FROM thread_commands WHERE id=$1", id)
	require(cmd != nil, 404, "not_found")
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='agent_session'", execution, cmd.S("runId"))
	require(e != nil, 409, "dispatch_conflict")
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
		canon := jsonText(Object{"record": record, "sequence": seq, "truncated": ev.B("truncated"), "turnId": ev.S("turnId")})
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
