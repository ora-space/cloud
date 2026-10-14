package core

import (
	"context"
	"strings"
)

// maxRecentComments bounds the deterministic context snapshot's recent-comments window.
const maxRecentComments = 10

// buildRunContext assembles the execution context snapshot for an executable interaction. One seam
// serves every mode (§38.10): Task Mode supplies `task`, Form Mode supplies `values`; both share the
// bounded issue context. Reads are bounded — recent comments are limited to maxRecentComments (ordered
// by timeline seq) and the issue's explicit context refs are returned as-is, followed by any
// `extraRefs` applied at confirm time (applied AI suggestions, which are deliberately not persisted as
// issue context — §38.14). When a ContextBuilder is wired it produces the deterministic bundle;
// otherwise the raw material is passed through (Unavailable fallback).
func buildRunContext(t *transaction, i Object, task, targetType, targetID string, values Object, extraRefs []Object) Object {
	if values == nil {
		values = Object{}
	}
	in := ContextInput{
		IssueTitle:        i.S("title"),
		IssueDescription:  i.S("description"),
		Task:              task,
		InteractionValues: values,
		TargetType:        targetType,
		TargetID:          targetID,
	}
	for _, c := range t.list("SELECT body FROM issue_comments WHERE issue_id=$1 AND tenant_id=$2 AND deleted_at IS NULL ORDER BY seq DESC LIMIT $3", i.S("id"), i.S("tenantId"), maxRecentComments) {
		in.RecentComments = append(in.RecentComments, c.S("body"))
	}
	in.ContextRefs = append(t.list("SELECT ref_type, ref_id FROM issue_context_refs WHERE issue_id=$1 AND tenant_id=$2 ORDER BY ref_type, created_at, id", i.S("id"), i.S("tenantId")), extraRefs...)
	if t.contextBuilder == nil {
		return Object{
			"task":              task,
			"interactionValues": values,
			"target":            Object{"type": targetType, "id": targetID},
			"contextRefs":       in.ContextRefs,
		}
	}
	out, err := t.contextBuilder.Build(t.ctx, in)
	if err != nil {
		panic(databaseFailure{err})
	}
	require(out != nil, 500, "internal_error")
	return out
}

// activityActor maps a run's executor type to the Timeline actor that may author its activities. A
// Workflow is not an ActorRef (§8), so a workflow run's activities are authored by `system` and carry
// the workflow identity in `details` instead of impersonating an actor (§38.26).
func activityActor(executorType string) string {
	if executorType == "workflow" {
		return "system"
	}
	return executorType
}

// runActivityDetails is the shared details payload for run-backed activities: the run reference plus
// the executor identity, so a workflow activity is still traceable to the workflow that produced it.
func runActivityDetails(runID, executorType, executorID string, extra Object) Object {
	out := Object{"runId": runID, "executorType": executorType, "executorId": executorID}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// enqueueRun persists a queued IssueRun (shared by the manual run POST, the Task Mode comment path and
// the Workflow confirm path), enforces the pending-executor dedup, snapshots its input, records the
// trigger_evidence provenance seam, and appends the run.enqueued Timeline activity. It never
// dispatches: dispatch is the caller's post-commit responsibility.
func enqueueRun(t *transaction, tid, iid, executorType, executorID string, input Object, triggerKind string, triggerRef any, actorType, actorID string) Object {
	require(t.one("SELECT id FROM issue_runs WHERE issue_id=$1 AND executor_type=$2 AND executor_id=$3 AND status IN ('queued','dispatched') AND deleted_at IS NULL", iid, executorType, executorID) == nil, 409, "pending_run_exists")
	if input == nil {
		input = Object{}
	}
	// This opaque reference is server-owned even for agents that do not use personal models.
	delete(input, "modelBindingId")
	id := newID()
	t.exec("INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,input,status,trigger_evidence_kind,trigger_evidence_ref_id) VALUES($1,$2,$3,$4,$5,$6,'queued',$7,$8)", id, tid, iid, executorType, executorID, jsonText(input), triggerKind, triggerRef)
	if executorType == "agent" {
		// Caller-supplied private bindings cannot authorize a model connection. The production
		// OpenCode path always creates its own owner-scoped binding in this transaction.
		bindRunModel(t, id, actorID, input)
	}
	appendActivity(t, tid, iid, actorType, actorID, "run.enqueued", runActivityDetails(id, executorType, executorID, nil))
	return run(t, tid, iid, id)
}

// dispatchRun is the post-commit outbound handoff for a queued Task Mode run. It loads the run,
// asks the dispatcher to accept it, transitions queued -> dispatched with the external execution id,
// and — for synchronous (mock) dispatchers — drives the observer lifecycle to completion. It is
// best-effort: on any failure the run simply stays queued (Unavailable/blocked degradation). It
// runs entirely in short, separate transactions because the comment transaction has already
// committed and the observer writes (reply comment + activities) must not contend on the comment's
// advisory lock.
func (s *Store) dispatchRun(ctx context.Context, tenantID, runID string) error {
	var (
		req      DispatchRequest
		found    bool
		agentRun bool
	)
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		if o == nil || o.S("status") != "queued" {
			return Object{}
		}
		if o.S("executorType") == "agent" && isSpaceAgentRun(t, tenantID, o.S("executorId")) {
			// A real Space Agent run is claimed by AgentRunDispatcher (Claim + Busy), independent of
			// whether the legacy ExecutionDispatcher is wired (nil in production). This routing is the
			// low-latency after-commit optimization only; the retry loop re-discovers the same runs
			// when it does not fire.
			agentRun = true
			return Object{}
		}
		req = DispatchRequest{
			TenantID:     tenantID,
			IssueID:      o.S("issueId"),
			RunID:        runID,
			ExecutorType: o.S("executorType"),
			ExecutorID:   o.S("executorId"),
			Input:        o.O("input"),
		}
		found = true
		return Object{}
	})
	if err != nil {
		return err
	}
	if agentRun {
		return s.agentRunDispatcher().Dispatch(ctx, runID)
	}
	if !found || s.Dispatcher == nil {
		return nil
	}
	res, err := s.Dispatcher.Dispatch(ctx, req)
	if err != nil || !res.Accepted {
		return err
	}
	if _, err = s.transact(ctx, func(t *transaction) Object {
		t.exec("UPDATE issue_runs SET status='dispatched',external_execution_id=$2,dispatched_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND status='queued'", runID, res.ExternalExecutionID)
		return Object{}
	}); err != nil {
		return err
	}
	if sync, ok := s.Dispatcher.(SyncExecutor); ok {
		sync.Execute(ctx, req, s)
	}
	return nil
}

// ObserveStarted maps the external "started" signal onto the running state (queued/dispatched ->
// running) and appends the run.started activity. ExecutionObserver member.
func (s *Store) ObserveStarted(ctx context.Context, tenantID, runID string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		require(o != nil, 404, "not_found")
		t.exec("UPDATE issue_runs SET status='running',started_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND status IN ('queued','dispatched')", runID)
		appendActivity(t, tenantID, o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.started", runActivityDetails(runID, o.S("executorType"), o.S("executorId"), nil))
		return run(t, tenantID, o.S("issueId"), runID)
	})
}

// ObserveMessage persists an executor reply. An agent/team reply is an IssueComment authored by the
// executor ActorRef (internal core path only — no public impersonation surface). A *workflow* is not an
// Actor (§8), so its human-readable output lands as a `system` IssueActivity carrying the workflow
// provenance in `details` instead — never a comment with author_type='workflow' (§38.26).
// ExecutionObserver member.
func (s *Store) ObserveMessage(ctx context.Context, tenantID, runID, message string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		require(o != nil, 404, "not_found")
		if o.S("executorType") == "workflow" {
			text := strings.TrimSpace(message)
			require(text != "" && len(text) <= 20000, 400, "invalid_input")
			appendActivity(t, tenantID, o.S("issueId"), "system", nil, "run.message", runActivityDetails(runID, "workflow", o.S("executorId"), Object{"message": text}))
			return run(t, tenantID, o.S("issueId"), runID)
		}
		return appendReplyComment(t, tenantID, o.S("issueId"), o.S("executorType"), o.S("executorId"), message)
	})
}

// ObserveProgress records a high-level, human-readable progress signal as a Timeline activity. It
// deliberately does NOT move the run's status (§38.24): progress is narrative, not a lifecycle edge.
// ExecutionObserver member.
func (s *Store) ObserveProgress(ctx context.Context, tenantID, runID, message string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		require(o != nil, 404, "not_found")
		text := strings.TrimSpace(message)
		require(text != "" && len(text) <= 2000, 400, "invalid_input")
		appendActivity(t, tenantID, o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.progress", runActivityDetails(runID, o.S("executorType"), o.S("executorId"), Object{"message": text}))
		return run(t, tenantID, o.S("issueId"), runID)
	})
}

// ObserveCompleted maps completed onto the terminal completed state and appends run.completed.
func (s *Store) ObserveCompleted(ctx context.Context, tenantID, runID string, result Object) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		require(o != nil, 404, "not_found")
		t.exec("UPDATE issue_runs SET status='completed',result=$2,completed_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND status='running'", runID, jsonText(result))
		appendActivity(t, tenantID, o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.completed", runActivityDetails(runID, o.S("executorType"), o.S("executorId"), nil))
		return run(t, tenantID, o.S("issueId"), runID)
	})
}

// ObserveFailed maps a failure onto the terminal failed state (queued/dispatched/running -> failed).
func (s *Store) ObserveFailed(ctx context.Context, tenantID, runID, reason string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		require(o != nil, 404, "not_found")
		t.exec("UPDATE issue_runs SET status='failed',failure_reason=$2,completed_at=now(),version=version+1,updated_at=now() WHERE id=$1 AND status IN ('queued','dispatched','running')", runID, reason)
		appendActivity(t, tenantID, o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.failed", runActivityDetails(runID, o.S("executorType"), o.S("executorId"), Object{"failureReason": reason}))
		return run(t, tenantID, o.S("issueId"), runID)
	})
}

// ObserveCancelled maps a cancellation onto cancelled (queued/dispatched -> cancelled).
func (s *Store) ObserveCancelled(ctx context.Context, tenantID, runID string) (Object, error) {
	return s.transact(ctx, func(t *transaction) Object {
		o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", runID, tenantID)
		require(o != nil, 404, "not_found")
		t.exec("UPDATE issue_runs SET status='cancelled',version=version+1,updated_at=now() WHERE id=$1 AND status IN ('queued','dispatched')", runID)
		appendActivity(t, tenantID, o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.cancelled", runActivityDetails(runID, o.S("executorType"), o.S("executorId"), nil))
		return run(t, tenantID, o.S("issueId"), runID)
	})
}

// appendReplyComment writes an executor-authored IssueComment (author_type=agent|team) through the
// internal core path. No public API can set authorType, so this is not an impersonation surface.
func appendReplyComment(t *transaction, tid, iid, authorType, authorID, body string) Object {
	body = strings.TrimSpace(body)
	require(body != "" && len(body) <= 20000, 400, "invalid_input")
	require(authorType == "agent" || authorType == "team", 400, "invalid_author")
	require(validID(authorID), 400, "invalid_author")
	id := newID()
	seq := nextTimelineSeq(t, iid)
	t.exec("INSERT INTO issue_comments(id,tenant_id,issue_id,author_type,author_id,author_user_id,body,seq) VALUES($1,$2,$3,$4,$5,NULL,$6,$7)", id, tid, iid, authorType, authorID, body, seq)
	return t.one("SELECT * FROM issue_comments WHERE id=$1 AND tenant_id=$2", id, tid)
}
