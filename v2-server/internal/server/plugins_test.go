package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func seedTestPluginCatalog(t *testing.T, s *store.Store) {
	t.Helper()
	if e := s.ReplacePluginCatalog(context.Background(), []pluginmarket.Entry{
		{Namespace: "official", Identifier: "mattpocock.skills", Title: "Matt Pocock Skills", URL: "https://example.org/skill.orax", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "skill", Version: "1.3.1", Description: "Engineering skills", Readme: "# Matt Pocock Skills\n\nReal skills.", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.tavily-search", Title: "Tavily", Kind: "mcp", Version: "0.1.0", Description: "Web search", Readme: "# Tavily", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.workflow-examples", Title: "Workflow Examples", Kind: "workflow", Version: "0.1.0", Description: "Examples", Readme: "# Workflow Examples", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.opencode", Title: "OpenCode", Kind: "agent", Version: "0.6.8", Description: "Agent", Readme: "# OpenCode", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.codex", Title: "Codex", Kind: "agent", Version: "0.3.14", Description: "Agent", Readme: "# Codex", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "test-pack", Title: "Test Pack", Kind: "pack", Version: "1.0.0", Description: "Pack", PackMembers: []string{"official/mattpocock.skills"}, MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
	}, time.Now()); e != nil {
		t.Fatal(e)
	}
}

func TestPluginMarketHTTP(t *testing.T) {
	f := newSquadServer(t)
	c := newContractClient(t, f.srv.URL)
	c.login()
	tenant := c.tenant()
	status, body := c.do("GET", "/api/v1/tenants/"+tenant+"/spaces", nil)
	if status != 200 {
		t.Fatalf("spaces = %d %v", status, body)
	}
	spaces, _ := body["items"].([]any)
	if len(spaces) != 1 {
		t.Fatalf("spaces = %v", body)
	}
	space := spaces[0].(map[string]any)["id"].(string)
	base := "/api/v1/tenants/" + tenant + "/spaces/" + space + "/plugins"

	status, body = c.do("GET", base+"/catalog", nil)
	if status != 200 {
		t.Fatalf("catalog = %d %v", status, body)
	}
	catalog, _ := body["items"].([]any)
	if len(catalog) != 6 || body["syncedAt"] == nil {
		t.Fatalf("catalog = %v", body)
	}
	agentIDs := map[string]bool{}
	for _, raw := range catalog {
		entry := raw.(map[string]any)
		if entry["kind"] == "agent" {
			agentIDs[entry["id"].(string)] = true
		}
	}
	if !agentIDs["official/ora-space.opencode"] || !agentIDs["official/ora-space.codex"] {
		t.Fatalf("catalog Agents = %v", agentIDs)
	}

	status, body = c.do("GET", base, nil)
	if status != 200 {
		t.Fatalf("plugins = %d %v", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("initial plugins = %v", body)
	}
	var preinstalledVersion float64
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["sourceNamespace"] == "official" && item["identifier"] == "ora-space.opencode" {
			preinstalledVersion = item["version"].(float64)
		}
	}

	input := map[string]any{
		"identifier":    "official/mattpocock.skills",
		"pluginVersion": "1.3.1",
		"version":       0,
	}
	status, body = c.doHeaders("POST", base, input, map[string]string{
		"Idempotency-Key": "install-skill",
	})
	if status != 200 {
		t.Fatalf("install = %d %v", status, body)
	}
	resource := body["resource"].(map[string]any)
	if resource["kind"] != "skill" || resource["observedState"] != "pending" ||
		resource["observedVersion"] != nil {
		t.Fatalf("installed = %v", resource)
	}
	version := resource["version"].(float64)

	status, replay := c.doHeaders("POST", base, input, map[string]string{
		"Idempotency-Key": "install-skill",
	})
	if status != 200 {
		t.Fatalf("replay = %d %v", status, replay)
	}
	if replayed := replay["resource"].(map[string]any); replayed["version"] != version {
		t.Fatalf("replayed = %v, want version %v", replayed, version)
	}
	status, body = c.doHeaders("POST", base, map[string]any{
		"identifier": "official/ora-space.tavily-search",
		"version":    0,
	}, map[string]string{"Idempotency-Key": "install-skill"})
	if status != 409 || body["code"] != "idempotency_conflict" {
		t.Fatalf("key reuse = %d %v", status, body)
	}

	status, body = c.doHeaders("POST", base, map[string]any{
		"identifier": "not-a-plugin",
		"version":    0,
	}, map[string]string{"Idempotency-Key": "invalid-plugin-id"})
	if status != 400 || body["code"] != "invalid_plugin_id" {
		t.Fatalf("invalid id = %d %v", status, body)
	}
	status, body = c.doHeaders("POST", base, map[string]any{
		"identifier":    "official/mattpocock.skills",
		"pluginVersion": "9.9.9",
		"version":       0,
	}, map[string]string{"Idempotency-Key": "missing-plugin-version"})
	if status != 400 || body["code"] != "plugin_version_unavailable" {
		t.Fatalf("unavailable version = %d %v", status, body)
	}
	status, body = c.doHeaders("POST", base, map[string]any{
		"identifier": "official/test-pack",
		"version":    0,
	}, map[string]string{"Idempotency-Key": "install-pack"})
	if status != 400 || body["code"] != "plugin_kind_not_installable" {
		t.Fatalf("pack = %d %v", status, body)
	}
	status, body = c.doHeaders("POST", base, map[string]any{
		"identifier":    "official/ora-space.codex",
		"pluginVersion": "0.3.14",
		"version":       0,
	}, map[string]string{"Idempotency-Key": "install-codex"})
	if status != 200 {
		t.Fatalf("install codex = %d %v", status, body)
	}
	codex := body["resource"].(map[string]any)
	if codex["kind"] != "agent" || codex["desiredState"] != "installed" ||
		codex["observedState"] != "installed" || codex["observedVersion"] != "0.3.14" {
		t.Fatalf("installed codex = %v", codex)
	}
	codexVersion := codex["version"].(float64)
	status, body = c.doHeaders("POST", base, map[string]any{
		"identifier": "official/ora-space.opencode",
		"version":    0,
	}, map[string]string{"Idempotency-Key": "install-runtime-agent"})
	if status != 409 || body["code"] != "plugin_runtime_managed" {
		t.Fatalf("preinstalled install = %d %v", status, body)
	}
	status, body = c.doHeaders("DELETE", base, map[string]any{
		"identifier": "official/ora-space.opencode",
		"version":    int64(preinstalledVersion),
	}, map[string]string{"Idempotency-Key": "remove-runtime-agent"})
	if status != 409 || body["code"] != "plugin_runtime_managed" {
		t.Fatalf("preinstalled removal = %d %v", status, body)
	}
	status, body = c.doHeaders("DELETE", base, map[string]any{
		"identifier": "official/ora-space.codex",
		"version":    int64(codexVersion),
	}, map[string]string{"Idempotency-Key": "remove-codex"})
	if status != 200 {
		t.Fatalf("remove codex = %d %v", status, body)
	}
	removedCodex := body["resource"].(map[string]any)
	if removedCodex["desiredState"] != "removed" || removedCodex["observedState"] != "removed" ||
		removedCodex["observedVersion"] != nil {
		t.Fatalf("removed codex = %v", removedCodex)
	}

	status, body = c.doHeaders("DELETE", base, map[string]any{
		"identifier": "official/mattpocock.skills",
	}, map[string]string{"Idempotency-Key": "remove-without-version"})
	if status != 428 || body["code"] != "version_required" {
		t.Fatalf("missing version = %d %v", status, body)
	}
	status, body = c.doHeaders("DELETE", base, map[string]any{
		"identifier": "official/mattpocock.skills",
		"version":    int64(version),
	}, map[string]string{"Idempotency-Key": "remove-skill"})
	if status != 200 {
		t.Fatalf("remove = %d %v", status, body)
	}
	removed := body["resource"].(map[string]any)
	if removed["desiredState"] != "removed" || removed["observedState"] != "removed" ||
		removed["observedVersion"] != nil {
		t.Fatalf("removed = %v", removed)
	}

	status, body = c.do("GET", base, nil)
	if status != 200 {
		t.Fatalf("plugins after remove = %d %v", status, body)
	}
	if items, _ := body["items"].([]any); len(items) != 4 {
		t.Fatalf("plugins after remove = %v", body)
	}
}
