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
	wid := o.S("workspaceId")
	w := t.one("SELECT w.requested_ref,p.repository_url FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE w.id=$1", wid)
	require(input.S("repositoryUrl") == w.S("repositoryUrl") && input.S("branch") == w.S("requestedRef"), 409, "dispatch_conflict")
	require(currentNode(t, wid).S("nodeId") == node, 409, "dispatch_conflict")
	require(t.one("SELECT execution_id FROM clone_executions WHERE operation_id=$1 AND (result IS NULL OR result->>'outcome'='clone_ready')", operation) == nil, 409, "dispatch_conflict")
	t.exec("INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch) VALUES($1,$2,$3,$4,$5,$6)", execution, operation, wid, node, jsonText(input), r.Body.N("epoch"))
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
