package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// TestWorkflowAssetImportDoesNotFanOutToWorkspaces covers the fan-out boundary of
// specs test-cases/cloud/plugin-marketplace/workflow-plugin-assets.md: the import is
// triggered by the space's desired state and stays scoped to it. The zero-Workspace case
// is already covered by TestWorkflowAssetImportFromInstalledPlugin; this is the missing
// complement — with several live Workspaces in the space, the Node-side install still
// fans out (one workspace_plugin_instances row per live runtime), but the import must not
// follow it: one source row, one workflow and one published snapshot per document.
func TestWorkflowAssetImportDoesNotFanOutToWorkspaces(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	pkg := workflowPackage(t, map[string]string{
		"release.json": executableDocument("Release Kit", ""),
		"deploy.json":  executableDocument("Deploy Kit", ""),
	})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/fanout-1.0.0.orax": pkg})
	publishWorkflowRelease(t, repo, "fanout-kit", "1.0.0", packages.server.URL+"/packages/fanout-1.0.0.orax", pkg)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	// Several live Workspaces in the one space: the project's main Workspace plus two
	// isolated ones, all ready before the install.
	created := f.create("fanout-project")
	f.drain()
	pid := created.O("resource").S("id")
	for _, target := range []struct{ title, key string }{{"Fanout One", "fanout-ws-one"}, {"Fanout Two", "fanout-ws-two"}} {
		out := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": target.title, "baseRef": "main"}, target.key, 202)
		f.drain()
		wid := out.O("resource").S("id")
		if state := f.ws(wid).S("observedState"); state != "ready" {
			t.Fatalf("workspace %s must be live before the install, got %q", wid, state)
		}
	}

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/fanout-kit", "")
	f.drain()
	// The Node-side install fans out to every live runtime; the import must not follow it.
	liveWorkspaces := f.scalar("SELECT count(*) FROM workspaces WHERE deleted_at IS NULL")
	if liveWorkspaces < 2 {
		t.Fatalf("the fixture expected at least two live workspaces, got %d", liveWorkspaces)
	}
	if n := f.scalar("SELECT count(*) FROM workspace_plugin_instances WHERE identifier='fanout-kit'"); n != liveWorkspaces {
		t.Fatalf("the install must fan out to all %d live workspaces, got %d instances", liveWorkspaces, n)
	}

	must(t, f.importWorkflowAssets(t))
	item := f.assetImport(t, sid, "official/fanout-kit")
	if item == nil || item.S("state") != "completed" || item.S("packageVersion") != "1.0.0" {
		t.Fatalf("package import = %v", item)
	}
	if documents := assetDocuments(item); len(documents) != 2 {
		t.Fatalf("documents = %v", documents)
	}
	// One source row for the whole space, not one per workspace.
	if n := f.scalar("SELECT count(*) FROM workflow_asset_imports WHERE identifier='fanout-kit'"); n != 1 {
		t.Fatalf("the import fanned out to workspaces: %d source rows", n)
	}
	// One workflow and one published snapshot per document, despite several live runtimes.
	if n := f.scalar("SELECT count(*) FROM workflow_asset_documents WHERE identifier='fanout-kit'"); n != 2 {
		t.Fatalf("per-workspace document results appeared: %d rows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflows WHERE deleted_at IS NULL"); n != 2 {
		t.Fatalf("workflows were created per workspace: %d rows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != 2 {
		t.Fatalf("snapshots were published per workspace: %d rows", n)
	}
	if live := f.liveWorkflows(t); len(live) != 2 || live["Release Kit"] == nil || live["Deploy Kit"] == nil {
		t.Fatalf("imported workflows = %v", live)
	}

	// A restart-equivalent second pass still creates nothing new.
	must(t, f.importWorkflowAssets(t))
	if n := f.scalar("SELECT count(*) FROM workflow_asset_documents"); n != 2 {
		t.Fatalf("second pass changed document results: %d rows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflows"); n != 2 {
		t.Fatalf("second pass created workflows: %d rows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != 2 {
		t.Fatalf("second pass published snapshots: %d rows", n)
	}
}

// TestWorkflowAssetImportKeepsUserEditsAcrossUpgrade covers the copy-ownership boundary of
// specs test-cases/cloud/plugin-marketplace/workflow-plugin-assets.md: once a document has
// imported, the copy is user data. After the user edits the imported workflow and publishes
// their own snapshot, a package upgrade imports only paths that never succeeded; the user's
// live graph and every published snapshot keep their exact stored text, and the upgraded
// document's new content appears nowhere.
func TestWorkflowAssetImportKeepsUserEditsAcrossUpgrade(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	v1 := workflowPackage(t, map[string]string{
		"release.json": executableDocument("Edit Kit", `"origin":"package-1.0.0"`),
		"deploy.json":  executableDocument("Deploy Kit", ""),
	})
	v2 := workflowPackage(t, map[string]string{
		"release.json": executableDocument("Edit Kit", `"origin":"package-1.1.0"`),
		"deploy.json":  executableDocument("Deploy Kit", ""),
		"hotfix.json":  executableDocument("Hotfix Kit", ""),
	})
	packages := newWorkflowPackages(t, map[string][]byte{
		"/packages/edit-kit-1.0.0.orax": v1,
		"/packages/edit-kit-1.1.0.orax": v2,
	})
	publishWorkflowRelease(t, repo, "edit-kit", "1.0.0", packages.server.URL+"/packages/edit-kit-1.0.0.orax", v1)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/edit-kit", "")
	must(t, f.importWorkflowAssets(t))
	documents := assetDocuments(f.assetImport(t, sid, "official/edit-kit"))
	editID := documents["assets/workflows/release.json"].S("workflowId")
	if editID == "" || documents["assets/workflows/deploy.json"].S("workflowId") == "" {
		t.Fatalf("documents were not imported: %v", documents)
	}

	// The user edits the imported copy and publishes their own second snapshot.
	edited := f.liveWorkflows(t)["Edit Kit"]
	editedGraph := core.Object{"nodes": []any{map[string]any{"id": "user-start", "data": map[string]any{"kind": "start"}}}, "edges": []any{}}
	f.call("PUT", f.path("/workflows/"+editID), core.Object{"graph": editedGraph, "version": edited.N("version")}, "edit-user-edit", 200)
	f.call("POST", f.path("/workflows/"+editID+"/publish"), core.Object{}, "edit-user-publish", 200)
	snapshotText := func(version int64) string {
		t.Helper()
		var text string
		must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflow_snapshots WHERE workflow_id=$1 AND version=$2", editID, version).Scan(&text))
		return text
	}
	liveText := func() string {
		t.Helper()
		var text string
		must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflows WHERE id=$1", editID).Scan(&text))
		return text
	}
	liveBefore, importSnapshotBefore, userSnapshotBefore := liveText(), snapshotText(1), snapshotText(2)

	// Upgrade the package: the already-imported path now carries different content.
	publishWorkflowRelease(t, repo, "edit-kit", "1.1.0", packages.server.URL+"/packages/edit-kit-1.1.0.orax", v2)
	f.syncMarketplace(t, repo)
	f.installWorkflowPlugin(t, sid, "official/edit-kit", "1.1.0")
	must(t, f.importWorkflowAssets(t))

	documents = assetDocuments(f.assetImport(t, sid, "official/edit-kit"))
	if len(documents) != 3 || documents["assets/workflows/hotfix.json"].S("outcome") != "imported" {
		t.Fatalf("the upgrade must import only the new path: %v", documents)
	}
	// The imported path keeps pointing at the copy the user now owns.
	if documents["assets/workflows/release.json"].S("workflowId") != editID {
		t.Fatalf("the upgrade revisited an imported path: %v", documents["assets/workflows/release.json"])
	}
	if live := f.liveWorkflows(t); len(live) != 3 || live["Hotfix Kit"] == nil {
		t.Fatalf("workflow library after upgrade = %v", live)
	}
	// The user's live graph and both published snapshots keep their exact stored text.
	if after := liveText(); after != liveBefore {
		t.Fatalf("the upgrade overwrote the user's edit: %s -> %s", liveBefore, after)
	}
	if after := snapshotText(1); after != importSnapshotBefore {
		t.Fatalf("the upgrade rewrote the imported snapshot: %s -> %s", importSnapshotBefore, after)
	}
	if after := snapshotText(2); after != userSnapshotBefore {
		t.Fatalf("the upgrade rewrote the user's snapshot: %s -> %s", userSnapshotBefore, after)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", editID); n != 2 {
		t.Fatalf("the upgrade must not add snapshots: %d rows", n)
	}
	// The upgraded document's new content appears nowhere.
	if n := f.scalar("SELECT count(*) FROM workflows WHERE graph::text LIKE '%package-1.1.0%'"); n != 0 {
		t.Fatalf("package upgrade content leaked into a live graph: %d rows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE graph::text LIKE '%package-1.1.0%'"); n != 0 {
		t.Fatalf("package upgrade content leaked into a snapshot: %d rows", n)
	}
	// The untouched sibling keeps exactly its own one snapshot.
	deployID := documents["assets/workflows/deploy.json"].S("workflowId")
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", deployID); n != 1 {
		t.Fatalf("the sibling's snapshots changed: %d rows", n)
	}
}
