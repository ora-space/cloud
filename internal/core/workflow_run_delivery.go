package core

import (
	"context"
	"time"
)

// prepareWorkflowRunDelivery owns the terminal save barrier. Execution success never implies
// permission to delete a checkout; unknown writers and failed storage retain the workspace.
func prepareWorkflowRunDelivery(t *transaction, runID string) bool {
	run := t.one("SELECT * FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if run == nil || (run.S("status") != "succeeded" && run.S("status") != "failed" && run.S("status") != "cancelled") {
		return false
	}
	switch run.S("deliveryState") {
	case "saved", "unchanged", "not_required":
		return true
	case "failed", "skipped":
		return false
	}
	// Undispatched sessions can no longer start. Registered attempts remain evidence forever.
	t.exec("DELETE FROM execution_work WHERE workflow_run_id=$1 AND kind='agent_session' AND execution_id IS NULL", runID)
	w := t.one("SELECT * FROM workspaces WHERE workflow_run_id=$1", runID)
	sessions := t.list("SELECT * FROM node_executions WHERE operation_id=$1 AND kind='agent_session' ORDER BY workflow_execution_order NULLS LAST,execution_id", runID)
	if len(sessions) == 0 {
		t.exec("UPDATE workflow_runs SET delivery_state='not_required',delivery_settled_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND delivery_state='pending'", runID)
		return true
	}
	if w == nil || w["deletedAt"] != nil {
		return false
	}
	if run["deliveryStartedAt"] == nil {
		t.exec("UPDATE workflow_runs SET delivery_started_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND delivery_started_at IS NULL", runID)
		run = t.one("SELECT * FROM workflow_runs WHERE id=$1", runID)
	}
	if workflowDeliveryGivenUp(t, run, w.S("id")) {
		failWorkflowDelivery(t, runID, "workspace_unavailable")
		return false
	}
	for _, session := range sessions {
		if session["workflowExecutionOrder"] == nil {
			failWorkflowDelivery(t, runID, "history_unavailable")
			return false
		}
		if len(session.O("result")) == 0 || session["terminatedByForceStopId"] != nil {
			// A graph deadline is not process-stop evidence. Keep asking the original attempt to end.
			if len(session.O("result")) == 0 && session["terminatedByForceStopId"] == nil && workflowSessionTarget(t, w.S("id")) != nil && t.one("SELECT id FROM thread_commands WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='end_session'", runID, session.S("workflowNodeId"), session.N("workflowNodeAttempt")) == nil {
				enqueueWorkflowThreadCommand(t, runID, session.S("workflowNodeId"), session.N("workflowNodeAttempt"), "end_session", Object{"reason": "cancelled"})
			}
			return false
		}
	}
	if run.S("deliveryState") == "delivering" {
		return false
	}
	reason := ""
	if t.objectStore == nil {
		reason = "object_store_unconfigured"
	} else if t.store.GitServer == nil {
		reason = gitServerUnconfigured
	}
	if reason != "" {
		t.exec("UPDATE workflow_runs SET delivery_state='skipped',delivery_error=$2,delivery_settled_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND delivery_state='pending'", runID, reason)
		return false
	}
	node := workflowSessionTarget(t, w.S("id"))
	// Cloud cannot issue a shape the target Node has never advertised understanding.
	if node == nil || !node.B("workflowHistoryDelivery") {
		return false
	}
	checkout := sessions[0].O("input").S("checkoutExecutionId")
	for _, session := range sessions {
		require(session.S("workspaceId") == w.S("id") && session.S("nodeId") == node.S("nodeId") && session.O("input").S("checkoutExecutionId") == checkout, 409, "dispatch_conflict")
	}
	t.exec("UPDATE workflow_runs SET delivery_state='delivering',delivery_started_at=COALESCE(delivery_started_at,now()),version=version+1,updated_at=now() WHERE id=$1 AND delivery_state='pending'", runID)
	enqueueWorkflowDelivery(t, runID, Object{"workspaceId": w.S("id"), "sandboxInstanceId": node.S("sandboxInstanceId"), "nodeId": node.S("nodeId")}, time.Time{})
	return false
}

// enqueueWorkflowDelivery fixes every key/ref before dispatch and reuses an already queued item.
func enqueueWorkflowDelivery(t *transaction, runID string, target Object, available time.Time) Object {
	if existing := t.one("SELECT * FROM execution_work WHERE workflow_run_id=$1 AND kind='deliver_revision' AND execution_id IS NULL", runID); existing != nil {
		return existing
	}
	run := t.one("SELECT * FROM workflow_runs WHERE id=$1 AND delivery_state='delivering' AND deleted_at IS NULL", runID)
	require(run != nil, 409, "dispatch_conflict")
	w := t.one("SELECT * FROM workspaces WHERE workflow_run_id=$1 AND deleted_at IS NULL", runID)
	require(w != nil && w.S("id") == target.S("workspaceId") && t.store.GitServer != nil, 409, "dispatch_conflict")
	sessions := t.list("SELECT * FROM node_executions WHERE operation_id=$1 AND kind='agent_session' ORDER BY workflow_execution_order NULLS LAST,execution_id", runID)
	require(len(sessions) > 0, 409, "dispatch_conflict")
	checkoutID := sessions[0].O("input").S("checkoutExecutionId")
	checkout := t.one("SELECT input FROM clone_executions WHERE execution_id=$1 AND workspace_id=$2 AND result->>'outcome'='clone_ready'", checkoutID, w.S("id"))
	require(checkout != nil && commitID(w.S("baseCommitId")), 409, "dispatch_conflict")
	id := newID()
	prefix := "revisions/" + run.S("tenantId") + "/" + runID + "/" + id + "/"
	attempts := make([]Object, 0, len(sessions))
	for _, session := range sessions {
		require(session["workflowExecutionOrder"] != nil, 409, "dispatch_conflict")
		require(session.S("workspaceId") == w.S("id") && session.S("nodeId") == target.S("nodeId") && session.O("input").S("checkoutExecutionId") == checkoutID && session.O("result").S("outcome") == agentSessionEndedOutcome && session["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
		attempts = append(attempts, Object{"executionOrder": session.N("workflowExecutionOrder"), "nodeId": session.S("workflowNodeId"), "attempt": session.N("workflowNodeAttempt"), "executionId": session.S("executionId"), "historyKey": prefix + session.S("workId") + ".jsonl"})
	}
	input := Object{"kind": "deliver_revision", "sessionExecutionId": "", "checkoutExecutionId": checkoutID, "baseCommit": w.S("baseCommitId"), "revisionRef": "refs/ora/revisions/" + runID, "historyKey": prefix + "manifest.json", "gitRepositoryUrl": t.store.GitServer.gitRepositoryURL(w.S("tenantId"), w.S("projectId")), "deliveryRef": GitDeliveryRefPrefix + id, "workspaceRef": checkout.O("input").S("workspaceRef"), "workflow": Object{"workflowRunId": runID, "sessions": attempts}}
	if available.IsZero() {
		available = time.Now()
	}
	t.exec("INSERT INTO execution_work(id,workflow_run_id,kind,input,target,available_at) VALUES($1,$2,'deliver_revision',$3,$4,$5)", id, runID, jsonText(input), jsonText(target), available)
	t.workSignals = append(t.workSignals, runID)
	return t.one("SELECT * FROM execution_work WHERE id=$1", id)
}

// workflowDeliveryGivenUp uses the existing deployment limits without turning a give-up into deletion.
func workflowDeliveryGivenUp(t *transaction, run Object, wid string) bool {
	if run["deliveryStartedAt"] == nil {
		return false
	}
	if t.store.DeliveryGiveUpAfter > 0 && t.one("SELECT id FROM workflow_runs WHERE id=$1 AND delivery_started_at<=clock_timestamp()-make_interval(secs=>$2)", run.S("id"), t.store.DeliveryGiveUpAfter.Seconds()) != nil {
		return true
	}
	return t.store.DeliveryUnreachableAfter > 0 && runWorkspaceNodeUnknown(t, run.S("id"), wid, t.store.DeliveryUnreachableAfter)
}

// failWorkflowDelivery retains the unsaved workspace as an explicit residual, never released.
func failWorkflowDelivery(t *transaction, runID, reason string) {
	t.exec("DELETE FROM execution_work WHERE workflow_run_id=$1 AND kind='deliver_revision' AND execution_id IS NULL", runID)
	t.exec("UPDATE workflow_runs SET delivery_state='failed',delivery_error=$2,delivery_settled_at=now(),workspace_release_state='abandoned',workspace_release_error=$2,version=version+1,updated_at=now() WHERE id=$1 AND delivery_state IN ('pending','delivering')", runID, reason)
}

// settleWorkflowDelivery commits with verification, Revision and receipt in the caller's transaction.
func settleWorkflowDelivery(t *transaction, runID, executionID string, settled Object) {
	run := t.one("SELECT * FROM workflow_runs WHERE id=$1", runID)
	require(run != nil, 409, "dispatch_conflict")
	if run.S("deliveryState") != "delivering" {
		return
	}
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2 AND kind='deliver_revision'", executionID, runID)
	require(e != nil, 409, "dispatch_conflict")
	switch settled.S("outcome") {
	case "saved", "unchanged":
		revision := t.one("SELECT id FROM revisions WHERE id=$1 AND workflow_run_id=$2", settled.S("revisionId"), runID)
		require(revision != nil, 409, "dispatch_conflict")
		t.exec("UPDATE workflow_runs SET delivery_state=$2,delivery_revision_id=$3,delivery_error='',delivery_settled_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND delivery_state='delivering'", runID, settled.S("outcome"), revision.S("id"))
		declareWorkflowRunWorkspaceRelease(t, runID)
	case "failed":
		w := t.one("SELECT id FROM workspaces WHERE workflow_run_id=$1", runID)
		if workflowDeliveryGivenUp(t, run, w.S("id")) {
			failWorkflowDelivery(t, runID, settled.S("reason"))
			return
		}
		work := t.one("SELECT target FROM execution_work WHERE id=$1 AND workflow_run_id=$2", e.S("workId"), runID)
		count := t.one("SELECT count(*) AS n FROM node_executions WHERE operation_id=$1 AND kind='deliver_revision'", runID).N("n")
		t.exec("UPDATE workflow_runs SET delivery_error=$2,version=version+1,updated_at=now() WHERE id=$1", runID, settled.S("reason"))
		enqueueWorkflowDelivery(t, runID, work.O("target"), time.Now().Add(deliveryRetryBackoff(int(count))))
	default:
		reject(409, "invalid_delivery_verification")
	}
}

// advanceWorkflowDeliveries is bounded and serializes each run with the scheduler's own right.
func (s *Store) advanceWorkflowDeliveries(ctx context.Context) error {
	ids, err := s.scanWorkflowDeliveries(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, _, err := s.transactWorkflowRun(ctx, id, func(t *transaction) Object {
			if prepareWorkflowRunDelivery(t, id) {
				declareWorkflowRunWorkspaceRelease(t, id)
			}
			return Object{}
		})
		if err != nil {
			continue
		}
	}
	return nil
}

// scanWorkflowDeliveries closes its cursor before any per-run transaction begins.
func (s *Store) scanWorkflowDeliveries(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `SELECT r.id FROM workflow_runs r WHERE r.deleted_at IS NULL
        AND r.status IN ('succeeded','failed','cancelled') AND r.delivery_state IN ('pending','delivering')
        AND NOT EXISTS(SELECT 1 FROM workspaces w WHERE w.workflow_run_id=r.id AND w.deleted_at IS NOT NULL)
        ORDER BY r.updated_at,r.id LIMIT $1`, workflowAdvanceBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
