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
	require(runtimeUsable(t, t.one("SELECT * FROM workspaces WHERE id=$1", wid), o.S("actorUserId")), 403, "runtime_use_forbidden")
	requireRepositoryAccess(t, o.S("projectId"))
	requireNoForceStop(t, wid)
	// The dispatched input must equal the input this Operation's Workspace may be cloned from, derived
	// here from live state rather than compared against the recorded row: for a credentialed Workspace
	// that input is the internal mirror and not the Project's upstream (D1, D7), and the derivation is
	// the same one the snapshot used, so a Controller that was handed a redirect can only dispatch that
	// redirect. A credentialed Workspace whose mirror is not ready has no admissible input at all, and
	// the refusal is the retryable mirror gate rather than a conflict with a snapshot.
	//
	// The one dispatch that is not governed by that derivation is the clone Cloud executed itself: the
	// credentialed-clone executor clones the Project's upstream with the credential, so its registration
	// is matched against the input that channel has always recorded, and the mirror plays no part in it.
	// `credentialedResult` is set on the request that registers exactly that clone and on no other.
	admissible, code := t.store.cloneInputFor(t, o, wid)
	if r.credentialedResult != nil {
		admissible, code = t.store.workspaceCloneInput(t, o, wid)
	}
	require(code == "", 409, code)
	require(input.S("repositoryUrl") == admissible.repository && input.S("branch") == admissible.branch, 409, "dispatch_conflict")
	require(currentNode(t, wid).S("nodeId") == node, 409, "dispatch_conflict")
	require(t.one("SELECT execution_id FROM clone_executions WHERE operation_id=$1 AND (result IS NULL OR result->>'outcome'='clone_ready')", operation) == nil, 409, "dispatch_conflict")
	t.exec("INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id) VALUES($1,$2,$3,$4,$5,$6,$1::text)", execution, operation, wid, node, jsonText(input), r.Body.N("epoch"))
	return t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution)
}

// advanceClone requires the operation's latest clone execution to have succeeded on this
// Workspace's Node and records its commit as the Workspace's baseline.
//
// A Workspace that already has a baseline may not have it moved by a clone. For a mirrored Workspace
// the baseline is the commit the mirror pins — pinned when the transition to `clone` was admitted —
// and the mirror holds one branch at that commit, so a Node reporting any other commit did not clone
// the artifact Cloud admitted. It is refused rather than recorded: recording it would silently rebind
// the Workspace, and therefore every later read authorization, to content no mirror serves.
func advanceClone(t *transaction, o Object, wid string) {
	e := t.one("SELECT * FROM clone_executions WHERE operation_id=$1 ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	require(e != nil && e.O("result").S("outcome") == "clone_ready" && commitID(e.O("result").S("commit")), 409, "clone_incomplete")
	require(currentNode(t, wid).S("nodeId") == e.S("nodeId"), 409, "clone_incomplete")
	w := t.one("SELECT base_commit_id FROM workspaces WHERE id=$1", wid)
	require(w.S("baseCommitId") == "" || w.S("baseCommitId") == e.O("result").S("commit"), 409, "clone_incomplete")
	t.exec("UPDATE workspaces SET base_commit_id=$2,version=version+1 WHERE id=$1", wid, e.O("result").S("commit"))
}
