package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestImportedWorkflowRemainsPublishableAndRunnableAfterUninstall proves an imported document is
// retained user data: removing its plugin neither removes its history nor prevents a new invocation.
func TestImportedWorkflowRemainsPublishableAndRunnableAfterUninstall(t *testing.T) {
	e := importWorkflowForInvocation(t)
	f, wid, old := e.f, e.workflow, e.snapshot
	readOldSnapshot := func() (string, string) {
		t.Helper()
		var graph, row string
		must(t, f.store.Pool.QueryRow(`SELECT graph::text, row_to_json(s)::text FROM workflow_snapshots s
			WHERE id=$1 AND tenant_id=$2 AND workflow_id=$3`, old.S("id"), f.tid, wid).Scan(&graph, &row))
		return graph, row
	}
	frozenGraph, frozenRow := readOldSnapshot()
	plugin := f.spacePlugin(e.spaceID, "official/invocation-kit")
	if plugin == nil {
		t.Fatal("imported workflow has no installed plugin selection to remove")
	}
	f.call("DELETE", f.pluginSpacePath(e.spaceID)+"/plugins", core.Object{
		"identifier": "official/invocation-kit", "version": plugin.N("version"),
	}, "asset-retained-uninstall", 200)
	if removed := f.spacePlugin(e.spaceID, "official/invocation-kit"); removed == nil || removed.S("desiredState") != "removed" {
		t.Fatalf("plugin uninstall did not persist its removed intent: %v", removed)
	}
	if retained := f.call("GET", f.path("/workflows/"+wid), nil, "", 200); retained.S("id") != wid {
		t.Fatalf("uninstall did not retain the imported workflow: %v", retained)
	}
	if graph, row := readOldSnapshot(); graph != frozenGraph || row != frozenRow {
		t.Fatal("uninstall changed the imported snapshot row or graph")
	}

	newSnapshot := publishWorkflow(t, f, wid, "asset-retained-publish")
	if newSnapshot.S("id") == "" || newSnapshot.S("id") == old.S("id") || newSnapshot.N("version") != old.N("version")+1 {
		t.Fatalf("retained workflow did not publish its next distinct snapshot: old=%v new=%v", old, newSnapshot)
	}
	run := f.call("POST", f.path("/workflows/"+wid+"/runs"), core.Object{
		"snapshotId": newSnapshot.S("id"), "input": importedInvocationInput(),
	}, "asset-retained-run", 200).O("resource")
	e.assertRun(t, f.user, run.S("id"), newSnapshot)
	if graph, row := readOldSnapshot(); graph != frozenGraph || row != frozenRow {
		t.Fatal("publishing or running the retained workflow changed its old snapshot")
	}
	snapshots := issueItems(f.call("GET", f.path("/workflows/"+wid+"/snapshots"), nil, "", 200))
	seen := map[string]int64{}
	for _, snapshot := range snapshots {
		seen[snapshot.S("id")] = snapshot.N("version")
	}
	if len(snapshots) != 2 || seen[old.S("id")] != old.N("version") || seen[newSnapshot.S("id")] != newSnapshot.N("version") {
		t.Fatalf("retained workflow lost its old or new snapshot: %v", snapshots)
	}
}
