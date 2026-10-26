package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// workflowPackage builds a plugin package whose assets/workflows directory holds the given
// documents, keyed by file name.
func workflowPackage(t *testing.T, documents map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range documents {
		file, err := writer.Create("assets/workflows/" + name)
		must(t, err)
		_, err = file.Write([]byte(content))
		must(t, err)
	}
	must(t, writer.Close())
	return buffer.Bytes()
}

// executableDocument is a document the publish gate accepts: exactly one start node.
func executableDocument(name, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return `{"name":"` + name + `","nodes":[{"id":"start","data":{"kind":"start"}}],"edges":[]` + extra + `}`
}

// workflowPackages serves package bytes by request path. Tests replace a map entry to model a
// publisher repairing a broken release.
type workflowPackages struct {
	server *httptest.Server
	mu     sync.Mutex
	files  map[string][]byte
}

func newWorkflowPackages(t *testing.T, files map[string][]byte) *workflowPackages {
	t.Helper()
	packages := &workflowPackages{files: files}
	packages.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		packages.mu.Lock()
		body, ok := packages.files[r.URL.Path]
		packages.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(packages.server.Close)
	return packages
}

func (p *workflowPackages) replace(path string, body []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.files[path] = body
}

// workflowMarketplace creates an empty local marketplace repository the real syncer clones.
func workflowMarketplace(t *testing.T, root string) string {
	t.Helper()
	repo := filepath.Join(root, "workflow-marketplace")
	must(t, os.MkdirAll(repo, 0o700))
	runGit(t, "init", "--initial-branch=main", repo)
	runGit(t, "-C", repo, "config", "user.name", "Marketplace")
	runGit(t, "-C", repo, "config", "user.email", "marketplace@example.invalid")
	return repo
}

// publishWorkflowRelease commits one kind='workflow' catalog listing whose universal release is
// the given package and URL, returning the digest it records.
func publishWorkflowRelease(t *testing.T, repo, identifier, version, url string, pkg []byte) string {
	t.Helper()
	digest := sha256.Sum256(pkg)
	listing := "resolver = 1\nidentifier = \"" + identifier + "\"\ntitle = \"" + identifier + "\"\nkind = \"workflow\"\nversion = \"" + version + "\"\ndescription = \"Workflow templates.\"\nurl = \"" + url + "\"\nsha256 = \"" + hex.EncodeToString(digest[:]) + "\"\n"
	path := filepath.Join(repo, "registry", "w", identifier, "orax.toml")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(listing), 0o600))
	runGit(t, "-C", repo, "add", ".")
	runGit(t, "-C", repo, "commit", "-m", identifier+" "+version)
	return hex.EncodeToString(digest[:])
}

func (f *fixture) installWorkflowPlugin(t *testing.T, sid, pluginID, pluginVersion string) core.Object {
	t.Helper()
	body := core.Object{"identifier": pluginID}
	if pluginVersion != "" {
		body["pluginVersion"] = pluginVersion
	}
	if existing := f.spacePlugin(sid, pluginID); existing != nil {
		body["version"] = existing.N("version")
	}
	return f.call("POST", f.pluginSpacePath(sid)+"/plugins", body, "asset-install-"+uuid.NewString(), 200)
}

func (f *fixture) importWorkflowAssets(t *testing.T) error {
	t.Helper()
	return f.store.ImportWorkflowAssetsOnce(context.Background())
}

// assetImport returns the plugin's asset-import item from the independent read API.
func (f *fixture) assetImport(t *testing.T, sid, pluginID string) core.Object {
	t.Helper()
	out := f.call("GET", f.pluginSpacePath(sid)+"/plugins/assets", nil, "", 200)
	for _, raw := range out["items"].([]any) {
		item := core.Object(raw.(map[string]any))
		if item.S("pluginId") == pluginID {
			return item
		}
	}
	return nil
}

func assetDocuments(item core.Object) map[string]core.Object {
	documents := map[string]core.Object{}
	for _, raw := range item["documents"].([]any) {
		document := core.Object(raw.(map[string]any))
		documents[document.S("documentPath")] = document
	}
	return documents
}

// liveWorkflows indexes the tenant's live workflow library by name.
func (f *fixture) liveWorkflows(t *testing.T) map[string]core.Object {
	t.Helper()
	out := f.call("GET", f.path("/workflows"), nil, "", 200)
	workflows := map[string]core.Object{}
	for _, raw := range out["items"].([]any) {
		workflow := core.Object(raw.(map[string]any))
		workflows[workflow.S("name")] = workflow
	}
	return workflows
}

// TestWorkflowAssetImportFromInstalledPlugin covers the approved Cloud ADR: the space's desired
// state alone triggers download, extraction and publishing, without any Node installation; the four
// document outcomes are independent; imported copies are idempotent across triggers and additive
// across upgrades; and archiving or uninstalling never resurrects or removes them.
func TestWorkflowAssetImportFromInstalledPlugin(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	v1 := workflowPackage(t, map[string]string{
		// The document's self-declared version must not influence the snapshot version.
		"release.json": executableDocument("Release Kit", `"version":7`),
		"deploy.json":  executableDocument("Deploy Kit", ""),
	})
	packages := newWorkflowPackages(t, map[string][]byte{
		"/packages/release-kit-1.0.0.orax": v1,
		"/packages/release-kit-1.1.0.orax": {},
	})
	publishWorkflowRelease(t, repo, "release-kit", "1.0.0", packages.server.URL+"/packages/release-kit-1.0.0.orax", v1)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	// No project and no workspace: the import must not depend on a live runtime.
	f.installWorkflowPlugin(t, sid, "official/release-kit", "")
	if n := f.scalar("SELECT count(*) FROM workspace_plugin_instances"); n != 0 {
		t.Fatalf("install must not fan out without a live workspace: %d instances", n)
	}
	if n := f.scalar("SELECT count(*) FROM operations WHERE kind IN ('install_plugin','remove_plugin')"); n != 0 {
		t.Fatalf("asset import must not create plugin operations: %d", n)
	}

	must(t, f.importWorkflowAssets(t))
	item := f.assetImport(t, sid, "official/release-kit")
	if item == nil || item.S("state") != "completed" || item.S("packageVersion") != "1.0.0" {
		t.Fatalf("package import = %v", item)
	}
	documents := assetDocuments(item)
	if len(documents) != 2 {
		t.Fatalf("documents = %v", documents)
	}
	for path, document := range documents {
		if document.S("outcome") != "imported" || document.S("workflowId") == "" || document.N("snapshotVersion") != 1 {
			t.Fatalf("%s outcome = %v", path, document)
		}
	}
	workflows := f.liveWorkflows(t)
	if len(workflows) != 2 || workflows["Release Kit"] == nil || workflows["Deploy Kit"] == nil {
		t.Fatalf("imported workflows = %v", workflows)
	}
	releaseID := documents["assets/workflows/release.json"].S("workflowId")
	if workflows["Release Kit"].S("id") != releaseID {
		t.Fatalf("import must create the workflow the document row names")
	}
	snapshots := f.call("GET", f.path("/workflows/"+releaseID+"/snapshots"), nil, "", 200)
	if len(snapshots["items"].([]any)) != 1 {
		t.Fatalf("import must publish exactly one snapshot: %v", snapshots)
	}
	published := core.Object(snapshots["items"].([]any)[0].(map[string]any))
	if published.S("name") != "Release Kit" || len(published.O("graph")) == 0 {
		t.Fatalf("published snapshot = %v", published)
	}
	// Cloud assigns the version; the document's own "version":7 plays no part.
	if published.N("version") != 1 {
		t.Fatalf("snapshot version = %v, want Cloud's first integer", published.N("version"))
	}
	// Upgrading must leave an already imported path's live graph and frozen snapshot untouched. The
	// stored jsonb text is what a reader gets back, so byte equality here rules out re-encoding too.
	var releaseGraph, releaseSnapshotGraph string
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflows WHERE id=$1", releaseID).Scan(&releaseGraph))
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflow_snapshots WHERE workflow_id=$1 AND version=1", releaseID).Scan(&releaseSnapshotGraph))
	// Imports must not introduce an author or publisher: the copy outlives any administrator.
	if n := f.scalar(`SELECT count(*) FROM information_schema.columns
		WHERE table_name IN ('workflows','workflow_snapshots')
		  AND column_name IN ('author','author_id','creator','creator_id','publisher','publisher_id')`); n != 0 {
		t.Fatalf("imported workflow tables gained an author column: %d", n)
	}
	// The trigger is recorded only on the source row, scoped to the space whose desired state caused it.
	if n := f.scalar("SELECT count(*) FROM workflow_asset_imports WHERE space_id=$1 AND tenant_id=$2 AND attempted_at IS NOT NULL", sid, f.tid); n != 1 {
		t.Fatalf("import source record = %d rows, want the triggering space", n)
	}
	// No runtime exists in this space, so no Node installation ever happened; the templates exist
	// anyway. This is the whole point of triggering from desired state rather than from install
	// evidence.
	if n := f.scalar("SELECT count(*) FROM workspaces WHERE deleted_at IS NULL"); n != 0 {
		t.Fatalf("asset import test must not provision a runtime: %d workspaces", n)
	}

	// A second trigger, a restart-equivalent pass and an expired lease all converge on the same
	// imported set without creating anything new.
	snapshotCount := f.scalar("SELECT count(*) FROM workflow_snapshots")
	must(t, f.importWorkflowAssets(t))
	_, err := f.store.Pool.Exec(`UPDATE workflow_asset_imports SET state='importing',attempted_revision=requested_revision,lease_until=now()-interval '1 minute' WHERE space_id=$1`, sid)
	must(t, err)
	must(t, f.importWorkflowAssets(t))
	if n := f.scalar("SELECT count(*) FROM workflows"); n != 2 {
		t.Fatalf("repeated triggers re-imported documents: %d workflows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != snapshotCount {
		t.Fatalf("repeated triggers republished snapshots: %d -> %d", snapshotCount, n)
	}

	// Upgrade: a reinstall of a newer version imports only paths that never succeeded.
	v11 := workflowPackage(t, map[string]string{
		"release.json": executableDocument("Release Kit", `"changed":true`),
		"deploy.json":  executableDocument("Deploy Kit", ""),
		"hotfix.json":  executableDocument("Hotfix Kit", ""),
	})
	packages.replace("/packages/release-kit-1.1.0.orax", v11)
	publishWorkflowRelease(t, repo, "release-kit", "1.1.0", packages.server.URL+"/packages/release-kit-1.1.0.orax", v11)
	f.syncMarketplace(t, repo)
	f.installWorkflowPlugin(t, sid, "official/release-kit", "1.1.0")
	must(t, f.importWorkflowAssets(t))
	item = f.assetImport(t, sid, "official/release-kit")
	documents = assetDocuments(item)
	if len(documents) != 3 {
		t.Fatalf("upgrade documents = %v", documents)
	}
	if documents["assets/workflows/release.json"].S("workflowId") != releaseID {
		t.Fatalf("an imported path must not be revisited across versions: %v", documents["assets/workflows/release.json"])
	}
	workflows = f.liveWorkflows(t)
	if len(workflows) != 3 || workflows["Hotfix Kit"] == nil {
		t.Fatalf("upgrade must add the new document only: %v", workflows)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots WHERE workflow_id=$1", releaseID); n != 1 {
		t.Fatalf("upgrade republished an imported path: %d snapshots", n)
	}
	var upgradedGraph, upgradedSnapshotGraph string
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflows WHERE id=$1", releaseID).Scan(&upgradedGraph))
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflow_snapshots WHERE workflow_id=$1 AND version=1", releaseID).Scan(&upgradedSnapshotGraph))
	if upgradedGraph != releaseGraph || upgradedSnapshotGraph != releaseSnapshotGraph {
		t.Fatalf("an upgrade rewrote an imported path: workflow changed=%v snapshot changed=%v",
			upgradedGraph != releaseGraph, upgradedSnapshotGraph != releaseSnapshotGraph)
	}

	// Archiving an imported workflow frees its name but never re-imports the path.
	release := workflows["Release Kit"]
	f.call("DELETE", f.path("/workflows/"+release.S("id")), core.Object{"version": release.N("version")}, "archive-release", 200)
	f.installWorkflowPlugin(t, sid, "official/release-kit", "1.1.0")
	must(t, f.importWorkflowAssets(t))
	if live := f.liveWorkflows(t); live["Release Kit"] != nil {
		t.Fatalf("an archived imported document was resurrected: %v", live["Release Kit"])
	}
	if n := f.scalar("SELECT count(*) FROM workflows WHERE name='Release Kit'"); n != 1 {
		t.Fatalf("archiving must keep the one imported row: %d", n)
	}

	// Removing the plugin keeps the tenant's copies and their snapshots: they are user data.
	snapshotsBeforeUninstall := f.scalar("SELECT count(*) FROM workflow_snapshots")
	plugin := f.spacePlugin(sid, "official/release-kit")
	f.call("DELETE", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/release-kit", "version": plugin.N("version")}, "remove-release-kit", 200)
	if live := f.liveWorkflows(t); len(live) != 2 || live["Hotfix Kit"] == nil {
		t.Fatalf("uninstall must not remove imported workflows: %v", live)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != snapshotsBeforeUninstall {
		t.Fatalf("uninstall rewrote snapshots: %d -> %d", snapshotsBeforeUninstall, n)
	}

	// Import results are space-member data and never leak across tenants.
	bob, bobID := f.addUser(t, "asset-bob", "Bob")
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	_, status, e := f.client.Call(context.Background(), "GET", f.pluginSpacePath(sid)+"/plugins/assets", "gateway", gw, &bob, "", nil)
	must(t, e)
	if status != 200 {
		t.Fatalf("space member asset read: want 200 got %d", status)
	}
	foreign := f.createPluginTenant("Other", "other-assets", "other-assets")
	// Bob is a member of the primary tenant only; the other tenant's space is not his to read.
	_, status, e = f.client.Call(context.Background(), "GET", f.path("/spaces/"+foreign.S("id")+"/plugins/assets"), "gateway", gw, &bob, "", nil)
	must(t, e)
	if status != 404 {
		t.Fatalf("foreign space asset read: want 404 got %d", status)
	}
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "member", "status": "disabled", "version": 1}, "", 200)
	_, status, e = f.client.Call(context.Background(), "GET", f.pluginSpacePath(sid)+"/plugins/assets", "gateway", gw, &bob, "", nil)
	must(t, e)
	if status != 403 {
		t.Fatalf("disabled member asset read: want 403 got %d", status)
	}
	if n := f.scalar("SELECT count(*) FROM workflows WHERE tenant_id=$1", f.tid); n != 3 {
		t.Fatalf("imported copies must stay in the plugin's tenant: %d", n)
	}
}

// TestWorkflowAssetImportNormalizesFileGlobals covers the import write path the publish gate shares:
// a packaged document whose file and array[file] globals carry legacy path strings freezes with the
// canonical {kind: "workspace_file", path} reference, so a condition reading the imported snapshot
// sees the same shape desktop's into_global_variables produces. The live copy keeps the authored
// strings, and a document without file globals still freezes byte-for-byte — only the documents that
// need the rewrite are re-encoded.
func TestWorkflowAssetImportNormalizesFileGlobals(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	fileDoc := executableDocument("File Kit", `"globalVariables":[{"name":"f.report","valueType":"file","value":"docs/report.md"},{"name":"f.reports","valueType":"array[file]","value":["docs/a.md","docs/b.md"]}]`)
	plainDoc := executableDocument("Plain Kit", `"description":"no file globals"`)
	pkg := workflowPackage(t, map[string]string{
		"file.json":  fileDoc,
		"plain.json": plainDoc,
	})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/file-kit-1.0.0.orax": pkg})
	publishWorkflowRelease(t, repo, "file-kit", "1.0.0", packages.server.URL+"/packages/file-kit-1.0.0.orax", pkg)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/file-kit", "")
	must(t, f.importWorkflowAssets(t))

	documents := assetDocuments(f.assetImport(t, sid, "official/file-kit"))
	fileID := documents["assets/workflows/file.json"].S("workflowId")
	plainID := documents["assets/workflows/plain.json"].S("workflowId")
	if fileID == "" || plainID == "" {
		t.Fatalf("both documents must import: %v", documents)
	}

	// The frozen snapshot stores canonical references; the live document keeps the authored strings.
	snapshots := f.call("GET", f.path("/workflows/"+fileID+"/snapshots"), nil, "", 200)
	snapshot := core.Object(snapshots["items"].([]any)[0].(map[string]any))
	globals, _ := snapshot.O("graph")["globalVariables"].([]any)
	if len(globals) != 2 {
		t.Fatalf("snapshot froze %d globals, want 2: %v", len(globals), globals)
	}
	report := globals[0].(map[string]any)["value"].(map[string]any)
	if report["kind"] != "workspace_file" || report["path"] != "docs/report.md" {
		t.Fatalf("file global value = %v; want {kind: workspace_file, path: docs/report.md}", report)
	}
	reports := globals[1].(map[string]any)["value"].([]any)
	for i, want := range []string{"docs/a.md", "docs/b.md"} {
		ref := reports[i].(map[string]any)
		if ref["kind"] != "workspace_file" || ref["path"] != want {
			t.Fatalf("array[file] global value[%d] = %v; want {kind: workspace_file, path: %s}", i, ref, want)
		}
	}

	live := f.liveWorkflows(t)["File Kit"]
	liveGlobals, _ := live.O("graph")["globalVariables"].([]any)
	if got := liveGlobals[0].(map[string]any)["value"]; got != "docs/report.md" {
		t.Fatalf("live graph value = %v; the authored string must not be rewritten", got)
	}

	// A document without file globals must freeze byte-for-byte: the snapshot stores the same text
	// the live copy does, not a re-encoded graph.
	var livePlain, snapshotPlain string
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflows WHERE id=$1", plainID).Scan(&livePlain))
	must(t, f.store.Pool.QueryRow("SELECT graph::text FROM workflow_snapshots WHERE workflow_id=$1 AND version=1", plainID).Scan(&snapshotPlain))
	if snapshotPlain != livePlain {
		t.Fatalf("a document without file globals must freeze verbatim: live=%s snapshot=%s", livePlain, snapshotPlain)
	}
}

// TestWorkflowAssetImportDocumentFailuresAreIndependent covers the four bounded document codes:
// one document failing never aborts the package, failures are retried on the next administrator
// trigger, and a successful path is never revisited even when the package changes.
func TestWorkflowAssetImportDocumentFailuresAreIndependent(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	broken := workflowPackage(t, map[string]string{
		"ok.json":     executableDocument("Good Kit", ""),
		"bad.json":    `{"name":`,
		"noname.json": `{"version":1}`,
		"graph.json":  `{"name":"Not Runnable","nodes":[],"edges":[]}`,
		"taken.json":  executableDocument("Taken", ""),
	})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/broken-1.0.0.orax": broken})
	publishWorkflowRelease(t, repo, "broken-kit", "1.0.0", packages.server.URL+"/packages/broken-1.0.0.orax", broken)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	// A live workflow already owns one of the imported names.
	f.call("POST", f.path("/workflows"), core.Object{"name": "Taken", "description": "owned", "graph": core.Object{"nodes": []any{}, "edges": []any{}}}, "seed-conflict", 200)
	f.installWorkflowPlugin(t, sid, "official/broken-kit", "")
	if err := f.importWorkflowAssets(t); err != nil {
		t.Fatalf("document failures must not fail the package: %v", err)
	}
	item := f.assetImport(t, sid, "official/broken-kit")
	if item.S("state") != "completed" {
		t.Fatalf("a package with failed documents still completed its attempt: %v", item)
	}
	documents := assetDocuments(item)
	for path, want := range map[string]string{
		"assets/workflows/ok.json":     "imported",
		"assets/workflows/bad.json":    "document_invalid_json",
		"assets/workflows/noname.json": "document_without_name",
		"assets/workflows/graph.json":  "document_not_executable_graph",
		"assets/workflows/taken.json":  "name_conflict",
	} {
		document := documents[path]
		got := document.S("outcome")
		if got == "failed" {
			got = document.S("failureCode")
		}
		if got != want {
			t.Fatalf("%s = %s (%v), want %s", path, got, document, want)
		}
	}
	if documents["assets/workflows/graph.json"]["failureDetail"] == nil {
		t.Fatal("a graph failure must carry its problems")
	}
	// Failed documents leave neither a workflow nor a snapshot behind.
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != 1 {
		t.Fatalf("failed documents must not leave snapshots: %d", n)
	}
	goodID := documents["assets/workflows/ok.json"].S("workflowId")
	if live := f.liveWorkflows(t); len(live) != 2 || live["Good Kit"] == nil || live["Taken"] == nil {
		t.Fatalf("only the good document and the pre-existing workflow must exist: %v", live)
	}

	// The import result and the editor's publish refusal must be the same problems, because both
	// come from the one shared gate. Publishing the same graph proves the lists have not drifted.
	probe := f.call("POST", f.path("/workflows"), core.Object{
		"name":  "Import Gate Probe",
		"graph": core.Object{"nodes": []any{}, "edges": []any{}},
	}, "import-gate-probe", 200).O("resource")
	refused := f.call("POST", f.path("/workflows/"+probe.S("id")+"/publish"), core.Object{}, "import-gate-probe-publish", 400)
	publishedProblems := []any{}
	for _, problem := range problems(t, refused) {
		publishedProblems = append(publishedProblems, map[string]any(problem))
	}
	importedProblems, _ := documents["assets/workflows/graph.json"].O("failureDetail")["problems"].([]any)
	if !reflect.DeepEqual(importedProblems, publishedProblems) {
		t.Fatalf("import and publish gates disagree: import=%v publish=%v", importedProblems, publishedProblems)
	}
	f.call("DELETE", f.path("/workflows/"+probe.S("id")), core.Object{"version": probe.N("version")}, "import-gate-probe-archive", 200)

	// A failed result is retried on the next trigger; the retry must refresh both the document's
	// timestamp and the source attempt's, so a reader can tell a retry from a stuck first attempt.
	_, err := f.store.Pool.Exec(`UPDATE workflow_asset_documents SET updated_at=now()-interval '1 hour'
		WHERE space_id=$1 AND outcome='failed'`, sid)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE workflow_asset_imports SET attempted_at=now()-interval '1 hour'
		WHERE space_id=$1 AND identifier='broken-kit'`, sid)
	must(t, err)
	if n := f.scalar(`SELECT count(*) FROM workflow_asset_documents WHERE space_id=$1 AND updated_at<now()-interval '30 minutes'`, sid); n != 4 {
		t.Fatalf("backdated failed results = %d, want 4", n)
	}
	if n := f.scalar(`SELECT count(*) FROM workflow_asset_imports WHERE space_id=$1 AND attempted_at<now()-interval '30 minutes'`, sid); n != 1 {
		t.Fatalf("backdated attempts = %d, want 1", n)
	}

	// A repaired package retries only the failed paths; the imported one keeps its original copy.
	fixed := workflowPackage(t, map[string]string{
		"ok.json":     executableDocument("Good Kit", `"changed":true`),
		"bad.json":    executableDocument("Bad Kit", ""),
		"noname.json": executableDocument("Named Kit", ""),
		"graph.json":  executableDocument("Runnable Kit", ""),
		"taken.json":  executableDocument("Taken", ""),
	})
	packages.replace("/packages/broken-1.1.0.orax", fixed)
	publishWorkflowRelease(t, repo, "broken-kit", "1.1.0", packages.server.URL+"/packages/broken-1.1.0.orax", fixed)
	f.syncMarketplace(t, repo)
	f.installWorkflowPlugin(t, sid, "official/broken-kit", "1.1.0")
	must(t, f.importWorkflowAssets(t))
	if n := f.scalar(`SELECT count(*) FROM workflow_asset_documents WHERE space_id=$1 AND updated_at<now()-interval '30 minutes'`, sid); n != 0 {
		t.Fatalf("retried documents did not refresh their timestamps: %d stale rows", n)
	}
	if n := f.scalar(`SELECT count(*) FROM workflow_asset_imports WHERE space_id=$1 AND attempted_at<now()-interval '30 minutes'`, sid); n != 0 {
		t.Fatalf("retried attempt did not refresh its timestamp: %d stale rows", n)
	}
	documents = assetDocuments(f.assetImport(t, sid, "official/broken-kit"))
	for _, path := range []string{"assets/workflows/bad.json", "assets/workflows/noname.json", "assets/workflows/graph.json"} {
		if documents[path].S("outcome") != "imported" {
			t.Fatalf("%s was not retried: %v", path, documents[path])
		}
	}
	if documents["assets/workflows/ok.json"].S("workflowId") != goodID {
		t.Fatal("an imported path must not be replaced by a newer package version")
	}
	if documents["assets/workflows/taken.json"].S("failureCode") != "name_conflict" {
		t.Fatalf("the conflict must persist while the name is taken: %v", documents["assets/workflows/taken.json"])
	}

	// Freeing the name lets the next trigger take the path over.
	taken := f.liveWorkflows(t)["Taken"]
	f.call("DELETE", f.path("/workflows/"+taken.S("id")), core.Object{"version": taken.N("version")}, "archive-taken", 200)
	f.installWorkflowPlugin(t, sid, "official/broken-kit", "1.1.0")
	must(t, f.importWorkflowAssets(t))
	documents = assetDocuments(f.assetImport(t, sid, "official/broken-kit"))
	if documents["assets/workflows/taken.json"].S("outcome") != "imported" {
		t.Fatalf("a freed name must be retried: %v", documents["assets/workflows/taken.json"])
	}
	if live := f.liveWorkflows(t); len(live) != 5 || live["Taken"] == nil {
		t.Fatalf("workflow library after retries = %v", live)
	}
}

// TestWorkflowAssetImportRejectsTextPostgresCannotStore covers the documents PostgreSQL's jsonb
// input refuses while Go's decoder accepts: NUL escapes, unpaired surrogate escapes and invalid
// UTF-8. The package text is stored verbatim, so these must become document failures before the
// result transaction; otherwise one such document aborts the write of the whole package and leaves
// the source row importing. A valid surrogate pair is accepted, so the check does not over-reject.
func TestWorkflowAssetImportRejectsTextPostgresCannotStore(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	pkg := workflowPackage(t, map[string]string{
		"good.json": executableDocument("Good Kit", ""),
		"pair.json": executableDocument("Paired Kit", `"description":"a`+"\\"+`ud83d`+"\\"+`ude00b"`),
		"nul.json":  executableDocument("NUL Kit", `"description":"a\u0000b"`),
		"lone.json": executableDocument("Surrogate Kit", `"description":"a\ud800b"`),
		"utf8.json": executableDocument("UTF Kit", "\"description\":\"bad \xff byte\""),
	})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/text-1.0.0.orax": pkg})
	publishWorkflowRelease(t, repo, "text-kit", "1.0.0", packages.server.URL+"/packages/text-1.0.0.orax", pkg)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/text-kit", "")
	if err := f.importWorkflowAssets(t); err != nil {
		t.Fatalf("unrepresentable documents must fail alone: %v", err)
	}
	item := f.assetImport(t, sid, "official/text-kit")
	if item.S("state") != "completed" {
		t.Fatalf("the package must complete despite document failures: %v", item)
	}
	documents := assetDocuments(item)
	for path, want := range map[string]string{
		"assets/workflows/good.json": "imported",
		"assets/workflows/pair.json": "imported",
		"assets/workflows/nul.json":  "unsupported_unicode_escape",
		"assets/workflows/lone.json": "unsupported_unicode_escape",
		"assets/workflows/utf8.json": "invalid_utf8",
	} {
		document := documents[path]
		if want == "imported" {
			if document.S("outcome") != "imported" {
				t.Fatalf("%s = %v, want imported", path, document)
			}
			continue
		}
		if document.S("outcome") != "failed" || document.S("failureCode") != "document_invalid_json" {
			t.Fatalf("%s = %v, want document_invalid_json", path, document)
		}
		if reason := document.O("failureDetail").S("reason"); reason != want {
			t.Fatalf("%s reason = %q, want %q", path, reason, want)
		}
	}
	if live := f.liveWorkflows(t); len(live) != 2 || live["Good Kit"] == nil || live["Paired Kit"] == nil {
		t.Fatalf("failed documents must not create workflows: %v", live)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != 2 {
		t.Fatalf("failed documents must not leave snapshots: %d", n)
	}
	// The result transaction committed: the source row is not stuck in importing.
	if n := f.scalar("SELECT count(*) FROM workflow_asset_imports WHERE space_id=$1 AND state='importing'", sid); n != 0 {
		t.Fatalf("the result transaction did not commit: %d importing rows", n)
	}
}

// TestWorkflowAssetImportRejectsNumbersPostgresCannotStore covers the same isolation for numbers
// outside jsonb's numeric range. Go decodes 1e-20000 to zero, and PostgreSQL also rejects exponent
// literals beyond 2^30-1 even when the mantissa is zero; storing either fails with "value overflows
// numeric format" and would roll back the result transaction for the whole package. Those documents
// must fail alone, while numbers exactly on the storage boundaries still import; re-encoding or
// truncating a document to make it storable is not allowed.
func TestWorkflowAssetImportRejectsNumbersPostgresCannotStore(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	pkg := workflowPackage(t, map[string]string{
		"good.json":              executableDocument("Good Kit", ""),
		"scale-boundary.json":    executableDocument("Scale Boundary Kit", `"extra":1e-16383`),
		"trailing-boundary.json": executableDocument("Trailing Boundary Kit", `"extra":100e-16383`),
		"exponent-boundary.json": executableDocument("Exponent Boundary Kit", `"extra":0e1073741823`),
		"numeric.json":           executableDocument("Numeric Kit", `"extra":1e-20000`),
		"zero-exponent.json":     executableDocument("Zero Exponent Kit", `"extra":0e2147483648`),
	})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/numeric-1.0.0.orax": pkg})
	publishWorkflowRelease(t, repo, "numeric-kit", "1.0.0", packages.server.URL+"/packages/numeric-1.0.0.orax", pkg)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/numeric-kit", "")
	if err := f.importWorkflowAssets(t); err != nil {
		t.Fatalf("a numeric document must fail alone: %v", err)
	}
	item := f.assetImport(t, sid, "official/numeric-kit")
	if item.S("state") != "completed" {
		t.Fatalf("the package must complete despite document failures: %v", item)
	}
	documents := assetDocuments(item)
	for _, path := range []string{
		"assets/workflows/good.json",
		"assets/workflows/scale-boundary.json",
		"assets/workflows/trailing-boundary.json",
		"assets/workflows/exponent-boundary.json",
	} {
		if got := documents[path].S("outcome"); got != "imported" {
			t.Fatalf("%s outcome = %q, want imported: %v", path, got, documents[path])
		}
	}
	for _, path := range []string{"assets/workflows/numeric.json", "assets/workflows/zero-exponent.json"} {
		document := documents[path]
		if document.S("outcome") != "failed" || document.S("failureCode") != "document_invalid_json" {
			t.Fatalf("%s = %v, want document_invalid_json", path, document)
		}
		if reason := document.O("failureDetail").S("reason"); reason != "numeric_out_of_range" {
			t.Fatalf("%s reason = %q, want numeric_out_of_range", path, reason)
		}
	}
	live := f.liveWorkflows(t)
	for _, name := range []string{"Good Kit", "Scale Boundary Kit", "Trailing Boundary Kit", "Exponent Boundary Kit"} {
		if live[name] == nil {
			t.Fatalf("boundary documents must import: %v", live)
		}
	}
	if len(live) != 4 {
		t.Fatalf("failed documents must not create workflows: %v", live)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != 4 {
		t.Fatalf("failed documents must not leave snapshots: %d", n)
	}
	// The result transaction committed: the source row is not stuck in importing.
	if n := f.scalar("SELECT count(*) FROM workflow_asset_imports WHERE space_id=$1 AND state='importing'", sid); n != 0 {
		t.Fatalf("the result transaction did not commit: %d importing rows", n)
	}
}

// reinstallDuringFetch models an administrator upgrading the plugin while the old package is in
// flight. The first call installs the newer version and still returns the older bytes.
type reinstallDuringFetch struct {
	f      *fixture
	t      *testing.T
	sid    string
	first  []byte
	second []byte
	calls  int
}

func (r *reinstallDuringFetch) Fetch(context.Context, string, string) ([]byte, error) {
	r.calls++
	if r.calls == 1 {
		r.f.installWorkflowPlugin(r.t, r.sid, "official/racing-kit", "2.0.0")
		return r.first, nil
	}
	return r.second, nil
}

// TestWorkflowAssetImportFencesAReinstallDuringDownload proves the claimed revision fences the
// result write: an upgrade during the download discards the stale attempt instead of importing
// the old document and occupying its skip key ahead of the newer package.
func TestWorkflowAssetImportFencesAReinstallDuringDownload(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	v1 := workflowPackage(t, map[string]string{"release.json": executableDocument("Old Kit", "")})
	v2 := workflowPackage(t, map[string]string{"release.json": executableDocument("New Kit", "")})
	packages := newWorkflowPackages(t, map[string][]byte{
		"/packages/racing-kit-1.0.0.orax": v1,
		"/packages/racing-kit-2.0.0.orax": v2,
	})
	publishWorkflowRelease(t, repo, "racing-kit", "1.0.0", packages.server.URL+"/packages/racing-kit-1.0.0.orax", v1)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/racing-kit", "")
	// The newer release must be in the catalog before the stale download triggers its install.
	publishWorkflowRelease(t, repo, "racing-kit", "2.0.0", packages.server.URL+"/packages/racing-kit-2.0.0.orax", v2)
	f.syncMarketplace(t, repo)

	f.store.WorkflowAssets = &reinstallDuringFetch{f: f, t: t, sid: sid, first: v1, second: v2}
	must(t, f.importWorkflowAssets(t))

	if n := f.scalar("SELECT count(*) FROM workflows"); n != 0 {
		t.Fatalf("a superseded attempt must not import documents: %d workflows", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_asset_documents"); n != 0 {
		t.Fatalf("a superseded attempt must not record document outcomes: %d rows", n)
	}
	item := f.assetImport(t, sid, "official/racing-kit")
	if item.S("state") != "pending" || item.S("packageVersion") != "2.0.0" {
		t.Fatalf("the newer revision must stay pending: %v", item)
	}

	must(t, f.importWorkflowAssets(t))
	item = f.assetImport(t, sid, "official/racing-kit")
	if item.S("state") != "completed" {
		t.Fatalf("the newer revision must import: %v", item)
	}
	documents := assetDocuments(item)
	if documents["assets/workflows/release.json"].S("outcome") != "imported" {
		t.Fatalf("document outcome = %v", documents["assets/workflows/release.json"])
	}
	// The imported copy comes from the version that was selected when the retry ran.
	if live := f.liveWorkflows(t); len(live) != 1 || live["New Kit"] == nil {
		t.Fatalf("imported workflows = %v", live)
	}
}

// upgradeBeforeClaim installs the second plugin's newer version during the first download. The
// second plugin's job was already resolved from the older selection before the pass started, so the
// pass must discard it rather than claim the newer revision with the stale locator.
type upgradeBeforeClaim struct {
	f      *fixture
	t      *testing.T
	sid    string
	first  []byte
	second []byte
	calls  int
}

func (u *upgradeBeforeClaim) Fetch(context.Context, string, string) ([]byte, error) {
	u.calls++
	if u.calls == 1 {
		u.f.installWorkflowPlugin(u.t, u.sid, "official/racing-kit", "2.0.0")
		return u.first, nil
	}
	return u.second, nil
}

// TestWorkflowAssetImportDiscardsAJobSupersededBeforeClaim covers the scan-to-claim window: the job
// list is resolved before any download, so an administrator's upgrade between the scan and a later
// claim must invalidate that job. Claiming it would download the previously selected package while
// recording the newer revision, and the succeeded path would occupy the skip key for the old
// document forever.
func TestWorkflowAssetImportDiscardsAJobSupersededBeforeClaim(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	first := workflowPackage(t, map[string]string{"first.json": executableDocument("First Kit", "")})
	old := workflowPackage(t, map[string]string{"same.json": executableDocument("Old Kit", "")})
	newer := workflowPackage(t, map[string]string{"same.json": executableDocument("New Kit", "")})
	packages := newWorkflowPackages(t, map[string][]byte{
		"/first.orax": first,
		"/old.orax":   old,
		"/new.orax":   newer,
	})
	publishWorkflowRelease(t, repo, "first-kit", "1.0.0", packages.server.URL+"/first.orax", first)
	publishWorkflowRelease(t, repo, "racing-kit", "1.0.0", packages.server.URL+"/old.orax", old)
	f.syncMarketplace(t, repo)

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/first-kit", "")
	f.installWorkflowPlugin(t, sid, "official/racing-kit", "")
	// The upgrade is in the catalog before the pass scans, but the install that selects it happens
	// during the pass, after racing-kit's job has already been resolved from the older release.
	publishWorkflowRelease(t, repo, "racing-kit", "2.0.0", packages.server.URL+"/new.orax", newer)
	f.syncMarketplace(t, repo)
	// Make the first plugin's job the earlier one so the upgrade lands before racing-kit's claim.
	_, err := f.store.Pool.Exec(`UPDATE workflow_asset_imports SET updated_at=now()-CASE
		WHEN identifier='first-kit' THEN interval '2 hours' ELSE interval '1 hour' END WHERE space_id=$1`, sid)
	must(t, err)

	f.store.WorkflowAssets = &upgradeBeforeClaim{f: f, t: t, sid: sid, first: first, second: newer}
	must(t, f.importWorkflowAssets(t))
	// The superseded job wrote nothing, so the row is still pending for the newer revision.
	item := f.assetImport(t, sid, "official/racing-kit")
	if item.S("state") != "pending" || item.S("packageVersion") != "2.0.0" {
		t.Fatalf("a superseded job claimed the newer revision: %v", item)
	}
	must(t, f.importWorkflowAssets(t))

	if live := f.liveWorkflows(t); live["New Kit"] == nil || live["Old Kit"] != nil || live["First Kit"] == nil {
		t.Fatalf("the superseded job imported stale content: %v", live)
	}
	item = f.assetImport(t, sid, "official/racing-kit")
	if item.S("state") != "completed" || item.S("packageVersion") != "2.0.0" {
		t.Fatalf("the newer revision did not complete: %v", item)
	}
	if n := f.scalar(`SELECT count(*) FROM workflow_asset_documents
		WHERE space_id=$1 AND identifier='racing-kit' AND package_version='1.0.0'`, sid); n != 0 {
		t.Fatalf("the superseded package left %d document results", n)
	}
}

// TestWorkflowAssetImportPackageFailureRetriesOnNextTrigger covers package-level failure: a digest
// mismatch records one bounded package code, produces no document rows, and is retried by the next
// administrator install of the same version.
func TestWorkflowAssetImportPackageFailureRetriesOnNextTrigger(t *testing.T) {
	f := setup(t)
	repo := workflowMarketplace(t, f.root)
	good := workflowPackage(t, map[string]string{"release.json": executableDocument("Retried Kit", "")})
	packages := newWorkflowPackages(t, map[string][]byte{"/packages/retry-1.0.0.orax": []byte("corrupted bytes")})
	publishWorkflowRelease(t, repo, "retry-kit", "1.0.0", packages.server.URL+"/packages/retry-1.0.0.orax", good)
	f.syncMarketplace(t, repo)
	f.store.WorkflowAssets = workflowasset.NewFetcher(nil, packages.server.Client().Transport)

	sid := f.defaultSpaceID()
	f.installWorkflowPlugin(t, sid, "official/retry-kit", "")
	if err := f.importWorkflowAssets(t); err == nil || !strings.Contains(err.Error(), workflowasset.FailureDigestMismatch) {
		t.Fatalf("digest mismatch must surface its bounded code: %v", err)
	}
	item := f.assetImport(t, sid, "official/retry-kit")
	if item.S("state") != "failed" || item.S("failureCode") != workflowasset.FailureDigestMismatch {
		t.Fatalf("package failure = %v", item)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_asset_documents"); n != 0 {
		t.Fatalf("a package failure must not record document outcomes: %d", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflows"); n != 0 {
		t.Fatalf("a package failure must not create workflows: %d", n)
	}

	// The storage serves the pinned bytes again; an administrator reinstall is the retry trigger.
	packages.replace("/packages/retry-1.0.0.orax", good)
	f.installWorkflowPlugin(t, sid, "official/retry-kit", "1.0.0")
	must(t, f.importWorkflowAssets(t))
	item = f.assetImport(t, sid, "official/retry-kit")
	if item.S("state") != "completed" || item.S("failureCode") != "" {
		t.Fatalf("retried package = %v", item)
	}
	if live := f.liveWorkflows(t); live["Retried Kit"] == nil {
		t.Fatalf("retried package did not import: %v", live)
	}
}
