package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSchemaRemovesLegacyDemoSelections(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Close(ctx) })
	var random [6]byte
	_, _ = rand.Read(random[:])
	namespace := "schema_cleanup_" + hex.EncodeToString(random[:])
	if _, e := admin.Exec(ctx, "CREATE SCHEMA "+namespace); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+namespace+" CASCADE") })
	config, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	config.ConnConfig.RuntimeParams["search_path"] = namespace + ",public"
	s, e := OpenConfig(ctx, config)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)

	var user, tenant, space, plugin, squad, task string
	if e := s.pool.QueryRow(ctx, `
		INSERT INTO users(source,subject,display_name) VALUES('test','owner','Owner') RETURNING id::text`).Scan(&user); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `INSERT INTO tenants(name) VALUES('Team') RETURNING id::text`).Scan(&tenant); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `
		INSERT INTO v2_spaces(tenant_id,name,slug,created_by)
		VALUES($1,'Space','demo',$2) RETURNING id::text`, tenant, user).Scan(&space); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `
		INSERT INTO v2_space_agents(tenant_id,space_id,name,source_namespace,identifier,kind)
		VALUES($1,$2,'Legacy','demo','legacy.skill','skill') RETURNING id::text`, tenant, space).Scan(&plugin); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `
		INSERT INTO v2_squads(tenant_id,name,created_by,leader_agent_id)
		VALUES($1,'Squad',$2,$3) RETURNING id::text`, tenant, user, plugin).Scan(&squad); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, `
		INSERT INTO v2_squad_members(squad_id,member_type,space_agent_id)
		VALUES($1,'agent',$2)`, squad, plugin); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `
		INSERT INTO v2_tasks(tenant_id,title,project_id,assignee_type,assignee_id,created_by)
		VALUES($1,'Task','project','agent',$2,$3) RETURNING id::text`, tenant, plugin, user).Scan(&task); e != nil {
		t.Fatal(e)
	}
	if _, e := s.pool.Exec(ctx, `
		INSERT INTO v2_plugin_mutations
			(tenant_id,user_id,method,path,idempotency_key,request_hash,response)
		VALUES($1,$2,'POST','plugins','legacy','\\x00',jsonb_build_object(
			'resource',jsonb_build_object('sourceNamespace','demo')))`, tenant, user); e != nil {
		t.Fatal(e)
	}

	if _, e := s.pool.Exec(ctx, schema); e != nil {
		t.Fatal(e)
	}
	var plugins, members, mutations int
	var assigneeType, leader *string
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM v2_space_agents WHERE source_namespace='demo'`).Scan(&plugins); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM v2_squad_members WHERE space_agent_id=$1`, plugin).Scan(&members); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM v2_plugin_mutations`).Scan(&mutations); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `SELECT assignee_type FROM v2_tasks WHERE id=$1`, task).Scan(&assigneeType); e != nil {
		t.Fatal(e)
	}
	if e := s.pool.QueryRow(ctx, `SELECT leader_agent_id::text FROM v2_squads WHERE id=$1`, squad).Scan(&leader); e != nil {
		t.Fatal(e)
	}
	if plugins != 0 || members != 0 || mutations != 0 || assigneeType != nil || leader != nil {
		t.Fatalf("legacy cleanup: plugins=%d members=%d mutations=%d assignee=%v leader=%v",
			plugins, members, mutations, assigneeType, leader)
	}
}
