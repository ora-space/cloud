package core

import "strings"

// runtimeControl reads validity using PostgreSQL time; no caller clock participates in fencing.
func runtimeControl(t *transaction, wid string) Object {
	c := t.one("SELECT *, expires_at>clock_timestamp() AS valid FROM runtime_controls WHERE workspace_id=$1", wid)
	require(c != nil, 409, "runtime_control_reconciliation_required")
	return c
}

// refreshRuntimeControls withdraws new eligibility while retaining the old session and executions.
// It does not infer input closure or process completion from expiry, revocation or disconnection.
func refreshRuntimeControls(t *transaction) {
	freezeInactiveRepositoryCredentials(t)
	changed := t.list(`SELECT c.* FROM runtime_controls c
 JOIN workspaces w ON w.id=c.workspace_id WHERE c.state IN ('held','acquiring')
 AND (c.expires_at<=clock_timestamp() OR (c.bound_sandbox_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM node_instances n WHERE n.sandbox_instance_id=c.bound_sandbox_id AND n.ended_at IS NULL AND n.connection_state='connected' AND n.last_seen_at>clock_timestamp()-interval '30 seconds')) OR NOT EXISTS(
 SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id
 JOIN tenants tn ON tn.id=m.tenant_id WHERE m.tenant_id=w.tenant_id AND m.user_id=c.holder_user_id
 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL
 AND tn.status='active' AND tn.deleted_at IS NULL
 AND (m.role='admin' OR w.creator_user_id=m.user_id)))`)
	for _, c := range changed {
		t.exec("UPDATE runtime_controls SET state='draining',binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", c.S("workspaceId"))
		auditRuntimeControl(t, runtimeControl(t, c.S("workspaceId")), "qualification_withdrawn", "")
	}
	reconcileRuntimeControls(t)
}

// reconcileRuntimeControls trusts confirmed termination or the Node's durable closed-input
// evidence. A missing connection, an empty new table or elapsed time alone proves neither.
func reconcileRuntimeControls(t *transaction) {
	rows := t.list(`SELECT c.* FROM runtime_controls c JOIN workspaces w ON w.id=c.workspace_id
 WHERE c.state IN ('draining','reconciling')
 AND NOT EXISTS(SELECT 1 FROM runtime_force_stops f WHERE f.workspace_id=w.id AND f.state<>'succeeded')
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.workspace_id=w.id AND ar.cleared_at IS NULL)
 AND (c.input_closed OR (w.observed_state IN ('stopped','deleted')
 AND NOT EXISTS(SELECT 1 FROM sandbox_instances s WHERE s.workspace_id=w.id AND s.terminated_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM execution_tickets a WHERE a.workspace_id=w.id AND a.state='active' AND a.terminated_by_force_stop_id IS NULL)
 AND NOT EXISTS(SELECT 1 FROM clone_executions e WHERE e.workspace_id=w.id AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL)
	AND NOT EXISTS(SELECT 1 FROM node_executions e WHERE e.workspace_id=w.id AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL)
 AND NOT EXISTS(SELECT 1 FROM operations o WHERE (o.workspace_id=w.id OR (o.workspace_id IS NULL AND o.project_id=w.project_id)) AND o.state IN ('queued','running','blocked','retry_wait'))`)
	for _, c := range rows {
		t.exec("UPDATE runtime_controls SET state='idle',session_id=NULL,holder_user_id=NULL,expires_at=NULL,maintenance_operation_id=NULL,maintenance_run_id=NULL,bound_sandbox_id=NULL,binding_confirmed=false,input_closed=true,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", c.S("workspaceId"))
		auditRuntimeControl(t, runtimeControl(t, c.S("workspaceId")), "responsibilities_settled", "")
	}
}

// auditRuntimeControl preserves the holder and original session without changing historical actors.
func auditRuntimeControl(t *transaction, c Object, reason, actor string) {
	t.exec("INSERT INTO runtime_control_events(id,workspace_id,control_epoch,session_id,actor_user_id,state,reason) VALUES($1,$2,$3,$4,$5,$6,$7)", newID(), c.S("workspaceId"), c.N("controlEpoch"), nullable(c.S("sessionId")), nullable(actor), c.S("state"), reason)
}

// controlView discloses safe occupancy, never the secret-free but page-specific session binding.
func controlView(c Object) Object {
	out := Object{}
	for _, key := range []string{"workspaceId", "state", "controlEpoch", "holderUserId", "expiresAt", "version"} {
		out[key] = c[key]
	}
	if (c.S("state") == "held" || c.S("state") == "acquiring") && !c.B("valid") {
		out["state"] = "draining"
	}
	return out
}

// runtimeControlPublic owns the finite control API. The session is allocated by Cloud on acquire,
// bound to the verified user, and never exposed by the shared occupancy query.
func runtimeControlPublic(t *transaction, r *PublicRequest, uid string) Object {
	w := t.one("SELECT * FROM workspaces WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", r.WorkspaceID, r.TenantID)
	require(w != nil, 404, "not_found")
	c := runtimeControl(t, w.S("id"))
	if r.Method == "GET" {
		return controlView(c)
	}
	require(runtimeUsable(t, w, uid), 403, "runtime_use_forbidden")
	version(c, r.Body.N("version"))
	reason := ""
	switch {
	case strings.HasSuffix(r.Path, "/acquire"):
		require(c.S("state") == "idle", 409, "runtime_control_held")
		require(w.S("observedState") == "ready" || w.S("observedState") == "stopped", 409, "resource_unavailable")
		checkActivities(t, w)
		sid := newID()
		t.exec("INSERT INTO runtime_control_sessions(id,workspace_id,tenant_id,actor_user_id) VALUES($1,$2,$3,$4)", sid, w.S("id"), r.TenantID, uid)
		live := t.one("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", w.S("id"))
		state := "acquiring"
		if live == nil && w.S("observedState") == "stopped" {
			state = "held"
		}
		t.exec("UPDATE runtime_controls SET state=$2,control_epoch=control_epoch+1,session_id=$3,holder_user_id=$4,expires_at=clock_timestamp()+interval '60 seconds',bound_sandbox_id=$5,binding_confirmed=$6,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", w.S("id"), state, sid, uid, nullable(live.S("id")), state == "held")
		reason = "acquired"
	case strings.HasSuffix(r.Path, "/renew"):
		require(c.S("holderUserId") == uid && c.S("sessionId") == r.Body.S("sessionId") && c.S("state") == "held" && c.B("valid"), 409, "stale_runtime_control")
		t.exec("UPDATE runtime_controls SET expires_at=clock_timestamp()+interval '60 seconds',version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", w.S("id"))
		reason = "renewed"
	case strings.HasSuffix(r.Path, "/release"):
		require(c.S("holderUserId") == uid && c.S("sessionId") == r.Body.S("sessionId") && (c.S("state") == "held" || c.S("state") == "acquiring"), 409, "stale_runtime_control")
		t.exec("UPDATE runtime_controls SET state='draining',expires_at=clock_timestamp(),binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", w.S("id"))
		reason = "released"
	default:
		reject(404, "not_found")
	}
	c = runtimeControl(t, w.S("id"))
	auditRuntimeControl(t, c, reason, uid)
	out := controlView(c)
	out["sessionId"] = c["sessionId"]
	return out
}

// requireRuntimeSession independently checks content permission, page binding and current expiry.
func requireRuntimeSession(t *transaction, w Object, uid, session string) Object {
	require(runtimeUsable(t, w, uid), 403, "runtime_use_forbidden")
	c := runtimeControl(t, w.S("id"))
	require(validID(session) && c.S("sessionId") == session && c.S("holderUserId") == uid && c.S("state") == "held" && c.B("valid"), 409, "runtime_control_required")
	return c
}

// reserveRuntimeMaintenance shares the user's write boundary with a fixed lifecycle intent.
func reserveRuntimeMaintenance(t *transaction, wid, oid string) {
	t.exec("INSERT INTO runtime_controls(workspace_id,state) VALUES($1,'idle') ON CONFLICT(workspace_id) DO NOTHING", wid)
	t.exec("UPDATE runtime_controls SET state='maintenance',maintenance_operation_id=$2,maintenance_run_id=NULL,control_epoch=control_epoch+1,session_id=NULL,holder_user_id=NULL,expires_at=NULL,binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", wid, oid)
	auditRuntimeControl(t, runtimeControl(t, wid), "lifecycle_reserved", "")
}

// finishRuntimeMaintenance requests closure after the fixed intent finishes; it cannot grant
// another session on a still-running Node before that Node confirms its durable responsibilities.
func finishRuntimeMaintenance(t *transaction, oid string) {
	if run := t.one("SELECT w.id,w.issue_run_id,w.workflow_run_id FROM workspaces w JOIN operations o ON o.workspace_id=w.id WHERE o.id=$1 AND o.kind='create_workspace' AND o.state='succeeded' AND (w.issue_run_id IS NOT NULL OR w.workflow_run_id IS NOT NULL)", oid); run != nil {
		// Initialization handed back terminal evidence for clone/plugins. The next control epoch
		// belongs to this run — IssueRun or WorkflowRun, whichever owns the Workspace (D8); Node
		// must acknowledge it before any session or delivery is permitted.
		owner := run.S("issueRunId")
		if owner == "" {
			owner = run.S("workflowRunId")
		}
		t.exec("UPDATE runtime_controls SET state='maintenance',maintenance_operation_id=NULL,maintenance_run_id=$2,control_epoch=control_epoch+1,binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE workspace_id=$1", run.S("id"), owner)
		auditRuntimeControl(t, runtimeControl(t, run.S("id")), "run_reserved", "")
		return
	}
	t.exec("UPDATE runtime_controls SET state='draining',binding_confirmed=false,input_closed=false,version=version+1,updated_at=clock_timestamp() WHERE maintenance_operation_id=$1 AND state='maintenance'", oid)
	reconcileRuntimeControls(t)
}
