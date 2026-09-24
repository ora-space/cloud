package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// TestPluginRouteRegistration pins the marketplace routes into the allowlist
// with their exact field whitelists: the router rejects anything not listed
// before the core ever sees it.
func TestPluginRouteRegistration(t *testing.T) {
	found := map[string][]string{}
	for _, route := range Routes() {
		if !strings.Contains(route.Path, "/plugins") {
			continue
		}
		found[route.Method+" "+route.Path] = route.Fields
	}
	want := map[string][]string{
		"GET /api/v1/tenants/:tid/spaces/:spaceId/plugins/catalog": nil,
		"GET /api/v1/tenants/:tid/spaces/:spaceId/plugins":         nil,
		"POST /api/v1/tenants/:tid/spaces/:spaceId/plugins":        {"identifier", "pluginVersion"},
		"DELETE /api/v1/tenants/:tid/spaces/:spaceId/plugins":      {"identifier", "version"},
	}
	if len(found) != len(want) {
		t.Fatalf("plugin routes = %v", found)
	}
	for route, fields := range want {
		got, ok := found[route]
		if !ok {
			t.Fatalf("route %s missing", route)
		}
		if strings.Join(got, ",") != strings.Join(fields, ",") {
			t.Fatalf("route %s fields = %v, want %v", route, got, fields)
		}
	}
}

// TestPluginFieldValidation pins the boundary types the router accepts for the
// new plugin fields: identifier/pluginVersion are strings, DELETE version is
// an integer, and the plugin effect evidence keys are typed strictly.
func TestPluginFieldValidation(t *testing.T) {
	if !validField("identifier", "official/hello-world") {
		t.Fatal("identifier must accept a canonical plugin id string")
	}
	if validField("identifier", json.Number("1")) {
		t.Fatal("identifier must reject non-string values")
	}
	if !validField("pluginVersion", "1.2.3") {
		t.Fatal("pluginVersion must accept a semver string")
	}
	if validField("pluginVersion", json.Number("1")) {
		t.Fatal("pluginVersion must reject non-string values")
	}
	if !validField("version", json.Number("3")) {
		t.Fatal("version must accept an integer")
	}
	if validField("version", "3") {
		t.Fatal("version must reject strings")
	}
	if !validField("result", map[string]any{"installed": true, "version": "1.2.3", "error": "external_failure", "diagnostic": "sha256 mismatch"}) {
		t.Fatal("plugin effect evidence must be accepted")
	}
	if validField("result", map[string]any{"installed": "yes"}) {
		t.Fatal("installed evidence must be boolean")
	}
	if validField("result", map[string]any{"unknownEvidence": true}) {
		t.Fatal("unknown evidence keys must be rejected")
	}
}

func TestUnexpectedPanicIncludesValueAndStackInLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logCore, recorded := observer.New(zap.ErrorLevel)
	engine := New(nil, nil, zap.New(logCore))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	request.Header.Set("Authorization", "Bearer triggers-nil-authenticator")

	engine.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	entries := recorded.FilterMessage("request panic").All()
	if len(entries) != 1 {
		t.Fatalf("expected one panic log, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["panic"] == nil || fields["stack"] == "" {
		t.Fatalf("panic log omitted value or stack: %v", fields)
	}
}
