package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// seedScheduledTaskClaudeModelOverride prepares a declared Claude agent whose declaration and adapter
// accept only anthropic-messages, a creator whose personal default connection is OpenAI, and a task
// whose definition selects the OpenAI default model. It returns the task ID.
func seedScheduledTaskClaudeModelOverride(t *testing.T, store *core.Store, s skeletonSeed) string {
	t.Helper()
	claudeAgentID := seedScheduledTaskAgentWithPlugin(t, store.Pool, s, "official/ora-space.claude")
	declaration := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(declaration))
	must(t, err)
	release := core.Object{
		"id": "official/ora-space.claude", "kind": "agent", "version": "1.0.0",
		"url": "https://packages.example.invalid/claude.orax", "sha256": strings.Repeat("c", 64),
		"agentConfiguration": core.Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": declaration},
	}
	_, err = store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,selected_release)
		VALUES($1,$2,'official','ora-space.claude','installed','1.0.0',$3)`, s.spaceID, s.tenantID, mustJSON(t, release))
	must(t, err)
	store.PersonalAgentRuntime = &core.PersonalAgentRuntime{Target: "x86_64-unknown-linux-gnu", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}

	// A creator-owned anthropic connection plus the Claude configuration pointing at it, so the
	// declared path resolves cleanly before the task override is applied.
	anthropicConn := uuid.NewString()
	cred := uuid.NewString()
	_, err = store.Pool.Exec(`INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id) VALUES($1,$2,repeat('a',64),'claude-test')`, cred, s.userID)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO personal_model_connections(id,user_id,credential_id,name,protocol,base_url,auth_mode,models,enabled)
		VALUES($1,$2,$3,'Claude Provider','anthropic-messages','https://models.example.invalid/anthropic','bearer',$4::jsonb,true)`,
		anthropicConn, s.userID, cred, `[{"id":"claude-model","name":"Claude Model","contextWindow":200000,"maxTokens":8192}]`)
	must(t, err)
	_, err = store.Pool.Exec(`INSERT INTO personal_agent_configurations(user_id,plugin_id,declaration_fingerprint,settings,connection_id,model_id,revision,cleared)
		VALUES($1,'official/ora-space.claude',$2,'{}'::jsonb,$3,'claude-model',1,false)`, s.userID, compiled.Fingerprint, anthropicConn)
	must(t, err)

	taskID := seedEnabledScheduledTask(t, store, s, claudeAgentID, "Protocol override", 65)
	_, err = store.Pool.Exec(`UPDATE scheduled_tasks SET model=$1 WHERE id=$2`, scheduledTaskModelID, taskID)
	must(t, err)
	return taskID
}

// TestScheduledTaskIncompatibleModelOverrideFailsOccurrence pins the review's protocol gate: a Claude
// declaration backed by Anthropic plus a task model selected on the creator's OpenAI default must be
// refused at admission as a model_unavailable occurrence, not frozen into an OpenAI binding the Claude
// runtime cannot execute.
func TestScheduledTaskIncompatibleModelOverrideFailsOccurrence(t *testing.T) {
	store, s, _ := scheduledTaskStore(t)
	taskID := seedScheduledTaskClaudeModelOverride(t, store, s)

	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	var count, runs int
	var outcome, reason string
	must(t, store.Pool.QueryRow(`SELECT count(*), COALESCE(min(outcome),''), COALESCE(min(failure_reason),''), count(run_id) FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&count, &outcome, &reason, &runs))
	if count != 1 || outcome != "failed" || reason != "model_unavailable" || runs != 0 {
		t.Fatalf("incompatible model override must leave one failed model_unavailable occurrence without a run, got count=%d outcome=%q reason=%q runs=%d", count, outcome, reason, runs)
	}
}
