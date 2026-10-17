package core

import "strings"

// signalOperations tells the lease holder which operations became claimable. It runs only after
// the queuing transaction committed so a Controller that claims immediately finds the row.
func (s *Store) signalOperations(ids []string) {
	if s.Signals == nil {
		return
	}
	for _, id := range ids {
		s.Signals.Publish(ControlSignal{Kind: SignalOperationAvailable, OperationID: id})
	}
}

// signalRuns publishes one hint per run after the transaction that made the work durable committed.
func (s *Store) signalRuns(kind ControlSignalKind, ids []string) {
	if s.Signals == nil {
		return
	}
	for _, id := range ids {
		signal := ControlSignal{Kind: kind, OperationID: id}
		if kind == SignalThreadCommandAvailable {
			signal = ControlSignal{Kind: kind, RunID: id}
		}
		s.Signals.Publish(signal)
	}
}

// submitted wraps one state-changing control action in its submission identity. The identity is
// checked and recorded inside the caller's transaction, so a recorded response and its effects
// commit together: a retry after a lost reply replays the response, never the effect.
func submitted(t *transaction, r *ControlRequest, apply func() Object) Object {
	require(len(r.SubmissionID) <= 200, 400, "invalid_submission")
	if r.SubmissionID == "" {
		return apply()
	}
	hash := requestHash(r.Action, r.Service.Subject, r.Body)
	if old := t.one("SELECT * FROM control_submissions WHERE submission_id=$1", r.SubmissionID); old != nil {
		require(old.S("requestHash") == hash, 409, "submission_conflict")
		return old.O("response")
	}
	out := apply()
	t.exec("INSERT INTO control_submissions(submission_id,holder_id,request_hash,response) VALUES($1,$2,$3,$4)", r.SubmissionID, r.Service.Subject, hash, jsonText(out))
	return out
}

// cloneCommand runs the execution-centric control actions. Lease validity was already checked;
// the epoch is recorded on dispatch so a stale worker's later writes are fenced by their own epoch.
func cloneCommand(t *transaction, r *ControlRequest) Object {
	switch r.Action {
	case "clone_claim":
		// A pure read: ownership moves only when the dispatch is recorded, so a Controller that
		// dies between claim and dispatch leaves nothing to recover.
		return claimWorkItem(t)
	case "clone_dispatch":
		return submitted(t, r, func() Object {
			if kind := r.Body.O("input").S("kind"); kind != "" && kind != "clone" {
				return nodeDispatch(t, r)
			}
			return cloneDispatch(t, r)
		})
	case "clone_takeover":
		return submitted(t, r, func() Object { return executionResult(t, r, true) })
	case "clone_queried":
		return submitted(t, r, func() Object { return executionResult(t, r, false) })
	case "clone_get":
		return lookupExecution(t, r.Body.S("executionId"))
	case "clone_pending":
		return pendingExecutions(t, r.Body.S("nodeId"))
	case "thread_events":
		return submitted(t, r, func() Object { return takeOverThreadEvents(t, r) })
	case "thread_claim":
		return claimThreadCommands(t, r.Body.N("limit"))
	case "thread_delivered":
		return submitted(t, r, func() Object { return recordThreadCommandDelivered(t, r) })
	case "grant_revision_upload":
		return revisionGrants(t, r.Body.S("executionId"), r.Body.O("checksums"))
	default:
		reject(404, "not_found")
	}
	return nil
}

// cloneDispatch registers execution identity, target Node and full input before any Node sees the
// command. Every clone is the clone step of a Workspace operation: Cloud has no tenant-level clone.
func cloneDispatch(t *transaction, r *ControlRequest) Object {
	operation, execution, node := r.Body.S("operationId"), r.Body.S("executionId"), r.Body.S("nodeId")
	input := r.Body.O("input")
	require(validID(operation) && execution != "" && node != "" && len(input) > 0, 400, "invalid_dispatch")
	require(t.one("SELECT execution_id FROM node_executions WHERE execution_id=$1", execution) == nil, 409, "dispatch_conflict")
	return workspaceCloneDispatch(t, r, operation, execution, node, input)
}

// cloneResult commits an execution fact. With a receipt it is the takeover of a Node event and the
// only path that later justifies an Ack; without one it is a queried result. Identical facts are
// idempotent, differing facts or receipts conflict and leave the original untouched.
func cloneResult(t *transaction, r *ControlRequest, withReceipt bool) Object {
	operation, execution := r.Body.S("operationId"), r.Body.S("executionId")
	result := r.Body.O("result")
	e := t.one("SELECT * FROM clone_executions WHERE execution_id=$1 AND operation_id=$2", execution, operation)
	require(e != nil, 404, "not_found")
	outcome := result.S("outcome")
	require((outcome == "clone_ready" || outcome == "clone_failed") && result.O("node").S("nodeId") == e.S("nodeId"), 409, "result_conflict")
	// A checkout of any other commit than the snapshot it was given is a contradiction, not a
	// fact to record: the baseline would be wrong for every later delivery and resume (contract D3).
	require(outcome != "clone_ready" || result.S("commit") == e.O("input").S("baseCommit"), 400, "base_commit_mismatch")
	if previous := e.O("result"); len(previous) > 0 {
		require(jsonText(previous) == jsonText(result), 409, "result_conflict")
	} else {
		t.exec("UPDATE clone_executions SET result=$2,updated_at=now() WHERE execution_id=$1", execution, jsonText(result))
	}
	if withReceipt {
		sequence, event := r.Body.N("sequence"), r.Body.S("event")
		require(sequence >= 0 && event != "", 400, "invalid_receipt")
		if old := t.one("SELECT * FROM clone_event_receipts WHERE execution_id=$1 AND sequence=$2", execution, sequence); old != nil {
			require(old.S("event") == event, 409, "receipt_conflict")
		} else {
			t.exec("INSERT INTO clone_event_receipts(execution_id,sequence,event) VALUES($1,$2,$3)", execution, sequence, event)
		}
	}
	return t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution)
}

// isCloneAction reports whether a control action belongs to the execution registry, including the
// Agent session methods that share its lease and submission rules.
func isCloneAction(action string) bool {
	return strings.HasPrefix(action, "clone_") || strings.HasPrefix(action, "thread_") || action == "grant_revision_upload"
}
