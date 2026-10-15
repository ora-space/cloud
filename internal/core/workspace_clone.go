package core

import "time"

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

// workspaceCloneAttemptLimit is how many clone executions one Operation's clone step may record before
// Cloud ends the step, and cloneRetryBackoff is the delay Cloud parks it for while attempts remain. D2
// fixes both: "同一 operation 以持久执行记录最多尝试三次；前两次 retry_wait 5/10 秒，第三次 failed，保留
// 执行记录；重试是新执行". The schedule is Cloud's, not the Controller's, and its 2026-10-11 revision says
// what that means for this budget — "次数和退避由 Cloud 决定，Controller 重启、重复提交或人工 retry 不
// 重置预算".
const workspaceCloneAttemptLimit = 3

// workspaceCloneAttempts reads the durable dispatches rather than an in-memory retry counter. Reading
// them is what makes the budget survive every reset D2 forbids: a Controller restart, a repeated request,
// and the public retry entry, which moves a parked Operation back to `queued` without deleting the
// executions that happened.
func workspaceCloneAttempts(t *transaction, operationID string) int64 {
	return t.one("SELECT count(*) AS attempts FROM clone_executions WHERE operation_id=$1", operationID).N("attempts")
}

// cloneRetryBackoff is the delay before the retry that follows `attempts` recorded executions: the first
// retry waits 5 seconds, the second 10, which is D2's schedule. The Controller proposes a delay of its
// own and Cloud overrides it for this step, because a Controller that restarted mid-retry still proposes
// its first-attempt constant.
func cloneRetryBackoff(attempts int64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	return time.Duration(5*attempts) * time.Second
}

// deferCloneFailure parks a clone step under Cloud's retry budget, or ends it when the budget is spent.
//
// A Node-reported clone failure is retryable — the repository may be briefly unreachable — but D2 does
// not let it be retried indefinitely: the third recorded execution is the last, and the Operation then
// settles as `failed`. Cloud applies the bound because Cloud owns it. The Controller reports a failure
// and nothing about how many have happened, and its defer carries a fixed delay, so a deployment whose
// repository is unreachable would otherwise re-clone for ever: the Workspace would never become ready,
// the Project would stay `provisioning`, and — because the failed Operation keeps coming back as the
// oldest candidate — no later Operation would be scheduled at all.
//
// Only a settled failure spends a retry. The latest recorded execution must itself report `clone_failed`:
// an outcome the Node could not tell is the separate `clone_result_unknown` park, and counting it here
// would spend one of the three attempts on a clone that may still be running in the sandbox.
//
// Ending the step also settles what the step was holding: `failWorkspaceOperation` is what does that,
// and it is shared with every other path that ends a Workspace-driving Operation.
//
// Admission is left closed on both paths: only a successful clone opens it (D2), so the Workspace
// cannot be used while its clone step is failing or after the step has ended.
func deferCloneFailure(t *transaction, o Object) Object {
	latest := t.one("SELECT result FROM clone_executions WHERE operation_id=$1 ORDER BY created_at DESC,execution_id DESC LIMIT 1", o.S("id"))
	require(latest.O("result").S("outcome") == "clone_failed", 409, "clone_incomplete")
	attempts := workspaceCloneAttempts(t, o.S("id"))
	if attempts < workspaceCloneAttemptLimit {
		t.exec("UPDATE operations SET state='retry_wait',error_code=$2,retry_at=clock_timestamp()+($3 * interval '1 second'),version=version+1,updated_at=now() WHERE id=$1",
			o.S("id"), "clone_failed", int64(cloneRetryBackoff(attempts)/time.Second))
		return t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
	}
	// `retry_at` is cleared rather than left as it was: `claim` moves a due retry to `running` without
	// clearing the deadline it fired on, so an ended Operation would otherwise carry a retry time that no
	// longer means anything and would be read back as a pending retry. The error code stays the one this
	// failure was reported with, so the public contract keeps showing the failure the Node reported.
	t.exec("UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now() WHERE id=$1", o.S("id"), "clone_failed")
	failWorkspaceOperation(t, o.S("id"))
	return t.one("SELECT * FROM operations WHERE id=$1", o.S("id"))
}

// failWorkspaceOperation settles everything a Workspace-driving Operation was holding, once the
// Operation row itself has already been recorded `failed`.
//
// It is deliberately separate from the write that fails the Operation and deliberately shared, because
// the two halves are the same obligation on every path that ends such an Operation: an Operation that
// stops being driven stops *being* the thing that holds the Workspace, the runtime reservation and the
// IssueRun. Leaving them behind is not a cosmetic omission — a Workspace whose Operation ended but
// whose `runtime_controls` row still says `maintenance` can never be handed to another Operation, and
// an IssueRun whose Workspace will never become ready never settles, which is the state that starves
// every later Operation of the Project.
//
// The three writes are the whole set, and each answers for one of the three things the Operation held:
//
//   - the Workspace is no longer available and admission stays closed. Only a successful clone and
//     plugin step opens admission (D2), so nothing here may open it, and a Workspace whose code was
//     never delivered must not keep advertising itself as ready.
//   - the runtime reservation is released through the ordinary closure path rather than by clearing the
//     row: `finishRuntimeMaintenance` requests closure and lets the Node confirm its durable
//     responsibilities, which is the same handover every other end of a lifecycle intent uses.
//   - the IssueRun the Workspace exists for is settled as `failed` when it has one. A Workspace created
//     for a run whose code can never arrive is a run that cannot proceed, and the run has to learn that
//     rather than wait forever.
//
// The Operation row is read here rather than passed in so that callers cannot hand over a stale copy:
// the Workspace is reached through the Operation's own `workspace_id`, and a caller that has just failed
// an Operation by id has nothing else it could name it by.
func failWorkspaceOperation(t *transaction, operation string) {
	w := t.one("SELECT w.* FROM workspaces w JOIN operations o ON o.workspace_id=w.id WHERE o.id=$1", operation)
	if w == nil {
		// Not an Operation that drives a Workspace: a Project-level or effect-level Operation holds no
		// Workspace and has nothing of this to settle.
		return
	}
	t.exec("UPDATE workspaces SET observed_state='unavailable',admission_open=false,version=version+1 WHERE id=$1", w.S("id"))
	finishRuntimeMaintenance(t, operation)
	if w.S("issueRunId") != "" {
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
	// D8 of the credentials decision: the registration records which credential admitted this clone, and
	// which version it was admitted under. It is written with the row rather than derived later, because
	// after a rotation nothing else can answer "which version did this clone use" — the reference now
	// names a different one, and the authorization that was spent is gone. The pair comes from the
	// admission `requireRepositoryAccess` performed above, in this same transaction, so the recorded
	// number is the version that guard actually compared rather than one read a second time.
	//
	// `execution_credential_binding` pairs the columns, so the only two shapes the row can take are both
	// and neither: an anonymous clone's absence is written here as two nulls, from this one decision,
	// instead of being left to a column default that could set half of it.
	var credentialID, credentialVersion any
	if c := t.admittedCredential; c != nil {
		credentialID, credentialVersion = c.CredentialID, c.CredentialVersion
	}
	t.exec(`INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,
			node_operation_id,credential_ref_id,credential_ref_version)
		VALUES($1,$2,$3,$4,$5,$6,$1::text,$7::uuid,$8::bigint)`,
		execution, operation, wid, node, jsonText(input), r.Body.N("epoch"), credentialID, credentialVersion)
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
