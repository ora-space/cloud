package core

import (
	"context"
	"time"
)

// EnqueueExecutionWork releases one Agent session or Revision delivery for Controllers to claim.
// Repeating the same unregistered input returns the original item. A different input conflicts.
func (s *Store) EnqueueExecutionWork(ctx context.Context, runID, kind string, input, target Object, availableAt time.Time) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		return enqueueExecutionWork(t, runID, kind, input, target, availableAt)
	})
}

func enqueueExecutionWork(t *transaction, runID, kind string, input, target Object, availableAt time.Time) Object {
	require(validID(runID), 400, "invalid_dispatch")
	run := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	require(run != nil, 404, "not_found")
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
	work := t.one("SELECT * FROM execution_work ew WHERE execution_id IS NULL AND available_at<=clock_timestamp() AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.run_id=ew.run_id) ORDER BY created_at,id LIMIT 1")
	if work == nil {
		return Object{"work": nil}
	}
	return Object{"work": work}
}
