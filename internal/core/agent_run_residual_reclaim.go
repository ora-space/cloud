package core

// claimResidualReclamation is the single serialized arbitration point for automatic cleanup and
// a future already-authorized controlled intent. It is internal, not a caller-selectable public
// delete flag. Controlled callers must have authenticated/persisted their intent before entry;
// this step implements only its reservation, never a controlled action or effect.
func claimResidualReclamation(t *transaction, runID, mode string, authorization Object) Object {
	require(mode == "owned_delete" || mode == "controlled", 400, "invalid_reclaim_mode")
	row := t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", runID)
	_, w, p := residualIdentity(t, row)
	require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	if row.S("reclaimState") != "reconciling" {
		require(row.S("reclaimMode") == mode, 409, "residual_mode_conflict")
		if mode == "controlled" {
			require(row.O("reclaimAuthorization").S("intentId") == authorization.S("intentId"), 409, "residual_mode_conflict")
		}
		return Object{"residual": row, "operation": residualBoundOperation(t, row), "claimed": false}
	}
	row = refreshResidualTargets(t, row)
	require(!w.B("admissionOpen") && w["deletedAt"] == nil && p.S("lifecycle") == "active", 409, "resource_unavailable")
	if t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", p.S("id")) != nil {
		return Object{"busy": true, "residual": row}
	}
	requireNoForceStop(t, w.S("id"))
	if mode == "owned_delete" && !residualEligibility(t, row) {
		return Object{"eligible": false, "residual": row}
	}
	if mode == "controlled" {
		// No boolean or arbitrary nonempty object is an authority. The future authenticated entry
		// passes the stable identity of its already accepted operator intent. This reservation by
		// itself does not authorize a terminate, data-delete, new operation or final disposition.
		require(authorization.S("kind") == "controlled_intent" && validID(authorization.S("intentId")) && authorization.S("source") != "", 409, "controlled_intent_required")
	}
	id := newID()
	auth := Object{"kind": "residual_owned_delete", "runId": runID, "workspaceId": w.S("id"), "tenantId": row.S("tenantId"), "projectId": p.S("id"), "runtimeGeneration": row.N("runtimeGeneration"), "evidenceVersion": row.N("reclaimVersion")}
	if mode == "controlled" {
		auth["kind"], auth["intentId"], auth["source"] = "controlled_intent", authorization.S("intentId"), authorization.S("source")
	}
	auth["claimedAt"] = t.one("SELECT clock_timestamp()::text AS at").S("at")
	var op Object
	if mode == "owned_delete" {
		snapshot := Object{}
		for key, value := range w {
			if key != "issueRunId" {
				snapshot[key] = value
			}
		}
		closeAdmission(t, w, "deleted")
		request := Object{"previous": Object{w.S("id"): snapshot}, "reclaimId": id}
		op = newOperation(t, &PublicRequest{TenantID: row.S("tenantId"), Key: "residual-" + id}, w.S("creatorUserId"), p.S("id"), w.S("id"), "delete_workspace", "quiesce", requestHash("residual-owned-delete", runID, Object{"reclaimId": id}), request)
		reserveRuntimeMaintenance(t, w.S("id"), op.S("id"))
	}
	t.exec(`UPDATE agent_run_residuals SET reclaim_state='reclaiming',reclaim_id=$2,reclaim_mode=$3,
 reclaim_operation_id=$4,reclaim_authorization=$5,reclaim_version=reclaim_version+1 WHERE run_id=$1 AND reclaim_state='reconciling'`, runID, id, mode, nullable(op.S("id")), jsonText(auth))
	return Object{"claimed": true, "residual": t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", runID), "operation": op}
}

func authorizeResidualOwnedDelete(t *transaction, runID string) Object {
	return claimResidualReclamation(t, runID, "owned_delete", nil)
}

func residualBoundOperation(t *transaction, row Object) Object {
	if row.S("reclaimOperationId") == "" {
		return nil
	}
	return t.one("SELECT * FROM operations WHERE id=$1", row.S("reclaimOperationId"))
}

// residualOperation resolves a persisted binding, including a cleared row for idempotent fact
// reception. A request's reclaimId without the database binding can never obtain this authority.
func residualOperation(t *transaction, op Object) Object {
	if op == nil {
		return nil
	}
	if row := t.one("SELECT * FROM agent_run_residuals WHERE reclaim_operation_id=$1", op.S("id")); row != nil {
		return row
	}
	if root := controlledOperationRoot(t, op); root != nil {
		return controlledRootRow(t, root)
	}
	return nil
}

// checkResidualOperation is called before plan, permit and advance, even when returning an existing
// effect. Ordinary operations return nil only when their Workspace has no residual responsibility.
// A conflicting cleared record remains a durable block, rather than reopening ordinary cleanup.
func checkResidualOperation(t *transaction, op Object) Object {
	if root := controlledOperationRoot(t, op); root != nil {
		return checkControlledOperation(t, op, root)
	}
	row := residualOperation(t, op)
	if row == nil {
		if op.S("workspaceId") != "" {
			requireNoControlledReclamation(t, op.S("workspaceId"), "")
			other := residualForWorkspace(t, op.S("workspaceId"))
			require(other == nil || (other.S("reclaimState") == "cleared" && other["reclaimConflictedAt"] == nil), 409, "residual_operation_required")
		}
		require(op.O("request").S("reclaimId") == "", 409, "residual_operation_required")
		return nil
	}
	_, w, _ := residualIdentity(t, row)
	require(row.S("reclaimState") == "reclaiming" && row.S("reclaimMode") == "owned_delete" &&
		row["reclaimConflictedAt"] == nil && row["clearedAt"] == nil && op.S("kind") == "delete_workspace" &&
		op.S("workspaceId") == w.S("id") && op.S("projectId") == row.S("projectId") && op.S("tenantId") == row.S("tenantId") &&
		op.O("request").S("reclaimId") == row.S("reclaimId") && !w.B("admissionOpen"), 409, "residual_operation_conflict")
	row = refreshResidualTargets(t, row)
	require(residualTargetsLocated(t, row), 409, "residual_target_unknown")
	return row
}

// checkResidualCleanupActivities exempts only this binding's own residual guard. All other activity
// guards remain. Unknown executions are physically closed only when every writable original
// sandbox/ensure identity is dual-confirmed, never by result=NULL or a ticket timeout.
func checkResidualCleanupActivities(t *transaction, w, op Object) {
	require(!executionNonAcceptanceConflicted(t, w.S("id")), 409, "execution_non_acceptance_conflict")
	row := checkResidualOperation(t, op)
	if row != nil && row.S("reclaimMode") == "controlled" {
		return
	}
	if row == nil {
		checkActivities(t, w)
		return
	}
	require(w.S("id") == row.S("workspaceId") && w.S("kind") == "isolated", 409, "residual_identity_conflict")
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE workspace_id=$1 AND run_id<>$2 AND cleared_at IS NULL", w.S("id"), row.S("runId")) == nil, 409, "resource_in_use")
	require(t.one("SELECT id FROM execution_tickets WHERE workspace_id=$1 AND state='active' AND terminated_by_force_stop_id IS NULL", w.S("id")) == nil, 409, "resource_in_use")
	if !residualAllSandboxesTerminated(t, row) {
		require(residualExecutionsTerminal(t, row), 409, "resource_in_use")
	}
	require(residualDeliveryComplete(t, row), 409, "residual_delivery_unknown")
}

// residualDeletionReady is the additional per-effect/permit gate. Planning the one current
// sandbox's termination is allowed; cleanup waits for every original ID, including historical
// ensures which never produced a sandbox row. Additional unconfirmed IDs get no delete effects.
func residualDeletionReady(t *transaction, row Object) bool {
	if row == nil || row["reclaimConflictedAt"] != nil || !residualTargetsLocated(t, row) || !residualDeliveryComplete(t, row) {
		return false
	}
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") == "sandbox" && !residualSandboxTerminated(t, row, target.S("id")) {
			return false
		}
	}
	return true
}

// checkResidualEffect authorizes only the owned-delete topology and the original current sandbox.
// Use this in both planEffect and effectPermit; pass the actual persisted effect for a permit.
func checkResidualEffect(t *transaction, op Object, kind, workspaceID string, effect Object) {
	row := checkResidualOperation(t, op)
	if row == nil {
		return
	}
	require(workspaceID == row.S("workspaceId"), 409, "residual_identity_conflict")
	if row.S("reclaimMode") == "controlled" {
		require(kind == "workspace_data_delete", 409, "residual_effect_forbidden")
		return
	}
	w := t.one("SELECT * FROM workspaces WHERE id=$1", workspaceID)
	checkResidualCleanupActivities(t, w, op)
	require(kind == "sandbox_terminate" || kind == "workspace_data_delete", 409, "residual_effect_forbidden")
	if kind == "workspace_data_delete" {
		require(residualDeletionReady(t, row), 409, "termination_unconfirmed")
		return
	}
	sid := ""
	if effect != nil {
		sid = effect.O("request").S("sandboxInstanceId")
	}
	current := t.one("SELECT * FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", workspaceID)
	if sid == "" {
		sid = current.S("id")
	}
	require(sid != "" && (current.S("id") == sid || residualSandboxTerminated(t, row, sid)), 409, "residual_target_unknown")
	found := false
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") != "sandbox" {
			continue
		}
		if target.S("id") == sid {
			found = true
			continue
		}
		require(residualSandboxTerminated(t, row, target.S("id")), 409, "residual_target_unknown")
	}
	require(found, 409, "residual_target_unknown")
}

// completeResidualReclamation is called from cleanup->done in the SAME transaction that marks the
// operation succeeded and soft-deletes the Workspace. Failure rolls those writes back together.
func completeResidualReclamation(t *transaction, op Object) bool {
	if root := controlledOperationRoot(t, op); root != nil {
		return completeControlledDataDelete(t, op, root)
	}
	row := residualOperation(t, op)
	if row == nil {
		return true
	}
	if row.S("reclaimState") == "cleared" {
		require(row["reclaimConflictedAt"] == nil && row.S("disposition") == "released", 409, "residual_fact_conflict")
		return true
	}
	row = checkResidualOperation(t, op)
	require(residualDeletionReady(t, row), 409, "residual_cleanup_incomplete")
	effect := t.one(`SELECT * FROM external_effects WHERE operation_id=$1 AND workspace_id=$2 AND kind='workspace_data_delete'
 AND state='succeeded' AND reconciled_epoch=$3`, op.S("id"), row.S("workspaceId"), op.N("controllerEpoch"))
	require(effect != nil && effect.O("result").B("removed"), 409, "residual_cleanup_incomplete")
	if !appendResidualFact(t, row.S("runId"), "reclaim:"+row.S("reclaimId")+":complete", Object{"kind": "reclaim_complete", "state": "succeeded", "reclaimId": row.S("reclaimId"), "operationId": op.S("id"), "effectId": effect.S("id"), "disposition": "released"}) {
		return false
	}
	t.exec(`UPDATE agent_run_residuals SET reclaim_state='cleared',disposition='released',cleared_at=clock_timestamp(),
 reclaim_version=reclaim_version+1 WHERE run_id=$1 AND reclaim_state='reclaiming' AND reclaim_conflicted_at IS NULL`, row.S("runId"))
	return true
}

// resumeResidualReclamation keeps the original authority and operation across process restarts.
// retry_wait is already claimable by the normal Controller path. A historically failed operation
// is never revived here, and neither a failed query nor quiesce refusal creates another operation.
func resumeResidualReclamation(t *transaction, row Object) Object {
	if row.S("reclaimMode") != "owned_delete" || row["reclaimConflictedAt"] != nil {
		return Object{"residual": row}
	}
	op := residualBoundOperation(t, row)
	require(op != nil, 409, "residual_operation_missing")
	checkResidualOperation(t, op)
	if op.S("state") == "queued" || op.S("state") == "retry_wait" || op.S("state") == "blocked" || op.S("state") == "running" {
		t.queued = append(t.queued, op.S("id"))
	}
	return Object{"residual": row, "operation": op}
}
