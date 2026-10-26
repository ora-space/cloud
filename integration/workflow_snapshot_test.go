package integration

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// snapshotGraph is a distinguishable graph so the test can prove snapshots freeze separate
// revisions rather than sharing one row.
func snapshotGraph(marker string) core.Object {
	return core.Object{
		"nodes": []any{core.Object{
			"id": "start", "type": "workflow", "deletable": false,
			"position": core.Object{"x": 0, "y": 0},
			"data": core.Object{
				"kind": "start", "title": "该修订 " + marker, "description": "",
				"inputVariables": []any{core.Object{
					"name": "marker", "displayName": "Marker", "fieldType": "text-input", "valueType": "string",
				}},
			},
		}},
		"edges":    []any{},
		"viewport": core.Object{"x": 0, "y": 0, "zoom": 1},
	}
}

func snapshotItems(o core.Object) []core.Object { return issueItems(o) }

// TestWorkflowSnapshotPublishServices covers publish: each POST freezes the live graph under the
// next per-workflow version, the name defaults to the workflow name, and the row is immutable (a
// second publish is a new row, not an overwrite).
func TestWorkflowSnapshotPublishServices(t *testing.T) {
	f := setup(t)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Versioned flow", "graph": snapshotGraph("v1"),
	}, "snap-wf", 200).O("resource")
	wfid := wf.S("id")
	v1version := wf.N("version")

	// Publish with no body: the snapshot inherits the workflow name.
	v1 := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "snap-1", 200).O("resource")
	if v1.S("workflowId") != wfid || v1.N("version") != 1 || v1.S("name") != "Versioned flow" {
		t.Fatalf("first publish wrong: %v", v1)
	}
	v1graph := v1.O("graph")
	nodes, _ := v1graph["nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["data"].(map[string]any)["title"] != "该修订 v1" {
		t.Fatalf("published graph is not the live graph: %v", v1graph)
	}

	// A second publish is the next version, never an overwrite of version 1.
	v2 := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{
		"name": "Release cut",
	}, "snap-2", 200).O("resource")
	if v2.N("version") != 2 || v2.S("name") != "Release cut" {
		t.Fatalf("second publish wrong: %v", v2)
	}
	if v2.S("id") == v1.S("id") {
		t.Fatal("publish reused the first snapshot row")
	}

	// Publishing never moves the live workflow's own version; it freezes, it does not edit.
	live := f.call("GET", f.path("/workflows/"+wfid), nil, "", 200)
	if live.N("version") != v1version {
		t.Fatalf("publish bumped the live workflow version: %v", live)
	}

	// The graph the second publish froze differs from the first: publish reads the live graph.
	// PUT returns the workflow unwrapped (unlike POST's {resource: ...} envelope).
	edited := f.call("PUT", f.path("/workflows/"+wfid), core.Object{
		"graph": snapshotGraph("v3"), "version": live.N("version"),
	}, "", 200)

	// A name that is not a string or is only whitespace is a 400.
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{"name": "  "}, "snap-3a", 400)
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{"name": 7}, "snap-3b", 400)

	v3 := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "snap-3", 200).O("resource")
	if v3.N("version") != 3 {
		t.Fatalf("third publish version %d != 3", v3.N("version"))
	}
	nodes3, _ := v3.O("graph")["nodes"].([]any)
	if nodes3[0].(map[string]any)["data"].(map[string]any)["title"] != "该修订 v3" {
		t.Fatalf("publish froze a stale graph: %v", v3.O("graph"))
	}
	if edited.N("version") != live.N("version")+1 {
		t.Fatalf("live workflow version unexpected after edit: %v", edited)
	}

	// List is per-workflow and ordered; it returns exactly the published versions.
	list := snapshotItems(f.call("GET", f.path("/workflows/"+wfid+"/snapshots"), nil, "", 200))
	byID := map[string]core.Object{}
	for _, s := range list {
		byID[s.S("id")] = s
	}
	if len(byID) != 3 {
		t.Fatalf("snapshot list has %d rows, want 3: %v", len(byID), list)
	}
	if byID[v1.S("id")].N("version") != 1 || byID[v2.S("id")].N("version") != 2 || byID[v3.S("id")].N("version") != 3 {
		t.Fatalf("snapshot list rows wrong: %v", byID)
	}

	// One snapshot read back whole, by id.
	one := f.call("GET", f.path("/workflows/"+wfid+"/snapshots/"+v2.S("id")), nil, "", 200)
	if one.S("name") != "Release cut" || one.N("version") != 2 {
		t.Fatalf("snapshot read wrong: %v", one)
	}
	f.call("GET", f.path("/workflows/"+wfid+"/snapshots/not-a-uuid"), nil, "", 404)
	f.call("GET", f.path("/workflows/"+wfid+"/snapshots/"+uuid.NewString()), nil, "", 404)

	// An archived workflow hides its whole history: publish, list and read are all 404 once archived.
	f.call("DELETE", f.path("/workflows/"+wfid), core.Object{"version": edited.N("version")}, "snap-del", 200)
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "snap-4", 404)
	f.call("GET", f.path("/workflows/"+wfid+"/snapshots"), nil, "", 404)
	f.call("GET", f.path("/workflows/"+wfid+"/snapshots/"+v1.S("id")), nil, "", 404)
}

// TestWorkflowSnapshotRestore covers restore: the live graph is replaced by the chosen snapshot's,
// the workflow's own version advances like any editing write, and the stale version conflicts.
func TestWorkflowSnapshotRestore(t *testing.T) {
	f := setup(t)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Rollback target", "graph": snapshotGraph("original"),
	}, "rst-wf", 200).O("resource")
	wfid := wf.S("id")

	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "rst-pub-1", 200)
	live := f.call("GET", f.path("/workflows/"+wfid), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+wfid), core.Object{"graph": snapshotGraph("v2"), "version": live.N("version")}, "", 200)
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "rst-pub-2", 200)
	// The list is keyed by row UUID, not version: find the snapshot holding version 1 by scanning.
	v1id := ""
	for _, s := range issueItems(f.call("GET", f.path("/workflows/"+wfid+"/snapshots"), nil, "", 200)) {
		if s.N("version") == 1 {
			v1id = s.S("id")
		}
	}
	if v1id == "" {
		t.Fatal("version-1 snapshot missing")
	}

	// Restore demands a current workflow version, like every write.
	f.call("PUT", f.path("/workflows/"+wfid+"/snapshots/"+v1id+"/restore"), core.Object{}, "", 428)
	f.call("PUT", f.path("/workflows/"+wfid+"/snapshots/"+v1id+"/restore"), core.Object{"version": 1}, "", 409)

	live = f.call("GET", f.path("/workflows/"+wfid), nil, "", 200)
	restored := f.call("PUT", f.path("/workflows/"+wfid+"/snapshots/"+v1id+"/restore"),
		core.Object{"version": live.N("version")}, "", 200)
	if restored.S("id") != wfid {
		t.Fatalf("restore returned the wrong workflow: %s", restored.S("id"))
	}
	if restored.N("version") != live.N("version")+1 {
		t.Fatalf("restore did not advance the live version: %v", restored)
	}
	nodes, _ := restored.O("graph")["nodes"].([]any)
	if nodes[0].(map[string]any)["data"].(map[string]any)["title"] != "该修订 original" {
		t.Fatalf("restore wrote the wrong graph: %v", restored.O("graph"))
	}
	// The version-2 snapshot still exists; restore adds a document revision, it does not rewrite history.
	again := snapshotItems(f.call("GET", f.path("/workflows/"+wfid+"/snapshots"), nil, "", 200))
	if len(again) != 2 {
		t.Fatalf("restore touched the snapshot history: %v", again)
	}

	// A snapshot from another workflow is not restorable here (the read is workflow-scoped).
	other := f.call("POST", f.path("/workflows"), core.Object{"name": "Other flow", "graph": snapshotGraph("other")}, "rst-other", 200).O("resource")
	f.call("POST", f.path("/workflows/"+other.S("id")+"/publish"), core.Object{}, "rst-op", 200)
	otherSnap := snapshotItems(f.call("GET", f.path("/workflows/"+other.S("id")+"/snapshots"), nil, "", 200))[0]
	f.call("PUT", f.path("/workflows/"+wfid+"/snapshots/"+otherSnap.S("id")+"/restore"),
		core.Object{"version": restored.N("version")}, "", 404)
}

// TestWorkflowSnapshotTenantIsolation covers cross-tenant publish, list, read and restore: every
// snapshot operation is invisible outside its tenant.
func TestWorkflowSnapshotTenantIsolation(t *testing.T) {
	f := setup(t)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Isolated flow", "graph": snapshotGraph("base"),
	}, "iso-wf", 200).O("resource")
	wfid := wf.S("id")
	snap := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "iso-1", 200).O("resource")

	other, e := f.store.Bootstrap(context.Background(), "Other", "corp", "other", "Other")
	must(t, e)
	original := f.tid
	f.tid = other.S("tenantId")
	// The fixture actor is not a member of the other tenant, so every snapshot operation is a
	// typed 403 — a cross-tenant workflow is neither readable nor writable.
	f.call("GET", f.path("/workflows/"+wfid+"/snapshots"), nil, "", 403)
	f.call("GET", f.path("/workflows/"+wfid+"/snapshots/"+snap.S("id")), nil, "", 403)
	f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "iso-2", 403)
	f.call("PUT", f.path("/workflows/"+wfid+"/snapshots/"+snap.S("id")+"/restore"), core.Object{"version": 1}, "", 403)
	f.tid = original
}

// TestWorkflowSnapshotPublishNormalizesFileGlobals covers the publish-time rewrite of file and
// array[file] globals: a declaration whose value is a legacy path string freezes with the canonical
// {kind: "workspace_file", path} reference, so a condition reading the snapshot sees the same shape
// desktop's into_global_variables produces. The live workflow document keeps the authored string —
// only the frozen snapshot is normalized.
func TestWorkflowSnapshotPublishNormalizesFileGlobals(t *testing.T) {
	f := setup(t)
	graph := core.Object{
		"nodes": []any{core.Object{
			"id": "start", "type": "workflow", "deletable": false,
			"position": core.Object{"x": 0, "y": 0},
			"data":     core.Object{"kind": "start", "title": "开始", "description": ""},
		}},
		"edges": []any{},
		"globalVariables": []any{
			core.Object{"name": "f.report", "valueType": "file", "value": "docs/report.md"},
			core.Object{"name": "f.reports", "valueType": "array[file]", "value": []any{"docs/a.md", "docs/b.md"}},
		},
	}
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "File globals", "graph": graph,
	}, "snap-file", 200).O("resource")
	wfid := wf.S("id")

	snap := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "snap-file-pub", 200).O("resource")
	globals, _ := snap.O("graph")["globalVariables"].([]any)
	if len(globals) != 2 {
		t.Fatalf("snapshot froze %d globals, want 2: %v", len(globals), globals)
	}
	report := globals[0].(map[string]any)["value"].(map[string]any)
	if report["kind"] != "workspace_file" || report["path"] != "docs/report.md" {
		t.Fatalf("file global value = %v; want {kind: workspace_file, path: docs/report.md}", report)
	}
	reports := globals[1].(map[string]any)["value"].([]any)
	wantPaths := []string{"docs/a.md", "docs/b.md"}
	if len(reports) != len(wantPaths) {
		t.Fatalf("array[file] global froze %d values, want %d: %v", len(reports), len(wantPaths), reports)
	}
	for i, want := range wantPaths {
		ref := reports[i].(map[string]any)
		if ref["kind"] != "workspace_file" || ref["path"] != want {
			t.Fatalf("array[file] global value[%d] = %v; want {kind: workspace_file, path: %s}", i, ref, want)
		}
	}

	// The live workflow document keeps the authored legacy string; publishing rewrites only the
	// snapshot it freezes, never the document the author keeps editing.
	live := f.call("GET", f.path("/workflows/"+wfid), nil, "", 200)
	liveGlobals, _ := live.O("graph")["globalVariables"].([]any)
	if got := liveGlobals[0].(map[string]any)["value"]; got != "docs/report.md" {
		t.Fatalf("live graph value = %v; the authored string must not be rewritten", got)
	}
}
