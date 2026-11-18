package core

// deliveryFactIsLate classifies intake again in the final write transaction. Evidence reads may
// race D5, but accepting an original fact never grants a new dispatch or business settlement.
// Ordinary deliveries still pass the unchanged liveDelivery fence.
func deliveryFactIsLate(t *transaction, e Object) bool {
	if len(e.O("input").O("workflow")) > 0 {
		workflowDeliveryIdentity(t, e)
		run := t.one("SELECT delivery_state FROM workflow_runs WHERE id=$1", e.S("operationId"))
		if run.S("deliveryState") != "delivering" {
			return true
		}
		liveWorkflowDelivery(t, e)
		return false
	}
	if residualExecutionIdentity(t, e) != nil {
		return true
	}
	if lateOriginalDelivery(t, e) {
		return true
	}
	liveDelivery(t, e)
	return false
}

// lateOriginalDelivery admits only the registered delivery abandoned by root D5/D8. It does not
// create a residual responsibility, require a connected Node, or reopen the deleted runtime.
func lateOriginalDelivery(t *transaction, e Object) bool {
	if e.S("kind") != "deliver_revision" {
		return false
	}
	// D1 retains the original identity after soft deletion; visibility is not takeover eligibility.
	run := t.one("SELECT * FROM issue_runs WHERE id=$1", e.S("operationId"))
	if run == nil {
		return false
	}
	givenUpPhase := run.S("phase") == "releasing" || (run.S("phase") == "done" && run.S("failureReason") == "workspace_unavailable")
	if run.S("executorType") != "agent" || run.O("result").S("deliveryState") != "failed" || !givenUpPhase {
		return false
	}
	require(run.O("result")["revisionId"] == nil && e["terminatedByForceStopId"] == nil && e.S("nodeOperationId") == e.S("executionId"), 409, "dispatch_conflict")
	w := t.one(`SELECT w.* FROM workspaces w JOIN projects p ON p.id=w.project_id AND p.tenant_id=w.tenant_id
 WHERE w.id=$1 AND w.issue_run_id=$2 AND w.tenant_id=$3`, e.S("workspaceId"), run.S("id"), run.S("tenantId"))
	// A failed quiesce restores running/ready and admission while the Run stays given up.
	// Eligibility belongs to the Run and original identity, not the current delete attempt.
	require(w != nil && run.S("workspaceId") == w.S("id"), 409, "dispatch_conflict")
	work := t.one("SELECT * FROM execution_work WHERE id=$1 AND run_id=$2 AND kind='deliver_revision'", e.S("workId"), run.S("id"))
	require(work != nil && work.S("executionId") == e.S("executionId") && jsonText(work.O("input")) == jsonText(e.O("input")), 409, "dispatch_conflict")
	target := work.O("target")
	require(target.S("workspaceId") == w.S("id") && target.S("nodeId") == e.S("nodeId"), 409, "dispatch_conflict")
	// Production targets freeze the sandbox id; its immutable generation is the generation
	// evidence. Older targets may additionally carry the generation explicitly.
	if target["runtimeGeneration"] != nil {
		require(target.N("runtimeGeneration") == w.N("runtimeGeneration"), 409, "dispatch_conflict")
	}
	sandbox := t.one(`SELECT s.id FROM sandbox_instances s JOIN node_instances n ON n.sandbox_instance_id=s.id
 WHERE s.id=$1 AND s.workspace_id=$2 AND s.generation=$3 AND n.workspace_id=s.workspace_id AND n.node_id=$4
 AND NOT EXISTS(SELECT 1 FROM node_instances replacement WHERE replacement.sandbox_instance_id=s.id
 AND replacement.ended_at IS NULL AND replacement.node_id<>n.node_id)`, target.S("sandboxInstanceId"), w.S("id"), w.N("runtimeGeneration"), e.S("nodeId"))
	require(sandbox != nil, 409, "dispatch_conflict")
	return true
}

// lateRevisionMayRegister uses the same transaction/advisory lock as enqueueRun. Every Run of
// this Issue, including deleted and non-agent Runs, must have exactly one creation record. Missing
// or ambiguous evidence is receipt-only; timestamps and UUIDs cannot recover a creation order.
func lateRevisionMayRegister(t *transaction, e Object) bool {
	row := t.one(`WITH original AS (
 SELECT id,tenant_id,issue_id FROM issue_runs WHERE id=$1
), ordered_runs AS (
 SELECT r.id,count(a.id) AS creation_count,min(a.seq) AS seq
 FROM original o JOIN issue_runs r ON r.tenant_id=o.tenant_id AND r.issue_id=o.issue_id
 LEFT JOIN issue_activities a ON a.tenant_id=r.tenant_id AND a.issue_id=r.issue_id
 AND a.action='run.enqueued' AND a.details->>'runId'=r.id::text
 GROUP BY r.id
)
SELECT EXISTS(SELECT 1 FROM ordered_runs original WHERE original.id=$1 AND original.creation_count=1
 AND NOT EXISTS(SELECT 1 FROM ordered_runs other WHERE other.creation_count<>1 OR other.seq>original.seq)) AS allowed`, e.S("operationId"))
	return row.B("allowed")
}
