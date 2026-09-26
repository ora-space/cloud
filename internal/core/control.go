package core

import (
	"context"
	"strings"
)

// ControlRequest contains a verified service principal, never a header-selected role. SubmissionID
// is the caller-chosen identity of one logical state change; when set, the same identity with the
// same content replays the recorded response instead of reapplying.
type ControlRequest struct {
	Action, OperationID, EffectID, WorkspaceID, TicketID string
	SubmissionID                                         string
	Body                                                 Object
	Service                                              *Claims
	Identity                                             *Claims
}

// Control is the finite internal command API. Controllers have no table-write or SQL interface.
// Committed plugin instance writebacks broadcast space invalidation notices exactly like public
// mutations, so live subscribers see fan-out progress without polling.
func (s *Store) Control(ctx context.Context, r *ControlRequest) (Object, error) {
	var events []SpaceEvent
	out, err := s.transact(ctx, func(t *transaction) Object {
		if r.Action == "access" || r.Action == "admit" {
			require(r.Service.Role == "controller", 403, "service_forbidden")
			if r.Action == "admit" || r.Body.S("action") == "execute" {
				leaseValid(t, r)
			}
			return access(t, r)
		}
		if strings.HasPrefix(r.Action, "node_") {
			require(r.Service.Role == "node", 403, "service_forbidden")
			return nodeCommand(t, r)
		}
		require(r.Service.Role == "controller", 403, "service_forbidden")
		if strings.HasPrefix(r.Action, "lease_") {
			return lease(t, r)
		}
		// Recovery reads need no lease: a replacement worker locates original executions before
		// it can hold one, and reading fences nothing.
		if r.Action == "clone_get" || r.Action == "clone_pending" {
			return cloneCommand(t, r)
		}
		leaseValid(t, r)
		if r.Action == "claim" {
			return claim(t, r)
		}
		if r.Action == "live_sandboxes" {
			return liveSandboxes(t)
		}
		if isCloneAction(r.Action) {
			return cloneCommand(t, r)
		}
		if strings.HasPrefix(r.Action, "report_node_") {
			return submitted(t, r, func() Object { return nodeReport(t, r) })
		}
		// The submission wraps the operation lookup too: a replay after the version moved on must
		// return the recorded response, not fail the version check it already passed.
		return submitted(t, r, func() Object { return operationCommand(t, r, &events) })
	})
	if err == nil && s.Events != nil {
		for _, ev := range events {
			s.Events.Publish(ev)
		}
	}
	return out, err
}

// operationCommand runs one Effect-level action on the operation the caller claimed.
func operationCommand(t *transaction, r *ControlRequest, events *[]SpaceEvent) Object {
	o := operation(t, r)
	switch r.Action {
	case "snapshot":
		return snapshot(t, o)
	case "plan":
		return planEffect(t, r, o, events)
	case "effect_result":
		return effectResult(t, r, o, events)
	case "advance":
		return advance(t, r, o, events)
	case "defer":
		state := r.Body.S("state")
		code := r.Body.S("errorCode")
		require(state == "blocked" || state == "retry_wait", 400, "invalid_operation_state")
		require(deferCodes[code], 400, "invalid_error_code")
		delay := r.Body.N("retrySeconds")
		require(delay >= 1 && delay <= 3600, 400, "invalid_retry_delay")
		t.exec("UPDATE operations SET state=$2,error_code=$3,retry_at=clock_timestamp()+($4 * interval '1 second'),version=version+1,updated_at=now() WHERE id=$1", o.S("id"), state, code, delay)
		return t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
	default:
		reject(404, "not_found")
	}
	return nil
}

func lease(t *transaction, r *ControlRequest) Object {
	old := t.one("SELECT *,expires_at>clock_timestamp() AS valid FROM controller_leases WHERE name='global'")
	if r.Action == "lease_acquire" {
		switch {
		case old == nil:
			t.exec("INSERT INTO controller_leases(name,holder_id,epoch,expires_at) VALUES('global',$1,1,clock_timestamp()+interval '30 seconds')", r.Service.Subject)
		case old.B("valid"):
			require(old.S("holderId") == r.Service.Subject, 409, "lease_held")
		default:
			t.exec("UPDATE controller_leases SET holder_id=$1,epoch=epoch+1,expires_at=clock_timestamp()+interval '30 seconds' WHERE name='global'", r.Service.Subject)
		}
	} else {
		leaseValid(t, r)
		switch r.Action {
		case "lease_renew":
			t.exec("UPDATE controller_leases SET expires_at=clock_timestamp()+interval '30 seconds' WHERE name='global'")
		case "lease_release":
			t.exec("UPDATE controller_leases SET expires_at=clock_timestamp() WHERE name='global'")
		case "lease_check":
			// Read-only: proves the caller holds the current lease without extending it.
		default:
			reject(404, "not_found")
		}
	}
	return t.one("SELECT * FROM controller_leases WHERE name='global'")
}

func leaseValid(t *transaction, r *ControlRequest) {
	l := t.one("SELECT * FROM controller_leases WHERE name='global' AND holder_id=$1 AND epoch=$2 AND expires_at>clock_timestamp()", r.Service.Subject, r.Body.N("epoch"))
	require(l != nil, 409, "stale_controller")
}

func claim(t *transaction, r *ControlRequest) Object {
	o := t.one("SELECT * FROM operations WHERE state='queued' OR (state='retry_wait' AND retry_at<=clock_timestamp()) OR state='running' ORDER BY created_at,id LIMIT 1")
	if o == nil {
		return Object{"operation": nil}
	}
	t.exec("UPDATE operations SET state='running',controller_epoch=$2,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), r.Body.N("epoch"))
	o = t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
	return snapshot(t, o)
}

func operation(t *transaction, r *ControlRequest) Object {
	require(validID(r.OperationID), 404, "not_found")
	o := t.one("SELECT * FROM operations WHERE id=$1", r.OperationID)
	require(o != nil, 404, "not_found")
	require(o.S("state") == "running" && o.N("controllerEpoch") == r.Body.N("epoch"), 409, "stale_operation")
	version(o, r.Body.N("version"))
	return o
}

// snapshot is everything a Controller needs to drive one operation. Workspaces carry their own
// requested ref and baseline commit; the retired Project storage and worktree rows are not part of
// it. clones lists this operation's Workspace clone executions so a recovering Controller finds the
// original execution instead of registering a second one.
func snapshot(t *transaction, o Object) Object {
	p := t.one("SELECT p.*,c.secret_ref FROM projects p LEFT JOIN credential_refs c ON c.id=p.credential_ref_id WHERE p.id=$1", o.S("projectId"))
	return Object{"operation": o, "project": p, "workspaces": t.list("SELECT w.* FROM workspaces w WHERE w.project_id=$1 AND w.deleted_at IS NULL ORDER BY w.id", o.S("projectId")), "sandboxes": t.list("SELECT s.* FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 ORDER BY s.id", o.S("projectId")), "nodes": t.list("SELECT n.* FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 ORDER BY n.id", o.S("projectId")), "effects": t.list("SELECT * FROM external_effects WHERE operation_id=$1 ORDER BY created_at,id", o.S("id")), "clones": t.list("SELECT * FROM clone_executions WHERE operation_id=$1 ORDER BY created_at,execution_id", o.S("id"))}
}

func operationWorkspaces(t *transaction, o Object) []Object {
	if o.S("workspaceId") != "" {
		return t.list("SELECT * FROM workspaces WHERE id=$1", o.S("workspaceId"))
	}
	return t.list("SELECT * FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL ORDER BY id", o.S("projectId"))
}

// reconciled requires every effect of the operation to have been reconciled by the current epoch.
// Retired storage and worktree effects of an operation that crossed migration 0016 are history no
// Substrate serves any more, so they cannot be reconciled and do not block it.
func reconciled(t *transaction, o Object) {
	require(t.one("SELECT id FROM external_effects WHERE operation_id=$1 AND reconciled_epoch<>$2 AND kind NOT IN ('storage_ensure','worktree_ensure','worktree_delete','storage_delete')", o.S("id"), o.N("controllerEpoch")) == nil, 409, "reconcile_required")
}

func effectFor(t *transaction, oid, kind, wid string) Object {
	return t.one("SELECT * FROM external_effects WHERE operation_id=$1 AND kind=$2 AND workspace_id IS NOT DISTINCT FROM $3::uuid", oid, kind, nullable(wid))
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func planEffect(t *transaction, r *ControlRequest, o Object, spaceEvents *[]SpaceEvent) Object {
	reconciled(t, o)
	kind, wid := r.Body.S("kind"), r.Body.S("workspaceId")
	require(validID(wid), 400, "invalid_effect_scope")
	found := false
	for _, w := range operationWorkspaces(t, o) {
		if w.S("id") == wid {
			found = true
		}
	}
	require(found, 403, "invalid_effect_scope")
	if existing := effectFor(t, o.S("id"), kind, wid); existing != nil {
		return Object{"effect": existing, "operation": o}
	}
	// The effect id doubles as the preallocated sandbox instance id, so it is
	// drawn before the step-specific request building below.
	id := newID()
	var request Object
	switch o.S("step") {
	case "plugin":
		request = planPluginEffect(t, o, kind, wid, spaceEvents)
	default:
		// Only the three lifecycle effects remain; the clone step dispatches through the execution
		// registry, not through a Substrate effect.
		allowed := map[string]string{"sandbox": "sandbox_ensure", "terminate": "sandbox_terminate", "cleanup": "workspace_data_delete"}
		require(allowed[o.S("step")] == kind, 409, "invalid_step")
		request = Object{"kind": kind, "projectId": o.S("projectId"), "workspaceId": wid}
		if kind == "sandbox_ensure" {
			w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
			require(w.S("desiredState") == "running", 409, "resource_unavailable")
			require(t.one("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", wid) == nil, 409, "termination_unconfirmed")
			t.exec("UPDATE workspaces SET runtime_generation=runtime_generation+1,observed_state='starting',version=version+1 WHERE id=$1", wid)
			t.exec("INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) SELECT $1,id,runtime_generation,'allocating' FROM workspaces WHERE id=$2", id, wid)
		}
		if kind == "sandbox_terminate" {
			s := t.one("SELECT * FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", wid)
			require(s != nil, 409, "no_current_sandbox")
			request["sandboxInstanceId"] = s.S("id")
			t.exec("UPDATE sandbox_instances SET observed_state='terminating' WHERE id=$1", s.S("id"))
		}
		if kind == "workspace_data_delete" {
			// The data is deleted only once no sandbox of this Workspace can still write to it.
			require(t.one("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", wid) == nil, 409, "termination_unconfirmed")
		}
	}
	t.exec("INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch) VALUES($1,$2,$3,$4,$5,'planned',$6,$7)", id, o.S("id"), o.S("projectId"), nullable(wid), kind, jsonText(request), o.N("controllerEpoch"))
	t.exec("UPDATE operations SET version=version+1,updated_at=now() WHERE id=$1", o.S("id"))
	return Object{"effect": t.one("SELECT * FROM external_effects WHERE id=$1", id), "operation": t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))}
}

// planPluginEffect builds the self-contained plugin effect payload and admits
// the dispatch. The payload carries the release info (url/sha256/targets)
// straight from the catalog snapshot, so the Node execution plane never needs
// a registry index or marketplace sync of its own; field names match the
// desktop plugin-manager DownloadRequest capabilities.
func planPluginEffect(t *transaction, o Object, kind, wid string, spaceEvents *[]SpaceEvent) Object {
	switch {
	case o.S("kind") == "install_plugin":
		require(kind == "plugin_ensure", 409, "invalid_step")
	case o.S("kind") == "remove_plugin":
		require(kind == "plugin_delete", 409, "invalid_step")
	default:
		reject(409, "invalid_step")
	}
	require(wid == o.S("workspaceId"), 403, "invalid_effect_scope")
	pluginID := o.O("request").S("pluginId")
	version := o.O("request").S("version")
	require(pluginID != "", 409, "invalid_plugin_request")
	request := Object{"kind": kind, "projectId": o.S("projectId"), "workspaceId": wid, "pluginId": pluginID}
	if kind == "plugin_delete" {
		request["version"] = version
		pluginInstanceWriteback(t, o, "removing", "", nil, spaceEvents)
		return request
	}
	entry := t.pluginCatalogEntry(pluginID)
	require(entry != nil, 409, "plugin_not_found")
	// Admission: plugin_ensure dispatches only to a ready workspace, mirroring
	// the node step gate — a provisioning or stopped workspace is not a valid
	// download target.
	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	require(w.S("observedState") == "ready", 409, "workspace_not_ready")
	request["version"] = entry.S("version")
	if entry.S("url") != "" {
		request["universal"] = Object{"url": entry.S("url"), "sha256": entry.S("sha256")}
	}
	if entry["targets"] != nil {
		request["targets"] = entry["targets"]
	}
	pluginInstanceWriteback(t, o, "installing", entry.S("version"), nil, spaceEvents)
	return request
}

func effectResult(t *transaction, r *ControlRequest, o Object, spaceEvents *[]SpaceEvent) Object {
	require(validID(r.EffectID), 404, "not_found")
	e := t.one("SELECT * FROM external_effects WHERE id=$1 AND operation_id=$2", r.EffectID, o.S("id"))
	require(e != nil, 404, "not_found")
	state := r.Body.S("state")
	require(state == "running" || state == "succeeded" || state == "failed" || state == "absent", 400, "invalid_effect_state")
	result := r.Body.O("result")
	external := r.Body.S("externalId")
	if state == "absent" {
		require(e.S("state") == "planned", 409, "external_state_conflict")
		state = "planned"
	} else {
		require(external != "" && len(external) <= 200, 400, "external_id_required")
		if e.S("externalId") != "" {
			require(external == e.S("externalId"), 409, "external_binding_conflict")
		}
	}
	if e.S("state") == "succeeded" {
		require(state == "succeeded" && jsonText(result) == jsonText(e.O("result")), 409, "external_state_conflict")
	}
	if state == "succeeded" {
		switch e.S("kind") {
		case "sandbox_terminate":
			require(result.B("terminated"), 409, "termination_unconfirmed")
		case "workspace_data_delete":
			require(result.B("removed"), 400, "invalid_cleanup_evidence")
		case "sandbox_ensure":
			// nodeId is the identity the sandbox's Node must present; node reports are checked against it.
			node := result.S("nodeId")
			require(result.S("sandboxInstanceId") == e.S("id") && node != "" && len(node) <= 200, 400, "invalid_sandbox_evidence")
		case "plugin_ensure":
			// Success evidence must name the exact version the effect was
			// planned with: a Node may not substitute its own resolution.
			require(result.B("installed") && result.S("version") == e.O("request").S("version"), 400, "invalid_plugin_evidence")
		case "plugin_delete":
			require(result.B("removed"), 400, "invalid_cleanup_evidence")
		}
	}
	if state == "failed" && (e.S("kind") == "plugin_ensure" || e.S("kind") == "plugin_delete") {
		// A failed install/remove surfaces on the space row immediately so the
		// UI can render the failure while the operation waits for retry.
		message := result.S("error")
		if message == "" {
			message = "external_failure"
		}
		pluginInstanceWriteback(t, o, "failed", "", &message, spaceEvents)
	}
	t.exec("UPDATE external_effects SET state=$2,external_id=COALESCE($3,external_id),result=$4,reconciled_epoch=$5 WHERE id=$1", e.S("id"), state, nullable(external), jsonText(result), o.N("controllerEpoch"))
	t.exec("UPDATE operations SET version=version+1,updated_at=now() WHERE id=$1", o.S("id"))
	return Object{"effect": t.one("SELECT * FROM external_effects WHERE id=$1", e.S("id")), "operation": t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))}
}

func commitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func completedEffect(t *transaction, o Object, kind, wid string) Object {
	e := effectFor(t, o.S("id"), kind, wid)
	require(e != nil && e.S("state") == "succeeded" && e.N("reconciledEpoch") == o.N("controllerEpoch"), 409, "effect_incomplete")
	return e
}

func advance(t *transaction, r *ControlRequest, o Object, spaceEvents *[]SpaceEvent) Object {
	reconciled(t, o)
	next := ""
	wid := o.S("workspaceId")
	switch o.S("step") {
	case "sandbox":
		e := completedEffect(t, o, "sandbox_ensure", wid)
		t.exec("UPDATE sandbox_instances SET substrate_sandbox_id=$2,observed_state='starting' WHERE id=$1 AND substrate_sandbox_id IS NULL AND terminated_at IS NULL", e.S("id"), e.S("externalId"))
		next = "node"
	case "node":
		currentNode(t, wid)
		t.exec("UPDATE sandbox_instances SET observed_state='running' WHERE workspace_id=$1 AND terminated_at IS NULL", wid)
		if o.S("kind") == "create_project" || o.S("kind") == "create_workspace" {
			// A new Workspace has a connected Node but no code yet: admission waits for the clone.
			next = "clone"
		} else {
			openWorkspace(t, o, wid)
			next = "done"
		}
	case "clone":
		advanceClone(t, o, wid)
		openWorkspace(t, o, wid)
		next = "done"
	case "quiesce":
		for _, w := range operationWorkspaces(t, o) {
			checkActivities(t, w)
			live := t.one("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", w.S("id"))
			if live != nil {
				require(t.one("SELECT id FROM node_instances WHERE sandbox_instance_id=$1 AND ended_at IS NULL AND idle_admission_epoch=$2 AND connection_state='connected' AND last_seen_at>clock_timestamp()-interval '30 seconds'", live.S("id"), w.N("admissionEpoch")) != nil, 409, "idle_unconfirmed")
			}
		}
		next = "terminate"
	case "terminate":
		for _, w := range operationWorkspaces(t, o) {
			live := t.one("SELECT id FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", w.S("id"))
			if live != nil {
				completedEffect(t, o, "sandbox_terminate", w.S("id"))
				t.exec("UPDATE node_instances SET ended_at=now(),connection_state='ended',version=version+1 WHERE sandbox_instance_id=$1 AND ended_at IS NULL", live.S("id"))
				t.exec("UPDATE sandbox_instances SET observed_state='terminated',terminated_at=now() WHERE id=$1", live.S("id"))
			}
		}
		if o.S("kind") == "stop" || o.S("kind") == "administrative_stop" {
			t.exec("UPDATE workspaces SET observed_state='stopped',version=version+1 WHERE id=$1", wid)
			next = "done"
		} else {
			next = "cleanup"
		}
	case "cleanup":
		// Each Workspace's data was deleted by its own effect, planned only after its sandbox ended.
		for _, w := range operationWorkspaces(t, o) {
			completedEffect(t, o, "workspace_data_delete", w.S("id"))
		}
		if o.S("kind") == "delete_project" {
			require(t.one("SELECT s.id FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 AND s.terminated_at IS NULL", o.S("projectId")) == nil, 409, "termination_unconfirmed")
			for _, w := range operationWorkspaces(t, o) {
				deleteWorkspace(t, w.S("id"))
			}
			t.exec("UPDATE projects SET lifecycle='deleted',deleted_at=now(),version=version+1 WHERE id=$1", o.S("projectId"))
		} else {
			deleteWorkspace(t, wid)
		}
		next = "done"
	case "plugin":
		// The plugin step completes the install/remove for exactly the
		// operation's bound workspace; the space-level aggregate is recomputed
		// from the fan-out rows in the same transaction.
		kind := "plugin_ensure"
		state, version := "installed", o.O("request").S("version")
		if o.S("kind") == "remove_plugin" {
			kind, state, version = "plugin_delete", "removed", ""
		}
		completedEffect(t, o, kind, wid)
		pluginInstanceWriteback(t, o, state, version, nil, spaceEvents)
		next = "done"
	default:
		reject(409, "invalid_step")
	}
	if next == "done" {
		t.exec("UPDATE operations SET step='done',state='succeeded',result=jsonb_build_object('resourceId',COALESCE(workspace_id,project_id)),version=version+1,updated_at=now() WHERE id=$1", o.S("id"))
	} else {
		t.exec("UPDATE operations SET step=$2,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), next)
	}
	return t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
}

// currentNode requires the Node evidence the node step is built on: an initialized, connected Node
// with a fresh heartbeat on the running Workspace's current, live sandbox generation.
func currentNode(t *transaction, wid string) Object {
	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	require(w.S("desiredState") == "running", 409, "resource_unavailable")
	n := t.one("SELECT n.* FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id WHERE s.workspace_id=$1 AND s.generation=$2 AND s.terminated_at IS NULL AND n.ended_at IS NULL AND n.initialized AND n.connection_state='connected' AND n.last_seen_at>clock_timestamp()-interval '30 seconds'", wid, w.N("runtimeGeneration"))
	require(n != nil, 409, "node_not_ready")
	return n
}

// openWorkspace commits Workspace readiness and admission together with the operation's success.
// The Node is checked again so admission never opens on a Node that went away since the node step.
func openWorkspace(t *transaction, o Object, wid string) {
	currentNode(t, wid)
	t.exec("UPDATE workspaces SET observed_state='ready',admission_open=true,version=version+1 WHERE id=$1", wid)
	t.exec("UPDATE projects SET lifecycle='active',version=version+1 WHERE id=$1 AND lifecycle='provisioning'", o.S("projectId"))
}

func deleteWorkspace(t *transaction, wid string) {
	t.exec("UPDATE workspaces SET observed_state='deleted',admission_open=false,deleted_at=now(),version=version+1 WHERE id=$1", wid)
	t.exec("UPDATE tasks SET deleted_at=now(),version=version+1 WHERE workspace_id=$1", wid)
}
