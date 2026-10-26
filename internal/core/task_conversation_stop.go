package core

// taskStoppableRun prefers the execution owning the task over a later, unclaimed direct Run.
func taskStoppableRun(t *transaction, tid, iid string) Object {
	return t.one(`SELECT * FROM issue_runs WHERE issue_id=$1 AND tenant_id=$2 AND executor_type='agent' AND deleted_at IS NULL
		AND (status IN ('queued','deferred','dispatched','running') OR phase IS NOT NULL AND phase<>'done')
		ORDER BY CASE WHEN phase IS NOT NULL OR status IN ('dispatched','running') THEN 0 ELSE 1 END,created_at,id LIMIT 1`, iid, tid)
}

// taskRecoveryRun returns the first unresolved barrier, including one older than an unclaimed
// direct Run. A later queue item can never be acknowledged to silently skip an earlier failure.
func taskRecoveryRun(t *transaction, tid, iid string) Object {
	return t.one(`SELECT r.* FROM issue_runs r LEFT JOIN task_conversations c ON c.issue_id=r.issue_id
		WHERE r.tenant_id=$1 AND r.issue_id=$2 AND r.executor_type='agent' AND r.deleted_at IS NULL
		AND (r.id=c.paused_run_id OR ((r.status IN ('failed','cancelled') OR r.result->>'deliveryState' IN ('failed','skipped'))
		AND NOT EXISTS (SELECT 1 FROM issue_runs acknowledged WHERE acknowledged.id=c.acknowledged_run_id
		AND (r.created_at,r.id)<=(acknowledged.created_at,acknowledged.id)))) ORDER BY r.created_at,r.id LIMIT 1`, tid, iid)
}

// taskConversationStop requests the existing cancellation lifecycle and freezes a task-level
// pause in the same transaction. A send from another tab cannot race past the persisted barrier.
func taskConversationStop(t *transaction, store *Store, request *PublicRequest, uid string) Object {
	issue(t, request.TenantID, request.IssueID)
	runID := request.Body.S("runId")
	require(validID(runID), 400, "invalid_run")
	latest := taskStoppableRun(t, request.TenantID, request.IssueID)
	require(latest != nil && latest.S("id") == runID, 409, "stale_run")
	require(latest.S("phase") != "done" && (latest.S("phase") != "" || latest.S("status") == "queued" || latest.S("status") == "deferred"), 409, "run_already_stopped")
	require(canEndForTaskHandoff(t, latest, uid), 403, "model_run_end_forbidden")
	ensureTaskConversation(t, request.TenantID, request.IssueID)
	barrier := taskRecoveryRun(t, request.TenantID, request.IssueID)
	require(barrier == nil || barrier.S("id") == runID, 409, "task_paused")
	t.exec("UPDATE task_conversations SET paused_run_id=$2,updated_at=now() WHERE issue_id=$1", request.IssueID, runID)
	t.exec("UPDATE issue_runs SET cancel_requested_at=COALESCE(cancel_requested_at,clock_timestamp()),version=version+1,updated_at=now() WHERE id=$1", runID)
	if latest.S("phase") == "" {
		// An unclaimed queue item owns no external work to cancel or save.
		t.exec(`UPDATE issue_runs SET status='cancelled',phase='done',completed_at=now(),result=COALESCE(result,'{}'::jsonb)||'{"deliveryState":"skipped"}'::jsonb WHERE id=$1`, runID)
		appendActivity(t, request.TenantID, request.IssueID, "user", uid, "run.cancelled", runActivityDetails(runID, "agent", latest.S("executorId"), Object{"deliveryState": "skipped"}))
	} else if threadAcceptStates[latest.S("threadState")] {
		store.endAgentThread(t, latest, "cancelled")
	}
	taskConversationChanged(t, request.TenantID, request.IssueID)
	return Object{"runId": runID, "state": "stopping"}
}
