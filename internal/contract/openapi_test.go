package contract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestPublishedOpenAPIIsValidAndCurrent(t *testing.T) {
	published, e := os.ReadFile("../../api/openapi.json")
	if e != nil {
		t.Fatal(e)
	}
	generated, e := json.MarshalIndent(Document(), "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if string(published) != string(append(generated, '\n')) {
		t.Fatal("api/openapi.json is stale; go run ./cmd/openapi")
	}
	loader := openapi3.NewLoader()
	doc, e := loader.LoadFromData(published)
	if e != nil {
		t.Fatal(e)
	}
	if e = doc.Validate(context.Background()); e != nil {
		t.Fatal(e)
	}
}

// kin-openapi tolerates "required": null and "required": [], but the OpenAPI 3.0 schema and the
// frontend generator do not, so the raw JSON tree is checked directly.
func TestRequiredIsOmittedWhenEmpty(t *testing.T) {
	b, e := json.Marshal(Document())
	if e != nil {
		t.Fatal(e)
	}
	var tree any
	if e = json.Unmarshal(b, &tree); e != nil {
		t.Fatal(e)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch node := v.(type) {
		case map[string]any:
			if raw, ok := node["required"]; ok && node["type"] == "object" {
				items, isList := raw.([]any)
				if !isList || len(items) == 0 {
					t.Errorf("%s: required must be a non-empty array, got %v", path, raw)
				}
			}
			for k, child := range node {
				walk(path+"/"+k, child)
			}
		case []any:
			for i, child := range node {
				walk(fmt.Sprintf("%s/%d", path, i), child)
			}
		}
	}
	walk("", tree)
}

// TestPluginRoutesInDocument pins the four marketplace routes into the
// generated contract: path shape, request body fields, and response schemas.
func TestPluginRoutesInDocument(t *testing.T) {
	doc := Document()
	paths := doc["paths"].(map[string]any)
	for _, route := range []struct{ method, path string }{
		{"get", "/api/v1/tenants/{tid}/spaces/{spaceId}/plugins/catalog"},
		{"get", "/api/v1/tenants/{tid}/spaces/{spaceId}/plugins"},
		{"post", "/api/v1/tenants/{tid}/spaces/{spaceId}/plugins"},
		{"delete", "/api/v1/tenants/{tid}/spaces/{spaceId}/plugins"},
	} {
		entry, ok := paths[route.path].(map[string]any)[route.method]
		if !ok {
			t.Fatalf("%s %s missing from Document()", route.method, route.path)
		}
		_ = entry
	}
	post := paths["/api/v1/tenants/{tid}/spaces/{spaceId}/plugins"].(map[string]any)["post"].(map[string]any)
	body := post["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	required := body["required"].([]string)
	if len(required) != 1 || required[0] != "identifier" {
		t.Fatalf("POST plugins required fields = %v", required)
	}
	if _, ok := body["properties"].(map[string]any)["pluginVersion"]; !ok {
		t.Fatal("POST plugins must accept an optional pluginVersion")
	}
	deleteOp := paths["/api/v1/tenants/{tid}/spaces/{spaceId}/plugins"].(map[string]any)["delete"].(map[string]any)
	deleteBody := deleteOp["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	deleteRequired := deleteBody["required"].([]string)
	if len(deleteRequired) != 2 {
		t.Fatalf("DELETE plugins required fields = %v", deleteRequired)
	}
}
