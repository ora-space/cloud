package core

import (
	"context"
	"time"
)

// executionWorkLiveRuns excludes business-closed Runs without discarding their recovery evidence.
// The execution API also serves unphased callers; only deletion or the lifecycle's release/done
// stages revoke their ability to register new work. Claim and dispatch must use the same predicate.
const executionWorkLiveRuns = "SELECT id FROM issue_runs WHERE deleted_at IS NULL AND (phase IS NULL OR phase NOT IN ('releasing','done'))"

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
	work := t.one("SELECT * FROM execution_work ew WHERE execution_id IS NULL AND available_at<=clock_timestamp() AND ew.run_id IN (" + executionWorkLiveRuns + ") AND NOT EXISTS(SELECT 1 FROM agent_run_residuals ar WHERE ar.run_id=ew.run_id) ORDER BY created_at,id LIMIT 1")
	if work != nil && (request == nil || work.S("createdAt") < request.S("createdAt")) {
		return Object{"work": work}
	}
	return Object{"request": request}
}
