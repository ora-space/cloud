package integration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/core"
)

// skeletonStore applies every forward-only migration to a fresh isolated PostgreSQL
// schema and returns the store plus its pool, proving 0018 applies cleanly on top of
// 0001..0017 and that Migrate/CheckSchema stay green after a second apply.
func skeletonStore(t *testing.T) (*core.Store, *sql.DB) {
	t.Helper()
	pool, _ := testSchema(t, "test_skeleton_")
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, e)
	store, e := core.NewStore(db)
	must(t, e)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, store.CheckSchema(context.Background()))
	return store, pool
}

// skeletonSeed holds the ids of one minimal ownership chain: user, tenant, the
// tenant's collaboration space, a project with its main workspace, one issue, one
// agent IssueRun, and the run's isolated workspace bound both ways.
type skeletonSeed struct {
	userID, tenantID, spaceID, projectID, mainWorkspaceID, issueID, runID, agentID, runWorkspaceID string
}

// seedAgentIssueRunSkeleton inserts the ownership chain the business tables reference and binds the
// run and its Workspace both ways. It needs the whole schema, including the business half.
func seedAgentIssueRunSkeleton(t *testing.T, pool *sql.DB) skeletonSeed {
	t.Helper()
	s := seedOwnershipChain(t, pool)
	bindWorkspaceToRun(t, pool, s)
	bindRunToWorkspace(t, pool, s)
	return s
}

// seedOwnershipChain inserts one minimal ownership chain: user, tenant, the tenant's collaboration
// space, a project with its main workspace, one issue, one agent IssueRun, and the run's isolated
// workspace.
//
// It deliberately writes neither half of the run↔Workspace binding, because which of those columns
// exists depends on how far the migrations under test have been applied: the control plane's
// back-reference arrives in 0024 and the business layer's handle in 0030. Upgrade-path tests seed a
// live database *before* the migration they exercise runs, so they bind afterwards through one of the
// two helpers below; fresh-schema tests use seedAgentIssueRunSkeleton.
func seedOwnershipChain(t *testing.T, pool *sql.DB) skeletonSeed {
	t.Helper()
	newID := func() string { return uuid.NewString() }
	s := skeletonSeed{
		userID: newID(), tenantID: newID(), spaceID: newID(), projectID: newID(),
		mainWorkspaceID: newID(), issueID: newID(), runID: newID(), agentID: newID(), runWorkspaceID: newID(),
	}
	tx, e := pool.Begin()
	must(t, e)
	defer func() { _ = tx.Rollback() }()
	exec := func(q string, args ...any) {
		_, e := tx.Exec(q, args...)
		must(t, e)
	}
	exec(`INSERT INTO users(id, display_name, status) VALUES($1, 'Skeleton user', 'active')`, s.userID)
	exec(`INSERT INTO tenants(id, name, status) VALUES($1, 'Skeleton tenant', 'active')`, s.tenantID)
	exec(`INSERT INTO tenant_memberships(tenant_id, user_id, role, status) VALUES($1, $2, 'admin', 'active')`, s.tenantID, s.userID)
	exec(`INSERT INTO collab_workspaces(id, tenant_id, name, slug, description, created_by) VALUES($1, $2, 'Skeleton space', 'skeleton-space', '', $3)`, s.spaceID, s.tenantID, s.userID)
	// The Project's repository moved from Project columns to its Binding in 0037.
	var bindings bool
	must(t, tx.QueryRow(`SELECT to_regclass('repository_bindings') IS NOT NULL`).Scan(&bindings))
	if bindings {
		exec(`WITH p AS (INSERT INTO projects(id,tenant_id,owner_user_id,name,lifecycle,space_id) VALUES($1,$2,$3,'Skeleton project','active',$4) RETURNING id,tenant_id) INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch) SELECT gen_random_uuid(),tenant_id,id,'https://example.invalid/skeleton.git','main' FROM p`, s.projectID, s.tenantID, s.userID, s.spaceID)
	} else {
		exec(`INSERT INTO projects(id, tenant_id, owner_user_id, name, repository_url, default_branch, lifecycle, space_id) VALUES($1, $2, $3, 'Skeleton project', 'https://example.invalid/skeleton.git', 'main', 'active', $4)`, s.projectID, s.tenantID, s.userID, s.spaceID)
	}
	exec(`INSERT INTO workspaces(id, tenant_id, owner_user_id, project_id, kind, desired_state, observed_state, requested_ref) VALUES($1, $2, $3, $4, 'main', 'running', 'ready', 'main')`, s.mainWorkspaceID, s.tenantID, s.userID, s.projectID)
	exec(`INSERT INTO issues(id, tenant_id, creator_user_id, title, number) VALUES($1, $2, $3, 'Skeleton issue', 1)`, s.issueID, s.tenantID, s.userID)
	exec(`INSERT INTO issue_runs(id, tenant_id, issue_id, executor_type, executor_id) VALUES($1, $2, $3, 'agent', $4)`, s.runID, s.tenantID, s.issueID, s.agentID)
	exec(`INSERT INTO workspaces(id, tenant_id, owner_user_id, project_id, kind, desired_state, observed_state, requested_ref) VALUES($1, $2, $3, $4, 'isolated', 'running', 'ready', 'main')`, s.runWorkspaceID, s.tenantID, s.userID, s.projectID)
	// 0003: a live isolated workspace must own exactly one task identity; the
	// deferred task_identity trigger checks this at commit, so seed the task here.
	exec(`INSERT INTO tasks(id, workspace_id, title) VALUES($1, $2, 'Skeleton run workspace task')`, newID(), s.runWorkspaceID)
	must(t, tx.Commit())
	return s
}

// bindWorkspaceToRun writes the control plane's half of the binding (0024): the Workspace names the
// IssueRun that owns it. It is what the control plane resolves run operations by.
func bindWorkspaceToRun(t *testing.T, pool *sql.DB, s skeletonSeed) {
	t.Helper()
	_, e := pool.Exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, s.runID, s.runWorkspaceID)
	must(t, e)
}

// bindRunToWorkspace writes the business layer's half of the binding (0030): the run names its
// exclusive run Workspace. Only the business layer writes it, so it does not exist before 0030.
func bindRunToWorkspace(t *testing.T, pool *sql.DB, s skeletonSeed) {
	t.Helper()
	_, e := pool.Exec(`UPDATE issue_runs SET workspace_id=$1 WHERE id=$2`, s.runWorkspaceID, s.runID)
	must(t, e)
}

// wantPGError asserts err is a PostgreSQL error with the given SQLSTATE.
func wantPGError(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want *pgconn.PgError (SQLSTATE %s), got %T: %v", code, err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("want SQLSTATE %s, got %s: %v", code, pgErr.Code, err)
	}
}

// TestAgentIssueRunSkeletonMigration verifies the new migration lands on a fresh
// schema: the agent columns, the workspaces back-reference, and the two new tables.
func TestAgentIssueRunSkeletonMigration(t *testing.T) {
	_, pool := skeletonStore(t)
	hasColumn := func(table, column string) {
		t.Helper()
		var n int
		must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2`, table, column).Scan(&n))
		if n != 1 {
			t.Fatalf("column %s.%s missing", table, column)
		}
	}
	for _, col := range []string{"phase", "workspace_id", "cancel_requested_at", "thread_state", "idle_since"} {
		hasColumn("issue_runs", col)
	}
	hasColumn("workspaces", "issue_run_id")
	for _, table := range []string{"space_agents", "thread_entries"} {
		var n int
		must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=$1`, table).Scan(&n))
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}
}

// TestAgentIssueRunSkeletonSchemaConstraints exercises the CHECK/FK/UNIQUE guards of
// 0018 against a seeded chain: space_agents, the agent-only issue_runs columns, the
// mutual run-workspace binding, and thread_entries.
func TestAgentIssueRunSkeletonSchemaConstraints(t *testing.T) {
	_, pool := skeletonStore(t)
	s := seedAgentIssueRunSkeleton(t, pool)

	// space_agents (IssueRun D1): one active-or-retired row per (space, plugin).
	_, e := pool.Exec(`INSERT INTO space_agents(id, space_id, tenant_id, plugin_id, display_name, status) VALUES($1, $2, $3, 'official/hello-world', 'Hello World', 'active')`, uuid.NewString(), s.spaceID, s.tenantID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO space_agents(id, space_id, tenant_id, plugin_id, display_name, status) VALUES($1, $2, $3, 'official/hello-world', 'Hello World', 'retired')`, uuid.NewString(), s.spaceID, s.tenantID)
	wantPGError(t, e, "23505") // one row per (space, plugin), even across statuses
	_, e = pool.Exec(`INSERT INTO space_agents(id, space_id, tenant_id, plugin_id, display_name, status) VALUES($1, $2, $3, 'official/other', 'Other', 'broken')`, uuid.NewString(), s.spaceID, s.tenantID)
	wantPGError(t, e, "23514") // status must be active|retired
	_, e = pool.Exec(`INSERT INTO space_agents(id, space_id, tenant_id, plugin_id, display_name, status) VALUES($1, $2, $3, $4, 'Long id', 'active')`, uuid.NewString(), s.spaceID, s.tenantID, strings.Repeat("p", 301))
	wantPGError(t, e, "23514") // plugin_id is bounded, not interpreted: the catalog owns its shape
	_, e = pool.Exec(`INSERT INTO space_agents(id, space_id, tenant_id, plugin_id, display_name, status) VALUES($1, $2, $3, 'official/ghost', 'Ghost', 'active')`, uuid.NewString(), uuid.NewString(), s.tenantID)
	wantPGError(t, e, "23503") // space must exist

	// issue_runs agent columns (IssueRun D3, Thread D1/D4).
	_, e = pool.Exec(`UPDATE issue_runs SET phase='provisioning', thread_state='pending' WHERE id=$1`, s.runID)
	must(t, e)
	_, e = pool.Exec(`UPDATE issue_runs SET phase='bogus' WHERE id=$1`, s.runID)
	wantPGError(t, e, "23514") // phase must be in the D3 state machine
	_, e = pool.Exec(`UPDATE issue_runs SET thread_state='sleeping' WHERE id=$1`, s.runID)
	wantPGError(t, e, "23514") // thread_state must be pending|active|idle|ending|ended
	_, e = pool.Exec(`INSERT INTO issue_runs(id, tenant_id, issue_id, executor_type, executor_id, phase) VALUES($1, $2, $3, 'workflow', $4, 'running')`, uuid.NewString(), s.tenantID, s.issueID, uuid.NewString())
	wantPGError(t, e, "23514") // phase is only used for executor_type = 'agent'

	// Mutual run-workspace binding (IssueRun D2): each side is unique and FK'd.
	_, e = pool.Exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, uuid.NewString(), s.runWorkspaceID)
	wantPGError(t, e, "23503") // workspaces.issue_run_id must reference a real run
	otherRun := uuid.NewString()
	_, e = pool.Exec(`INSERT INTO issue_runs(id, tenant_id, issue_id, executor_type, executor_id) VALUES($1, $2, $3, 'agent', $4)`, otherRun, s.tenantID, s.issueID, uuid.NewString())
	must(t, e)
	_, e = pool.Exec(`UPDATE issue_runs SET workspace_id=$1 WHERE id=$2`, s.runWorkspaceID, otherRun)
	wantPGError(t, e, "23505") // one run per workspace
	otherWorkspace := uuid.NewString()
	bindTX, e := pool.Begin()
	must(t, e)
	_, e = bindTX.Exec(`INSERT INTO workspaces(id, tenant_id, owner_user_id, project_id, kind, desired_state, observed_state, requested_ref) VALUES($1, $2, $3, $4, 'isolated', 'running', 'ready', 'main')`, otherWorkspace, s.tenantID, s.userID, s.projectID)
	must(t, e)
	// Keep the task_identity trigger (0003) satisfied for the second isolated workspace.
	_, e = bindTX.Exec(`INSERT INTO tasks(id, workspace_id, title) VALUES($1, $2, 'Skeleton second workspace task')`, uuid.NewString(), otherWorkspace)
	must(t, e)
	must(t, bindTX.Commit())
	_, e = pool.Exec(`UPDATE workspaces SET issue_run_id=$1 WHERE id=$2`, s.runID, otherWorkspace)
	wantPGError(t, e, "23505") // one workspace per run
	_, e = pool.Exec(`UPDATE issue_runs SET workspace_id=$1 WHERE id=$2`, uuid.NewString(), s.runID)
	wantPGError(t, e, "23503") // issue_runs.workspace_id must reference a real workspace

	// thread_entries (Thread D1/D2): sources, gapless seq, object record, and the
	// node-sourced idempotency key.
	turnID := uuid.NewString()
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 1, 'system', 'user_turn', '{"role":"system"}'::jsonb)`, s.runID)
	must(t, e)
	// 0022 gives a user turn its D3 lifecycle, and makes it mandatory: a persisted user turn is
	// 'queued' until the Node echoes it or the session ends (see thread_entries_user_status).
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id, status) VALUES($1, 2, 'user', 'user_turn', '{"role":"user","content":"hi"}'::jsonb, $2, 'queued')`, s.runID, turnID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, node_execution_id, node_sequence) VALUES($1, 3, 'node', 'message', '{"role":"assistant"}'::jsonb, 'exec-1', 1)`, s.runID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 3, 'node', 'message', '{}'::jsonb)`, s.runID)
	wantPGError(t, e, "23505") // (run_id, seq) primary key
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, node_execution_id, node_sequence) VALUES($1, 4, 'node', 'message', '{}'::jsonb, 'exec-1', 1)`, s.runID)
	wantPGError(t, e, "23505") // (node_execution_id, node_sequence) idempotency key
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 5, 'bot', 'message', '{}'::jsonb)`, s.runID)
	wantPGError(t, e, "23514") // source must be node|user|system
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 0, 'system', 'user_turn', '{}'::jsonb)`, s.runID)
	wantPGError(t, e, "23514") // seq starts at 1
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 6, 'system', 'user_turn', '[1,2]'::jsonb)`, s.runID)
	wantPGError(t, e, "23514") // record must be a JSON object
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, node_execution_id) VALUES($1, 7, 'node', 'message', '{}'::jsonb, 'exec-2')`, s.runID)
	wantPGError(t, e, "23514") // node_execution_id and node_sequence are paired
	big := `{"data":"` + strings.Repeat("x", 262200) + `"}`
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 8, 'system', 'user_turn', $2::jsonb)`, s.runID, big)
	wantPGError(t, e, "23514") // single entry at most 256 KiB (Thread D2)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id) VALUES($1, 9, 'user', 'user_turn', '{}'::jsonb, $2)`, s.runID, uuid.NewString())
	wantPGError(t, e, "23514") // a user turn without its D3 lifecycle (0022)
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record, status) VALUES($1, 10, 'system', 'user_turn', '{}'::jsonb, 'queued')`, s.runID)
	wantPGError(t, e, "23514") // a node/system entry can never carry one
	_, e = pool.Exec(`INSERT INTO thread_entries(run_id, seq, source, kind, record) VALUES($1, 1, 'system', 'user_turn', '{}'::jsonb)`, uuid.NewString())
	wantPGError(t, e, "23503") // run must exist
}
