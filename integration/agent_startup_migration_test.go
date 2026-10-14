package integration

import "testing"

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
	must(t, pool.QueryRow(`SELECT (to_jsonb(r)-'startup_ready_at'-'startup_deadline_at'-'startup_resume'-'startup_ending_deadline_at')::text
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
