package core

import (
	"context"
	"strings"
)

// ControlRequest contains a verified service principal, never a header-selected role.
type ControlRequest struct {
	Action, OperationID, EffectID, WorkspaceID, TicketID string
	Body                                                 Object
	Service                                              *Claims
	Identity                                             *Claims
}

// Control is the finite internal command API. Controllers have no table-write or SQL interface.
func (s *Store) Control(ctx context.Context, r *ControlRequest) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
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
		leaseValid(t, r)
		if r.Action == "claim" {
			return claim(t, r)
		}
		o := operation(t, r)
		switch r.Action {
		case "snapshot":
			return snapshot(t, o)
		case "plan":
			return planEffect(t, r, o)
		case "effect_result":
			return effectResult(t, r, o)
		case "advance":
			return advance(t, r, o)
		case "defer":
			state := r.Body.S("state")
			code := r.Body.S("errorCode")
			require(state == "blocked" || state == "retry_wait", 400, "invalid_operation_state")
			require(code == "substrate_timeout" || code == "termination_unconfirmed" || code == "git_cleanup_failed" || code == "node_unavailable" || code == "external_failure", 400, "invalid_error_code")
			delay := r.Body.N("retrySeconds")
			require(delay >= 1 && delay <= 3600, 400, "invalid_retry_delay")
			t.exec("UPDATE operations SET state=$2,error_code=$3,retry_at=clock_timestamp()+($4 * interval '1 second'),version=version+1,updated_at=now() WHERE id=$1", o.S("id"), state, code, delay)
			return t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
		default:
			reject(404, "not_found")
		}
		return nil
	})
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
		if r.Action == "lease_renew" {
			t.exec("UPDATE controller_leases SET expires_at=clock_timestamp()+interval '30 seconds' WHERE name='global'")
		} else {
			require(r.Action == "lease_release", 404, "not_found")
			t.exec("UPDATE controller_leases SET expires_at=clock_timestamp() WHERE name='global'")
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

func snapshot(t *transaction, o Object) Object {
	p := t.one("SELECT p.*,c.secret_ref FROM projects p LEFT JOIN credential_refs c ON c.id=p.credential_ref_id WHERE p.id=$1", o.S("projectId"))
	return Object{"operation": o, "project": p, "storage": t.one("SELECT * FROM project_storage WHERE project_id=$1", o.S("projectId")), "workspaces": t.list("SELECT w.*,wt.relative_path,wt.branch_name,wt.requested_ref,wt.base_commit_id FROM workspaces w JOIN workspace_worktrees wt ON wt.workspace_id=w.id WHERE w.project_id=$1 AND w.deleted_at IS NULL ORDER BY w.id", o.S("projectId")), "sandboxes": t.list("SELECT s.* FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 ORDER BY s.id", o.S("projectId")), "nodes": t.list("SELECT n.* FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 ORDER BY n.id", o.S("projectId")), "effects": t.list("SELECT * FROM external_effects WHERE operation_id=$1 ORDER BY created_at,id", o.S("id"))}
}

func operationWorkspaces(t *transaction, o Object) []Object {
	if o.S("workspaceId") != "" {
		return t.list("SELECT * FROM workspaces WHERE id=$1", o.S("workspaceId"))
	}
	return t.list("SELECT * FROM workspaces WHERE project_id=$1 AND deleted_at IS NULL ORDER BY id", o.S("projectId"))
}

func reconciled(t *transaction, o Object) {
	require(t.one("SELECT id FROM external_effects WHERE operation_id=$1 AND reconciled_epoch<>$2", o.S("id"), o.N("controllerEpoch")) == nil, 409, "reconcile_required")
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

func planEffect(t *transaction, r *ControlRequest, o Object) Object {
	reconciled(t, o)
	kind, wid := r.Body.S("kind"), r.Body.S("workspaceId")
	allowed := map[string]string{"storage": "storage_ensure", "worktree": "worktree_ensure", "sandbox": "sandbox_ensure", "terminate": "sandbox_terminate", "cleanup": "worktree_delete", "storage_delete": "storage_delete"}
	require(allowed[o.S("step")] == kind, 409, "invalid_step")
	if kind == "storage_ensure" || kind == "storage_delete" {
		require(wid == "", 400, "invalid_effect_scope")
	} else {
		require(validID(wid), 400, "invalid_effect_scope")
		found := false
		for _, w := range operationWorkspaces(t, o) {
			if w.S("id") == wid {
				found = true
			}
		}
		require(found, 403, "invalid_effect_scope")
	}
	if existing := effectFor(t, o.S("id"), kind, wid); existing != nil {
		return Object{"effect": existing, "operation": o}
	}
	id := newID()
	request := Object{"kind": kind, "projectId": o.S("projectId")}
	if wid != "" {
		request["workspaceId"] = wid
	}
	if kind == "worktree_ensure" {
		project := t.one("SELECT repository_url FROM projects WHERE id=$1", o.S("projectId"))
		worktree := t.one("SELECT requested_ref FROM workspace_worktrees WHERE workspace_id=$1", wid)
		request["repositoryUrl"] = project.S("repositoryUrl")
		request["requestedRef"] = worktree.S("requestedRef")
	}
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
	if kind == "storage_delete" {
		require(t.one("SELECT s.id FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 AND s.terminated_at IS NULL", o.S("projectId")) == nil, 409, "termination_unconfirmed")
		require(t.one("SELECT id FROM external_effects WHERE project_id=$1 AND state IN ('planned','running')", o.S("projectId")) == nil, 409, "maintenance_unconfirmed")
	}
	t.exec("INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch) VALUES($1,$2,$3,$4,$5,'planned',$6,$7)", id, o.S("id"), o.S("projectId"), nullable(wid), kind, jsonText(request), o.N("controllerEpoch"))
	t.exec("UPDATE operations SET version=version+1,updated_at=now() WHERE id=$1", o.S("id"))
	return Object{"effect": t.one("SELECT * FROM external_effects WHERE id=$1", id), "operation": t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))}
}

func effectResult(t *transaction, r *ControlRequest, o Object) Object {
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
		case "worktree_ensure":
			require(commitID(result.S("commitId")) && result.B("jobTerminated"), 400, "invalid_worktree_evidence")
		case "worktree_delete":
			require(result.B("jobTerminated") && result.B("removed"), 400, "invalid_cleanup_evidence")
		case "sandbox_terminate":
			require(result.B("terminated"), 409, "termination_unconfirmed")
		case "storage_delete":
			require(result.B("removed"), 400, "invalid_cleanup_evidence")
		case "storage_ensure":
			require(result.N("layoutVersion") == 1, 400, "invalid_storage_layout")
		case "sandbox_ensure":
			require(result.S("sandboxInstanceId") == e.S("id"), 400, "invalid_sandbox_evidence")
		}
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
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
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

func advance(t *transaction, r *ControlRequest, o Object) Object {
	reconciled(t, o)
	next := ""
	wid := o.S("workspaceId")
	switch o.S("step") {
	case "storage":
		e := completedEffect(t, o, "storage_ensure", "")
		t.exec("UPDATE project_storage SET substrate_storage_id=$2,observed_state='ready',version=version+1 WHERE project_id=$1 AND substrate_storage_id IS NULL", o.S("projectId"), e.S("externalId"))
		next = "worktree"
	case "worktree":
		e := completedEffect(t, o, "worktree_ensure", wid)
		t.exec("UPDATE workspace_worktrees SET base_commit_id=$2 WHERE workspace_id=$1", wid, e.O("result").S("commitId"))
		next = "sandbox"
	case "sandbox":
		e := completedEffect(t, o, "sandbox_ensure", wid)
		t.exec("UPDATE sandbox_instances SET substrate_sandbox_id=$2,observed_state='starting' WHERE id=$1 AND substrate_sandbox_id IS NULL AND terminated_at IS NULL", e.S("id"), e.S("externalId"))
		next = "node"
	case "node":
		w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
		require(w.S("desiredState") == "running", 409, "resource_unavailable")
		n := t.one("SELECT n.id FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id WHERE s.workspace_id=$1 AND s.generation=$2 AND s.terminated_at IS NULL AND n.ended_at IS NULL AND n.initialized AND n.connection_state='connected' AND n.last_seen_at>clock_timestamp()-interval '30 seconds'", wid, w.N("runtimeGeneration"))
		require(n != nil, 409, "node_not_ready")
		require(t.one("SELECT workspace_id FROM workspace_worktrees WHERE workspace_id=$1 AND base_commit_id IS NOT NULL", wid) != nil, 409, "worktree_not_ready")
		t.exec("UPDATE workspace_worktrees SET provisioning_state='ready' WHERE workspace_id=$1", wid)
		t.exec("UPDATE sandbox_instances SET observed_state='running' WHERE workspace_id=$1 AND terminated_at IS NULL", wid)
		t.exec("UPDATE workspaces SET observed_state='ready',admission_open=true,version=version+1 WHERE id=$1", wid)
		t.exec("UPDATE projects SET lifecycle='active',version=version+1 WHERE id=$1 AND lifecycle='provisioning'", o.S("projectId"))
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
		for _, w := range operationWorkspaces(t, o) {
			completedEffect(t, o, "worktree_delete", w.S("id"))
			t.exec("UPDATE workspace_worktrees SET provisioning_state='deleted' WHERE workspace_id=$1", w.S("id"))
		}
		if o.S("kind") == "delete_project" {
			t.exec("UPDATE project_storage SET observed_state='deleting',version=version+1 WHERE project_id=$1", o.S("projectId"))
			next = "storage_delete"
		} else {
			deleteWorkspace(t, wid)
			next = "done"
		}
	case "storage_delete":
		completedEffect(t, o, "storage_delete", "")
		require(t.one("SELECT s.id FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.project_id=$1 AND s.terminated_at IS NULL", o.S("projectId")) == nil, 409, "termination_unconfirmed")
		for _, w := range operationWorkspaces(t, o) {
			deleteWorkspace(t, w.S("id"))
		}
		t.exec("UPDATE project_storage SET observed_state='deleted',version=version+1 WHERE project_id=$1", o.S("projectId"))
		t.exec("UPDATE projects SET lifecycle='deleted',deleted_at=now(),version=version+1 WHERE id=$1", o.S("projectId"))
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

func deleteWorkspace(t *transaction, wid string) {
	t.exec("UPDATE workspaces SET observed_state='deleted',admission_open=false,deleted_at=now(),version=version+1 WHERE id=$1", wid)
	t.exec("UPDATE tasks SET deleted_at=now(),version=version+1 WHERE workspace_id=$1", wid)
}
