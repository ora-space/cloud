package core

// runtimeControlCommand is a finite, lease-fenced Controller contract. The Controller may
// acknowledge Node evidence, but cannot assign a holder, epoch or target chosen by a user.
func runtimeControlCommand(t *transaction, r *ControlRequest) Object {
	switch r.Action {
	case "runtime_pending":
		return Object{"bindings": runtimeBindings(t)}
	case "runtime_ack":
		c, n := runtimeDeliveryTarget(t, r)
		require(r.Body.N("controlVersion") == c.N("version"), 409, "stale_runtime_control")
		closed := r.Body.B("inputClosed")
		unfinished, ok := r.Body["unfinishedExecutionIds"].([]any)
		require(ok, 400, "invalid_runtime_evidence")
		for _, id := range unfinished {
			v, ok := id.(string)
			require(ok && v != "" && len(v) <= 256, 400, "invalid_runtime_evidence")
		}
		if closed && (c.S("state") == "held" || c.S("state") == "acquiring") {
			// Node expiry is conservative. Observed closed input cannot be reopened by a late renew,
			// even if the PostgreSQL deadline has not elapsed yet.
			t.exec("UPDATE runtime_controls SET state='draining',binding_confirmed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", c.S("workspaceId"))
			c = runtimeControl(t, c.S("workspaceId"))
			auditRuntimeControl(t, c, "node_input_closed", "")
		}
		if c.S("state") == "draining" || c.S("state") == "reconciling" {
			require(closed, 409, "input_closure_required")
			t.exec("UPDATE runtime_controls SET input_closed=$2,binding_confirmed=false,bound_sandbox_id=$3 WHERE workspace_id=$1", c.S("workspaceId"), len(unfinished) == 0, n.S("sandboxInstanceId"))
			reconcileRuntimeControls(t)
		} else {
			require(!closed && len(unfinished) == 0, 409, "resource_in_use")
			require(c.S("state") == "maintenance" || ((c.S("state") == "acquiring" || c.S("state") == "held") && c.B("valid")), 409, "stale_runtime_control")
			t.exec("UPDATE runtime_controls SET state=CASE WHEN state='acquiring' THEN 'held' ELSE state END,binding_confirmed=true,bound_sandbox_id=$2 WHERE workspace_id=$1", c.S("workspaceId"), n.S("sandboxInstanceId"))
		}
		return controlView(runtimeControl(t, c.S("workspaceId")))
	case "runtime_permit":
		c, n := runtimeDeliveryTarget(t, r)
		requireNoForceStop(t, c.S("workspaceId"))
		require(c.S("state") == "maintenance" && c.B("bindingConfirmed"), 409, "runtime_control_required")
		e := t.one("SELECT * FROM clone_executions WHERE execution_id=$1 AND workspace_id=$2", r.Body.S("executionId"), c.S("workspaceId"))
		step := "clone"
		if e == nil {
			e = t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND workspace_id=$2", r.Body.S("executionId"), c.S("workspaceId"))
			step = "plugin"
		}
		if c.S("maintenanceRunId") != "" {
			require(e != nil && (e.S("kind") == "agent_session" || e.S("kind") == "deliver_revision") && e.S("operationId") == c.S("maintenanceRunId") && e.S("nodeId") == n.S("nodeId") && e["result"] == nil && e["terminatedByForceStopId"] == nil, 409, "stale_execution")
			work := t.one("SELECT target FROM execution_work WHERE id=$1", e.S("workId"))
			require(work.O("target").S("sandboxInstanceId") == n.S("sandboxInstanceId"), 409, "stale_node")
			run := t.one("SELECT r.id FROM issue_runs r JOIN issues i ON i.id=r.issue_id WHERE r.id=$1 AND r.deleted_at IS NULL AND i.deleted_at IS NULL", c.S("maintenanceRunId"))
			w := t.one("SELECT * FROM workspaces WHERE id=$1", c.S("workspaceId"))
			require(run != nil && runtimeUsable(t, w, w.S("creatorUserId")), 403, "runtime_use_forbidden")
			binding := runtimeBinding(t, c, n)
			binding["executionId"], binding["nodeOperationId"] = e.S("executionId"), e.S("nodeOperationId")
			return binding
		}
		require(e != nil && e.S("operationId") == c.S("maintenanceOperationId") && e.S("nodeId") == n.S("nodeId") && e["result"] == nil, 409, "stale_execution")
		o := t.one("SELECT * FROM operations WHERE id=$1", c.S("maintenanceOperationId"))
		require(o.S("state") == "running" && o.N("controllerEpoch") == r.Body.N("epoch") && o.S("step") == step, 409, "execution_closed")
		require(runtimeUsable(t, t.one("SELECT * FROM workspaces WHERE id=$1", c.S("workspaceId")), o.S("actorUserId")), 403, "runtime_use_forbidden")
		binding := runtimeBinding(t, c, n)
		binding["executionId"] = e.S("executionId")
		binding["nodeOperationId"] = e.S("nodeOperationId")
		return binding
	default:
		reject(404, "not_found")
	}
	return nil
}

func runtimeDeliveryTarget(t *transaction, r *ControlRequest) (control, node Object) {
	c := runtimeControl(t, r.Body.S("workspaceId"))
	require(c.N("controlEpoch") == r.Body.N("controlEpoch"), 409, "stale_runtime_control")
	n := t.one(`SELECT n.* FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id
 JOIN workspaces w ON w.id=s.workspace_id WHERE n.id=$1 AND w.id=$2 AND s.generation=w.runtime_generation
 AND s.terminated_at IS NULL AND n.ended_at IS NULL AND n.initialized AND n.connection_state='connected'
 AND n.last_seen_at>clock_timestamp()-interval '30 seconds'`, r.Body.S("nodeInstanceId"), c.S("workspaceId"))
	require(n != nil && (c.S("boundSandboxId") == "" || c.S("boundSandboxId") == n.S("sandboxInstanceId")), 409, "stale_node")
	return c, n
}

func runtimeBindings(t *transaction) []Object {
	rows := t.list(`SELECT c.* FROM runtime_controls c JOIN workspaces w ON w.id=c.workspace_id
 WHERE c.state<>'idle' AND w.deleted_at IS NULL ORDER BY c.workspace_id`)
	out := []Object{}
	for _, c := range rows {
		n := t.one(`SELECT n.* FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id
 JOIN workspaces w ON w.id=s.workspace_id WHERE w.id=$1 AND s.generation=w.runtime_generation
 AND s.terminated_at IS NULL AND n.ended_at IS NULL AND n.initialized AND n.connection_state='connected'
 AND n.last_seen_at>clock_timestamp()-interval '30 seconds'`, c.S("workspaceId"))
		if n != nil {
			out = append(out, runtimeBinding(t, runtimeControl(t, c.S("workspaceId")), n))
		}
	}
	return out
}

func runtimeBinding(t *transaction, c, n Object) Object {
	s := t.one("SELECT * FROM sandbox_instances WHERE id=$1", n.S("sandboxInstanceId"))
	w := t.one("SELECT * FROM workspaces WHERE id=$1", c.S("workspaceId"))
	actor := c.S("holderUserId")
	if c.S("maintenanceOperationId") != "" {
		actor = t.one("SELECT actor_user_id FROM operations WHERE id=$1", c.S("maintenanceOperationId")).S("actorUserId")
	}
	operation := c.S("maintenanceOperationId")
	if c.S("maintenanceRunId") != "" {
		operation = c.S("maintenanceRunId")
		actor = w.S("creatorUserId")
	}
	clock := t.one("SELECT floor(extract(epoch FROM clock_timestamp())*1000)::bigint AS issued_at_ms,floor(extract(epoch FROM LEAST(CASE WHEN $2::boolean THEN clock_timestamp()+interval '60 seconds' ELSE COALESCE($1::timestamptz,clock_timestamp()+interval '60 seconds') END,(SELECT expires_at FROM controller_leases WHERE name='global')))*1000)::bigint AS expires_at_ms", c["expiresAt"], c.S("state") == "draining" || c.S("state") == "reconciling")
	return Object{"workspaceId": w.S("id"), "tenantId": w.S("tenantId"), "controlEpoch": c.N("controlEpoch"), "controlVersion": c.N("version"), "sessionId": c.S("sessionId"), "actorUserId": actor, "operationId": operation, "inputClosed": c.S("state") == "draining" || c.S("state") == "reconciling", "sandboxId": s.S("id"), "runtimeGeneration": s.N("generation"), "nodeInstanceId": n.S("id"), "nodeId": n.S("nodeId"), "nodeIncarnationId": n.S("nodeIncarnationId"), "issuedAtMs": clock.N("issuedAtMs"), "expiresAtMs": clock.N("expiresAtMs")}
}
