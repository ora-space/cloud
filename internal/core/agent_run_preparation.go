package core

// withAgentRunDetails projects bounded preparation progress before removing private control fields.
// It exposes no Workspace/Node identity, repository address, credential or raw failure detail.
func withAgentRunDetails(t *transaction, run Object) Object {
	run["preparation"] = nil
	if run.S("executorType") == "agent" {
		run["preparation"] = agentRunPreparation(t, run)
	}
	return withRevision(t, stripAgentRunSkeleton(run))
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
			if code := op.S("errorCode"); deferCodes[code] {
				progress["errorCode"] = code
			}
		}
	}
	if stage == "failed" {
		progress["errorCode"] = publicSessionFailureCode(run.S("failureReason"))
	}
	return progress
}
