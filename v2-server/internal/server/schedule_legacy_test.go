package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestLegacyScheduleReferencesRemainReadableButCannotBeWritten(t *testing.T) {
	// A task reference has no writable target at all; a workflow reference without the workflow
	// target's fields is not one.
	refusal := map[string]int{"task": 503, "workflow": 400}
	for _, kind := range []string{"workflow", "task"} {
		t.Run(kind, func(t *testing.T) {
			db := openStore(t)
			ctx := context.Background()
			token := storetest.SignIn(t, db, "legacy", "Owner", store.RoleOwner)
			user, err := db.SessionUser(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			tenants, err := db.Tenants(ctx, user.ID)
			if err != nil {
				t.Fatal(err)
			}
			tenant := tenants[0].ID
			// Existing payloads are opaque: no conversion of provider-specific input types.
			target, _ := json.Marshal(map[string]any{"kind": kind, "providerReference": "legacy-id", "inputs": map[string]any{"count": 3, "enabled": false, "nested": []string{"one"}}})
			input := store.ScheduleInput{Name: "legacy", ProjectID: "", Target: target, Rule: json.RawMessage(`{"kind":"hours","time":"09:00","days":[],"hours":1,"timezone":"UTC"}`)}
			saved, err := db.CreateScheduleConfig(ctx, tenant, user.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			handler := server.New(db, server.Options{})
			call := func(method, path string, body any) *httptest.ResponseRecorder {
				t.Helper()
				raw, _ := json.Marshal(body)
				req := httptest.NewRequest(method, path, bytes.NewReader(raw))
				req.AddCookie(&http.Cookie{Name: server.SessionCookie, Value: token})
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, req)
				return out
			}
			path := "/api/v2/tenants/" + tenant + "/schedule-configs"
			if out := call("POST", path, input); out.Code != refusal[kind] {
				t.Fatalf("new reference write: %d %s", out.Code, out.Body)
			}
			input.Version = 1
			input.Name = "must not overwrite"
			if out := call("PUT", path+"/"+saved.ID, input); out.Code != refusal[kind] {
				t.Fatalf("reference update: %d %s", out.Code, out.Body)
			}
			out := call("GET", path, nil)
			var list struct {
				Items []store.ScheduleConfig `json:"items"`
			}
			if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &list) != nil || len(list.Items) != 1 {
				t.Fatalf("read compatibility: %d %s", out.Code, out.Body)
			}
			var before, after any
			_ = json.Unmarshal(saved.Target, &before)
			_ = json.Unmarshal(list.Items[0].Target, &after)
			if !reflect.DeepEqual(before, after) || list.Items[0].Name != "legacy" || list.Items[0].Version != 1 {
				t.Fatal("opaque reference was rewritten")
			}
			out = call("PATCH", path+"/"+saved.ID+"/enabled", map[string]any{"version": 1, "enabled": false})
			if out.Code != 200 {
				t.Fatalf("pause legacy schedule: %d", out.Code)
			}
			items, err := db.ScheduleConfigs(ctx, tenant, user.ID)
			if err != nil || len(items) != 1 {
				t.Fatal(err)
			}
			_ = json.Unmarshal(items[0].Target, &after)
			if items[0].Enabled || !reflect.DeepEqual(before, after) {
				t.Fatal("pause changed the provider payload")
			}
		})
	}
}
