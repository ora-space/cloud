package core

import (
	"context"
	"testing"
)

// readFrozenPluginRelease reads the release package off the run row. Like the frozen version, it is a
// stored creation-time fact: the only way to tell "the creation transaction wrote it" from "something
// resolved it later" is to look at what that transaction left behind.
func readFrozenPluginRelease(t *testing.T, store *Store, runID string) Object {
	t.Helper()
	var frozen Object
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		frozen = tx.one("SELECT plugin_release FROM workflow_runs WHERE id=$1", runID).O("pluginRelease")
		return Object{}
	}); err != nil {
		t.Fatalf("read run plugin release: %v", err)
	}
	return frozen
}

// frozenPluginSnapshot creates the run's Workspace the way the scheduler does and returns the plugin
// plan its create_workspace operation froze — the exact set the Node is told to install.
func frozenPluginSnapshot(t *testing.T, store *Store, runID string) []Object {
	t.Helper()
	var plugins []Object
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		created := createWorkflowRunWorkspace(tx, runID)
		if created.B("busy") {
			t.Fatal("create run workspace: the project must be idle")
		}
		plugins, _ = pluginSnapshot(tx, created.O("operation"))
		return Object{}
	}); err != nil {
		t.Fatalf("create run workspace: %v", err)
	}
	return plugins
}

func pluginByID(plugins []Object, id string) Object {
	for _, plugin := range plugins {
		if plugin.S("pluginId") == id {
			return plugin
		}
	}
	return nil
}

// The run Workspace's plugin step installs the package the run froze, not the Space's current
// selection (circus#527). Before the release was frozen, a run created against 1.0.0 and dispatched
// after an upgrade installed 2.0.0 from the Space, while its session — which carries the frozen
// version — required 1.0.0; the Node then ended the session as `agent_plugin_unavailable` and the run
// failed. Every other plugin still follows the Space: only the Agent CLI identity the session
// dispatches is frozen on the run.
func TestWorkflowRunPluginStepInstallsTheFrozenRelease(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, space, _ := seedWorkflowSessionScene(t, store)
	want := workflowSessionPluginRelease("1.0.0")
	if frozen := readFrozenPluginRelease(t, store, runID); frozen.S("version") != "1.0.0" ||
		frozen.S("url") != want.S("url") || frozen.S("sha256") != want.S("sha256") ||
		len(objectsOf(frozen["targets"])) != 1 {
		t.Fatalf("run froze release %v; want the Space's 1.0.0 package", frozen)
	}

	// An administrator upgrades the Agent CLI and installs an unrelated plugin while the run waits.
	if _, err := store.Pool.Exec(`UPDATE space_plugins SET desired_version='2.0.0',selected_release=$2::jsonb,
		desired_revision=desired_revision+1,version=version+1 WHERE space_id=$1`, space, jsonText(workflowSessionPluginRelease("2.0.0"))); err != nil {
		t.Fatalf("upgrade plugin: %v", err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state,selected_release)
		SELECT space_id,tenant_id,'official','ora-space.other','installed','3.0.0','installed',$2::jsonb FROM space_plugins WHERE space_id=$1`,
		space, jsonText(Object{"id": "official/ora-space.other", "version": "3.0.0", "url": "https://example.invalid/other-3.0.0.tgz"})); err != nil {
		t.Fatalf("install another plugin: %v", err)
	}

	plugins := frozenPluginSnapshot(t, store, runID)
	agent := pluginByID(plugins, opencodeAgentCLI)
	if agent == nil || agent.S("version") != "1.0.0" ||
		agent.O("universal").S("url") != want.S("url") || agent.O("universal").S("sha256") != want.S("sha256") {
		t.Fatalf("agent plugin in the plugin step = %v; want the run's frozen 1.0.0 package, not the Space's 2.0.0", agent)
	}
	if other := pluginByID(plugins, "official/ora-space.other"); other == nil || other.S("version") != "3.0.0" {
		t.Fatalf("other plugin = %v; want the Space's current 3.0.0 selection", other)
	}
}

// A plugin removed after the run was created is not reinstalled for it. The frozen package only
// replaces an entry the Space still selects; reinstalling it would undo an administrator's decision
// for a run that has not started yet, and the start gate is what reports that bounded failure — the
// run fails as `agent_plugin_unavailable` before any session is handed over (#518, #526).
func TestWorkflowRunPluginStepDoesNotReinstallARemovedPlugin(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, space, _ := seedWorkflowSessionScene(t, store)
	if _, err := store.Pool.Exec(`UPDATE space_plugins SET desired_state='removed',
		desired_revision=desired_revision+1,version=version+1 WHERE space_id=$1`, space); err != nil {
		t.Fatalf("remove plugin: %v", err)
	}
	var plugins []Object
	var run, ws Object
	var gap string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		created := createWorkflowRunWorkspace(tx, runID)
		if created.B("busy") {
			t.Fatal("create run workspace: the project must be idle")
		}
		operation := created.O("operation")
		plugins, _ = pluginSnapshot(tx, operation)
		enterPluginStep(tx, operation)
		run = tx.one("SELECT * FROM workflow_runs WHERE id=$1", runID)
		ws = tx.one("SELECT * FROM workspaces WHERE id=$1", created.O("workspace").S("id"))
		gap = workflowPinnedPluginGap(tx, run, ws)
		return Object{}
	}); err != nil {
		t.Fatalf("stage plugin step: %v", err)
	}
	if agent := pluginByID(plugins, opencodeAgentCLI); agent != nil {
		t.Fatalf("the plugin step reinstalled a removed plugin from the frozen package: %v", agent)
	}
	// Nothing was installed, so the instance the start gate reads is missing and the run fails with
	// the same bounded reason as a Workspace whose install failed.
	if gap != "agent_plugin_unavailable" {
		t.Fatalf("start gate reports %q for a plugin removed after creation; want agent_plugin_unavailable", gap)
	}
}

// The package is frozen only when the Space's selection is the very version being frozen. A selection
// naming another release — one still converging, or a catalog entry replaced under the same version —
// has no package that honestly answers for the frozen version, so the run keeps the 0042 behavior:
// the version is still frozen, no release is recorded, and the start gate reports the mismatch.
func TestWorkflowRunPluginReleaseRequiresTheSameSelection(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, uid, space, project := seedWorkflowSessionScene(t, store)
	if _, err := store.Pool.Exec(`UPDATE space_plugins SET selected_release=jsonb_set(selected_release,'{version}','"0.9.0"') WHERE space_id=$1`, space); err != nil {
		t.Fatalf("replace selected release: %v", err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		version, release := resolveWorkflowRunPlugin(tx, tid, project)
		if version != "1.0.0" || release != nil {
			t.Fatalf("resolved version=%v release=%v; want the 1.0.0 version and no package", version, release)
		}
		origin := tx.one("SELECT * FROM workflow_runs WHERE id=$1", runID)
		snapshot := tx.one("SELECT * FROM workflow_snapshots WHERE id=$1", origin.S("snapshotId"))
		created := insertWorkflowRun(tx, tid, origin.S("workflowId"), snapshot, Object{}, "mismatched", uid, project)
		if frozen := created.O("pluginRelease"); frozen.S("version") != "" {
			t.Fatalf("a mismatched selection froze release %v", frozen)
		}
		if created.S("pluginVersion") != "1.0.0" {
			t.Fatalf("plugin version = %q; want 1.0.0 frozen regardless of the package", created.S("pluginVersion"))
		}
		return Object{}
	}); err != nil {
		t.Fatal(err)
	}
}
