package integration

// The clone step consumes the Binding's current snapshot (specs
// decisions/cloud/operation/20261016-clone-step-consumes-binding-snapshot.md). Every case drives the
// production claim, advance and dispatch paths through the simulator Controller or the JSON and gRPC
// control surfaces, against real PostgreSQL and the Git Server double's repositories on disk; the
// git-server-preset fixtures stand in for the external line.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitpreset"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// preset is the git-server-preset tooling over the fixture's database and Git Server double.
func (f *fixture) preset() *gitpreset.Preset {
	return &gitpreset.Preset{DB: f.store.Pool, Root: f.git.Root, Git: "git"}
}

// operationRow reads the columns the clone-step decisions are stated in.
func (f *fixture) operationRow(id string) core.Object {
	f.t.Helper()
	var state, step, code, result string
	must(f.t, f.store.Pool.QueryRow(`SELECT state,step,COALESCE(error_code,''),result::text FROM operations WHERE id=$1`, id).Scan(&state, &step, &code, &result))
	return core.Object{"state": state, "step": step, "errorCode": code, "result": result}
}

// due makes a parked operation claimable now, standing in for its retry delay elapsing.
func (f *fixture) due(id string) {
	f.t.Helper()
	_, err := f.store.Pool.Exec(`UPDATE operations SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND state='retry_wait'`, id)
	must(f.t, err)
}

// D1, D2, D3, invariants 1–3, 5 and 6: while the Binding's first sync has not ended the clone step
// waits without registering an execution or spending an attempt, a later claim rechecks and keeps
// waiting, and once the snapshot is recorded the same operation freezes it once and completes.
func TestCloneStepWaitsForTheFirstSyncThenFreezesTheSnapshot(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	created := f.create("await-snapshot")
	pid, wid, oid := created.O("resource").S("id"), created.O("workspace").S("id"), created.O("operation").S("id")
	f.drain()
	waiting := func(when string) {
		t.Helper()
		op := f.operationRow(oid)
		if op.S("state") != "retry_wait" || op.S("errorCode") != "snapshot_not_ready" || op.S("step") != "clone" {
			t.Fatalf("%s: the clone step must wait for the snapshot, got %v", when, op)
		}
		if f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 0 || f.frozenClone(oid) != nil {
			t.Fatalf("%s: waiting registers no execution and freezes no input", when)
		}
		if w := f.ws(wid); w.B("admissionOpen") {
			t.Fatalf("%s: admission opened without a clone: %v", when, w)
		}
	}
	waiting("entering the step")
	f.due(oid)
	f.drain()
	waiting("a later claim")

	commit := f.syncProject(pid)
	f.due(oid)
	f.drain()
	if op := f.operationRow(oid); op.S("state") != "succeeded" {
		t.Fatalf("once the snapshot exists the operation completes, got %v", op)
	}
	binding, branch := f.bindingOf(pid).S("id"), f.bindingOf(pid).S("branch")
	want := core.Object{
		"kind": "clone", "repositoryUrl": simulator.GitServerBaseURL + "/" + f.tid + "/" + pid + ".git",
		"snapshotRef": core.GitSnapshotRef(binding, commit), "baseCommit": commit, "workspaceRef": core.GitWorkspaceRefPrefix(wid) + branch,
	}
	if got := f.frozenClone(oid); mustJSON(t, got) != mustJSON(t, want) {
		t.Fatalf("request.clone = %v, want %v", got, want)
	}
	// One execution, the first attempt, carrying the frozen input verbatim (jsonb equality).
	if f.scalar("SELECT count(*) FROM clone_executions e JOIN operations o ON o.id=e.operation_id WHERE e.operation_id=$1 AND e.input=o.request->'clone'", oid) != 1 ||
		f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 1 {
		t.Fatal("exactly one clone execution must carry the frozen input")
	}
	w := f.ws(wid)
	if w.S("baseCommitId") != commit || w.S("requestedRef") != branch || !w.B("admissionOpen") {
		t.Fatalf("the Workspace's baseline is the snapshot commit on the Binding branch, got %v", w)
	}
}

// D2: once the Binding's latest sync ended without success the waiting step fails with that sync
// recorded, and a failed create_project still leaves the Project usable. A sync the external line
// replays in the same transaction it ends the failed one keeps the step waiting instead.
func TestCloneStepEndsWithTheBindingSyncFailure(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	created := f.create("sync-replayed")
	pid, oid := created.O("resource").S("id"), created.O("operation").S("id")
	f.drain()
	binding := f.bindingOf(pid).S("id")
	// The automatic replay: the failed sync's settlement creates its successor in one transaction.
	tx, err := f.store.Pool.Begin()
	must(t, err)
	_, err = tx.Exec(`UPDATE repository_operations SET state='failed',error_code='upstream_unreachable',finished_at=clock_timestamp() WHERE binding_id=$1 AND state='pending'`, binding)
	must(t, err)
	_, err = tx.Exec(`INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger) VALUES(gen_random_uuid(),'repository_sync',$1,$2,$3,'pending','lease_expired')`, f.tid, pid, binding)
	must(t, err)
	must(t, tx.Commit())
	f.due(oid)
	f.drain()
	if op := f.operationRow(oid); op.S("state") != "retry_wait" || op.S("errorCode") != "snapshot_not_ready" {
		t.Fatalf("a replayed sync keeps the step waiting, got %v", op)
	}

	failed, err := f.preset().Operation(context.Background(), pid, "sync", "failed", "credential_rejected")
	must(t, err)
	f.due(oid)
	f.drain()
	op := f.operationRow(oid)
	if op.S("state") != "failed" || op.S("errorCode") != "repository_sync_failed" {
		t.Fatalf("a failed latest sync ends the step, got %v", op)
	}
	recorded := core.Object{}
	must(t, json.Unmarshal([]byte(op.S("result")), &recorded))
	if recorded.S("syncOperationId") != failed["operationId"] || recorded.S("syncErrorCode") != "credential_rejected" || len(recorded) != 2 {
		t.Fatalf("the failure must record the sync and its code: %s, want %v", op.S("result"), failed["operationId"])
	}
	// The public view keeps the safe code and leaves the internal sync Operation's ID to Cloud.
	if public := f.call("GET", f.path("/operations/"+oid), nil, "", 200).O("result"); public.S("syncErrorCode") != "credential_rejected" || len(public) != 1 {
		t.Fatalf("the public operation must carry only the sync's code, got %v", public)
	}
	if f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 0 {
		t.Fatal("a failed sync must never fall back to a clone")
	}
	if f.scalar("SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='active'", pid) != 1 {
		t.Fatal("a failed create_project must leave the Project active")
	}
	if w := f.ws(created.O("workspace").S("id")); w.B("admissionOpen") || w.S("observedState") != "unavailable" {
		t.Fatalf("the main Workspace stays closed: %v", w)
	}
}

// D2: the claim decides by one consistent read of the snapshot and the latest sync. The external
// line records a successful sync and its snapshot in one transaction without taking any lock the
// claim holds; here that transaction also holds repository_operations exclusively until the claim
// has started reading, so it commits between the claim's view of the snapshot table and its view of
// the sync. A claim that read them in two statements would see no snapshot and then the successful
// sync, and fail a step that has a snapshot to clone; one statement sees both or neither.
func TestCloneStepReadsTheSnapshotAndItsSyncTogether(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	created := f.create("consistent-read")
	pid, oid := created.O("resource").S("id"), created.O("operation").S("id")
	f.drain()
	if op := f.operationRow(oid); op.S("errorCode") != "snapshot_not_ready" {
		t.Fatalf("the step must first wait for the snapshot, got %v", op)
	}
	binding := f.bindingOf(pid).S("id")
	f.due(oid)

	writer, err := f.store.Pool.Begin()
	must(t, err)
	defer func() { _ = writer.Rollback() }()
	var sync string
	must(t, writer.QueryRow(`UPDATE repository_operations SET state='succeeded',finished_at=clock_timestamp() WHERE binding_id=$1 AND state='pending' RETURNING id`, binding).Scan(&sync))
	_, err = writer.Exec(`INSERT INTO repository_snapshots(binding_id,tenant_id,project_id,commit_oid,snapshot_ref,sync_operation_id) VALUES($1,$2,$3,$4,$5,$6)`, binding, f.tid, pid, f.commit, core.GitSnapshotRef(binding, f.commit), sync)
	must(t, err)
	_, err = writer.Exec(`LOCK TABLE repository_operations IN ACCESS EXCLUSIVE MODE`)
	must(t, err)

	type answer struct {
		body   core.Object
		status int
		err    error
	}
	claimed := make(chan answer, 1)
	go func() {
		o, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/operations/claim", "controller", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: f.client.Subject}}, &f.user, "", core.Object{"epoch": f.controller.Epoch})
		claimed <- answer{o, status, e}
	}()
	// The claim is now blocked on the writer's lock, past any read it already made.
	deadline := time.Now().Add(30 * time.Second)
	for f.scalar(`SELECT count(*) FROM pg_locks WHERE relation='repository_operations'::regclass AND NOT granted`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the claim never reached the Binding's syncs")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, writer.Commit())
	got := <-claimed
	must(t, got.err)
	if got.status != http.StatusOK || got.body.O("operation").S("id") != oid {
		t.Fatalf("the claim must hand out the step with its snapshot: %d %v", got.status, got.body)
	}
	if op := f.operationRow(oid); op.S("state") != "running" || op.S("step") != "clone" {
		t.Fatalf("a successful sync recorded with its snapshot must not fail the step, got %v", op)
	}
	if frozen := f.frozenClone(oid); frozen.S("baseCommit") != f.commit {
		t.Fatalf("the step froze %v, want the snapshot %s", frozen, f.commit)
	}
}

// D2 and invariant 4: a waiter at the head of the queue never empties a claim; the same claim parks
// it again and hands out the next claimable operation.
func TestClaimHandsOutTheNextOperationPastAWaiter(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	waiter := f.create("head-waiter").O("operation").S("id")
	f.drain()
	f.withoutSnapshot = false
	other := f.call("POST", f.path("/projects"), core.Object{"name": "Second", "repositoryUrl": "https://example.invalid/second.git", "defaultBranch": "main"}, "behind-waiter", 202).O("operation").S("id")
	f.due(waiter)
	claimed := f.internal("/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch}, 200)
	if got := claimed.O("operation").S("id"); got != other {
		t.Fatalf("the claim must hand out the operation behind the waiter, got %q want %q", got, other)
	}
	if op := f.operationRow(waiter); op.S("state") != "retry_wait" || op.S("errorCode") != "snapshot_not_ready" {
		t.Fatalf("the waiter is parked again in the same claim, got %v", op)
	}
	if f.scalar("SELECT count(*) FROM operations WHERE id=$1 AND retry_at>clock_timestamp()", waiter) != 1 {
		t.Fatal("the waiter must wait again before its next recheck")
	}
}

// D1, invariant 2: the input is frozen once per operation. A retry after a failed attempt clones the
// same snapshot even when the Binding's current snapshot moved meanwhile, while a Workspace created
// afterwards freezes the new one.
func TestFrozenCloneInputOutlivesSnapshotUpdates(t *testing.T) {
	f := setup(t)
	created := f.create("frozen-input")
	pid, wid, oid := created.O("resource").S("id"), created.O("workspace").S("id"), created.O("operation").S("id")
	f.substrate.SetFault("clone", "fail")
	if err := f.controller.Drain(context.Background()); err == nil {
		t.Fatal("the fixture did not fail the first clone")
	}
	frozen := f.frozenClone(oid)
	if frozen.S("baseCommit") != f.commit {
		t.Fatalf("the first attempt froze %v, want the first snapshot %s", frozen, f.commit)
	}
	moved, err := f.preset().Snapshot(context.Background(), pid, "", 3)
	must(t, err)
	f.substrate.SetFault("clone", "")
	f.due(oid)
	f.drain()
	if got := f.frozenClone(oid); mustJSON(t, got) != mustJSON(t, frozen) {
		t.Fatalf("a retry must not re-freeze the input: %v, was %v", got, frozen)
	}
	if f.scalar("SELECT count(DISTINCT input::text) FROM clone_executions WHERE operation_id=$1", oid) != 1 || f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 2 {
		t.Fatal("every attempt of one operation carries the same input")
	}
	if f.ws(wid).S("baseCommitId") != f.commit {
		t.Fatal("the main Workspace's baseline is the snapshot it froze")
	}
	isolated := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Later"}, "after-update", 202)
	f.drain()
	if got := f.frozenClone(isolated.O("operation").S("id")); got.S("baseCommit") != moved["commit"] {
		t.Fatalf("a later Workspace freezes the current snapshot %v, got %v", moved["commit"], got)
	}
	if f.ws(wid).S("baseCommitId") != f.commit {
		t.Fatal("a later snapshot must not touch an existing Workspace's baseline")
	}
}

// D3: an isolated Workspace starts from the Binding's branch. An absent, empty or HEAD baseRef means
// it; any other branch is refused before anything is recorded.
func TestIsolatedWorkspaceBaseRefMustBeTheSyncedBranch(t *testing.T) {
	f := setup(t)
	created := f.create("base-ref")
	pid := created.O("resource").S("id")
	f.drain()
	workspaces, operations := f.scalar("SELECT count(*) FROM workspaces"), f.scalar("SELECT count(*) FROM operations")
	refused := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Feature", "baseRef": "feature"}, "base-feature", 400)
	if refused.S("code") != "base_ref_not_synced" || f.scalar("SELECT count(*) FROM workspaces") != workspaces || f.scalar("SELECT count(*) FROM operations") != operations {
		t.Fatalf("another branch must be refused with nothing recorded: %v", refused)
	}
	for i, body := range []core.Object{{"title": "Omitted"}, {"title": "Empty", "baseRef": ""}, {"title": "Head", "baseRef": "HEAD"}, {"title": "Main", "baseRef": "main"}} {
		accepted := f.call("POST", f.path("/projects/"+pid+"/workspaces"), body, "base-ok-"+string(rune('a'+i)), 202)
		if got := f.ws(accepted.O("resource").S("id")).S("requestedRef"); got != "main" {
			t.Fatalf("%v: requestedRef = %q, want the Binding branch", body, got)
		}
		f.drain()
	}
}

// D4, invariant 7: creating a run Workspace starts no sync and clones the current snapshot as it is.
func TestRunWorkspaceUsesTheCurrentSnapshotWithoutSyncing(t *testing.T) {
	f := setup(t)
	created := f.create("run-no-sync")
	pid := created.O("resource").S("id")
	f.drain()
	snapshot := func() string {
		var row string
		must(t, f.store.Pool.QueryRow(`SELECT to_jsonb(s)::text FROM repository_snapshots s WHERE project_id=$1`, pid).Scan(&row))
		return row
	}
	before, syncs := snapshot(), f.scalar("SELECT count(*) FROM repository_operations WHERE project_id=$1", pid)
	run := f.insertAgentRun(t, pid)
	wid := f.readyRunWorkspace(t, run)
	if snapshot() != before || f.scalar("SELECT count(*) FROM repository_operations WHERE project_id=$1", pid) != syncs {
		t.Fatal("a run Workspace must not trigger or record a sync")
	}
	var input string
	must(t, f.store.Pool.QueryRow(`SELECT o.request->'clone'->>'baseCommit' FROM operations o WHERE o.workspace_id=$1 AND o.kind='create_workspace'`, wid).Scan(&input))
	if input != f.commit || f.scalar("SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id=$2", wid, f.commit) != 1 {
		t.Fatalf("the run Workspace clones the current snapshot, got %s", input)
	}
}

// D6, invariant 9: delete_project is not handed to a Controller while a sync or publish of the
// Project is running; it waits without failing and proceeds once the Operation ended.
func TestDeleteProjectWaitsForARunningRepositoryOperation(t *testing.T) {
	for _, kind := range []string{"sync", "publish"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			created := f.create("delete-waits-" + kind)
			pid, wid := created.O("resource").S("id"), created.O("workspace").S("id")
			f.drain()
			running, err := f.preset().Operation(context.Background(), pid, kind, "running", "")
			must(t, err)
			p := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
			f.releaseRuntimeControl(wid)
			deletion := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": p.N("version")}, "delete-waits-delete-"+kind, 202)
			oid := deletion.O("operation").S("id")
			f.drain()
			if op := f.operationRow(oid); op.S("state") != "retry_wait" || op.S("errorCode") != "repository_operation_running" || op.S("step") != "quiesce" {
				t.Fatalf("delete_project must wait for the running %s before quiescing, got %v", kind, op)
			}
			if f.scalar("SELECT count(*) FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NULL", wid) != 1 {
				t.Fatalf("the sandbox must not be terminated while the %s runs", kind)
			}
			// The external line ends the Operation it was running.
			_, err = f.store.Pool.Exec(`UPDATE repository_operations SET state='succeeded',finished_at=clock_timestamp() WHERE id=$1`, running["operationId"])
			must(t, err)
			f.due(oid)
			f.drain()
			if op := f.operationRow(oid); op.S("state") != "succeeded" || op.S("step") != "done" {
				t.Fatalf("delete_project proceeds once the %s ended, got %v", kind, op)
			}
		})
	}
}

// D1: a deployment without a Git Server cannot freeze a clone input; the step blocks for good.
func TestCloneStepBlocksWithoutAGitServer(t *testing.T) {
	f := setup(t)
	f.store.GitServer = nil
	oid := f.create("no-git-server").O("operation").S("id")
	f.drain()
	if op := f.operationRow(oid); op.S("state") != "blocked" || op.S("errorCode") != "git_server_unconfigured" || op.S("step") != "clone" {
		t.Fatalf("without a Git Server the clone step blocks, got %v", op)
	}
	if f.scalar("SELECT count(*) FROM clone_executions WHERE operation_id=$1", oid) != 0 {
		t.Fatal("no clone may be dispatched without a Git Server")
	}
}

// D5, invariant 8: Cloud has no tenant-level clone entry point or storage.
func TestTenantLevelClonesAreGone(t *testing.T) {
	f := setup(t)
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		req, err := http.NewRequestWithContext(t.Context(), method, f.cloud.URL+f.path("/clones"), strings.NewReader(`{"requestId":"r","repository":"https://example.invalid/repo.git","branch":"main"}`))
		must(t, err)
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s /clones answered %d, want 404", method, res.StatusCode)
		}
	}
	var table *string
	must(t, f.store.Pool.QueryRow(`SELECT to_regclass('clone_requests')::text`).Scan(&table))
	if table != nil {
		t.Fatalf("clone_requests still exists: %s", *table)
	}
}
