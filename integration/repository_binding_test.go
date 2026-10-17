package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// bindingOf returns the Project's only Binding.
func (f *fixture) bindingOf(projectID string) core.Object {
	f.t.Helper()
	var id, url, branch string
	var resync bool
	must(f.t, f.store.Pool.QueryRow("SELECT id,repository_url,branch,resync_requested FROM repository_bindings WHERE project_id=$1", projectID).Scan(&id, &url, &branch, &resync))
	return core.Object{"id": id, "repositoryUrl": url, "branch": branch, "resyncRequested": resync}
}

// repositoryOperation inserts one Operation row in the given state, as the external line would.
func (f *fixture) repositoryOperation(projectID, bindingID, kind, state string) (string, error) {
	f.t.Helper()
	id := uuid.NewString()
	unfinished := map[string]bool{"awaiting_approval": true, "pending": true, "leased": true, "running": true, "uncertain": true}[state]
	_, err := f.store.Pool.Exec("INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,finished_at) VALUES($1,$2,$3,$4,$5,$6,'test',CASE WHEN $7 THEN NULL ELSE clock_timestamp() END)", id, kind, f.tid, projectID, bindingID, state, unfinished)
	return id, err
}

func (f *fixture) repositoryOperationState(id string) (state, code string) {
	f.t.Helper()
	var errorCode *string
	var finished bool
	must(f.t, f.store.Pool.QueryRow("SELECT state,error_code,finished_at IS NOT NULL FROM repository_operations WHERE id=$1", id).Scan(&state, &errorCode, &finished))
	if errorCode != nil {
		code = *errorCode
	}
	return state, code
}

// Creating a Project records its Binding and the sync that must complete before any snapshot
// exists, in the same transaction; the published Project keeps its repository fields.
func TestCreateProjectRecordsBindingAndInitialSync(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	created := f.create("binding-create")
	project := created.O("resource")
	if project.S("repositoryUrl") != "https://example.invalid/repo.git" || project.S("defaultBranch") != "main" {
		t.Fatalf("the Project resource lost its repository fields: %v", project)
	}
	b := f.bindingOf(project.S("id"))
	if b.S("repositoryUrl") != "https://example.invalid/repo.git" || b.S("branch") != "main" || b.B("resyncRequested") {
		t.Fatalf("binding = %v", b)
	}
	var kind, state, trigger string
	var finished bool
	must(t, f.store.Pool.QueryRow("SELECT type,state,trigger,finished_at IS NOT NULL FROM repository_operations WHERE binding_id=$1", b.S("id")).Scan(&kind, &state, &trigger, &finished))
	if kind != core.RepositorySync || state != "pending" || trigger != "project_created" || finished {
		t.Fatalf("initial sync = %s/%s/%s finished=%v, want a pending project_created sync", kind, state, trigger, finished)
	}
	if f.scalar("SELECT count(*) FROM repository_operations WHERE project_id=$1", project.S("id")) != 1 || f.scalar("SELECT count(*) FROM repository_snapshots WHERE binding_id=$1", b.S("id")) != 0 {
		t.Fatal("a new Project must have exactly one sync and no snapshot")
	}
	// A refused Project writes no Binding or Operation either.
	f.call("POST", f.path("/projects"), core.Object{"name": "P", "repositoryUrl": "ftp://example.invalid/r", "defaultBranch": "main"}, "binding-refused", 400)
	if f.scalar("SELECT count(*) FROM repository_bindings") != 1 || f.scalar("SELECT count(*) FROM repository_operations") != 1 {
		t.Fatal("a refused Project left a Binding or Operation")
	}
}

// The database itself fixes the Operation states per type, finished_at with the terminal states,
// one unfinished sync per Binding, and the snapshot ref's name.
func TestRepositoryOperationStorageConstraints(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	pid := f.create("binding-constraints").O("resource").S("id")
	bid := f.bindingOf(pid).S("id")

	if _, err := f.repositoryOperation(pid, bid, core.RepositorySync, "running"); err == nil {
		t.Fatal("a second unfinished sync for one Binding was accepted")
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='failed',error_code='test',finished_at=clock_timestamp() WHERE binding_id=$1", bid)))
	sync, err := f.repositoryOperation(pid, bid, core.RepositorySync, "leased")
	must(t, err)
	for _, state := range []string{"awaiting_approval", "pending", "running"} {
		if _, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, state); err != nil {
			t.Fatalf("an unfinished publish next to an unfinished sync was refused: %v", err)
		}
	}
	for _, c := range []struct{ kind, state string }{
		{core.RepositorySync, "uncertain"},
		{core.RepositorySync, "awaiting_approval"},
		{core.RepositorySync, "conflict"},
		{core.RepositoryPublish, "superseded"},
		{"repository_clone", "pending"},
	} {
		if _, err := f.repositoryOperation(pid, bid, c.kind, c.state); err == nil {
			t.Errorf("%s in state %s was accepted", c.kind, c.state)
		}
	}
	if _, err := f.store.Pool.Exec("UPDATE repository_operations SET state='succeeded' WHERE id=$1", sync); err == nil {
		t.Error("a terminal state without finished_at was accepted")
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='succeeded',finished_at=clock_timestamp() WHERE id=$1", sync)))

	commit := strings.Repeat("ab", 20)
	snapshot := "INSERT INTO repository_snapshots(binding_id,tenant_id,project_id,commit_oid,snapshot_ref,sync_operation_id) VALUES($1,$2,$3,$4,$5,$6)"
	if _, err := f.store.Pool.Exec(snapshot, bid, f.tid, pid, commit, core.GitSnapshotRef(bid, strings.Repeat("cd", 20)), sync); err == nil {
		t.Error("a snapshot whose ref names another commit was accepted")
	}
	if _, err := f.store.Pool.Exec(snapshot, bid, f.tid, pid, strings.Repeat("0", 40), core.GitSnapshotRef(bid, strings.Repeat("0", 40)), sync); err == nil {
		t.Error("a zero snapshot commit was accepted")
	}
	must(t, firstErr(f.store.Pool.Exec(snapshot, bid, f.tid, pid, commit, core.GitSnapshotRef(bid, commit), sync)))
}

// Entering deleting cancels every Operation that has not reached running and drops the resync
// request in the same transaction; delete_project then waits until the running ones have ended.
func TestDeletingProjectCancelsQueuedRepositoryOperationsAndWaitsForRunning(t *testing.T) {
	f := setup(t)
	pid := f.create("binding-delete").O("resource").S("id")
	f.drain()
	bid := f.bindingOf(pid).S("id")
	// The Project's first sync produced its snapshot; a later one is still pending.
	pending, err := f.repositoryOperation(pid, bid, core.RepositorySync, "pending")
	must(t, err)
	queued := []string{pending}
	for _, state := range []string{"awaiting_approval", "pending", "leased"} {
		id, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, state)
		must(t, err)
		queued = append(queued, id)
	}
	running, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, "running")
	must(t, err)
	uncertain, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, "uncertain")
	must(t, err)
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_bindings SET resync_requested=true WHERE id=$1", bid)))

	// The Controller's binding acknowledgement returns the main Workspace's control to idle.
	f.acknowledgeSimulatorBindings()
	project := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	deleting := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "binding-delete-request", 202)
	for _, id := range queued {
		if state, code := f.repositoryOperationState(id); state != "cancelled" || code != "project_deleting" {
			t.Fatalf("operation %s = %s/%s, want cancelled/project_deleting", id, state, code)
		}
	}
	for _, id := range []string{running, uncertain} {
		if state, _ := f.repositoryOperationState(id); state != "running" && state != "uncertain" {
			t.Fatalf("an Operation already past leased was cancelled: %s", state)
		}
	}
	if f.bindingOf(pid).B("resyncRequested") {
		t.Fatal("entering deleting kept the resync request")
	}

	// Operation D6: the claim does not hand delete_project to the Controller while Operations are
	// unfinished; it waits without failing.
	f.acknowledgeSimulatorBindings()
	f.drain()
	if op := f.call("GET", f.path("/operations/"+deleting.O("operation").S("id")), nil, "", 200); op.S("step") != "quiesce" || op.S("state") != "retry_wait" || op.S("errorCode") != "repository_operation_running" {
		t.Fatalf("delete_project must wait at quiesce, got %v", op)
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='succeeded',finished_at=clock_timestamp() WHERE id=$1", running)))
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='failed',error_code='test',finished_at=clock_timestamp() WHERE id=$1", uncertain)))
	f.due(deleting.O("operation").S("id"))
	f.drain()
	var lifecycle string
	must(t, f.store.Pool.QueryRow("SELECT lifecycle FROM projects WHERE id=$1", pid).Scan(&lifecycle))
	if lifecycle != "deleted" {
		t.Fatalf("project lifecycle = %s after its Operations ended, want deleted", lifecycle)
	}
}

// A Node refusing quiesce returns the Project from deleting to active. The sync the deletion
// cancelled or the resync request it dropped comes back in that same transaction, as a pending
// deletion_aborted sync or, while a running sync still holds the one-unfinished-sync slot, as the
// resync request; cancelled publishes stay cancelled, and a deletion that owed nothing adds none.
func TestAbortedProjectDeletionRestoresTheOwedSync(t *testing.T) {
	for _, c := range []struct {
		name string
		// prepare shapes the Binding before deletion and returns the publish expected to stay cancelled.
		prepare             func(f *fixture, pid, bid, initial string) (publish string)
		wantAborted, resync bool
	}{
		{
			name: "queued sync cancelled",
			prepare: func(f *fixture, pid, bid, _ string) string {
				// The fixture's Git Server double already ended the project_created sync with a
				// snapshot, so the queued sync the deletion cancels is a later one.
				_, err := f.repositoryOperation(pid, bid, core.RepositorySync, "pending")
				must(f.t, err)
				publish, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, "awaiting_approval")
				must(f.t, err)
				return publish
			},
			wantAborted: true,
		},
		{
			name: "resync requested while a sync runs",
			prepare: func(f *fixture, pid, bid, _ string) string {
				_, err := f.repositoryOperation(pid, bid, core.RepositorySync, "running")
				must(f.t, err)
				must(f.t, firstErr(f.store.Pool.Exec("UPDATE repository_bindings SET resync_requested=true WHERE id=$1", bid)))
				return ""
			},
			resync: true,
		},
		{
			name: "nothing owed",
			prepare: func(f *fixture, _, _, initial string) string {
				must(f.t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='succeeded',finished_at=clock_timestamp() WHERE id=$1", initial)))
				return ""
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			created := f.create("binding-abort")
			pid, wid := created.O("resource").S("id"), created.O("workspace").S("id")
			f.drain()
			bid := f.bindingOf(pid).S("id")
			var initial string
			must(t, f.store.Pool.QueryRow("SELECT id FROM repository_operations WHERE binding_id=$1", bid).Scan(&initial))
			publish := c.prepare(f, pid, bid, initial)

			f.acknowledgeSimulatorBindings()
			project := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
			deleting := f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": project.N("version")}, "binding-abort-delete", 202)
			node := f.node(wid)
			var nodeVersion int64
			must(t, f.store.Pool.QueryRow("SELECT version FROM node_instances WHERE id=$1", node.Subject).Scan(&nodeVersion))
			refused, status, err := f.client.Call(context.Background(), "POST", "/internal/v1/nodes/idle", "node", node, nil, "", core.Object{"version": nodeVersion, "admissionEpoch": f.ws(wid).N("admissionEpoch"), "operationId": deleting.O("operation").S("id"), "idle": false})
			must(t, err)
			if status != 200 || refused.B("accepted") {
				t.Fatalf("quiesce refusal = %d %v", status, refused)
			}
			if p := f.call("GET", f.path("/projects/"+pid), nil, "", 200); p.S("lifecycle") != "active" {
				t.Fatalf("a refused deletion left the Project %s", p.S("lifecycle"))
			}

			aborted := f.scalar("SELECT count(*) FROM repository_operations WHERE binding_id=$1 AND type='repository_sync' AND state='pending' AND trigger='deletion_aborted'", bid)
			if want := map[bool]int{true: 1, false: 0}[c.wantAborted]; aborted != want {
				t.Fatalf("pending deletion_aborted syncs = %d, want %d", aborted, want)
			}
			if got := f.bindingOf(pid).B("resyncRequested"); got != c.resync {
				t.Fatalf("resync requested = %v, want %v", got, c.resync)
			}
			if publish != "" {
				if state, code := f.repositoryOperationState(publish); state != "cancelled" || code != "project_deleting" {
					t.Fatalf("a cancelled publish came back as %s/%s", state, code)
				}
			}
		})
	}
}

// However create_project fails, the Project does not stay in provisioning.
func TestFailedProjectCreationLeavesProjectActive(t *testing.T) {
	f := setup(t)
	created := f.create("binding-create-failed")
	pid, oid := created.O("resource").S("id"), created.O("operation").S("id")
	f.substrate.SetFault("clone", "fail")
	for attempt := 1; attempt <= 3; attempt++ {
		if err := f.controller.Drain(context.Background()); err == nil {
			t.Fatal("fixture did not produce a clone failure")
		}
		op := f.call("GET", f.path("/operations/"+oid), nil, "", 200)
		if attempt < 3 {
			f.call("POST", f.path("/operations/"+oid+"/retry"), core.Object{"version": op.N("version")}, "binding-create-retry-"+string(rune('0'+attempt)), 202)
		}
	}
	if op := f.call("GET", f.path("/operations/"+oid), nil, "", 200); op.S("state") != "failed" {
		t.Fatalf("create_project = %s, want failed", op.S("state"))
	}
	if p := f.call("GET", f.path("/projects/"+pid), nil, "", 200); p.S("lifecycle") != "active" {
		t.Fatalf("a Project whose creation failed is %s, want active", p.S("lifecycle"))
	}
}

func firstErr(_ any, err error) error { return err }
