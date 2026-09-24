package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// marketplaceFixture builds a local marketplace git repository in the desktop
// layout (registry/**/orax.toml) with system git, and returns its directory
// plus the .orax artifact files whose digests are baked into the listings.
func marketplaceFixture(t *testing.T, root string) (repoDir string, artifacts map[string]string) {
	t.Helper()
	repo := filepath.Join(root, "marketplace")
	must(t, os.MkdirAll(repo, 0o700))
	runGit(t, "init", "--initial-branch=main", repo)
	runGit(t, "-C", repo, "config", "user.name", "Marketplace")
	runGit(t, "-C", repo, "config", "user.email", "marketplace@example.invalid")
	artifacts = map[string]string{}
	writeArtifact := func(name, content string) string {
		t.Helper()
		path := filepath.Join(root, name)
		must(t, os.WriteFile(path, []byte(content), 0o600))
		sum := sha256.Sum256([]byte(content))
		artifacts["https://example.invalid/artifacts/"+name] = path
		return hex.EncodeToString(sum[:])
	}
	helloDigest := writeArtifact("hello-1.0.0.orax", "ora plugin archive bytes for hello-world 1.0.0\n")
	toolDigest := writeArtifact("tool-linux.orax", "ora plugin archive bytes for native-tool 2.0.0\n")
	listings := map[string]string{
		"registry/a/hello-world/orax.toml": "resolver = 1\nidentifier = \"hello-world\"\ntitle = \"Hello World\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"A friendly agent.\"\nurl = \"https://example.invalid/artifacts/hello-1.0.0.orax\"\nsha256 = \"" + helloDigest + "\"\n",
		"registry/a/hello-world/README.md": "# Hello World\n",
		"registry/b/native-tool/orax.toml": "resolver = 1\nidentifier = \"native-tool\"\nkind = \"hook\"\nversion = \"2.0.0\"\ndescription = \"A native tool.\"\n[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/artifacts/tool-linux.orax\"\nsha256 = \"" + toolDigest + "\"\n[[targets]]\ntarget = \"aarch64-unknown-linux-gnu\"\nurl = \"https://example.invalid/artifacts/tool-arm.orax\"\nsha256 = \"" + strings.Repeat("ab", 32) + "\"\n",
		"registry/c/hidden/orax.toml":      "resolver = 1\nidentifier = \"hidden\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Hidden.\"\nmarketplace_visible = false\n",
		"registry/p/all-in-one/orax.toml":  "resolver = 1\nidentifier = \"all-in-one\"\nkind = \"pack\"\nversion = \"1.0.0\"\ndescription = \"A pack.\"\n[pack]\nmembers = [\"hello-world\", \"native-tool\"]\n",
		"registry/w/dup/orax.toml":         "resolver = 1\nidentifier = \"hello-world\"\nkind = \"agent\"\nversion = \"0.0.1\"\ndescription = \"Duplicate.\"\n",
	}
	for rel, content := range listings {
		path := filepath.Join(repo, rel)
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(content), 0o600))
	}
	runGit(t, "-C", repo, "add", ".")
	runGit(t, "-C", repo, "commit", "-m", "marketplace fixture")
	return repo, artifacts
}

// syncMarketplace runs the production syncer against a local fixture
// repository: real go-git clone/pull plus the real PostgreSQL sink.
func (f *fixture) syncMarketplace(t *testing.T, repo string) *pluginmarket.Syncer {
	t.Helper()
	syncer := pluginmarket.NewSyncer(
		pluginmarket.Source{Namespace: "official", URL: "file:///" + filepath.ToSlash(repo), Branch: "main"},
		filepath.Join(f.root, "plugin-checkout"),
		f.store, zap.NewNop(),
	)
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	return syncer
}

// defaultSpaceID returns the bootstrap tenant's default collaboration space.
func (f *fixture) defaultSpaceID() string {
	f.t.Helper()
	var id string
	must(f.t, f.store.Pool.QueryRow("SELECT id::text FROM collab_workspaces WHERE tenant_id=$1 AND slug='default'", f.tid).Scan(&id))
	return id
}

// spaceProject creates a space-scoped project and drains its lifecycle so the
// main workspace is ready for plugin effects.
func (f *fixture) spaceProject(t *testing.T, sid, key string) core.Object {
	t.Helper()
	created := f.call("POST", f.path("/spaces/"+sid+"/projects"), core.Object{"name": "Project", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "main"}, key, 202)
	f.drain()
	return created
}

// TestPluginMigrationAndCatalogPersistence covers IT-1 and IT-2: the migration
// constraints hold, the syncer seeds the source and replaces the snapshot
// transactionally, and a failed sync keeps the previous catalog.
func TestPluginMigrationAndCatalogPersistence(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)

	// IT-1.1: the catalog CHECK constraints reject out-of-contract values.
	if _, e := f.store.Pool.Exec(`INSERT INTO plugin_catalog_entries(source_namespace,identifier,title,kind,version,description,source_url,indexed_at) VALUES('official','x','X','daemon','1.0.0','','https://example.invalid/m.git',now())`); e == nil {
		t.Fatal("kind CHECK must reject an unknown plugin kind")
	}
	sid := f.defaultSpaceID()
	if _, e := f.store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state) VALUES($1,$2,'official','x','installed','1.0.0','bogus')`, sid, f.tid); e == nil {
		t.Fatal("observed_state CHECK must reject an unknown state")
	}
	// Seed one live project with its main workspace (in one transaction: the
	// deferred project_main trigger validates at commit) so the widened
	// operation CHECKs are exercised against real rows.
	pid, wid := uuid.NewString(), uuid.NewString()
	tx, e := f.store.Pool.Begin()
	must(t, e)
	if _, e = tx.Exec(`INSERT INTO projects(id,tenant_id,owner_user_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,'Constraint test','https://example.invalid/r.git','main','active')`, pid, f.tid, f.uid); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`INSERT INTO project_storage(project_id,observed_state) VALUES($1,'ready')`, pid); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state) VALUES($1,$2,$3,$4,'main','running','ready')`, wid, f.tid, f.uid, pid); e != nil {
		t.Fatal(e)
	}
	must(t, tx.Commit())
	if _, e := f.store.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,'install_plugin','queued','plugin','{}','k','h')`, uuid.NewString(), f.tid, f.uid, pid); e != nil {
		t.Fatalf("widened operations kind CHECK must accept install_plugin: %v", e)
	}
	if _, e := f.store.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash) VALUES($1,$2,$3,$4,'bogus','queued','plugin','{}','k2','h2')`, uuid.NewString(), f.tid, f.uid, pid); e == nil {
		t.Fatal("widened operations kind CHECK must still reject unknown kinds")
	}

	// IT-2.1: first sync seeds the source and fills the snapshot in one replace.
	syncer := f.syncMarketplace(t, repo)
	var syncedAt, syncErr string
	must(t, f.store.Pool.QueryRow(`SELECT coalesce(synced_at::text,''),coalesce(sync_error,'') FROM plugin_sources WHERE namespace='official'`).Scan(&syncedAt, &syncErr))
	if syncedAt == "" || syncErr != "" {
		t.Fatalf("source after sync: syncedAt=%q syncErr=%q", syncedAt, syncErr)
	}
	entries := map[string]bool{}
	rows, e := f.store.Pool.Query(`SELECT source_namespace||'/'||identifier FROM plugin_catalog_entries`)
	must(t, e)
	defer func() { must(t, rows.Close()) }()
	for rows.Next() {
		var id string
		must(t, rows.Scan(&id))
		entries[id] = true
	}
	must(t, rows.Err())
	for _, want := range []string{"official/hello-world", "official/native-tool", "official/all-in-one"} {
		if !entries[want] {
			t.Fatalf("catalog missing %s: %v", want, entries)
		}
	}
	if entries["official/hidden"] || entries["official/dup"] {
		t.Fatalf("hidden/duplicate listings must not enter the catalog: %v", entries)
	}

	// IT-2.1 continued: a second sync replaces the snapshot atomically.
	path := filepath.Join(repo, "registry", "z", "fresh", "orax.toml")
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte("resolver = 1\nidentifier = \"fresh\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Fresh.\"\n"), 0o600))
	runGit(t, "-C", repo, "add", ".")
	runGit(t, "-C", repo, "commit", "-m", "add fresh")
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if f.scalar("SELECT count(*) FROM plugin_catalog_entries WHERE identifier='fresh'") != 1 {
		t.Fatal("second sync must add the new listing")
	}

	// IT-2.2: a divergent checkout is refused and keeps the previous catalog.
	checkout := filepath.Join(f.root, "plugin-checkout")
	must(t, os.WriteFile(filepath.Join(checkout, "local.txt"), []byte("local\n"), 0o600))
	runGit(t, "-C", checkout, "add", ".")
	runGit(t, "-C", checkout, "-c", "user.name=Local", "-c", "user.email=local@example.invalid", "commit", "-m", "diverge")
	runGit(t, "-C", repo, "commit", "--allow-empty", "-m", "origin moves on")
	if e := syncer.Sync(context.Background()); e == nil || !strings.Contains(e.Error(), "non-fast-forward") {
		t.Fatalf("non-fast-forward sync must fail, got %v", e)
	}
	must(t, f.store.Pool.QueryRow(`SELECT coalesce(synced_at::text,''),coalesce(sync_error,'') FROM plugin_sources WHERE namespace='official'`).Scan(&syncedAt, &syncErr))
	if syncedAt == "" || !strings.Contains(syncErr, "non-fast-forward") {
		t.Fatalf("failed sync must keep synced_at and record sync_error: syncedAt=%q syncErr=%q", syncedAt, syncErr)
	}
	if f.scalar("SELECT count(*) FROM plugin_catalog_entries WHERE identifier='fresh'") != 1 {
		t.Fatal("failed sync must keep the previous catalog snapshot")
	}

	// IT-2.3: the catalog read never leaves the database — the fixture
	// repository can disappear and the API still serves the snapshot.
	must(t, os.RemoveAll(repo))
	catalog := f.call("GET", f.path("/spaces/"+f.defaultSpaceID()+"/plugins/catalog"), nil, "", 200)
	items := catalog["items"].([]any)
	if len(items) != 4 {
		t.Fatalf("catalog must serve the snapshot after the source disappeared: %d items", len(items))
	}
}
