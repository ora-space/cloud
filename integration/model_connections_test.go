package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func TestPersonalModelHTTPContractAndIsolation(t *testing.T) {
	f := setup(t)
	body := core.Object{"name": "Personal", "protocol": "openai-completions", "baseUrl": "https://models.example.invalid/v1", "authMode": "bearer", "models": []any{map[string]any{"id": "vendor/model-v1", "name": "Model", "contextWindow": 128000, "maxTokens": 8192}}}
	created := f.call("POST", "/api/v1/me/model-connections", body, "model-create", 201)
	connection := created.O("resource")
	id := connection.S("id")
	if connection.B("credentialConfigured") {
		t.Fatal("new connection reports a credential")
	}
	replayed := f.call("POST", "/api/v1/me/model-connections", body, "model-create", 201)
	if replayed.O("resource").S("id") != id {
		t.Fatal("create replay changed identity")
	}
	f.call("PUT", "/api/v1/me/model-default", core.Object{"connectionId": id, "modelId": "vendor/model-v1", "version": 0}, "", 200)
	f.call("PUT", "/api/v1/me/model-default", core.Object{"connectionId": id, "modelId": "vendor/model-v1"}, "", 428)
	body["version"] = 1
	body["name"] = "Renamed"
	f.call("PUT", "/api/v1/me/model-connections/"+id, body, "", 200)
	f.call("PUT", "/api/v1/me/model-connections/"+id, body, "", 409)
	f.user.Subject = "model-other-user"
	f.call("GET", "/api/v1/me/model-connections/"+id, nil, "", 404)
	other := f.call("GET", "/api/v1/me/model-default", nil, "", 200)
	if other.S("connectionId") != "" || other.N("version") != 0 {
		t.Fatal("default leaked to another user")
	}
	f.user.Subject = "alice"
	f.call("DELETE", "/api/v1/me/model-connections/"+id, core.Object{"version": 2}, "model-delete", 200)
	f.call("DELETE", "/api/v1/me/model-connections/"+id, core.Object{"version": 2}, "model-delete", 200)
	f.call("GET", "/api/v1/me/model-connections/"+id, nil, "", 404)
}

func TestModelConnectionsUpgradePreservesHistoricalRuns(t *testing.T) {
	pool, _ := testSchema(t, "test_model_upgrade_")
	entries, err := os.ReadDir(filepath.Join("..", "internal", "core", "migrations"))
	must(t, err)
	var versions []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".sql" && entry.Name() < "0033" {
			versions = append(versions, entry.Name())
		}
	}
	applyMigrationsUpTo(t, pool, versions)
	user, tenant, issue, run := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := pool.BeginTx(t.Context(), nil)
	must(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	for _, item := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO users(id,display_name,status) VALUES($1,'Actor','active')", []any{user}},
		{"INSERT INTO tenants(id,name,status) VALUES($1,'Upgrade','active')", []any{tenant}},
		{"INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", []any{tenant, user}},
		{"INSERT INTO issues(id,tenant_id,creator_user_id,title,number) VALUES($1,$2,$3,'Upgrade',1)", []any{issue, tenant, user}},
		{"INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,input) VALUES($1,$2,$3,'agent',$4,'{\"task\":\"historical echo\"}')", []any{run, tenant, issue, uuid.NewString()}},
	} {
		_, err = tx.Exec(item.query, item.args...)
		must(t, err)
	}
	must(t, tx.Commit())
	s := newStoreOnSchema(t, pool)
	must(t, s.Migrate(t.Context()))
	must(t, s.Migrate(t.Context()))
	must(t, s.CheckSchema(t.Context()))
	var bindingCount, grantCount int
	must(t, pool.QueryRow("SELECT count(*) FROM run_model_bindings WHERE run_id=$1", run).Scan(&bindingCount))
	must(t, pool.QueryRow("SELECT count(*) FROM model_access_grants").Scan(&grantCount))
	if bindingCount != 0 || grantCount != 0 {
		t.Fatal("migration invented a historical model binding")
	}
	var input string
	must(t, pool.QueryRow("SELECT input->>'task' FROM issue_runs WHERE id=$1", run).Scan(&input))
	if input != "historical echo" {
		t.Fatal("migration changed historical run input")
	}
	for _, table := range []string{"personal_model_connections", "personal_model_credentials", "personal_model_defaults", "personal_model_idempotency", "run_model_bindings", "model_access_grants"} {
		if !tableExists(t, pool, table) {
			t.Fatalf("missing table %s", table)
		}
	}
}
