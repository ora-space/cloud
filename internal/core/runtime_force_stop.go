package core

// forceStopPublic is an administrator-only intent, independent of the project's ordinary operation.
// Its targets include allocating sandboxes: terminating their stable ensure identity fences late creation.
func forceStopPublic(t *transaction, r *PublicRequest, uid string) Object {
	w := workspace(t, r.TenantID, uid, r.WorkspaceID, true)
	membership(t, r.TenantID, uid, true)
	if r.Method == "GET" {
		return Object{"forceStop": t.one("SELECT * FROM runtime_force_stops WHERE workspace_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1", w.S("id"))}
	}
	version(w, r.Body.N("version"))
	require(r.Body.B("impactConfirmed"), 400, "force_stop_impact_confirmation_required")
	reason := validText(r.Body.S("reason"), 2000)
	require(t.one("SELECT id FROM runtime_force_stops WHERE workspace_id=$1 AND state<>'succeeded'", w.S("id")) == nil, 409, "force_stop_pending")
	c := runtimeControl(t, w.S("id"))
	// A legacy runtime without a stable sandbox target cannot be proven absent by Cloud rows.
	if w.S("creatorEvidence") == "unknown" {
		require(t.one("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", w.S("id")) != nil, 409, "force_stop_target_unknown")
	}
	id := newID()
	t.exec("INSERT INTO runtime_force_stops(id,tenant_id,workspace_id,actor_user_id,reason,state,control_epoch,runtime_generation) VALUES($1,$2,$3,$4,$5,'registered',$6,$7)", id, r.TenantID, w.S("id"), uid, reason, c.N("controlEpoch"), w.N("runtimeGeneration"))
	for _, sb := range t.list("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL ORDER BY id", w.S("id")) {
		t.exec("INSERT INTO runtime_force_stop_targets(id,force_stop_id,sandbox_id) VALUES($1,$2,$3)", newID(), id, sb.S("id"))
	}
	closeAdmission(t, w, "stopped")
	t.exec("UPDATE runtime_controls SET state='reconciling',binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", w.S("id"))
	auditRuntimeControl(t, runtimeControl(t, w.S("id")), "administrative_force_stop", uid)
	return Object{"resource": adminResource(t.one("SELECT * FROM workspaces WHERE id=$1", w.S("id"))), "forceStop": t.one("SELECT * FROM runtime_force_stops WHERE id=$1", id)}
}

func requireNoForceStop(t *transaction, wid string) {
	require(t.one("SELECT id FROM runtime_force_stops WHERE workspace_id=$1 AND state<>'succeeded'", wid) == nil, 409, "force_stop_pending")
}

// Force-stop commands never reuse or delete an ordinary operation. Evidence is exact-target
// Docker termination with a durable tombstone, not a user session timeout or a fabricated Node result.
func forceStopCommand(t *transaction, r *ControlRequest) Object {
	switch r.Action {
	case "force_pending":
		plans := []Object{}
		for _, f := range t.list("SELECT * FROM runtime_force_stops WHERE state<>'succeeded' ORDER BY created_at,id") {
			p := t.one("SELECT project_id FROM workspaces WHERE id=$1", f.S("workspaceId"))
			targets := []Object{}
			for _, target := range t.list("SELECT * FROM runtime_force_stop_targets WHERE force_stop_id=$1 ORDER BY id", f.S("id")) {
				targets = append(targets, Object{"id": target.S("id"), "workspaceId": f.S("workspaceId"), "kind": "sandbox_terminate", "state": target.S("state"), "reconciledEpoch": r.Body.N("epoch"), "request": Object{"kind": "sandbox_terminate", "projectId": p.S("projectId"), "workspaceId": f.S("workspaceId"), "sandboxInstanceId": target.S("sandboxId")}})
			}
			plans = append(plans, Object{"forceStop": f, "effects": targets})
		}
		return Object{"plans": plans}
	case "force_confirm":
		f := t.one("SELECT * FROM runtime_force_stops WHERE id=$1", r.Body.S("forceStopId"))
		require(f != nil, 404, "not_found")
		version(f, r.Body.N("version"))
		require(f.S("state") != "succeeded", 409, "force_stop_completed")
		if targetID := r.Body.S("effectId"); targetID != "" {
			target := t.one("SELECT * FROM runtime_force_stop_targets WHERE id=$1 AND force_stop_id=$2", targetID, f.S("id"))
			require(target != nil, 404, "not_found")
			require(r.Body.B("terminated") && r.Body.B("lateEnsureFenced"), 409, "termination_unconfirmed")
			t.exec("UPDATE runtime_force_stop_targets SET state='succeeded',result=$2,confirmed_at=clock_timestamp() WHERE id=$1", targetID, jsonText(Object{"terminated": true, "lateEnsureFenced": true}))
		}
		t.exec("UPDATE runtime_force_stops SET state='terminating',controller_epoch=$2,version=version+1 WHERE id=$1", f.S("id"), r.Body.N("epoch"))
		if t.one("SELECT id FROM runtime_force_stop_targets WHERE force_stop_id=$1 AND state<>'succeeded'", f.S("id")) == nil {
			// Every old sandbox was selected before dispatch closed; no future ensure plan can be created.
			for _, target := range t.list("SELECT * FROM runtime_force_stop_targets WHERE force_stop_id=$1", f.S("id")) {
				t.exec("UPDATE execution_tickets a SET terminated_by_force_stop_id=$2 FROM node_instances n WHERE a.node_instance_id=n.id AND n.sandbox_instance_id=$1 AND a.state='active'", target.S("sandboxId"), f.S("id"))
				t.exec("UPDATE node_instances SET ended_at=COALESCE(ended_at,clock_timestamp()),connection_state='ended',version=version+1 WHERE sandbox_instance_id=$1", target.S("sandboxId"))
				t.exec("UPDATE sandbox_instances SET terminated_at=COALESCE(terminated_at,clock_timestamp()),observed_state='terminated' WHERE id=$1", target.S("sandboxId"))
			}
			t.exec("UPDATE clone_executions SET terminated_by_force_stop_id=$2 WHERE workspace_id=$1 AND result IS NULL", f.S("workspaceId"), f.S("id"))
			t.exec("UPDATE node_executions SET terminated_by_force_stop_id=$2 WHERE workspace_id=$1 AND result IS NULL", f.S("workspaceId"), f.S("id"))
			// Cancellation is recorded as failure with the force intent reference; the original input,
			// actor, execution identity and any real result remain intact. Project-wide deletion resumes.
			starting := t.one("SELECT w.issue_run_id FROM workspaces w JOIN operations o ON o.workspace_id=w.id WHERE w.id=$1 AND o.kind='create_workspace' AND o.state IN ('queued','running','retry_wait','blocked')", f.S("workspaceId"))
			t.exec("UPDATE operations SET state='failed',error_code='administrative_force_stop',result=result||jsonb_build_object('forceStopId',$2::text),version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1 AND state IN ('queued','running','retry_wait','blocked')", f.S("workspaceId"), f.S("id"))
			if starting.S("issueRunId") != "" {
				runWorkspaceSettled(t, starting.S("issueRunId"), "failed")
			}
			// Canceling initialization preserves the shared project and the incomplete runtime.
			// A member can create a new independent runtime; start never silently re-clones this one.
			settleFailedProjectCreation(t, t.one("SELECT project_id FROM workspaces WHERE id=$1", f.S("workspaceId")).S("projectId"))
			t.exec("UPDATE workspaces SET desired_state='stopped',observed_state='stopped',admission_open=false,version=version+1 WHERE id=$1", f.S("workspaceId"))
			t.exec("UPDATE runtime_force_stops SET state='succeeded',confirmed_at=clock_timestamp() WHERE id=$1", f.S("id"))
			reconcileRuntimeControls(t)
		}
		return t.one("SELECT * FROM runtime_force_stops WHERE id=$1", f.S("id"))
	default:
		reject(404, "not_found")
	}
	return nil
}

// effectPermit is a fresh read, deliberately outside idempotent submission replay.
func effectPermit(t *transaction, r *ControlRequest) Object {
	var wid, sid, tid string
	var generation, controlEpoch int64
	if fid := r.Body.S("forceStopId"); fid != "" {
		f := t.one("SELECT f.*,w.project_id FROM runtime_force_stops f JOIN workspaces w ON w.id=f.workspace_id WHERE f.id=$1 AND f.state<>'succeeded'", fid)
		require(f != nil, 409, "force_stop_completed")
		target := t.one("SELECT * FROM runtime_force_stop_targets WHERE id=$1 AND force_stop_id=$2", r.EffectID, fid)
		require(target != nil && target.S("state") == "planned", 409, "stale_effect")
		wid, sid, tid, generation, controlEpoch = f.S("workspaceId"), target.S("sandboxId"), f.S("tenantId"), f.N("runtimeGeneration"), f.N("controlEpoch")
	} else {
		e := t.one("SELECT e.*,o.state AS operation_state,o.controller_epoch,w.tenant_id,w.runtime_generation FROM external_effects e JOIN operations o ON o.id=e.operation_id JOIN workspaces w ON w.id=e.workspace_id WHERE e.id=$1", r.EffectID)
		require(e != nil && e.S("operationState") == "running" && e.N("controllerEpoch") == r.Body.N("epoch"), 409, "stale_effect")
		wid, tid, generation = e.S("workspaceId"), e.S("tenantId"), e.N("runtimeGeneration")
		requireNoForceStop(t, wid)
		c := runtimeControl(t, wid)
		require(c.S("state") == "maintenance" && c.S("maintenanceOperationId") == e.S("operationId"), 409, "runtime_control_required")
		controlEpoch = c.N("controlEpoch")
		sid = e.O("request").S("sandboxInstanceId")
		if e.S("kind") == "sandbox_ensure" {
			sid = e.S("id")
		}
		require(e.S("kind") == "sandbox_ensure" || e.S("kind") == "sandbox_terminate" || e.S("kind") == "workspace_data_delete", 409, "executor_capability_unavailable")
	}
	clock := t.one("SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS issued_at_ms,floor(extract(epoch FROM LEAST(clock_timestamp()+interval '10 seconds',(SELECT expires_at FROM controller_leases WHERE name='global')))*1000)::bigint AS expires_at_ms")
	return Object{"tenantId": tid, "workspaceId": wid, "sandboxId": sid, "runtimeGeneration": generation, "controlEpoch": controlEpoch, "controllerEpoch": r.Body.N("epoch"), "issuedAtMs": clock.N("issuedAtMs"), "expiresAtMs": clock.N("expiresAtMs")}
}
