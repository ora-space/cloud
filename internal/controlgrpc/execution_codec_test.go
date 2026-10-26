package controlgrpc

import (
	"encoding/json"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// A frozen install plan crosses the controller contract and comes back as the dispatch input that
// the equality gate compares against it. An undeclared plugin must produce no association
// message, and the round trip must reproduce the frozen plan byte for byte.
func TestInstallPlanAgentConfigurationRoundTrip(t *testing.T) {
	const frozen = `{"kind":"install_plugins","plugins":[` +
		`{"agentConfiguration":{"declarationSha256":"ab","schemaVersion":2},"pluginId":"official/declared","version":"1.0.0"},` +
		`{"pluginId":"official/undeclared","version":"1.0.0"}]}`
	var plan core.Object
	if e := json.Unmarshal([]byte(frozen), &plan); e != nil {
		t.Fatal(e)
	}
	message := input(plan).GetInstallPlugins().GetPlugins()
	if len(message) != 2 {
		t.Fatalf("plan must map every plugin: %v", message)
	}
	if agent := message[0].GetAgentConfiguration(); agent.GetSchemaVersion() != 2 || agent.GetDeclarationSha256() != "ab" {
		t.Fatalf("declared association must cross verbatim: %v", agent)
	}
	if agent := message[1].GetAgentConfiguration(); agent != nil {
		t.Fatalf("undeclared plugin must carry no association message, got %v", agent)
	}
	back, e := inputObject(input(plan))
	if e != nil {
		t.Fatal(e)
	}
	text, e := json.Marshal(back)
	if e != nil {
		t.Fatal(e)
	}
	if string(text) != frozen {
		t.Fatalf("round trip must reproduce the frozen plan:\n got %s\nwant %s", text, frozen)
	}
}
