package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// claimPluginOp claims the next queued operation and requires it to be the
// plugin kind, returning its snapshot row.
func (f *fixture) claimPluginOp(t *testing.T, kind string) core.Object {
	t.Helper()
	f.acknowledgeSimulatorBindings() // Explicitly close the previous fixture intent before recovery scanning.
	snap, e := f.client.Control(context.Background(), "/internal/v1/operations/claim", core.Object{"epoch": f.controller.Epoch})
	must(t, e)
	op := snap.O("operation")
	if op == nil || op.S("kind") != kind {
		t.Fatalf("claim returned %v, want %s operation", op, kind)
	}
	f.acknowledgeSimulatorBindings() // Only the fixture Node acknowledges its maintenance binding.
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

// TestPluginEffectChainAndEvidence covers the Node execution that replaced plugin_ensure:
// the input is the catalog snapshot, a failed item is recorded without blocking the step, and an
// installed version that is not the planned one is refused before any writeback.
func TestPluginEffectChainAndEvidence(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	created := f.spaceProject(t, sid, "space-project")
	wid := created.O("workspace").S("id")

	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-effect", 200)
	op := f.claimPluginOp(t, "install_plugin")
	plugins := objectList(op.O("request")["plugins"])
	if len(plugins) != 1 || plugins[0].S("pluginId") != "official/hello-world" || plugins[0].S("version") != "1.0.0" {
		t.Fatalf("plugin input = %v", plugins)
	}
	universal := plugins[0].O("universal")
	if universal.S("url") != "https://example.invalid/artifacts/hello-1.0.0.orax" || len(universal.S("sha256")) != 64 {
		t.Fatalf("plugin universal release = %v", universal)
	}
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installing" {
		t.Fatalf("entering the step must mark the row installing: %v", f.spacePlugin(sid, "official/hello-world"))
	}
	if _, e := f.store.Pool.Exec(`INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request) VALUES($1,$2,$3,$4,'plugin_ensure','planned','{}')`, uuid.NewString(), op.S("id"), op.S("projectId"), wid); e == nil {
		t.Fatal("new plugin effects must be refused")
	}

	stream, cancelEvents := f.store.Events.Subscribe(sid)
	defer cancelEvents()
	f.dispatchPlugin(t, op, "exec-fail")
	must(t, f.reportPlugin(t, op, "exec-fail", []*controlpb.PluginItemResult{{
		PluginId: "official/hello-world",
		Outcome:  &controlpb.PluginItemResult_Failed{Failed: &controlpb.PluginItemFailed{Reason: controlpb.PluginFailureReason_PLUGIN_FAILURE_REASON_CHECKSUM_MISMATCH}},
	}}))
	row := f.spacePlugin(sid, "official/hello-world")
	if row.S("observedState") != "failed" || row.S("installError") != "checksum_mismatch" {
		t.Fatalf("failed item must surface on the row: %v", row)
	}
	select {
	case ev := <-stream:
		if ev.Type != "space.plugins_updated" {
			t.Fatalf("writeback event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed writeback must broadcast space.plugins_updated")
	}
	f.controlStep(t, op, "/advance", core.Object{}, 200)
	if f.ws(wid).S("observedState") != "ready" {
		t.Fatal("one failed plugin must not block the workspace")
	}

	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/native-tool"}, "install-evidence", 200)
	op = f.claimPluginOp(t, "install_plugin")
	f.dispatchPlugin(t, op, "exec-bad-version")
	err := f.reportPlugin(t, op, "exec-bad-version", []*controlpb.PluginItemResult{{
		PluginId: "official/native-tool",
		Outcome:  &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: "9.9.9"}},
	}})
	expectStatus(t, err, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
	if f.spacePlugin(sid, "official/native-tool").S("observedState") != "installing" {
		t.Fatal("rejected evidence must leave the instance unchanged")
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
	f.call("POST", f.path("/workspaces/"+wid+"/stop"), f.lifecycleBody(wid, ws.N("version")), "stop-1", 202)
	f.drain()

	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-admission", 200)
	if f.scalar("SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='install_plugin'", wid) != 0 {
		t.Fatal("stopped runtime acquired plugin maintenance")
	}
	var reason string
	must(t, f.store.Pool.QueryRow("SELECT pending_reason FROM workspace_plugin_instances WHERE workspace_id=$1", wid).Scan(&reason))
	if reason != "waiting_start" || f.ws(wid).S("observedState") != "stopped" {
		t.Fatalf("stopped pending: %s", reason)
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

	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-agg", 200)

	op := f.claimPluginOp(t, "install_plugin")
	wid1 := op.S("workspaceId")
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installing" {
		t.Fatal("one in-flight install must aggregate to installing")
	}
	f.finishClaimedPlugin(t, op)
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installing" {
		t.Fatal("one installed plus one pending must aggregate to installing")
	}
	op = f.claimPluginOp(t, "install_plugin")
	if op.S("workspaceId") == wid1 {
		t.Fatal("second operation must bind the second workspace")
	}
	f.finishClaimedPlugin(t, op)
	if f.spacePlugin(sid, "official/hello-world").S("observedState") != "installed" {
		t.Fatal("all instances installed must aggregate to installed")
	}

	// A space without live runtime workspaces converges immediately.
	other := f.createPluginTenant("Empty", "empty-space", "space-empty")
	o := f.call("POST", f.pluginSpacePath(other.S("id"))+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-empty", 200)
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
	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-serial", 200)
	o := f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/native-tool"}, "install-serial-2", 200)
	if o.O("resource").S("observedState") != "pending" {
		t.Fatalf("second install on a busy project = %v", o)
	}
	f.completeNextPlugin(t, "install_plugin")

	// IT-5.1: another space never sees the first space's plugins, even though
	// both serve the same catalog snapshot.
	other := f.createPluginTenant("Other", "other-space", "space-other")
	plugins := f.call("GET", f.pluginSpacePath(other.S("id"))+"/plugins", nil, "", 200)
	if len(plugins["items"].([]any)) != 0 {
		t.Fatalf("space isolation leaked plugins: %v", plugins)
	}
	// The composite foreign keys reject cross-tenant instance rows.
	if _, e := f.store.Pool.Exec(`INSERT INTO workspace_plugin_instances(workspace_id,tenant_id,owner_user_id,project_id,source_namespace,identifier) SELECT w.id,$1,w.owner_user_id,w.project_id,'official','hello-world' FROM workspaces w LIMIT 1`, sid); e == nil {
		t.Fatal("cross-tenant instance insert must be rejected by the composite foreign key")
	}

	// IT-5.4: controller takeover — the first controller plans the effect and
	// is lost before dispatch; a second controller reconciles by stable id.
	f.acknowledgeSimulatorBindings()
	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/native-tool", "version": f.spacePlugin(sid, "official/native-tool").N("version")}, "install-recover", 200)
	_ = f.claimPluginOp(t, "install_plugin")
	if _, e := f.client.Control(context.Background(), "/internal/v1/controller-lease/release", core.Object{"epoch": f.controller.Epoch}); e != nil {
		t.Fatal(e)
	}
	replacement := &simulator.Controller{Client: &simulator.Client{URL: f.cloud.URL, Credentials: f.client.Credentials, HTTP: f.client.HTTP, Subject: "controller-replacement"}, SubstrateURL: f.external.URL, Executions: f.executions}
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
	spaceB := f.createPluginTenant("Bee", "bee-space", "space-bee")
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
			_, status, e := f.client.Call(context.Background(), "POST", f.pluginSpacePath(sid)+"/plugins", "gateway",
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

// completeNextPlugin claims the next plugin operation and reports a successful Node execution.
// The report is simulated evidence of Cloud's writeback rules, not of a package installed on disk.
func (f *fixture) completeNextPlugin(t *testing.T, kind string) {
	t.Helper()
	f.acknowledgeSimulatorBindings()
	f.finishClaimedPlugin(t, f.claimPluginOp(t, kind))
}

func (f *fixture) finishClaimedPlugin(t *testing.T, op core.Object) {
	t.Helper()
	execution := "exec-" + op.S("id")
	f.dispatchPlugin(t, op, execution)
	must(t, f.reportPlugin(t, op, execution, successPluginItems(op)))
	f.controlStep(t, op, "/advance", core.Object{}, 200)
}

func (f *fixture) dispatchPlugin(t *testing.T, op core.Object, execution string) {
	t.Helper()
	nodeID, _ := f.workspaceNode(t, op.S("workspaceId"))
	_, e := f.executions.RecordDispatch(asController(f.client.Subject), &controlpb.RecordDispatchRequest{
		SubmissionId: "dispatch-" + execution, Epoch: op.N("controllerEpoch"), OperationId: op.S("id"),
		ExecutionId: execution, NodeId: nodeID, Input: pluginInputProto(op),
	})
	must(t, e)
}

func (f *fixture) reportPlugin(t *testing.T, op core.Object, execution string, items []*controlpb.PluginItemResult) error {
	t.Helper()
	nodeID, incarnation := f.workspaceNode(t, op.S("workspaceId"))
	_, e := f.executions.RecordQueriedResult(asController(f.client.Subject), &controlpb.RecordQueriedResultRequest{
		SubmissionId: "result-" + execution, Epoch: op.N("controllerEpoch"), OperationId: op.S("id"), ExecutionId: execution,
		Result: &controlpb.ExecutionResult{
			Node:    &controlpb.NodeIdentity{NodeId: nodeID, NodeIncarnationId: incarnation},
			Outcome: &controlpb.ExecutionResult_PluginsResult{PluginsResult: &controlpb.PluginsResult{Items: items}},
		},
	})
	return e
}

func (f *fixture) workspaceNode(t *testing.T, wid string) (string, string) {
	t.Helper()
	var id, incarnation string
	must(t, f.store.Pool.QueryRow(`SELECT n.node_id,n.node_incarnation_id FROM node_instances n JOIN sandbox_instances s ON s.id=n.sandbox_instance_id WHERE s.workspace_id=$1 AND n.ended_at IS NULL ORDER BY n.id DESC LIMIT 1`, wid).Scan(&id, &incarnation))
	return id, incarnation
}

func successPluginItems(op core.Object) []*controlpb.PluginItemResult {
	plugins := objectList(op.O("request")["plugins"])
	items := make([]*controlpb.PluginItemResult, 0, len(plugins))
	for _, plugin := range plugins {
		if op.S("kind") == "remove_plugin" {
			items = append(items, &controlpb.PluginItemResult{PluginId: plugin.S("pluginId"), Outcome: &controlpb.PluginItemResult_Removed{Removed: &controlpb.PluginItemRemoved{}}})
			continue
		}
		items = append(items, &controlpb.PluginItemResult{PluginId: plugin.S("pluginId"), Outcome: &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: plugin.S("version")}}})
	}
	return items
}

func pluginInputProto(op core.Object) *controlpb.ExecutionInput {
	plugins := objectList(op.O("request")["plugins"])
	if op.S("kind") == "remove_plugin" {
		items := make([]*controlpb.PluginRemoval, 0, len(plugins))
		for _, plugin := range plugins {
			items = append(items, &controlpb.PluginRemoval{PluginId: plugin.S("pluginId"), Version: plugin.S("version")})
		}
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_RemovePlugins{RemovePlugins: &controlpb.RemovePluginsSpec{Plugins: items}}}
	}
	items := make([]*controlpb.PluginInstall, 0, len(plugins))
	for _, plugin := range plugins {
		item := &controlpb.PluginInstall{PluginId: plugin.S("pluginId"), Version: plugin.S("version")}
		if universal := plugin.O("universal"); universal.S("url") != "" {
			item.Universal = &controlpb.PluginDownload{Url: universal.S("url"), Sha256: universal.S("sha256")}
		}
		for _, target := range objectList(plugin["targets"]) {
			item.Targets = append(item.Targets, &controlpb.PluginTargetDownload{Target: target.S("target"), Download: &controlpb.PluginDownload{Url: target.S("url"), Sha256: target.S("sha256")}})
		}
		// Mirror the Controller: a present association is forwarded as received, an absent one
		// produces no message. Object.O would invent an empty association for every plugin.
		if raw, declared := plugin["agentConfiguration"].(map[string]any); declared {
			agent := core.Object(raw)
			item.AgentConfiguration = &controlpb.PluginAgentConfiguration{SchemaVersion: uint32(agent.N("schemaVersion")), DeclarationSha256: agent.S("declarationSha256")}
		}
		items = append(items, item)
	}
	return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_InstallPlugins{InstallPlugins: &controlpb.InstallPluginsSpec{Plugins: items}}}
}

func objectList(v any) []core.Object {
	list, _ := v.([]any)
	out := make([]core.Object, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, core.Object(m))
		}
	}
	return out
}
