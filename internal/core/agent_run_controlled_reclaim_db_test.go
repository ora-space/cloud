package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func seedControlledRequest(scene residualReclaimScene, key string) ResidualManagementRequest {
	r := ResidualManagementRequest{Action: ResidualTerminate, TenantID: scene.tenant, ProjectID: scene.project, RunID: scene.run, WorkspaceID: scene.workspace, Key: key, Reason: "deployment operator accepted residual impact", OperatorLabel: "test operator", ImpactConfirmed: true}
	if scene.responsibility == "" {
		r.OperationID = scene.operation
	}
	return r
}

func seedControlledManage(t *testing.T, s *Store, r ResidualManagementRequest) Object {
	t.Helper()
	out, err := s.ManageResidual(t.Context(), &r)
	if err != nil {
		t.Fatalf("manage residual %s: %v", r.Action, err)
	}
	return out
}

func seedControlledRow(t *testing.T, s *Store, id string) Object {
	t.Helper()
	out, err := s.transact(t.Context(), func(tx *transaction) Object {
		return tx.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func seedControlledRoot(t *testing.T, scene residualReclaimScene) Object {
	t.Helper()
	out := seedControlledManage(t, scene.store, seedControlledRequest(scene, "terminate-"+newID()))
	intent := out.O("intent")
	if intent.S("id") == "" || intent.S("commandKind") != "terminate" || intent.S("state") != "registered" {
		t.Fatalf("terminate did not register an explicit root: %v", out)
	}
	return intent
}

// List and confirm through the same Controller API as the deployed force-stop transport. The
// test never writes succeeded/cleared or target proof directly to manufacture completion.
func seedControlledPlan(t *testing.T, scene residualReclaimScene, id string) Object {
	t.Helper()
	out, err := scene.store.Control(t.Context(), &ControlRequest{Action: "force_pending", Body: Object{"epoch": 1}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range residualObjects(out["plans"]) {
		if plan.O("forceStop").S("id") == id {
			return plan
		}
	}
	t.Fatalf("controlled root %s missing from force_pending: %v", id, out)
	return nil
}

func seedControlledConfirm(t *testing.T, scene residualReclaimScene, id, effect string, proof Object) (Object, error) {
	t.Helper()
	intent := seedControlledRow(t, scene.store, id)
	body := Object{"epoch": 1, "forceStopId": id, "version": intent.N("version"), "effectId": effect}
	for key, value := range proof {
		body[key] = value
	}
	return scene.store.Control(t.Context(), &ControlRequest{Action: "force_confirm", Body: body, Service: scene.claims})
}

func seedControlledRetained(t *testing.T, scene residualReclaimScene) Object {
	t.Helper()
	root := seedControlledRoot(t, scene)
	plan := seedControlledPlan(t, scene, root.S("id"))
	for _, target := range residualObjects(plan["effects"]) {
		if _, err := seedControlledConfirm(t, scene, root.S("id"), target.S("id"), Object{"terminated": true, "lateEnsureFenced": true}); err != nil {
			t.Fatal(err)
		}
	}
	if len(residualObjects(plan["effects"])) == 0 {
		if _, err := seedControlledConfirm(t, scene, root.S("id"), "", nil); err != nil {
			t.Fatal(err)
		}
	}
	root = seedControlledRow(t, scene.store, root.S("id"))
	if root.S("state") != "succeeded" || root.S("disposition") != "terminated_data_retained" {
		t.Fatalf("physical dual confirmations did not retain data: %v", root)
	}
	return root
}

func seedControlledDeleteRequest(scene residualReclaimScene, root Object, key string) ResidualManagementRequest {
	return ResidualManagementRequest{Action: ResidualDeleteData, TenantID: scene.tenant, IntentID: root.S("id"), Key: key, Reason: "independent approval to delete retained workspace data", DataDeleteConfirmed: true}
}

func seedControlledDeletePlan(t *testing.T, scene residualReclaimScene, root Object) (Object, Object, Object) {
	t.Helper()
	out := seedControlledManage(t, scene.store, seedControlledDeleteRequest(scene, root, "delete-"+newID()))
	child := out.O("intent")
	if child.S("id") == root.S("id") || child.S("rootIntentId") != root.S("id") || child.S("commandKind") != "delete_data" {
		t.Fatalf("data deletion did not create a separate child intent: %v", out)
	}
	claimed, err := scene.store.Control(t.Context(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	op := claimed.O("operation")
	if op.S("id") != out.O("operation").S("id") || op.S("step") != "cleanup" {
		t.Fatalf("controlled delete did not start its exact operation at cleanup: %v / %v", out, claimed)
	}
	planned := seedResidualAction(t, scene, op, "plan", Object{"kind": "workspace_data_delete", "workspaceId": scene.workspace}, "")
	return child, planned.O("operation"), planned.O("effect")
}

func seedControlledExternalRequest(scene residualReclaimScene, intent Object, key string) ResidualManagementRequest {
	facts := map[string]string{"data:" + scene.workspace: "incident-251/data-responsibility-transferred"}
	for _, target := range residualObjects(intent["targets"]) {
		if target.S("kind") == "sandbox" {
			facts["sandbox:"+target.S("id")] = "incident-251/host-disposal/" + target.S("id")
		}
	}
	if scene.effect != "" && intent.S("commandKind") == "terminate" {
		facts["sandbox:"+scene.effect] = "incident-251/host-disposal/" + scene.effect
	}
	return ResidualManagementRequest{Action: ResidualConfirmExternal, TenantID: scene.tenant, IntentID: intent.S("id"), Key: key, Reason: "operator accepts unresolved physical responsibility", EvidenceNote: "incident-251 records each disposal responsibility", ResponsibilityTransferred: true, ExternalFacts: facts}
}

func assertControlledRejected(t *testing.T, err error) {
	t.Helper()
	var fault *Fault
	if !errors.As(err, &fault) || fault.Status < 400 || fault.Status >= 500 {
		t.Fatalf("expected a stable command rejection, got %v", err)
	}
}

func TestControlledResidualCommands(t *testing.T) {
	// Every invalid command rolls back; the assertions below keep this same pristine scene.
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	for _, invalid := range []string{"impact", "reason", "tenant", "workspace", "key"} {
		t.Run(invalid, func(t *testing.T) {
			r := seedControlledRequest(scene, "invalid-command")
			switch invalid {
			case "impact":
				r.ImpactConfirmed = false
			case "reason":
				r.Reason = "  "
			case "tenant":
				r.TenantID = newID()
			case "workspace":
				r.WorkspaceID = newID()
			case "key":
				r.Key = ""
			}
			_, err := scene.store.ManageResidual(t.Context(), &r)
			assertControlledRejected(t, err)
			seedResidualAssert(t, scene, `SELECT reclaim_id IS NULL AND reclaim_state='reconciling'
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_intents WHERE workspace_id=$2)
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_targets x JOIN agent_run_reclamation_intents i ON i.id=x.root_intent_id WHERE i.workspace_id=$2)
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, scene.workspace)
		})
	}
	r := seedControlledRequest(scene, "one-command")
	first := seedControlledManage(t, scene.store, r).O("intent")
	replay := seedControlledManage(t, seedResidualRebuildStore(scene.store), r).O("intent")
	if replay.S("id") != first.S("id") {
		t.Fatal("same command replay substituted a new intent")
	}
	r.Reason = "different authorized input"
	_, err := scene.store.ManageResidual(t.Context(), &r)
	assertControlledRejected(t, err)
	var actor string
	if err := scene.store.Pool.QueryRow("SELECT session_user::text").Scan(&actor); err != nil {
		t.Fatal(err)
	}
	seedResidualAssert(t, scene, `SELECT i.actor_subject=$2 AND i.operator_label='test operator' AND i.reason=$3 AND i.impact_confirmed
 AND r.reclaim_mode='controlled' AND r.reclaim_authorization->>'intentId'=i.id::text
 AND r.responsibility=$4::jsonb AND NOT w.admission_open AND w.deleted_at IS NULL
 AND (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=w.id AND command_kind='terminate')=1
 AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')
 FROM agent_run_residuals r JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id JOIN workspaces w ON w.id=r.workspace_id WHERE r.run_id=$1`, scene.run, actor, first.S("reason"), scene.responsibility)
}

func TestControlledResidualEveryPlannedSandbox(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	// A historical ensure can have no sandbox row. Its stable original identity is still a
	// termination target; the NULL workspace scope is resolved via its original operation.
	neverEnsured := newID()
	seedResidualExec(t, scene.store, `INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch)
 VALUES($1,$2,$3,NULL,'sandbox_ensure','planned',$4,1)`, neverEnsured, scene.operation, scene.project, jsonText(Object{"kind": "sandbox_ensure", "workspaceId": scene.workspace}))
	root := seedControlledRoot(t, scene)
	plan := seedControlledPlan(t, scene, root.S("id"))
	targets := residualObjects(plan["effects"])
	seen := map[string]bool{}
	for _, target := range targets {
		sid := target.O("request").S("sandboxInstanceId")
		seen[sid] = true
		permit, err := scene.store.Control(t.Context(), &ControlRequest{Action: "effect_permit", EffectID: target.S("id"), Body: Object{"epoch": 1, "forceStopId": root.S("id")}, Service: scene.claims})
		if err != nil || permit.S("sandboxId") != sid || permit.S("workspaceId") != scene.workspace || permit.N("runtimeGeneration") != 1 || permit.N("expiresAtMs") <= permit.N("issuedAtMs") {
			t.Fatalf("target permit did not bind stable identity: %v / %v", permit, err)
		}
		for _, proof := range []Object{{"terminated": true}, {"lateEnsureFenced": true}, {"terminated": false, "lateEnsureFenced": true}} {
			_, err := seedControlledConfirm(t, scene, root.S("id"), target.S("id"), proof)
			assertControlledRejected(t, err)
		}
	}
	if !seen[scene.effect] || !seen[neverEnsured] || len(targets) != 2 {
		t.Fatalf("planned sandbox union omitted an allocating or never ensured target: %v", plan)
	}
	if _, err := seedControlledConfirm(t, scene, root.S("id"), targets[0].S("id"), Object{"terminated": true, "lateEnsureFenced": true}); err != nil {
		t.Fatal(err)
	}
	seedResidualAssert(t, scene, `SELECT r.cleared_at IS NULL AND i.state<>'succeeded' AND w.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=w.id AND kind='workspace_data_delete')
 FROM agent_run_residuals r JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id JOIN workspaces w ON w.id=r.workspace_id WHERE r.run_id=$1`, scene.run)
	if _, err := seedControlledConfirm(t, scene, root.S("id"), targets[1].S("id"), Object{"terminated": true, "lateEnsureFenced": true}); err != nil {
		t.Fatal(err)
	}
	seedResidualAssert(t, scene, `SELECT r.disposition='terminated_data_retained' AND r.cleared_at IS NOT NULL
 AND (SELECT count(*) FROM agent_run_reclamation_targets WHERE root_intent_id=i.id AND state='succeeded' AND result @> '{"terminated":true,"lateEnsureFenced":true}')=2
 AND NOT EXISTS(SELECT 1 FROM runtime_force_stops WHERE workspace_id=r.workspace_id)
 FROM agent_run_residuals r JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id WHERE r.run_id=$1`, scene.run)
}

func TestControlledResidualRetainedThenReleased(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	root := seedControlledRetained(t, scene)
	var clearedAt string
	if err := scene.store.Pool.QueryRow(`SELECT cleared_at::text FROM agent_run_residuals WHERE run_id=$1`, scene.run).Scan(&clearedAt); err != nil {
		t.Fatal(err)
	}
	r := seedControlledDeleteRequest(scene, root, "explicit-delete")
	r.DataDeleteConfirmed = false
	_, err := scene.store.ManageResidual(t.Context(), &r)
	assertControlledRejected(t, err)
	child, op, effect := seedControlledDeletePlan(t, scene, root)
	seedResidualAssert(t, scene, `SELECT r.disposition='terminated_data_retained' AND r.cleared_at::text=$2 AND w.deleted_at IS NULL
 AND i.state='succeeded' AND i.completed_at IS NOT NULL AND c.state<>'succeeded' AND c.root_intent_id=i.id
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id
 JOIN agent_run_reclamation_intents c ON c.id=$3 WHERE r.run_id=$1`, scene.run, clearedAt, child.S("id"))
	op = seedResidualAction(t, scene, op, "effect_result", Object{"state": "succeeded", "externalId": "controlled-data-delete", "result": Object{"removed": true}}, effect.S("id")).O("operation")
	finished := seedResidualAction(t, scene, op, "advance", nil, "").O("operation")
	if finished.S("state") != "succeeded" {
		t.Fatalf("physical data result did not finish the bound operation: %v", finished)
	}
	seedResidualAssert(t, scene, `SELECT r.disposition='released' AND r.cleared_at::text=$2 AND r.responsibility=$3::jsonb
 AND i.state='succeeded' AND i.disposition='terminated_data_retained' AND i.completed_at=$4::timestamptz
 AND c.state='succeeded' AND c.disposition='released' AND c.delete_operation_id=$5 AND w.deleted_at IS NOT NULL
 AND run.phase='done' AND run.status='failed' AND run.failure_reason='workspace_unavailable'
 AND original.state='failed'
 AND (SELECT count(*) FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')=1
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN issue_runs run ON run.id=r.run_id
 JOIN operations original ON original.id=$6 JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id
 JOIN agent_run_reclamation_intents c ON c.id=$7 WHERE r.run_id=$1`, scene.run, clearedAt, scene.responsibility, root.S("completedAt"), finished.S("id"), scene.operation, child.S("id"))
}

func TestControlledResidualExternalRequiresUnfinishedIntent(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	initialRoot := seedControlledRoot(t, scene)
	for _, invalid := range []string{"no_intent", "reason", "note", "transfer", "missing_sandbox", "missing_data", "succeeded"} {
		t.Run(invalid, func(t *testing.T) {
			// Rejections leave the unfinished intent intact; the final case completes it first.
			root := initialRoot
			if invalid == "succeeded" {
				root = seedControlledRetained(t, scene)
			}
			r := seedControlledExternalRequest(scene, root, "external-invalid")
			switch invalid {
			case "no_intent":
				r.IntentID = newID()
			case "reason":
				r.Reason = " "
			case "note":
				r.EvidenceNote = " "
			case "transfer":
				r.ResponsibilityTransferred = false
			case "missing_sandbox":
				delete(r.ExternalFacts, "sandbox:"+scene.effect)
			case "missing_data":
				delete(r.ExternalFacts, "data:"+scene.workspace)
			}
			_, err := scene.store.ManageResidual(t.Context(), &r)
			assertControlledRejected(t, err)
			seedResidualAssert(t, scene, `SELECT w.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')
 AND ($3 OR NOT EXISTS(SELECT 1 FROM agent_run_reclamation_targets WHERE root_intent_id=$2 AND result @> '{"terminated":true}'))
 FROM workspaces w JOIN issue_runs run ON run.workspace_id=w.id WHERE run.id=$1`, scene.run, root.S("id"), invalid == "succeeded")
		})
	}
}

func TestControlledResidualExternalDisposition(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	root := seedControlledRoot(t, scene)
	r := seedControlledExternalRequest(scene, root, "external-close")
	out := seedControlledManage(t, scene.store, r)
	if out.O("intent").S("disposition") != "externally_confirmed" {
		t.Fatalf("external materials did not produce distinct disposition: %v", out)
	}
	if replay := seedControlledManage(t, scene.store, r); replay.O("intent").S("id") != root.S("id") {
		t.Fatal("external command replay changed intent identity")
	}
	seedResidualAssert(t, scene, `SELECT r.reclaim_state='cleared' AND r.disposition='externally_confirmed' AND r.cleared_at IS NOT NULL
 AND r.responsibility=$2::jsonb AND w.deleted_at IS NULL AND NOT w.admission_open AND sb.terminated_at IS NULL
 AND i.state='succeeded' AND i.disposition='externally_confirmed'
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_targets WHERE root_intent_id=i.id AND state='succeeded')
 AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN sandbox_instances sb ON sb.id=$3
 JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id WHERE r.run_id=$1`, scene.run, scene.responsibility, scene.effect)
	deleteRequest := seedControlledDeleteRequest(scene, root, "external-cannot-authorize-delete")
	_, err := scene.store.ManageResidual(t.Context(), &deleteRequest)
	assertControlledRejected(t, err)
}

func TestControlledResidualGuardsAndRollback(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	r := seedControlledRequest(scene, "rolled-back-command")
	_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
		manageResidualCommand(tx, &r)
		reject(409, "controlled_fixture_rollback")
		return nil
	})
	assertControlledRejected(t, err)
	seedResidualAssert(t, scene, `SELECT reclaim_id IS NULL AND reclaim_state='reconciling'
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_intents WHERE workspace_id=$2)
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_command_receipts WHERE tenant_id=$3)
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, scene.workspace, scene.tenant)
	root := seedControlledRetained(t, scene)
	for _, stage := range []string{"retained", "delete_pending"} {
		if stage == "delete_pending" {
			seedControlledManage(t, scene.store, seedControlledDeleteRequest(scene, root, "guard-delete"))
		}
		if _, err := scene.store.DeleteRunWorkspace(t.Context(), scene.run); err == nil {
			t.Fatalf("%s ordinary deletion bypassed controlled responsibility", stage)
		}
		for _, projectWide := range []bool{false, true} {
			_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
				if projectWide {
					for _, w := range tx.list("SELECT * FROM workspaces WHERE project_id=$1", scene.project) {
						checkActivities(tx, w)
					}
				} else {
					checkActivities(tx, tx.one("SELECT * FROM workspaces WHERE id=$1", scene.workspace))
				}
				return nil
			})
			if err == nil {
				t.Fatalf("%s project=%v activity guard ignored controlled data protection", stage, projectWide)
			}
		}
	}
}

func TestControlledResidualQueryAndResume(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	root := seedControlledRoot(t, scene)
	restarted := seedResidualRebuildStore(scene.store)
	if err := restarted.ReconcileAgentRunResidualsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := seedControlledManage(t, restarted, ResidualManagementRequest{Action: ResidualGet, TenantID: scene.tenant, IntentID: root.S("id")})
	if out.O("intent").S("id") != root.S("id") {
		t.Fatalf("get lost durable root identity: %v", out)
	}
	list := seedControlledManage(t, restarted, ResidualManagementRequest{Action: ResidualList, TenantID: scene.tenant, Limit: 1})
	items := residualObjects(list["items"])
	if len(items) != 1 {
		t.Fatalf("list did not return bounded durable management state: %v", list)
	}
	_, err := restarted.ManageResidual(t.Context(), &ResidualManagementRequest{Action: ResidualGet, TenantID: newID(), IntentID: root.S("id")})
	assertControlledRejected(t, err)
	seedResidualAssert(t, scene, `SELECT i.state<>'succeeded' AND i.id=$2 AND r.reclaim_mode='controlled' AND r.cleared_at IS NULL
 AND (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=r.workspace_id)=1
 AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=r.workspace_id AND kind='delete_workspace')
 FROM agent_run_residuals r JOIN agent_run_reclamation_intents i ON i.reclaim_id=r.reclaim_id WHERE r.run_id=$1`, scene.run, root.S("id"))
}

// This is a genuine D8 terminal: production declares the original delete and the give-up pass
// fails it. Only the predecessor scene (a releasing run with an unreachable sandbox) is staged.
func seedControlledLegacyD8(t *testing.T, prepared ...*Store) residualReclaimScene {
	t.Helper()
	var s *Store
	if len(prepared) == 0 {
		s = controlledStores(t)(t)
	} else {
		s = prepared[0]
	}
	legacy := seedReleaseScene(t, s, "releasing", "failed")
	sandbox := newID()
	seedResidualExec(t, s, `INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,1,'running')`, sandbox, legacy.runws)
	if _, err := s.DeleteRunWorkspace(t.Context(), legacy.seed.run); err != nil {
		t.Fatal(err)
	}
	s.DeliveryUnreachableAfter = time.Second
	if err := s.GiveUpStaleWorkspaceReleasesOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	seedControllerLease(t, s)
	var operation string
	if err := s.Pool.QueryRow(`SELECT id::text FROM operations WHERE workspace_id=$1 AND kind='delete_workspace'`, legacy.runws).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	return residualReclaimScene{store: s, abandonedCreation: abandonedCreation{provisioningSeed: provisioningSeed{dispSeed: legacy.seed, workspace: legacy.runws, operation: operation}, claims: controllerClaims(), effect: sandbox}}
}

func TestControlledResidualLegacyD8(t *testing.T) {
	scene := seedControlledLegacyD8(t)
	root := seedControlledRoot(t, scene)
	if root.S("source") != "legacy_d8" || root.S("legacyDeleteOperationId") != scene.operation {
		t.Fatalf("D8 root did not preserve its original delete binding: %v", root)
	}
	if err := scene.store.RedeclareRunWorkspaceDeletesOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := scene.store.DeleteRunWorkspace(t.Context(), scene.run); err == nil {
		t.Fatal("D8 root without a ledger failed to protect ordinary deletion")
	}
	seedControlledManage(t, scene.store, seedControlledExternalRequest(scene, root, "external-d8"))
	var valid bool
	if err := scene.store.Pool.QueryRow(`SELECT run.phase='done' AND run.status='failed' AND run.failure_reason='workspace_unavailable'
 AND op.state='failed' AND op.error_code='node_unavailable' AND w.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals WHERE run_id=run.id)
 AND (SELECT count(*) FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')=1
 FROM issue_runs run JOIN workspaces w ON w.id=run.workspace_id JOIN operations op ON op.id=$2 WHERE run.id=$1`, scene.run, scene.operation).Scan(&valid); err != nil || !valid {
		t.Fatalf("D8 management rewrote history or invented a ledger: %v / %v", valid, err)
	}
}

func TestControlledResidualLegacyD8RetainedThenReleased(t *testing.T) {
	scene := seedControlledLegacyD8(t)
	var originalRun, originalOperation string
	if err := scene.store.Pool.QueryRow(`SELECT to_jsonb(run)::text,to_jsonb(op)::text
 FROM issue_runs run JOIN operations op ON op.id=$2 WHERE run.id=$1`, scene.run, scene.operation).Scan(&originalRun, &originalOperation); err != nil {
		t.Fatal(err)
	}
	root := seedControlledRetained(t, scene)
	var retained bool
	if err := scene.store.Pool.QueryRow(`SELECT i.source='legacy_d8' AND i.legacy_delete_operation_id=$2
 AND i.state='succeeded' AND i.disposition='terminated_data_retained' AND i.completed_at IS NOT NULL
 AND w.deleted_at IS NULL AND NOT w.admission_open
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals WHERE run_id=i.run_id)
 AND (SELECT count(*) FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')=1
 FROM agent_run_reclamation_intents i JOIN workspaces w ON w.id=i.workspace_id WHERE i.id=$1`, root.S("id"), scene.operation).Scan(&retained); err != nil || !retained {
		t.Fatalf("D8 physical termination did not retain data without a ledger: %v / %v", retained, err)
	}
	child, op, effect := seedControlledDeletePlan(t, scene, root)
	op = seedResidualAction(t, scene, op, "effect_result", Object{"state": "succeeded", "externalId": "legacy-controlled-data-delete", "result": Object{"removed": true}}, effect.S("id")).O("operation")
	finished := seedResidualAction(t, scene, op, "advance", nil, "").O("operation")
	if finished.S("state") != "succeeded" || finished.S("id") == scene.operation {
		t.Fatalf("D8 controlled data deletion did not finish its new bound operation: %v", finished)
	}
	var released bool
	if err := scene.store.Pool.QueryRow(`SELECT to_jsonb(run)=$2::jsonb AND to_jsonb(original)=$3::jsonb
 AND run.phase='done' AND run.status='failed' AND run.failure_reason='workspace_unavailable'
 AND original.state='failed' AND original.error_code='node_unavailable'
 AND root.state='succeeded' AND root.disposition='terminated_data_retained' AND root.completed_at=$4::timestamptz
 AND root.legacy_delete_operation_id=original.id AND root.delete_operation_id=$5
 AND child.root_intent_id=root.id AND child.command_kind='delete_data' AND child.state='succeeded'
 AND child.disposition='released' AND child.delete_operation_id=$5 AND child.completed_at IS NOT NULL
 AND disposal.id=$5 AND disposal.state='succeeded' AND disposal.step='done'
 AND w.deleted_at IS NOT NULL AND w.issue_run_id=run.id
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals WHERE run_id=run.id)
 AND (SELECT count(*) FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')=2
 AND (SELECT count(*) FROM external_effects WHERE operation_id=disposal.id AND kind='workspace_data_delete'
 AND state='succeeded' AND result @> '{"removed":true}')=1
 FROM issue_runs run JOIN workspaces w ON w.id=run.workspace_id
 JOIN operations original ON original.id=$6 JOIN agent_run_reclamation_intents root ON root.id=$7
 JOIN agent_run_reclamation_intents child ON child.id=$8 JOIN operations disposal ON disposal.id=child.delete_operation_id
 WHERE run.id=$1`, scene.run, originalRun, originalOperation, root.S("completedAt"), finished.S("id"), scene.operation, root.S("id"), child.S("id")).Scan(&released); err != nil || !released {
		t.Fatalf("D8 retained-to-released path lost history, binding, or physical evidence: %v / %v", released, err)
	}
}

func TestControlledResidualDoesNotAuthorizeAutomaticEscalation(t *testing.T) {
	scene := seedControlledScene(t, controlledStores(t)(t), false)
	root := seedControlledRoot(t, scene)
	if err := scene.store.ReconcileAgentRunResidualsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	seedResidualAssert(t, scene, `SELECT reclaim_mode='controlled' AND reclaim_authorization->>'intentId'=$2 AND cleared_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=$3 AND kind='delete_workspace')
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=$3 AND kind='workspace_data_delete')
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, root.S("id"), scene.workspace)
}

func controlledRaceError(result residualRaceResult) error {
	if result.err != nil {
		return result.err
	}
	if code := result.out.S("managementConflict"); code != "" {
		return &Fault{Status: 409, Code: code}
	}
	return nil
}

func controlledRaceName(first bool) string { return fmt.Sprint("controlled_first=", first) }
