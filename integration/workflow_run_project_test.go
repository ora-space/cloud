package integration

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// frozenRunProject reads the Project a run froze, straight off the row. The run resource carries no
// project field, so the only way to tell "the API accepted the value" from "the creation transaction
// wrote it" is to look at what the transaction left behind.
func frozenRunProject(t *testing.T, f *fixture, runID string) string {
	t.Helper()
	var frozen sql.NullString
	if err := f.store.Pool.QueryRow(`SELECT project_id FROM workflow_runs WHERE id=$1`, runID).Scan(&frozen); err != nil {
		t.Fatalf("read run project: %v", err)
	}
	return frozen.String
}

func runCount(t *testing.T, f *fixture, wfid string) int {
	t.Helper()
	return len(issueItems(f.call("GET", f.path("/workflows/"+wfid+"/runs"), nil, "", 200)))
}

// runProject creates a Project the fixture actor may start runs in and returns its id. Every direct
// run names one, so a test that creates a run needs a reachable Project even when its graph never
// provisions a Workspace. The Project is deliberately left `provisioning`: a Project still converging
// is an accepted place to freeze a run against, and draining it would make each of these tests wait
// on an operation it does not care about.
func runProject(t *testing.T, f *fixture, key string) string {
	t.Helper()
	return f.call("POST", f.path("/projects"), core.Object{
		"name": "Run project", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main",
	}, key, 202).O("resource").S("id")
}

// archiveProject takes a fully provisioned Project into `deleting` and stops there — the caller
// drains when it wants the delete to settle. The runtime control the clone left on the Project's main
// workspace has to be handed back first, or the Project API refuses the delete as held.
func archiveProject(t *testing.T, f *fixture, pid, key string) {
	t.Helper()
	active := f.call("GET", f.path("/projects/"+pid), nil, "", 200)
	if active.S("lifecycle") != "active" {
		t.Fatalf("project lifecycle = %q; want active before the delete", active.S("lifecycle"))
	}
	f.call("DELETE", f.path("/projects/"+pid), core.Object{"version": active.N("version")}, key, 202)
}

// foreignProject provisions a second tenant and returns a live Project id inside it. The row is
// seeded directly because the fixture actor is not a member of that tenant and so cannot create one
// there through the API — which is exactly the property under test: the id names a Project that
// really exists and is unreachable from the calling tenant, so the refusal cannot be explained by
// the Project merely being absent. The Project and its main workspace go in together because the
// deferred project_main trigger requires a live Project to carry exactly one.
func foreignProject(t *testing.T, f *fixture) string {
	t.Helper()
	other, err := f.store.Bootstrap(t.Context(), "Other", "corp", "other", "Other")
	must(t, err)
	pid := uuid.NewString()
	tx, err := f.store.Pool.Begin()
	if err != nil {
		t.Fatalf("foreign project begin: %v", err)
	}
	defer tx.Rollback()
	// The repository lives in the Project's Binding, not in Project columns (0037): the two go in
	// together, the way every other direct seed in this repository now does it.
	if _, err := tx.Exec(`WITH p AS (INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle) VALUES($1,$2,$3,$4,'Foreign','active') RETURNING id,tenant_id) INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch) SELECT gen_random_uuid(),tenant_id,id,'https://example.invalid/foreign.git','main' FROM p`,
		pid, other.S("tenantId"), other.S("userId"), other.S("spaceId")); err != nil {
		t.Fatalf("seed foreign project: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',1,'HEAD')`,
		uuid.NewString(), other.S("tenantId"), other.S("userId"), pid); err != nil {
		t.Fatalf("seed foreign project workspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed foreign project commit: %v", err)
	}
	return pid
}

// The direct run's Project is stated by the caller and frozen in the creation transaction (D3).
// Before this, no production path wrote workflow_runs.project_id at all, so every editor-created run
// was identity-incomplete and could only ever fail at its first agent node
// (workflow_run_project_required); the run's Workspace had nowhere to provision and the project
// access a run implies was never checked against anyone.
func TestWorkflowRunFreezesCallerProject(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	pid := f.call("POST", f.path("/projects"), core.Object{
		"name": "Run project", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main",
	}, "run-project", 202).O("resource").S("id")
	wfid := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Project run", "graph": runGraph(),
	}, "run-project-wf", 200).O("resource").S("id")
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "run-project-pub", 200)

	run := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": pid}, "run-project-1", 200).O("resource")
	if got := frozenRunProject(t, f, run.S("id")); got != pid {
		t.Fatalf("run froze project %q; want %q", got, pid)
	}
	// The same run replayed under its idempotency key keeps the one frozen value.
	replay := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": pid}, "run-project-1", 200).O("resource")
	if replay.S("id") != run.S("id") || frozenRunProject(t, f, replay.S("id")) != pid {
		t.Fatalf("replayed create diverged: %v", replay)
	}

	// The identity is frozen, not re-derived: deleting the Project afterwards cannot change what an
	// already-created run was created against.
	f.drain()
	f.acknowledgeSimulatorBindings()
	archiveProject(t, f, pid, "run-project-del")
	f.drain()
	if got := frozenRunProject(t, f, run.S("id")); got != pid {
		t.Fatalf("project after delete = %q; want the frozen %q", got, pid)
	}
}

// A Project the caller cannot reach is refused, and refused inside the creation transaction: the run
// does not exist afterwards. The check is the same one every project-scoped route makes, so an
// unknown, a malformed and another tenant's Project are one indistinguishable 404 — a run is never
// created against a Project its creator could not open, and never against a default nobody named.
func TestWorkflowRunRefusesUnreachableProject(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	wfid := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Guarded run", "graph": runGraph(),
	}, "guard-run-wf", 200).O("resource").S("id")
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "guard-run-pub", 200)
	baseline := runCount(t, f, wfid)

	// A Project the caller owns but is deleting. Not drained, so it sits in `deleting` — the state a
	// run must never be frozen against, because its Workspace could never be created and the run would
	// stay `running` for good instead of reaching a terminal state.
	deleting := f.call("POST", f.path("/projects"), core.Object{
		"name": "Deleting", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main",
	}, "guard-run-deleting-project", 202).O("resource").S("id")
	f.drain()
	f.acknowledgeSimulatorBindings()
	archiveProject(t, f, deleting, "guard-run-deleting-del")

	for _, c := range []struct {
		name, projectID string
		status          int
		code            string
	}{
		{"unknown", uuid.NewString(), 404, "not_found"},
		{"malformed", "not-a-uuid", 404, "not_found"},
		{"other tenant", foreignProject(t, f), 404, "not_found"},
		{"deleting", deleting, 409, "resource_unavailable"},
	} {
		refused := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": c.projectID}, "guard-run-"+c.name, c.status)
		if refused.S("code") != c.code {
			t.Fatalf("%s project: code = %q; want %q", c.name, refused.S("code"), c.code)
		}
		if got := runCount(t, f, wfid); got != baseline {
			t.Fatalf("%s project created a run anyway: %d rows, want %d", c.name, got, baseline)
		}
	}

	// Once the delete settles the same Project is gone, and reads as the same 404 an unknown id does.
	f.drain()
	if refused := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": deleting}, "guard-run-deleted", 404); refused.S("code") != "not_found" {
		t.Fatalf("deleted project: code = %q; want not_found", refused.S("code"))
	}

	// Omitting the Project is refused at creation, and refused as the same condition
	// createWorkflowRunWorkspace raises it as: a run with no frozen Project is an aggregate that can
	// never provision the Workspace its agent nodes need, so it must not be persisted at all. A
	// missing Project is a distinct case from an unreachable one — nothing was named, so nothing was
	// authorized — which is why it is asserted here rather than as another row of the table above.
	missing := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{}, "guard-run-unbound", 409)
	if missing.S("code") != "workflow_run_project_required" {
		t.Fatalf("missing project: code = %q; want workflow_run_project_required", missing.S("code"))
	}
	if got := runCount(t, f, wfid); got != baseline {
		t.Fatalf("a run naming no project was created anyway: %d rows, want %d", got, baseline)
	}
}

// Naming a Project is mandatory for every direct run, including one whose graph never provisions a
// Workspace. project_id is half of the run's frozen identity (D3), so a run created without one would
// be born unable to finish rather than merely incomplete, and the place to say so is the creation
// transaction rather than an execution that can never proceed. A start -> input graph is refused
// exactly like an agent graph: that is the part a "only graphs that need a Workspace" reading of the
// rule would get wrong, so it is the case this test pins.
func TestWorkflowRunWithoutProjectIsRefused(t *testing.T) {
	f := setup(t)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "No project", "graph": startInputGraph(nil),
	}, "no-project-wf", 200).O("resource")
	wfid := wf.S("id")
	publishWorkflow(t, f, wfid, "no-project-pub")
	baseline := runCount(t, f, wfid)

	refused := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{}, "no-project-run", 409)
	if refused.S("code") != "workflow_run_project_required" {
		t.Fatalf("code = %q; want workflow_run_project_required", refused.S("code"))
	}
	if got := runCount(t, f, wfid); got != baseline {
		t.Fatalf("a refused start left %d runs, want %d", got, baseline)
	}

	// The same graph runs once the caller names a Project, so the refusal is about the missing
	// identity and not about the graph shape.
	f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": runProject(t, f, "no-project-project")}, "no-project-run-2", 200)
}
