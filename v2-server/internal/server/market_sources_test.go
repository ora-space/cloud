package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Only these methods are exercised by the sources read route; unimplemented calls fail visibly.
type marketCapabilityStore struct {
	Store
	enabled bool
}

func (s marketCapabilityStore) OfficialMarketEnabled() bool { return s.enabled }
func (s marketCapabilityStore) SessionUser(context.Context, string) (store.User, error) {
	return store.User{ID: "user", Status: "active"}, nil
}
func (s marketCapabilityStore) TenantAccess(_ context.Context, tenant, user string) (store.Access, error) {
	return store.Access{TenantID: tenant, UserID: user, Role: store.RoleMember}, nil
}
func (s marketCapabilityStore) MarketSources(context.Context, string, string) ([]store.MarketSource, error) {
	return []store.MarketSource{}, nil
}

func TestMarketSourcesReportsOfficialCapabilityWithoutCatalog(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "market-sources.openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	schema := doc.Paths.Find("/api/v1/tenants/{tid}/spaces/{sid}/plugins/sources").Get.Responses.Status(200).Value.Content["application/json"].Schema.Value
	for _, enabled := range []bool{true, false} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			h := New(marketCapabilityStore{enabled: enabled}, Options{})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/00000000-0000-4000-8000-0000000000aa/spaces/00000000-0000-4000-8000-0000000000bb/plugins/sources", nil)
			req.AddCookie(&http.Cookie{Name: SessionCookie, Value: "session"})
			response := httptest.NewRecorder()
			h.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if err := schema.VisitJSON(body); err != nil {
				t.Fatalf("candidate contract: %v", err)
			}
			if body["officialEnabled"] != enabled || len(body["items"].([]any)) != 0 {
				t.Fatalf("capability response=%v", body)
			}
		})
	}
}
