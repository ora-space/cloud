package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestExecutorSlotMappingHTTPBindsSlotPath pins the :slot path parameter binding at the HTTP
// boundary. The core DB tests call Store.Public directly and pass SlotName themselves, so they
// cannot catch a router that drops the binding; without it every named-slot PUT/DELETE is
// classified as the collection route and answers 405 method_not_allowed.
func TestExecutorSlotMappingHTTPBindsSlotPath(t *testing.T) {
	f := setup(t)
	created := f.call("POST", "/api/v1/me/model-connections", core.Object{
		"name":     "Slot provider",
		"protocol": "openai-completions",
		"baseUrl":  "https://models.example.invalid/v1",
		"authMode": "bearer",
		"models":   []any{map[string]any{"id": "vendor/model-v1", "name": "Model", "contextWindow": 128000, "maxTokens": 8192}},
	}, "slot-connection", 201)
	connection := created.O("resource")

	mapping := f.call("PUT", "/api/v1/me/executor-slot-mappings/writer", core.Object{
		"agentCli":     "official/ora-space.opencode",
		"connectionId": connection.S("id"),
		"modelId":      "vendor/model-v1",
		"version":      0,
	}, "slot-create", 200).O("resource")
	if mapping.S("slotName") != "writer" || mapping.N("version") != 1 {
		t.Fatalf("created mapping = %v", mapping)
	}

	list := f.call("GET", "/api/v1/me/executor-slot-mappings", nil, "", 200)
	items, ok := list["items"].([]any)
	if !ok || len(items) != 1 || core.Object(items[0].(map[string]any)).S("slotName") != "writer" {
		t.Fatalf("list = %v", list)
	}

	deleted := f.call("DELETE", "/api/v1/me/executor-slot-mappings/writer", core.Object{"version": 1}, "slot-delete", 200).O("resource")
	if deleted.S("slotName") != "writer" {
		t.Fatalf("deleted mapping = %v", deleted)
	}
	if list = f.call("GET", "/api/v1/me/executor-slot-mappings", nil, "", 200); len(list["items"].([]any)) != 0 {
		t.Fatalf("list after delete = %v", list)
	}
}
