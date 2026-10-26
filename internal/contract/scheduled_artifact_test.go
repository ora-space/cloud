package contract

import (
	"slices"
	"testing"
)

// Pin the actual response shapes: the generic /runs fallback must never turn artifact reads into
// IssueRun, which would make regenerated clients incompatible with the public API.
func TestScheduledArtifactRouteResponseSchemas(t *testing.T) {
	paths := Document()["paths"].(map[string]any)
	for _, tc := range []struct{ path, schema, media string }{
		{"/api/v1/tenants/{tid}/issues/{iid}/runs/{rid}/artifacts", "ScheduledArtifactList", "application/json"},
		{"/api/v1/tenants/{tid}/issues/{iid}/runs/{rid}/artifacts/{aidx}", "", "application/octet-stream"},
	} {
		operation := paths[tc.path].(map[string]any)["get"].(map[string]any)
		response := operation["responses"].(map[string]any)["200"].(map[string]any)
		schema := response["content"].(map[string]any)[tc.media].(map[string]any)["schema"].(map[string]any)
		if tc.schema == "" {
			for _, param := range operation["parameters"].([]any) {
				p := param.(map[string]any)
				if p["name"] == "aidx" {
					index := p["schema"].(map[string]any)
					if index["type"] != "integer" || index["minimum"] != 0 || index["maximum"] != 63 {
						t.Fatalf("artifact index schema = %v, want 0..63", index)
					}
				}
			}
			if schema["type"] != "string" || schema["format"] != "binary" {
				t.Fatalf("download schema = %v, want raw bytes", schema)
			}
			for _, name := range []string{"Content-Disposition", "X-Content-Type-Options", "Cache-Control"} {
				if response["headers"].(map[string]any)[name] == nil {
					t.Fatalf("download response missing %s", name)
				}
			}
		} else if schema["$ref"] != "#/components/schemas/"+tc.schema {
			t.Fatalf("%s response = %v, want %s", tc.path, schema, tc.schema)
		}
	}
}

func TestSpaceEventIncludesScheduledArtifactInvalidation(t *testing.T) {
	schemas := Document()["components"].(map[string]any)["schemas"].(map[string]any)
	event := schemas["SpaceEvent"].(map[string]any)["properties"].(map[string]any)
	types := event["type"].(map[string]any)["enum"].([]string)
	if !slices.Contains(types, "scheduled_task.artifacts_updated") {
		t.Fatal("artifact invalidation missing from the published SSE contract")
	}
}

// Pin the D7 invalidation surface: the SpaceEvent carries the scheduled-task notice type and its task
// identity, so a regenerated client can refetch a tenant's task list and one open history.
func TestSpaceEventIncludesScheduledTaskInvalidation(t *testing.T) {
	schemas := Document()["components"].(map[string]any)["schemas"].(map[string]any)
	event := schemas["SpaceEvent"].(map[string]any)["properties"].(map[string]any)
	types := event["type"].(map[string]any)["enum"].([]string)
	if !slices.Contains(types, "space.scheduled_tasks_updated") {
		t.Fatal("scheduled-task invalidation missing from the published SSE contract")
	}
	if event["scheduledTaskId"] == nil {
		t.Fatal("scheduledTaskId missing from the published SpaceEvent contract")
	}
}

// Pin the D1 resource surface: the task resource exposes the creator and the frozen model selection
// so a regenerated client can read both.
func TestScheduledTaskResourceCarriesModelAndCreator(t *testing.T) {
	schemas := Document()["components"].(map[string]any)["schemas"].(map[string]any)
	props := schemas["ScheduledTask"].(map[string]any)["properties"].(map[string]any)
	if props["creatorUserId"] == nil {
		t.Fatal("creatorUserId missing from the ScheduledTask contract")
	}
	if props["model"] == nil {
		t.Fatal("model missing from the ScheduledTask contract")
	}
}
