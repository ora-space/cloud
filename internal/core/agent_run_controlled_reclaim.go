package core

import "context"

// ResidualManagementAction selects a deployment-only command, never a tenant HTTP action.
type ResidualManagementAction string

const (
	// ResidualList queries residual responsibilities in a tenant and optional resource scope.
	ResidualList ResidualManagementAction = "list"
	// ResidualGet returns a responsibility's plans, evidence and audit.
	ResidualGet ResidualManagementAction = "get"
	// ResidualTerminate explicitly abandons unconfirmed output and terminates original targets.
	ResidualTerminate ResidualManagementAction = "terminate"
	// ResidualDeleteData separately authorizes deletion after confirmed termination.
	ResidualDeleteData ResidualManagementAction = "delete-data"
	// ResidualConfirmExternal transfers an unfinished intent's responsibility to an operator.
	ResidualConfirmExternal ResidualManagementAction = "confirm-external"
)

// ResidualManagementRequest is accepted only by the local deployment command. Database
// session_user supplies the authenticated actor; OperatorLabel is self-reported, not authority.
type ResidualManagementRequest struct {
	Action                                                                    ResidualManagementAction
	TenantID, ProjectID, RunID, WorkspaceID, OperationID, ReclaimID, IntentID string
	Key, Reason, OperatorLabel, After                                         string
	Limit                                                                     int
	ImpactConfirmed, DataDeleteConfirmed, ResponsibilityTransferred           bool
	// AllowMissingDeleteOperation explicitly selects the D8 variant with no historical delete.
	AllowMissingDeleteOperation bool
	EvidenceNote                string
	ExternalFacts               map[string]string
}

// ManageResidual uses the deployment's database credential, independently of tenant identities.
// A conflict response is returned only after its audit has committed.
func (s *Store) ManageResidual(ctx context.Context, r *ResidualManagementRequest) (Object, error) {
	out, err := s.transact(ctx, func(t *transaction) Object { return manageResidualCommand(t, r) })
	if err == nil && out.S("managementConflict") != "" {
		return nil, &Fault{Status: 409, Code: out.S("managementConflict"), Params: Object{}}
	}
	return out, err
}

func manageResidualCommand(t *transaction, r *ResidualManagementRequest) Object {
	request := *r
	r = &request
	require(validID(r.TenantID), 400, "invalid_tenant")
	for _, id := range []string{r.ProjectID, r.RunID, r.WorkspaceID, r.OperationID, r.ReclaimID, r.IntentID, r.After} {
		require(id == "" || validID(id), 400, "invalid_residual_scope")
	}
	if r.Action == ResidualList {
		return listManagedResiduals(t, r)
	}
	if r.Action == ResidualGet {
		return getManagedResidual(t, r)
	}
	require(r.Action == ResidualTerminate || r.Action == ResidualDeleteData || r.Action == ResidualConfirmExternal, 400, "invalid_residual_action")
	if r.AllowMissingDeleteOperation {
		require(r.Action == ResidualTerminate && r.RunID != "" && r.WorkspaceID != "" && r.OperationID == "", 400, "invalid_residual_scope")
	}
	require(r.Key != "" && len(r.Key) <= 200, 400, "idempotency_key_required")
	r.Reason = validText(r.Reason, 2000)
	require(len(r.OperatorLabel) <= 200, 400, "invalid_operator_label")
	actor := t.one("SELECT session_user::text AS actor").S("actor")
	input := Object{"action": string(r.Action), "tenantId": r.TenantID, "projectId": r.ProjectID, "runId": r.RunID, "workspaceId": r.WorkspaceID, "operationId": r.OperationID, "reclaimId": r.ReclaimID, "intentId": r.IntentID, "reason": r.Reason, "impactConfirmed": r.ImpactConfirmed, "dataDeleteConfirmed": r.DataDeleteConfirmed, "responsibilityTransferred": r.ResponsibilityTransferred, "evidenceNote": r.EvidenceNote, "externalFacts": r.ExternalFacts, "operatorLabel": r.OperatorLabel}
	if r.AllowMissingDeleteOperation {
		// Omit the default so receipts written before this option keep their canonical input.
		input["allowMissingDeleteOperation"] = true
	}
	if old := t.one("SELECT * FROM agent_run_reclamation_command_receipts WHERE tenant_id=$1 AND idempotency_key=$2", r.TenantID, r.Key); old != nil {
		if jsonText(old.O("commandInput")) != jsonText(input) {
			event := Object{"kind": "idempotency_conflict", "actorSubject": actor, "incoming": input, "at": controlledNow(t)}
			t.exec("UPDATE agent_run_reclamation_command_receipts SET audit=audit||jsonb_build_array($2::jsonb) WHERE id=$1", old.S("id"), jsonText(event))
			return Object{"managementConflict": "idempotency_conflict"}
		}
		return managedIntentView(t, old.S("intentId"))
	}
	var intent Object
	switch r.Action {
	case ResidualTerminate:
		intent = registerControlledTermination(t, r, actor)
	case ResidualDeleteData:
		intent = registerControlledDataDelete(t, r, actor)
	case ResidualConfirmExternal:
		intent = confirmControlledExternal(t, r, actor)
	}
	t.exec(`INSERT INTO agent_run_reclamation_command_receipts(id,tenant_id,idempotency_key,command_input,intent_id,actor_subject,result)
 VALUES($1,$2,$3,$4,$5,$6,$7)`, newID(), r.TenantID, r.Key, jsonText(input), intent.S("id"), actor, jsonText(Object{"intentId": intent.S("id")}))
	return managedIntentView(t, intent.S("id"))
}

func controlledNow(t *transaction) string {
	return t.one("SELECT clock_timestamp()::text AS at").S("at")
}

func controlledAudit(t *transaction, id, kind, actor string, detail Object) {
	event := Object{"kind": kind, "actorSubject": actor, "at": controlledNow(t), "detail": detail}
	t.exec("UPDATE agent_run_reclamation_intents SET audit=audit||jsonb_build_array($2::jsonb),version=version+1 WHERE id=$1", id, jsonText(event))
}

func registerControlledTermination(t *transaction, r *ResidualManagementRequest, actor string) Object {
	require(r.ImpactConfirmed, 400, "force_stop_impact_confirmation_required")
	require(r.RunID != "" || r.WorkspaceID != "", 400, "residual_target_required")
	w := t.one(`SELECT * FROM workspaces WHERE tenant_id=$1 AND ($2::uuid IS NULL OR issue_run_id=$2)
 AND ($3::uuid IS NULL OR id=$3) AND ($4::uuid IS NULL OR project_id=$4)`, r.TenantID, nullable(r.RunID), nullable(r.WorkspaceID), nullable(r.ProjectID))
	require(w != nil && w.S("issueRunId") != "" && w.S("kind") == "isolated" && w["deletedAt"] == nil, 404, "residual_not_found")
	if root := controlledRootForWorkspace(t, w.S("id")); root != nil {
		require(controlledRootRow(t, root)["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
		require(r.OperationID == "" || r.OperationID == root.S("legacyDeleteOperationId"), 409, "residual_identity_conflict")
		require(r.AllowMissingDeleteOperation == root.B("legacyDeleteOperationMissing"), 409, "residual_identity_conflict")
		return root
	}
	row := t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", w.S("issueRunId"))
	source := "residual_ledger"
	if row == nil {
		source = "legacy_d8"
		if r.AllowMissingDeleteOperation {
			// D8 may finish before any delete was declared. Only its original closed Run and
			// exact retained Workspace can select this explicitly acknowledged variant.
			require(t.one(`SELECT id FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND workspace_id=$3
 AND executor_type='agent' AND phase='done' AND failure_reason='workspace_unavailable'
 AND status IN ('completed','failed','cancelled')
 AND result->>'deliveryState' IN ('saved','unchanged','failed','skipped')`, w.S("issueRunId"), r.TenantID, w.S("id")) != nil, 409, "legacy_residual_identity_required")
			require(t.one("SELECT id FROM operations WHERE workspace_id=$1 AND kind='delete_workspace' LIMIT 1", w.S("id")) == nil, 409, "legacy_residual_identity_required")
		} else {
			old := t.one(`SELECT o.* FROM operations o JOIN issue_runs i ON i.id=$2 WHERE o.id=$1 AND o.tenant_id=$3
 AND o.workspace_id=$4 AND o.kind='delete_workspace' AND o.state='failed' AND o.error_code='node_unavailable' AND i.phase='done'`, nullable(r.OperationID), w.S("issueRunId"), r.TenantID, w.S("id"))
			require(old != nil, 409, "legacy_residual_identity_required")
		}
		row = Object{"runId": w.S("issueRunId"), "tenantId": r.TenantID, "projectId": w.S("projectId"), "workspaceId": w.S("id"), "runtimeGeneration": w.N("runtimeGeneration"), "responsibility": Object{}, "reclaimTargets": []Object{}}
	} else {
		require(r.OperationID == "" && !r.AllowMissingDeleteOperation, 400, "invalid_residual_scope")
		require(row.S("reclaimState") == "reconciling", 409, "residual_mode_conflict")
		require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	}
	_, _, p := residualIdentity(t, row)
	require(p.S("lifecycle") == "active" && !w.B("admissionOpen"), 409, "resource_unavailable")
	require(t.one("SELECT id FROM operations WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')", p.S("id")) == nil, 409, "resource_in_use")
	requireNoForceStop(t, w.S("id"))
	id, reclaim := newID(), newID()
	c := runtimeControl(t, w.S("id"))
	t.exec(`INSERT INTO agent_run_reclamation_intents(id,command_kind,source,tenant_id,project_id,run_id,workspace_id,runtime_generation,reclaim_id,legacy_delete_operation_id,control_epoch,actor_subject,operator_label,reason,impact_confirmed,legacy_delete_operation_missing)
 VALUES($1,'terminate',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,true,$14)`, id, source, r.TenantID, p.S("id"), row.S("runId"), w.S("id"), row.N("runtimeGeneration"), reclaim, nullable(r.OperationID), c.N("controlEpoch"), actor, r.OperatorLabel, r.Reason, r.AllowMissingDeleteOperation)
	if source == "residual_ledger" {
		claimed := claimResidualReclamation(t, row.S("runId"), "controlled", Object{"kind": "controlled_intent", "intentId": id, "source": "cloudctl"})
		require(claimed.B("claimed"), 409, "resource_in_use")
		row = claimed.O("residual")
		t.exec("UPDATE agent_run_reclamation_intents SET reclaim_id=$2 WHERE id=$1", id, row.S("reclaimId"))
	}
	closeAdmission(t, w, "stopped")
	t.exec(`UPDATE runtime_controls SET state='reconciling',control_epoch=control_epoch+1,session_id=NULL,holder_user_id=NULL,
 expires_at=NULL,binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1`, w.S("id"))
	t.exec("UPDATE agent_run_reclamation_intents SET control_epoch=(SELECT control_epoch FROM runtime_controls WHERE workspace_id=$2) WHERE id=$1", id, w.S("id"))
	root := t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", id)
	refreshControlledTargets(t, root)
	controlledAudit(t, id, "registered", actor, Object{"impactConfirmed": true, "operatorLabel": r.OperatorLabel, "source": source, "legacyDeleteOperationMissing": r.AllowMissingDeleteOperation})
	return t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", id)
}

func controlledRootForWorkspace(t *transaction, wid string) Object {
	if !validID(wid) {
		return nil
	}
	return t.one("SELECT * FROM agent_run_reclamation_intents WHERE workspace_id=$1 AND root_intent_id IS NULL", wid)
}

func controlledIntentRoot(t *transaction, intent Object) Object {
	if intent.S("rootIntentId") == "" {
		return intent
	}
	return t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", intent.S("rootIntentId"))
}

func controlledRootRow(t *transaction, root Object) Object {
	if root.S("source") == "residual_ledger" {
		return t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", root.S("runId"))
	}
	state := "reclaiming"
	if root.S("state") == "succeeded" {
		state = "cleared"
	}
	disposition := root.S("disposition")
	if child := t.one("SELECT disposition FROM agent_run_reclamation_intents WHERE root_intent_id=$1 AND state='succeeded'", root.S("id")); child != nil {
		disposition = child.S("disposition")
	}
	return Object{"runId": root.S("runId"), "tenantId": root.S("tenantId"), "projectId": root.S("projectId"), "workspaceId": root.S("workspaceId"), "runtimeGeneration": root.N("runtimeGeneration"), "responsibility": Object{}, "reclaimId": root.S("reclaimId"), "reclaimMode": "controlled", "reclaimState": state, "reclaimOperationId": root.S("deleteOperationId"), "reclaimTargets": root["targets"], "reclaimEvidence": root["evidence"], "reclaimConflictedAt": root["conflictedAt"], "clearedAt": root["completedAt"], "disposition": disposition, "legacyIntentId": root.S("id")}
}
