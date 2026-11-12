package core

import (
	"context"
	"time"
)

// executionWorkLiveRuns excludes business-closed IssueRuns without discarding their recovery
// evidence. The execution API also serves unphased callers; only deletion or the lifecycle's
// release/done stages revoke their ability to register new work. Claim and dispatch apply the same
// predicate, but only to work owned by an IssueRun: workflow work lives in workflow_runs.
const executionWorkLiveRuns = "SELECT id FROM issue_runs WHERE deleted_at IS NULL AND (phase IS NULL OR phase NOT IN ('releasing','done'))"

// EnqueueExecutionWork releases one Agent session or Revision delivery for Controllers to claim.
// Repeating the same unregistered input returns the original item. A different input conflicts.
func (s *Store) EnqueueExecutionWork(ctx context.Context, runID, kind string, input, target Object, availableAt time.Time) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		return enqueueExecutionWork(t, runID, kind, input, target, "", 0, availableAt)
	})
}

// graphNode names the workflow graph node a WorkflowRun's session work belongs to, so the scheduler
// can read that node's own execution back (workflowAgentSessionOutcome), and graphAttempt names the
// node's attempt: one node has one work item per attempt, so the attempt is part of the item's
// identity the same way the node id is (retry decision D4). IssueRun work has no graph and passes
// "" and zero; a workflow item without a node (the public seam) is attempt 1.
func enqueueExecutionWork(t *transaction, runID, kind string, input, target Object, graphNode string, graphAttempt int64, availableAt time.Time) Object {
	require(validID(runID), 400, "invalid_dispatch")
	run := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if run == nil {
		return enqueueWorkflowExecutionWork(t, runID, kind, input, target, graphNode, graphAttempt, availableAt)
	}
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE run_id=$1", runID) == nil, 409, "dispatch_conflict")
	requireNoControlledReclamation(t, target.S("workspaceId"), "")
	if kind == "deliver_revision" {
		if t.one("SELECT run_id FROM revision_delivery_skips WHERE run_id=$1", runID) != nil {
			return Object{"skipped": true}
		}
		require(t.one("SELECT id FROM revisions WHERE run_id=$1", runID) == nil, 409, "delivery_already_settled")
	}
	require(kind == "agent_session" || kind == "deliver_revision" || kind == "deliver_scheduled_artifacts", 400, "invalid_dispatch")
	require(input.S("kind") == kind, 400, "invalid_dispatch")
	require(target.S("workspaceId") != "" && target.S("sandboxInstanceId") != "" && target.S("nodeId") != "", 400, "invalid_dispatch")
	// One unregistered item per run *and kind*: a scheduled run holds its Revision delivery and its
	// artifact upload at the same time, and dispatch still finds each of them by `kind`.
	if existing := t.one("SELECT * FROM execution_work WHERE run_id=$1 AND kind=$2 AND execution_id IS NULL", runID, kind); existing != nil {
		if kind == "deliver_revision" {
			input = deliveryWorkInput(t, run, existing.S("id"), input)
		}
		require(existing.S("kind") == kind && jsonText(existing.O("input")) == jsonText(input) && jsonText(existing.O("target")) == jsonText(target), 409, "dispatch_conflict")
		return existing
	}
	id := newID()
	if kind == "deliver_revision" {
		// B owns the skipped/releasing transition; A supplies its same-transaction settlement. A
		// delivery needs both the object store (session history) and the Git Server (code); the
		// object store is named first when both are missing (Revision D2).
		reason := ""
		switch {
		case t.objectStore == nil:
			reason = "object_store_unconfigured"
		case t.store.GitServer == nil:
			reason = gitServerUnconfigured
		}
		if reason != "" {
			if t.execRows("INSERT INTO revision_delivery_skips(run_id) VALUES($1) ON CONFLICT DO NOTHING", runID) == 1 {
				deliverySettled(t, runID, "", Object{"outcome": "skipped", "reason": reason})
			}
			return Object{"skipped": true}
		}
		input = deliveryWorkInput(t, run, id, input)
	}
	if availableAt.IsZero() {
		t.exec("INSERT INTO execution_work(id,run_id,kind,input,target) VALUES($1,$2,$3,$4,$5)", id, runID, kind, jsonText(input), jsonText(target))
	} else {
		t.exec("INSERT INTO execution_work(id,run_id,kind,input,target,available_at) VALUES($1,$2,$3,$4,$5,$6)", id, runID, kind, jsonText(input), jsonText(target), availableAt)
	}
	t.workSignals = append(t.workSignals, runID)
	return t.one("SELECT * FROM execution_work WHERE id=$1", id)
}

// enqueueWorkflowExecutionWork releases one workflow run's session work for Controllers to claim.
// D8: the item registers against workflow_runs — exactly one unregistered item per run (the
// workflow partial index), kind fixed to agent_session, and no delivery variant (D7 defers
// 成果交付). graphNode records which graph node's session this is: a run has one agent node
// running at a time but several over its life (D4), and the scheduler reads the node's terminal
// evidence back through this link. Repeating the same unregistered input returns the original item;
// a different input conflicts.
func enqueueWorkflowExecutionWork(t *transaction, runID, kind string, input, target Object, graphNode string, graphAttempt int64, availableAt time.Time) Object {
	require(t.one("SELECT id FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID) != nil, 404, "not_found")
	require(kind == "agent_session", 400, "invalid_dispatch")
	require(t.one("SELECT id FROM workflow_runs WHERE id=$1 AND status='running' AND cancel_requested_at IS NULL AND delivery_state='pending'", runID) != nil, 409, "dispatch_conflict")
	require(input.S("kind") == kind, 400, "invalid_dispatch")
	require(target.S("workspaceId") != "" && target.S("sandboxInstanceId") != "" && target.S("nodeId") != "", 400, "invalid_dispatch")
	attempt := graphAttempt
	if attempt < 1 {
		attempt = 1
	}
	if existing := t.one("SELECT * FROM execution_work WHERE workflow_run_id=$1 AND kind=$2 AND execution_id IS NULL", runID, kind); existing != nil {
		require(existing.S("kind") == kind && existing.S("workflowNodeId") == graphNode && existing.N("workflowNodeAttempt") == attempt && jsonText(existing.O("input")) == jsonText(input) && jsonText(existing.O("target")) == jsonText(target), 409, "dispatch_conflict")
		return existing
	}
	id := newID()
	// Work released without a graph node (the public seam) stores NULL, not the empty string: the
	// column's CHECK and the scheduler's node-scoped lookups both read "no node" as NULL. The attempt
	// is written explicitly and never derived from the wire: it is the scheduler's own counter.
	if availableAt.IsZero() {
		t.exec("INSERT INTO execution_work(id,workflow_run_id,kind,input,target,workflow_node_id,workflow_node_attempt) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7)", id, runID, kind, jsonText(input), jsonText(target), graphNode, attempt)
	} else {
		t.exec("INSERT INTO execution_work(id,workflow_run_id,kind,input,target,workflow_node_id,workflow_node_attempt,available_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8)", id, runID, kind, jsonText(input), jsonText(target), graphNode, attempt, availableAt)
	}
	t.workSignals = append(t.workSignals, runID)
	return t.one("SELECT * FROM execution_work WHERE id=$1", id)
}

// deliveryWorkInput fixes server-owned locations before an execution identity exists: the session
// history key, the Node-local revision ref, the Git Server repository and this attempt's own
// delivery ref, and the run Workspace's branch from the clone that made its checkout (Revision D1,
// contract D6). Copying the caller's map keeps retries from accidentally retaining an earlier
// attempt's locations.
func deliveryWorkInput(t *transaction, run Object, workID string, input Object) Object {
	w := t.one("SELECT * FROM workspaces WHERE issue_run_id=$1 AND deleted_at IS NULL", run.S("id"))
	require(w != nil, 409, "dispatch_conflict")
	require(commitID(input.S("baseCommit")) && input.S("baseCommit") == w.S("baseCommitId"), 409, "dispatch_conflict")
	require(input.S("sessionExecutionId") != "" && input.S("checkoutExecutionId") != "", 400, "invalid_dispatch")
	require(t.store.GitServer != nil, 409, gitServerUnconfigured)
	checkout := t.one("SELECT input FROM clone_executions WHERE execution_id=$1 AND workspace_id=$2 AND result->>'outcome'='clone_ready'", input.S("checkoutExecutionId"), w.S("id"))
	require(checkout != nil && checkout.O("input").S("workspaceRef") != "", 409, "dispatch_conflict")
	out := Object{}
	for key, value := range input {
		out[key] = value
	}
	prefix := "revisions/" + run.S("tenantId") + "/" + run.S("id") + "/" + workID + "/"
	out["historyKey"] = prefix + "session.jsonl"
	out["revisionRef"] = "refs/ora/revisions/" + run.S("id")
	out["gitRepositoryUrl"] = t.store.GitServer.gitRepositoryURL(w.S("tenantId"), w.S("projectId"))
	out["deliveryRef"] = GitDeliveryRefPrefix + workID
	out["workspaceRef"] = checkout.O("input").S("workspaceRef")
	return out
}

// claimWorkItem is the pure read behind ClaimWork: the oldest unregistered session or delivery.
// Ownership moves only when the dispatch is recorded. Clones are never work items: every clone is
// a Workspace operation's clone step, handed out by the operation claim.
func claimWorkItem(t *transaction) Object {
	// A work item names the run it belongs to in one of two columns: agent work carries run_id (an
	// issue_runs id), workflow work carries workflow_run_id (a workflow_runs id) and leaves run_id
	// NULL by the mutual-exclusion CHECK 0039 added. The Controller is handed the run id as the
	// operation id and echoes it back on dispatch, which resolves either column — so the claim has to
	// project one value. Reading the agent-only column instead hands back the empty string a workflow
	// row produces, and every dispatch of that item is then refused as a conflict it cannot resolve.
	//
	// The alias is deliberate rather than coalescing over run_id: the row is serialized with
	// row_to_json, which keeps both copies of a repeated column name, and which one the decoder ends
	// up with is not something this query should depend on.
	//
	// The eligibility fence covers IssueRun-owned work only. Workflow work leaves run_id NULL and its
	// run lives in workflow_runs, so joining it to the issue_runs predicate would hide every workflow
	// item from the queue.
	work := t.one(`SELECT ew.*, coalesce(ew.run_id, ew.workflow_run_id) AS claimed_run_id
		FROM execution_work ew
		WHERE execution_id IS NULL AND available_at<=clock_timestamp()
		  AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.run_id=ew.run_id)
		  AND (ew.run_id IS NULL OR ew.run_id IN (` + executionWorkLiveRuns + `))
		ORDER BY created_at,id LIMIT 1`)
	if work == nil {
		return Object{"work": nil}
	}
	return Object{"work": work}
}
