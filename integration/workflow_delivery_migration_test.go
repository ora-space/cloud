package integration

import (
	"database/sql"
	"testing"
)

// The additive migration preserves old Agent history and never fabricates workflow outcomes.
func TestWorkflowDeliveryMigrationFreshAndUpgrade(t *testing.T) {
	assertShape := func(t *testing.T, pool *sql.DB) {
		t.Helper()
		for _, name := range []string{"revision_owner_exclusive", "revision_workflow_owner", "revision_workflow_workspace", "revision_history_owner"} {
			var validated bool
			must(t, pool.QueryRow("SELECT convalidated FROM pg_constraint WHERE conrelid='revisions'::regclass AND conname=$1", name).Scan(&validated))
			if !validated {
				t.Fatalf("unvalidated %s", name)
			}
		}
		var nullable string
		must(t, pool.QueryRow("SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='revisions' AND column_name='run_id'").Scan(&nullable))
		if nullable != "YES" {
			t.Fatal("workflow Revision still requires an IssueRun")
		}
	}
	fresh, _ := testSchema(t, "wf_delivery_fresh_")
	migratedTwice(t, newStoreOnSchema(t, fresh))
	assertShape(t, fresh)
	pool, _ := testSchema(t, "wf_delivery_upgrade_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, "0071_workflow_run_delivery.sql"))
	seed := seedOwnershipChain(t, pool)
	run, workspace := newBoundRun(t, pool, seed)
	row := validRevisionSeed(seed, run, workspace, newRevisionExecution(t, pool, run, "unchanged"))
	must(t, insertRevision(t, pool, row))
	workflowRun := seedInvocationWorkflowRun(t, pool, seed.tenantID)
	migratedTwice(t, newStoreOnSchema(t, pool))
	assertShape(t, pool)
	var oldRun, key, kind string
	var workflow sql.NullString
	must(t, pool.QueryRow("SELECT run_id,workflow_run_id,history_key,history_kind FROM revisions WHERE id=$1", row.id).Scan(&oldRun, &workflow, &key, &kind))
	if oldRun != run || workflow.Valid || key != row.historyKey || kind != "agent_jsonl" {
		t.Fatal("legacy Revision was reinterpreted")
	}
	var count int
	var state string
	must(t, pool.QueryRow("SELECT delivery_state,(SELECT count(*) FROM revisions WHERE workflow_run_id=$1) FROM workflow_runs WHERE id=$1", workflowRun).Scan(&state, &count))
	if state != "pending" || count != 0 {
		t.Fatalf("invented workflow delivery: state=%s revisions=%d", state, count)
	}
}
