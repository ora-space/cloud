package core

import (
	"context"
	"net/url"
	"strings"
	"unicode"
)

// EnqueueClone accepts a clone request in Cloud's own transaction and hands it to Controllers as
// queued work; it is the programmatic twin of the public clones API used by tests and tooling.
func (s *Store) EnqueueClone(ctx context.Context, tenantID, userID, requestID, repositoryURL, branch string) (Object, error) {
	created := false
	out, err := s.transact(ctx, func(t *transaction) Object {
		var row Object
		row, created = enqueueClone(t, tenantID, userID, requestID, repositoryURL, branch)
		return row
	})
	if err == nil && created {
		s.signalWork(out.S("id"))
	}
	return out, err
}

// signalWork tells the lease holder that a claim is worth trying. It runs only after the accepting
// transaction committed so a Controller that claims immediately finds the row.
func (s *Store) signalWork(operationID string) {
	if s.Signals != nil {
		s.Signals.Publish(ControlSignal{Kind: SignalWorkAvailable, OperationID: operationID})
	}
}

// signalOperations tells the lease holder which operations became claimable. Like signalWork it
// runs only after the queuing transaction committed.
func (s *Store) signalOperations(ids []string) {
	if s.Signals == nil {
		return
	}
	for _, id := range ids {
		s.Signals.Publish(ControlSignal{Kind: SignalOperationAvailable, OperationID: id})
	}
}

// enqueueClone records one clone request inside the caller's transaction. Repeating
// (tenant, user, requestId) with the same input returns the original request and reports nothing
// new; different input is a conflict. Nothing is dispatched here: a Controller claims the row.
func enqueueClone(t *transaction, tenantID, userID, requestID, repositoryURL, branch string) (row Object, created bool) {
	require(requestID != "" && len(requestID) <= 200, 400, "invalid_clone_request")
	validCloneSource(repositoryURL, branch)
	membership(t, tenantID, userID, false)
	if existing := t.one("SELECT * FROM clone_requests WHERE tenant_id=$1 AND actor_user_id=$2 AND request_id=$3", tenantID, userID, requestID); existing != nil {
		require(existing.S("repositoryUrl") == repositoryURL && existing.S("branch") == branch, 409, "idempotency_conflict")
		return existing, false
	}
	id := newID()
	t.exec("INSERT INTO clone_requests(id,tenant_id,actor_user_id,request_id,repository_url,branch,state) VALUES($1,$2,$3,$4,$5,$6,'queued')", id, tenantID, userID, requestID, repositoryURL, branch)
	return t.one("SELECT * FROM clone_requests WHERE id=$1", id), true
}

// validCloneSource applies the Controller's own clone input policy at acceptance time. Cloud must
// not accept what no Controller can dispatch: such a request would sit queued forever with no
// terminal fact, and the caller would read that as "awaiting reconciliation" rather than failure.
func validCloneSource(repository, branch string) {
	require(repository != "" && len(repository) <= 2000 && !strings.ContainsAny(repository, "\\") && !hasSpaceOrControl(repository), 400, "invalid_repository_url")
	scheme, rest, ok := strings.Cut(repository, "://")
	require(ok && (scheme == "https" || scheme == "ssh") && !strings.HasPrefix(rest, "/"), 400, "invalid_repository_url")
	parsed, e := url.Parse(repository)
	require(e == nil && parsed.Host != "" && parsed.Path != "" && parsed.Path != "/" && parsed.RawQuery == "" && !parsed.ForceQuery && parsed.Fragment == "", 400, "invalid_repository_url")
	authority, _, _ := strings.Cut(rest, "/")
	// Git receives the text verbatim: https never carries userinfo, ssh carries a user but no password.
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		require(scheme == "ssh" && i > 0 && !strings.Contains(authority[:i], ":"), 400, "invalid_repository_url")
	}
	require(branch != "HEAD", 400, "invalid_ref")
	// validRef judges the trimmed text, but the branch is stored and handed to Git verbatim. A
	// surrounding space or an inner tab or control character would pass here and be refused by the
	// Controller; ClaimWork keeps returning the oldest queued request, so that one refusal would
	// stall every clone queued behind it.
	require(!hasSpaceOrControl(branch), 400, "invalid_ref")
	validRef(branch)
	require(!strings.HasPrefix(branch, "refs/") && !strings.HasPrefix(branch, "/") && !strings.HasSuffix(branch, "/") && !strings.Contains(branch, "//") && !strings.HasSuffix(branch, ".") && branch != "@", 400, "invalid_ref")
	for component := range strings.SplitSeq(branch, "/") {
		require(!strings.HasPrefix(component, ".") && !strings.HasSuffix(component, ".lock"), 400, "invalid_ref")
	}
}

// hasSpaceOrControl reports any Unicode space or control character, which Git never receives from
// a clone source: the text is passed verbatim, so nothing is trimmed on its behalf.
func hasSpaceOrControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
}

// cloneQuery joins each request with its at-most-one execution; the LEFT JOIN keeps queued
// requests visible before a Controller registers a dispatch.
const cloneQuery = "SELECT r.*, e.execution_id, e.node_id, e.result FROM clone_requests r LEFT JOIN clone_executions e ON e.operation_id=r.id WHERE r.tenant_id=$1 AND r.actor_user_id=$2"

// clonesPublic serves the public clones routes for the verified actor: only the user who submitted
// a request can read it, mirroring operation visibility. accepted names a newly queued request the
// caller must signal once the transaction committed.
func clonesPublic(t *transaction, r *PublicRequest, uid string) (out Object, accepted string) {
	switch {
	case r.CloneID != "":
		require(validID(r.CloneID), 404, "not_found")
		row := t.one(cloneQuery+" AND r.id=$3", r.TenantID, uid, r.CloneID)
		require(row != nil, 404, "not_found")
		return cloneView(row), ""
	case r.Method == "GET":
		listing := page(t, cloneQuery, []any{r.TenantID, uid}, "r.id", r)
		rows, _ := listing["items"].([]Object)
		items := make([]Object, 0, len(rows))
		for _, row := range rows {
			items = append(items, cloneView(row))
		}
		listing["items"] = items
		return listing, ""
	default:
		row, created := enqueueClone(t, r.TenantID, uid, r.Body.S("requestId"), r.Body.S("repository"), r.Body.S("branch"))
		if created {
			accepted = row.S("id")
		}
		return cloneView(t.one(cloneQuery+" AND r.id=$3", r.TenantID, uid, row.S("id"))), accepted
	}
}

// cloneView is the public shape of one clone request: the transitional Controller DTO field for
// field, so a browser observes one clone semantics whichever authority accepted the request.
// executionId and nodeId are null until a Controller records the dispatch.
func cloneView(row Object) Object {
	state := Object{"kind": "pending"}
	result := row.O("result")
	switch row.S("state") {
	case "succeeded":
		state = Object{"kind": "succeeded", "path": result.S("path"), "commit": result.S("commit")}
	case "failed":
		state = Object{"kind": "failed", "reason": cloneFailureReason(result.S("reason"))}
		if retained, ok := result["retainedPath"].(string); ok {
			state["retainedPath"] = retained
		}
	}
	return Object{"operationId": row.S("id"), "requestId": row.S("requestId"), "repository": row.S("repositoryUrl"), "branch": row.S("branch"), "executionId": row["executionId"], "nodeId": row["nodeId"], "state": state, "createdAt": row["createdAt"], "updatedAt": row["updatedAt"]}
}

// cloneFailureReason turns the stored contract enum name (CLONE_FAILURE_REASON_BRANCH_NOT_FOUND)
// into the public camelCase value (branchNotFound) the browser DTO uses.
func cloneFailureReason(stored string) string {
	return camel(strings.ToLower(strings.TrimPrefix(stored, "CLONE_FAILURE_REASON_")))
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
		work := t.one("SELECT * FROM clone_requests WHERE state='queued' ORDER BY created_at,id LIMIT 1")
		return Object{"request": work}
	case "clone_dispatch":
		return submitted(t, r, func() Object { return cloneDispatch(t, r) })
	case "clone_takeover":
		return submitted(t, r, func() Object { return cloneResult(t, r, true) })
	case "clone_queried":
		return submitted(t, r, func() Object { return cloneResult(t, r, false) })
	case "clone_get":
		e := t.one("SELECT * FROM clone_executions WHERE execution_id=$1", r.Body.S("executionId"))
		require(e != nil, 404, "not_found")
		return e
	case "clone_pending":
		node := r.Body.S("nodeId")
		if node == "" {
			return Object{"executions": t.list("SELECT * FROM clone_executions WHERE result IS NULL ORDER BY created_at,execution_id")}
		}
		return Object{"executions": t.list("SELECT * FROM clone_executions WHERE result IS NULL AND node_id=$1 ORDER BY created_at,execution_id", node)}
	default:
		reject(404, "not_found")
	}
	return nil
}

// cloneDispatch registers execution identity, target Node and full input before any Node sees the
// command. The request must still be queued (or already dispatched as exactly this execution). An
// operation ID that names no tenant clone request is a Workspace operation's clone step.
func cloneDispatch(t *transaction, r *ControlRequest) Object {
	operation, execution, node := r.Body.S("operationId"), r.Body.S("executionId"), r.Body.S("nodeId")
	input := r.Body.O("input")
	require(validID(operation) && execution != "" && node != "" && len(input) > 0, 400, "invalid_dispatch")
	request := t.one("SELECT * FROM clone_requests WHERE id=$1", operation)
	if request == nil {
		return workspaceCloneDispatch(t, r, operation, execution, node, input)
	}
	require(input.S("repositoryUrl") == request.S("repositoryUrl") && input.S("branch") == request.S("branch"), 409, "dispatch_conflict")
	if existing := t.one("SELECT * FROM clone_executions WHERE operation_id=$1", operation); existing != nil {
		require(existing.S("executionId") == execution && existing.S("nodeId") == node && jsonText(existing.O("input")) == jsonText(input), 409, "dispatch_conflict")
		return existing
	}
	require(request.S("state") == "queued", 409, "dispatch_conflict")
	require(t.one("SELECT execution_id FROM clone_executions WHERE execution_id=$1", execution) == nil, 409, "dispatch_conflict")
	t.exec("INSERT INTO clone_executions(execution_id,operation_id,node_id,input,dispatched_epoch) VALUES($1,$2,$3,$4,$5)", execution, operation, node, jsonText(input), r.Body.N("epoch"))
	t.exec("UPDATE clone_requests SET state='dispatched',updated_at=now() WHERE id=$1", operation)
	return t.one("SELECT * FROM clone_executions WHERE execution_id=$1", execution)
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
	if previous := e.O("result"); len(previous) > 0 {
		require(jsonText(previous) == jsonText(result), 409, "result_conflict")
	} else {
		state := "succeeded"
		if outcome == "clone_failed" {
			state = "failed"
		}
		t.exec("UPDATE clone_executions SET result=$2,updated_at=now() WHERE execution_id=$1", execution, jsonText(result))
		t.exec("UPDATE clone_requests SET state=$2,updated_at=now() WHERE id=$1", operation, state)
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

// isCloneAction reports whether a control action belongs to the execution registry.
func isCloneAction(action string) bool { return strings.HasPrefix(action, "clone_") }
