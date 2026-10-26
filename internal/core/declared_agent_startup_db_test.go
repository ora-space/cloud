package core

import (
	"context"
	"strings"
	"testing"
)

// The real freeze/create path plus a completed clone supplies a dispatch-ready baseline.
func declaredStartupScene(t *testing.T) (personalAgentScene, string) {
	t.Helper()
	scene := newPersonalAgentScene(t)
	if _, err := scene.request("PUT", "", newID(), scene.body(0)); err != nil {
		t.Fatal(err)
	}
	scene.store.PersonalAgentRuntime = &PersonalAgentRuntime{Target: "x86_64-unknown-linux-gnu", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}
	result, err := scene.store.transact(context.Background(), func(tx *transaction) Object {
		input := Object{"agentPluginId": claudeAgentCLI, "agentPluginVersion": "1.0.0"}
		bindRunModel(tx, scene.seed.run, scene.uid, input, "")
		created := createRunWorkspace(tx, scene.seed.run)
		workspace, operation := created.O("workspace").S("id"), created.O("operation").S("id")
		tx.exec("UPDATE operations SET state='succeeded',step='done',version=version+1 WHERE id=$1", operation)
		finishRuntimeMaintenance(tx, operation)
		tx.exec("UPDATE workspaces SET observed_state='ready',admission_open=true,base_commit_id=$2 WHERE id=$1", workspace, baseCommit)
		tx.exec(`INSERT INTO clone_executions(execution_id,operation_id,node_id,node_operation_id,workspace_id,input,result,dispatched_epoch)
          VALUES($1,$2,'node-checkout','op-checkout',$3,'{"kind":"clone"}',jsonb_build_object('outcome','clone_ready','commit',$4::text),1)`, "clone-"+workspace, operation, workspace, baseCommit)
		tx.exec("UPDATE issue_runs SET phase='starting',status='dispatched',workspace_id=$2 WHERE id=$1", scene.seed.run, workspace)
		return Object{"workspaceId": workspace}
	})
	if err != nil {
		t.Fatal(err)
	}
	return scene, result.S("workspaceId")
}

// Dispatch reads private frozen facts, never the latest personal options or a public prompt field.
func TestDeclaredAgentSessionSnapshotIsPrivateAndImmutable(t *testing.T) {
	scene, _ := declaredStartupScene(t)
	body := scene.body(1)
	body["settings"] = Object{"style": "edited-after-admission"}
	if _, err := scene.request("PUT", "", newID(), body); err != nil {
		t.Fatal(err)
	}
	spec, err := scene.store.transact(context.Background(), func(tx *transaction) Object {
		run := tx.one("SELECT * FROM issue_runs WHERE id=$1", scene.seed.run)
		spec, err := scene.store.sessionStartSpec(tx, run, run.O("input"), "turn-1", "task", nil)
		if err != nil {
			t.Fatal(err)
		}
		return spec
	})
	if err != nil {
		t.Fatal(err)
	}
	configuration := spec.O("agentConfiguration")
	if configuration.N("configurationRevision") != 1 || configuration.S("protocol") != "anthropic-messages" || configuration.O("model").S("id") != "vendor/model-v1" || !strings.Contains(configuration.S("settingsJson"), "concise") || strings.Contains(jsonText(spec), "edited-after-admission") {
		t.Fatalf("frozen input changed: %v", configuration)
	}
	if strings.Contains(jsonText(spec.O("initialTurn")), "concise") {
		t.Fatal("personal settings entered the prompt")
	}
}

// A reachable legacy Node has no v1 adapter proof; the run settles before any first prompt.
func TestDeclaredAgentCapabilityRefusalSettlesBeforePrompt(t *testing.T) {
	scene, workspace := declaredStartupScene(t)
	bindConnectedNode(t, scene.store, workspace)
	if _, err := scene.store.Pool.Exec("UPDATE node_instances SET model_proxy=true WHERE workspace_id=$1", workspace); err != nil {
		t.Fatal(err)
	}
	startupPass(t, scene.store)
	phase, status, reason, _, _, _ := runFields(t, scene.store, scene.seed.run)
	if phase.String != "releasing" || status != "failed" || reason != "agent_adapter_unavailable" {
		t.Fatalf("missing adapter did not settle: %s/%s/%s", phase.String, status, reason)
	}
	if _, _, _, _, present := firstTurn(t, scene.store, scene.seed.run); present {
		t.Fatal("incompatible Node received a prompt")
	}
}
