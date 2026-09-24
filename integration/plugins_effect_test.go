package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// substrateRun dispatches one planned effect to the simulated Node and returns
// the journal entry; the caller asserts the expected status.
func (f *fixture) substrateRun(t *testing.T, effect core.Object, wantStatus int) core.Object {
	t.Helper()
	body, e := json.Marshal(effect.O("request"))
	must(t, e)
	req, e := http.NewRequest(http.MethodPut, f.external.URL+"/effects/"+effect.S("id"), bytes.NewReader(body))
	must(t, e)
	req.Header.Set("Content-Type", "application/json")
	resp, e := f.client.HTTP.Do(req)
	must(t, e)
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("substrate PUT: want %d got %d", wantStatus, resp.StatusCode)
	}
	var out core.Object
	must(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// substrateSucceed dispatches an effect and requires the simulated Node to
// succeed (real digest verified against the mapped artifact).
func (f *fixture) substrateSucceed(t *testing.T, effect core.Object) core.Object {
	t.Helper()
	out := f.substrateRun(t, effect, http.StatusOK)
	if out.S("state") != "succeeded" {
		t.Fatalf("substrate result = %v", out)
	}
	return out
}

// substrateFailed dispatches an effect and requires the simulated Node to
// fail it (digest mismatch), returning the failed journal entry.
func (f *fixture) substrateFailed(t *testing.T, effect core.Object) core.Object {
	t.Helper()
	out := f.substrateRun(t, effect, http.StatusServiceUnavailable)
	if out.S("state") != "failed" {
		t.Fatalf("substrate result = %v", out)
	}
	return out
}

// substrateRerun dispatches the same effect again after the failure was
// corrected; the journal keeps the stable external binding.
func (f *fixture) substrateRerun(t *testing.T, effect core.Object) core.Object {
	t.Helper()
	return f.substrateSucceed(t, effect)
}

// claimPluginOp claims the next queued operation and requires it to be the
// plugin kind, returning its snapshot row.
func (f *fixture) claimPluginOp(t *testing.T, kind string) core.Object {
	t.Helper()
	snap, e := f.client.Control(context.Background(), "/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch})
	must(t, e)
	op := snap.O("operation")
	if op == nil || op.S("kind") != kind {
		t.Fatalf("claim returned %v, want %s operation", op, kind)
	}
	return op
}

// controlStep drives one bounded control command for the claimed operation.
func (f *fixture) controlStep(t *testing.T, op core.Object, path string, body core.Object, wantCode int) core.Object {
	t.Helper()
	body["epoch"], body["version"] = op.N("controllerEpoch"), op.N("version")
	out, status, e := f.client.Call(context.Background(), "POST", "/internal/v1/operations/"+op.S("id")+path, "controller", core.Claims{Subject: f.client.Subject}, nil, "", body)
	must(t, e)
	if status != wantCode {
		t.Fatalf("control %s: want %d got %d %v", path, wantCode, status, out)
	}
	return out
}

// TestPluginEffectChainAndEvidence covers IT-4.1/4.2/4.6: the effect payload is
// self-contained, the digest verification is real and mandatory, failed
// effects surface on the fan-out rows, and success evidence is validated.
func TestPluginEffectChainAndEvidence(t *testing.T) {
	f := setup(t)
	repo, artifacts := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	created := f.spaceProject(t, sid, "space-project")
	wid := created.O("workspace").S("id")
	// Map a WRONG artifact first: the digest in the manifest cannot match.
	wrong := filepath.Join(f.root, "wrong.orax")
	must(t, os.WriteFile(wrong, []byte("tampered bytes\n"), 0o600))
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", wrong)

	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-effect", 200)
	op := f.claimPluginOp(t, "install_plugin")
	planned := f.controlStep(t, op, "/effects", core.Object{"kind": "plugin_ensure", "workspaceId": wid}, 200)
	effect := planned.O("effect")
	request := effect.O("request")
	// IT-4.1: the payload carries the complete release straight from the
	// catalog snapshot — the Node needs no registry index of its own.
	if request.S("pluginId") != "official/hello-world" || request.S("version") != "1.0.0" {
		t.Fatalf("effect request = %v", request)
	}
	universal := request.O("universal")
	if universal.S("url") != "https://example.invalid/artifacts/hello-1.0.0.orax" || len(universal.S("sha256")) != 64 {
		t.Fatalf("effect universal release = %v", universal)
	}
	row := f.spacePlugin(sid, "official/hello-world")
	if row.S("observedState") != "installing" {
		t.Fatalf("planned install must mark the row installing: %v", row)
	}

	// IT-4.2: the simulated Node verifies the digest and fails the download.
	// Subscribe before the writeback so the broadcast is not missed.
	op = planned.O("operation")
	stream, cancelEvents := f.store.Events.Subscribe(sid)
	defer cancelEvents()
	failed := f.substrateFailed(t, effect)
	result := f.controlStep(t, op, "/effects/"+effect.S("id")+"/result", core.Object{"state": "failed", "externalId": failed.S("externalId"), "result": failed.O("result")}, 200)
	row = f.spacePlugin(sid, "official/hello-world")
	if row.S("observedState") != "failed" || row.S("installError") == "" {
		t.Fatalf("failed download must surface on the row: %v", row)
	}
	select {
	case ev := <-stream:
		if ev.Type != "space.plugins_updated" {
			t.Fatalf("writeback event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed writeback must broadcast space.plugins_updated")
	}

	// Fix the artifact and retry the same effect by stable id: the journal is
	// reused, the download succeeds, evidence is validated, advance converges.
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", artifacts["https://example.invalid/artifacts/hello-1.0.0.orax"])
	op = result.O("operation")
	external := f.substrateRerun(t, effect)
	succeeded := f.controlStep(t, op, "/effects/"+effect.S("id")+"/result", core.Object{"state": "succeeded", "externalId": external.S("externalId"), "result": external.O("result")}, 200)
	op = succeeded.O("operation")
	f.controlStep(t, op, "/advance", core.Object{}, 200)
	row = f.spacePlugin(sid, "official/hello-world")
	if row.S("observedState") != "installed" || row.S("observedVersion") != "1.0.0" {
		t.Fatalf("successful install must converge the row: %v", row)
	}

	// IT-4.6: success evidence is validated strictly — an installed flag
	// without the exact version is refused before any writeback.
	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/native-tool"}, "install-evidence", 200)
	op = f.claimPluginOp(t, "install_plugin")
	planned = f.controlStep(t, op, "/effects", core.Object{"kind": "plugin_ensure", "workspaceId": wid}, 200)
	badEvidence := f.controlStep(t, planned.O("operation"), "/effects/"+planned.O("effect").S("id")+"/result",
		core.Object{"state": "succeeded", "externalId": "sim-evidence", "result": core.Object{"installed": true}}, 400)
	if badEvidence.S("code") != "invalid_plugin_evidence" {
		t.Fatalf("missing evidence = %v", badEvidence)
	}
}

// TestPluginEffectAdmissionGate covers IT-4.3: plugin_ensure dispatches only
// to a ready workspace, mirroring the node step gate.
func TestPluginEffectAdmissionGate(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	created := f.spaceProject(t, sid, "space-project")
	wid := created.O("workspace").S("id")
	// Stop the workspace: it stays live (fan-out still targets it) but is no
	// longer ready, so the effect plan must refuse dispatch.
	ws := f.ws(wid)
	f.call("POST", f.path("/workspaces/"+wid+"/stop"), core.Object{"version": ws.N("version")}, "stop-1", 202)
	f.drain()

	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-admission", 200)
	op := f.claimPluginOp(t, "install_plugin")
	refused := f.controlStep(t, op, "/effects", core.Object{"kind": "plugin_ensure", "workspaceId": wid}, 409)
	if refused.S("code") != "workspace_not_ready" {
		t.Fatalf("plan on a stopped workspace = %v", refused)
	}
}

// TestPluginAggregationMatrix covers IT-4.5: the two-instance intermediate
// states aggregate deterministically, and a space without live workspaces
// converges immediately.
func TestPluginAggregationMatrix(t *testing.T) {
	f := setup(t)
	repo, artifacts := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", artifacts["https://example.invalid/artifacts/hello-1.0.0.orax"])
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "project-one")
	f.spaceProject(t, sid, "project-two")

	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-agg", 200)

	// First workspace: plan → installing; complete → still installing (the
	// second workspace is pending).
	op := f.claimPluginOp(t, "install_plugin")
	wid1 := op.S("workspaceId")
	planned := f.controlStep(t, op, "/effects", core.Object{"kind": "plugin_ensure", "workspaceId": wid1}, 200)
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installing" {
		t.Fatal("one in-flight install must aggregate to installing")
	}
	effect := planned.O("effect")
	external := f.substrateSucceed(t, effect)
	op = planned.O("operation")
	result := f.controlStep(t, op, "/effects/"+effect.S("id")+"/result", core.Object{"state": "succeeded", "externalId": external.S("externalId"), "result": external.O("result")}, 200)
	f.controlStep(t, result.O("operation"), "/advance", core.Object{}, 200)
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installing" {
		t.Fatal("one installed plus one pending must aggregate to installing")
	}

	// Second workspace completes → installed.
	op = f.claimPluginOp(t, "install_plugin")
	wid2 := op.S("workspaceId")
	if wid2 == wid1 {
		t.Fatal("second operation must bind the second workspace")
	}
	planned = f.controlStep(t, op, "/effects", core.Object{"kind": "plugin_ensure", "workspaceId": wid2}, 200)
	effect = planned.O("effect")
	external = f.substrateSucceed(t, effect)
	op = planned.O("operation")
	result = f.controlStep(t, op, "/effects/"+effect.S("id")+"/result", core.Object{"state": "succeeded", "externalId": external.S("externalId"), "result": external.O("result")}, 200)
	f.controlStep(t, result.O("operation"), "/advance", core.Object{}, 200)
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installed" {
		t.Fatal("all instances installed must aggregate to installed")
	}

	// A space without live runtime workspaces converges immediately.
	other := f.createSpace("Empty", "empty-space", "space-empty")
	o := f.call("POST", f.path("/spaces/"+other.S("id")+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-empty", 200)
	if o.O("resource").S("observedState") != "installed" {
		t.Fatalf("no live workspaces means nothing to do: %v", o)
	}
}

// TestPluginIsolationSerializationAndRecovery covers IT-5.1/5.2/5.4:
// cross-space isolation, the one_project_operation serialization, and
// controller takeover recovery.
func TestPluginIsolationSerializationAndRecovery(t *testing.T) {
	f := setup(t)
	repo, artifacts := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", artifacts["https://example.invalid/artifacts/hello-1.0.0.orax"])
	f.substrate.MapArtifact("https://example.invalid/artifacts/tool-linux.orax", artifacts["https://example.invalid/artifacts/tool-linux.orax"])
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "project-one")

	// IT-5.2: an in-flight install serializes the project; a second install
	// while the first operation is queued is refused.
	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/hello-world"}, "install-serial", 200)
	o := f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/native-tool"}, "install-serial-2", 409)
	if o.S("code") != "operation_in_progress" {
		t.Fatalf("second install on a busy project = %v", o)
	}
	f.drain()

	// IT-5.1: another space never sees the first space's plugins, even though
	// both serve the same catalog snapshot.
	other := f.createSpace("Other", "other-space", "space-other")
	plugins := f.call("GET", f.path("/spaces/"+other.S("id")+"/plugins"), nil, "", 200)
	if len(plugins["items"].([]any)) != 0 {
		t.Fatalf("space isolation leaked plugins: %v", plugins)
	}
	// The composite foreign keys reject cross-tenant instance rows.
	if _, e := f.store.Pool.Exec(`INSERT INTO workspace_plugin_instances(workspace_id,tenant_id,owner_user_id,project_id,source_namespace,identifier) SELECT w.id,$1,w.owner_user_id,w.project_id,'official','hello-world' FROM workspaces w LIMIT 1`, sid); e == nil {
		t.Fatal("cross-tenant instance insert must be rejected by the composite foreign key")
	}

	// IT-5.4: controller takeover — the first controller plans the effect and
	// is lost before dispatch; a second controller reconciles by stable id.
	f.call("POST", f.path("/spaces/"+sid+"/plugins"), core.Object{"identifier": "official/native-tool"}, "install-recover", 200)
	op := f.claimPluginOp(t, "install_plugin")
	_ = f.controlStep(t, op, "/effects", core.Object{"kind": "plugin_ensure", "workspaceId": op.S("workspaceId")}, 200)
	if _, e := f.client.Control(context.Background(), "/internal/v1/controller-lease/release", core.Object{"epoch": f.controller.Epoch}); e != nil {
		t.Fatal(e)
	}
	replacement := &simulator.Controller{Client: &simulator.Client{URL: f.cloud.URL, Credentials: f.client.Credentials, HTTP: f.client.HTTP, Subject: "controller-replacement"}, SubstrateURL: f.external.URL}
	if e := replacement.Acquire(context.Background()); e != nil {
		t.Fatal(e)
	}
	// The replacement drains the abandoned operation: the planned effect is
	// reconciled from the journal and the install converges.
	if e := replacement.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	row := f.spacePlugin(sid, "official/native-tool")
	if row.S("observedState") != "installed" {
		t.Fatalf("takeover recovery must converge the install: %v", row)
	}
}

// TestPluginConcurrentInstallsAcrossSpaces runs under task test:race: two
// spaces install plugins concurrently and both converge; the advisory lock and
// unique constraints keep the writes serialized and duplicate-free.
func TestPluginConcurrentInstallsAcrossSpaces(t *testing.T) {
	f := setup(t)
	repo, artifacts := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	f.substrate.MapArtifact("https://example.invalid/artifacts/hello-1.0.0.orax", artifacts["https://example.invalid/artifacts/hello-1.0.0.orax"])
	spaceA := f.defaultSpaceID()
	spaceB := f.createSpace("Bee", "bee-space", "space-bee")
	f.spaceProject(t, spaceA, "project-a")
	f.spaceProject(t, spaceB.S("id"), "project-b")

	var wg sync.WaitGroup
	results := make(chan string, 2)
	for _, install := range []struct{ sid, id, key string }{
		{spaceA, "official/hello-world", "concurrent-a"},
		{spaceB.S("id"), "official/hello-world", "concurrent-b"},
	} {
		wg.Add(1)
		go func(sid, id, key string) {
			defer wg.Done()
			_, status, e := f.client.Call(context.Background(), "POST", f.path("/spaces/"+sid+"/plugins"), "gateway",
				core.Claims{Subject: "gateway-a"}, &f.user, key, core.Object{"identifier": id})
			results <- fmt.Sprintf("%s:%d:%v", key, status, e)
		}(install.sid, install.id, install.key)
	}
	wg.Wait()
	close(results)
	for r := range results {
		if !strings.Contains(r, ":200:<nil>") {
			t.Fatalf("concurrent install failed: %s", r)
		}
	}
	f.drain()
	for _, sid := range []string{spaceA, spaceB.S("id")} {
		if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installed" {
			t.Fatalf("space %s must converge to installed", sid)
		}
	}
}
