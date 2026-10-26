package core

// refreshControlledTargets extends the original responsibility under the same transaction lock
// as planning and dispatch. It never removes an ensure identity merely because it has no sandbox row.
func refreshControlledTargets(t *transaction, root Object) Object {
	row := refreshResidualTargets(t, controlledRootRow(t, root))
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") != "sandbox" {
			continue
		}
		generation := target.N("generation")
		if generation == 0 {
			if sb := t.one("SELECT generation FROM sandbox_instances WHERE id=$1 AND workspace_id=$2", target.S("id"), root.S("workspaceId")); sb != nil {
				generation = sb.N("generation")
			} else {
				generation = root.N("runtimeGeneration")
			}
		}
		request := Object{"kind": "sandbox_terminate", "projectId": root.S("projectId"), "workspaceId": root.S("workspaceId"), "sandboxInstanceId": target.S("id")}
		t.exec(`INSERT INTO agent_run_reclamation_targets(id,root_intent_id,sandbox_id,generation,request)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(root_intent_id,sandbox_id) DO NOTHING`, newID(), root.S("id"), target.S("id"), generation, jsonText(request))
	}
	return row
}

func controlledTerminationComplete(t *transaction, root, row Object) bool {
	if row["reclaimConflictedAt"] != nil || !residualTargetsLocated(t, row) {
		return false
	}
	found := false
	for _, target := range residualObjects(row["reclaimTargets"]) {
		if target.S("kind") == "sandbox" {
			found = true
			if !residualSandboxTerminated(t, row, target.S("id")) {
				return false
			}
		}
	}
	if !found && !residualNeverPlanned(t, row) {
		return false
	}
	return t.one("SELECT id FROM agent_run_reclamation_targets WHERE root_intent_id=$1 AND state<>'succeeded'", root.S("id")) == nil
}

func controlledPendingPlans(t *transaction, r *ControlRequest) []Object {
	plans := []Object{}
	for _, root := range t.list("SELECT * FROM agent_run_reclamation_intents WHERE command_kind='terminate' AND state<>'succeeded' ORDER BY registered_at,id") {
		row := refreshControlledTargets(t, root)
		if row["reclaimConflictedAt"] != nil || !residualTargetsLocated(t, row) {
			continue
		}
		t.exec("UPDATE agent_run_reclamation_intents SET state='terminating',started_at=COALESCE(started_at,clock_timestamp()),last_checked_at=clock_timestamp(),version=version+1 WHERE id=$1 AND state='registered'", root.S("id"))
		root = t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", root.S("id"))
		effects := []Object{}
		for _, target := range t.list("SELECT * FROM agent_run_reclamation_targets WHERE root_intent_id=$1 ORDER BY id", root.S("id")) {
			effects = append(effects, Object{"id": target.S("id"), "workspaceId": root.S("workspaceId"), "kind": "sandbox_terminate", "state": target.S("state"), "reconciledEpoch": r.Body.N("epoch"), "request": target.O("request"), "result": target.O("result")})
		}
		plans = append(plans, Object{"forceStop": root, "effects": effects})
	}
	return plans
}

func controlledForceConfirm(t *transaction, r *ControlRequest, root Object) Object {
	// Completed roots remain evidence intake identities, not reusable execution authority.
	row := refreshControlledTargets(t, root)
	id := r.Body.S("effectId")
	if id != "" {
		target := t.one("SELECT * FROM agent_run_reclamation_targets WHERE id=$1 AND root_intent_id=$2", id, root.S("id"))
		require(target != nil, 404, "not_found")
		fact := Object{"kind": "effect", "effectKind": "sandbox_terminate", "state": "succeeded", "result": Object{"terminated": r.Body.B("terminated"), "lateEnsureFenced": r.Body.B("lateEnsureFenced")}}
		if root.S("state") == "succeeded" {
			if target.S("state") == "succeeded" && jsonText(target.O("result")) != jsonText(fact.O("result")) {
				appendResidualConflict(t, row, "controlled_effect:"+id, Object{"kind": "effect", "state": "succeeded", "result": target.O("result")}, fact)
			} else {
				appendResidualFact(t, row.S("runId"), "controlled_late:"+id, fact)
			}
			return root
		}
		version(root, r.Body.N("version"))
		require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
		require(r.Body.B("terminated") && r.Body.B("lateEnsureFenced"), 409, "termination_unconfirmed")
		if !appendResidualFact(t, row.S("runId"), "controlled_effect:"+id, fact) {
			return root
		}
		t.exec("UPDATE agent_run_reclamation_targets SET state='succeeded',result=$2,confirmed_epoch=$3,confirmed_at=COALESCE(confirmed_at,clock_timestamp()) WHERE id=$1", id, jsonText(fact.O("result")), r.Body.N("epoch"))
		// Only confirmed physical targets change resource observations; business execution results
		// and the historical failed operation are never synthesized or rewritten here.
		t.exec("UPDATE sandbox_instances SET observed_state='terminated',terminated_at=COALESCE(terminated_at,clock_timestamp()) WHERE id=$1 AND workspace_id=$2", target.S("sandboxId"), root.S("workspaceId"))
		t.exec("UPDATE node_instances SET connection_state='ended',ended_at=COALESCE(ended_at,clock_timestamp()),version=version+1 WHERE sandbox_instance_id=$1 AND ended_at IS NULL", target.S("sandboxId"))
	} else {
		require(root.S("state") != "succeeded", 409, "force_stop_completed")
		version(root, r.Body.N("version"))
	}
	t.exec("UPDATE agent_run_reclamation_intents SET state='terminating',started_at=COALESCE(started_at,clock_timestamp()),last_checked_at=clock_timestamp(),version=version+1 WHERE id=$1", root.S("id"))
	completeControlledTermination(t, root)
	return t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", root.S("id"))
}

func completeControlledTermination(t *transaction, root Object) bool {
	root = t.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", root.S("id"))
	if root.S("state") == "succeeded" {
		return root.S("disposition") == "terminated_data_retained"
	}
	row := refreshControlledTargets(t, root)
	if !controlledTerminationComplete(t, root, row) {
		return false
	}
	if !appendResidualFact(t, row.S("runId"), "controlled:"+root.S("id")+":retained", Object{"kind": "reclaim_complete", "state": "succeeded", "intentId": root.S("id"), "disposition": "terminated_data_retained"}) {
		return false
	}
	t.exec("UPDATE agent_run_reclamation_intents SET state='succeeded',disposition='terminated_data_retained',started_at=COALESCE(started_at,clock_timestamp()),completed_at=clock_timestamp(),version=version+1 WHERE id=$1 AND state<>'succeeded'", root.S("id"))
	if root.S("source") == "residual_ledger" {
		t.exec("UPDATE agent_run_residuals SET reclaim_state='cleared',disposition='terminated_data_retained',cleared_at=COALESCE(cleared_at,clock_timestamp()),reclaim_version=reclaim_version+1 WHERE run_id=$1", root.S("runId"))
	}
	t.exec("UPDATE workspaces SET desired_state='stopped',observed_state='stopped',admission_open=false,version=version+1 WHERE id=$1", root.S("workspaceId"))
	controlledAudit(t, root.S("id"), "terminated_data_retained", "controller", Object{})
	return true
}

func controlledEffectPermit(t *transaction, r *ControlRequest, root Object) Object {
	require(root.S("state") != "succeeded", 409, "force_stop_completed")
	row := refreshControlledTargets(t, root)
	require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	require(residualTargetsLocated(t, row), 409, "residual_target_unknown")
	target := t.one("SELECT * FROM agent_run_reclamation_targets WHERE id=$1 AND root_intent_id=$2 AND state='planned'", r.EffectID, root.S("id"))
	require(target != nil, 409, "stale_effect")
	c := runtimeControl(t, root.S("workspaceId"))
	require(c.N("controlEpoch") == root.N("controlEpoch"), 409, "stale_control")
	clock := t.one("SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS issued_at_ms,floor(extract(epoch FROM LEAST(clock_timestamp()+interval '10 seconds',(SELECT expires_at FROM controller_leases WHERE name='global')))*1000)::bigint AS expires_at_ms")
	return Object{"tenantId": root.S("tenantId"), "workspaceId": root.S("workspaceId"), "sandboxId": target.S("sandboxId"), "runtimeGeneration": target.N("generation"), "controlEpoch": root.N("controlEpoch"), "controllerEpoch": r.Body.N("epoch"), "issuedAtMs": clock.N("issuedAtMs"), "expiresAtMs": clock.N("expiresAtMs")}
}

// requireNoControlledReclamation protects ledgerless D8 and retained/external resources too.
// The only exception is the exact dependent delete operation bound to that root responsibility.
func requireNoControlledReclamation(t *transaction, wid, operationID string) {
	root := controlledRootForWorkspace(t, wid)
	if root == nil {
		return
	}
	row := controlledRootRow(t, root)
	require(row["reclaimConflictedAt"] == nil, 409, "residual_fact_conflict")
	if operationID != "" && row.S("reclaimOperationId") == operationID {
		return
	}
	require(root.S("state") == "succeeded" && row.S("disposition") == "released", 409, "resource_in_use")
}
