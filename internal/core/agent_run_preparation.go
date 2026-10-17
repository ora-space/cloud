package core

// withAgentRunDetails projects bounded preparation progress before removing private control fields.
// It exposes no Workspace/Node identity, repository address, credential or raw failure detail.
func withAgentRunDetails(t *transaction, run Object) Object {
	run["preparation"] = nil
	if run.S("executorType") == "agent" {
		if p := agentRunPreparation(t, run); p != nil {
			p["syncErrorCode"] = preparationSyncErrorCode(t, run, p)
			run["preparation"] = p
		}
	}
	return withRevision(t, stripAgentRunSkeleton(run))
}

// preparationWaitCodes are the clone-step codes a run's preparation shows as they are: safe, bounded
// reasons the step is not making progress that name no Binding, snapshot or deployment detail.
var preparationWaitCodes = map[string]bool{snapshotNotReady: true, gitServerUnconfigured: true}

// repositorySyncFailed is the preparation error code of a clone step that ended because its
// Binding's latest sync failed; syncErrorCode then names why.
const repositorySyncFailed = "repository_sync_failed"

// preparationSyncErrorCode is the failed sync's error code when the clone step ended with
// repository_sync_failed, and nil otherwise. Sync error codes are the external line's safe,
// bounded vocabulary (the database admits only short snake_case codes); no executor detail is
// ever stored there.
func preparationSyncErrorCode(t *transaction, run, progress Object) any {
	if progress.S("errorCode") != repositorySyncFailed || run.S("workspaceId") == "" {
		return nil
	}
	// The clone step recorded the very sync it failed on (operation D2); the latest failed sync of
	// the Binding answers for operations that ended before that record existed.
	if op := t.one("SELECT result FROM operations WHERE workspace_id=$1 AND kind='create_workspace' AND error_code=$2 ORDER BY created_at DESC,id DESC LIMIT 1", run.S("workspaceId"), repositorySyncFailed); op.O("result").S("syncErrorCode") != "" {
		return op.O("result").S("syncErrorCode")
	}
	o := t.one(`SELECT o.error_code FROM repository_operations o JOIN workspaces w ON w.project_id=o.project_id
	 WHERE w.id=$1 AND o.type='repository_sync' AND o.state='failed' ORDER BY o.finished_at DESC,o.id DESC LIMIT 1`, run.S("workspaceId"))
	if o == nil || o.S("errorCode") == "" {
		return nil
	}
	return o.S("errorCode")
}

// agentRunPreparation reads progress and durable attempt counts in the run read's transaction.
func agentRunPreparation(t *transaction, run Object) Object {
	if run.S("status") == "cancelled" {
		return Object{"stage": "cancelled", "cloneAttempts": int64(0), "maxCloneAttempts": workspaceCloneAttemptLimit, "retryAt": nil, "errorCode": nil}
	}
	stage := "waiting"
	switch run.S("phase") {
	case "starting":
		stage = "start"
	case "provisioning":
		stage = "environment"
	case "running", "delivering", "releasing", "done":
		if run.S("status") != "failed" {
			return nil
		}
		stage = "failed"
	}
	if run.S("status") == "failed" {
		stage = "failed"
	}
	progress := Object{"stage": stage, "cloneAttempts": int64(0), "maxCloneAttempts": workspaceCloneAttemptLimit, "retryAt": nil, "errorCode": nil}
	if run.S("workspaceId") != "" {
		op := t.one("SELECT id,step,state,error_code,retry_at FROM operations WHERE workspace_id=$1 AND kind='create_workspace' ORDER BY created_at DESC,id DESC LIMIT 1", run.S("workspaceId"))
		if op != nil {
			progress["cloneAttempts"] = workspaceCloneAttempts(t, op.S("id"))
			progress["retryAt"] = op["retryAt"]
			if stage == "environment" && (op.S("step") == "clone" || op.S("step") == "plugin") {
				progress["stage"] = op.S("step")
			}
			if op.S("state") == "failed" && op.S("errorCode") == "clone_failed" {
				progress["stage"] = "failed"
				progress["errorCode"] = "clone_attempts_exhausted"
				return progress
			}
			// The Binding's sync ended without a snapshot to clone: the run fails for that reason,
			// whatever the session layer later records (operation D2).
			if op.S("state") == "failed" && op.S("errorCode") == repositorySyncFailed {
				progress["stage"] = "failed"
				progress["errorCode"] = repositorySyncFailed
				return progress
			}
			if code := op.S("errorCode"); deferCodes[code] || preparationWaitCodes[code] {
				progress["errorCode"] = code
			}
		}
	}
	if stage == "failed" {
		progress["errorCode"] = publicSessionFailureCode(run.S("failureReason"))
	}
	return progress
}
