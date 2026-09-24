package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

// spacePlugin states the space-level row for one canonical plugin id, with
// the same camelCase keys the API contract exposes.
func (f *fixture) spacePlugin(sid, pluginID string) core.Object {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`SELECT to_jsonb(p) FROM space_plugins p WHERE p.space_id=$1 AND (p.source_namespace||'/'||p.identifier)=$2`, sid, pluginID)
	must(f.t, e)
	defer rows.Close()
	if !rows.Next() {
		return nil
	}
	var b []byte
	must(f.t, rows.Scan(&b))
	var raw map[string]any
	must(f.t, json.Unmarshal(b, &raw))
	out := core.Object{}
	for k, v := range raw {
		parts := strings.Split(k, "_")
		for i := 1; i < len(parts); i++ {
			if parts[i] != "" {
				parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
			}
		}
		out[strings.Join(parts, "")] = v
	}
	return out
}

// TestPluginCatalogAPIAndGating covers IT-3.1/3.2: only active members read
// the catalog and the plugin list, and the catalog exposes the contract shape.
func TestPluginCatalogAPIAndGating(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	// The gate is the space itself: a tenant member who never joined gets 404
	// (no existence leak), a non-tenant member is already stopped by the
	// tenant membership precondition (403), the pre-existing tenant gate.
	created := f.createSpace("Gated", "gated-space", "space-gated")
	sid := created.S("id")
	bob, _ := f.addUser(t, "bob", "Bob")

	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	_, status, e := f.client.Call(context.Background(), "GET", f.path("/spaces/"+sid+"/plugins/catalog"), "gateway", gw, &bob, "", nil)
	must(t, e)
	if status != 404 {
		t.Fatalf("non-member catalog read: want 404 got %d", status)
	}
	_, status, e = f.client.Call(context.Background(), "GET", f.path("/spaces/"+sid+"/plugins"), "gateway", gw, &bob, "", nil)
	must(t, e)
	if status != 404 {
		t.Fatalf("non-member plugin list: want 404 got %d", status)
	}
	outsider := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "outsider"}, Source: "corp", DisplayName: "Outsider"}
	_, status, e = f.client.Call(context.Background(), "GET", f.path("/spaces/"+sid+"/plugins"), "gateway", gw, &outsider, "", nil)
	must(t, e)
	if status != 403 {
		t.Fatalf("non-tenant-member read: want 403 got %d", status)
	}

	catalog := f.call("GET", f.path("/spaces/"+sid+"/plugins/catalog"), nil, "", 200)
	if catalog.S("syncedAt") == "" {
		t.Fatal("catalog must expose the sync freshness timestamp")
	}
	byID := map[string]core.Object{}
	for _, item := range catalog["items"].([]any) {
		o := core.Object(item.(map[string]any))
		byID[o.S("id")] = o
	}
	hello := byID["official/hello-world"]
	if hello.S("kind") != "agent" || hello.S("version") != "1.0.0" || hello.S("title") != "Hello World" {
		t.Fatalf("hello-world catalog entry = %v", hello)
	}
	native := byID["official/native-tool"]
	if native["targets"] == nil {
		t.Fatalf("native-tool must expose its target triples: %v", native)
	}
	if byID["official/all-in-one"].S("kind") != "pack" {
		t.Fatalf("pack listing must be in the catalog: %v", byID["official/all-in-one"])
	}

	plugins := f.call("GET", f.path("/spaces/"+sid+"/plugins"), nil, "", 200)
	if len(plugins["items"].([]any)) != 0 {
		t.Fatalf("fresh space must have no plugins: %v", plugins)
	}
}

// TestPluginInstallAPI covers IT-3.3/3.4/3.5/3.6: the install transaction,
// idempotent replay, strict decoding, and input validation.
func TestPluginInstallAPI(t *testing.T) {
	f := setup(t)
	repo, artifacts := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", artifacts["https://example.invalid/artifacts/hello-1.0.0.orax"])
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "space-project")

	stream, cancel := f.store.Events.Subscribe(sid)
	defer cancel()

	// IT-3.3: the install writes the authoritative row and fans out one
	// instance plus one operation per live runtime workspace.
	out := f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-1", 200)
	row := out.O("resource")
	if row.S("desiredState") != "installed" || row.S("desiredVersion") != "1.0.0" || row.S("observedState") != "pending" {
		t.Fatalf("space plugin row after install = %v", row)
	}
	if f.scalar(`SELECT count(*) FROM workspace_plugin_instances wi JOIN projects p ON p.id=wi.project_id WHERE p.space_id=$1`, sid) != 1 {
		t.Fatal("fan-out must create one instance per live workspace")
	}
	var opID string
	must(t, f.store.Pool.QueryRow(`SELECT o.id::text FROM operations o JOIN projects p ON p.id=o.project_id WHERE p.space_id=$1 AND o.kind='install_plugin'`, sid).Scan(&opID))
	if opID == "" {
		t.Fatal("install must create an install_plugin operation")
	}
	var raw []byte
	must(t, f.store.Pool.QueryRow(`SELECT request FROM operations WHERE id=$1`, opID).Scan(&raw))
	var request core.Object
	must(t, json.Unmarshal(raw, &request))
	if request.S("pluginId") != "official/hello-world" || request.S("version") != "1.0.0" {
		t.Fatalf("operation request = %v", request)
	}
	select {
	case ev := <-stream:
		if ev.Type != "space.plugins_updated" {
			t.Fatalf("SSE event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("install must broadcast space.plugins_updated")
	}

	// IT-3.4: replay with the same idempotency key returns the same response;
	// a fresh install of the same plugin+version does not fan out again.
	replay := f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-1", 200)
	if replay.O("resource").S("version") != row.S("version") {
		t.Fatalf("idempotent replay = %v", replay)
	}
	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-2", 200)
	if f.scalar("SELECT count(*) FROM operations WHERE kind='install_plugin' AND project_id IN (SELECT id FROM projects WHERE space_id=$1)", sid) != 1 {
		t.Fatal("re-installing the same version must not fan out again")
	}

	// IT-3.6: strict decoding and input validation.
	o := f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world", "bogus": true}, "bad-field", 400)
	if o.S("code") != "unknown_field" {
		t.Fatalf("unknown field must be rejected: %v", o)
	}
	o = f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "no-slash"}, "bad-id", 400)
	if o.S("code") != "invalid_plugin_id" {
		t.Fatalf("malformed identifier must be rejected: %v", o)
	}
	o = f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/absent"}, "absent", 404)
	if o.S("code") != "plugin_not_found" {
		t.Fatalf("unknown plugin must be 404: %v", o)
	}
	o = f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/all-in-one"}, "pack", 400)
	if o.S("code") != "plugin_kind_not_installable" {
		t.Fatalf("pack install must be refused in v1: %v", o)
	}
	o = f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world", "pluginVersion": "9.9.9"}, "bad-version", 400)
	if o.S("code") != "plugin_version_unavailable" {
		t.Fatalf("unpinned version must be refused: %v", o)
	}

	// IT-3.5: DELETE without a version is 428; a stale version is 409.
	o = f.call("DELETE", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "del-nover", 428)
	if o.S("code") != "version_required" {
		t.Fatalf("DELETE without version = %v", o)
	}
	o = f.call("DELETE", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world", "version": 2}, "del-stale", 409)
	if o.S("code") != "version_conflict" {
		t.Fatalf("stale DELETE version = %v", o)
	}
}

// TestPluginInstallAndRemoveLifecycle covers IT-4.1/4.4: the full install
// effect chain (plan → simulated Node download with real digest verification →
// advance) and the removal loop, including the SSE writeback broadcasts.
func TestPluginInstallAndRemoveLifecycle(t *testing.T) {
	f := setup(t)
	repo, artifacts := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", artifacts["https://example.invalid/artifacts/hello-1.0.0.orax"])
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "space-project")

	stream, cancel := f.store.Events.Subscribe(sid)
	defer cancel()

	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-life", 200)
	f.drain()
	row := f.spacePlugin(sid, "official/hello-world")
	if row.S("observedState") != "installed" || row.S("observedVersion") != "1.0.0" {
		t.Fatalf("space plugin after drain = %v", row)
	}
	var instanceState string
	must(t, f.store.Pool.QueryRow(`SELECT observed_state FROM workspace_plugin_instances WHERE source_namespace='official' AND identifier='hello-world'`).Scan(&instanceState))
	if instanceState != "installed" {
		t.Fatalf("instance after drain = %s", instanceState)
	}
	// The drain broadcasts at least one writeback event after the install.
	select {
	case ev := <-stream:
		if ev.Type != "space.plugins_updated" {
			t.Fatalf("SSE event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("effect writeback must broadcast space.plugins_updated")
	}

	// IT-4.4: removal fans out remove_plugin, the delete effect converges the
	// instance and aggregate to removed.
	plugins := f.call("GET", f.path("/spaces/"+sid+"/plugins"), nil, "", 200)
	version := core.Object(plugins["items"].([]any)[0].(map[string]any)).N("version")
	removed := f.call("DELETE", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world", "version": version}, "remove-1", 200)
	if removed.O("resource").S("desiredState") != "removed" || removed.O("resource").S("observedState") != "removing" {
		t.Fatalf("removal row = %v", removed)
	}
	f.drain()
	row = f.spacePlugin(sid, "official/hello-world")
	if row.S("observedState") != "removed" {
		t.Fatalf("space plugin after removal drain = %v", row)
	}
	var instanceState2 string
	must(t, f.store.Pool.QueryRow(`SELECT observed_state FROM workspace_plugin_instances WHERE source_namespace='official' AND identifier='hello-world'`).Scan(&instanceState2))
	if instanceState2 != "removed" {
		t.Fatalf("instance after removal drain = %s", instanceState2)
	}
}
