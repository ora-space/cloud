package core

import (
	"reflect"
	"testing"
	"time"
)

// The real D8 worker permits a releasing Run to finish before any delete was declared.
func seedControlledD8WithoutDelete(t *testing.T, s *Store) residualReclaimScene {
	t.Helper()
	legacy := seedReleaseScene(t, s, "releasing", "completed")
	sandbox := newID()
	seedResidualExec(t, s, `INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,1,'running')`, sandbox, legacy.runws)
	// The session established control before release; no delete operation creates it in this case.
	seedResidualExec(t, s, "INSERT INTO runtime_controls(workspace_id,state) VALUES($1,'reconciling')", legacy.runws)
	seedResidualExec(t, s, `UPDATE issue_runs SET result='{"revisionId":null,"deliveryState":"failed"}' WHERE id=$1`, legacy.seed.run)
	s.DeliveryUnreachableAfter = time.Second
	if err := s.GiveUpStaleWorkspaceReleasesOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	seedControllerLease(t, s)
	return residualReclaimScene{store: s, abandonedCreation: abandonedCreation{provisioningSeed: provisioningSeed{dispSeed: legacy.seed, workspace: legacy.runws}, claims: controllerClaims(), effect: sandbox}}
}

// Explicit opt-in must be enough to recover this real D8 state without inventing a delete.
func TestControlledD8MissingDeleteRequiresExplicitScopeAndKeepsHistory(t *testing.T) {
	scene := seedControlledD8WithoutDelete(t, controlledStores(t)(t))
	r := seedControlledRequest(scene, "missing-delete")
	listed := seedControlledManage(t, scene.store, ResidualManagementRequest{Action: ResidualList, TenantID: scene.tenant, RunID: scene.run})
	items := residualObjects(listed["items"])
	if len(items) != 1 || !items[0].B("legacyDeleteOperationMissing") {
		t.Fatalf("operator cannot discover the unhandled D8 variant: %v", listed)
	}
	if _, err := scene.store.ManageResidual(t.Context(), &r); err == nil {
		t.Fatal("default termination silently broadened D8 eligibility")
	}
	r.AllowMissingDeleteOperation = true
	before := seedControlledRunSnapshot(t, scene)
	root := seedControlledManage(t, scene.store, r).O("intent")
	if root.S("source") != "legacy_d8" || !root.B("legacyDeleteOperationMissing") || root["legacyDeleteOperationId"] != nil {
		t.Fatalf("missing-delete source was not recorded honestly: %v", root)
	}
	replay := seedControlledManage(t, scene.store, r).O("intent")
	if !reflect.DeepEqual(root, replay) {
		t.Fatalf("replay changed the durable intent: %v / %v", root, replay)
	}
	if got := seedControlledRunSnapshot(t, scene); got != before {
		t.Fatalf("management rewrote the original Run: %s / %s", before, got)
	}
	var count int
	if err := scene.store.Pool.QueryRow("SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='delete_workspace'", scene.workspace).Scan(&count); err != nil || count != 0 {
		t.Fatalf("management invented a historical delete: %d / %v", count, err)
	}
}

// The new acknowledgement cannot reinterpret a live, mismatched, or already represented resource.
func TestControlledD8MissingDeleteRejectsOtherResources(t *testing.T) {
	reset := controlledStores(t)
	for _, name := range []string{"missing run", "missing workspace", "foreign tenant", "wrong run", "not D8", "still releasing", "live status", "no delivery conclusion", "admission open", "existing delete"} {
		t.Run(name, func(t *testing.T) {
			scene := seedControlledD8WithoutDelete(t, reset(t))
			r := seedControlledRequest(scene, "reject-"+name)
			r.AllowMissingDeleteOperation = true
			switch name {
			case "missing run":
				r.RunID = ""
			case "missing workspace":
				r.WorkspaceID = ""
			case "foreign tenant":
				r.TenantID = newID()
			case "wrong run":
				r.RunID = newID()
			case "not D8":
				seedResidualExec(t, scene.store, "UPDATE issue_runs SET failure_reason='' WHERE id=$1", scene.run)
			case "still releasing":
				seedResidualExec(t, scene.store, "UPDATE issue_runs SET phase='releasing' WHERE id=$1", scene.run)
			case "live status":
				seedResidualExec(t, scene.store, "UPDATE issue_runs SET status='queued' WHERE id=$1", scene.run)
			case "no delivery conclusion":
				seedResidualExec(t, scene.store, "UPDATE issue_runs SET result=NULL WHERE id=$1", scene.run)
			case "admission open":
				seedResidualExec(t, scene.store, "UPDATE workspaces SET desired_state='running',observed_state='ready',admission_open=true WHERE id=$1", scene.workspace)
			case "existing delete":
				// An old failed delete is a different variant and still requires its exact ID.
				scene = seedControlledLegacyD8(t, reset(t))
				r = seedControlledRequest(scene, "existing-delete")
				r.OperationID = ""
				r.AllowMissingDeleteOperation = true
			}
			if _, err := scene.store.ManageResidual(t.Context(), &r); err == nil {
				t.Fatalf("missing-delete acknowledgement admitted %s", name)
			}
			var count int
			if err := scene.store.Pool.QueryRow("SELECT count(*) FROM agent_run_reclamation_intents").Scan(&count); err != nil || count != 0 {
				t.Fatalf("refusal left a partial intent: %d / %v", count, err)
			}
		})
	}
}

// Termination retains the volume; only a separate intent and physical cleanup can release it.
func TestControlledD8MissingDeleteRetainsThenReleasesWithoutRewritingRun(t *testing.T) {
	scene := seedControlledD8WithoutDelete(t, controlledStores(t)(t))
	before := seedControlledRunSnapshot(t, scene)
	r := seedControlledRequest(scene, "terminate-missing-delete")
	r.AllowMissingDeleteOperation = true
	root := seedControlledManage(t, scene.store, r).O("intent")
	plan := seedControlledPlan(t, scene, root.S("id"))
	for _, target := range residualObjects(plan["effects"]) {
		if _, err := seedControlledConfirm(t, scene, root.S("id"), target.S("id"), Object{"terminated": true, "lateEnsureFenced": true}); err != nil {
			t.Fatal(err)
		}
	}
	root = seedControlledRow(t, scene.store, root.S("id"))
	if root.S("state") != "succeeded" || root.S("disposition") != "terminated_data_retained" {
		t.Fatalf("termination did not retain data: %v", root)
	}
	child, op, effect := seedControlledDeletePlan(t, scene, root)
	if !child.B("legacyDeleteOperationMissing") || child["legacyDeleteOperationId"] != nil {
		t.Fatalf("dependent deletion lost the honest missing-delete source: %v", child)
	}
	op = seedResidualAction(t, scene, op, "effect_result", Object{"state": "succeeded", "externalId": "missing-delete-cleanup", "result": Object{"removed": true}}, effect.S("id")).O("operation")
	finished := seedResidualAction(t, scene, op, "advance", nil, "").O("operation")
	if finished.S("state") != "succeeded" {
		t.Fatalf("controlled cleanup did not finish: %v", finished)
	}
	if got := seedControlledRunSnapshot(t, scene); got != before {
		t.Fatalf("cleanup rewrote the original Run: %s / %s", before, got)
	}
	var released bool
	if err := scene.store.Pool.QueryRow(`SELECT deleted_at IS NOT NULL AND issue_run_id=$2
 AND (SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='delete_workspace')=1
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals WHERE run_id=$2)
 FROM workspaces WHERE id=$1`, scene.workspace, scene.run).Scan(&released); err != nil || !released {
		t.Fatalf("cleanup failed to release only its new operation: %v / %v", released, err)
	}
}

// Forward migration must preserve old intent identities while adding a safe default.
func TestControlledD8MissingDeleteMigrationUpgradesAndRepeats(t *testing.T) {
	s := controlledStores(t)(t)
	scene := seedControlledLegacyD8(t, s)
	root := seedControlledRoot(t, scene)
	var before string
	if err := s.Pool.QueryRow("SELECT (to_jsonb(i)-'legacy_delete_operation_missing')::text FROM agent_run_reclamation_intents i WHERE id=$1", root.S("id")).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the preceding schema only inside this isolated test database.
	if _, err := s.Pool.Exec(`ALTER TABLE agent_run_reclamation_intents DROP CONSTRAINT agent_run_reclamation_legacy_delete_identity;
 ALTER TABLE agent_run_reclamation_intents DROP COLUMN legacy_delete_operation_missing;
 ALTER TABLE agent_run_reclamation_intents ADD CONSTRAINT agent_run_reclamation_intents_check1
 CHECK((source='legacy_d8' AND legacy_delete_operation_id IS NOT NULL) OR (source='residual_ledger' AND legacy_delete_operation_id IS NULL));
 DELETE FROM schema_migrations WHERE version='0070_d8_missing_delete_operation.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := s.CheckSchema(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var after string
	var missing bool
	if err := s.Pool.QueryRow("SELECT (to_jsonb(i)-'legacy_delete_operation_missing')::text,legacy_delete_operation_missing FROM agent_run_reclamation_intents i WHERE id=$1", root.S("id")).Scan(&after, &missing); err != nil || after != before || missing {
		t.Fatalf("upgrade changed the old intent or enabled missing-delete mode: %s / %s / %v / %v", before, after, missing, err)
	}
	for _, query := range []string{
		"UPDATE agent_run_reclamation_intents SET legacy_delete_operation_missing=true WHERE id=$1",
		"UPDATE agent_run_reclamation_intents SET legacy_delete_operation_id=NULL WHERE id=$1",
	} {
		if _, err := s.Pool.Exec(query, root.S("id")); err == nil {
			t.Fatal("migration allowed contradictory original delete evidence")
		}
	}
}

// Comparing the complete Run prevents an operational command from rewriting business history.
func seedControlledRunSnapshot(t *testing.T, scene residualReclaimScene) string {
	t.Helper()
	var out string
	if err := scene.store.Pool.QueryRow("SELECT row_to_json(r)::text FROM issue_runs r WHERE id=$1", scene.run).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
