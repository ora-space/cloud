package integration

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const residualReclaimMigration = "0057_agent_run_residual_reclamation.sql"

func seedResidualMigrationRow(t *testing.T, pool *sql.DB, seed skeletonSeed, reason string) {
	t.Helper()
	_, err := pool.Exec(`INSERT INTO agent_run_residuals
		(run_id,tenant_id,project_id,workspace_id,runtime_generation,responsibility,reason,session_ended_at)
		VALUES($1,$2,$3,$4,27,'{"lateFacts":{"original":{"state":"ended"}},"original":"keep"}',$5,'2026-01-02T03:04:05Z')`,
		seed.runID, seed.tenantID, seed.projectID, seed.runWorkspaceID, reason)
	must(t, err)
}

// Compare the complete old ledger and business rows, rather than only the newly added defaults.
// Columns added by later additive migrations are excluded, as in the other upgrade-path tests.
func residualMigrationHistory(t *testing.T, pool *sql.DB) string {
	t.Helper()
	var snapshot string
	must(t, pool.QueryRow(`SELECT jsonb_build_object(
		'residuals',(SELECT jsonb_agg(to_jsonb(r)-ARRAY['reclaim_state','disposition','reclaim_id','reclaim_mode',
		  'reclaim_operation_id','reclaim_authorization','reclaim_targets','reclaim_evidence','reclaim_version',
		  'reclaim_checked_at','reclaim_conflicted_at','stop_evidence'] ORDER BY run_id) FROM agent_run_residuals r),
		'runs',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM issue_runs r),
		'workspaces',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM workspaces w),
		'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM operations o))::text`).Scan(&snapshot))
	return snapshot
}

func TestMigration0057ResidualReclamationFreshAndConstraints(t *testing.T) {
	pool, _ := testSchema(t, "test_residual57f_")
	must(t, newStoreOnSchema(t, pool).Migrate(context.Background()))
	seed := seedAgentIssueRunSkeleton(t, pool)
	seedResidualMigrationRow(t, pool, seed, "agent_start_failed")
	var columns int
	must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='agent_run_residuals'
		AND column_name IN ('reclaim_state','disposition','reclaim_id','reclaim_mode','reclaim_operation_id',
		'reclaim_authorization','reclaim_targets','reclaim_evidence','reclaim_version','reclaim_checked_at','reclaim_conflicted_at')`).Scan(&columns))
	if columns != 11 {
		t.Fatalf("fresh schema has %d reclamation columns, want 11", columns)
	}
	var scanDefinition, scanPredicate string
	must(t, pool.QueryRow(`SELECT pg_get_indexdef(indexrelid),pg_get_expr(indpred,indrelid)
		FROM pg_index WHERE indexrelid='agent_run_residuals_reclaim_scan'::regclass`).Scan(&scanDefinition, &scanPredicate))
	if !strings.Contains(scanDefinition, "(reclaim_checked_at NULLS FIRST, created_at, run_id)") ||
		!strings.Contains(scanPredicate, "reconciling") || !strings.Contains(scanPredicate, "reclaiming") || strings.Contains(scanPredicate, "cleared") {
		t.Fatalf("residual scan must order pending rows fairly: %s WHERE %s", scanDefinition, scanPredicate)
	}
	var initialized bool
	must(t, pool.QueryRow(`SELECT reclaim_state='reconciling' AND disposition IS NULL AND reclaim_id IS NULL
		AND reclaim_mode IS NULL AND reclaim_operation_id IS NULL AND reclaim_authorization='{}'::jsonb
		AND reclaim_targets='[]'::jsonb AND reclaim_evidence='[]'::jsonb AND reclaim_version=0
		AND reclaim_checked_at IS NULL AND reclaim_conflicted_at IS NULL AND cleared_at IS NULL
		FROM agent_run_residuals WHERE run_id=$1`, seed.runID).Scan(&initialized))
	if !initialized {
		t.Fatal("fresh residual did not receive empty reconciling defaults")
	}
	op := uuid.NewString()
	_, err := pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash)
		VALUES($1,$2,$3,$4,$5,'delete_workspace','succeeded','done','{}','residual-migration','fixture-request')`,
		op, seed.tenantID, seed.userID, seed.projectID, seed.runWorkspaceID)
	must(t, err)
	claim := "reclaim_state='reclaiming',reclaim_id='" + uuid.NewString() + "',reclaim_mode='owned_delete',reclaim_authorization='{\"source\":\"test\"}',reclaim_operation_id='" + op + "'"
	for _, tc := range []struct {
		name, set string
	}{
		{"unknown state", "reclaim_state='unknown'"},
		{"null state", "reclaim_state=NULL"},
		{"unknown disposition", "disposition='unknown'"},
		{"unknown mode", "reclaim_mode='unknown'"},
		{"authorization array", "reclaim_authorization='[]'"},
		{"authorization null", "reclaim_authorization=NULL"},
		{"targets object", "reclaim_targets='{}'"},
		{"targets null", "reclaim_targets=NULL"},
		{"evidence object", "reclaim_evidence='{}'"},
		{"evidence null", "reclaim_evidence=NULL"},
		{"negative version", "reclaim_version=-1"},
		{"null version", "reclaim_version=NULL"},
		{"reconciling id", "reclaim_id='" + uuid.NewString() + "'"},
		{"reconciling mode", "reclaim_mode='controlled'"},
		{"reconciling operation", "reclaim_operation_id='" + op + "'"},
		{"reconciling authorization", "reclaim_authorization='{\"source\":\"test\"}'"},
		{"missing id", "reclaim_state='reclaiming',reclaim_mode='controlled',reclaim_authorization='{\"source\":\"test\"}'"},
		{"missing mode", "reclaim_state='reclaiming',reclaim_id='" + uuid.NewString() + "',reclaim_authorization='{\"source\":\"test\"}'"},
		{"empty authorization", "reclaim_state='reclaiming',reclaim_id='" + uuid.NewString() + "',reclaim_mode='controlled'"},
		{"owned delete without operation", "reclaim_state='reclaiming',reclaim_id='" + uuid.NewString() + "',reclaim_mode='owned_delete',reclaim_authorization='{\"source\":\"test\"}'"},
		{"cleared missing timestamp", strings.Replace(claim, "reclaiming", "cleared", 1) + ",disposition='released'"},
		{"cleared missing disposition", strings.Replace(claim, "reclaiming", "cleared", 1) + ",cleared_at=now()"},
		{"reconciling timestamp", "cleared_at=now()"},
		{"reconciling disposition", "disposition='released'"},
		{"reclaiming timestamp", claim + ",cleared_at=now()"},
		{"reclaiming disposition", claim + ",disposition='released'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec("UPDATE agent_run_residuals SET "+tc.set+" WHERE run_id=$1", seed.runID)
			code := "23514"
			if strings.Contains(tc.name, "null") {
				code = "23502"
			}
			wantPGError(t, err, code)
		})
	}
	_, err = pool.Exec("UPDATE agent_run_residuals SET "+strings.Replace(claim, op, uuid.NewString(), 1)+" WHERE run_id=$1", seed.runID)
	wantPGError(t, err, "23503")
	_, err = pool.Exec("UPDATE agent_run_residuals SET "+claim+" WHERE run_id=$1", seed.runID)
	must(t, err)
	other := seed
	other.runID, other.runWorkspaceID = newBoundRun(t, pool, seed)
	seedResidualMigrationRow(t, pool, other, "workspace_unavailable")
	_, err = pool.Exec(`UPDATE agent_run_residuals SET reclaim_state='reclaiming',reclaim_mode='controlled',
		reclaim_authorization='{"source":"test"}',reclaim_id=(SELECT reclaim_id FROM agent_run_residuals WHERE run_id=$1)
		WHERE run_id=$2`, seed.runID, other.runID)
	wantPGError(t, err, "23505")
	_, err = pool.Exec(`UPDATE agent_run_residuals SET reclaim_state='reclaiming',reclaim_mode='controlled',
		reclaim_authorization='{"source":"test"}',reclaim_id=$1,reclaim_operation_id=$2 WHERE run_id=$3`, uuid.NewString(), op, other.runID)
	wantPGError(t, err, "23505")
	// Controlled responsibility may omit an operation; all three explicit dispositions are legal.
	_, err = pool.Exec(`UPDATE agent_run_residuals SET reclaim_state='reclaiming',reclaim_mode='controlled',
		reclaim_authorization='{"source":"test"}',reclaim_id=$1 WHERE run_id=$2`, uuid.NewString(), other.runID)
	must(t, err)
	for _, disposition := range []string{"released", "terminated_data_retained", "externally_confirmed"} {
		_, err = pool.Exec(`UPDATE agent_run_residuals SET reclaim_state='cleared',cleared_at=now(),disposition=$1 WHERE run_id=$2`, disposition, other.runID)
		must(t, err)
	}
}

func TestMigration0057ResidualReclamationUpgradePreservesHistory(t *testing.T) {
	pool, _ := testSchema(t, "test_residual57u_")
	// Start after the mainline 0054 schema, including the 0052 settlement and 0053 clone retirement.
	applyMigrationsUpTo(t, pool, migrationsBefore(t, residualReclaimMigration))
	seed := seedAgentIssueRunSkeleton(t, pool)
	seedResidualMigrationRow(t, pool, seed, "agent_start_failed")
	other := seed
	other.runID, other.runWorkspaceID = newBoundRun(t, pool, seed)
	seedResidualMigrationRow(t, pool, other, "workspace_unavailable")
	before := residualMigrationHistory(t, pool)
	must(t, newStoreOnSchema(t, pool).Migrate(context.Background()))
	if after := residualMigrationHistory(t, pool); before != after {
		t.Fatal("reclamation upgrade rewrote legacy residual or business facts")
	}
	var count int
	must(t, pool.QueryRow(`SELECT count(*) FROM agent_run_residuals WHERE reclaim_state='reconciling'
		AND reclaim_authorization='{}'::jsonb AND reclaim_targets='[]'::jsonb AND reclaim_evidence='[]'::jsonb
		AND reclaim_version=0 AND reclaim_id IS NULL AND reclaim_mode IS NULL AND reclaim_operation_id IS NULL
		AND disposition IS NULL AND reclaim_checked_at IS NULL AND reclaim_conflicted_at IS NULL`).Scan(&count))
	if count != 2 {
		t.Fatalf("upgrade initialized %d legacy residuals, want both D4 and D5 without invented evidence", count)
	}
}

func TestMigration0057ResidualReclamationReplayPreservesProgress(t *testing.T) {
	pool, _ := testSchema(t, "test_residual57r_")
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	base := seedAgentIssueRunSkeleton(t, pool)
	for _, state := range []string{"reconciling", "reclaiming", "cleared"} {
		seed := base
		if state != "reconciling" {
			seed.runID, seed.runWorkspaceID = newBoundRun(t, pool, base)
		}
		seedResidualMigrationRow(t, pool, seed, "agent_start_failed")
		if state != "reconciling" {
			_, err := pool.Exec(`UPDATE agent_run_residuals SET reclaim_state=$1,reclaim_id=$2,reclaim_mode='controlled',
				reclaim_authorization='{"source":"test"}',reclaim_targets='[{"sandboxId":"original"}]',
				reclaim_evidence='[{"eventKey":"original"}]',reclaim_version=7,reclaim_checked_at=now(),reclaim_conflicted_at=now(),
				cleared_at=CASE WHEN $1='cleared' THEN now() END,disposition=CASE WHEN $1='cleared' THEN 'released' END
				WHERE run_id=$3`, state, uuid.NewString(), seed.runID)
			must(t, err)
		}
	}
	var before, after string
	query := "SELECT jsonb_agg(to_jsonb(r) ORDER BY run_id)::text FROM agent_run_residuals r"
	must(t, pool.QueryRow(query).Scan(&before))
	// Exercise both runner replay and direct reentrant DDL on already progressed rows.
	must(t, store.Migrate(context.Background()))
	ddl, err := os.ReadFile(filepath.Join("..", "internal", "core", "migrations", residualReclaimMigration))
	must(t, err)
	tx, err := pool.Begin()
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(string(ddl))
	must(t, err)
	must(t, tx.Commit())
	must(t, pool.QueryRow(query).Scan(&after))
	if before != after {
		t.Fatal("migration replay reset reclamation progress or evidence")
	}
}

func TestMigration0057ResidualReclamationRejectsLegacyClearedRows(t *testing.T) {
	pool, _ := testSchema(t, "test_residual57a2_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, residualReclaimMigration))
	seed := seedAgentIssueRunSkeleton(t, pool)
	seedResidualMigrationRow(t, pool, seed, "agent_start_failed")
	_, err := pool.Exec("UPDATE agent_run_residuals SET cleared_at='2026-01-03T03:04:05Z' WHERE run_id=$1", seed.runID)
	must(t, err)
	before := residualMigrationHistory(t, pool)
	err = newStoreOnSchema(t, pool).Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "non-null cleared_at") {
		t.Fatalf("legacy cleared row must produce the explicit A2 compatibility error, got %v", err)
	}
	if after := residualMigrationHistory(t, pool); before != after {
		t.Fatal("failed A2 migration changed original data")
	}
	var columns, recorded int
	must(t, pool.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='agent_run_residuals' AND column_name='reclaim_state'`).Scan(&columns))
	must(t, pool.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=$1", residualReclaimMigration).Scan(&recorded))
	if columns != 0 || recorded != 0 {
		t.Fatalf("failed A2 migration left schema/data state: columns=%d recorded=%d", columns, recorded)
	}
}
