package integration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

type importedWorkflowEvidence struct {
	f        *fixture
	spaceID  string
	workflow string
	snapshot core.Object
}

// importWorkflowForInvocation reuses the real .orax/HTTPS/marketplace harness. No workflow or
// snapshot is seeded: the plugin's desired state must produce both through the import worker.
func importWorkflowForInvocation(t *testing.T) *importedWorkflowEvidence {
	t.Helper()
	f := setup(t)
	// These tests prove persisted invocation identity, never a simulated node trace.
	f.store.Simulator = nil
	document := startPromptGraph([]any{core.Object{
		"name": "releaseName", "displayName": "Imported release", "fieldType": "text-input",
		"valueType": "string", "required": true, "value": "package-default",
	}}, "Ship the imported release")
	document["name"] = "Imported Release Evidence"
	document["version"] = 47
	document["globalVariables"] = []any{core.Object{"name": "release.mode", "valueType": "string", "value": "safe"}}
	encoded, err := json.Marshal(document)
	must(t, err)
	pkg := workflowPackage(t, map[string]string{"release.json": string(encoded)})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/invocation-1.0.0.orax": pkg})
	repo := workflowMarketplace(t, f.root)
	publishWorkflowRelease(t, repo, "invocation-kit", "1.0.0", packages.server.URL+"/packages/invocation-1.0.0.orax", pkg)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)
	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/invocation-kit", "")
	must(t, f.importWorkflowAssets(t))
	item := f.assetImport(t, sid, "official/invocation-kit")
	if item == nil || item.S("state") != "completed" {
		t.Fatalf("import did not complete: %v", item)
	}
	imported := assetDocuments(item)["assets/workflows/release.json"]
	if imported.S("outcome") != "imported" || imported.S("workflowId") == "" || imported.N("snapshotVersion") != 1 {
		t.Fatalf("document was not imported and published: %v", imported)
	}
	wid := imported.S("workflowId")
	snapshots := issueItems(f.call("GET", f.path("/workflows/"+wid+"/snapshots"), nil, "", 200))
	if len(snapshots) != 1 || snapshots[0].S("id") == "" || snapshots[0].N("version") != 1 {
		t.Fatalf("import must leave exactly one Cloud-assigned snapshot: %v", snapshots)
	}
	var expected core.Object
	must(t, json.Unmarshal(encoded, &expected))
	if !reflect.DeepEqual(snapshots[0].O("graph"), expected) {
		t.Fatalf("published graph differs from the package document: %v", snapshots[0])
	}
	return &importedWorkflowEvidence{f: f, spaceID: sid, workflow: wid, snapshot: snapshots[0]}
}

func importedInvocationInput() core.Object {
	return core.Object{"releaseName": "release-from-package", "release.mode": "strict"}
}

// assertRun checks both the public frozen definition and the real PostgreSQL binding.
// IDs and timestamps differ by invocation; the returned projection names only run semantics.
func (e *importedWorkflowEvidence) assertRun(t *testing.T, user core.Claims, id string, snapshot core.Object) core.Object {
	t.Helper()
	run := callAs(t, e.f, user, "GET", e.f.path("/workflows/"+e.workflow+"/runs/"+id), "", nil, 200)
	if run.S("snapshotId") != snapshot.S("id") || run.S("workflowId") != e.workflow || run.S("status") != "pending" {
		t.Fatalf("run did not bind a pending imported invocation: %v", run)
	}
	if !reflect.DeepEqual(run.O("definitionSnapshot"), snapshot.O("graph")) || !reflect.DeepEqual(run.O("input"), importedInvocationInput()) {
		t.Fatalf("run did not freeze the selected graph and input: %v", run)
	}
	if n := e.f.scalar("SELECT count(*) FROM workflow_runs WHERE id=$1 AND tenant_id=$2 AND workflow_id=$3 AND snapshot_id=$4", id, e.f.tid, e.workflow, snapshot.S("id")); n != 1 {
		t.Fatalf("stored WorkflowRun binding = %d rows, want 1", n)
	}
	return core.Object{
		"workflowId": run.S("workflowId"), "snapshotId": run.S("snapshotId"),
		"status": run.S("status"), "input": run.O("input"), "definitionSnapshot": run.O("definitionSnapshot"),
	}
}

func TestImportedWorkflowDirectoryAndRunsIgnoreImportTriggerUser(t *testing.T) {
	e := importWorkflowForInvocation(t)
	f := e.f
	trigger := f.user
	other, otherID := f.addUser(t, "asset-evidence-admin", "Other asset administrator")
	f.call("PUT", f.path("/members/"+otherID), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Discover imported workflow"}, "asset-directory-issue", 200).O("resource").S("id")
	formPath := f.path("/workflows/" + e.workflow + "/snapshots/" + e.snapshot.S("id") + "/form?issueId=" + iid)
	initialForm := f.call("GET", formPath, nil, "", 200)
	liveFormPath := f.path("/collaboration/forms/" + e.workflow + "?issueId=" + iid)
	if live := f.call("GET", liveFormPath, nil, "", 200); !reflect.DeepEqual(live["fields"], initialForm["fields"]) {
		t.Fatalf("the imported live copy and initial published form disagree: live=%v snapshot=%v", live, initialForm)
	}
	// Distinguish the selected snapshot's form from the mutable live form. This is the same
	// snapshot-form endpoint the @ composer reads when the user selects a published version.
	live := f.call("GET", f.path("/workflows/"+e.workflow), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+e.workflow), core.Object{
		"version": live.N("version"), "graph": startPromptGraph([]any{invocationVariable("replacement")}, "Draft-only prompt"),
	}, "", 200)
	changed := formFields(t, f.call("GET", liveFormPath, nil, "", 200))
	if changed["replacement"] == nil || changed["releaseName"] != nil {
		t.Fatalf("test did not give the live draft a different form: %v", changed)
	}
	project := func(user core.Claims) core.Object {
		t.Helper()
		// This is the public directory used by useCollaborationTargets in the Issue @ picker.
		directory := callAs(t, f, user, "GET", f.path("/collaboration/targets"), "", nil, 200)
		var target core.Object
		for _, item := range issueItems(directory) {
			if item.S("type") == "workflow" && item.S("id") == e.workflow {
				target = item
			}
		}
		if target == nil || target.S("displayName") != "Imported Release Evidence" {
			t.Fatalf("imported copy is absent from the @ directory: %v", directory)
		}
		interaction := target.O("interactionDescriptor")
		if interaction.S("mode") != "form" || interaction.B("requiresTask") || interaction.S("formRef") != e.workflow {
			t.Fatalf("imported target does not advertise its workflow form: %v", target)
		}
		form := callAs(t, f, user, "GET", formPath, "", nil, 200)
		fields := formFields(t, form)
		if form.S("snapshotId") != e.snapshot.S("id") || form.S("formRef") != interaction.S("formRef") ||
			fields["releaseName"].S("label") != "Imported release" || fields["releaseName"].S("defaultValue") != "package-default" ||
			!fields["releaseName"].B("required") || fields["replacement"] != nil ||
			fields["prompt"].S("defaultValue") != "Ship the imported release" || fields["release.mode"].S("defaultValue") != "safe" {
			t.Fatalf("selected form was not projected from the imported snapshot: %v", form)
		}
		if !reflect.DeepEqual(form, initialForm) {
			t.Fatalf("editing the live copy changed its published form: initial=%v selected=%v", initialForm, form)
		}
		workflow := callAs(t, f, user, "GET", f.path("/workflows/"+e.workflow), "", nil, 200)
		return core.Object{"target": target, "form": form, "workflow": workflow}
	}
	start := func(user core.Claims, key string) core.Object {
		t.Helper()
		run := callAs(t, f, user, "POST", f.path("/workflows/"+e.workflow+"/runs"), key, core.Object{
			"snapshotId": e.snapshot.S("id"), "input": importedInvocationInput(),
		}, 200).O("resource")
		return e.assertRun(t, user, run.S("id"), e.snapshot)
	}
	baseline := project(trigger)
	baselineRun := start(trigger, "asset-author-trigger-run")
	if otherProjection, otherRun := project(other), start(other, "asset-author-other-run"); !reflect.DeepEqual(otherProjection, baseline) || !reflect.DeepEqual(otherRun, baselineRun) {
		t.Fatalf("changing administrator changed imported projection/run: projection=%v run=%v", otherProjection, otherRun)
	}
	// Global account disable has no public admin endpoint; change the actual authorization row,
	// then prove the old identity is refused over HTTP before comparing the surviving admin.
	_, err := f.store.Pool.Exec("UPDATE users SET status='disabled' WHERE id=$1", f.uid)
	must(t, err)
	callAs(t, f, trigger, "GET", f.path("/collaboration/targets"), "", nil, 403)
	if afterProjection, afterRun := project(other), start(other, "asset-author-after-disable-run"); !reflect.DeepEqual(afterProjection, baseline) || !reflect.DeepEqual(afterRun, baselineRun) {
		t.Fatalf("deactivating the import trigger changed the retained copy: projection=%v run=%v", afterProjection, afterRun)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1 AND snapshot_id=$2", e.workflow, e.snapshot.S("id")); n != 3 {
		t.Fatalf("expected the same snapshot for all three administrators/account states, got %d", n)
	}
}

func TestImportedWorkflowSnapshotBindsConfirmAndDirectRuns(t *testing.T) {
	e := importWorkflowForInvocation(t)
	f, wid, snapshot := e.f, e.workflow, e.snapshot
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Use imported snapshot"}, "asset-inv-issue", 200).O("resource").S("id")
	interaction := workflowComment(t, f, wid, iid, "asset-inv-comment", "@ imported release")
	values := importedInvocationInput()
	values["repository"], values["branch"], values["snapshotId"] = "https://example.invalid/repo.git", "main", snapshot.S("id")
	issueRun := f.call("POST", confirmPath(f, iid, interaction.S("id")), core.Object{"values": values}, "asset-inv-confirm", 200).O("resource")
	ref := issueRun.S("workflowInvocationRef")
	if ref == "" || issueRun.O("input").S("workflowSnapshotId") != snapshot.S("id") {
		t.Fatalf("Confirm did not link the imported snapshot: %v", issueRun)
	}
	e.assertRun(t, f.user, ref, snapshot)
	if n := f.scalar(`SELECT count(*) FROM issue_interactions ix
		JOIN issue_runs ir ON ir.id=ix.run_id
		JOIN workflow_runs wr ON wr.id=ir.workflow_invocation_ref AND wr.tenant_id=ir.tenant_id
		WHERE ix.id=$1 AND ir.id=$2 AND wr.id=$3 AND wr.snapshot_id=$4`, interaction.S("id"), issueRun.S("id"), ref, snapshot.S("id")); n != 1 {
		t.Fatalf("imported Confirm did not persist the interaction/run pair: %d", n)
	}
	create := func(key string) string {
		t.Helper()
		return f.call("POST", f.path("/workflows/"+wid+"/runs"), core.Object{
			"snapshotId": snapshot.S("id"), "input": importedInvocationInput(),
		}, key, 200).O("resource").S("id")
	}
	first := create("asset-inv-direct")
	e.assertRun(t, f.user, first, snapshot)
	var frozen string
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflow_snapshots WHERE id=$1", snapshot.S("id")).Scan(&frozen))
	live := f.call("GET", f.path("/workflows/"+wid), nil, "", 200)
	draft := startPromptGraph([]any{invocationVariable("replacement")}, "Changed draft prompt")
	f.call("PUT", f.path("/workflows/"+wid), core.Object{"graph": draft, "version": live.N("version")}, "", 200)
	// Direct admission judges the selected snapshot even when the draft has a different schema.
	// Confirm's separate live-form compatibility gate is deliberately not bypassed here.
	second := create("asset-inv-after-edit")
	if first == second || second == ref || first == ref {
		t.Fatal("distinct starts must create distinct WorkflowRun records")
	}
	e.assertRun(t, f.user, second, snapshot)
	e.assertRun(t, f.user, first, snapshot)
	e.assertRun(t, f.user, ref, snapshot)
	var after string
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflow_snapshots WHERE id=$1", snapshot.S("id")).Scan(&after))
	if after != frozen {
		t.Fatal("editing the live draft rewrote the imported snapshot")
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1 AND snapshot_id=$2", wid, snapshot.S("id")); n != 3 {
		t.Fatalf("expected Confirm and two direct starts bound to the imported snapshot, got %d", n)
	}
}
