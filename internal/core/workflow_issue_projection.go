package core

import (
	"strings"
	"unicode/utf8"
)

// workflowIssueOutputSummary selects existing output-node results or the run error. Callers bound
// the UTF-8 message before projection so execution settlement cannot fail on activity size alone.
func workflowIssueOutputSummary(graph, states Object, runErr string) string {
	message := strings.TrimSpace(runErr)
	if message == "" {
		outputs := Object{}
		for _, node := range readWorkflowGraph(graph).Nodes {
			if state := states.O(node.ID); node.Kind == "output" && state.S("status") == "succeeded" && len(state.O("output")) > 0 {
				outputs[node.ID] = state.O("output")
			}
		}
		if len(outputs) > 0 {
			message = jsonText(outputs)
		}
	}
	if len(message) > 20000 {
		end := 20000
		for !utf8.RuneStart(message[end]) {
			end--
		}
		message = message[:end]
	}
	return message
}

// projectWorkflowRunToIssueRun mirrors the committed-in-this-transaction workflow state onto its
// optional IssueRun. Execution callers invoke it only after a successful state write, in the same
// Store.transact boundary, so a projection or activity failure rolls back the workflow transition.
// Direct workflow runs have no IssueRun and are a no-op. The caller supplies the execution's output
// summary because per-node output has no canonical summary field in workflow_runs. awaiting_input
// has no IssueRun counterpart; it leaves the current projection untouched without blocking the
// caller's workflow transition.
func projectWorkflowRunToIssueRun(t *transaction, tid, workflowRunID, outputSummary string) Object {
	w := t.one("SELECT * FROM workflow_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", workflowRunID, tid)
	require(w != nil, 404, "not_found")
	i := t.one("SELECT * FROM issue_runs WHERE workflow_invocation_ref=$1 AND tenant_id=$2 AND executor_type='workflow' AND deleted_at IS NULL FOR UPDATE", workflowRunID, tid)
	if i == nil {
		return nil
	}
	require(i.S("executorId") == w.S("workflowId"), 409, "workflow_run_projection_conflict")
	var status, action string
	switch w.S("status") {
	case "pending":
		status = "queued"
	case "running":
		status, action = "running", "run.started"
	case "awaiting_input":
		return run(t, tid, i.S("issueId"), i.S("id"))
	case "succeeded":
		status, action = "completed", "run.completed"
	case "failed":
		status, action = "failed", "run.failed"
	case "cancelled":
		status, action = "cancelled", "run.cancelled"
	default:
		require(false, 409, "workflow_run_state_not_projectable")
	}
	if i.S("status") == status {
		return run(t, tid, i.S("issueId"), i.S("id"))
	}
	// A terminal IssueRun cannot be reopened or silently given a different outcome. Recovery must
	// project the authoritative current WorkflowRun, never replay a stale transition argument.
	require(i.S("status") == "queued" || i.S("status") == "dispatched" || i.S("status") == "running", 409, "workflow_run_projection_conflict")
	require(status != "queued", 409, "workflow_run_projection_conflict")
	terminal := status == "completed" || status == "failed" || status == "cancelled"
	if status == "running" {
		require(w.S("startedAt") != "", 409, "workflow_run_projection_incomplete")
	}
	if terminal {
		require(w.S("finishedAt") != "", 409, "workflow_run_projection_incomplete")
	}
	message := strings.TrimSpace(outputSummary)
	require(len(message) <= 20000, 400, "invalid_input")
	reason := ""
	if status == "failed" || status == "cancelled" {
		reason = w.S("error")
	}
	t.exec(`UPDATE issue_runs i SET status=$3, started_at=w.started_at,
		completed_at=w.finished_at, error=$4, failure_reason=$4,
		version=i.version+1, updated_at=now()
		FROM workflow_runs w WHERE i.id=$1 AND i.tenant_id=$2
		AND w.id=i.workflow_invocation_ref AND w.tenant_id=i.tenant_id`, i.S("id"), tid, status, reason)
	details := runActivityDetails(i.S("id"), "workflow", i.S("executorId"), Object{"workflowInvocationRef": workflowRunID})
	// A scheduler may finish a workflow within one transaction, before an intermediate running
	// projection exists. Only authoritative start evidence permits the missing lifecycle activity;
	// cancellation before execution must never fabricate a start.
	if terminal && (i.S("status") == "queued" || i.S("status") == "dispatched") && w.S("startedAt") != "" {
		appendActivity(t, tid, i.S("issueId"), "system", nil, "run.started", details)
	}
	if terminal && message != "" {
		messageDetails := runActivityDetails(i.S("id"), "workflow", i.S("executorId"), Object{"workflowInvocationRef": workflowRunID, "message": message})
		appendActivity(t, tid, i.S("issueId"), "system", nil, "run.message", messageDetails)
	}
	if status == "failed" {
		details["failureReason"] = reason
	}
	appendActivity(t, tid, i.S("issueId"), "system", nil, action, details)
	return run(t, tid, i.S("issueId"), i.S("id"))
}
