package integration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

const (
	scheduledTaskMigration                    = "0061_scheduled_tasks.sql"
	scheduledTaskModelMigration               = "0071_scheduled_task_model.sql"
	scheduledTaskConversationReasonsMigration = "0072_scheduled_task_conversation_reasons.sql"
)

// seedScheduledTaskAgent inserts the one space_agents row a definition binds, returning its id.
func seedScheduledTaskAgent(t *testing.T, pool *sql.DB, s skeletonSeed) string {
	t.Helper()
	return seedScheduledTaskAgentWithPlugin(t, pool, s, "official/ora-space.opencode")
}

func seedScheduledTaskAgentWithPlugin(t *testing.T, pool *sql.DB, s skeletonSeed, pluginID string) string {
	t.Helper()
	agentID := uuid.NewString()
	_, err := pool.Exec(`INSERT INTO space_agents(id, space_id, tenant_id, plugin_id, display_name, status)
		VALUES($1, $2, $3, $4, 'Codex', 'active')`, agentID, s.spaceID, s.tenantID, pluginID)
	must(t, err)
	return agentID
}

// scheduledTaskColumns is every required definition column in the seeded order below.
const scheduledTaskColumns = "id, tenant_id, issue_id, creator_user_id, agent_id, name, prompt, schedule, timezone, revision, version"

func insertScheduledTask(t *testing.T, pool *sql.DB, s skeletonSeed, agentID, tail string) string {
	t.Helper()
	id := uuid.NewString()
	_, err := pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`) VALUES($1,$2,$3,$4,$5,'x','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1)`+tail, id, s.tenantID, s.issueID, s.userID, agentID)
	must(t, err)
	return id
}

func TestMigration0061ScheduledTasksFreshSchemaAndConstraints(t *testing.T) {
	_, pool := skeletonStore(t)
	s := seedAgentIssueRunSkeleton(t, pool)
	agentID := seedScheduledTaskAgent(t, pool, s)
	taskID := insertScheduledTask(t, pool, s, agentID, "")

	for _, table := range []string{"scheduled_tasks", "scheduled_task_occurrences"} {
		var n int
		must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.tables
			WHERE table_schema=current_schema() AND table_name=$1`, table).Scan(&n))
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}
	for _, index := range []string{
		"scheduled_tasks_tenant_list", "scheduled_tasks_due",
		"scheduled_task_occurrences_run", "scheduled_task_deadlines_due",
	} {
		var n int
		must(t, pool.QueryRow(`SELECT count(*) FROM pg_indexes
			WHERE schemaname=current_schema() AND indexname=$1`, index).Scan(&n))
		if n != 1 {
			t.Fatalf("index %s missing", index)
		}
	}

	// The idempotent admission identity is (task_id, scheduled_at): a repeated point is refused.
	first := `INSERT INTO scheduled_task_occurrences
		(tenant_id, task_id, scheduled_at, outcome, task_revision, run_id, observed_at)
		VALUES($1,$2,'2026-01-02T03:04:05Z','admitted',1,$3,now())`
	_, err := pool.Exec(first, s.tenantID, taskID, s.runID)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO scheduled_task_occurrences
		(tenant_id, task_id, scheduled_at, outcome, task_revision, observed_at)
		VALUES($1,$2,'2026-01-02T03:04:05Z','missed',1,now())`, s.tenantID, taskID)
	wantPGError(t, err, "23505")

	occurrence := func(fields, values, code string) {
		t.Helper()
		_, e := pool.Exec(`INSERT INTO scheduled_task_occurrences
			(tenant_id, task_id, scheduled_at, `+fields+`)
			VALUES($1,$2,'2026-01-03T00:00:00Z',`+values+`)`, s.tenantID, taskID)
		wantPGError(t, e, code)
	}
	occurrence("outcome, task_revision, observed_at", "'paused',1,now()", "23514")
	occurrence("outcome, failure_reason, task_revision, observed_at", "'failed','quota_exceeded',1,now()", "23514")

	// Definition shape guards: a complete insert that breaks exactly one guard each.
	guard := func(tail, code string) {
		t.Helper()
		_, e := pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`) VALUES($1,$2,$3,$4,$5,`+tail+`)`, uuid.NewString(), s.tenantID, s.issueID, s.userID, agentID)
		wantPGError(t, e, code)
	}
	guard(`'','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1`, "23514") // empty name
	guard(`'x','','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1`, "23514") // empty prompt
	guard(`'x','x','[]'::jsonb,'Asia/Shanghai',1,1`, "23514")                                      // array schedule
	guard(`'x','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'',1,1`, "23514")             // empty timezone

	_, e := pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`,enabled) VALUES($1,$2,$3,$4,$5,'enabled-no-consent','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1,true)`, uuid.NewString(), s.tenantID, s.issueID, s.userID, agentID)
	wantPGError(t, e, "23514")
	_, e = pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`,enabled,unattended_policy,unattended_consented_at) VALUES($1,$2,$3,$4,$5,'enabled-no-instant','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1,true,'full_auto_in_cloud_workspace',now())`, uuid.NewString(), s.tenantID, s.issueID, s.userID, agentID)
	wantPGError(t, e, "23514")

	// Foreign agent and issue must hit the FK, not the (tenant, issue, creator, name) unique key.
	_, e = pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`) VALUES($1,$2,$3,$4,$5,'foreign-agent','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1)`, uuid.NewString(), s.tenantID, s.issueID, s.userID, uuid.NewString())
	wantPGError(t, e, "23503") // foreign agent
	_, e = pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`) VALUES($1,$2,$3,$4,$5,'foreign-issue','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1)`, uuid.NewString(), s.tenantID, uuid.NewString(), s.userID, agentID)
	wantPGError(t, e, "23503") // foreign issue

	// One definition per (tenant, issue, creator, name).
	_, e = pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`) VALUES($1,$2,$3,$4,$5,'Daily sync','Sync','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1)`, uuid.NewString(), s.tenantID, s.issueID, s.userID, agentID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`) VALUES($1,$2,$3,$4,$5,'Daily sync','Sync','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1)`, uuid.NewString(), s.tenantID, s.issueID, s.userID, agentID)
	wantPGError(t, e, "23505")

	// A disabled definition may omit consent and enabled_at and carries its own reason.
	_, e = pool.Exec(`INSERT INTO scheduled_tasks (`+scheduledTaskColumns+`,disabled_reason) VALUES($1,$2,$3,$4,$5,'Off','x','{"kind":"every_hours","intervalHours":1}'::jsonb,'Asia/Shanghai',1,1,'manual')`, uuid.NewString(), s.tenantID, s.issueID, s.userID, agentID)
	must(t, e)
}

func TestMigration0061ScheduledTasksUpgradeAndReplay(t *testing.T) {
	pool, _ := testSchema(t, "test_scheduled59u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, scheduledTaskMigration))
	s := seedAgentIssueRunSkeleton(t, pool)
	before := scheduledMigrationBusinessSnapshot(t, pool)

	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	var tables int
	must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema=current_schema() AND table_name IN ('scheduled_tasks','scheduled_task_occurrences')`).Scan(&tables))
	if tables != 2 {
		t.Fatalf("upgrade added %d scheduled-task tables, want 2", tables)
	}
	if after := scheduledMigrationBusinessSnapshot(t, pool); before != after {
		t.Fatal("scheduled-task migration rewrote unrelated business facts")
	}

	agentID := seedScheduledTaskAgent(t, pool, s)
	taskID := insertScheduledTask(t, pool, s, agentID, "")
	_, err := pool.Exec(`INSERT INTO scheduled_task_occurrences
		(tenant_id, task_id, scheduled_at, outcome, task_revision, observed_at)
		VALUES($1,$2,'2026-01-02T03:04:05Z','skipped_busy',1,now())`, s.tenantID, taskID)
	must(t, err)

	// Migrating again is a no-op: the runner skips the applied file by checksum, and the seeded
	// definitions and occurrences are left untouched.
	must(t, store.Migrate(context.Background()))
	var definitions, occurrences int
	must(t, pool.QueryRow(`SELECT count(*) FROM scheduled_tasks`).Scan(&definitions))
	must(t, pool.QueryRow(`SELECT count(*) FROM scheduled_task_occurrences`).Scan(&occurrences))
	if definitions != 1 || occurrences != 1 {
		t.Fatalf("replay left %d definitions / %d occurrences, want 1/1", definitions, occurrences)
	}
}

func TestMigration0071ScheduledTaskModelUpgradeAndReplay(t *testing.T) {
	pool, _ := testSchema(t, "test_scheduled71u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, scheduledTaskModelMigration))
	s := seedAgentIssueRunSkeleton(t, pool)
	agentID := seedScheduledTaskAgent(t, pool, s)
	taskID := insertScheduledTask(t, pool, s, agentID, "")

	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	var model sql.NullString
	must(t, pool.QueryRow(`SELECT model FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&model))
	if model.Valid {
		t.Fatalf("0071 must leave existing definitions without a selected model, got %q", model.String)
	}
	_, err := pool.Exec(`UPDATE scheduled_tasks SET model='vendor/model-v2' WHERE id=$1`, taskID)
	must(t, err)
	must(t, store.Migrate(context.Background()))
	must(t, pool.QueryRow(`SELECT model FROM scheduled_tasks WHERE id=$1`, taskID).Scan(&model))
	if !model.Valid || model.String != "vendor/model-v2" {
		t.Fatalf("replaying 0071 must preserve selected model, got %#v", model)
	}
}

// TestMigration0072ScheduledTaskConversationReasonsUpgradeAndReplay widens the failure reason set
// without rewriting an existing failed occurrence, and still refuses a reason outside D7.
func TestMigration0072ScheduledTaskConversationReasonsUpgradeAndReplay(t *testing.T) {
	pool, _ := testSchema(t, "test_scheduled72u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, scheduledTaskConversationReasonsMigration))
	s := seedAgentIssueRunSkeleton(t, pool)
	agentID := seedScheduledTaskAgent(t, pool, s)
	taskID := insertScheduledTask(t, pool, s, agentID, "")
	occurrence := func(at, reason string) error {
		_, err := pool.Exec(`INSERT INTO scheduled_task_occurrences(tenant_id, task_id, scheduled_at, outcome, failure_reason, task_revision, observed_at)
			VALUES($1,$2,$3::timestamptz,'failed',$4,1,now())`, s.tenantID, taskID, at, reason)
		return err
	}
	must(t, occurrence("2026-01-01T00:00:00Z", "target_unavailable"))
	if occurrence("2026-01-01T01:00:00Z", "task_paused") == nil {
		t.Fatal("before 0072 the failure reason set must not accept task_paused")
	}

	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, occurrence("2026-01-01T01:00:00Z", "task_paused"))
	must(t, occurrence("2026-01-01T02:00:00Z", "task_handoff_pending"))
	if occurrence("2026-01-01T03:00:00Z", "skipped_busy") == nil {
		t.Fatal("0072 must still refuse a failure reason outside D7")
	}
	var reason string
	must(t, pool.QueryRow(`SELECT failure_reason FROM scheduled_task_occurrences WHERE task_id=$1 AND scheduled_at='2026-01-01T00:00:00Z'`, taskID).Scan(&reason))
	if reason != "target_unavailable" {
		t.Fatalf("0072 must not rewrite an existing occurrence, got %q", reason)
	}
}

// scheduledMigrationBusinessSnapshot captures the business tables a definition references so the
// upgrade path can prove it rewrote none of them.
func scheduledMigrationBusinessSnapshot(t *testing.T, pool *sql.DB) string {
	t.Helper()
	var snapshot string
	must(t, pool.QueryRow(`SELECT jsonb_build_object(
		'users',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM users r),
		'tenants',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM tenants r),
		'issues',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM issues r),
		'space_agents',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM space_agents r),
		'issue_runs',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM issue_runs r)
		)::text`).Scan(&snapshot))
	return snapshot
}
