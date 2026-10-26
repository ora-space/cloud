package integration

import "testing"

// A named observation is additive evidence; old responsibility and business facts are immutable.
func TestMigration0059RestoreStopEvidenceFreshUpgradeAndRepeat(t *testing.T) {
	fresh, _ := testSchema(t, "test_stop58f_")
	migratedTwice(t, newStoreOnSchema(t, fresh))
	if columnNullable(t, fresh, "agent_run_residuals", "stop_evidence") {
		t.Fatal("stop evidence must be an object, including when empty")
	}
	pool, _ := testSchema(t, "test_stop58u_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0059_agent_restore_stop_evidence.sql"))
	seed := seedOwnershipChain(t, pool)
	_, err := pool.Exec(`INSERT INTO agent_run_residuals(run_id,tenant_id,project_id,workspace_id,runtime_generation,responsibility,reason)
	 VALUES($1,$2,$3,$4,0,'{"original":"retained"}','agent_start_failed')`, seed.runID, seed.tenantID, seed.projectID, seed.runWorkspaceID)
	must(t, err)
	var before, after, evidence string
	must(t, pool.QueryRow("SELECT row_to_json(r)::text FROM agent_run_residuals r WHERE run_id=$1", seed.runID).Scan(&before))
	migratedTwice(t, newStoreOnSchema(t, pool))
	must(t, pool.QueryRow("SELECT (to_jsonb(r)-'stop_evidence')::text,stop_evidence::text FROM agent_run_residuals r WHERE run_id=$1", seed.runID).Scan(&after, &evidence))
	var equal bool
	must(t, pool.QueryRow("SELECT $1::jsonb=$2::jsonb", before, after).Scan(&equal))
	if !equal || evidence != "{}" {
		t.Fatalf("upgrade rewrote responsibility or invented stop evidence: %s", evidence)
	}
	_, err = pool.Exec("UPDATE agent_run_residuals SET stop_evidence='[]'::jsonb WHERE run_id=$1", seed.runID)
	wantPGError(t, err, "23514")
}
