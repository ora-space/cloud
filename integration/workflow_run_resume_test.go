package integration

// Acceptance for the workflow-run resume endpoint (specs decisions/cloud/workflow
// 20261021-node-retry-and-resume-contract.md D7; circus#460).
//
// The final contract resumes a terminal failed run inside the same run identity. This phase ships the
// endpoint's shape and nothing else: authorization identical to cancellation, the final request
// vocabulary validated at the HTTP boundary, and a stable 501 for every valid request. These cases
// drive the shipped route, because the obligations are about what a client observes:
//
//   - every valid request is answered `501 workflow_run_resume_unavailable`, regardless of the run's
//     status;
//   - the refusal writes nothing — no run state, no Workspace or execution rows, and no idempotency
//     record, so a repeat under the same key is the same refusal rather than a replay;
//   - an unknown body field is the router's `400 unknown_field`, and an invalid `mode` is
//     `400 invalid_field_value`;
//   - only the run's initiator and the tenant's administrators may resume a run carrying frozen
//     executor bindings, and a run outside this tenant/workflow, a soft-deleted run and a non-id are
//     all `404 not_found`.

import (
	"context"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

func workflowRunResumePath(tenant, workflow, run string) string {
	return "/api/v1/tenants/" + tenant + "/workflows/" + workflow + "/runs/" + run + "/resume"
}

// resumeWorkflowRun issues the production resume request as the fixture's verified user.
func (f *fixture) resumeWorkflowRun(wfid, runID, key string, body core.Object, want int, code string) core.Object {
	f.t.Helper()
	return f.resumeWorkflowRunAs(f.user, f.tid, wfid, runID, key, body, want, code)
}

// resumeWorkflowRunAs issues the request as a specific caller and asserts the exact status and stable
// public code.
func (f *fixture) resumeWorkflowRunAs(user core.Claims, tenant, wfid, runID, key string, body core.Object, want int, code string) core.Object {
	f.t.Helper()
	out, status, err := f.client.Call(context.Background(), "POST",
		workflowRunResumePath(tenant, wfid, runID), "gateway",
		core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &user, key, body)
	must(f.t, err)
	if status != want {
		f.t.Fatalf("POST workflow run/resume: want %d got %d %v", want, status, out)
	}
	if code != "" && out.S("code") != code {
		f.t.Fatalf("POST workflow run/resume: want code %q got %v", code, out)
	}
	return out
}

// resumeRecordCount reads whether a key left a durable idempotency record. The 501 is a refusal, so
// it must not: the recorded-response path is for committed successes.
func (f *fixture) resumeRecordCount(uid, key string) int {
	f.t.Helper()
	return f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, f.tid, uid, key)
}

// resumeSideEffects counts the rows a resume could plausibly write if it were implemented by accident
// or left a partial write behind.
func (f *fixture) resumeSideEffects(runID string) (workspaces, work, executions, commands int) {
	f.t.Helper()
	return f.scalar(`SELECT count(*) FROM workspaces WHERE workflow_run_id=$1`, runID),
		f.scalar(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1`, runID),
		f.scalar(`SELECT count(*) FROM node_executions WHERE operation_id=$1`, runID),
		f.scalar(`SELECT count(*) FROM thread_commands WHERE workflow_run_id=$1`, runID)
}

// Every valid request — each documented mode and an absent mode (the `keep` default) — is refused
// with the published 501. The run row is byte-for-byte unchanged, no Workspace or execution rows
// appear, and no idempotency record is written: the same key repeated is refused the same way, not
// replayed as a recorded success.
func TestWorkflowRunResumeIsUnavailableForEveryValidRequest(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "resume-valid", "failed", f.uid)
	before := f.workflowRunFacts(runID)

	bodies := []core.Object{
		{"mode": "keep"},
		{"mode": "node_files"},
		{"mode": "checkpoint"},
		{}, // mode defaults to keep
	}
	for i, body := range bodies {
		key := "wf-resume-valid-" + string(rune('a'+i))
		out := f.resumeWorkflowRun(wfid, runID, key, body, 501, "workflow_run_resume_unavailable")
		if out.S("code") != "workflow_run_resume_unavailable" {
			t.Fatalf("body %v: want workflow_run_resume_unavailable, got %v", body, out)
		}
		if after := f.workflowRunFacts(runID); after != before {
			t.Fatalf("body %v changed the run row:\nbefore %s\nafter  %s", body, before, after)
		}
		if n := f.resumeRecordCount(f.uid, key); n != 0 {
			t.Fatalf("body %v left %d idempotency records", body, n)
		}
		if worked, items, executions, commands := f.resumeSideEffects(runID); worked != 0 || items != 0 || executions != 0 || commands != 0 {
			t.Fatalf("body %v wrote side effects: workspaces=%d work=%d executions=%d commands=%d", body, worked, items, executions, commands)
		}
	}

	// A repeat under the first key is the same 501, because there is no recorded response to replay.
	f.resumeWorkflowRun(wfid, runID, "wf-resume-valid-a", core.Object{"mode": "keep"}, 501, "workflow_run_resume_unavailable")
	if after := f.workflowRunFacts(runID); after != before {
		t.Fatalf("a repeated refusal changed the run row:\nbefore %s\nafter  %s", before, after)
	}
	if n := f.resumeRecordCount(f.uid, "wf-resume-valid-a"); n != 0 {
		t.Fatalf("a repeated refusal recorded %d idempotency rows", n)
	}
}

// The HTTP boundary validates the final request vocabulary before availability is decided: a field
// the contract does not define is the router's unknown_field, and a mode outside the closed set is
// invalid_field_value. Neither is answered as unavailable, and neither writes anything.
func TestWorkflowRunResumeRejectsAnUnknownFieldAndAnInvalidMode(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "resume-invalid", "failed", f.uid)
	before := f.workflowRunFacts(runID)

	f.resumeWorkflowRun(wfid, runID, "wf-resume-unknown", core.Object{"mode": "keep", "future": true}, 400, "unknown_field")
	for _, mode := range []any{nil, "everything", 3, false, []any{"keep"}} {
		f.resumeWorkflowRun(wfid, runID, "wf-resume-mode", core.Object{"mode": mode}, 400, "invalid_field_value")
	}
	if after := f.workflowRunFacts(runID); after != before {
		t.Fatalf("a rejected body changed the run row:\nbefore %s\nafter  %s", before, after)
	}
	for _, key := range []string{"wf-resume-unknown", "wf-resume-mode"} {
		if n := f.resumeRecordCount(f.uid, key); n != 0 {
			t.Fatalf("rejected request %q left %d idempotency records", key, n)
		}
	}
	if workspaces, items, executions, commands := f.resumeSideEffects(runID); workspaces != 0 || items != 0 || executions != 0 || commands != 0 {
		t.Fatalf("rejected bodies wrote side effects: workspaces=%d work=%d executions=%d commands=%d", workspaces, items, executions, commands)
	}
}

// Authorization is cancellation's rule: on a run carrying frozen bindings only the initiator and the
// tenant's administrators reach the 501, and an ordinary member is refused before learning whether
// the operation is available. An unbound run keeps the member rule.
func TestWorkflowRunResumeAuthorizesTheInitiatorOrAnAdmin(t *testing.T) {
	f := setup(t)
	stranger := joinUser("wf-resume-stranger")
	callAs(t, f, stranger, "GET", "/api/v1/me", "", nil, 200) // account, no membership
	member, memberID := f.addUser(t, "wf-resume-member", "Member")

	// Bound to the fixture's administrator: the member is neither initiator nor admin.
	boundByAdmin, boundByAdminRun := stageWorkflowRun(t, f, "resume-bound-admin", "failed", f.uid)
	f.bindWorkflowModelToRun(boundByAdminRun, f.uid)
	f.resumeWorkflowRunAs(stranger, f.tid, boundByAdmin, boundByAdminRun, "wf-resume-stranger", core.Object{}, 403, "membership_required")
	f.resumeWorkflowRunAs(member, f.tid, boundByAdmin, boundByAdminRun, "wf-resume-member-refused", core.Object{}, 403, "workflow_run_resume_forbidden")
	f.resumeWorkflowRunAs(f.user, f.tid, boundByAdmin, boundByAdminRun, "wf-resume-admin", core.Object{}, 501, "workflow_run_resume_unavailable")
	// The stranger's record is keyed by the stranger's own user id, which this fixture does not read
	// back; a key-only count in the tenant is enough to prove nothing was written.
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND key=$2`, f.tid, "wf-resume-stranger"); n != 0 {
		t.Fatalf("a refused caller left %d idempotency records", n)
	}
	if n := f.resumeRecordCount(memberID, "wf-resume-member-refused"); n != 0 {
		t.Fatalf("a refused member left %d idempotency records", n)
	}

	// Bound to the member: the initiator reaches the 501.
	boundByMember, boundByMemberRun := stageWorkflowRun(t, f, "resume-bound-member", "failed", memberID)
	f.bindWorkflowModelToRun(boundByMemberRun, memberID)
	f.resumeWorkflowRunAs(member, f.tid, boundByMember, boundByMemberRun, "wf-resume-initiator", core.Object{}, 501, "workflow_run_resume_unavailable")

	// No binding, no run-level rule: an active member reaches the 501 too.
	unbound, unboundRun := stageWorkflowRun(t, f, "resume-unbound", "failed", f.uid)
	f.resumeWorkflowRunAs(member, f.tid, unbound, unboundRun, "wf-resume-unbound", core.Object{}, 501, "workflow_run_resume_unavailable")
}

// Scope is the workflow routes' answer, unchanged: a run that is not this tenant's, not this
// workflow's, not a live row, or not an id is the same not-found a caller gets for a run that does
// not exist.
func TestWorkflowRunResumeIsNotFoundOutsideItsScope(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "resume-scope", "failed", f.uid)
	otherWorkflow, _ := stageWorkflowRun(t, f, "resume-scope-other", "failed", f.uid)
	otherTenant := f.moreTenant()

	for _, tc := range []struct{ name, tenant, workflow, run string }{
		{"cross tenant", otherTenant, wfid, runID},
		{"cross workflow", f.tid, otherWorkflow, runID},
		{"unknown run", f.tid, wfid, newTestID()},
		{"run id is not an id", f.tid, wfid, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.resumeWorkflowRunAs(f.user, tc.tenant, tc.workflow, tc.run, "wf-resume-scope-"+tc.name, core.Object{}, 404, "not_found")
		})
	}

	t.Run("a soft-deleted run is gone", func(t *testing.T) {
		_, gone := stageWorkflowRun(t, f, "resume-scope-deleted", "failed", f.uid)
		_, err := f.store.Pool.Exec(`UPDATE workflow_runs SET deleted_at=now() WHERE id=$1`, gone)
		must(t, err)
		f.resumeWorkflowRun(f.workflowOf(gone), gone, "wf-resume-deleted", core.Object{}, 404, "not_found")
	})
}
