package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// runGraph is a two-step document (start -> agent) so the test proves node_states follow the
// graph the run pinned, including that the second node's state appears. Its agent node carries the
// pre-slot `agentConfig.executor` field a desktop-imported graph still has, which the gate reads as
// the implicit `default` slot — so this graph is also the evidence that such a document keeps running
// (specs#98 D5).
func runGraph() core.Object { return agentRunGraph("", true, nil) }

// fixtureModelID is the model the fixture personal connection offers, and the one the fixture default
// selection names.
const fixtureModelID = "vendor/model-v1"

// fixtureModels is a connection's model list. An empty list is not a legal connection, so every caller
// names at least one.
func fixtureModels(ids ...string) []any {
	out := []any{}
	for _, id := range ids {
		out = append(out, core.Object{"id": id, "name": id, "contextWindow": 128000, "maxTokens": 8192})
	}
	return out
}

// personalModelBody is a complete connection body. The personal model API replaces the whole
// connection on update, so a case that changes one field resends the rest.
func personalModelBody(name string, models []any, enabled bool) core.Object {
	return core.Object{
		"name": name, "protocol": "openai-completions", "baseUrl": "https://models.example.invalid/v1",
		"authMode": "bearer", "models": models, "enabled": enabled,
	}
}

// seedPersonalModel gives the fixture actor a complete personal model configuration — an enabled
// connection carrying a credential, and a default selection naming one of its models — and returns the
// connection as the API reports it. The workflow start gate resolves the implicit `default` slot
// through exactly this state, so a test that needs a start to be admitted answers it the way a user
// would: through the personal model API, never by patching the store.
//
// The credential is written through Store.PutModelCredential because core never accepts a plaintext
// key: in production the gateway hands it an authenticated ciphertext, and the fixture stands in for
// that boundary with an in-memory buffer that is not a key and is never logged.
func seedPersonalModel(t *testing.T, f *fixture) core.Object {
	t.Helper()
	connection := f.call("POST", "/api/v1/me/model-connections",
		personalModelBody("Personal", fixtureModels(fixtureModelID), true), "wf-model-connection", 201).O("resource")
	_, err := f.store.PutModelCredential(context.Background(), &f.user, connection.S("id"), connection.N("version"),
		"wf-model-credential", uuid.NewString(), make([]byte, 32), "fixture-key")
	must(t, err)
	f.call("PUT", "/api/v1/me/model-default", core.Object{
		"connectionId": connection.S("id"), "modelId": fixtureModelID, "version": 0,
	}, "", 200)
	return f.call("GET", "/api/v1/me/model-connections/"+connection.S("id"), nil, "", 200)
}

func TestWorkflowRunLifecycle(t *testing.T) {
	f := setup(t)
	// The document's agent node runs on the initiator's personal default model, so the actor needs one
	// before any start can be admitted.
	seedPersonalModel(t, f)
	// Every direct run names the Project it freezes, whether or not its graph provisions a Workspace.
	pid := runProject(t, f, "run-project")
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Run target", "graph": runGraph(),
	}, "run-wf", 200).O("resource")
	wfid := wf.S("id")

	// A workflow with no published snapshot cannot be run: the live graph is a draft. The snapshot is
	// resolved before the executor slots are, so a missing version is reported as itself rather than as
	// an unconfigured slot.
	noSnapshot := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"name": "X"}, "run-0", 400)
	if noSnapshot.S("code") != "workflow_no_published_snapshot" {
		t.Fatalf("no-snapshot create code = %q", noSnapshot.S("code"))
	}

	// Publish, then run: the dev simulator (wired by setup) fills the trace and the row is terminal.
	snap := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "run-pub", 200).O("resource")
	snapID := snap.S("id")
	run := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{
		"name": "Trial run", "input": core.Object{"branch": "main"}, "projectId": pid,
	}, "run-1", 200).O("resource")
	if run.S("workflowId") != wfid || run.S("name") != "Trial run" || run.S("status") != "succeeded" {
		t.Fatalf("create run wrong: %v", run)
	}
	if run.S("snapshotId") != snapID {
		t.Fatalf("run pinned wrong snapshot: %v", run)
	}
	states := run.O("nodeStates")
	if states.O("start").S("status") != "succeeded" || states.O("agent-1").S("status") != "succeeded" {
		t.Fatalf("run node_states missing nodes: %v", states)
	}
	// The run echoes the kickoff input as its start-node output: the one thing the sim can say truthfully.
	output := states.O("start").O("output").O("input")
	if output.S("branch") != "main" {
		t.Fatalf("start output did not echo input: %v", output)
	}
	// definitionSnapshot is the frozen graph, not the live document: the run carries its own copy.
	ds := run.O("definitionSnapshot")
	if ds.O("viewport").N("zoom") != 1 {
		t.Fatalf("definition snapshot missing: %v", ds)
	}
	if _, ok := run["updatedAt"]; !ok {
		t.Fatal("run missing updatedAt")
	}

	// Same idempotency key replays the same run; a fresh key with no name defaults to the workflow name.
	replay := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{
		"name": "Trial run", "input": core.Object{"branch": "main"}, "projectId": pid,
	}, "run-1", 200).O("resource")
	if replay.S("id") != run.S("id") {
		t.Fatalf("idempotent create made another run: %v", replay)
	}
	f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": pid}, "run-2", 200).O("resource")

	// Read one run back and list them (newest of the run-2 page is visible by reading ids).
	one := f.call("GET", f.path("/workflows/"+wfid+"/runs/"+run.S("id")), nil, "", 200)
	if one.S("status") != "succeeded" || one.O("nodeStates").O("agent-1").S("status") != "succeeded" {
		t.Fatalf("run read-back changed the trace: %v", one)
	}
	listed := issueItems(f.call("GET", f.path("/workflows/"+wfid+"/runs"), nil, "", 200))
	if len(listed) != 2 {
		t.Fatalf("run list has %d rows, want 2", len(listed))
	}
	// The generic list returns full rows; page it with limit to prove the envelope carries nextCursor.
	page := f.call("GET", f.path("/workflows/"+wfid+"/runs")+"?limit=1", nil, "", 200)
	if len(issueItems(page)) != 1 || page.S("nextCursor") == "" {
		t.Fatalf("run page envelope wrong: %v", page)
	}

	// A run pinned to a specific, still-owned snapshot works; a foreign snapshot id is read 404.
	explicit := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{
		"snapshotId": snapID, "projectId": pid,
	}, "run-3", 200).O("resource")
	if explicit.S("snapshotId") != snapID {
		t.Fatalf("explicit snapshot create wrong: %v", explicit)
	}
	f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{
		"snapshotId": uuid.NewString(),
	}, "run-4", 404)

	// The input must be an object.
	f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"input": []any{}}, "run-5", 400)
	f.call("GET", f.path("/workflows/"+wfid+"/runs/not-a-uuid"), nil, "", 404)
	f.call("GET", f.path("/workflows/"+wfid+"/runs/"+uuid.NewString()), nil, "", 404)

	// Archiving the workflow hides its whole run history.
	f.call("DELETE", f.path("/workflows/"+wfid), core.Object{"version": f.call("GET", f.path("/workflows/"+wfid), nil, "", 200).N("version")}, "run-del", 200)
	f.call("GET", f.path("/workflows/"+wfid+"/runs"), nil, "", 404)
	f.call("GET", f.path("/workflows/"+wfid+"/runs/"+run.S("id")), nil, "", 404)
	f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{}, "run-6", 404)
}

func TestWorkflowRunTenantIsolation(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Isolated run source", "graph": runGraph(),
	}, "iso-run-wf", 200).O("resource")
	wfid := wf.S("id")
	snap := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "iso-run-pub", 200).O("resource")
	run := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"projectId": runProject(t, f, "iso-run-project")}, "iso-run-1", 200).O("resource")

	other, e := f.store.Bootstrap(context.Background(), "Other", "corp", "other", "Other")
	must(t, e)
	original := f.tid
	f.tid = other.S("tenantId")
	// The fixture actor is not a member of the other tenant, so every run operation is a typed 403.
	f.call("GET", f.path("/workflows/"+wfid+"/runs"), nil, "", 403)
	f.call("GET", f.path("/workflows/"+wfid+"/runs/"+run.S("id")), nil, "", 403)
	reject := f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{}, "iso-run-2", 403)
	if reject.S("code") == "" {
		t.Fatalf("cross-tenant reject had no code: %v", reject)
	}
	// Naming the snapshot explicitly does not get past the membership check either: the version a run
	// executes is never a way around the tenant boundary.
	f.call("POST", f.path("/workflows/"+wfid+"/runs"), core.Object{"snapshotId": snap.S("id")}, "iso-run-3", 403)
	f.tid = original
}
