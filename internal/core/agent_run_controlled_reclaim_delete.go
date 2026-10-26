package core

func registerControlledDataDelete(t *transaction, r *ResidualManagementRequest, actor string) Object {
	require(r.DataDeleteConfirmed, 400, "data_delete_confirmation_required")
	root := t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1 AND tenant_id=$2 AND command_kind='terminate'", nullable(r.IntentID), r.TenantID)
	require(root != nil, 404, "residual_not_found")
	require(controlledRootRow(t, root)["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	if child := t.one("SELECT * FROM agent_run_reclamation_intents WHERE root_intent_id=$1", root.S("id")); child != nil {
		return child
	}
	require(root.S("state") == "succeeded" && root.S("disposition") == "terminated_data_retained", 409, "termination_unconfirmed")
	row := refreshControlledTargets(t, root)
	require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	require(controlledTerminationComplete(t, root, row), 409, "termination_unconfirmed")
	_, w, p := residualIdentity(t, row)
	require(p.S("lifecycle") == "active" && w["deletedAt"] == nil && !w.B("admissionOpen"), 409, "resource_unavailable")
	require(t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", p.S("id")) == nil, 409, "resource_in_use")
	id := newID()
	t.exec(`INSERT INTO agent_run_reclamation_intents(id,root_intent_id,command_kind,source,tenant_id,project_id,run_id,workspace_id,runtime_generation,legacy_delete_operation_id,control_epoch,actor_subject,operator_label,reason,data_delete_confirmed,legacy_delete_operation_missing)
 VALUES($1,$2,'delete_data',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,true,$14)`, id, root.S("id"), root.S("source"), root.S("tenantId"), root.S("projectId"), root.S("runId"), root.S("workspaceId"), root.N("runtimeGeneration"), nullable(root.S("legacyDeleteOperationId")), root.N("controlEpoch"), actor, r.OperatorLabel, r.Reason, root.B("legacyDeleteOperationMissing"))
	snapshot := Object{}
	for key, value := range w {
		if key != "issueRunId" {
			snapshot[key] = value
		}
	}
	closeAdmission(t, w, "deleted")
	request := Object{"previous": Object{w.S("id"): snapshot}, "reclaimId": root.S("reclaimId"), "controlledIntentId": root.S("id"), "dataDeleteIntentId": id}
	operationActor := w.S("creatorUserId")
	if operationActor == "" && root.S("legacyDeleteOperationId") != "" {
		// Legacy creator evidence may be absent. Keep the original deletion's tenant actor
		// reference; the deployment operator's authenticated identity stays on the intent.
		operationActor = t.one("SELECT actor_user_id FROM operations WHERE id=$1", root.S("legacyDeleteOperationId")).S("actorUserId")
	}
	require(validID(operationActor), 409, "residual_actor_unknown")
	op := newOperation(t, &PublicRequest{TenantID: root.S("tenantId"), Key: "controlled-" + root.S("reclaimId")}, operationActor, p.S("id"), w.S("id"), "delete_workspace", "cleanup", jsonText(Object{"intentId": id}), request)
	t.exec("UPDATE agent_run_reclamation_intents SET delete_operation_id=$2 WHERE id=$1", id, op.S("id"))
	if root.S("source") == "residual_ledger" {
		t.exec("UPDATE agent_run_residuals SET reclaim_operation_id=$2,reclaim_version=reclaim_version+1 WHERE run_id=$1 AND reclaim_operation_id IS NULL", root.S("runId"), op.S("id"))
	} else {
		t.exec("UPDATE agent_run_reclamation_intents SET delete_operation_id=$2,version=version+1 WHERE id=$1 AND delete_operation_id IS NULL", root.S("id"), op.S("id"))
	}
	reserveRuntimeMaintenance(t, w.S("id"), op.S("id"))
	controlledAudit(t, id, "data_delete_registered", actor, Object{"dataDeleteConfirmed": true, "operationId": op.S("id")})
	return t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", id)
}

func controlledOperationRoot(t *transaction, op Object) Object {
	if op == nil {
		return nil
	}
	return t.one(`SELECT i.* FROM agent_run_reclamation_intents i LEFT JOIN agent_run_residuals ar ON ar.run_id=i.run_id
 WHERE i.root_intent_id IS NULL AND (i.delete_operation_id=$1 OR (i.source='residual_ledger' AND ar.reclaim_operation_id=$1))`, op.S("id"))
}

func checkControlledOperation(t *transaction, op, root Object) Object {
	row := refreshControlledTargets(t, root)
	_, w, p := residualIdentity(t, row)
	child := t.one("SELECT * FROM agent_run_reclamation_intents WHERE root_intent_id=$1 AND command_kind='delete_data'", root.S("id"))
	require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	require(root.S("state") == "succeeded" && root.S("disposition") == "terminated_data_retained" && child != nil && child.S("state") != "succeeded" &&
		child.B("dataDeleteConfirmed") && op.S("id") == row.S("reclaimOperationId") && op.S("kind") == "delete_workspace" && op.S("step") == "cleanup" &&
		op.S("tenantId") == root.S("tenantId") && op.S("projectId") == p.S("id") && op.S("workspaceId") == w.S("id") &&
		op.O("request").S("controlledIntentId") == root.S("id") && op.O("request").S("dataDeleteIntentId") == child.S("id") &&
		op.O("request").S("reclaimId") == root.S("reclaimId") && !w.B("admissionOpen"), 409, "residual_operation_conflict")
	require(controlledTerminationComplete(t, root, row), 409, "termination_unconfirmed")
	require(t.one("SELECT id FROM operations WHERE project_id=$1 AND id<>$2 AND state IN ('queued','running','retry_wait','blocked')", p.S("id"), op.S("id")) == nil, 409, "resource_in_use")
	t.exec("UPDATE agent_run_reclamation_intents SET state='terminating',started_at=COALESCE(started_at,clock_timestamp()),version=version+1 WHERE id=$1 AND state='registered'", child.S("id"))
	return row
}

func completeControlledDataDelete(t *transaction, op, root Object) bool {
	row := checkControlledOperation(t, op, root)
	effect := t.one("SELECT * FROM external_effects WHERE operation_id=$1 AND workspace_id=$2 AND kind='workspace_data_delete' AND state='succeeded' AND reconciled_epoch=$3", op.S("id"), root.S("workspaceId"), op.N("controllerEpoch"))
	require(effect != nil && effect.O("result").B("removed"), 409, "residual_cleanup_incomplete")
	child := t.one("SELECT * FROM agent_run_reclamation_intents WHERE root_intent_id=$1", root.S("id"))
	if !appendResidualFact(t, row.S("runId"), "controlled:"+child.S("id")+":released", Object{"kind": "reclaim_complete", "state": "succeeded", "intentId": child.S("id"), "operationId": op.S("id"), "effectId": effect.S("id"), "disposition": "released"}) {
		return false
	}
	t.exec("UPDATE agent_run_reclamation_intents SET state='succeeded',disposition='released',completed_at=clock_timestamp(),version=version+1 WHERE id=$1", child.S("id"))
	if root.S("source") == "residual_ledger" {
		t.exec("UPDATE agent_run_residuals SET disposition='released',reclaim_version=reclaim_version+1 WHERE run_id=$1", root.S("runId"))
	}
	controlledAudit(t, child.S("id"), "released", "controller", Object{"effectId": effect.S("id")})
	return true
}

func confirmControlledExternal(t *transaction, r *ResidualManagementRequest, actor string) Object {
	require(r.ResponsibilityTransferred, 400, "external_responsibility_confirmation_required")
	r.EvidenceNote = validText(r.EvidenceNote, 2000)
	intent := t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1 AND tenant_id=$2", nullable(r.IntentID), r.TenantID)
	require(intent != nil, 404, "residual_not_found")
	require(intent.S("state") != "succeeded", 409, "reclamation_intent_completed")
	root := controlledIntentRoot(t, intent)
	row := refreshControlledTargets(t, root)
	require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	open := controlledOpenTargets(t, root, row, intent)
	require(len(r.ExternalFacts) == len(open), 400, "external_target_confirmation_required")
	for _, target := range open {
		validText(r.ExternalFacts[target.S("key")], 2000)
	}
	fact := Object{"kind": "external_disposition", "state": "succeeded", "intentId": intent.S("id"), "disposition": "externally_confirmed", "reason": r.Reason, "evidenceNote": r.EvidenceNote, "externalFacts": r.ExternalFacts, "actorSubject": actor, "responsibilityTransferred": true}
	if !appendResidualFact(t, row.S("runId"), "controlled:"+intent.S("id")+":external", fact) {
		return intent
	}
	t.exec("UPDATE agent_run_reclamation_intents SET state='succeeded',disposition='externally_confirmed',started_at=COALESCE(started_at,clock_timestamp()),completed_at=clock_timestamp(),version=version+1 WHERE id=$1", intent.S("id"))
	if root.S("source") == "residual_ledger" {
		t.exec("UPDATE agent_run_residuals SET reclaim_state='cleared',disposition='externally_confirmed',cleared_at=COALESCE(cleared_at,clock_timestamp()),reclaim_version=reclaim_version+1 WHERE run_id=$1", row.S("runId"))
	}
	if intent.S("commandKind") == "delete_data" {
		t.exec("UPDATE operations SET state='failed',error_code='external_disposition',version=version+1,updated_at=clock_timestamp() WHERE id=$1 AND state IN ('queued','running','retry_wait','blocked')", nullable(row.S("reclaimOperationId")))
	}
	controlledAudit(t, intent.S("id"), "externally_confirmed", actor, fact)
	return t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", intent.S("id"))
}

func controlledOpenTargets(t *transaction, root, row, intent Object) []Object {
	open := []Object{}
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") == "sandbox" && !residualSandboxTerminated(t, row, target.S("id")) {
			open = append(open, Object{"key": "sandbox:" + target.S("id"), "kind": "sandbox", "id": target.S("id")})
		}
	}
	// Terminating only retains data by an operator decision too. External disposition must
	// explicitly account for the volume responsibility instead of silently writing it off.
	open = append(open, Object{"key": "data:" + root.S("workspaceId"), "kind": "data", "id": root.S("workspaceId")})
	return open
}
