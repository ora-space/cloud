package core

// DB tests for the personal executor-slot mappings API (model_slot_mappings.go, specs decision
// 20261015-executor-slot-mappings D1): owner-only CRUD over `/api/v1/me/executor-slot-mappings`,
// driven through Store.Public — the real route, replay and CAS machinery. `default` is never a
// mapping (it IS the personal default model), and a mapping survives a broken connection with its
// unavailability computed in the view instead of the row vanishing.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// pathSlot extracts the :slot segment of a mappings path for the direct s.Public calls.
func pathSlot(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return ""
}

// slotRequest drives one public API call against the mappings surface. SlotName is normally
// filled by the router from the path; calling s.Public directly must pass it explicitly.
func slotRequest(t *testing.T, s *Store, c *Claims, method, path, slot, key string, body Object) Object {
	t.Helper()
	o, _, err := s.Public(context.Background(), &PublicRequest{Method: method, Path: path, SlotName: slot, Key: key, Body: body, Identity: c})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return o
}

func TestExecutorSlotMappingsLifecycle(t *testing.T) {
	s := commandStore(t)
	owner, _ := modelUser(t, s, "slot-owner")
	conn, _ := configuredModel(t, s, owner)

	// Create: version starts at 1, the view resolves the model label and reports availability.
	body := Object{"agentCli": opencodeAgentCLI, "connectionId": conn.S("id"), "modelId": "vendor/model-v1", "version": int64(0)}
	view := slotRequest(t, s, owner, "PUT", "/api/v1/me/executor-slot-mappings/strong", "strong", uuid.NewString(), body).O("resource")
	if view.S("slotName") != "strong" || view.N("version") != 1 || view.S("modelName") != "Model V1" || !view.B("available") || view.S("connectionName") != "Personal Provider" {
		t.Fatalf("created view = %v", view)
	}

	// Update is a CAS upsert: the next write needs the current version, and bumps it.
	body["version"] = int64(1)
	body["modelId"] = "vendor/model-v1"
	view = slotRequest(t, s, owner, "PUT", "/api/v1/me/executor-slot-mappings/strong", "strong", uuid.NewString(), body).O("resource")
	if view.N("version") != 2 {
		t.Fatalf("upsert version = %d; want 2", view.N("version"))
	}
	body["version"] = int64(1)
	if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "PUT", Path: "/api/v1/me/executor-slot-mappings/strong", SlotName: "strong", Body: body, Identity: owner}); err == nil || ErrorCode(err).Code != "version_conflict" {
		t.Fatalf("stale version: err=%v; want version_conflict", err)
	}
	body["version"] = int64(2)

	// List shows the single mapping, owner-scoped.
	list := slotRequest(t, s, owner, "GET", "/api/v1/me/executor-slot-mappings", "", "", nil)
	items, ok := list["items"].([]Object)
	if !ok || len(items) != 1 || items[0].S("slotName") != "strong" {
		t.Fatalf("list = %v", list)
	}
	if other, _ := modelUser(t, s, "slot-other"); len(slotRequest(t, s, other, "GET", "/api/v1/me/executor-slot-mappings", "", "", nil)["items"].([]Object)) != 0 {
		t.Fatal("cross-user list leak")
	}

	// Writes are gated on the slot name, the agent CLI and the referenced connection's state.
	for _, tc := range []struct {
		path  string
		body  Object
		fault string
		note  string
	}{
		{"/api/v1/me/executor-slot-mappings/default", Object{"agentCli": opencodeAgentCLI, "connectionId": conn.S("id"), "modelId": "vendor/model-v1", "version": int64(2)}, "invalid_slot_name", "default is the personal default model, never a mapping"},
		{"/api/v1/me/executor-slot-mappings/Bad", Object{"agentCli": opencodeAgentCLI, "connectionId": conn.S("id"), "modelId": "vendor/model-v1", "version": int64(2)}, "invalid_slot_name", "name pattern"},
		{"/api/v1/me/executor-slot-mappings/strong", Object{"agentCli": "other/cli", "connectionId": conn.S("id"), "modelId": "vendor/model-v1", "version": int64(2)}, "unsupported_agent_cli", "plugin identity"},
		{"/api/v1/me/executor-slot-mappings/strong", Object{"agentCli": opencodeAgentCLI, "connectionId": uuid.NewString(), "modelId": "vendor/model-v1", "version": int64(2)}, "not_found", "foreign connection"},
		{"/api/v1/me/executor-slot-mappings/strong", Object{"agentCli": opencodeAgentCLI, "connectionId": conn.S("id"), "modelId": "vendor/nope", "version": int64(2)}, "model_not_found", "unknown model"},
	} {
		if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "PUT", Path: tc.path, SlotName: pathSlot(tc.path), Body: tc.body, Identity: owner}); err == nil || ErrorCode(err).Code != tc.fault {
			t.Fatalf("%s: err=%v; want %s", tc.note, err, tc.fault)
		}
	}

	// A disabled connection keeps the mapping but the view reports the break, and further writes
	// against the disabled connection are refused (the gate would resolve nothing).
	modelBodyPayload := modelBody("openai-completions")
	modelBodyPayload["version"] = conn.N("version")
	modelBodyPayload["enabled"] = false
	modelRequest(t, s, owner, "PUT", "/api/v1/me/model-connections", conn.S("id"), "", modelBodyPayload)
	view = slotRequest(t, s, owner, "GET", "/api/v1/me/executor-slot-mappings", "", "", nil)["items"].([]Object)[0]
	if view.B("available") || view.S("unavailableReason") != "model_connection_unavailable" {
		t.Fatalf("disabled connection view = %v", view)
	}
	if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "PUT", Path: "/api/v1/me/executor-slot-mappings/strong", SlotName: "strong", Body: Object{"agentCli": opencodeAgentCLI, "connectionId": conn.S("id"), "modelId": "vendor/model-v1", "version": int64(2)}, Identity: owner}); err == nil || ErrorCode(err).Code != "model_connection_disabled" {
		t.Fatalf("write against disabled connection: err=%v; want model_connection_disabled", err)
	}
}

func TestExecutorSlotMappingsDeleteCASAndReplay(t *testing.T) {
	s := commandStore(t)
	owner, _ := modelUser(t, s, "slot-del")
	conn, _ := configuredModel(t, s, owner)
	path := "/api/v1/me/executor-slot-mappings/fast"
	body := Object{"agentCli": opencodeAgentCLI, "connectionId": conn.S("id"), "modelId": "vendor/model-v1", "version": int64(0)}
	slotRequest(t, s, owner, "PUT", path, "fast", uuid.NewString(), body)

	// Missing version is a precondition failure, not a delete. A keyless delete is refused before
	// the CAS is even read — replay safety comes first.
	if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "DELETE", Path: path, SlotName: "fast", Key: uuid.NewString(), Identity: owner}); err == nil || ErrorCode(err).Code != "version_required" {
		t.Fatalf("no version: err=%v; want version_required", err)
	}
	if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "DELETE", Path: path, SlotName: "fast", Key: uuid.NewString(), Body: Object{"version": int64(2)}, Identity: owner}); err == nil || ErrorCode(err).Code != "version_conflict" {
		t.Fatalf("wrong version: err=%v; want version_conflict", err)
	}
	if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "DELETE", Path: path, SlotName: "fast", Identity: owner}); err == nil || ErrorCode(err).Code != "idempotency_key_required" {
		t.Fatalf("no key: err=%v; want idempotency_key_required", err)
	}

	// The delete is CAS'd and replayed: the same key returns the captured row again; a fresh key
	// over a deleted row is 404.
	key := uuid.NewString()
	deleted := slotRequest(t, s, owner, "DELETE", path, "fast", key, Object{"version": int64(1)}).O("resource")
	if deleted.S("slotName") != "fast" {
		t.Fatalf("deleted view = %v", deleted)
	}
	replayed := slotRequest(t, s, owner, "DELETE", path, "fast", key, Object{"version": int64(1)}).O("resource")
	if replayed.S("slotName") != "fast" || replayed.N("version") != deleted.N("version") {
		t.Fatalf("replay = %v; want the captured delete", replayed)
	}
	if _, _, err := s.Public(context.Background(), &PublicRequest{Method: "DELETE", Path: path, SlotName: "fast", Key: uuid.NewString(), Body: Object{"version": int64(1)}, Identity: owner}); err == nil || ErrorCode(err).Code != "not_found" {
		t.Fatalf("fresh key over deleted row: err=%v; want not_found", err)
	}
	if len(slotRequest(t, s, owner, "GET", "/api/v1/me/executor-slot-mappings", "", "", nil)["items"].([]Object)) != 0 {
		t.Fatal("mapping survived its delete")
	}
}
