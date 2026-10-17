package core

import (
	"context"
	"time"
)

// EnqueueExecutionWork releases one Agent session or Revision delivery for Controllers to claim.
// Repeating the same unregistered input returns the original item. A different input conflicts.
func (s *Store) EnqueueExecutionWork(ctx context.Context, runID, kind string, input, target Object, availableAt time.Time) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		return enqueueExecutionWork(t, runID, kind, input, target, "", availableAt)
	})
}

// graphNode names the workflow graph node a WorkflowRun's session work belongs to, so the scheduler
// can read that node's own execution back (workflowAgentSessionOutcome). IssueRun work has no graph
// and passes "".
func enqueueExecutionWork(t *transaction, runID, kind string, input, target Object, graphNode string, availableAt time.Time) Object {
	require(validID(runID), 400, "invalid_dispatch")
	run := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if run == nil {
		return enqueueWorkflowExecutionWork(t, runID, kind, input, target, graphNode, availableAt)
	}
	require(t.one("SELECT run_id FROM agent_run_residuals WHERE run_id=$1", runID) == nil, 409, "dispatch_conflict")
	if kind == "deliver_revision" {
		if t.one("SELECT run_id FROM revision_delivery_skips WHERE run_id=$1", runID) != nil {
			return Object{"skipped": true}
		}
		require(t.one("SELECT id FROM revisions WHERE run_id=$1", runID) == nil, 409, "delivery_already_settled")
	}
	require(kind == "agent_session" || kind == "deliver_revision", 400, "invalid_dispatch")
	require(input.S("kind") == kind, 400, "invalid_dispatch")
	require(target.S("workspaceId") != "" && target.S("sandboxInstanceId") != "" && target.S("nodeId") != "", 400, "invalid_dispatch")
	if existing := t.one("SELECT * FROM execution_work WHERE run_id=$1 AND execution_id IS NULL", runID); existing != nil {
		if kind == "deliver_revision" {
			input = deliveryWorkInput(t, run, existing.S("id"), input)
		}
		require(existing.S("kind") == kind && jsonText(existing.O("input")) == jsonText(input) && jsonText(existing.O("target")) == jsonText(target), 409, "dispatch_conflict")
		return existing
	}
	id := newID()
	if kind == "deliver_revision" {
		// B owns the skipped/releasing transition; A supplies its same-transaction settlement.
		if t.objectStore == nil {
			if t.execRows("INSERT INTO revision_delivery_skips(run_id) VALUES($1) ON CONFLICT DO NOTHING", runID) == 1 {
				deliverySettled(t, runID, "", Object{"outcome": "skipped", "reason": "object_store_unconfigured"})
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
func enqueueWorkflowExecutionWork(t *transaction, runID, kind string, input, target Object, graphNode string, availableAt time.Time) Object {
	require(t.one("SELECT id FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID) != nil, 404, "not_found")
	require(kind == "agent_session", 400, "invalid_dispatch")
	require(input.S("kind") == kind, 400, "invalid_dispatch")
	require(target.S("workspaceId") != "" && target.S("sandboxInstanceId") != "" && target.S("nodeId") != "", 400, "invalid_dispatch")
	if existing := t.one("SELECT * FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL", runID); existing != nil {
		require(existing.S("kind") == kind && existing.S("workflowNodeId") == graphNode && jsonText(existing.O("input")) == jsonText(input) && jsonText(existing.O("target")) == jsonText(target), 409, "dispatch_conflict")
		return existing
	}
	id := newID()
	// Work released without a graph node (the public seam) stores NULL, not the empty string: the
	// column's CHECK and the scheduler's node-scoped lookups both read "no node" as NULL.
	if availableAt.IsZero() {
		t.exec("INSERT INTO execution_work(id,workflow_run_id,kind,input,target,workflow_node_id) VALUES($1,$2,$3,$4,$5,NULLIF($6,''))", id, runID, kind, jsonText(input), jsonText(target), graphNode)
	} else {
		t.exec("INSERT INTO execution_work(id,workflow_run_id,kind,input,target,workflow_node_id,available_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7)", id, runID, kind, jsonText(input), jsonText(target), graphNode, availableAt)
	}
	t.workSignals = append(t.workSignals, runID)
	return t.one("SELECT * FROM execution_work WHERE id=$1", id)
}

// deliveryWorkInput fixes server-owned locations before an execution identity exists.
// Copying the caller's map keeps retries from accidentally retaining an earlier attempt's keys.
func deliveryWorkInput(t *transaction, run Object, workID string, input Object) Object {
	w := t.one("SELECT * FROM workspaces WHERE issue_run_id=$1 AND deleted_at IS NULL", run.S("id"))
	require(w != nil, 409, "dispatch_conflict")
	require(commitID(input.S("baseCommit")) && input.S("baseCommit") == w.S("baseCommitId"), 409, "dispatch_conflict")
	require(input.S("sessionExecutionId") != "" && input.S("checkoutExecutionId") != "", 400, "invalid_dispatch")
	out := Object{}
	for key, value := range input {
		out[key] = value
	}
	prefix := "revisions/" + run.S("tenantId") + "/" + run.S("id") + "/" + workID + "/"
	out["bundleKey"], out["historyKey"] = prefix+"revision.bundle", prefix+"session.jsonl"
	out["revisionRef"] = "refs/ora/revisions/" + run.S("id")
	return out
}

// claimWorkItem is the pure read behind ClaimWork: the older of a queued tenant clone and an
// unregistered session or delivery. Ownership moves only when the dispatch is recorded.
func claimWorkItem(t *transaction) Object {
	var request Object
	if t.legacyCloneFixture {
		request = t.one("SELECT * FROM clone_requests WHERE state='queued' ORDER BY created_at,id LIMIT 1")
	} else if t.one("SELECT id FROM clone_requests WHERE state='queued'") != nil {
		reject(410, "runtime_scope_required")
	}
	// A work item names the run it belongs to in one of two columns: agent work carries run_id (an
	// issue_runs id), workflow work carries workflow_run_id (a workflow_runs id) and leaves run_id
	// NULL by the mutual-exclusion CHECK 0038 added. The Controller is handed the run id as the
	// operation id and echoes it back on dispatch, which resolves either column — so the claim has to
	// project one value. Reading the agent-only column instead hands back the empty string a workflow
	// row produces, and every dispatch of that item is then refused as a conflict it cannot resolve.
	//
	// The alias is deliberate rather than coalescing over run_id: the row is serialized with
	// row_to_json, which keeps both copies of a repeated column name, and which one the decoder ends
	// up with is not something this query should depend on.
	work := t.one(`SELECT ew.*, coalesce(ew.run_id, ew.workflow_run_id) AS claimed_run_id
		FROM execution_work ew
		WHERE execution_id IS NULL AND available_at<=clock_timestamp()
		  AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.run_id=ew.run_id)
		ORDER BY created_at,id LIMIT 1`)
	if work != nil && (request == nil || work.S("createdAt") < request.S("createdAt")) {
		return Object{"work": work}
	}
	return Object{"request": request}
}
