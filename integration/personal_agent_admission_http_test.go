package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// An available global default must never satisfy Claude's missing personal plugin selection.
// Both an absent row and a cleared tombstone must fail at the real authenticated HTTP entry
// points, leave no user-visible intent/run, and become admissible only after an explicit save.
func TestPersonalAgentMissingModelSelectionHTTPAdmissionAndRepair(t *testing.T) {
	for _, cleared := range []bool{false, true} {
		name := "never configured"
		if cleared {
			name = "cleared configuration"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			defaultConnection := seedPersonalModel(t, f)
			space, agentID := f.defaultSpaceID(), uuid.NewString()
			f.seedSpaceAgent(t, space, f.tid, agentID, "official/ora-space.claude", "Claude", "active", "1.0.0")
			declaration := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`
			compiled, err := pluginmarket.CompileAgentConfiguration([]byte(declaration))
			must(t, err)
			release := core.Object{
				"id": "official/ora-space.claude", "kind": "agent", "version": "1.0.0",
				"url": "https://packages.example.invalid/claude.orax", "sha256": strings.Repeat("c", 64),
				"agentConfiguration": core.Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": declaration},
			}
			_, err = f.store.Pool.Exec(`UPDATE space_plugins SET selected_release=$2 WHERE space_id=$1 AND identifier='ora-space.claude'`, space, mustJSON(t, release))
			must(t, err)
			f.store.PersonalAgentRuntime = &core.PersonalAgentRuntime{Target: "x86_64-unknown-linux-gnu", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}
			connection := f.call("POST", "/api/v1/me/model-connections", core.Object{
				"name": "Claude private", "protocol": "anthropic-messages", "baseUrl": "https://models.example.invalid/anthropic",
				"authMode": "bearer", "models": fixtureModels("claude-model"),
			}, uuid.NewString(), 201).O("resource")
			// The credential boundary is a setup double: core receives opaque ciphertext, never a
			// plaintext API key. Credential entry through Gateway belongs to the browser scenario.
			_, err = f.store.PutModelCredential(context.Background(), &f.user, connection.S("id"), connection.N("version"),
				uuid.NewString(), uuid.NewString(), make([]byte, 32), "fixture-key")
			must(t, err)
			configPath := "/api/v1/me/agent-configurations/" + space + "/official/ora-space.claude"
			body := core.Object{
				"revision": 0, "selectionVersion": 1, "declarationFingerprint": compiled.Fingerprint,
				"settings": core.Object{}, "connectionId": connection.S("id"), "modelId": "claude-model",
			}
			if cleared {
				f.call("PUT", configPath, body, uuid.NewString(), 200)
				f.call("DELETE", configPath, core.Object{"revision": 1}, uuid.NewString(), 200)
			}
			configuration := f.call("GET", configPath, nil, "", 200)
			if !configuration.B("cleared") || configuration.S("modelId") != "" {
				t.Fatal("fixture must begin without a personal plugin model selection")
			}
			project := f.create(uuid.NewString()).O("resource").S("id")
			issueID := f.issueForAgent(t, "Configure Claude", project, uuid.NewString())
			base := f.path("/issues/" + issueID)
			page := f.call("GET", base+"/conversation", nil, "", 200)
			readiness, ok := page["agentReadiness"].([]any)
			if !ok || len(readiness) != 1 {
				t.Fatalf("expected one authoritative Agent readiness: %v", page["agentReadiness"])
			}
			want := map[string]any{"agentId": agentID, "requiresPersonalModel": true, "errorCode": "agent_configuration_required"}
			if !reflect.DeepEqual(readiness[0], want) {
				t.Fatalf("unconfigured readiness = %v, want %v", readiness[0], want)
			}
			manualStatus, manual := f.manualAgentRun(t, issueID, agentID, uuid.NewString(), core.Object{"task": "Read only"})
			commentStatus, comment := f.agentCommentT(t, issueID, agentID, uuid.NewString())
			message := f.call("POST", base+"/conversation/messages", core.Object{"agentId": agentID, "body": "Read only"}, uuid.NewString(), 409)
			if manualStatus != 409 || commentStatus != 409 || manual.S("code") != "agent_configuration_required" || comment.S("code") != "agent_configuration_required" || message.S("code") != "agent_configuration_required" {
				t.Fatalf("admission did not consistently reject the missing selection: manual=%d %v comment=%d %v message=%v", manualStatus, manual, commentStatus, comment, message)
			}
			for _, path := range []string{"/runs", "/comments", "/conversation"} {
				if items := issueItems(f.call("GET", base+path, nil, "", 200)); len(items) != 0 {
					t.Fatalf("refused request left resources at %s: %v", path, items)
				}
			}
			if got := f.call("GET", "/api/v1/me/model-default", nil, "", 200); got.S("connectionId") != defaultConnection.S("id") || got.S("modelId") != fixtureModelID {
				t.Fatal("Claude refusal changed the user's global default")
			}
			body["revision"] = configuration.N("revision")
			saved := f.call("PUT", configPath, body, uuid.NewString(), 200).O("resource")
			page = f.call("GET", base+"/conversation", nil, "", 200)
			ready, ok := page["agentReadiness"].([]any)
			want["errorCode"] = nil
			if !ok || len(ready) != 1 || !reflect.DeepEqual(ready[0], want) {
				t.Fatalf("explicit repair did not restore readiness: %v", page["agentReadiness"])
			}
			f.agentComment(t, issueID, agentID, uuid.NewString())
			run := f.singleRun(t, issueID)
			if run.S("executorId") != agentID || run.O("input").S("agentPluginId") != "official/ora-space.claude" {
				t.Fatal("repaired request did not persist the explicitly selected Claude run")
			}
			frozen := run.O("agentConfiguration")
			if frozen.S("pluginId") != "official/ora-space.claude" || frozen.N("configurationRevision") != saved.N("revision") {
				t.Fatalf("admitted run did not freeze the repaired plugin configuration: %v", frozen)
			}
			if got := f.call("GET", configPath, nil, "", 200); got.S("connectionId") != connection.S("id") || got.S("modelId") != "claude-model" {
				t.Fatal("explicit repair did not retain the plugin's own connection and model")
			}
		})
	}
}
