package core

func access(t *transaction, r *ControlRequest) Object {
	require(r.Identity != nil, 401, "user_credential_required")
	u := identity(t, r.Identity.Source, r.Identity.Subject, r.Identity.DisplayName)
	tid, wid := r.Body.S("tenantId"), r.Body.S("workspaceId")
	membership(t, tid, u.S("id"), false)
	w := workspace(t, tid, u.S("id"), wid, false)
	p := project(t, tid, u.S("id"), w.S("projectId"))
	action := r.Body.S("action")
	require(action == "read" || action == "execute", 400, "invalid_action")
	executable := w.B("admissionOpen") && w.S("desiredState") == "running" && w.S("observedState") == "ready" && p.S("lifecycle") == "active"
	n := t.one("SELECT n.id,s.id AS sandbox_id,s.generation FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id WHERE s.workspace_id=$1 AND s.generation=$2 AND s.terminated_at IS NULL AND n.ended_at IS NULL AND n.initialized AND n.connection_state='connected' AND n.last_seen_at>clock_timestamp()-interval '30 seconds'", wid, w.N("runtimeGeneration"))
	executable = executable && n != nil
	if action == "execute" {
		require(executable, 409, "execution_closed")
	}
	if r.Action == "access" {
		return Object{"userId": u.S("id"), "tenantId": tid, "workspaceId": wid, "allowedAction": action, "executable": executable, "runtimeGeneration": w.N("runtimeGeneration")}
	}
	require(action == "execute", 400, "invalid_action")
	ticketID := r.Body.S("ticketId")
	kind := r.Body.S("kind")
	require(validID(ticketID) && (kind == "task" || kind == "interaction"), 400, "invalid_ticket")
	existing := t.one("SELECT * FROM execution_tickets WHERE id=$1", ticketID)
	if existing != nil {
		require(existing.S("tenantId") == tid && existing.S("workspaceId") == wid && existing.S("actorUserId") == u.S("id") && existing.S("kind") == kind, 409, "idempotency_conflict")
		return existing
	}
	t.exec("INSERT INTO execution_tickets(id,tenant_id,workspace_id,node_instance_id,actor_user_id,admission_epoch,kind,state) VALUES($1,$2,$3,$4,$5,$6,$7,'active')", ticketID, tid, wid, n.S("id"), u.S("id"), w.N("admissionEpoch"), kind)
	return t.one("SELECT * FROM execution_tickets WHERE id=$1", ticketID)
}

func nodeCommand(t *transaction, r *ControlRequest) Object {
	c := r.Service
	require(validID(c.Subject) && validID(c.WorkspaceID) && validID(c.SandboxID) && c.Generation > 0, 403, "node_scope_required")
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND runtime_generation=$2 AND deleted_at IS NULL", c.WorkspaceID, c.Generation)
	require(w != nil, 409, "stale_node")
	sandbox := t.one("SELECT * FROM sandbox_instances WHERE id=$1 AND workspace_id=$2 AND generation=$3 AND terminated_at IS NULL AND substrate_sandbox_id IS NOT NULL", c.SandboxID, c.WorkspaceID, c.Generation)
	require(sandbox != nil && sandbox.S("observedState") != "terminating", 409, "stale_sandbox")
	if r.Action == "node_register" {
		require(r.Body.N("protocolVersion") == 1, 400, "unsupported_protocol")
		require(w.S("desiredState") == "running", 409, "execution_closed")
		existing := t.one("SELECT * FROM node_instances WHERE id=$1", c.Subject)
		if existing != nil {
			require(existing.S("sandboxInstanceId") == c.SandboxID && existing["endedAt"] == nil, 409, "stale_node")
			return existing
		}
		require(t.one("SELECT id FROM node_instances WHERE sandbox_instance_id=$1 AND ended_at IS NULL", c.SandboxID) == nil, 409, "node_already_registered")
		t.exec("INSERT INTO node_instances(id,sandbox_instance_id,workspace_id,service_subject,connection_state,protocol_version) VALUES($1,$2,$3,$4,'connected',1)", c.Subject, c.SandboxID, c.WorkspaceID, c.Subject)
		return t.one("SELECT * FROM node_instances WHERE id=$1", c.Subject)
	}
	n := t.one("SELECT * FROM node_instances WHERE id=$1 AND sandbox_instance_id=$2 AND ended_at IS NULL AND service_subject=$3", c.Subject, c.SandboxID, c.Subject)
	require(n != nil, 409, "stale_node")
	switch r.Action {
	case "node_status":
		version(n, r.Body.N("version"))
		state := r.Body.S("connectionState")
		require(state == "connected" || state == "disconnected", 400, "invalid_node_state")
		require(!n.B("initialized") || r.Body.B("initialized"), 409, "initialization_regression")
		t.exec("UPDATE node_instances SET connection_state=$2,initialized=$3,last_seen_at=clock_timestamp(),idle_admission_epoch=NULL,version=version+1 WHERE id=$1", c.Subject, state, r.Body.B("initialized"))
		if state == "disconnected" && w.S("observedState") == "ready" {
			t.exec("UPDATE workspaces SET admission_open=false,observed_state='unavailable',version=version+1 WHERE id=$1", w.S("id"))
		}
	case "node_finish":
		require(validID(r.TicketID), 404, "not_found")
		ticket := t.one("SELECT * FROM execution_tickets WHERE id=$1 AND node_instance_id=$2 AND workspace_id=$3", r.TicketID, c.Subject, c.WorkspaceID)
		require(ticket != nil, 404, "not_found")
		if ticket.S("state") == "finished" {
			return ticket
		}
		version(ticket, r.Body.N("version"))
		t.exec("UPDATE execution_tickets SET state='finished',finished_at=COALESCE(finished_at,now()) WHERE id=$1", r.TicketID)
		return t.one("SELECT * FROM execution_tickets WHERE id=$1", r.TicketID)
	case "node_idle":
		version(n, r.Body.N("version"))
		require(!w.B("admissionOpen") && r.Body.N("admissionEpoch") == w.N("admissionEpoch"), 409, "stale_admission")
		require(validID(r.Body.S("operationId")), 400, "operation_required")
		o := t.one("SELECT * FROM operations WHERE id=$1 AND project_id=$2 AND (workspace_id IS NULL OR workspace_id=$3) AND step='quiesce' AND state IN ('queued','running','retry_wait','blocked')", r.Body.S("operationId"), w.S("projectId"), w.S("id"))
		require(o != nil, 409, "idle_not_requested")
		if !r.Body.B("idle") {
			restoreAdmission(t, o)
			return Object{"accepted": false, "errorCode": "resource_in_use"}
		}
		checkActivities(t, w)
		require(n.B("initialized") && n.S("connectionState") == "connected", 409, "idle_unconfirmed")
		t.exec("UPDATE node_instances SET idle_admission_epoch=$2,last_seen_at=clock_timestamp(),version=version+1 WHERE id=$1", c.Subject, w.N("admissionEpoch"))
	default:
		reject(404, "not_found")
	}
	return t.one("SELECT * FROM node_instances WHERE id=$1", c.Subject)
}

func restoreAdmission(t *transaction, o Object) {
	for wid, v := range o.O("request").O("previous") {
		raw, ok := v.(map[string]any)
		require(ok, 500, "internal_error")
		w := Object(raw)
		t.exec("UPDATE workspaces SET desired_state=$2,observed_state=$3,admission_open=$4,admission_epoch=admission_epoch+1,version=version+1 WHERE id=$1", wid, w.S("desiredState"), w.S("observedState"), w.B("admissionOpen"))
	}
	t.exec("UPDATE projects SET lifecycle='active',version=version+1 WHERE id=$1 AND lifecycle='deleting'", o.S("projectId"))
	t.exec("UPDATE operations SET state='failed',error_code='resource_in_use',version=version+1,updated_at=now() WHERE id=$1", o.S("id"))
}
