package integration

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// agentRunGraph is a start -> agent document whose agent node asks for `slot` (empty means the
// implicit `default`). `legacy` keeps the pre-slot `agentConfig.executor` field a desktop-imported
// graph still carries, and a non-nil `slots` declares the graph's executor slots. Every admission case
// publishes a document built here, so the gate is exercised through the publish path an author uses
// rather than by writing a snapshot row directly.
func agentRunGraph(slot string, legacy bool, slots []any) core.Object {
	config := core.Object{"schemaVersion": 1, "prompt": "评审"}
	if slot != "" {
		config["executorSlot"] = slot
	}
	if legacy {
		config["executor"] = core.Object{}
	}
	graph := core.Object{
		"nodes": []any{
			core.Object{
				"id": "start", "type": "workflow", "deletable": false,
				"position": core.Object{"x": 0, "y": 0},
				"data":     core.Object{"kind": "start", "title": "开始", "description": ""},
			},
			core.Object{
				"id": "agent-1", "type": "workflow",
				"position": core.Object{"x": 300, "y": 0},
				"data": core.Object{
					"kind": "agent", "title": "评审", "description": "", "agentConfig": config,
				},
			},
		},
		"edges": []any{core.Object{
			"id": "e1", "kind": "data", "source": "start", "target": "agent-1",
		}},
		"viewport": core.Object{"x": 0, "y": 0, "zoom": 1},
	}
	if slots != nil {
		graph["executorSlots"] = slots
	}
	return graph
}

// problemCodes names the violations a publish refusal carries, in the order the validator reported
// them, so a case compares the whole list rather than searching for one code.
func problemCodes(t *testing.T, refusal core.Object) []string {
	t.Helper()
	out := []string{}
	for _, problem := range problems(t, refusal) {
		out = append(out, problem.S("code"))
	}
	return out
}

// problems reads the violation list of a publish refusal. It fails the test when the refusal is not
// the one stable code the publish gate uses, because a different code would reach the editor as an
// unrenderable error.
func problems(t *testing.T, refusal core.Object) []core.Object {
	t.Helper()
	if refusal.S("code") != "workflow_graph_invalid" {
		t.Fatalf("publish refusal = %v, want workflow_graph_invalid", refusal)
	}
	raw, _ := refusal.O("params")["problems"].([]any)
	out := []core.Object{}
	for _, item := range raw {
		problem, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("problem is not an object: %v", item)
		}
		out = append(out, core.Object(problem))
	}
	return out
}

// slotGaps reads the per-slot reasons of a start refusal, in the order the graph references them.
func slotGaps(t *testing.T, refusal core.Object) []core.Object {
	t.Helper()
	if refusal.S("code") != "workflow_executor_unconfigured" {
		t.Fatalf("start refusal = %v, want workflow_executor_unconfigured", refusal)
	}
	raw, _ := refusal.O("params")["slots"].([]any)
	out := []core.Object{}
	for _, item := range raw {
		gap, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("slot gap is not an object: %v", item)
		}
		out = append(out, core.Object(gap))
	}
	return out
}

// gapNodes names the nodes a slot gap reports.
func gapNodes(gap core.Object) []string {
	raw, _ := gap["nodeIds"].([]any)
	out := []string{}
	for _, id := range raw {
		if text, ok := id.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

// callRaw performs one authenticated public request and hands back its status instead of asserting it,
// so a caller running requests concurrently can collect every outcome and assert the complete set
// afterwards — and never calls t.Fatalf from a goroutine that is not the test's own.
func callRaw(f *fixture, method, path string, body core.Object, key string) (core.Object, int, error) {
	return f.client.Call(context.Background(), method, path, "gateway",
		core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &f.user, key, body)
}

// TestWorkflowPublishValidatesTheGraphAndSavingDoesNot covers the split the publish gate exists for.
// An unfinished draft is a legitimate document — the author is mid-edit — so create and update accept
// any graph object; publishing is the one place Cloud refuses a document it could not run, and the
// refusal names the node and the kind so the editor can mark it. The refused publish freezes nothing.
func TestWorkflowPublishValidatesTheGraphAndSavingDoesNot(t *testing.T) {
	f := setup(t)
	// A node kind outside the executable subset and no Start node: a canvas mid-edit.
	draft := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Unfinished flow",
		"graph": core.Object{
			"nodes": []any{core.Object{"id": "t", "data": core.Object{"kind": "tool"}}},
			"edges": []any{},
		},
	}, "adm-draft", 200).O("resource")
	if len(draft.O("graph")) == 0 {
		t.Fatalf("the unfinished draft was not stored: %v", draft)
	}
	// Editing an unfinished draft is allowed for the same reason: the author keeps working on it.
	f.call("PUT", f.path("/workflows/"+draft.S("id")), core.Object{
		"graph": core.Object{
			"nodes": []any{core.Object{"id": "t", "data": core.Object{"kind": "tool", "title": "工具"}}},
			"edges": []any{},
		},
		"version": draft.N("version"),
	}, "", 200)

	refused := f.call("POST", f.path("/workflows/"+draft.S("id")+"/publish"), core.Object{}, "adm-pub-1", 400)
	if codes := problemCodes(t, refused); !slices.Equal(codes, []string{"node_kind_unsupported", "start_node_count"}) {
		t.Fatalf("publish problems = %v", codes)
	}
	first := problems(t, refused)[0]
	if first.S("nodeId") != "t" || first.S("kind") != "tool" {
		t.Fatalf("the refusal does not name the node at fault: %v", first)
	}
	// A refused publish is not a version: nothing was frozen, and the workflow still has no history.
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", draft.S("id")); n != 0 {
		t.Fatalf("a refused publish froze %d snapshots", n)
	}
	// Nor can a graph that is not a document at all be published.
	f.call("POST", f.path("/workflows/"+draft.S("id")+"/publish"), core.Object{}, "adm-pub-1b", 400)

	// A legal graph publishes, and the draft's own history is what it publishes.
	live := f.call("GET", f.path("/workflows/"+draft.S("id")), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+draft.S("id")), core.Object{
		"graph": agentRunGraph("", true, nil), "version": live.N("version"),
	}, "", 200)
	snap := f.call("POST", f.path("/workflows/"+draft.S("id")+"/publish"), core.Object{}, "adm-pub-2", 200).O("resource")
	if snap.N("version") != 1 {
		t.Fatalf("the first accepted publish is not version 1: %v", snap)
	}
}

// TestWorkflowPublishRefusesUnusedAgentConfiguration covers the other half of the publish gate's job
// (#67 D2): refusing a node kind Cloud does not execute, and refusing a *configuration field* it does
// not execute. The outcome the decision rules out is publishing the document with the field silently
// ignored, which reads to its author as "my setting is in effect" when no runner will ever apply it.
// Saving stays allowed for the same reason it does elsewhere — the draft is still being written.
func TestWorkflowPublishRefusesUnusedAgentConfiguration(t *testing.T) {
	f := setup(t)
	draft := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Configured flow", "graph": agentRunGraph("", false, nil),
	}, "adm-cfg-create", 200).O("resource")

	// The graph the editor would write for an author who configured retry and skills: both are in the
	// canonical schema, and neither is in the first phase's executable subset.
	configured := agentRunGraph("", false, nil)
	config := configured["nodes"].([]any)[1].(core.Object).O("data").O("agentConfig")
	config["retry"] = core.Object{"max": 3}
	config["skills"] = []any{"review"}
	live := f.call("GET", f.path("/workflows/"+draft.S("id")), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+draft.S("id")), core.Object{
		"graph": configured, "version": live.N("version"),
	}, "", 200)

	refused := f.call("POST", f.path("/workflows/"+draft.S("id")+"/publish"), core.Object{}, "adm-cfg-pub", 400)
	want := []core.Object{
		{"code": "node_config_unsupported", "nodeId": "agent-1", "field": "skills"},
		{"code": "node_config_unsupported", "nodeId": "agent-1", "field": "retry"},
	}
	if got := problems(t, refused); !reflect.DeepEqual(got, want) {
		t.Fatalf("publish problems = %v, want %v", got, want)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", draft.S("id")); n != 0 {
		t.Fatalf("a refused publish froze %d snapshots", n)
	}

	// The same document with only the first phase's configuration publishes: the refusal is about the
	// fields Cloud cannot apply, not about an agent node having a configuration at all.
	live = f.call("GET", f.path("/workflows/"+draft.S("id")), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+draft.S("id")), core.Object{
		"graph": agentRunGraph("", false, nil), "version": live.N("version"),
	}, "", 200)
	f.call("POST", f.path("/workflows/"+draft.S("id")+"/publish"), core.Object{}, "adm-cfg-pub-2", 200)
}

// TestWorkflowRunSnapshotIsScopedToItsWorkflow covers the snapshot a run may name: an id belonging to
// another workflow of the same tenant is the same 404 as an unknown one, so a run can never execute a
// version of a document it was not started against.
func TestWorkflowRunSnapshotIsScopedToItsWorkflow(t *testing.T) {
	f := setup(t)
	first := f.call("POST", f.path("/workflows"), core.Object{"name": "First flow", "graph": startInputGraph(nil)}, "adm-scope-a", 200).O("resource")
	second := f.call("POST", f.path("/workflows"), core.Object{"name": "Second flow", "graph": startInputGraph(nil)}, "adm-scope-b", 200).O("resource")
	mine := publishWorkflow(t, f, first.S("id"), "adm-scope-pub-a")
	publishWorkflow(t, f, second.S("id"), "adm-scope-pub-b")

	// The workflow's own snapshot runs.
	run := f.call("POST", f.path("/workflows/"+first.S("id")+"/runs"), core.Object{"snapshotId": mine.S("id"), "projectId": runProject(t, f, "adm-scope-project")}, "adm-scope-1", 200).O("resource")
	if run.S("snapshotId") != mine.S("id") {
		t.Fatalf("the run did not execute the version it named: %v", run)
	}
	// Another workflow's version, and an id that names nothing, are both 404 — never a silent fallback
	// to the newest version.
	other := issueItems(f.call("GET", f.path("/workflows/"+second.S("id")+"/snapshots"), nil, "", 200))[0]
	f.call("POST", f.path("/workflows/"+first.S("id")+"/runs"), core.Object{"snapshotId": other.S("id")}, "adm-scope-2", 404)
	f.call("POST", f.path("/workflows/"+first.S("id")+"/runs"), core.Object{"snapshotId": uuid.NewString()}, "adm-scope-3", 404)
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", first.S("id")); n != 1 {
		t.Fatalf("a refused start left %d runs behind", n)
	}
}

// TestWorkflowRunValidatesAgainstTheVersionItRuns covers the reason the gate reads a snapshot rather
// than the live document: a version published earlier keeps accepting exactly what it accepted then,
// and the newest published version is what an unqualified run gets.
func TestWorkflowRunValidatesAgainstTheVersionItRuns(t *testing.T) {
	f := setup(t)
	pid := runProject(t, f, "adm-hist-project")
	graph := func(names ...string) core.Object {
		variables := []any{}
		for _, name := range names {
			variables = append(variables, core.Object{
				"name": name, "displayName": name, "fieldType": "text-input", "valueType": "string", "required": true,
			})
		}
		return startInputGraph(variables)
	}
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Growing flow", "graph": graph("topic"),
	}, "adm-hist-wf", 200).O("resource")
	older := publishWorkflow(t, f, wf.S("id"), "adm-hist-pub-1")
	// The author adds a required variable and publishes again; the live document is now the newer one.
	live := f.call("GET", f.path("/workflows/"+wf.S("id")), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+wf.S("id")), core.Object{
		"graph": graph("topic", "added-later"), "version": live.N("version"),
	}, "", 200)
	newer := publishWorkflow(t, f, wf.S("id"), "adm-hist-pub-2")

	// The older version does not demand a variable that did not exist when it was frozen...
	f.call("POST", f.path("/workflows/"+wf.S("id")+"/runs"), core.Object{
		"snapshotId": older.S("id"), "input": core.Object{"topic": "auth"}, "projectId": pid,
	}, "adm-hist-1", 200)
	// ...and still enforces its own required variable.
	f.call("POST", f.path("/workflows/"+wf.S("id")+"/runs"), core.Object{
		"snapshotId": older.S("id"), "input": core.Object{},
	}, "adm-hist-2", 400)
	// A run that names no version gets the newest published one, so it owes the newer schema.
	missing := f.call("POST", f.path("/workflows/"+wf.S("id")+"/runs"), core.Object{
		"input": core.Object{"topic": "auth"},
	}, "adm-hist-3", 400)
	if missing.O("params").S("field") != "added-later" {
		t.Fatalf("the newest version was not the one applied: %v", missing)
	}
	newest := f.call("POST", f.path("/workflows/"+wf.S("id")+"/runs"), core.Object{
		"snapshotId": newer.S("id"), "input": core.Object{"topic": "auth", "added-later": "x"}, "projectId": pid,
	}, "adm-hist-4", 200).O("resource")

	// The version a run executed is recorded on the run row, not left to be re-derived later.
	var pinned string
	must(t, f.store.Pool.QueryRow("SELECT snapshot_id FROM workflow_runs WHERE id=$1", newest.S("id")).Scan(&pinned))
	if pinned != newer.S("id") {
		t.Fatalf("the run row pinned %q, want %q", pinned, newer.S("id"))
	}
}

// manualWorkflowRun posts a manual IssueRun for a workflow and returns the raw status and body, so a
// case can assert the exact accepted/rejected contract.
func manualWorkflowRun(t *testing.T, f *fixture, iid, executorID, key string, input core.Object) (int, core.Object) {
	t.Helper()
	body := core.Object{"executorType": "workflow", "executorId": executorID}
	if input != nil {
		body["input"] = input
	}
	out, status, e := callRaw(f, "POST", f.path("/issues/"+iid+"/runs"), body, key)
	must(t, e)
	return status, out
}

// TestManualWorkflowRunResolvesItsExecutor covers the manual-run entry: executorType=workflow no
// longer accepts an arbitrary UUID. An id that names nothing is 404, a workflow with nothing published
// has no version to execute, and an archived or foreign workflow is invisible — so this entry cannot
// be used to start a run the other two entries would refuse.
func TestManualWorkflowRunResolvesItsExecutor(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Manual workflow run"}, "adm-manual-issue", 200).O("resource")
	iid := iss.S("id")

	// An id that names no workflow of this tenant, published or not.
	if status, body := manualWorkflowRun(t, f, iid, uuid.NewString(), "adm-manual-1", nil); status != 404 {
		t.Fatalf("unknown executor id: status %d %v", status, body)
	}
	// A live workflow with nothing published: the workflow exists and belongs to the caller, and the
	// actionable fact is that there is no frozen version to run.
	draft := f.call("POST", f.path("/workflows"), core.Object{"name": "Manual draft", "graph": startInputGraph(nil)}, "adm-manual-wf", 200).O("resource")
	if status, body := manualWorkflowRun(t, f, iid, draft.S("id"), "adm-manual-2", nil); status != 400 || body.S("code") != "workflow_no_published_snapshot" {
		t.Fatalf("unpublished workflow: status %d %v", status, body)
	}

	// Published: the run starts and freezes the version it executes onto its own input.
	snap := publishWorkflow(t, f, draft.S("id"), "adm-manual-pub")
	status, body := manualWorkflowRun(t, f, iid, draft.S("id"), "adm-manual-3", nil)
	if status != 200 {
		t.Fatalf("published workflow: status %d %v", status, body)
	}
	run := body.O("resource")
	if run.S("executorType") != "workflow" || run.S("executorId") != draft.S("id") {
		t.Fatalf("manual workflow run has the wrong executor: %v", run)
	}
	if run.O("input").S("workflowSnapshotId") != snap.S("id") {
		t.Fatalf("the executed version was not frozen onto the run input: %v", run.O("input"))
	}
	var stored string
	must(t, f.store.Pool.QueryRow("SELECT input->>'workflowSnapshotId' FROM issue_runs WHERE id=$1", run.S("id")).Scan(&stored))
	if stored != snap.S("id") {
		t.Fatalf("stored input pinned %q, want %q", stored, snap.S("id"))
	}

	// Archiving the workflow hides it from this entry too, exactly as it hides its run history.
	live := f.call("GET", f.path("/workflows/"+draft.S("id")), nil, "", 200)
	f.call("DELETE", f.path("/workflows/"+draft.S("id")), core.Object{"version": live.N("version")}, "adm-manual-del", 200)
	if status, body := manualWorkflowRun(t, f, iid, draft.S("id"), "adm-manual-4", nil); status != 404 {
		t.Fatalf("archived workflow: status %d %v", status, body)
	}
	// The run created before the archive is untouched: archiving hides a workflow, it does not rewrite
	// the history of what already ran.
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", run.S("id")); n != 1 {
		t.Fatalf("archiving the workflow removed an existing run")
	}
}

// TestManualWorkflowRunCannotNameAForeignWorkflow covers the tenant boundary of the same entry: a
// workflow of another tenant is the same 404 as an unknown id, so the manual entry is not a way to
// start work in a tenant the caller only guesses at.
func TestManualWorkflowRunCannotNameAForeignWorkflow(t *testing.T) {
	f := setup(t)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Foreign workflow"}, "adm-foreign-issue", 200).O("resource")

	// Another tenant, whose own member publishes a workflow there. The fixture actor is switched to
	// that member so the setup itself is authorized; only the id crosses back.
	other, e := f.store.Bootstrap(context.Background(), "Other", "corp", "other", "Other")
	must(t, e)
	originalTenant, originalSubject := f.tid, f.user.Subject
	f.tid, f.user.Subject = other.S("tenantId"), "other"
	foreign := f.call("POST", f.path("/workflows"), core.Object{"name": "Foreign flow", "graph": startInputGraph(nil)}, "adm-foreign-wf", 200).O("resource")
	publishWorkflow(t, f, foreign.S("id"), "adm-foreign-pub")
	f.tid, f.user.Subject = originalTenant, originalSubject

	if status, body := manualWorkflowRun(t, f, iss.S("id"), foreign.S("id"), "adm-foreign-1", nil); status != 404 {
		t.Fatalf("a foreign workflow resolved in this tenant: status %d %v", status, body)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("a refused manual run left %d runs behind", n)
	}
}

// TestWorkflowRunDefaultSlotGate covers the executor half of the start gate for the implicit `default`
// slot: the user's personal default model must be complete, each way it can be incomplete is reported
// as itself, and no run is created while it is unresolved.
func TestWorkflowRunDefaultSlotGate(t *testing.T) {
	f := setup(t)
	pid := runProject(t, f, "adm-gate-project")
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Gated flow", "graph": agentRunGraph("", false, nil),
	}, "adm-gate-wf", 200).O("resource")
	wfid := wf.S("id")
	publishWorkflow(t, f, wfid, "adm-gate-pub")
	runPath := f.path("/workflows/" + wfid + "/runs")

	// Nothing configured at all.
	refused := f.call("POST", runPath, core.Object{}, "adm-gate-1", 409)
	gaps := slotGaps(t, refused)
	if len(gaps) != 1 || gaps[0].S("slot") != "default" || gaps[0].S("reason") != "default_model_missing" ||
		!slices.Equal(gapNodes(gaps[0]), []string{"agent-1"}) {
		t.Fatalf("unconfigured default reported as %v", gaps)
	}
	// A refusal is not a run: the gate runs before anything is created.
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wfid); n != 0 {
		t.Fatalf("a refused start created %d runs", n)
	}

	// A connection and a default selection, but no credential: the connection cannot be used yet.
	connection := f.call("POST", "/api/v1/me/model-connections",
		personalModelBody("Personal", fixtureModels(fixtureModelID), true), "adm-gate-connection", 201).O("resource")
	f.call("PUT", "/api/v1/me/model-default", core.Object{
		"connectionId": connection.S("id"), "modelId": fixtureModelID, "version": 0,
	}, "", 200)
	refused = f.call("POST", runPath, core.Object{}, "adm-gate-2", 409)
	if gaps = slotGaps(t, refused); len(gaps) != 1 || gaps[0].S("reason") != "credential_missing" {
		t.Fatalf("a connection without a credential reported as %v", gaps)
	}

	// With a credential the gate is satisfied and the run executes.
	_, err := f.store.PutModelCredential(context.Background(), &f.user, connection.S("id"), connection.N("version"),
		"adm-gate-credential", uuid.NewString(), make([]byte, 32), "fixture-key")
	must(t, err)
	run := f.call("POST", runPath, core.Object{"projectId": pid}, "adm-gate-3", 200).O("resource")
	if run.S("status") != "succeeded" {
		t.Fatalf("the admitted run did not execute: %v", run)
	}

	// Disabling the connection takes it away again, and says so rather than reporting a missing default.
	// An update replaces the whole connection and demands its current version, like every write.
	current := f.call("GET", "/api/v1/me/model-connections/"+connection.S("id"), nil, "", 200)
	disabled := personalModelBody("Personal", fixtureModels(fixtureModelID), false)
	disabled["version"] = current.N("version")
	f.call("PUT", "/api/v1/me/model-connections/"+connection.S("id"), disabled, "", 200)
	refused = f.call("POST", runPath, core.Object{}, "adm-gate-4", 409)
	if gaps = slotGaps(t, refused); len(gaps) != 1 || gaps[0].S("reason") != "connection_unavailable" {
		t.Fatalf("a disabled connection reported as %v", gaps)
	}

	// Re-enabled, but the default still names a model the connection no longer offers.
	replaced := personalModelBody("Personal", fixtureModels("vendor/other-model"), true)
	replaced["version"] = current.N("version") + 1
	f.call("PUT", "/api/v1/me/model-connections/"+connection.S("id"), replaced, "", 200)
	refused = f.call("POST", runPath, core.Object{}, "adm-gate-5", 409)
	if gaps = slotGaps(t, refused); len(gaps) != 1 || gaps[0].S("reason") != "model_unavailable" {
		t.Fatalf("a withdrawn model reported as %v", gaps)
	}
	// Only the one admitted run exists: every refusal above rolled back whole.
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wfid); n != 1 {
		t.Fatalf("the gate admitted %d runs, want 1", n)
	}
}

// TestWorkflowRunNamedSlotGate covers the other half: a slot the author declared is the user's to map,
// and until personal slot mappings exist it is reported as unconfigured — with the author's
// description and hint, which are the only things that tell the user what to map it to. An undeclared
// reference never reaches a run at all: publishing refuses it, because only the author can say what
// the slot means.
func TestWorkflowRunNamedSlotGate(t *testing.T) {
	f := setup(t)
	// The user's default model is complete, so nothing about this refusal is about `default`.
	seedPersonalModel(t, f)

	declared := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Named slot flow",
		"graph": agentRunGraph("strong", false, []any{core.Object{
			"name": "strong", "description": "深度推理",
			"hint": core.Object{"agentCli": "official/ora-space.opencode", "modelId": "vendor/model-v1"},
		}}),
	}, "adm-slot-wf", 200).O("resource")
	publishWorkflow(t, f, declared.S("id"), "adm-slot-pub")

	refused := f.call("POST", f.path("/workflows/"+declared.S("id")+"/runs"), core.Object{}, "adm-slot-1", 409)
	gaps := slotGaps(t, refused)
	if len(gaps) != 1 || gaps[0].S("slot") != "strong" || gaps[0].S("reason") != "slot_not_configured" ||
		!slices.Equal(gapNodes(gaps[0]), []string{"agent-1"}) {
		t.Fatalf("an unmapped named slot reported as %v", gaps)
	}
	// The author's description and hint travel with the refusal so the user can decide what to map.
	if gaps[0].S("description") != "深度推理" || gaps[0].O("hint").S("modelId") != "vendor/model-v1" {
		t.Fatalf("the refusal dropped what the author wrote about the slot: %v", gaps[0])
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", declared.S("id")); n != 0 {
		t.Fatalf("an unmapped slot still produced %d runs", n)
	}

	// A reference to a slot the document never declares is refused at publish, not at run time.
	undeclared := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Undeclared slot flow", "graph": agentRunGraph("strong", false, nil),
	}, "adm-slot-wf-2", 200).O("resource")
	refused = f.call("POST", f.path("/workflows/"+undeclared.S("id")+"/publish"), core.Object{}, "adm-slot-pub-2", 400)
	if codes := problemCodes(t, refused); !slices.Equal(codes, []string{"slot_not_declared"}) {
		t.Fatalf("publish problems = %v", codes)
	}
	if problem := problems(t, refused)[0]; problem.S("slot") != "strong" || problem.S("nodeId") != "agent-1" {
		t.Fatalf("the refusal does not name the slot and the node: %v", problem)
	}
}

// TestWorkflowPublishConcurrentVersions covers publishing under concurrency: the workflow row is
// locked for the transaction, so callers racing to publish get consecutive versions rather than
// sharing one. The assertion is on the complete set of versions, never on which caller won.
func TestWorkflowPublishConcurrentVersions(t *testing.T) {
	f := setup(t)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Racing flow", "graph": startInputGraph(nil),
	}, "adm-race-wf", 200).O("resource")
	before := wf.N("version")

	const callers = 4
	start := make(chan struct{})
	bodies := make([]core.Object, callers)
	statuses := make([]int, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bodies[i], statuses[i], errs[i] = callRaw(f, "POST", f.path("/workflows/"+wf.S("id")+"/publish"),
				core.Object{}, "adm-race-"+uuid.NewString())
		}(i)
	}
	close(start)
	wg.Wait()

	versions := []int{}
	for i := range callers {
		must(t, errs[i])
		if statuses[i] != 200 {
			t.Fatalf("caller %d: status %d %v", i, statuses[i], bodies[i])
		}
		versions = append(versions, int(bodies[i].O("resource").N("version")))
	}
	slices.Sort(versions)
	if !slices.Equal(versions, []int{1, 2, 3, 4}) {
		t.Fatalf("concurrent publishes produced versions %v, want 1..4", versions)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", wf.S("id")); n != callers {
		t.Fatalf("concurrent publishes left %d snapshot rows, want %d", n, callers)
	}
	// Publishing freezes; it never edits the live document.
	if live := f.call("GET", f.path("/workflows/"+wf.S("id")), nil, "", 200); live.N("version") != before {
		t.Fatalf("concurrent publish bumped the live version: %v", live)
	}
}
