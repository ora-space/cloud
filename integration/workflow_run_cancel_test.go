package integration

// Acceptance for the workflow-side public cancellation request (specs decisions/cloud/workflow
// 0-cloud-side-scheduler-owns-workflow-execution.md D4/D6; circus#75 acceptance items 2 and 6).
//
// The writer already had core-level coverage in internal/core/workflow_run_cancel_db_test.go; what
// was missing was the real HTTP boundary the run page actually calls. These cases drive the shipped
// route and check the obligations the agent-side integration tests check for the other run kind:
//
//   - the request records one instant and nothing else, and the instant it answers with is the
//     durable one;
//   - every repeat — same key, a fresh key, or after the run settled — returns that instant and
//     changes nothing;
//   - only the run's initiator and the tenant's administrators may cancel a run carrying frozen
//     executor bindings;
//   - a run outside this tenant/workflow, a soft-deleted run and a terminal run are refused;
//   - concurrent requests record one instant and one request-caused version bump;
//   - the request written through HTTP is the fact the scheduler's real pass consumes, so the
//     pending run settles cancelled without starting, provisioning or leaving work behind.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func workflowRunCancelPath(tenant, workflow, run string) string {
	return "/api/v1/tenants/" + tenant + "/workflows/" + workflow + "/runs/" + run + "/cancel"
}

// stageWorkflowRun creates a real workflow and publishes its snapshot, then stages a run row at the
// storage boundary. A run created through POST …/runs is settled by the dev simulator before a
// caller could ever see it pending, and the endpoint under test does not create runs — it cancels
// them — so the row is seeded in the state the case is about and every write from then on goes
// through the shipped route.
func stageWorkflowRun(t *testing.T, f *fixture, key, status, owner string) (string, string) {
	t.Helper()
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Cancel target " + key, "graph": runGraph(),
	}, key+"-workflow", 200).O("resource")
	wfid := wf.S("id")
	snap := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, key+"-publish", 200).O("resource")
	runID := uuid.NewString()
	_, err := f.store.Pool.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,node_states,created_by)
		VALUES($1,$2,$3,$4,$5,$6,'{}'::jsonb,'{}'::jsonb,$7)`,
		runID, f.tid, wfid, snap.S("id"), "Staged run "+key, status, owner)
	must(t, err)
	return wfid, runID
}

// cancelWorkflowRun issues the production cancellation request as the fixture's verified user and
// asserts the exact status and — for refusals — the stable public code the client branches on.
func (f *fixture) cancelWorkflowRun(wfid, runID, key string, want int, code string) core.Object {
	f.t.Helper()
	return f.cancelWorkflowRunAs(f.user, f.tid, wfid, runID, key, core.Object{}, want, code)
}

func (f *fixture) cancelWorkflowRunAs(user core.Claims, tenant, wfid, runID, key string, body core.Object, want int, code string) core.Object {
	f.t.Helper()
	out, status, err := f.client.Call(context.Background(), "POST",
		workflowRunCancelPath(tenant, wfid, runID), "gateway",
		core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &user, key, body)
	must(f.t, err)
	if status != want {
		f.t.Fatalf("POST workflow run/cancel: want %d got %d %v", want, status, out)
	}
	if code != "" && out.S("code") != code {
		f.t.Fatalf("POST workflow run/cancel: want code %q got %v", code, out)
	}
	return out
}

// workflowCancelInstant reads the recorded request exactly as stored, so "no request" stays
// distinguishable from "a request recorded at the zero time".
func (f *fixture) workflowCancelInstant(runID string) (time.Time, bool) {
	f.t.Helper()
	var at *time.Time
	must(f.t, f.store.Pool.QueryRow(`SELECT cancel_requested_at FROM workflow_runs WHERE id=$1`, runID).Scan(&at))
	if at == nil {
		return time.Time{}, false
	}
	return *at, true
}

// workflowRunFacts is every column of the run except the ones a request is allowed to move. A
// request that writes anything else — status, timestamps, node states — changes this string.
func (f *fixture) workflowRunFacts(runID string) string {
	f.t.Helper()
	var facts string
	must(f.t, f.store.Pool.QueryRow(`SELECT (to_jsonb(r) - ARRAY['cancel_requested_at','version','updated_at'])::text
		FROM workflow_runs r WHERE id=$1`, runID).Scan(&facts))
	return facts
}

// assertWorkflowCancelRecorded proves the caller's key produced a durable idempotency record whose
// stored response is the 202 the caller saw — the thing that makes a replay a replay rather than a
// second decision.
func (f *fixture) assertWorkflowCancelRecorded(uid, key, instant string) {
	f.t.Helper()
	var stored int
	var response string
	must(f.t, f.store.Pool.QueryRow(`SELECT status, response::text FROM idempotency_records
		WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, f.tid, uid, key).Scan(&stored, &response))
	if stored != 202 {
		f.t.Fatalf("the recorded status must be the 202 the caller saw, got %d", stored)
	}
	var recorded core.Object
	must(f.t, json.Unmarshal([]byte(response), &recorded))
	if recorded.S("cancelRequestedAt") != instant || len(recorded) != 1 {
		f.t.Fatalf("the recorded response must be the cancelRequestedAt the caller saw, got %s", response)
	}
}

// bindWorkflowModelToRun stages one run-level executor binding at the storage boundary, the way the
// creation transaction writes it: the binding is what makes the initiator-or-admin rule apply to
// this run, and the fixture seeds it directly because the connection and credential behind it are
// created by the model API, which is not what these cases are about.
func (f *fixture) bindWorkflowModelToRun(runID, userID string) {
	f.t.Helper()
	credential, connection, binding := newTestID(), newTestID(), newTestID()
	_, err := f.store.Pool.Exec(`INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id)
		VALUES($1,$2,repeat('a',64),'cancel-test')`, credential, userID)
	must(f.t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO personal_model_connections(id,user_id,credential_id,name,protocol,base_url,auth_mode,models,enabled)
		VALUES($1,$2,$3,'Cancel test','openai-completions','https://models.example.invalid/v1','bearer','[{"id":"vendor/model-v1"}]'::jsonb,true)`,
		connection, userID, credential)
	must(f.t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO run_model_bindings(id,workflow_run_id,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model,slot_name)
		VALUES($1,$2,$3,$4,1,$5,'Cancel test','openai-completions','https://models.example.invalid/v1','bearer','{"id":"vendor/model-v1"}'::jsonb,'default')`,
		binding, runID, userID, connection, credential)
	must(f.t, err)
}

// The first request. One accepted cancellation records the request, reports the instant it was
// recorded, and does nothing else: the run keeps its status, timestamps and node states, no
// Workspace or execution work exists, and no command or operation is derived. Replays — the same
// key and a fresh one — return that instant and do not write again.
func TestWorkflowRunCancelRecordsOnlyTheRequest(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "records", "pending", f.uid)

	if at, ok := f.workflowCancelInstant(runID); ok {
		t.Fatalf("precondition: the staged run carries no request, got %v", at)
	}
	factsBefore := f.workflowRunFacts(runID)
	versionBefore := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID)

	out := f.cancelWorkflowRun(wfid, runID, "wf-cancel-1", 202, "")
	// The response is the recorded instant alone, and it has to be the whole body.
	if len(out) != 1 {
		t.Fatalf("the response must carry cancelRequestedAt and nothing else, got %v", out)
	}
	reported, err := time.Parse(time.RFC3339, out.S("cancelRequestedAt"))
	must(t, err)
	durable, ok := f.workflowCancelInstant(runID)
	if !ok || !reported.Equal(durable) {
		t.Fatalf("cancelRequestedAt = %s, want the durable instant %v", reported, durable)
	}

	// The run is untouched apart from the request and its version bump.
	if after := f.workflowRunFacts(runID); after != factsBefore {
		t.Fatalf("recording a request moved the run:\nbefore %s\nafter  %s", factsBefore, after)
	}
	if got := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID); got != versionBefore+1 {
		t.Fatalf("recording the request must bump the version exactly once, got %d from %d", got, versionBefore+1)
	}
	for _, q := range []struct {
		what  string
		query string
	}{
		{"workspaces", `SELECT count(*) FROM workspaces WHERE workflow_run_id=$1`},
		{"execution work", `SELECT count(*) FROM execution_work WHERE workflow_run_id=$1`},
		{"thread commands", `SELECT count(*) FROM thread_commands WHERE workflow_run_id=$1`},
		{"operations", `SELECT count(*) FROM operations o JOIN workspaces w ON w.id=o.workspace_id WHERE w.workflow_run_id=$1`},
	} {
		if n := f.scalar(q.query, runID); n != 0 {
			t.Fatalf("a recorded request produced %d %s", n, q.what)
		}
	}
	f.assertWorkflowCancelRecorded(f.uid, "wf-cancel-1", out.S("cancelRequestedAt"))

	// The run resource reports the request, which is what lets the page show a pending cancellation.
	if got := f.call("GET", f.path("/workflows/"+wfid+"/runs/"+runID), nil, "", 200).S("cancelRequestedAt"); got != out.S("cancelRequestedAt") {
		t.Fatalf("the run resource reports cancelRequestedAt=%q; want %q", got, out.S("cancelRequestedAt"))
	}

	// Replays return the recorded instant instead of writing a second one.
	same := f.cancelWorkflowRun(wfid, runID, "wf-cancel-1", 202, "")
	fresh := f.cancelWorkflowRun(wfid, runID, "wf-cancel-2", 202, "")
	for i, again := range []core.Object{same, fresh} {
		if again.S("cancelRequestedAt") != out.S("cancelRequestedAt") {
			t.Fatalf("replay %d returned %q; want the recorded instant %q", i, again.S("cancelRequestedAt"), out.S("cancelRequestedAt"))
		}
	}
	if got := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID); got != versionBefore+1 {
		t.Fatalf("a replay wrote again: version %d, want %d", got, versionBefore+1)
	}
	f.assertWorkflowCancelRecorded(f.uid, "wf-cancel-2", out.S("cancelRequestedAt"))

	// The body is `{}` and nothing else: an unknown field is refused rather than ignored.
	if refused := f.cancelWorkflowRunAs(f.user, f.tid, wfid, runID, "wf-cancel-unknown", core.Object{"reason": "stop"}, 400, "unknown_field"); refused.S("code") != "unknown_field" {
		t.Fatalf("an unknown body field must be refused, got %v", refused)
	}
}

// Cancel-before-start is the whole cancellation: the request written through HTTP is the fact the
// scheduler's pass reads, the run settles cancelled without ever claiming, no Workspace or derived
// operation appears, and later passes do not touch the terminal row.
func TestWorkflowRunCancelSettlesThroughTheScheduler(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "scheduler", "pending", f.uid)
	instant := f.cancelWorkflowRun(wfid, runID, "wf-cancel-pass", 202, "").S("cancelRequestedAt")

	must(t, f.store.AdvanceWorkflowRunsOnce(context.Background()))
	run := f.call("GET", f.path("/workflows/"+wfid+"/runs/"+runID), nil, "", 200)
	if run.S("status") != "cancelled" {
		t.Fatalf("run status = %q; want cancelled", run.S("status"))
	}
	if run.S("startedAt") != "" {
		t.Fatalf("a run cancelled before its claim stamped startedAt %q", run.S("startedAt"))
	}
	if run.S("finishedAt") == "" {
		t.Fatal("a cancelled run carries no finishedAt")
	}
	if run.S("cancelRequestedAt") != instant {
		t.Fatalf("the settlement cleared the recorded request: %q; want %q", run.S("cancelRequestedAt"), instant)
	}
	if states := run.O("nodeStates"); len(states) != 0 {
		t.Fatalf("a cancelled-before-start run has node states %v; want none", states)
	}
	if n := f.scalar(`SELECT count(*) FROM workspaces WHERE workflow_run_id=$1`, runID); n != 0 {
		t.Fatalf("a cancelled-before-start run left %d workspaces", n)
	}
	if n := f.scalar(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1`, runID); n != 0 {
		t.Fatalf("a cancelled-before-start run left %d execution work items", n)
	}
	if n := f.scalar(`SELECT count(*) FROM operations o JOIN workspaces w ON w.id=o.workspace_id WHERE w.workflow_run_id=$1 AND o.kind='delete_workspace'`, runID); n != 0 {
		t.Fatalf("a cancelled-before-start run derived %d Workspace deletions", n)
	}
	// S5 records the release fact in the transaction that settles the run. This run never
	// provisioned a Workspace, so the honest state is not_required — and no release instant is
	// invented for a deletion Cloud never observed.
	var releaseState, releaseErr string
	var releasedAt *time.Time
	must(t, f.store.Pool.QueryRow(`SELECT workspace_release_state, workspace_release_error, workspace_released_at
		FROM workflow_runs WHERE id=$1`, runID).Scan(&releaseState, &releaseErr, &releasedAt))
	if releaseState != "not_required" || releaseErr != "" || releasedAt != nil {
		t.Fatalf("cancelled-before-start release = state %q reason %q released_at %v; want not_required with no release instant",
			releaseState, releaseErr, releasedAt)
	}

	// The request outlives the settlement: a repeat returns the same instant rather than a conflict.
	if again := f.cancelWorkflowRun(wfid, runID, "wf-cancel-after-settlement", 202, ""); again.S("cancelRequestedAt") != instant {
		t.Fatalf("a repeat after settlement returned %q; want %q", again.S("cancelRequestedAt"), instant)
	}
	version := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID)
	must(t, f.store.AdvanceWorkflowRunsOnce(context.Background()))
	if again := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID); again != version {
		t.Fatalf("a later pass bumped the terminal run's version from %d to %d", version, again)
	}
}

// A run carrying frozen executor bindings is cancellable by its initiator and by the tenant's
// administrators only; an ordinary member who is neither is refused before learning whether the run
// was cancellable. A run with no binding keeps the member rule.
func TestWorkflowRunCancelAuthorizesTheInitiatorOrAnAdmin(t *testing.T) {
	f := setup(t)
	stranger := joinUser("wf-cancel-stranger")
	callAs(t, f, stranger, "GET", "/api/v1/me", "", nil, 200) // account, no membership
	member, memberID := f.addUser(t, "wf-cancel-member", "Member")

	// Bound to the fixture's administrator: the member is neither initiator nor admin.
	boundByAdmin, boundByAdminRun := stageWorkflowRun(t, f, "bound-admin", "pending", f.uid)
	f.bindWorkflowModelToRun(boundByAdminRun, f.uid)
	f.cancelWorkflowRunAs(stranger, f.tid, boundByAdmin, boundByAdminRun, "wf-cancel-stranger", core.Object{}, 403, "membership_required")
	f.cancelWorkflowRunAs(member, f.tid, boundByAdmin, boundByAdminRun, "wf-cancel-member-refused", core.Object{}, 403, "workflow_run_cancel_forbidden")
	if _, recorded := f.workflowCancelInstant(boundByAdminRun); recorded {
		t.Fatal("a refused caller must not record a request")
	}
	f.cancelWorkflowRunAs(f.user, f.tid, boundByAdmin, boundByAdminRun, "wf-cancel-admin", core.Object{}, 202, "")

	// Bound to the member: the initiator may cancel.
	boundByMember, boundByMemberRun := stageWorkflowRun(t, f, "bound-member", "pending", memberID)
	f.bindWorkflowModelToRun(boundByMemberRun, memberID)
	f.cancelWorkflowRunAs(member, f.tid, boundByMember, boundByMemberRun, "wf-cancel-initiator", core.Object{}, 202, "")

	// No binding, no run-level rule: an active member may cancel.
	unbound, unboundRun := stageWorkflowRun(t, f, "unbound", "pending", f.uid)
	f.cancelWorkflowRunAs(member, f.tid, unbound, unboundRun, "wf-cancel-unbound", core.Object{}, 202, "")
}

// Scope is the workflow routes' answer, unchanged: a run that is not this tenant's, not this
// workflow's, not a live row, or not an id is the same not-found a caller gets for a run that does
// not exist, and every refusal writes nothing.
func TestWorkflowRunCancelIsNotFoundOutsideItsScope(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "scope", "pending", f.uid)
	otherWorkflow, _ := stageWorkflowRun(t, f, "scope-other", "pending", f.uid)
	otherTenant := f.moreTenant()

	for _, tc := range []struct{ name, tenant, workflow, run string }{
		{"cross tenant", otherTenant, wfid, runID},
		{"cross workflow", f.tid, otherWorkflow, runID},
		{"unknown run", f.tid, wfid, newTestID()},
		{"run id is not an id", f.tid, wfid, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := f.cancelWorkflowRunAs(f.user, tc.tenant, tc.workflow, tc.run, "wf-cancel-scope-"+tc.name, core.Object{}, 404, "not_found")
			if out.S("code") != "not_found" {
				t.Fatalf("%s: want not_found, got %v", tc.name, out)
			}
		})
	}

	t.Run("a soft-deleted run is gone", func(t *testing.T) {
		_, gone := stageWorkflowRun(t, f, "scope-deleted", "pending", f.uid)
		_, err := f.store.Pool.Exec(`UPDATE workflow_runs SET deleted_at=now() WHERE id=$1`, gone)
		must(t, err)
		f.cancelWorkflowRun(f.workflowOf(gone), gone, "wf-cancel-deleted", 404, "not_found")
		if _, recorded := f.workflowCancelInstant(gone); recorded {
			t.Fatal("a soft-deleted run must not record a request")
		}
	})
}

// workflowOf reads the workflow a staged run belongs to, which the route needs to address it.
func (f *fixture) workflowOf(runID string) string {
	f.t.Helper()
	var wid string
	must(f.t, f.store.Pool.QueryRow(`SELECT workflow_id FROM workflow_runs WHERE id=$1`, runID).Scan(&wid))
	return wid
}

// A run that already settled, and the never-written awaiting_input state, cannot be cancelled: there
// is no live execution to stop, so the request is refused rather than recorded as if it had decided
// anything.
func TestWorkflowRunCancelAfterSettlementIsRefused(t *testing.T) {
	f := setup(t)
	for _, status := range []string{"succeeded", "failed", "cancelled", "awaiting_input"} {
		t.Run(status, func(t *testing.T) {
			wfid, runID := stageWorkflowRun(t, f, "terminal-"+status, status, f.uid)
			out := f.cancelWorkflowRun(wfid, runID, "wf-cancel-terminal-"+status, 409, "run_not_cancellable")
			if out.S("code") != "run_not_cancellable" {
				t.Fatalf("a %s run's request: want run_not_cancellable, got %v", status, out)
			}
			if _, recorded := f.workflowCancelInstant(runID); recorded {
				t.Fatal("a refused request must not record an instant")
			}
		})
	}
}

// Two callers racing to cancel the same run still record one request: the transactions serialize,
// whichever runs second reads the instant the first recorded, and both answers carry it. The row
// keeps one instant and one request-caused version bump.
func TestWorkflowRunCancelRacesToOneRequest(t *testing.T) {
	f := setup(t)
	wfid, runID := stageWorkflowRun(t, f, "race", "pending", f.uid)
	versionBefore := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID)

	type answer struct {
		instant string
		status  int
		err     error
	}
	answers := make([]answer, 2)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i, key := range []string{"wf-cancel-race-a", "wf-cancel-race-b"} {
		group.Add(1)
		go func(i int, key string) {
			defer group.Done()
			<-start
			out, status, err := f.client.Call(context.Background(), "POST",
				workflowRunCancelPath(f.tid, wfid, runID), "gateway",
				core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, key, core.Object{})
			answers[i] = answer{out.S("cancelRequestedAt"), status, err}
		}(i, key)
	}
	close(start)
	group.Wait()

	for i, got := range answers {
		if got.err != nil || got.status != 202 || got.instant == "" {
			t.Fatalf("request %d: status=%d instant=%q err=%v; want 202 carrying the recorded instant", i, got.status, got.instant, got.err)
		}
		if got.instant != answers[0].instant {
			t.Fatalf("request %d recorded %q; request 0 recorded %q", i, got.instant, answers[0].instant)
		}
	}
	durable, ok := f.workflowCancelInstant(runID)
	reported, err := time.Parse(time.RFC3339, answers[0].instant)
	must(t, err)
	if !ok || !reported.Equal(durable) {
		t.Fatalf("the row carries %v; want the reported instant %s", durable, answers[0].instant)
	}
	if got := f.scalar(`SELECT version FROM workflow_runs WHERE id=$1`, runID); got != versionBefore+1 {
		t.Fatalf("a race wrote the version %d times, got %d from %d", got-versionBefore, got, versionBefore)
	}
}
