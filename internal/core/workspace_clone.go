package core

// The clone step of a runtime Workspace (specs decisions/cloud/operation/0-workspace-runtime-follows-
// desktop-node.md D2). The Controller dispatches one clone to the Workspace's current Node through the
// same execution registry as tenant clone requests; the execution is additionally bound to the
// operation and Workspace, and Cloud, not the Controller, decides its input and target Node.

// deferCodes are the finite reasons a Controller may give when it parks an operation. clone_failed
// covers every failure a Node reported (a retry is a new execution); clone_result_unknown parks an
// execution whose outcome the Node could not tell, which is never retried automatically.
var deferCodes = map[string]bool{
	"substrate_timeout": true, "termination_unconfirmed": true, "git_cleanup_failed": true, "node_unavailable": true,
	"external_failure": true, "clone_failed": true, "clone_result_unknown": true,
	"plugin_execution_failed": true, "plugin_result_unknown": true,
}

// workspaceCloneAttemptLimit bounds one operation across Controller restarts and manual retries.
const workspaceCloneAttemptLimit = 3

// workspaceCloneAttempts reads durable dispatches, rather than an in-memory retry counter.
func workspaceCloneAttempts(t *transaction, operationID string) int64 {
	return t.one("SELECT count(*) AS attempts FROM clone_executions WHERE operation_id=$1", operationID).N("attempts")
}

// settleRefusedClone settles one unregistered deterministic repository refusal inside the claim
// transaction (ADR settle-deterministic-dispatch-refusals D2). It reports whether the candidate
// was settled; claim then inspects the next candidate instead of handing back an operation whose
// clone dispatch can only be refused again. Settlement keeps the stage, request, external effects
// and resource identities for audit, closes new execution admission on the main Workspace, and
// settles the run Workspace in the same transaction, so a hook or cleanup failure rolls back the
// whole settlement instead of leaving a half-failed operation behind.
func settleRefusedClone(t *transaction, o Object) bool {
	// Only clone steps of the two creating kinds start new remote Git work; a succeeded clone
	// already advanced past it and must not be rewritten by a later freeze.
	if o.S("step") != "clone" || (o.S("kind") != "create_project" && o.S("kind") != "create_workspace") {
		return false
	}
	// No unsettled clone, unknown effect or unknown execution of this operation may exist: an
	// already dispatched clone keeps its original authorization and outcome obligations, and the
	// recovery rules for registered identities take precedence over an unregistered refusal.
	if t.one("SELECT execution_id FROM clone_executions WHERE operation_id=$1 AND result IS NULL AND terminated_by_force_stop_id IS NULL", o.S("id")) != nil {
		return false
	}
	if t.one("SELECT execution_id FROM clone_executions WHERE operation_id=$1 AND result->>'outcome'='clone_ready'", o.S("id")) != nil {
		return false
	}
	if t.one("SELECT id FROM external_effects WHERE operation_id=$1 AND state IN ('planned','running') AND kind NOT IN ('storage_ensure','worktree_ensure','worktree_delete','storage_delete','plugin_ensure','plugin_delete')", o.S("id")) != nil {
		return false
	}
	if t.one("SELECT execution_id FROM node_executions WHERE operation_id=$1 AND result IS NULL AND terminated_by_force_stop_id IS NULL", o.S("id")) != nil {
		return false
	}
	code := repositoryAccessFault(t, o.S("projectId"))
	if code == "" {
		return false
	}
	t.exec("UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), code)
	t.exec("UPDATE workspaces SET observed_state='unavailable',admission_open=false,version=version+1 WHERE id=$1", o.S("workspaceId"))
	finishRuntimeMaintenance(t, o.S("id"))
	if w := t.one("SELECT issue_run_id FROM workspaces WHERE id=$1", o.S("workspaceId")); w.S("issueRunId") != "" {
		runWorkspaceSettled(t, w.S("issueRunId"), "failed")
	}
	return true
}

// deferWorkspaceClone retries only settled failures, with a server-owned bounded backoff.
// An unknown outcome retains its existing blocked policy and can never consume a new dispatch.
func deferWorkspaceClone(t *transaction, o Object) {
	latest := t.one("SELECT result FROM clone_executions WHERE operation_id=$1 ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	require(latest.O("result").S("outcome") == "clone_failed", 409, "clone_incomplete")
	attempts := workspaceCloneAttempts(t, o.S("id"))
	if attempts < workspaceCloneAttemptLimit {
		delay := int64(5) * max(attempts, 1)
		t.exec("UPDATE operations SET state='retry_wait',error_code='clone_failed',retry_at=clock_timestamp()+($2 * interval '1 second'),version=version+1,updated_at=now() WHERE id=$1", o.S("id"), delay)
		return
	}
	t.exec("UPDATE operations SET state='failed',error_code='clone_failed',retry_at=NULL,version=version+1,updated_at=now() WHERE id=$1", o.S("id"))
	t.exec("UPDATE workspaces SET observed_state='unavailable',admission_open=false,version=version+1 WHERE id=$1", o.S("workspaceId"))
	finishRuntimeMaintenance(t, o.S("id"))
	if w := t.one("SELECT issue_run_id FROM workspaces WHERE id=$1", o.S("workspaceId")); w.S("issueRunId") != "" {
		runWorkspaceSettled(t, w.S("issueRunId"), "failed")
	}
}

// workspaceCloneDispatch registers the clone of a Workspace operation in its clone step. Replaying
// the same execution is idempotent; anything else must match the Workspace's Project repository,
// requested ref and current Node exactly, and waits until no earlier execution is still unresolved.
func workspaceCloneDispatch(t *transaction, r *ControlRequest, operation, execution, node string, input Object) Object {
	o := t.one("SELECT * FROM operations WHERE id=$1 AND workspace_id IS NOT NULL", operation)
	require(o != nil, 404, "not_found")
	if existing := t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution); existing != nil {
		require(existing.S("operationId") == operation && existing.S("nodeId") == node && jsonText(existing.O("input")) == jsonText(input), 409, "dispatch_conflict")
		return existing
	}
	require(o.S("step") == "clone" && o.S("state") == "running", 409, "dispatch_conflict")
	require(o.N("controllerEpoch") == r.Body.N("epoch"), 409, "stale_operation")
	require(workspaceCloneAttempts(t, operation) < workspaceCloneAttemptLimit, 409, "dispatch_conflict")
	wid := o.S("workspaceId")
	require(runtimeUsable(t, t.one("SELECT * FROM workspaces WHERE id=$1", wid), o.S("actorUserId")), 403, "runtime_use_forbidden")
	requireRepositoryAccess(t, o.S("projectId"))
	requireNoForceStop(t, wid)
	w := t.one("SELECT w.requested_ref,p.repository_url FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE w.id=$1", wid)
	require(input.S("repositoryUrl") == w.S("repositoryUrl") && input.S("branch") == w.S("requestedRef"), 409, "dispatch_conflict")
	require(currentNode(t, wid).S("nodeId") == node, 409, "dispatch_conflict")
	require(t.one("SELECT execution_id FROM clone_executions WHERE operation_id=$1 AND (result IS NULL OR result->>'outcome'='clone_ready')", operation) == nil, 409, "dispatch_conflict")
	t.exec("INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id) VALUES($1,$2,$3,$4,$5,$6,$1::text)", execution, operation, wid, node, jsonText(input), r.Body.N("epoch"))
	return t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution)
}

// advanceClone requires the operation's latest clone execution to have succeeded on this
// Workspace's Node and records its commit as the Workspace's baseline.
func advanceClone(t *transaction, o Object, wid string) {
	e := t.one("SELECT * FROM clone_executions WHERE operation_id=$1 ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	require(e != nil && e.O("result").S("outcome") == "clone_ready" && commitID(e.O("result").S("commit")), 409, "clone_incomplete")
	require(currentNode(t, wid).S("nodeId") == e.S("nodeId"), 409, "clone_incomplete")
	t.exec("UPDATE workspaces SET base_commit_id=$2,version=version+1 WHERE id=$1", wid, e.O("result").S("commit"))
}
