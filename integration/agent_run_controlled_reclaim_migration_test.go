package integration

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

const controlledReclaimMigration = "0058_agent_run_controlled_reclamation.sql"

func insertControlledMigrationRoot(t *testing.T, pool *sql.DB, seed skeletonSeed, id, reclaim string) {
	t.Helper()
	_, err := pool.Exec(`INSERT INTO agent_run_reclamation_intents
 (id,command_kind,source,tenant_id,project_id,run_id,workspace_id,runtime_generation,reclaim_id,
 control_epoch,actor_subject,reason,impact_confirmed)
 VALUES($1,'terminate','residual_ledger',$2,$3,$4,$5,27,$6,4,'migration-operator','Retain original data',true)`,
		id, seed.tenantID, seed.projectID, seed.runID, seed.runWorkspaceID, reclaim)
	must(t, err)
}

func TestControlledResidualMigrationFreshAndConstraints(t *testing.T) {
	pool, _ := testSchema(t, "test_controlled_fresh_")
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	seed := seedAgentIssueRunSkeleton(t, pool)
	root, reclaim := uuid.NewString(), uuid.NewString()
	insertControlledMigrationRoot(t, pool, seed, root, reclaim)
	for _, table := range []string{"agent_run_reclamation_intents", "agent_run_reclamation_targets", "agent_run_reclamation_command_receipts"} {
		if !tableExists(t, pool, table) {
			t.Fatalf("fresh migration omitted %s", table)
		}
	}
	for _, tc := range []struct{ name, change string }{
		{"unknown state", "state='unknown'"},
		{"unknown source", "source='unknown'"},
		{"missing D8 source operation", "source='legacy_d8'"},
		{"unknown command", "command_kind='unknown'"},
		{"root without confirmation", "impact_confirmed=false"},
		{"root without responsibility", "reclaim_id=NULL"},
		{"empty reason", "reason=' '"},
		{"targets object", "targets='{}'"},
		{"evidence object", "evidence='{}'"},
		{"audit object", "audit='{}'"},
		{"negative generation", "runtime_generation=-1"},
		{"zero version", "version=0"},
		{"completion without evidence", "state='succeeded'"},
		{"unfinished with disposition", "disposition='released'"},
		{"unfinished with time", "completed_at=clock_timestamp()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec("UPDATE agent_run_reclamation_intents SET "+tc.change+" WHERE id=$1", root)
			wantPGError(t, err, "23514")
		})
	}
	_, err := pool.Exec(`UPDATE agent_run_reclamation_intents SET delete_operation_id=$2 WHERE id=$1`, root, uuid.NewString())
	wantPGError(t, err, "23503")
	other := seed
	other.runID, other.runWorkspaceID = newBoundRun(t, pool, seed)
	// One Workspace and one reclaim identity each have exactly one root owner.
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_intents
 (id,command_kind,source,tenant_id,project_id,run_id,workspace_id,runtime_generation,reclaim_id,control_epoch,actor_subject,reason,impact_confirmed)
 VALUES($1,'terminate','residual_ledger',$2,$3,$4,$5,27,$6,4,'operator','Duplicate root',true)`,
		uuid.NewString(), seed.tenantID, seed.projectID, seed.runID, seed.runWorkspaceID, uuid.NewString())
	wantPGError(t, err, "23505")
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_intents
 (id,command_kind,source,tenant_id,project_id,run_id,workspace_id,runtime_generation,reclaim_id,control_epoch,actor_subject,reason,impact_confirmed)
 VALUES($1,'terminate','residual_ledger',$2,$3,$4,$5,27,$6,4,'operator','Duplicate responsibility',true)`,
		uuid.NewString(), other.tenantID, other.projectID, other.runID, other.runWorkspaceID, reclaim)
	wantPGError(t, err, "23505")
	child := uuid.NewString()
	insertChild := `INSERT INTO agent_run_reclamation_intents
 (id,root_intent_id,command_kind,source,tenant_id,project_id,run_id,workspace_id,runtime_generation,control_epoch,actor_subject,reason,data_delete_confirmed)
 VALUES($1,$2,'delete_data','residual_ledger',$3,$4,$5,$6,27,4,'operator','Separately confirmed deletion',true)`
	_, err = pool.Exec(insertChild, uuid.NewString(), root, other.tenantID, other.projectID, other.runID, other.runWorkspaceID)
	wantPGError(t, err, "23503")
	_, err = pool.Exec(insertChild, child, root, seed.tenantID, seed.projectID, seed.runID, seed.runWorkspaceID)
	must(t, err)
	_, err = pool.Exec(insertChild, uuid.NewString(), root, seed.tenantID, seed.projectID, seed.runID, seed.runWorkspaceID)
	wantPGError(t, err, "23505")
	// An ensure plan is a durable target even if no sandbox_instances row was ever created.
	target, sandbox := uuid.NewString(), uuid.NewString()
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_targets(id,root_intent_id,sandbox_id,generation,request)
 VALUES($1,$2,$3,27,'{"kind":"sandbox_terminate"}')`, target, root, sandbox)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_targets(id,root_intent_id,sandbox_id,generation,request)
 VALUES($1,$2,$3,27,'{"kind":"sandbox_terminate"}')`, uuid.NewString(), root, sandbox)
	wantPGError(t, err, "23505")
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_targets(id,root_intent_id,sandbox_id,generation,request)
 VALUES($1,$2,$3,27,'{}')`, uuid.NewString(), uuid.NewString(), uuid.NewString())
	wantPGError(t, err, "23503")
	_, err = pool.Exec(`UPDATE agent_run_reclamation_targets SET state='succeeded',confirmed_at=clock_timestamp(),confirmed_epoch=1,result='{"terminated":true}' WHERE id=$1`, target)
	wantPGError(t, err, "23514")
	_, err = pool.Exec(`UPDATE agent_run_reclamation_targets SET state='succeeded',confirmed_at=clock_timestamp(),confirmed_epoch=1,result='{"terminated":true,"lateEnsureFenced":true}' WHERE id=$1`, target)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_command_receipts(id,tenant_id,idempotency_key,command_input,intent_id,actor_subject)
 VALUES($1,$2,'one-shared-key','{"action":"terminate"}',$3,'operator')`, uuid.NewString(), seed.tenantID, root)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_command_receipts(id,tenant_id,idempotency_key,command_input,intent_id,actor_subject)
 VALUES($1,$2,'one-shared-key','{"action":"delete-data"}',$3,'operator')`, uuid.NewString(), seed.tenantID, child)
	wantPGError(t, err, "23505")
	_, err = pool.Exec(`INSERT INTO agent_run_reclamation_command_receipts(id,tenant_id,idempotency_key,command_input,intent_id,actor_subject)
 VALUES($1,$2,'missing-intent','{}',$3,'operator')`, uuid.NewString(), seed.tenantID, uuid.NewString())
	wantPGError(t, err, "23503")
}

func TestControlledResidualMigrationUpgradePreserves0057HistoryAndDoesNotInventD8Ledger(t *testing.T) {
	pool, _ := testSchema(t, "test_controlled_upgrade_")
	// Include the complete mainline schema through 0055 and the 0057 residual ledger before upgrading.
	applyMigrationsUpTo(t, pool, migrationsBefore(t, controlledReclaimMigration))
	base := seedAgentIssueRunSkeleton(t, pool)
	for _, state := range []string{"reconciling", "reclaiming", "cleared"} {
		seed := base
		if state != "reconciling" {
			seed.runID, seed.runWorkspaceID = newBoundRun(t, pool, base)
		}
		seedResidualMigrationRow(t, pool, seed, map[string]string{"reconciling": "agent_start_failed", "reclaiming": "workspace_unavailable", "cleared": "agent_start_failed"}[state])
		if state != "reconciling" {
			_, err := pool.Exec(`UPDATE agent_run_residuals SET reclaim_state=$2,reclaim_id=$3,reclaim_mode='controlled',
 reclaim_authorization='{"kind":"legacy-controlled","operator":"original"}',reclaim_targets='[{"kind":"sandbox","id":"original"}]',
 reclaim_evidence='[{"key":"original-fact","fact":{"state":"succeeded"}}]',reclaim_version=9,
 reclaim_checked_at='2026-01-02T04:05:06Z',reclaim_conflicted_at='2026-01-02T04:06:07Z',
 cleared_at=CASE WHEN $2='cleared' THEN '2026-01-03T03:04:05Z'::timestamptz END,
 disposition=CASE WHEN $2='cleared' THEN 'terminated_data_retained' END WHERE run_id=$1`, seed.runID, state, uuid.NewString())
			must(t, err)
		}
	}
	d8 := base
	d8.runID, d8.runWorkspaceID = newBoundRun(t, pool, base)
	_, err := pool.Exec(`UPDATE issue_runs SET phase='done',status='completed',completed_at='2026-01-04T03:04:05Z',
 failure_reason='workspace_unavailable',result='{"deliveryState":"delivered"}' WHERE id=$1`, d8.runID)
	must(t, err)
	_, err = pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,error_code)
 VALUES($1,$2,$3,$4,$5,'delete_workspace','failed','quiesce','{"original":"D8"}','historical-d8','fixture-request','node_unavailable')`,
		uuid.NewString(), d8.tenantID, d8.userID, d8.projectID, d8.runWorkspaceID)
	must(t, err)
	const snapshot = `SELECT jsonb_build_object(
 'residuals',(SELECT jsonb_agg(to_jsonb(r)-'stop_evidence' ORDER BY run_id) FROM agent_run_residuals r),
 'runs',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM issue_runs r),
 'workspaces',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM workspaces w),
 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM operations o))::text`
	// Migrate applies every later migration too; stop_evidence is 0059's additive column, not a rewrite of history.
	var before, after string
	must(t, pool.QueryRow(snapshot).Scan(&before))
	store := newStoreOnSchema(t, pool)
	must(t, store.Migrate(context.Background()))
	must(t, store.Migrate(context.Background()))
	must(t, pool.QueryRow(snapshot).Scan(&after))
	if before != after {
		t.Fatal("controlled migration changed 0057 responsibility/state/clear time or historical business rows")
	}
	var empty bool
	must(t, pool.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM agent_run_reclamation_intents)
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_targets)
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_command_receipts)
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals WHERE run_id=$1)`, d8.runID).Scan(&empty))
	if !empty {
		t.Fatal("migration invented a controlled command, effect, receipt or D8 residual ledger")
	}
}
