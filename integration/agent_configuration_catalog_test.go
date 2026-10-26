package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// The Agent declaration publication chain: the marketplace sync reads static declaration copies
// into the catalog snapshot, an administrator's selection freezes the association with the exact
// release it chose, later catalog updates never rewrite that frozen identity, and the install
// plan a Node receives carries the frozen association. Configuration reads never touch the
// release packages: the whole flow runs against a package server that fails the test on the
// first request.
func TestAgentConfigurationCatalogSyncFreezesAndDispatches(t *testing.T) {
	f := setup(t)

	// The package origin must never be contacted by sync, selection, catalog reads, or dispatch.
	// The locator is HTTPS because that is what a listing may declare; the server's certificate is
	// irrelevant because no request may ever reach it.
	var packageRequests atomic.Int64
	packageServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		packageRequests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(packageServer.Close)

	declaration := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`
	declarationDigest := sha256.Sum256([]byte(declaration))
	validDigest := hex.EncodeToString(declarationDigest[:])
	reformed := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["openai-completions","anthropic-messages"],"modelCardinality":"single"}}}`
	reformedDigest := sha256.Sum256([]byte(reformed))
	secondDigest := hex.EncodeToString(reformedDigest[:])

	repo := filepath.Join(f.root, "declaration-marketplace")
	must(t, os.MkdirAll(repo, 0o700))
	runGit(t, "init", "--initial-branch=main", repo)
	runGit(t, "-C", repo, "config", "user.name", "Marketplace")
	runGit(t, "-C", repo, "config", "user.email", "marketplace@example.invalid")

	listing := func(version, digest string) string {
		return "resolver = 1\nidentifier = \"declared-agent\"\nkind = \"agent\"\nversion = \"" + version +
			"\"\ndescription = \"A declared agent.\"\nurl = \"" + packageServer.URL + "/declared-agent-" + version +
			".orax\"\nsha256 = \"" + strings.Repeat("11", 32) +
			"\"\n\n[agent_configuration]\nschema_version = 2\ndeclaration_sha256 = \"" + digest + "\"\ncopy = \"agent-configuration.json\"\n"
	}
	commitListing := func(dir, name, manifest, copyName, copyContent string) {
		t.Helper()
		path := filepath.Join(repo, "registry", dir, name, "orax.toml")
		must(t, os.MkdirAll(filepath.Dir(path), 0o700))
		must(t, os.WriteFile(path, []byte(manifest), 0o600))
		if copyName != "" {
			must(t, os.WriteFile(filepath.Join(filepath.Dir(path), copyName), []byte(copyContent), 0o600))
		}
	}

	// One declared agent with a valid copy, one whose copy contradicts its association, and one
	// undeclared agent: three distinct facts the catalog must keep distinct.
	commitListing("a", "declared-agent", listing("1.0.0", validDigest), "agent-configuration.json", declaration)
	commitListing("b", "mismatched", strings.ReplaceAll(strings.ReplaceAll(listing("1.0.0", "66"+validDigest[2:]), "declared-agent", "mismatched"), "\"A declared agent.\"", "\"Mismatched.\""), "agent-configuration.json", declaration)
	commitListing("c", "undeclared", "resolver = 1\nidentifier = \"undeclared\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Undeclared.\"\nurl = \""+packageServer.URL+"/undeclared.orax\"\nsha256 = \""+strings.Repeat("22", 32)+"\"\n", "", "")
	// A rejected member containing NUL must not poison the PostgreSQL text parameter and
	// roll back the valid entries in this same source snapshot.
	malformed := strings.Replace(declaration, `"schemaVersion":2`, `"schemaVersion":2,"privateMarker\u0000":1`, 1)
	malformedHash := sha256.Sum256([]byte(malformed))
	commitListing("d", "malformed", strings.ReplaceAll(listing("1.0.0", hex.EncodeToString(malformedHash[:])), "declared-agent", "malformed"), "agent-configuration.json", malformed)
	commitListing("e", "missing-copy", strings.ReplaceAll(listing("1.0.0", validDigest), "declared-agent", "missing-copy"), "", "")
	runGit(t, "-C", repo, "add", ".")
	runGit(t, "-C", repo, "commit", "-m", "declaration marketplace")

	syncer := f.syncMarketplace(t, repo)
	for identifier, expected := range map[string]string{
		"malformed":    "declaration invalid: invalid_structure: unknown declaration member",
		"missing-copy": "copy unreadable",
	} {
		var metadata *string
		var reason string
		must(t, f.store.Pool.QueryRow(`SELECT agent_configuration::text,agent_configuration_issue FROM plugin_catalog_entries WHERE identifier=$1`, identifier).Scan(&metadata, &reason))
		if metadata != nil || reason != expected {
			t.Fatalf("%s: invalid declaration must persist only a safe reason: metadata=%v reason=%q", identifier, metadata, reason)
		}
	}

	// The sync consumes only the static copies; every package stays un-downloaded.
	if got := packageRequests.Load(); got != 0 {
		t.Fatalf("sync must not download release packages, got %d requests", got)
	}

	var stored struct {
		SchemaVersion     uint32 `json:"schemaVersion"`
		DeclarationSha256 string `json:"declarationSha256"`
		CopyPath          string `json:"copyPath"`
		Declaration       string `json:"declaration"`
	}
	row := f.store.Pool.QueryRow(`SELECT agent_configuration::text,agent_configuration_issue FROM plugin_catalog_entries WHERE identifier='declared-agent'`)
	var raw, issue *string
	must(t, row.Scan(&raw, &issue))
	if raw == nil || issue != nil {
		t.Fatalf("valid association must be consumable: raw=%v issue=%v", raw, issue)
	}
	must(t, json.Unmarshal([]byte(*raw), &stored))
	if stored.SchemaVersion != 2 ||
		stored.DeclarationSha256 != validDigest ||
		stored.CopyPath != "agent-configuration.json" ||
		stored.Declaration != declaration {
		t.Fatalf("stored association: %+v", stored)
	}

	var mismatchedRaw, mismatchedIssue *string
	row = f.store.Pool.QueryRow(`SELECT agent_configuration::text,agent_configuration_issue FROM plugin_catalog_entries WHERE identifier='mismatched'`)
	must(t, row.Scan(&mismatchedRaw, &mismatchedIssue))
	if mismatchedRaw != nil || mismatchedIssue == nil || !strings.Contains(*mismatchedIssue, "digest does not match") {
		t.Fatalf("mismatched copy must record an issue without a consumable declaration: raw=%v issue=%v", deref(mismatchedRaw), deref(mismatchedIssue))
	}
	var undeclaredRaw, undeclaredIssue *string
	row = f.store.Pool.QueryRow(`SELECT agent_configuration::text,agent_configuration_issue FROM plugin_catalog_entries WHERE identifier='undeclared'`)
	must(t, row.Scan(&undeclaredRaw, &undeclaredIssue))
	if undeclaredRaw != nil || undeclaredIssue != nil {
		t.Fatalf("undeclared listing must carry neither field: raw=%v issue=%v", undeclaredRaw, undeclaredIssue)
	}

	// The catalog read serves the association from the snapshot alone.
	catalog := f.call("GET", f.path("/spaces/"+f.defaultSpaceID()+"/plugins/catalog"), nil, "", 200)
	items, _ := json.Marshal(catalog["items"])
	if !strings.Contains(string(items), validDigest) {
		t.Fatalf("catalog read must serve the persisted association: %s", string(items)[:min(len(items), 400)])
	}
	if got := packageRequests.Load(); got != 0 {
		t.Fatalf("catalog reads must not download release packages, got %d requests", got)
	}

	// An administrator's selection freezes the association with the exact release chosen.
	sid := f.defaultSpaceID()
	selection := f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/declared-agent"}, "select-declared", 200)
	if selection.O("resource").S("kind") != "agent" {
		t.Fatalf("selection must expose the frozen release kind: %+v", selection)
	}
	frozen := f.store.Pool.QueryRow(`SELECT selected_release::text FROM space_plugins WHERE source_namespace='official' AND identifier='declared-agent'`)
	var frozenText string
	must(t, frozen.Scan(&frozenText))
	if !strings.Contains(frozenText, validDigest) {
		t.Fatalf("the selection must freeze the declaration association: %s", frozenText)
	}

	// A marketplace update publishes a new version with different declaration bytes. The catalog
	// moves on; the frozen selection and any workspace it feeds must not.
	commitListing("a", "declared-agent", listing("2.0.0", secondDigest), "agent-configuration.json", reformed)
	runGit(t, "-C", repo, "add", ".")
	runGit(t, "-C", repo, "commit", "-m", "publish 2.0.0")
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}

	if f.scalar("SELECT count(*) FROM plugin_catalog_entries WHERE identifier='declared-agent' AND version='2.0.0'") != 1 {
		t.Fatal("re-sync must publish the new version")
	}
	frozen = f.store.Pool.QueryRow(`SELECT selected_release::text FROM space_plugins WHERE source_namespace='official' AND identifier='declared-agent'`)
	must(t, frozen.Scan(&frozenText))
	if !strings.Contains(frozenText, validDigest) || !strings.Contains(frozenText, `"version": "1.0.0"`) {
		t.Fatalf("a catalog update must not rewrite the frozen selection: %s", frozenText)
	}

	// An undeclared release installs beside it and must reach the Node with no association at
	// all: the Node rejects an empty one, so an invented association blocks every such install.
	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/undeclared"}, "select-undeclared", 200)

	// The workspace install plan carries the frozen association, not the latest catalog content.
	// The fixture controller drives the real claim/dispatch/result boundary, so the durable
	// evidence below crossed the production codec and the input-equality gate.
	created := f.spaceProject(t, sid, "declared-agent-workspace")
	wid := created.O("workspace").S("id")
	if wid == "" {
		t.Fatal("the space project must create its workspace")
	}

	// The install travels as the workspace start operation's plugin step; its frozen plan and the
	// dispatched execution input must both carry the association the selection froze.
	var operationID, requestText string
	must(t, f.store.Pool.QueryRow(`SELECT o.id::text,o.request::text FROM operations o JOIN node_executions ne ON ne.operation_id=o.id AND ne.kind='install_plugins' WHERE o.workspace_id=$1`, wid).Scan(&operationID, &requestText))
	if !strings.Contains(requestText, validDigest) || !strings.Contains(requestText, `"pluginId": "official/declared-agent"`) {
		t.Fatalf("the install plan must carry the frozen association: %s", requestText)
	}

	var inputText string
	must(t, f.store.Pool.QueryRow(`SELECT input::text FROM node_executions WHERE operation_id=$1 AND kind='install_plugins'`, operationID).Scan(&inputText))
	if !strings.Contains(inputText, validDigest) || strings.Contains(inputText, secondDigest) {
		t.Fatalf("the durable execution input must carry the frozen, not the latest, association: %s", inputText)
	}
	expected := map[string]any{"schemaVersion": float64(2), "declarationSha256": validDigest}
	for name, text := range map[string]string{"frozen plan": requestText, "execution input": inputText} {
		plugins := plannedPlugins(t, text)
		declared, undeclared := plugins["official/declared-agent"], plugins["official/undeclared"]
		if declared == nil || undeclared == nil {
			t.Fatalf("%s must plan both plugins: %s", name, text)
		}
		if !reflect.DeepEqual(declared["agentConfiguration"], expected) {
			t.Fatalf("%s must carry the declared association verbatim: %#v", name, declared["agentConfiguration"])
		}
		if association, present := undeclared["agentConfiguration"]; present {
			t.Fatalf("%s must omit the association of an undeclared plugin, got %#v", name, association)
		}
	}
	for _, identifier := range []string{"declared-agent", "undeclared"} {
		if state := f.scalar("SELECT count(*) FROM workspace_plugin_instances WHERE workspace_id=$1 AND identifier=$2 AND observed_state='installed'", wid, identifier); state != 1 {
			t.Fatalf("the install execution must settle the %s workspace plugin instance", identifier)
		}
	}

	if got := packageRequests.Load(); got != 0 {
		t.Fatalf("planning and dispatch must not download release packages, got %d requests", got)
	}

	// The space's plugin state still resolves against the frozen release.
	if state := f.call("GET", f.pluginSpacePath(sid)+"/plugins", nil, "", 200); len(state["items"].([]any)) != 2 {
		t.Fatalf("space plugin list: %+v", state)
	}
}

// The Go compiler mirror is constrained by the same shared vectors as the desktop Rust compiler;
// the sync uses it, so its acceptance is the catalog's acceptance.
func TestAgentConfigurationCompilerIsTheSharedContract(t *testing.T) {
	declaration := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`
	compiled, e := pluginmarket.CompileAgentConfiguration([]byte(declaration))
	if e != nil {
		t.Fatal(e)
	}
	if compiled.SchemaVersion != 2 || len(compiled.Settings) != 0 || compiled.Fingerprint == "" {
		t.Fatalf("compiled declaration: %+v", compiled)
	}
	if _, e := pluginmarket.CompileAgentConfiguration([]byte(`{"schemaVersion":3}`)); e == nil {
		t.Fatal("an unsupported schema version must be rejected, not treated as absent")
	}
}

// 0045 adds the agent declaration snapshot columns to the catalog without touching existing
// entries: a pre-0045 catalog row keeps its facts, the new columns arrive NULL (a legacy entry is
// undeclared, not invalid), and a repeated migration changes nothing.
func TestMigration0045AgentConfigurationCatalogAppliesFreshAndUpgrades(t *testing.T) {
	fresh, _ := testSchema(t, "test_agentcfg39f_")
	migratedTwice(t, newStoreOnSchema(t, fresh))
	var columns int
	must(t, fresh.QueryRow(`SELECT count(*) FROM information_schema.columns
	 WHERE table_schema=current_schema() AND table_name='plugin_catalog_entries'
	 AND column_name IN ('agent_configuration','agent_configuration_issue')`).Scan(&columns))
	if columns != 2 {
		t.Fatalf("fresh schema missing the agent declaration columns: %d", columns)
	}

	pool, _ := testSchema(t, "test_agentcfg39u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0045_agent_configuration_catalog.sql"))
	_, e := pool.Exec(`INSERT INTO plugin_sources(namespace,url) VALUES('official','https://example.invalid/market.git')`)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO plugin_catalog_entries(source_namespace,identifier,title,kind,version,source_url,indexed_at)
	 VALUES('official','legacy-agent','Legacy Agent','agent','1.0.0','https://example.invalid/market.git',now())`)
	must(t, e)
	var before, after string
	must(t, pool.QueryRow(`SELECT row_to_json(e)::text FROM plugin_catalog_entries e WHERE identifier='legacy-agent'`).Scan(&before))
	migratedTwice(t, newStoreOnSchema(t, pool))
	must(t, pool.QueryRow(`SELECT (to_jsonb(e)-'agent_configuration'-'agent_configuration_issue')::text
	 FROM plugin_catalog_entries e WHERE identifier='legacy-agent'`).Scan(&after))
	var equal bool
	must(t, pool.QueryRow("SELECT $1::jsonb=$2::jsonb", before, after).Scan(&equal))
	if !equal {
		t.Fatal("agent declaration migration rewrote an existing catalog entry")
	}
	var undeclared bool
	must(t, pool.QueryRow(`SELECT agent_configuration IS NULL AND agent_configuration_issue IS NULL
	 FROM plugin_catalog_entries WHERE identifier='legacy-agent'`).Scan(&undeclared))
	if !undeclared {
		t.Fatal("migration fabricated a declaration association for a legacy entry")
	}
	if !columnNullable(t, pool, "plugin_catalog_entries", "agent_configuration") ||
		!columnNullable(t, pool, "plugin_catalog_entries", "agent_configuration_issue") {
		t.Fatal("the declaration columns must stay nullable: absence is the undeclared fact")
	}
}

// deref renders one optional string for a failure message.
// plannedPlugins indexes the plugins of a frozen plan or execution input by plugin id.
func plannedPlugins(t *testing.T, text string) map[string]map[string]any {
	t.Helper()
	var document struct {
		Plugins []map[string]any `json:"plugins"`
	}
	must(t, json.Unmarshal([]byte(text), &document))
	out := map[string]map[string]any{}
	for _, plugin := range document.Plugins {
		id, _ := plugin["pluginId"].(string)
		out[id] = plugin
	}
	return out
}

func deref(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}
