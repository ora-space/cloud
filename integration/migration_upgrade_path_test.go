package integration

// Migration reconciliation guard for the append-only sequence:
//
//	0001-0007  upstream baseline (byte-identical to upstream/main)
//	0008-0012  Issue capability migrations (previously 0006-0010)
//	0013       Project Space optional compatibility migration
//
// The two tests below pin the two upgrade paths that matter for the future
// zhangshilong123:922GithubAuth -> ora-space:main PR:
//
//   - TestMigrationUpstream0007UpgradePath simulates an EXISTING upstream deployment (migrated
//     through 0007) with real data, then applies the new 0008-0013 on top: no duplicate DDL, no
//     checksum mismatch, projects.space_id becomes nullable, and the bindings 0007 already made are
//     preserved (0013 does not unbind).
//   - TestMigrationFullSequenceFreshDB verifies a clean PostgreSQL from empty through 0013 and that
//     the final schema is the one the application expects.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/core"
)

// applyMigrationsUpTo applies and records the named migrations exactly as the runner would
// (executing each file, recording filename + SHA256 checksum), stopping before the new additions.
// This mirrors how an upstream deployment reached 0007.
func applyMigrationsUpTo(t *testing.T, pool *sql.DB, versions []string) {
	t.Helper()
	_, e := pool.Exec("CREATE TABLE schema_migrations(version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
	must(t, e)
	for _, version := range versions {
		b, e := os.ReadFile(filepath.Join("..", "internal", "core", "migrations", version))
		must(t, e)
		_, e = pool.Exec(string(b))
		must(t, e)
		sum := sha256.Sum256(b)
		_, e = pool.Exec("INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)", version, hex.EncodeToString(sum[:]))
		must(t, e)
	}
}

func newStoreOnSchema(t *testing.T, pool *sql.DB) *core.Store {
	t.Helper()
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, e)
	store, e := core.NewStore(db)
	must(t, e)
	return store
}

func tableExists(t *testing.T, pool *sql.DB, name string) bool {
	t.Helper()
	var n int
	must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=$1`, name).Scan(&n))
	return n == 1
}

func columnNullable(t *testing.T, pool *sql.DB, table, column string) bool {
	t.Helper()
	var isNullable string
	must(t, pool.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`, table, column).Scan(&isNullable))
	return isNullable == "YES"
}

// TestMigrationUpstream0007UpgradePath is the most important upgrade path: a database that ran the
// real upstream 0001-0007 (including 0007's project->default-space binding and NOT NULL space_id)
// must upgrade cleanly to the new sequence without re-running any upstream migration, without
// checksum errors, keeping 0007's data bindings while finally allowing unscoped projects.
func TestMigrationUpstream0007UpgradePath(t *testing.T) {
	pool, _ := testSchema(t, "test_upg_")
	// The current 0001-0007 files are byte-identical to upstream/main (asserted separately by the
	// git-level reconciliation audit); applying them here reproduces an existing upstream deployment.
	applyMigrationsUpTo(t, pool, []string{
		"0001_core.sql", "0002_aggregate_guards.sql", "0003_resource_versions.sql",
		"0004_effect_intent_and_ticket_scope.sql", "0005_gateway_auth.sql",
		"0006_collab_spaces.sql", "0007_project_space_scope.sql",
	})

	// Simulate data 0007 left behind: a tenant with a default space and a project bound to it
	// (space_id NOT NULL at this point, exactly as upstream 0007 forces).
	ids := make([]string, 6)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	seed := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO users(id,display_name,status) VALUES($1,'Upgrade user','active')", []any{ids[0]}},
		{"INSERT INTO tenants(id,name,status) VALUES($1,'Upgrade tenant','active')", []any{ids[1]}},
		{"INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", []any{ids[1], ids[0]}},
		{"INSERT INTO collab_workspaces(id,tenant_id,name,slug,description,created_by) VALUES($1,$2,'Default','default','',$3)", []any{ids[2], ids[1], ids[0]}},
		{"INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,$4,'Bound project','https://example.invalid/upg.git','main','active')", []any{ids[3], ids[1], ids[0], ids[2]}},
		// A live project requires exactly one main workspace (deferred one_main trigger in 0001).
		{"INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state) VALUES($1,$2,$3,$4,'main','running','ready')", []any{ids[5], ids[1], ids[0], ids[3]}},
	}
	tx, e := pool.Begin()
	must(t, e)
	for _, s := range seed {
		_, e = tx.Exec(s.query, s.args...)
		must(t, e)
	}
	must(t, tx.Commit())

	// Now the current binary migrates the rest (0008-0013) on top of the upstream baseline.
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	// Idempotent: a second Migrate and a strict CheckSchema must both pass with no unknown/missing
	// versions and no checksum mismatches.
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))

	// 0007's binding is preserved: 0013 must NOT unbind existing projects.
	var bound sql.NullString
	must(t, pool.QueryRow(`SELECT space_id FROM projects WHERE id=$1`, ids[3]).Scan(&bound))
	if !bound.Valid || bound.String != ids[2] {
		t.Fatalf("upstream 0007 project binding lost: want %s got %v", ids[2], bound)
	}

	// projects.space_id is now nullable: an unscoped project may be created. The project and its
	// required main workspace go in one tx so the deferred one_main trigger fires once, at commit.
	unscopedTx, e := pool.Begin()
	must(t, e)
	_, e = unscopedTx.Exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,NULL,'Unscoped project','https://example.invalid/unscoped.git','main','active')`, ids[4], ids[1], ids[0])
	must(t, e)
	_, e = unscopedTx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,requested_ref) VALUES($1,$2,$3,$4,'main','running','ready','main')`, uuid.NewString(), ids[1], ids[0], ids[4])
	must(t, e)
	must(t, unscopedTx.Commit())
	if !columnNullable(t, pool, "projects", "space_id") {
		t.Fatal("projects.space_id must be nullable after 0013")
	}

	// Issue capability schema (0008-0012) is present.
	for _, table := range []string{"issues", "issue_comments", "issue_runs", "issue_activities", "issue_context_refs", "issue_interactions"} {
		if !tableExists(t, pool, table) {
			t.Fatalf("missing issue migration table %s", table)
		}
	}
}

// TestMigrationFullSequenceFreshDB verifies a clean database from empty through the complete
// sequence: Migrate PASS, CheckSchema PASS, and the final schema matches the application contract
// (nullable projects.space_id, composite space FK, project_space_list index, Issue schema intact).
func TestMigrationFullSequenceFreshDB(t *testing.T) {
	pool, _ := testSchema(t, "test_fresh_")
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))

	if !columnNullable(t, pool, "projects", "space_id") {
		t.Fatal("projects.space_id must be nullable after full sequence")
	}
	var idxCount int
	must(t, pool.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname=current_schema() AND indexname='project_space_list'`).Scan(&idxCount))
	if idxCount != 1 {
		t.Fatalf("project_space_list index missing (want 1, got %d)", idxCount)
	}
	// Composite FK (space_id, tenant_id) -> collab_workspaces must be intact.
	var fkCount int
	must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.referential_constraints rc JOIN information_schema.key_column_usage kcu
		ON rc.constraint_name=kcu.constraint_name AND rc.constraint_schema=kcu.constraint_schema
		WHERE rc.unique_constraint_schema=current_schema() AND kcu.table_schema=current_schema()
		AND kcu.table_name='projects' AND kcu.column_name='space_id' AND rc.delete_rule='NO ACTION'`).Scan(&fkCount))
	if fkCount < 1 {
		t.Fatalf("projects.space_id FK missing (want >=1, got %d)", fkCount)
	}

	for _, table := range []string{
		"issues", "issue_statuses", "issue_comments", "labels", "issue_labels", "issue_subscribers",
		"issue_views", "issue_runs", "issue_activities", "issue_context_refs", "issue_interactions",
		"collab_workspaces", "collab_workspace_members",
	} {
		if !tableExists(t, pool, table) {
			t.Fatalf("missing table %s", table)
		}
	}

	// Sanity: a project can be created unscoped on the fresh schema. All rows go in one tx so the
	// deferred triggers (tenant_admin, one_main) fire once, at commit.
	fuid, ftid, fpid := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, e := pool.Begin()
	must(t, e)
	_, e = tx.Exec(`INSERT INTO users(id,display_name,status) VALUES($1,'Fresh user','active')`, fuid)
	must(t, e)
	_, e = tx.Exec(`INSERT INTO tenants(id,name,status) VALUES($1,'Fresh tenant','active')`, ftid)
	must(t, e)
	_, e = tx.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, ftid, fuid)
	must(t, e)
	_, e = tx.Exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle)
		VALUES($1,$2,$3,NULL,'Fresh','https://example.invalid/fresh.git','main','active')`, fpid, ftid, fuid)
	must(t, e)
	// A live project requires exactly one main workspace (deferred one_main trigger in 0001).
	_, e = tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,requested_ref)
		VALUES($1,$2,$3,$4,'main','running','ready','main')`, uuid.NewString(), ftid, fuid, fpid)
	must(t, e)
	must(t, tx.Commit())
}

// Migration 0016 retires the Project storage and worktree steps without touching their history:
// in-flight operations still in storage or worktree fail with lifecycle_flow_retired, a project
// deletion waiting on storage deletion returns to cleanup, planned effects keep their records, and
// every Workspace gets the ref it will clone. Running Migrate again changes nothing.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #retired-storage-and-worktree-steps-leave-history-intact.
func TestMigration0016RetiresStorageAndWorktreeStepsKeepingHistory(t *testing.T) {
	pool, _ := testSchema(t, "test_m16_")
	entries, e := os.ReadDir(filepath.Join("..", "internal", "core", "migrations"))
	must(t, e)
	var previous []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".sql" && entry.Name() < "0016" {
			previous = append(previous, entry.Name())
		}
	}
	applyMigrationsUpTo(t, pool, previous)

	user, tenant := uuid.NewString(), uuid.NewString()
	type legacy struct{ project, workspace, operation, effect, kind, step, state, effectKind, effectState string }
	cases := map[string]*legacy{
		"storage":        {kind: "create_project", step: "storage", state: "queued"},
		"worktree":       {kind: "create_project", step: "worktree", state: "running", effectKind: "worktree_ensure", effectState: "planned"},
		"storage_delete": {kind: "delete_project", step: "storage_delete", state: "retry_wait", effectKind: "worktree_delete", effectState: "succeeded"},
		"done":           {kind: "create_project", step: "done", state: "succeeded", effectKind: "storage_ensure", effectState: "succeeded"},
	}
	tx, e := pool.Begin()
	must(t, e)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := tx.Exec(q, args...)
		must(t, err)
	}
	exec("INSERT INTO users(id,display_name,status) VALUES($1,'Legacy user','active')", user)
	exec("INSERT INTO tenants(id,name,status) VALUES($1,'Legacy tenant','active')", tenant)
	exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", tenant, user)
	for _, c := range cases {
		c.project, c.workspace, c.operation, c.effect = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		exec("INSERT INTO projects(id,tenant_id,owner_user_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,'Legacy','https://example.invalid/legacy.git','trunk','active')", c.project, tenant, user)
		exec("INSERT INTO project_storage(project_id,substrate_storage_id,observed_state) VALUES($1,$2,'ready')", c.project, "storage-"+c.project)
		exec("INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state) VALUES($1,$2,$3,$4,'main','running','provisioning')", c.workspace, tenant, user, c.project)
		exec("INSERT INTO workspace_worktrees(workspace_id,relative_path,branch_name,requested_ref,provisioning_state) VALUES($1,$2,$3,'release','pending')", c.workspace, "workspaces/"+c.workspace+"/checkout", "ora/"+c.workspace)
		exec("INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,controller_epoch) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'{}','k','h',1)", c.operation, tenant, user, c.project, c.workspace, c.kind, c.state, c.step)
		if c.effectKind != "" {
			exec("INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch) VALUES($1,$2,$3,$4,$5,$6,'{\"kind\":\"legacy\"}',1)", c.effect, c.operation, c.project, c.workspace, c.effectKind, c.effectState)
		}
	}
	// A Workspace without a worktree row takes its Project's default branch.
	bare := uuid.NewString()
	exec("INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state) VALUES($1,$2,$3,$4,'isolated','stopped','stopped')", bare, tenant, user, cases["done"].project)
	exec("INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'Bare')", uuid.NewString(), bare)
	must(t, tx.Commit())
	snapshot := func(q string) string {
		t.Helper()
		var out string
		must(t, pool.QueryRow(q).Scan(&out))
		return out
	}
	storageBefore := snapshot("SELECT string_agg(row_to_json(s)::text,'|' ORDER BY project_id) FROM project_storage s")
	worktreesBefore := snapshot("SELECT string_agg(row_to_json(w)::text,'|' ORDER BY workspace_id) FROM workspace_worktrees w")
	effectsBefore := snapshot("SELECT string_agg(row_to_json(e)::text,'|' ORDER BY id) FROM external_effects e")

	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))

	if snapshot("SELECT string_agg(row_to_json(s)::text,'|' ORDER BY project_id) FROM project_storage s") != storageBefore ||
		snapshot("SELECT string_agg(row_to_json(w)::text,'|' ORDER BY workspace_id) FROM workspace_worktrees w") != worktreesBefore ||
		snapshot("SELECT string_agg(row_to_json(e)::text,'|' ORDER BY id) FROM external_effects e") != effectsBefore {
		t.Fatal("migration rewrote retired storage, worktree or effect history")
	}
	want := map[string]struct{ state, step, errorCode string }{
		"storage":        {"failed", "storage", "lifecycle_flow_retired"},
		"worktree":       {"failed", "worktree", "lifecycle_flow_retired"},
		"storage_delete": {"retry_wait", "cleanup", ""},
		"done":           {"succeeded", "done", ""},
	}
	for name, c := range cases {
		var state, step string
		var code sql.NullString
		must(t, pool.QueryRow("SELECT state,step,error_code FROM operations WHERE id=$1", c.operation).Scan(&state, &step, &code))
		if w := want[name]; state != w.state || step != w.step || code.String != w.errorCode {
			t.Errorf("%s operation became %s/%s/%s, want %v", name, state, step, code.String, w)
		}
		var ref string
		var base sql.NullString
		must(t, pool.QueryRow("SELECT requested_ref,base_commit_id FROM workspaces WHERE id=$1", c.workspace).Scan(&ref, &base))
		if ref != "release" || base.Valid {
			t.Errorf("%s workspace got ref %q base %v, want the worktree's ref and no baseline", name, ref, base)
		}
	}
	if snapshot("SELECT requested_ref FROM workspaces WHERE id='"+bare+"'") != "trunk" {
		t.Error("a Workspace without a worktree must take its Project's default branch")
	}
}
