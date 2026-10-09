package integration

// Migration reconciliation guards preserve the immutable upstream 0001-0016 sequence.
// Compatible tenants upgrade to 0017 without losing project bindings or runtime history;
// fresh databases must reach the same mandatory project-space association.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
	_, e := pool.Exec("CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())")
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
// must upgrade cleanly without re-running upstream migrations or losing project bindings.
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
		{"INSERT INTO collab_workspaces(id,tenant_id,name,slug,description,created_by) VALUES($1,$2,'Upgrade tenant','default','',$3)", []any{ids[2], ids[1], ids[0]}},
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

	// Now the current binary migrates the rest (0008-0017) on top of the upstream baseline.
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

	if columnNullable(t, pool, "projects", "space_id") {
		t.Fatal("projects.space_id must be mandatory after 0017")
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
// (mandatory projects.space_id, composite space FK, project_space_list index, Issue schema intact).
func TestMigrationFullSequenceFreshDB(t *testing.T) {
	pool, _ := testSchema(t, "test_fresh_")
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))

	if columnNullable(t, pool, "projects", "space_id") {
		t.Fatal("projects.space_id must be mandatory after full sequence")
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
		"collab_workspaces", "tenant_invitations", "tenant_join_links", "tenant_join_requests",
	} {
		if !tableExists(t, pool, table) {
			t.Fatalf("missing table %s", table)
		}
	}

	// Sanity: a project can be created in the tenant's sole space. All rows go in one tx so the
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
	fspace := uuid.NewString()
	_, e = tx.Exec(`INSERT INTO collab_workspaces(id,tenant_id,name,slug,description,created_by) VALUES($1,$2,'Fresh tenant','fresh-tenant','',$3)`, fspace, ftid, fuid)
	must(t, e)
	_, e = tx.Exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle)
		VALUES($1,$2,$3,$4,'Fresh','https://example.invalid/fresh.git','main','active')`, fpid, ftid, fuid, fspace)
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
	space := uuid.NewString()
	exec("INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'Legacy tenant','legacy-runtime',$3)", space, tenant, user)
	for _, c := range cases {
		c.project, c.workspace, c.operation, c.effect = uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
		exec("INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,$4,'Legacy','https://example.invalid/legacy.git','trunk','active')", c.project, tenant, user, space)
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

// seedDeclaredAgentRun inserts one more agent run on the seeded issue with its own live isolated run
// Workspace, optionally cancelled and optionally carrying the seq=1 first prompt. It is how the
// Thread tests start from a run that was dispatched but whose session was never activated.
func seedDeclaredAgentRun(t *testing.T, pool *sql.DB, s skeletonSeed, phase, status string, cancelled, withFirstPrompt bool) string {
	t.Helper()
	runID, wsID := uuid.NewString(), uuid.NewString()
	tx, e := pool.Begin()
	must(t, e)
	defer func() { _ = tx.Rollback() }()
	exec := func(q string, args ...any) {
		_, e := tx.Exec(q, args...)
		must(t, e)
	}
	exec(`INSERT INTO issue_runs(id, tenant_id, issue_id, executor_type, executor_id, phase, status)
		VALUES($1,$2,$3,'agent',$4,$5,$6)`, runID, s.tenantID, s.issueID, uuid.NewString(), phase, status)
	exec(`INSERT INTO workspaces(id, tenant_id, owner_user_id, project_id, kind, desired_state, observed_state, requested_ref)
		VALUES($1,$2,$3,$4,'isolated','running','ready','main')`, wsID, s.tenantID, s.userID, s.projectID)
	// 0003: a live isolated workspace owns exactly one task identity (deferred task_identity trigger).
	exec(`INSERT INTO tasks(id, workspace_id, title) VALUES($1,$2,'0022 run workspace task')`, uuid.NewString(), wsID)
	exec(`UPDATE issue_runs SET workspace_id=$1 WHERE id=$2`, wsID, runID)
	exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, runID, wsID)
	if cancelled {
		exec(`UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1`, runID)
	}
	if withFirstPrompt {
		exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id)
			VALUES($1, 1, 'system', 'user_turn', '{"content":"Begin this task."}', $2)`, runID, uuid.NewString())
	}
	must(t, tx.Commit())
	return runID
}

// migrationsBefore lists the migration files that sort before one named file, which is how a
// deployment that stopped at the previous version is reproduced: they are applied verbatim, with
// their checksums recorded, exactly as the runner would have applied them.
func migrationsBefore(t *testing.T, name string) []string {
	t.Helper()
	entries, e := os.ReadDir(filepath.Join("..", "internal", "core", "migrations"))
	must(t, e)
	var versions []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".sql" && entry.Name() < name {
			versions = append(versions, entry.Name())
		}
	}
	if len(versions) == 0 {
		t.Fatalf("no migration sorts before %s", name)
	}
	return versions
}

// migratedTwice lets the current binary apply the remaining migrations and asserts the promise every
// file in the sequence makes: a second Migrate changes nothing and CheckSchema agrees with the files
// on disk. Every upgrade-path test below starts from a schema the previous version left behind, so
// this is also the path the deployment takes.
func migratedTwice(t *testing.T, store *core.Store) {
	t.Helper()
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))
}

// newIsolatedWorkspace inserts one more live isolated Workspace of the seeded Project, with the task
// identity 0003 requires, and returns its id.
func newIsolatedWorkspace(t *testing.T, pool *sql.DB, s skeletonSeed) string {
	t.Helper()
	id := uuid.NewString()
	tx, e := pool.Begin()
	must(t, e)
	defer func() { _ = tx.Rollback() }()
	_, e = tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,requested_ref)
		VALUES($1,$2,$3,$4,'isolated','running','ready','main')`, id, s.tenantID, s.userID, s.projectID)
	must(t, e)
	_, e = tx.Exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'upgrade path task')`, uuid.NewString(), id)
	must(t, e)
	must(t, tx.Commit())
	return id
}

// newBoundRun inserts one more agent run of the seeded issue together with its own run Workspace,
// bound to it both ways, and returns both ids. It is the whole ownership chain a Revision row names,
// which is why the composite foreign key can be exercised with real rows instead of invented ids.
// Each run carries a fresh executor, because one queued run per (issue, executor) is allowed.
func newBoundRun(t *testing.T, pool *sql.DB, s skeletonSeed) (string, string) {
	t.Helper()
	runID, wsID := uuid.NewString(), uuid.NewString()
	tx, e := pool.Begin()
	must(t, e)
	defer func() { _ = tx.Rollback() }()
	_, e = tx.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,phase,status)
		VALUES($1,$2,$3,'agent',$4,'delivering','dispatched')`, runID, s.tenantID, s.issueID, uuid.NewString())
	must(t, e)
	_, e = tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,requested_ref)
		VALUES($1,$2,$3,$4,'isolated','running','ready','main')`, wsID, s.tenantID, s.userID, s.projectID)
	must(t, e)
	_, e = tx.Exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'upgrade path run task')`, uuid.NewString(), wsID)
	must(t, e)
	_, e = tx.Exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, runID, wsID)
	must(t, e)
	_, e = tx.Exec(`UPDATE issue_runs SET workspace_id=$1 WHERE id=$2`, wsID, runID)
	must(t, e)
	must(t, tx.Commit())
	return runID, wsID
}

// TestMigration0024AgentRunControlPlaneAppliesFreshAndUpgrades pins the schema the merged control
// plane runs on, on the path a deployment actually arrives by: a database that stopped at 0023,
// holding live data, receives 0024 alone.
//
// The constraints asserted here are the ones the control plane relies on instead of an
// application-level check, and each is asserted by violating it, because an index that exists but
// does not refuse is not the backstop the dispatch and receipt rules need: one unregistered work
// item per run, one pending execution per run, the Controller-generated execution identity, the
// receipt identity, and the Thread command queue's ordering.
//
// Fresh coverage is TestMigrationFullSequenceFreshDB, which migrates a clean database to the end.
func TestMigration0024AgentRunControlPlaneAppliesFreshAndUpgrades(t *testing.T) {
	pool, _ := testSchema(t, "test_cp24_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0024_agent_run_control_plane.sql"))
	if tableExists(t, pool, "execution_work") {
		t.Fatal("the upgrade path must start from a database without the control plane tables")
	}
	seed := seedOwnershipChain(t, pool)
	// The row as JSON, minus the columns a migration adds. An upgrade may extend a table, and what it
	// must never do is change a value that was already there; `to_jsonb` carries every column,
	// including the new ones, so the comparison drops exactly the ones this file introduces.
	rowOf := func(table, id string, added ...string) string {
		expr := "to_jsonb(r)"
		for _, column := range added {
			expr += " - '" + column + "'"
		}
		var raw string
		must(t, pool.QueryRow(`SELECT `+expr+`::text FROM `+table+` r WHERE id=$1`, id).Scan(&raw))
		return raw
	}
	runBefore := rowOf("issue_runs", seed.runID)
	workspaceBefore := rowOf("workspaces", seed.runWorkspaceID, "issue_run_id")

	// 0024 is applied alone first, so "it did not rewrite the rows it was applied on top of" is a
	// claim about this file and not about the later migrations the same Migrate call would run.
	applyMigrationsUpTo(t, pool, []string{"0024_agent_run_control_plane.sql"})
	if got := rowOf("issue_runs", seed.runID); got != runBefore {
		t.Fatalf("0024 must not rewrite the runs it was applied on top of:\n before %s\n after  %s", runBefore, got)
	}
	if got := rowOf("workspaces", seed.runWorkspaceID, "issue_run_id"); got != workspaceBefore {
		t.Fatalf("0024 must not rewrite the Workspaces it was applied on top of:\n before %s\n after  %s", workspaceBefore, got)
	}
	store := newStoreOnSchema(t, pool)
	migratedTwice(t, store)

	// workspaces.issue_run_id: the run Workspace's back-reference, which is how the control plane
	// resolves an operation to the run that owns it. Nullable, because a public Workspace has no run;
	// unique and referential, because a Workspace belongs to at most one run.
	if !columnNullable(t, pool, "workspaces", "issue_run_id") {
		t.Fatal("workspaces.issue_run_id must stay nullable — a public Workspace has no run")
	}
	bindWorkspaceToRun(t, pool, seed)
	other := newIsolatedWorkspace(t, pool, seed)
	_, e := pool.Exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, seed.runID, other)
	wantPGError(t, e, "23505") // one Workspace per run
	_, e = pool.Exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, uuid.NewString(), seed.runWorkspaceID)
	wantPGError(t, e, "23503") // the run must exist

	// execution_work: the work Cloud accepted for a Controller to claim. A run holds at most one
	// unregistered item, which is what makes a retried enqueue converge on the same work instead of
	// queueing a second delivery; registering it frees the slot for the next attempt.
	insertWork := func(runID, executionID string) error {
		_, e := pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target,execution_id)
			VALUES($1,$2,'deliver_revision','{"kind":"deliver_revision"}'::jsonb,'{}'::jsonb,NULLIF($3,''))`,
			uuid.NewString(), runID, executionID)
		return e
	}
	must(t, insertWork(seed.runID, ""))
	wantPGError(t, insertWork(seed.runID, ""), "23505")
	must(t, insertWork(seed.runID, "exec-registered"))
	wantPGError(t, insertWork(uuid.NewString(), ""), "23503") // the run must exist
	_, e = pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target)
		VALUES($1,$2,'bogus','{}'::jsonb,'{}'::jsonb)`, uuid.NewString(), seed.runID)
	wantPGError(t, e, "23514") // the work vocabulary is the control plane's own
	_, e = pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target)
		VALUES($1,$2,'agent_session','[]'::jsonb,'{}'::jsonb)`, uuid.NewString(), seed.runID)
	wantPGError(t, e, "23514") // the dispatch input is a frozen object
	var def string
	must(t, pool.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema()
		AND tablename='execution_work' AND indexname='one_unregistered_execution_work'`).Scan(&def))
	if !strings.Contains(def, "execution_id IS NULL") {
		t.Fatalf("one_unregistered_execution_work must be partial over execution_id IS NULL, got %q", def)
	}

	// node_executions: the Controller-generated execution identity, the run's single pending
	// execution, and the two distinct rules behind them. 0024 allows one *unsettled* run execution
	// per run, so the run's next delivery may start once the previous attempt has a durable result;
	// 0025 additionally pins one agent session per run for the run's whole life, because a run's
	// session is its identity and not a retryable attempt. A plugin step is a separate kind and
	// occupies neither slot.
	insertExecution := func(execution, operation, kind string) error {
		_, e := pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,node_id,node_operation_id,input,dispatched_epoch)
			VALUES($1,$2,$3,'node-a',$1,'{"kind":"agent_session"}'::jsonb,1)`, execution, kind, operation)
		return e
	}
	must(t, insertExecution("exec-a", seed.runID, "agent_session"))
	wantPGError(t, insertExecution("exec-a", seed.runID, "agent_session"), "23505") // the identity is the execution id
	wantPGError(t, insertExecution("exec-b", seed.runID, "agent_session"), "23505") // one session per run, settled or not
	wantPGError(t, insertExecution("exec-c", seed.runID, "deliver_revision"), "23505")
	wantPGError(t, insertExecution("exec-d", seed.runID, "bogus"), "23514") // the execution vocabulary
	must(t, insertExecution("exec-plugin", seed.runID, "install_plugins"))
	_, e = pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,node_id,node_operation_id,input,dispatched_epoch)
		VALUES($1,'deliver_revision',$2,$3,'node-a',$1,'{}'::jsonb,1)`, "exec-orphan", uuid.NewString(), uuid.NewString())
	wantPGError(t, e, "23503") // work_id must name accepted work
	_, e = pool.Exec(`UPDATE node_executions SET result='{"outcome":"agent_session_ended"}'::jsonb WHERE execution_id='exec-a'`)
	must(t, e)
	must(t, insertExecution("exec-delivery", seed.runID, "deliver_revision"))       // the pending slot is free again
	wantPGError(t, insertExecution("exec-f", seed.runID, "agent_session"), "23505") // the session slot never is

	// node_event_receipts: the receipt identity is (execution, sequence) in the Node's own sequence
	// space, and the acknowledged event is stored as the bytes the Controller sent — text, not a
	// decoded document, because the acknowledgement is of a byte range and not of a structure.
	var eventType string
	must(t, pool.QueryRow(`SELECT data_type FROM information_schema.columns WHERE table_schema=current_schema()
		AND table_name='node_event_receipts' AND column_name='event'`).Scan(&eventType))
	if eventType != "text" {
		t.Fatalf("node_event_receipts.event must be text (the Controller's own bytes), got %q", eventType)
	}
	insertReceipt := func(execution string, sequence int64) error {
		_, e := pool.Exec(`INSERT INTO node_event_receipts(execution_id,sequence,event) VALUES($1,$2,'eyJ0dXJuSWQiOiJ0In0=')`, execution, sequence)
		return e
	}
	must(t, insertReceipt("exec-a", 1))
	wantPGError(t, insertReceipt("exec-a", 1), "23505") // one receipt per (execution, sequence)
	must(t, insertReceipt("exec-a", 2))                 // the next sequence is a new fact
	wantPGError(t, insertReceipt("no-such-execution", 1), "23503")
	_, e = pool.Exec(`INSERT INTO node_event_receipts(execution_id,sequence,event) VALUES('exec-a',0,'x')`)
	wantPGError(t, e, "23514") // the Node's sequence space starts at 1

	// thread_commands: what Cloud still owes a session. The vocabulary and the payload shape are the
	// proto's; the delivery is a later fact of the same row. 0025 adds the ordinal that keeps the
	// order of commands accepted in one transaction independent of their created_at.
	insertCommand := func(kind, body string) error {
		_, e := pool.Exec(`INSERT INTO thread_commands(id,run_id,kind,body) VALUES($1,$2,$3,$4::jsonb)`,
			uuid.NewString(), seed.runID, kind, body)
		return e
	}
	must(t, insertCommand("submit_user_turn", `{"turnId":"t"}`))
	must(t, insertCommand("end_session", `{}`))
	wantPGError(t, insertCommand("bogus", `{}`), "23514")
	wantPGError(t, insertCommand("end_session", `[]`), "23514")
	var ordinals []int64
	rows, e := pool.Query(`SELECT queue_sequence FROM thread_commands ORDER BY queue_sequence`)
	must(t, e)
	defer rows.Close()
	for rows.Next() {
		var ordinal int64
		must(t, rows.Scan(&ordinal))
		ordinals = append(ordinals, ordinal)
	}
	must(t, rows.Err())
	if len(ordinals) != 2 || ordinals[0] >= ordinals[1] {
		t.Fatalf("thread_commands.queue_sequence must order commands monotonically, got %v", ordinals)
	}

	// space_agents: the Space's selected agent plugins, one row per (space, plugin) whatever the
	// status, so a retired agent stays nameable by past runs.
	if !tableExists(t, pool, "space_agents") {
		t.Fatal("space_agents must exist after 0024")
	}
}

// revisionSeed is one `revisions` row in the shape its columns demand, held as a value so each
// constraint below can be violated by rewriting exactly one field of an otherwise valid row.
type revisionSeed struct {
	id, executionID, tenantID, runID, workspaceID, projectID, repositoryURL string
	baseCommit, finalCommit, revisionRef                                    string
	bundleKey                                                               *string
	bundleSize                                                              *int64
	bundleSHA256                                                            *string
	historyKey                                                              string
	historySize                                                             int64
	historySHA256                                                           string
}

// newRevisionExecution registers the delivery execution that produced a Revision and Cloud's verdict
// on it, and returns the execution id the `revisions` row must name. Both `execution_id` and `run_id`
// are unique keys of a Revision, so every attempt gets a fresh one; the result is written because a
// settled execution is what a registered Revision must have come from.
func newRevisionExecution(t *testing.T, pool *sql.DB, runID, outcome string) string {
	t.Helper()
	id := "exec-" + uuid.NewString()
	_, e := pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,node_id,node_operation_id,input,result,dispatched_epoch)
		VALUES($1,'deliver_revision',$2,'node-a',$1,'{"kind":"deliver_revision"}'::jsonb,'{"outcome":"revision_delivered"}'::jsonb,1)`,
		id, runID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO revision_verifications(execution_id,outcome) VALUES($1,$2)`, id, outcome)
	must(t, e)
	return id
}

// validRevisionSeed is the row Cloud writes for a run whose checkout did not change: no bundle (the
// three columns move together) and a history that always exists (Cloud Revision invariant 5). The
// execution id is filled in by the caller, because it names a registration that must exist.
func validRevisionSeed(s skeletonSeed, runID, workspaceID, executionID string) revisionSeed {
	return revisionSeed{
		id: uuid.NewString(), executionID: executionID, tenantID: s.tenantID, runID: runID,
		workspaceID: workspaceID, projectID: s.projectID,
		repositoryURL: "https://example.invalid/skeleton.git",
		baseCommit:    strings.Repeat("a", 40), finalCommit: strings.Repeat("a", 40),
		revisionRef: "refs/ora/revisions/" + runID,
		historyKey:  "revisions/" + s.tenantID + "/" + runID + "/attempt/session.jsonl",
		historySize: 512, historySHA256: strings.Repeat("c", 64),
	}
}

// insertRevision inserts one seeded row and returns the error, so a case states the violation it
// expects rather than asserting on a boolean the insert either way cannot explain.
func insertRevision(t *testing.T, pool *sql.DB, row revisionSeed) error {
	t.Helper()
	_, e := pool.Exec(`
		INSERT INTO revisions(id, execution_id, tenant_id, run_id, workspace_id, project_id, repository_url,
		                      base_commit, final_commit, revision_ref,
		                      bundle_key, bundle_size, bundle_sha256,
		                      history_key, history_size, history_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		row.id, row.executionID, row.tenantID, row.runID, row.workspaceID, row.projectID, row.repositoryURL,
		row.baseCommit, row.finalCommit, row.revisionRef,
		row.bundleKey, row.bundleSize, row.bundleSHA256,
		row.historyKey, row.historySize, row.historySHA256)
	return e
}

// assertRevisionSchema checks the constraints 0027 is the authority for, on a database that has
// already applied it. The seed supplies the run the row must reference, because the foreign keys are
// checked against real rows: a schema check that inserted invented ids would pass on a table with no
// foreign keys at all.
//
// The uniqueness is asserted by violating it rather than by reading `pg_indexes`, because "a unique
// index exists" and "two rows for one run are refused" are different claims and only the second is
// the one the registration rule relies on (Cloud Revision D4, invariant 7).
func assertRevisionSchema(t *testing.T, pool *sql.DB, s skeletonSeed) {
	t.Helper()
	for _, table := range []string{"revisions", "revision_verifications", "revision_delivery_skips"} {
		if !tableExists(t, pool, table) {
			t.Fatalf("%s must exist after 0027", table)
		}
	}

	var pkColumns []string
	rows, e := pool.Query(`
		SELECT kcu.column_name FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema AND kcu.table_name = tc.table_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = 'revisions' AND tc.constraint_type = 'PRIMARY KEY'
		ORDER BY kcu.ordinal_position`)
	must(t, e)
	defer rows.Close()
	for rows.Next() {
		var col string
		must(t, rows.Scan(&col))
		pkColumns = append(pkColumns, col)
	}
	must(t, rows.Err())
	if len(pkColumns) != 1 || pkColumns[0] != "id" {
		t.Fatalf("revisions PRIMARY KEY must be (id) — Cloud's own generated identity, got %v", pkColumns)
	}

	// Every accepted row names a whole ownership chain, so each case that must be accepted gets its
	// own run and Workspace: a spare run is bound to the Workspace a Revision may name, which is what
	// the composite foreign key checks. The cases that must be refused reuse one spare chain, because
	// a refused insert leaves nothing behind for the next case to trip over.
	freeRun, freeWorkspace := newBoundRun(t, pool, s)
	changedRun, changedWorkspace := newBoundRun(t, pool, s)

	// A well-formed row is accepted, and it names no expiry: retention is an explicit non-goal of the
	// approved decision, so a defaulted window here would promise a rule nothing enforces.
	first := validRevisionSeed(s, s.runID, s.runWorkspaceID, newRevisionExecution(t, pool, s.runID, "unchanged"))
	must(t, insertRevision(t, pool, first))
	var expires sql.NullTime
	must(t, pool.QueryRow(`SELECT expires_at FROM revisions WHERE id=$1`, first.id).Scan(&expires))
	if expires.Valid {
		t.Fatalf("expires_at must default to NULL in the first version, got %v", expires.Time)
	}

	// One Revision per logical delivery, and one logical delivery per run (invariant 7).
	second := validRevisionSeed(s, s.runID, s.runWorkspaceID, newRevisionExecution(t, pool, s.runID, "unchanged"))
	wantPGError(t, insertRevision(t, pool, second), "23505")
	// The id is an identity in its own right, not a surrogate made redundant by the run's uniqueness:
	// the same id under a different run is refused too.
	other := validRevisionSeed(s, freeRun, freeWorkspace, newRevisionExecution(t, pool, freeRun, "unchanged"))
	other.id = first.id
	wantPGError(t, insertRevision(t, pool, other), "23505")

	// The run must exist, and the Workspace must be the one that run owns: the composite reference is
	// what keeps a Revision from describing a Workspace that never belonged to its run.
	orphan := validRevisionSeed(s, uuid.NewString(), uuid.NewString(), newRevisionExecution(t, pool, uuid.NewString(), "unchanged"))
	wantPGError(t, insertRevision(t, pool, orphan), "23503")
	mismatched := validRevisionSeed(s, freeRun, s.runWorkspaceID, newRevisionExecution(t, pool, freeRun, "unchanged"))
	wantPGError(t, insertRevision(t, pool, mismatched), "23503")

	// The execution is what Cloud judged, so a Revision cannot name one that was never registered or
	// never verified. Both are asserted directly, because a Revision body without its verdict would be
	// a registered delivery nothing vouches for.
	_, e = pool.Exec(`INSERT INTO revisions(id,execution_id,tenant_id,run_id,workspace_id,project_id,repository_url,
		base_commit,final_commit,revision_ref,history_key,history_size,history_sha256)
		VALUES($1,'no-such-execution',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		uuid.NewString(), s.tenantID, freeRun, freeWorkspace, s.projectID, "https://example.invalid/skeleton.git",
		strings.Repeat("a", 40), strings.Repeat("a", 40), "refs/ora/revisions/"+freeRun,
		"revisions/history.jsonl", 1, strings.Repeat("c", 64))
	wantPGError(t, e, "23503")
	unverified := "exec-" + uuid.NewString()
	_, e = pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,node_id,node_operation_id,input,result,dispatched_epoch)
		VALUES($1,'deliver_revision',$2,'node-a',$1,'{}'::jsonb,'{"outcome":"revision_delivered"}'::jsonb,1)`, unverified, freeRun)
	must(t, e)
	row := validRevisionSeed(s, freeRun, freeWorkspace, unverified)
	wantPGError(t, insertRevision(t, pool, row), "23503")
	// outcome and reason are one fact: only a failed verification may carry one.
	_, e = pool.Exec(`UPDATE revision_verifications SET reason='verification_failed' WHERE execution_id=$1`,
		first.executionID)
	wantPGError(t, e, "23514")

	// An absent bundle is a fact of the row, so the three columns move together or not at all, and a
	// bundle that exists carries a real object: a positive size and a lowercase digest. A changed
	// checkout is the other legal shape — the constraint refuses drift between the columns, not the
	// bundle itself.
	key := "revisions/" + s.tenantID + "/" + freeRun + "/attempt/revision.bundle"
	size := int64(4096)
	sha := strings.Repeat("d", 64)
	for _, tc := range []struct {
		name   string
		mutate func(*revisionSeed)
	}{
		{"a bundle size without its key", func(r *revisionSeed) { r.bundleSize = &size }},
		{"a bundle digest without its key", func(r *revisionSeed) { r.bundleSHA256 = &sha }},
		{"a bundle key without its size", func(r *revisionSeed) { r.bundleKey = &key }},
		{"a zero bundle size", func(r *revisionSeed) { r.bundleKey, r.bundleSize, r.bundleSHA256 = &key, ptrInt64(0), &sha }},
		{"a negative bundle size", func(r *revisionSeed) { r.bundleKey, r.bundleSize, r.bundleSHA256 = &key, ptrInt64(-1), &sha }},
		{"an uppercase bundle digest", func(r *revisionSeed) {
			upper := strings.ToUpper(sha)
			r.bundleKey, r.bundleSize, r.bundleSHA256 = &key, &size, &upper
		}},
		{"a changed checkout without a bundle", func(r *revisionSeed) { r.finalCommit = strings.Repeat("b", 40) }},
		{"an uppercase history digest", func(r *revisionSeed) { r.historySHA256 = strings.ToUpper(r.historySHA256) }},
		{"a history digest that is not a digest", func(r *revisionSeed) { r.historySHA256 = "nope" }},
		{"a negative history size", func(r *revisionSeed) { r.historySize = -1 }},
		{"a final commit that is not a commit", func(r *revisionSeed) { r.finalCommit = "not-a-commit" }},
		{"a base commit that is not a commit", func(r *revisionSeed) { r.baseCommit = strings.Repeat("A", 40) }},
	} {
		attempt := validRevisionSeed(s, freeRun, freeWorkspace, newRevisionExecution(t, pool, freeRun, "unchanged"))
		tc.mutate(&attempt)
		wantPGError(t, insertRevision(t, pool, attempt), "23514")
	}

	changed := validRevisionSeed(s, changedRun, changedWorkspace, newRevisionExecution(t, pool, changedRun, "delivered"))
	changed.finalCommit = strings.Repeat("b", 40)
	changed.bundleKey, changed.bundleSize, changed.bundleSHA256 = &key, &size, &sha
	must(t, insertRevision(t, pool, changed))

	// A delivery Cloud decided to skip is one row per run as well: the skip is the delivery's
	// settlement, so a second one would be a second decision about the same delivery.
	_, e = pool.Exec(`INSERT INTO revision_delivery_skips(run_id) VALUES($1)`, s.runID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO revision_delivery_skips(run_id) VALUES($1)`, s.runID)
	wantPGError(t, e, "23505")
}

// ptrInt64 is the address of a fresh int64, for the seeded rows whose bundle columns are pointers
// because NULL and zero are different facts about them.
func ptrInt64(v int64) *int64 { return &v }

// TestMigration0027VerifiedRevisionsAppliesFreshAndUpgrades verifies the verified-Revision schema on
// both paths a deployment can arrive by: a fresh database that runs the whole sequence, and a
// database already at 0026 that receives 0027 alone.
//
// 0027 preserves the raw Node evidence and adds Cloud's own verdict beside it, so the upgrade path's
// obligation is that it applies on top of live data without rewriting it. The constraints asserted
// below are the ones the registration rule rests on (Cloud Revision D4): a Revision is a
// verified delivery of one run, and every column is refused unless the whole row is coherent.
func TestMigration0027VerifiedRevisionsAppliesFreshAndUpgrades(t *testing.T) {
	// Fresh: the whole sequence, applied twice (idempotent), with the schema check green.
	freshPool, _ := testSchema(t, "test_rev27f_")
	fresh := newStoreOnSchema(t, freshPool)
	migratedTwice(t, fresh)
	assertRevisionSchema(t, freshPool, seedAgentIssueRunSkeleton(t, freshPool))

	// Upgrade: everything before 0027 applied as the runner would, live data seeded, then 0027.
	pool, _ := testSchema(t, "test_rev27_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0027_verified_revisions.sql"))
	if tableExists(t, pool, "revisions") {
		t.Fatal("the upgrade path must start from a database without the Revisions tables")
	}
	seed := seedOwnershipChain(t, pool)
	bindWorkspaceToRun(t, pool, seed)
	var before string
	must(t, pool.QueryRow(`SELECT to_jsonb(r)::text FROM issue_runs r WHERE id=$1`, seed.runID).Scan(&before))

	// 0027 is applied alone first, so the no-rewrite claim is about this file and not about the later
	// migrations the same Migrate call would run (0030 adds the business columns to issue_runs).
	applyMigrationsUpTo(t, pool, []string{"0027_verified_revisions.sql"})
	var after string
	must(t, pool.QueryRow(`SELECT to_jsonb(r)::text FROM issue_runs r WHERE id=$1`, seed.runID).Scan(&after))
	if after != before {
		t.Fatalf("0027 must not rewrite the runs it was applied on top of:\n before %s\n after  %s", before, after)
	}
	store := newStoreOnSchema(t, pool)
	migratedTwice(t, store)
	assertRevisionSchema(t, pool, seed)
}

// TestMigration0030AgentRunBusinessLifecycleAppliesFreshAndUpgrades pins the business half of an
// Agent IssueRun on the upgrade path: a database at the control plane's last migration receives the
// lifecycle columns, the exclusive run-Workspace binding, the cancellation request and the Thread
// state, without one existing row being rewritten.
//
// The columns are asserted by violating each guard, because they are the database's share of the
// business state machine: a phase outside D3's set, a Thread state outside D4's, a Workspace claimed
// by two runs, and an agent column on a run that is not an agent run are all refused here rather than
// by the Go code that happens to write them.
func TestMigration0030AgentRunBusinessLifecycleAppliesFreshAndUpgrades(t *testing.T) {
	// Fresh: the whole sequence, applied twice (idempotent), with the schema check green.
	freshPool, _ := testSchema(t, "test_life30f_")
	fresh := newStoreOnSchema(t, freshPool)
	migratedTwice(t, fresh)
	seedAgentIssueRunSkeleton(t, freshPool)

	// Upgrade: everything before 0030 applied as the runner would, live data seeded, then 0030.
	pool, _ := testSchema(t, "test_life30_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0030_agent_run_business_lifecycle.sql"))
	if !tableExists(t, pool, "revision_delivery_skips") {
		t.Fatal("the upgrade path must start from the control plane's last migration (0029)")
	}
	for _, col := range []string{"phase", "workspace_id", "cancel_requested_at", "thread_state", "idle_since"} {
		_, e := pool.Exec(`SELECT ` + col + ` FROM issue_runs LIMIT 1`)
		if e == nil {
			t.Fatalf("issue_runs.%s must not exist before 0030", col)
		}
	}
	seed := seedOwnershipChain(t, pool)
	bindWorkspaceToRun(t, pool, seed)
	// 0030 adds columns (which can only ever read as NULL on the rows that predate it) and touches no
	// existing column, so the promise to keep is that every pre-existing key of the row still holds
	// exactly the value it held. The comparison removes the added keys, because `to_jsonb` of a row
	// gains one entry per new column.
	added := []string{"phase", "workspace_id", "cancel_requested_at", "thread_state", "idle_since"}
	strip := " - ARRAY['" + strings.Join(added, "','") + "']::text[]"
	rowOf := func(table, id, projection string) string {
		var raw string
		must(t, pool.QueryRow(`SELECT (`+projection+`)::text FROM `+table+` r WHERE id=$1`, id).Scan(&raw))
		return raw
	}
	runBefore := rowOf("issue_runs", seed.runID, "to_jsonb(r)"+strip)
	workspaceBefore := rowOf("workspaces", seed.runWorkspaceID, "to_jsonb(r)")

	// 0030 is applied alone first, for the same reason: the later 0031 must not be able to account for
	// a difference this file's own claim is about.
	applyMigrationsUpTo(t, pool, []string{"0030_agent_run_business_lifecycle.sql"})
	if got := rowOf("issue_runs", seed.runID, "to_jsonb(r)"+strip); got != runBefore {
		t.Fatalf("0030 must not rewrite the runs it was applied on top of:\n before %s\n after  %s", runBefore, got)
	}
	if got := rowOf("workspaces", seed.runWorkspaceID, "to_jsonb(r)"); got != workspaceBefore {
		t.Fatalf("0030 must not rewrite the Workspaces it was applied on top of:\n before %s\n after  %s", workspaceBefore, got)
	}
	// And the rows it was applied on top of carry no business state: nothing backfilled a phase or a
	// Thread for a run that predates the file.
	for _, col := range added {
		var value any
		must(t, pool.QueryRow(`SELECT to_jsonb(r)->>$2 FROM issue_runs r WHERE id=$1`, seed.runID, col).Scan(&value))
		if value != nil {
			t.Fatalf("0030 must not invent %s for a run that predates it, got %v", col, value)
		}
	}
	store := newStoreOnSchema(t, pool)
	migratedTwice(t, store)

	// The business half of the binding, written by the business layer alone (D6 table ownership).
	bindRunToWorkspace(t, pool, seed)
	secondRun, _ := newBoundRun(t, pool, seed)
	_, e := pool.Exec(`UPDATE issue_runs SET workspace_id=$1 WHERE id=$2`, seed.runWorkspaceID, secondRun)
	wantPGError(t, e, "23505") // one run per run Workspace

	// The state machines the Go layer advances, as far as the database can see them.
	_, e = pool.Exec(`UPDATE issue_runs SET phase='provisioning', thread_state='pending' WHERE id=$1`, seed.runID)
	must(t, e)
	_, e = pool.Exec(`UPDATE issue_runs SET phase='bogus' WHERE id=$1`, seed.runID)
	wantPGError(t, e, "23514")
	_, e = pool.Exec(`UPDATE issue_runs SET thread_state='sleeping' WHERE id=$1`, seed.runID)
	wantPGError(t, e, "23514")
	// A non-agent run must never carry them: the business transitions never write them and the control
	// plane never reads them, so a value there could only be a bug.
	_, e = pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,phase)
		VALUES($1,$2,$3,'workflow',$4,'running')`, uuid.NewString(), seed.tenantID, seed.issueID, uuid.NewString())
	wantPGError(t, e, "23514")

	// The idle-window scan is partial over the state that gives idle_since its meaning.
	var def string
	must(t, pool.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema()
		AND tablename='issue_runs' AND indexname='issue_runs_idle_threads'`).Scan(&def))
	if !strings.Contains(def, "thread_state = 'idle'") {
		t.Fatalf("issue_runs_idle_threads must be partial over thread_state = 'idle', got %q", def)
	}
}

// TestMigration0031AgentRunThreadEntriesAppliesFreshAndUpgrades pins the durable Thread on the
// upgrade path: a database that already holds runs receives the Thread table, and its identity and
// guards are the ones the append and read paths rely on. One row per (run, seq) makes the Thread an
// ordered log; the (execution, sequence) unique index is what makes a replayed Node batch append
// nothing; and the turn lifecycle belongs to user turns alone, which is what makes "no user turn
// still queued" a two-valued idle predicate.
func TestMigration0031AgentRunThreadEntriesAppliesFreshAndUpgrades(t *testing.T) {
	// Fresh: the whole sequence, applied twice (idempotent), with the schema check green.
	freshPool, _ := testSchema(t, "test_te31f_")
	fresh := newStoreOnSchema(t, freshPool)
	migratedTwice(t, fresh)
	seedAgentIssueRunSkeleton(t, freshPool)

	// Upgrade: everything before 0031 applied as the runner would, live data seeded, then 0031.
	pool, _ := testSchema(t, "test_te31_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0031_agent_run_thread_entries.sql"))
	if !tableExists(t, pool, "issue_runs") || !tableExists(t, pool, "revision_delivery_skips") {
		t.Fatal("the upgrade path must start from the control plane and the business lifecycle")
	}
	if tableExists(t, pool, "thread_entries") {
		t.Fatal("thread_entries must not exist before 0031")
	}
	seed := seedAgentIssueRunSkeleton(t, pool)

	store := newStoreOnSchema(t, pool)
	migratedTwice(t, store)

	var pkColumns []string
	rows, e := pool.Query(`
		SELECT kcu.column_name FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema AND kcu.table_name = tc.table_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = 'thread_entries' AND tc.constraint_type = 'PRIMARY KEY'
		ORDER BY kcu.ordinal_position`)
	must(t, e)
	defer rows.Close()
	for rows.Next() {
		var col string
		must(t, rows.Scan(&col))
		pkColumns = append(pkColumns, col)
	}
	must(t, rows.Err())
	if len(pkColumns) != 2 || pkColumns[0] != "run_id" || pkColumns[1] != "seq" {
		t.Fatalf("thread_entries PRIMARY KEY must be (run_id, seq) — the Thread's own order, got %v", pkColumns)
	}
	var hasFK bool
	must(t, pool.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = tc.constraint_name AND kcu.table_schema = tc.table_schema AND kcu.table_name = tc.table_name
		WHERE tc.table_schema=current_schema() AND tc.table_name='thread_entries'
		  AND tc.constraint_type='FOREIGN KEY' AND kcu.column_name='run_id'
	)`).Scan(&hasFK))
	if !hasFK {
		t.Fatal("thread_entries.run_id must reference the run whose Thread it is")
	}
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record) VALUES($1,1,'system','user_turn','{}'::jsonb)`, seed.runID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record) VALUES($1,1,'system','user_turn','{}'::jsonb)`, seed.runID)
	wantPGError(t, e, "23505") // one entry per (run, seq)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,node_execution_id) VALUES($1,2,'node','message','{"role":"assistant"}'::jsonb,'exec-1')`, seed.runID)
	wantPGError(t, e, "23514") // a node entry must carry both halves of its idempotency key: the sequence is missing
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,node_execution_id,node_sequence)
		VALUES($1,3,'node','message','{}'::jsonb,'exec-1',1)`, seed.runID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,node_execution_id,node_sequence)
		VALUES($1,4,'node','message','{}'::jsonb,'exec-1',1)`, seed.runID)
	wantPGError(t, e, "23505") // a replayed Node batch appends nothing

	for _, idx := range []struct{ name, predicate string }{
		{"thread_entries_queued", "status = 'queued'"},
		{"thread_entries_node_uniq", "node_execution_id IS NOT NULL"},
	} {
		var def string
		must(t, pool.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname=current_schema()
			AND tablename='thread_entries' AND indexname=$1`, idx.name).Scan(&def))
		if !strings.Contains(def, idx.predicate) {
			t.Fatalf("%s must be partial over %q, got %q", idx.name, idx.predicate, def)
		}
	}
}

// TestMigration0032UserGitIdentitiesAppliesFreshAndUpgrades covers 0032 on a fresh database and as an
// upgrade over 0031 with an existing user: the table is created once, an existing user keeps their
// row untouched, and the CHECKs refuse what identity-access D1 refuses.
func TestMigration0032UserGitIdentitiesAppliesFreshAndUpgrades(t *testing.T) {
	freshPool, _ := testSchema(t, "test_gi32f_")
	migratedTwice(t, newStoreOnSchema(t, freshPool))
	if !tableExists(t, freshPool, "user_git_identities") {
		t.Fatal("a fresh database must have user_git_identities")
	}

	pool, _ := testSchema(t, "test_gi32_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0032_user_git_identities.sql"))
	if tableExists(t, pool, "user_git_identities") {
		t.Fatal("user_git_identities must not exist before 0032")
	}
	user := uuid.NewString()
	_, e := pool.Exec(`INSERT INTO users(id,display_name,status) VALUES($1,'Existing','active')`, user)
	must(t, e)
	migratedTwice(t, newStoreOnSchema(t, pool))

	var name string
	must(t, pool.QueryRow(`SELECT display_name FROM users WHERE id=$1`, user).Scan(&name))
	if name != "Existing" {
		t.Fatalf("an existing user row must be untouched, got %q", name)
	}
	_, e = pool.Exec(`INSERT INTO user_git_identities(user_id,name,email) VALUES($1,'Ada','ada@example.invalid')`, user)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO user_git_identities(user_id,name,email) VALUES($1,'Ada','ada@example.invalid')`, user)
	wantPGError(t, e, "23505") // one identity per user
	other := uuid.NewString()
	_, e = pool.Exec(`INSERT INTO users(id,display_name,status) VALUES($1,'Other','active')`, other)
	must(t, e)
	for _, bad := range [][2]string{{"", "a@b"}, {"a\nb", "a@b"}, {"<a>", "a@b"}, {"Ada", "no-at"}, {"Ada", "a b@c"}} {
		_, e = pool.Exec(`INSERT INTO user_git_identities(user_id,name,email) VALUES($1,$2,$3)`, other, bad[0], bad[1])
		wantPGError(t, e, "23514")
	}
	_, e = pool.Exec(`INSERT INTO user_git_identities(user_id,name,email) VALUES($1,'Ghost','g@x')`, uuid.NewString())
	wantPGError(t, e, "23503") // the identity belongs to a real user
}
