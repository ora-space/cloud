package integration

// Acceptance for the public cancellation request (IssueRun D6).
//
// The endpoint is the writer D6's first sentence needs — "用户在 Issue 页取消运行 … Cloud 只记录请求" —
// and what it has to hold is deliberately narrow:
//
//   - It records, it does not act. The response is 202 with the instant that was recorded, and the
//     run's phase, status, Thread state and Workspace are all untouched. Ending the Thread, or
//     releasing a run whose session was never declared, belongs to the reaction pass, which
//     internal/core/agent_run_cancel_db_test.go drives from a request written through this same
//     endpoint.
//   - It writes the request once. The instant the first request recorded is the one the run's outcome
//     is decided from, so every repeat — under the same key, under a fresh key, or after the run has
//     moved on — returns that instant and changes nothing.
//   - It refuses a run that has left the agent-execution window, and a caller who may not act on it.
//
// Every case goes through real HTTP and real PostgreSQL, because those are the obligations: the CAS,
// the caller's idempotency record and the run projection the instant has to appear in are all
// database facts, and the two authorization rules the endpoint joins (the shared membership check and
// the personal-model decision's run-level rule) are both decided from stored state.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

func cancelRunPath(tenant, issue, run string) string {
	return "/api/v1/tenants/" + tenant + "/issues/" + issue + "/runs/" + run + "/cancel"
}

// cancelRun issues the production cancellation request with the one legal body, asserting the exact
// status and — for the refusals — the stable public code the client branches on.
func (f *fixture) cancelRun(scene threadScene, key string, want int, code string) core.Object {
	f.t.Helper()
	return f.cancelRunRaw(scene, key, `{}`, want, code)
}

// cancelRunRaw is cancelRun with the body spelled out, so a case can send one the endpoint must
// refuse: `{}` is the whole accepted body, and every other shape is the strict decoder's business.
func (f *fixture) cancelRunRaw(scene threadScene, key, body string, want int, code string) core.Object {
	f.t.Helper()
	return f.cancelRunAsRaw(f.user, scene, key, body, want, code)
}

// cancelRunAs is cancelRun for an explicit caller, which the run-level authorization cases need: the
// endpoint must allow the Binding's initiator and the tenant's administrators and refuse a member who
// is neither, and proving that means issuing the same request as each of them.
func (f *fixture) cancelRunAs(user core.Claims, scene threadScene, key string, want int, code string) core.Object {
	f.t.Helper()
	return f.cancelRunAsRaw(user, scene, key, `{}`, want, code)
}

func (f *fixture) cancelRunAsRaw(user core.Claims, scene threadScene, key, body string, want int, code string) core.Object {
	f.t.Helper()
	status, out, e := f.threadRequestAs(user, "POST", cancelRunPath(scene.tenantID, scene.issueID, scene.runID), body, key)
	must(f.t, e)
	if status != want {
		f.t.Fatalf("POST run/cancel: want %d got %d %v", want, status, out)
	}
	if code != "" && out.S("code") != code {
		f.t.Fatalf("POST run/cancel: want code %q got %v", code, out)
	}
	return out
}

// cancelInstant reads the recorded request exactly as stored, so "no request" stays distinguishable
// from "a request recorded at the zero time".
func (f *fixture) cancelInstant(runID string) (time.Time, bool) {
	f.t.Helper()
	var at *time.Time
	must(f.t, f.store.Pool.QueryRow(`SELECT cancel_requested_at FROM issue_runs WHERE id=$1`, runID).Scan(&at))
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

// cancelRequestedAt is the instant the run's own resource reports, parsed as an instant rather than
// compared as a string: the durable column is a `timestamptz` and the wire carries the API's own
// spelling of it, and neither spelling is the contract.
func (f *fixture) cancelRequestedAt(scene threadScene) string {
	f.t.Helper()
	status, out, e := f.threadRequest("GET", "/api/v1/tenants/"+scene.tenantID+"/issues/"+scene.issueID+"/runs/"+scene.runID, "", "")
	must(f.t, e)
	if status != 200 {
		f.t.Fatalf("GET run: want 200 got %d %v", status, out)
	}
	return out.S("cancelRequestedAt")
}

// assertCancelRecorded proves the caller's key produced a durable idempotency record whose stored
// response is the 202 the caller saw — the thing that makes a replay a replay rather than a second
// decision.
func (f *fixture) assertCancelRecorded(scene threadScene, key string, want int, instant string) {
	f.t.Helper()
	var stored int
	var response string
	must(f.t, f.store.Pool.QueryRow(`SELECT status, response::text FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, scene.tenantID, f.uid, key).Scan(&stored, &response))
	if stored != want {
		f.t.Fatalf("the recorded status must be the %d the caller saw, got %d", want, stored)
	}
	// jsonb's own text form is what comes back, so the recorded body is compared as a decoded value
	// rather than as a byte string: the response is the contract, not PostgreSQL's spacing.
	var recorded core.Object
	must(f.t, json.Unmarshal([]byte(response), &recorded))
	if recorded.S("cancelRequestedAt") != instant || len(recorded) != 1 {
		f.t.Fatalf("the recorded response must be the cancelRequestedAt the caller saw, got %s", response)
	}
}

// bindModelToRun stages one run-level model Binding at the storage boundary, the way the enqueue path
// writes it: the Binding is what makes the personal-model decision's run-level rule apply to this run,
// and the fixture seeds it directly because the connection and credential behind it are created by
// the model API, which is not what these cases are about.
func (f *fixture) bindModelToRun(runID, userID string) {
	f.t.Helper()
	credential, connection, binding := newTestID(), newTestID(), newTestID()
	_, e := f.store.Pool.Exec(`INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id) VALUES($1,$2,repeat('a',64),'cancel-test')`, credential, userID)
	must(f.t, e)
	_, e = f.store.Pool.Exec(`INSERT INTO personal_model_connections(id,user_id,name,protocol,base_url,auth_mode,models,credential_id)
		VALUES($1,$2,'Cancel test','openai-completions','https://models.example.invalid/v1','bearer','[{"id":"vendor/model-v1"}]'::jsonb,$3)`, connection, userID, credential)
	must(f.t, e)
	_, e = f.store.Pool.Exec(`INSERT INTO run_model_bindings(id,run_id,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model)
		VALUES($1,$2,$3,$4,1,$5,'Cancel test','openai-completions','https://models.example.invalid/v1','bearer','{"id":"vendor/model-v1"}'::jsonb)`, binding, runID, userID, connection, credential)
	must(f.t, e)
}

// stageRunState moves a seeded run to the exact (phase, status, Thread state) triple a case is about.
// phase and state are `any` so a case can stage the absent phase of a queued run and the NULL Thread
// state of a run whose session was never declared.
func (f *fixture) stageRunState(runID string, phase any, status string, state any) {
	f.t.Helper()
	_, e := f.store.Pool.Exec(`UPDATE issue_runs SET phase=$2, status=$3, thread_state=$4, idle_since=NULL WHERE id=$1`, runID, phase, status, state)
	must(f.t, e)
}

// The first request. One accepted cancellation records the request, reports the instant it was
// recorded, and does nothing else: the run keeps its phase, status and Thread state, no command is
// released, nothing is appended to the issue's timeline, and no Workspace delete is declared. That is
// the whole of D6's "取消与结束是请求" on this side of the seam.
func TestCancelRunRecordsTheRequest(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	if at, ok := f.cancelInstant(scene.runID); ok {
		t.Fatalf("precondition: the seeded run carries no request, got %v", at)
	}
	versionBefore := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID)
	phaseBefore, statusBefore, stateBefore := f.threadRunState(scene.runID)

	out := f.cancelRun(scene, "cancel-1", 202, "")
	// The response is the recorded instant alone — the endpoint creates no resource to point at — and
	// it has to be the whole body, not a field of a larger one.
	if len(out) != 1 {
		t.Fatalf("the response must carry cancelRequestedAt and nothing else, got %v", out)
	}
	reported, e := time.Parse(time.RFC3339, out.S("cancelRequestedAt"))
	must(t, e)
	durable, ok := f.cancelInstant(scene.runID)
	if !ok || !reported.Equal(durable) {
		t.Fatalf("cancelRequestedAt = %s, want the durable instant %v", reported, durable)
	}

	// The run is untouched. Cancellation is a request: the shutdown, the settlement and the Workspace
	// release all proceed on the session's own evidence (IssueRun D6), which is the reaction pass's
	// business and not this endpoint's.
	if phase, status, state := f.threadRunState(scene.runID); phase != phaseBefore || status != statusBefore || state != stateBefore {
		t.Fatalf("recording a request must not move the run: %s/%s thread=%s became %s/%s thread=%s", phaseBefore, statusBefore, stateBefore, phase, status, state)
	}
	// Exactly one write, so a reader can tell the request apart from a second one.
	if got := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID); got != versionBefore+1 {
		t.Fatalf("recording the request must bump the version exactly once, got %d from %d", got, versionBefore)
	}
	if n := f.scalar(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, scene.runID); n != 0 {
		t.Fatalf("a recorded request releases no command, got %d", n)
	}
	if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, scene.runID); n != 0 {
		t.Fatalf("a recorded request appends no entry, got %d", n)
	}
	if n := f.scalar(`SELECT count(*) FROM issue_activities WHERE issue_id=$1`, scene.issueID); n != 0 {
		t.Fatalf("a recorded request writes no activity; the reaction owns run.cancelled, got %d", n)
	}
	f.assertCancelRecorded(scene, "cancel-1", 202, out.S("cancelRequestedAt"))

	// The body is `{}` and nothing else: an unknown field is refused rather than ignored, exactly as
	// on the Thread routes.
	refused := f.cancelRunRaw(scene, "cancel-unknown-field", `{"reason":"stop"}`, 400, "unknown_field")
	if refused.S("code") != "unknown_field" {
		t.Fatalf("an unknown body field must be refused, got %v", refused)
	}
}

// One key, one write. A replay is answered from the recorded response and must not touch the run
// again; the boundary cases around it are the generic idempotency mechanism's, reused rather than
// reimplemented.
func TestCancelRunIsIdempotentUnderTheSameKey(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	first := f.cancelRun(scene, "cancel-replay", 202, "")
	versionAfterFirst := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID)
	durable, ok := f.cancelInstant(scene.runID)
	if !ok {
		t.Fatal("the first request must record the instant")
	}

	replay := f.cancelRun(scene, "cancel-replay", 202, "")
	if replay.S("cancelRequestedAt") != first.S("cancelRequestedAt") {
		t.Fatalf("a replay must return the recorded instant %s, got %s", first.S("cancelRequestedAt"), replay.S("cancelRequestedAt"))
	}
	if got := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID); got != versionAfterFirst {
		t.Fatalf("a replay must not write again, version moved to %d", got)
	}
	if again, _ := f.cancelInstant(scene.runID); !again.Equal(durable) {
		t.Fatalf("a replay must leave the recorded instant alone, got %v want %v", again, durable)
	}
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key='cancel-replay'`, scene.tenantID, f.uid); n != 1 {
		t.Fatalf("a replay must reuse the one record, got %d", n)
	}
}

// A fresh key is not a fresh cancellation. The instant the first request recorded is the one the
// run's outcome is decided from, so a second request — a second click, or a retry whose key was
// regenerated — is answered with that same instant and changes nothing. This is the endpoint's own
// idempotency, the one that holds even when the caller's key does not.
func TestCancelRunKeepsTheFirstInstantAcrossFreshKeys(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	first := f.cancelRun(scene, "cancel-first", 202, "")
	versionAfterFirst := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID)
	durable, _ := f.cancelInstant(scene.runID)

	second := f.cancelRun(scene, "cancel-second", 202, "")
	if second.S("cancelRequestedAt") != first.S("cancelRequestedAt") {
		t.Fatalf("a second request must return the first instant %s, got %s", first.S("cancelRequestedAt"), second.S("cancelRequestedAt"))
	}
	if got := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID); got != versionAfterFirst {
		t.Fatalf("a second request must not write again, version moved to %d", got)
	}
	if again, _ := f.cancelInstant(scene.runID); !again.Equal(durable) {
		t.Fatalf("a second request must not replace the recorded instant, got %v want %v", again, durable)
	}
	// Each key is still recorded under its own name: the second caller's request was answered, and the
	// record of that answer is what makes a third replay of *that* key a replay.
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key IN ('cancel-first','cancel-second')`, scene.tenantID, f.uid); n != 2 {
		t.Fatalf("each key must keep its own record, got %d", n)
	}
	f.assertCancelRecorded(scene, "cancel-second", 202, first.S("cancelRequestedAt"))
}

// The window is the run's agent-execution window. A queued run is cancellable because the request
// outlives the dispatch it precedes, and `provisioning`, `starting` and `running` are the three live
// phases; everything past them is refused with 409, because the session is over and what remains is
// delivery and release, which D5's give-up limits own rather than the user.
func TestCancelRunOnlyAcceptsTheAgentExecutionWindow(t *testing.T) {
	for _, tc := range []struct {
		name           string
		phase          any
		status, thread string
		want           int
		code           string
	}{
		{name: "queued", phase: nil, status: "queued", thread: "pending", want: 202},
		{name: "provisioning", phase: "provisioning", status: "dispatched", thread: "pending", want: 202},
		{name: "starting", phase: "starting", status: "dispatched", thread: "pending", want: 202},
		{name: "running", phase: "running", status: "running", thread: "active", want: 202},
		{name: "delivering", phase: "delivering", status: "running", thread: "ended", want: 409, code: "run_not_cancellable"},
		{name: "releasing", phase: "releasing", status: "cancelled", thread: "ended", want: 409, code: "run_not_cancellable"},
		{name: "done", phase: "done", status: "completed", thread: "ended", want: 409, code: "run_not_cancellable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedThreadScene(t, f)
			f.stageRunState(scene.runID, tc.phase, tc.status, tc.thread)

			out := f.cancelRun(scene, "cancel-window", tc.want, tc.code)
			_, recorded := f.cancelInstant(scene.runID)
			if tc.want == 202 {
				if !recorded || out.S("cancelRequestedAt") == "" {
					t.Fatalf("%s must accept the request, got %v with %v", tc.name, recorded, out)
				}
				return
			}
			if recorded {
				t.Fatalf("a run past its window must not record a request, got %v", out)
			}
		})
	}
}

// Scope and kind are the Thread routes' answers, unchanged: a run that is not this tenant's, not this
// Issue's, not a live row, or not an agent run is the same not-found a caller gets for a run that does
// not exist, so the endpoint cannot be used to probe which runs exist. Every refusal writes nothing.
func TestCancelRunIsNotFoundOutsideItsScope(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	otherTenant := f.moreTenant()

	for _, tc := range []struct{ name, tenant, issue, run string }{
		{"cross tenant", otherTenant, scene.issueID, scene.runID},
		{"cross issue", scene.tenantID, f.moreIssue(scene), scene.runID},
		{"unknown run", scene.tenantID, scene.issueID, newTestID()},
		{"run id is not an id", scene.tenantID, scene.issueID, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out, e := f.threadRequest("POST", cancelRunPath(tc.tenant, tc.issue, tc.run), `{}`, "cancel-hidden")
			must(t, e)
			if status != 404 || out.S("code") != "not_found" {
				t.Fatalf("%s: want 404 not_found got %d %v", tc.name, status, out)
			}
		})
	}

	t.Run("a soft-deleted run is gone", func(t *testing.T) {
		gone := scene.moreRun(t, f, "pending")
		_, e := f.store.Pool.Exec(`UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, gone.runID)
		must(t, e)
		f.cancelRun(gone, "cancel-deleted", 404, "not_found")
		if _, recorded := f.cancelInstant(gone.runID); recorded {
			t.Fatal("a soft-deleted run must not record a request")
		}
	})

	t.Run("a run that is not an agent run has no agent lifecycle", func(t *testing.T) {
		other := scene.moreRun(t, f, "pending")
		// The agent columns go with the type: issue_runs_agent_columns only permits them on an agent
		// run, which is exactly the invariant this refusal rests on.
		_, e := f.store.Pool.Exec(`UPDATE issue_runs SET executor_type='team', phase=NULL, thread_state=NULL, workspace_id=NULL WHERE id=$1`, other.runID)
		must(t, e)
		f.cancelRun(other, "cancel-team", 404, "not_found")
		if _, recorded := f.cancelInstant(other.runID); recorded {
			t.Fatal("a non-agent run must not record a cancellation request")
		}
	})
}

// Authorization joins two rules and must not confuse them. The shared membership check refuses a
// caller with no membership before the run is read; on a run carrying a model Binding the
// personal-model decision then allows only the Binding's initiator and the tenant's current
// administrators, and a member who is neither is refused with its own code; an unbound run keeps
// Thread D3/D5's rule, so the same member may cancel it.
func TestCancelRunAuthorizesLikeEndingTheThread(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	stranger, _ := f.addUser(t, "cancel-stranger", "Stranger")
	f.cancelRunAs(stranger, scene, "cancel-stranger", 403, "membership_required")

	initiator, initiatorID := f.addUser(t, "cancel-initiator", "Initiator")
	member, memberID := f.addUser(t, "cancel-member", "Member")
	for _, u := range []struct{ id, role string }{{initiatorID, "member"}, {memberID, "member"}} {
		_, e := f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,$3,'active')`, scene.tenantID, u.id, u.role)
		must(t, e)
	}

	// The fixture's verified user is the tenant's administrator, so the binding's initiator is
	// deliberately somebody else: the two allowed branches have to be distinguishable.
	bound := scene.moreRun(t, f, "pending")
	f.bindModelToRun(bound.runID, initiatorID)
	f.cancelRunAs(member, bound, "cancel-member-bound", 403, "model_run_cancel_forbidden")
	if _, recorded := f.cancelInstant(bound.runID); recorded {
		t.Fatal("a refused caller must not record a request")
	}
	f.cancelRunAs(f.user, bound, "cancel-admin-bound", 202, "")

	other := scene.moreRun(t, f, "pending")
	f.bindModelToRun(other.runID, initiatorID)
	f.cancelRunAs(initiator, other, "cancel-initiator-bound", 202, "")

	// No binding, no run-level rule: any active member who can read the Issue may cancel, which is the
	// rule the Thread write path already uses and the one the personal-model decision leaves alone.
	unbound := scene.moreRun(t, f, "pending")
	f.cancelRunAs(member, unbound, "cancel-member-unbound", 202, "")
}

// The recorded request is public state, not a private control column: the run's own resource reports
// it, so the Issue page can show that a cancellation is pending. Before the request the field is
// null; after it, the field is the instant the endpoint returned.
func TestCancelRunIsVisibleOnTheRunResource(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	if got := f.cancelRequestedAt(scene); got != "" {
		t.Fatalf("a run with no request must report none, got %q", got)
	}
	out := f.cancelRun(scene, "cancel-visible", 202, "")
	reported, e := time.Parse(time.RFC3339, f.cancelRequestedAt(scene))
	must(t, e)
	durable, ok := f.cancelInstant(scene.runID)
	if !ok || !reported.Equal(durable) {
		t.Fatalf("the run resource must report the durable instant %v, got %s", durable, reported)
	}
	if out.S("cancelRequestedAt") != f.cancelRequestedAt(scene) {
		t.Fatalf("the resource and the endpoint must agree, got %s and %s", f.cancelRequestedAt(scene), out.S("cancelRequestedAt"))
	}
}
