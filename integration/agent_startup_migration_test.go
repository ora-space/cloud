package integration

import (
	"testing"

	"github.com/google/uuid"
)

func TestMigration0034StartupWindowsPreserveExistingRunOnUpgrade(t *testing.T) {
	fresh, _ := testSchema(t, "test_start34f_")
	migratedTwice(t, newStoreOnSchema(t, fresh))
	if !tableExists(t, fresh, "agent_run_residuals") {
		t.Fatal("fresh schema missing residual ownership")
	}
	pool, _ := testSchema(t, "test_start34u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0034_agent_startup_windows.sql"))
	seed := seedOwnershipChain(t, pool)
	var before, after string
	must(t, pool.QueryRow("SELECT row_to_json(r)::text FROM issue_runs r WHERE id=$1", seed.runID).Scan(&before))
	migratedTwice(t, newStoreOnSchema(t, pool))
	must(t, pool.QueryRow(`SELECT (to_jsonb(r)-'startup_ready_at'-'startup_deadline_at'-'startup_resume'-'startup_ending_deadline_at'
	 -'workspace_blocked_at'-'workspace_deadline_at'-'workspace_blocked_source')::text
	 FROM issue_runs r WHERE id=$1`, seed.runID).Scan(&after))
	var equal bool
	must(t, pool.QueryRow("SELECT $1::jsonb=$2::jsonb", before, after).Scan(&equal))
	if !equal {
		t.Fatal("startup migration rewrote existing run facts")
	}
	var uninitialized bool
	must(t, pool.QueryRow("SELECT startup_deadline_at IS NULL AND startup_ending_deadline_at IS NULL FROM issue_runs WHERE id=$1", seed.runID).Scan(&uninitialized))
	if !uninitialized {
		t.Fatal("migration guessed a legacy run deadline")
	}
}

// 0038 adds the blocked-creation deadline columns and widens the residual reason set without
// rewriting existing outcomes: a legacy run keeps its facts, its deadline columns stay NULL (no
// first-blocked evidence is invented), and a D4 residual keeps its original reason while the new
// D5 reason becomes writable.
func TestMigration0038WorkspaceBlockedDeadlineAppliesFreshAndUpgrades(t *testing.T) {
	fresh, _ := testSchema(t, "test_start35f_")
	migratedTwice(t, newStoreOnSchema(t, fresh))
	var columns int
	must(t, fresh.QueryRow(`SELECT count(*) FROM information_schema.columns
	 WHERE table_schema=current_schema() AND table_name='issue_runs'
	 AND column_name IN ('workspace_blocked_at','workspace_deadline_at','workspace_blocked_source')`).Scan(&columns))
	if columns != 3 {
		t.Fatalf("fresh schema missing the blocked-creation columns: %d", columns)
	}

	pool, _ := testSchema(t, "test_start35u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0038_agent_workspace_blocked_deadline.sql"))
	seed := seedOwnershipChain(t, pool)
	// A pre-0038 blocked creation and a pre-0038 D4 residual: the upgrade must not invent a
	// first-blocked instant for the former or rewrite the reason of the latter.
	_, e := pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash)
	 VALUES($1,$2,$3,$4,$5,'create_workspace','blocked','sandbox','{}','skel-create','skel-create-hash')`,
		uuid.NewString(), seed.tenantID, seed.userID, seed.projectID, seed.runWorkspaceID)
	must(t, e)
	_, e = pool.Exec(`INSERT INTO agent_run_residuals(run_id,tenant_id,project_id,workspace_id,runtime_generation,responsibility,reason)
	 VALUES($1,$2,$3,$4,0,'{}','agent_start_failed')`, seed.runID, seed.tenantID, seed.projectID, seed.runWorkspaceID)
	must(t, e)
	var before, after string
	must(t, pool.QueryRow("SELECT row_to_json(r)::text FROM issue_runs r WHERE id=$1", seed.runID).Scan(&before))
	migratedTwice(t, newStoreOnSchema(t, pool))
	must(t, pool.QueryRow(`SELECT (to_jsonb(r)-'workspace_blocked_at'-'workspace_deadline_at'-'workspace_blocked_source')::text
	 FROM issue_runs r WHERE id=$1`, seed.runID).Scan(&after))
	var equal bool
	must(t, pool.QueryRow("SELECT $1::jsonb=$2::jsonb", before, after).Scan(&equal))
	if !equal {
		t.Fatal("blocked-creation migration rewrote existing run facts")
	}
	var unanchored bool
	must(t, pool.QueryRow("SELECT workspace_deadline_at IS NULL AND workspace_blocked_source IS NULL FROM issue_runs WHERE id=$1", seed.runID).Scan(&unanchored))
	if !unanchored {
		t.Fatal("migration backfilled a legacy blocked creation deadline")
	}
	var kept string
	must(t, pool.QueryRow("SELECT reason FROM agent_run_residuals WHERE run_id=$1", seed.runID).Scan(&kept))
	if kept != "agent_start_failed" {
		t.Fatal("migration rewrote an existing residual reason")
	}
	// The widened constraint accepts the D5 reason alongside the D4 one, and still rejects anything else.
	if _, e := pool.Exec(`UPDATE agent_run_residuals SET reason='workspace_unavailable' WHERE run_id=$1`, seed.runID); e != nil {
		t.Fatalf("widened reason constraint rejected the D5 reason: %v", e)
	}
	_, e = pool.Exec(`UPDATE agent_run_residuals SET reason='something_else' WHERE run_id=$1`, seed.runID)
	wantPGError(t, e, "23514")
}
