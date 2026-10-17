package core

// The clone step of a runtime Workspace (specs decisions/cloud/operation/0-workspace-runtime-follows-
// desktop-node.md D2, as changed by 20261016-clone-step-consumes-binding-snapshot.md). The Workspace's
// Node clones a Git Server snapshot of the Project's Binding, never the external repository. Cloud
// freezes that input into the operation before a Controller can see the step, and the Controller
// forwards it verbatim; the execution is bound to the operation and Workspace, and Cloud, not the
// Controller, decides its input and target Node.

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

// Error codes of a clone step that cannot be handed to a Controller (operation D1, D2).
const (
	// snapshotNotReady parks the step while the Binding's latest sync may still produce a snapshot.
	snapshotNotReady = "snapshot_not_ready"
	// gitServerUnconfigured blocks the step: the deployment has no Git Server to clone from, and
	// waiting cannot change that.
	gitServerUnconfigured = "git_server_unconfigured"
	// invalidWorkspaceRef blocks a step whose Workspace branch cannot name a Git ref. Project
	// creation refuses such branches, so it can only follow from data written before that rule.
	invalidWorkspaceRef = "invalid_workspace_ref"
	// repositoryOperationRunning parks delete_project while a sync or publish may still write.
	repositoryOperationRunning = "repository_operation_running"
)

// repositoryWaitSeconds is how long a step parked on repository state waits before the next claim
// rechecks it. The recheck, not a notification from the external line, ends the wait.
const repositoryWaitSeconds = 10

// workspaceCloneAttempts reads durable dispatches, rather than an in-memory retry counter. Waiting
// for a snapshot registers nothing and so never consumes an attempt.
func workspaceCloneAttempts(t *transaction, operationID string) int64 {
	return t.one("SELECT count(*) AS attempts FROM clone_executions WHERE operation_id=$1", operationID).N("attempts")
}

// cloneInputOf is the operation's frozen clone input, or nil before it is frozen.
func cloneInputOf(o Object) Object {
	input := o.O("request").O("clone")
	if len(input) == 0 {
		return nil
	}
	return input
}

// freezeCloneInput fixes the clone step's input from the Binding's current snapshot, once per
// operation (operation D1). It runs in the transaction that moves the operation into the clone step
// and in every claim of a clone step not yet frozen. It reports whether the operation may be handed
// to a Controller; when it may not, the operation has been parked or ended here (operation D2), so a
// Controller never sees a clone step without its input. A frozen input is never recomputed: every
// execution of the operation, including retries after a failure, clones the same snapshot.
func freezeCloneInput(t *transaction, o Object) bool {
	if cloneInputOf(o) != nil {
		return true
	}
	g := t.store.GitServer
	if g == nil {
		parkOperation(t, o, "blocked", gitServerUnconfigured, 0)
		return false
	}
	// Phase one has exactly one Binding per Project. The snapshot row exists only after Cloud
	// verified its ref on the Git Server, so it never names a ref that is not there. The snapshot and
	// the Binding's latest sync are read in one statement, so one consistent view decides (operation
	// D2): the external line records a successful sync and its snapshot in one transaction without
	// taking any lock the claim holds, and two statements at READ COMMITTED could see no snapshot and
	// then that sync's success, failing a step that has a snapshot to clone.
	b := t.one(`SELECT b.id AS binding_id,s.commit_oid,s.snapshot_ref,ls.id AS sync_id,ls.state AS sync_state,ls.error_code AS sync_error_code
	 FROM repository_bindings b LEFT JOIN repository_snapshots s ON s.binding_id=b.id
	 LEFT JOIN LATERAL (SELECT id,state,error_code FROM repository_operations WHERE binding_id=b.id AND type=$2 ORDER BY created_at DESC,id DESC LIMIT 1) ls ON true
	 WHERE b.project_id=$1`, o.S("projectId"), RepositorySync)
	if b == nil {
		// Every Project is created with its Binding; parking keeps a defect from failing every claim.
		parkOperation(t, o, "blocked", "binding_missing", 0)
		return false
	}
	if b.S("commitOid") == "" {
		awaitSnapshot(t, o, b)
		return false
	}
	w := t.one("SELECT id,tenant_id,project_id,requested_ref FROM workspaces WHERE id=$1", o.S("workspaceId"))
	workspaceRef := GitWorkspaceRefPrefix(w.S("id")) + w.S("requestedRef")
	if name, ok := ParseGitRef(workspaceRef); !ok || name.Namespace != GitWorkspaceBranch {
		parkOperation(t, o, "blocked", invalidWorkspaceRef, 0)
		return false
	}
	input := Object{
		"kind":          "clone",
		"repositoryUrl": g.gitRepositoryURL(w.S("tenantId"), w.S("projectId")),
		"snapshotRef":   b.S("snapshotRef"),
		"baseCommit":    b.S("commitOid"),
		"workspaceRef":  workspaceRef,
	}
	t.exec("UPDATE operations SET request=jsonb_set(request,'{clone}',$2::jsonb),version=version+1,updated_at=now() WHERE id=$1", o.S("id"), jsonText(input))
	return true
}

// awaitSnapshot decides a clone step whose Binding has no current snapshot by the Binding's latest
// sync (operation D2), read together with the snapshot: while it may still produce one the step waits
// without consuming an attempt; once it ended without success the step fails with that sync
// recorded. It never falls back to the external repository and never starts a sync itself.
func awaitSnapshot(t *transaction, o, b Object) {
	switch b.S("syncState") {
	case "pending", "leased", "running":
		parkOperation(t, o, "retry_wait", snapshotNotReady, repositoryWaitSeconds)
		return
	}
	// Every Binding is created with its first sync and a successful sync records its snapshot in the
	// same transaction, so no sync at all, or a successful latest sync without a snapshot, is a defect
	// and fails the same way rather than waiting forever.
	result := Object{}
	if id := b.S("syncId"); id != "" {
		result["syncOperationId"] = id
		if code := b.S("syncErrorCode"); code != "" {
			result["syncErrorCode"] = code
		}
	}
	t.exec("UPDATE operations SET result=$2 WHERE id=$1", o.S("id"), jsonText(result))
	failWorkspaceClone(t, o, repositorySyncFailed)
}

// parkOperation takes an operation out of the claimable set without handing it to a Controller.
// retry_wait becomes claimable again after delaySeconds; blocked needs an explicit retry.
func parkOperation(t *transaction, o Object, state, code string, delaySeconds int64) {
	if state == "blocked" {
		t.exec("UPDATE operations SET state='blocked',error_code=$2,retry_at=NULL,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), code)
		return
	}
	t.exec("UPDATE operations SET state='retry_wait',error_code=$2,retry_at=clock_timestamp()+($3 * interval '1 second'),version=version+1,updated_at=now() WHERE id=$1", o.S("id"), code, delaySeconds)
}

// deferWorkspaceClone retries only settled failures, with a server-owned bounded backoff.
// An unknown outcome retains its existing blocked policy and can never consume a new dispatch.
func deferWorkspaceClone(t *transaction, o Object) {
	latest := t.one("SELECT result FROM clone_executions WHERE operation_id=$1 ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	require(latest.O("result").S("outcome") == "clone_failed", 409, "clone_incomplete")
	attempts := workspaceCloneAttempts(t, o.S("id"))
	if attempts < workspaceCloneAttemptLimit {
		parkOperation(t, o, "retry_wait", "clone_failed", int64(5)*max(attempts, 1))
		return
	}
	failWorkspaceClone(t, o, "clone_failed")
}

// failWorkspaceClone ends a clone step for good: admission stays closed, runtime maintenance is
// handed back, a run Workspace takes its failure release and a failed create_project still leaves
// the Project usable, so it can be repaired or deleted.
func failWorkspaceClone(t *transaction, o Object, code string) {
	t.exec("UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), code)
	if o.S("kind") == "create_project" {
		settleFailedProjectCreation(t, o.S("projectId"))
	}
	t.exec("UPDATE workspaces SET observed_state='unavailable',admission_open=false,version=version+1 WHERE id=$1", o.S("workspaceId"))
	finishRuntimeMaintenance(t, o.S("id"))
	if w := t.one("SELECT issue_run_id FROM workspaces WHERE id=$1", o.S("workspaceId")); w.S("issueRunId") != "" {
		runWorkspaceSettled(t, w.S("issueRunId"), "failed")
	}
}

// workspaceCloneDispatch registers the clone of a Workspace operation in its clone step. Replaying
// the same execution is idempotent; anything else must carry the operation's frozen input verbatim
// and name the Workspace's current Node, and waits until no earlier execution is still unresolved.
func workspaceCloneDispatch(t *transaction, r *ControlRequest, operation, execution, node string, input Object) Object {
	o := t.one("SELECT * FROM operations WHERE id=$1 AND workspace_id IS NOT NULL", operation)
	// Tenant-level clone requests no longer exist; an ID that names no Workspace operation is not a
	// clone this registry can accept.
	require(o != nil, 404, "not_found")
	if existing := t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution); existing != nil {
		require(existing.S("operationId") == operation && existing.S("nodeId") == node && jsonText(existing.O("input")) == jsonText(input), 409, "dispatch_conflict")
		return existing
	}
	require(o.S("step") == "clone" && o.S("state") == "running", 409, "dispatch_conflict")
	require(o.N("controllerEpoch") == r.Body.N("epoch"), 409, "stale_operation")
	frozen := cloneInputOf(o)
	require(frozen != nil && jsonText(frozen) == jsonText(input), 409, "dispatch_conflict")
	require(workspaceCloneAttempts(t, operation) < workspaceCloneAttemptLimit, 409, "dispatch_conflict")
	wid := o.S("workspaceId")
	require(runtimeUsable(t, t.one("SELECT * FROM workspaces WHERE id=$1", wid), o.S("actorUserId")), 403, "runtime_use_forbidden")
	requireNoForceStop(t, wid)
	require(currentNode(t, wid).S("nodeId") == node, 409, "dispatch_conflict")
	require(t.one("SELECT execution_id FROM clone_executions WHERE operation_id=$1 AND (result IS NULL OR result->>'outcome'='clone_ready')", operation) == nil, 409, "dispatch_conflict")
	t.exec("INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id) VALUES($1,$2,$3,$4,$5,$6,$1::text)", execution, operation, wid, node, jsonText(input), r.Body.N("epoch"))
	return t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution)
}

// advanceClone requires the operation's latest clone execution to have succeeded on this
// Workspace's Node with exactly the frozen snapshot commit, and records it as the Workspace's
// baseline (operation D3).
func advanceClone(t *transaction, o Object, wid string) {
	e := t.one("SELECT * FROM clone_executions WHERE operation_id=$1 ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	require(e != nil && e.O("result").S("outcome") == "clone_ready" && commitID(e.O("result").S("commit")), 409, "clone_incomplete")
	frozen := cloneInputOf(o)
	require(frozen != nil && e.O("result").S("commit") == frozen.S("baseCommit") && jsonText(e.O("input")) == jsonText(frozen), 409, "clone_incomplete")
	require(currentNode(t, wid).S("nodeId") == e.S("nodeId"), 409, "clone_incomplete")
	t.exec("UPDATE workspaces SET base_commit_id=$2,version=version+1 WHERE id=$1", wid, e.O("result").S("commit"))
}
