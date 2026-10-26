package core

// WorkflowRun resume: the public request registered by specs decisions/cloud/workflow
// 20261021-node-retry-and-resume-contract.md D7, and explicitly unavailable in the first phase.
//
// The final contract resumes a terminal `failed` run inside the same run identity: the failed node
// and its descendants are cleared, the surviving state is reprojected, and the scheduler dispatches
// from there, with `keep` / `node_files` / `checkpoint` selecting how much Workspace state rolls
// back. None of that can run yet — a failed run's Workspace is already declared deleted, so `keep`
// has nothing to resume onto, and the checkpoint infrastructure does not exist. What this phase
// ships is the endpoint's shape: authorization identical to cancellation, request validation against
// the final contract, and a stable named refusal. The alternative owner rejected — not registering
// the route — leaves clients unable to tell "the server cannot do this" from "the path is wrong".

// workflowResumeModes is the closed set of rollback modes the final contract defines (D7). It is
// validated now so a client can integrate against the final shape and a request that will never be
// valid is answered as malformed rather than as unavailable.
var workflowResumeModes = map[string]bool{"keep": true, "node_files": true, "checkpoint": true}

// resumeWorkflowRun is the public request behind
// `POST .../workflows/:wfid/runs/:rid/resume`. It checks the same things cancellation checks, in the
// same order — resource scope, then authorization — and then refuses with the published code.
//
// The refusal is a Fault, so the transaction rolls back: no run, node or Workspace state changes,
// and no idempotency record is written (Public writes one only after the handler returns). A caller
// that repeats the request therefore gets the same answer every time rather than a replay of a
// recorded success.
//
// Authorization runs before the availability check so an unauthorized caller never learns whether
// the operation is available, and resource lookup before both so a run of another tenant or workflow
// is indistinguishable from a missing one.
func resumeWorkflowRun(t *transaction, r *PublicRequest, uid string) Object {
	workflow(t, r.TenantID, r.WorkflowID)
	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT * FROM workflow_runs
		WHERE id=$1 AND tenant_id=$2 AND workflow_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, r.WorkflowID)
	require(run != nil, 404, "not_found")
	require(mayControlWorkflowRun(t, run, uid), 403, "workflow_run_resume_forbidden")
	// `mode` defaults to keep only when the field is absent. Every present value must be one of the
	// three mode strings: a null, a non-string or an unknown string is the final contract's
	// invalid_field_value, because the field was given but names no mode.
	if mode, present := r.Body["mode"]; present {
		text, ok := mode.(string)
		require(ok && workflowResumeModes[text], 400, "invalid_field_value")
	}
	reject(501, "workflow_run_resume_unavailable")
	return nil
}
