package integration

import (
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

const workflowInvocationMigration = "0056_workflow_invocation_reference.sql"

// seedInvocationWorkflowRun creates real rows for the composite reference under test. The graph is
// deliberately minimal: these tests exercise database integrity rather than graph admission.
func seedInvocationWorkflowRun(t *testing.T, pool *sql.DB, tenantID string) string {
	t.Helper()
	wid, sid, rid := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, err := pool.Exec(`INSERT INTO workflows(id,tenant_id,name) VALUES($1,$2,$3)`, wid, tenantID, "Invocation "+wid)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,1,'Snapshot','{}')`, sid, tenantID, wid)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name) VALUES($1,$2,$3,$4,'Invocation')`, rid, tenantID, wid, sid)
	must(t, err)
	return rid
}

func insertInvocationIssueRun(pool *sql.DB, seed skeletonSeed, executorType string, reference any) (string, error) {
	id := uuid.NewString()
	_, err := pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,workflow_invocation_ref)
		VALUES($1,$2,$3,$4,$5,$6)`, id, seed.tenantID, seed.issueID, executorType, uuid.NewString(), reference)
	return id, err
}

// assertInvocationConstraintValidation requires the same validated schema for fresh and upgraded data.
func assertInvocationConstraintValidation(t *testing.T, pool *sql.DB) {
	t.Helper()
	for _, constraint := range []struct {
		table, name string
		validated   bool
	}{
		{"issue_runs", "issue_runs_workflow_invocation_shape", true},
		{"issue_runs", "issue_runs_workflow_invocation_fk", true},
		{"issue_runs", "issue_runs_workflow_invocation_uniq", true},
	} {
		var validated bool
		must(t, pool.QueryRow(`SELECT convalidated FROM pg_constraint WHERE conrelid=$1::regclass AND conname=$2`, constraint.table, constraint.name).Scan(&validated))
		if validated != constraint.validated {
			t.Fatalf("%s validation = %v, want %v", constraint.name, validated, constraint.validated)
		}
	}
}

func TestWorkflowInvocationMigrationFreshConstraints(t *testing.T) {
	pool, _ := testSchema(t, "test_invocation_fresh_")
	store := newStoreOnSchema(t, pool)
	migratedTwice(t, store)
	seed := seedOwnershipChain(t, pool)
	assertInvocationConstraintValidation(t, pool)

	// A valid same-tenant reference succeeds and remains a durable one-to-one link.
	ref := seedInvocationWorkflowRun(t, pool, seed.tenantID)
	id, err := insertInvocationIssueRun(pool, seed, "workflow", ref)
	must(t, err)
	var linked string
	must(t, pool.QueryRow(`SELECT workflow_invocation_ref FROM issue_runs WHERE id=$1`, id).Scan(&linked))
	if linked != ref {
		t.Fatalf("valid reference = %q, want %q", linked, ref)
	}
	_, err = insertInvocationIssueRun(pool, seed, "workflow", nil)
	wantPGError(t, err, "23514")
	_, err = insertInvocationIssueRun(pool, seed, "workflow", uuid.NewString())
	wantPGError(t, err, "23503")
	_, err = insertInvocationIssueRun(pool, seed, "workflow", ref)
	wantPGError(t, err, "23505")
	_, err = insertInvocationIssueRun(pool, seed, "agent", seedInvocationWorkflowRun(t, pool, seed.tenantID))
	wantPGError(t, err, "23514")

	// The referenced UUID exists, but belongs to another tenant. The composite FK must reject it.
	foreignTenant := uuid.NewString()
	tx, err := pool.Begin()
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT INTO tenants(id,name,status) VALUES($1,'Foreign tenant','active')`, foreignTenant)
	must(t, err)
	_, err = tx.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, foreignTenant, seed.userID)
	must(t, err)
	must(t, tx.Commit())
	foreignRef := seedInvocationWorkflowRun(t, pool, foreignTenant)
	_, err = insertInvocationIssueRun(pool, seed, "workflow", foreignRef)
	wantPGError(t, err, "23503")
}

func TestWorkflowInvocationMigrationUpgradePreservesHistory(t *testing.T) {
	pool, _ := testSchema(t, "test_invocation_upgrade_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, workflowInvocationMigration))
	seed := seedOwnershipChain(t, pool)
	ids := map[string]string{}
	for _, status := range []string{"queued", "dispatched", "running", "deferred", "completed", "failed", "cancelled"} {
		id := uuid.NewString()
		ids[status] = id
		_, err := pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,input,failure_reason)
			VALUES($1,$2,$3,'workflow',$4,$5,'{"topic":"historical"}','existing-history')`, id, seed.tenantID, seed.issueID, uuid.NewString(), status)
		must(t, err)
	}
	rowJSON := func(id string) string {
		t.Helper()
		var out string
		must(t, pool.QueryRow(`SELECT row_to_json(ir)::text FROM issue_runs ir WHERE id=$1`, id).Scan(&out))
		return out
	}
	terminalBefore := map[string]string{}
	for _, status := range []string{"completed", "failed", "cancelled"} {
		terminalBefore[status] = rowJSON(ids[status])
	}
	agentBefore := rowJSON(seed.runID)
	store := newStoreOnSchema(t, pool)
	migratedTwice(t, store)
	assertInvocationConstraintValidation(t, pool)

	for _, before := range []string{"queued", "dispatched", "running", "deferred"} {
		var status, reason, topic string
		var reference sql.NullString
		var completed bool
		var version int
		must(t, pool.QueryRow(`SELECT status,failure_reason,input->>'topic',workflow_invocation_ref,completed_at IS NOT NULL,version
			FROM issue_runs WHERE id=$1`, ids[before]).Scan(&status, &reason, &topic, &reference, &completed, &version))
		if status != "failed" || reason != "workflow_execution_unavailable_before_invocation_linkage" || topic != "historical" || reference.Valid || !completed || version != 2 {
			t.Fatalf("historical %s settlement: status=%q reason=%q topic=%q ref=%v completed=%v version=%d", before, status, reason, topic, reference, completed, version)
		}
	}
	for status, before := range terminalBefore {
		if got := rowJSON(ids[status]); got != before {
			t.Fatalf("terminal %s workflow history changed: before=%s after=%s", status, before, got)
		}
	}
	if got := rowJSON(seed.runID); got != agentBefore {
		t.Fatalf("ordinary agent row changed: before=%s after=%s", agentBefore, got)
	}
	var issues, workflowRuns int
	must(t, pool.QueryRow(`SELECT count(*) FROM issue_runs`).Scan(&issues))
	must(t, pool.QueryRow(`SELECT count(*) FROM workflow_runs`).Scan(&workflowRuns))
	if issues != 8 || workflowRuns != 0 {
		t.Fatalf("upgrade lost history or invented execution: IssueRuns=%d WorkflowRuns=%d, want 8/0", issues, workflowRuns)
	}

	// A queued invocation must link execution; historical terminal rows remain editable.
	_, err := insertInvocationIssueRun(pool, seed, "workflow", nil)
	wantPGError(t, err, "23514")
	for _, id := range ids {
		_, err = pool.Exec(`UPDATE issue_runs SET trigger_summary='retained history' WHERE id=$1`, id)
		must(t, err)
		var summary string
		must(t, pool.QueryRow(`SELECT trigger_summary FROM issue_runs WHERE id=$1`, id).Scan(&summary))
		if summary != "retained history" {
			t.Fatalf("historical row %s was not editable", id)
		}
	}
	ref := seedInvocationWorkflowRun(t, pool, seed.tenantID)
	validID, err := insertInvocationIssueRun(pool, seed, "workflow", ref)
	must(t, err)
	_, err = pool.Exec(`UPDATE issue_runs SET workflow_invocation_ref=NULL WHERE id=$1`, validID)
	wantPGError(t, err, "23514")
}

func TestWorkflowInvocationMigrationUpgradeWithoutLegacyValidates(t *testing.T) {
	pool, _ := testSchema(t, "test_invocation_clean_upgrade_")
	applyMigrationsUpTo(t, pool, migrationsBefore(t, workflowInvocationMigration))
	seedOwnershipChain(t, pool)
	migratedTwice(t, newStoreOnSchema(t, pool))
	assertInvocationConstraintValidation(t, pool)
}
