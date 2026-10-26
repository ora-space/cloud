package core

// workflowDeliveryIdentity verifies immutable registration before either an upload grant or a
// late fact. Soft deletion cannot retarget an old execution to a replacement sandbox.
func workflowDeliveryIdentity(t *transaction, e Object) Object {
	runID := e.S("operationId")
	work := t.one("SELECT * FROM execution_work WHERE id=$1 AND workflow_run_id=$2 AND kind='deliver_revision'", e.S("workId"), runID)
	require(work != nil && work.S("executionId") == e.S("executionId") && jsonText(work.O("input")) == jsonText(e.O("input")) && e["terminatedByForceStopId"] == nil, 409, "dispatch_conflict")
	input := e.O("input")
	require(input.O("workflow").S("workflowRunId") == runID && input.S("sessionExecutionId") == "" && len(input.O("priorRevision")) == 0, 409, "dispatch_conflict")
	w := t.one("SELECT w.* FROM workspaces w JOIN workflow_runs r ON r.id=w.workflow_run_id AND r.tenant_id=w.tenant_id AND r.project_id=w.project_id WHERE w.id=$1 AND w.workflow_run_id=$2", e.S("workspaceId"), runID)
	require(w != nil, 409, "dispatch_conflict")
	target := work.O("target")
	require(target.S("workspaceId") == w.S("id") && target.S("nodeId") == e.S("nodeId") && e.S("nodeOperationId") == e.S("executionId"), 409, "dispatch_conflict")
	require(t.one(`SELECT s.id FROM sandbox_instances s JOIN node_instances n ON n.sandbox_instance_id=s.id
 WHERE s.id=$1 AND s.workspace_id=$2 AND s.generation=$3 AND n.node_id=$4
 AND NOT EXISTS(SELECT 1 FROM node_instances replacement WHERE replacement.sandbox_instance_id=s.id AND replacement.ended_at IS NULL AND replacement.node_id<>n.node_id)`, target.S("sandboxInstanceId"), w.S("id"), w.N("runtimeGeneration"), e.S("nodeId")) != nil, 409, "dispatch_conflict")
	return w
}

// liveWorkflowDelivery authorizes only the current independent delivery execution, even on cancel.
func liveWorkflowDelivery(t *transaction, e Object) {
	w := workflowDeliveryIdentity(t, e)
	require(t.one("SELECT id FROM workflow_runs WHERE id=$1 AND delivery_state='delivering' AND status IN ('succeeded','failed','cancelled') AND deleted_at IS NULL", e.S("operationId")) != nil, 409, "dispatch_conflict")
	require(w["deletedAt"] == nil && w.S("observedState") == "ready" && w.B("admissionOpen"), 409, "dispatch_conflict")
	node := currentNode(t, w.S("id"))
	require(node.S("nodeId") == e.S("nodeId"), 409, "dispatch_conflict")
	c := runtimeControl(t, w.S("id"))
	require(c.S("state") == "maintenance" && c.S("maintenanceRunId") == e.S("operationId"), 409, "dispatch_conflict")
}

// settleVerifiedWorkflowRevision never turns a late fact into a new logical result or a delete.
func settleVerifiedWorkflowRevision(t *transaction, r *ControlRequest, e Object, late bool) {
	if late || t.one("SELECT id FROM revisions WHERE workflow_run_id=$1", e.S("operationId")) != nil {
		return
	}
	result := r.Body.O("result")
	outcome := "delivered"
	if result.S("outcome") == "revision_unchanged" {
		outcome = "unchanged"
	}
	var reason any
	if !r.verification.valid {
		outcome, reason = "failed", revisionVerificationFailure
	}
	t.exec("INSERT INTO revision_verifications(execution_id,outcome,reason) VALUES($1,$2,$3)", e.S("executionId"), outcome, reason)
	settled := Object{"outcome": outcome}
	if !r.verification.valid {
		settled["reason"] = reason
	} else {
		w := t.one("SELECT w.*,b.repository_url FROM workspaces w JOIN repository_bindings b ON b.project_id=w.project_id WHERE w.id=$1", e.S("workspaceId"))
		history := result.O("history")
		id := newID()
		t.exec(`INSERT INTO revisions(id,execution_id,tenant_id,workflow_run_id,workspace_id,project_id,repository_url,base_commit,final_commit,revision_ref,delivery_ref,history_key,history_size,history_sha256,history_kind)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'workflow_manifest_v1')`, id, e.S("executionId"), w.S("tenantId"), e.S("operationId"), w.S("id"), w.S("projectId"), w.S("repositoryUrl"), result.S("baseCommit"), result.S("finalCommit"), result.S("revisionRef"), e.O("input").S("deliveryRef"), history.S("key"), history.N("size"), history.S("sha256"))
		settled["revisionId"] = id
		if outcome == "delivered" {
			settled["outcome"] = "saved"
		}
	}
	settleWorkflowDelivery(t, e.S("operationId"), e.S("executionId"), settled)
}
