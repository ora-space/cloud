package core

import (
	"fmt"
	"testing"
	"time"
)

func TestControlledResidualRaceAutomaticClaimBothCommitOrders(t *testing.T) {
	stores := controlledStores(t)
	for _, controlledFirst := range []bool{false, true} {
		t.Run(controlledRaceName(controlledFirst), func(t *testing.T) {
			scene := seedControlledScene(t, stores(t), true)
			r := seedControlledRequest(scene, "race-controlled")
			automatic := func(tx *transaction) Object { return authorizeResidualOwnedDelete(tx, scene.run) }
			controlled := func(tx *transaction) Object { return manageResidualCommand(tx, &r) }
			first, second := automatic, controlled
			if controlledFirst {
				first, second = controlled, automatic
			}
			a, b := seedResidualRaceStores(t, scene, seedResidualRebuildStore(scene.store), seedResidualRebuildStore(scene.store), first, second)
			if err := controlledRaceError(a); err != nil {
				t.Fatal(err)
			}
			if err := controlledRaceError(b); err == nil {
				t.Fatal("second claimant acquired a second mode instead of respecting committed authority")
			} else if fault := ErrorCode(err); fault.Code != "residual_mode_conflict" {
				t.Fatalf("losing claimant returned %v", err)
			}
			mode, intents, operations := "owned_delete", 0, 1
			if controlledFirst {
				mode, intents, operations = "controlled", 1, 0
			}
			seedResidualAssert(t, scene, `SELECT r.reclaim_mode=$2 AND r.reclaim_id IS NOT NULL AND r.reclaim_state='reclaiming'
 AND r.responsibility=$3::jsonb AND r.cleared_at IS NULL
 AND (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=r.workspace_id)=$4
 AND (SELECT count(*) FROM operations WHERE workspace_id=r.workspace_id AND kind='delete_workspace')=$5
 AND (r.reclaim_mode<>'controlled' OR EXISTS(SELECT 1 FROM agent_run_reclamation_intents i WHERE i.reclaim_id=r.reclaim_id AND r.reclaim_authorization->>'intentId'=i.id::text))
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=r.workspace_id AND kind='workspace_data_delete')
 FROM agent_run_residuals r WHERE r.run_id=$1`, scene.run, mode, scene.responsibility, intents, operations)
		})
	}
}

func TestControlledResidualRaceIdempotencyBothCommitOrders(t *testing.T) {
	stores := controlledStores(t)
	for _, action := range []ResidualManagementAction{ResidualTerminate, ResidualDeleteData, ResidualConfirmExternal} {
		for _, conflict := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprint(action, "/conflict=", conflict, "/reverse=", reverse), func(t *testing.T) {
					scene := seedControlledScene(t, stores(t), false)
					r := seedControlledRequest(scene, "racing-command")
					switch action {
					case ResidualDeleteData:
						r = seedControlledDeleteRequest(scene, seedControlledRetained(t, scene), r.Key)
					case ResidualConfirmExternal:
						r = seedControlledExternalRequest(scene, seedControlledRoot(t, scene), r.Key)
					}
					other := r
					if conflict {
						other.Reason = "a different operator command with the same key"
					}
					if reverse {
						r, other = other, r
					}
					first := func(tx *transaction) Object { return manageResidualCommand(tx, &r) }
					second := func(tx *transaction) Object { return manageResidualCommand(tx, &other) }
					a, b := seedResidualRaceStores(t, scene, seedResidualRebuildStore(scene.store), seedResidualRebuildStore(scene.store), first, second)
					if err := controlledRaceError(a); err != nil {
						t.Fatal(err)
					}
					if conflict {
						if err := controlledRaceError(b); err == nil || ErrorCode(err).Code != "idempotency_conflict" {
							t.Fatalf("same-key conflicting input did not commit a conflict response: %v / %v", b.out, err)
						}
					} else if err := controlledRaceError(b); err != nil || b.out.O("intent").S("id") != a.out.O("intent").S("id") {
						t.Fatalf("same command did not replay original intent: %v / %v", b.out, err)
					}
					seedResidualAssert(t, scene, `SELECT (SELECT count(*) FROM agent_run_reclamation_command_receipts WHERE tenant_id=r.tenant_id AND idempotency_key=$2)=1
 AND EXISTS(SELECT 1 FROM agent_run_reclamation_command_receipts x WHERE x.tenant_id=r.tenant_id AND x.idempotency_key=$2
 AND x.command_input->>'reason'=$3 AND jsonb_array_length(x.audit)=$4)
 AND (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=r.workspace_id AND command_kind='terminate')=1
 AND (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=r.workspace_id AND command_kind='delete_data')=$5
 AND (SELECT count(*) FROM operations WHERE workspace_id=r.workspace_id AND kind='delete_workspace')=$5
 FROM agent_run_residuals r WHERE r.run_id=$1`, scene.run, r.Key, r.Reason, map[bool]int{false: 0, true: 1}[conflict], map[bool]int{false: 0, true: 1}[action == ResidualDeleteData])
				})
			}
		}
	}
}

func TestControlledResidualRaceDifferentKeysKeepOneRoot(t *testing.T) {
	stores := controlledStores(t)
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			scene := seedControlledScene(t, stores(t), false)
			r, other := seedControlledRequest(scene, "first-key"), seedControlledRequest(scene, "second-key")
			if reverse {
				r, other = other, r
			}
			a, b := seedResidualRace(t, scene, func(tx *transaction) Object { return manageResidualCommand(tx, &r) }, func(tx *transaction) Object { return manageResidualCommand(tx, &other) })
			if controlledRaceError(a) != nil || controlledRaceError(b) != nil || a.out.O("intent").S("id") != b.out.O("intent").S("id") {
				t.Fatalf("different keys obtained a second root: %v / %v", a, b)
			}
			seedResidualAssert(t, scene, `SELECT (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=r.workspace_id)=1
 AND (SELECT count(*) FROM agent_run_reclamation_targets x JOIN agent_run_reclamation_intents i ON i.id=x.root_intent_id WHERE i.workspace_id=r.workspace_id)=1
 FROM agent_run_residuals r WHERE r.run_id=$1`, scene.run)
		})
	}
}

func seedControlledCompletionRace(t *testing.T, store *Store, released bool) (residualReclaimScene, Object, func(*transaction) Object) {
	t.Helper()
	scene := seedControlledScene(t, store, true)
	if released {
		root := seedControlledRetained(t, scene)
		_, op, effect := seedControlledDeletePlan(t, scene, root)
		op = seedResidualAction(t, scene, op, "effect_result", Object{"state": "succeeded", "externalId": "race-controlled-data", "result": Object{"removed": true}}, effect.S("id")).O("operation")
		return scene, root, func(tx *transaction) Object {
			return advance(tx, &ControlRequest{}, tx.one("SELECT * FROM operations WHERE id=$1", op.S("id")))
		}
	}
	root := seedControlledRoot(t, scene)
	plan := seedControlledPlan(t, scene, root.S("id"))
	targets := residualObjects(plan["effects"])
	if len(targets) != 1 {
		t.Fatalf("race fixture has %d termination targets", len(targets))
	}
	return scene, root, func(tx *transaction) Object {
		intent := tx.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", root.S("id"))
		return forceStopCommand(tx, &ControlRequest{Action: "force_confirm", Body: Object{"epoch": 1, "forceStopId": root.S("id"), "version": intent.N("version"), "effectId": targets[0].S("id"), "terminated": true, "lateEnsureFenced": true}})
	}
}

func TestControlledResidualRaceCompletionAndLateFactsBothCommitOrders(t *testing.T) {
	stores := controlledStores(t)
	for _, released := range []bool{false, true} {
		for _, kind := range []string{"creation", "effect", "delivery", "session"} {
			for _, factFirst := range []bool{false, true} {
				t.Run(fmt.Sprint("released=", released, "/", kind, "/fact_first=", factFirst), func(t *testing.T) {
					scene, root, complete := seedControlledCompletionRace(t, stores(t), released)
					fact := func(tx *transaction) Object {
						switch kind {
						case "creation":
							return provisioningCreationFact(tx, &ControlRequest{OperationID: scene.operation, Body: Object{"outcome": "failed"}}, scene.run)
						case "effect":
							return provisioningFact(tx, &ControlRequest{OperationID: scene.operation, Body: Object{"kind": "effect", "effectId": scene.effect, "state": "failed", "externalId": "residual-substrate", "result": Object{"error": "late-conflicting-failure"}}})
						case "delivery":
							return seedResidualDelivery(tx, scene, Object{"outcome": "revision_failed", "reason": "upload_failed", "node": Object{"nodeId": "residual-node", "nodeIncarnationId": "residual-incarnation"}})
						default:
							return seedResidualLateExecution(tx, scene, "agent_session")
						}
					}
					first, second := complete, fact
					if factFirst {
						first, second = fact, complete
					}
					a, b := seedResidualRace(t, scene, first, second)
					conflict := kind != "session"
					if !conflict {
						if a.err != nil || b.err != nil {
							t.Fatalf("same fenced sandbox's late business terminal blocked physical completion: %v / %v", a.err, b.err)
						}
					} else if factFirst {
						assertResidualFactConflict(t, a.err)
						if b.err == nil {
							t.Fatal("completion ignored the other transaction's durable conflict")
						}
					} else {
						if a.err != nil {
							t.Fatal(a.err)
						}
						assertResidualFactConflict(t, b.err)
					}
					completed := !conflict || !factFirst
					seedResidualAssert(t, scene, `SELECT r.responsibility=$2::jsonb AND (r.reclaim_conflicted_at IS NOT NULL)=$3
 AND (w.deleted_at IS NOT NULL)=$4 AND run.phase='done' AND run.status='failed' AND run.failure_reason='workspace_unavailable'
 AND (i.state='succeeded')=$5 AND (SELECT count(*) FROM revisions WHERE run_id=r.run_id)=1
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN issue_runs run ON run.id=r.run_id
 JOIN agent_run_reclamation_intents i ON i.id=$6 WHERE r.run_id=$1`, scene.run, scene.responsibility, conflict, released && completed, released || completed, root.S("id"))
				})
			}
		}
	}
}

func TestControlledResidualRaceExternalAndPhysicalCompletionBothCommitOrders(t *testing.T) {
	stores := controlledStores(t)
	for _, released := range []bool{false, true} {
		for _, externalFirst := range []bool{false, true} {
			t.Run(fmt.Sprint("released=", released, "/external_first=", externalFirst), func(t *testing.T) {
				scene, root, complete := seedControlledCompletionRace(t, stores(t), released)
				intent := root
				if released {
					var id string
					if err := scene.store.Pool.QueryRow(`SELECT id::text FROM agent_run_reclamation_intents WHERE root_intent_id=$1`, root.S("id")).Scan(&id); err != nil {
						t.Fatal(err)
					}
					intent = seedControlledRow(t, scene.store, id)
				}
				r := seedControlledExternalRequest(scene, intent, "external-race")
				external := func(tx *transaction) Object { return manageResidualCommand(tx, &r) }
				first, second := complete, external
				if externalFirst {
					first, second = external, complete
				}
				a, b := seedResidualRace(t, scene, first, second)
				if err := controlledRaceError(a); err != nil {
					t.Fatal(err)
				}
				if !externalFirst && controlledRaceError(b) == nil {
					t.Fatal("new external command overwrote an already physically completed intent")
				}
				disposition := "terminated_data_retained"
				if released {
					disposition = "released"
				}
				if externalFirst {
					disposition = "externally_confirmed"
				}
				seedResidualAssert(t, scene, `SELECT r.disposition=$2 AND r.cleared_at IS NOT NULL AND r.responsibility=$3::jsonb
 AND i.state='succeeded' AND i.disposition=$2 AND (w.deleted_at IS NOT NULL)=$4
 AND (SELECT count(*) FROM agent_run_reclamation_intents WHERE workspace_id=w.id AND command_kind='terminate')=1
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN agent_run_reclamation_intents i ON i.id=$5 WHERE r.run_id=$1`, scene.run, disposition, scene.responsibility, released && !externalFirst, intent.S("id"))
			})
		}
	}
}

func TestControlledResidualRaceNoTargetProofAndDispatchBothCommitOrders(t *testing.T) {
	stores := controlledStores(t)
	for _, proofFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("proof_first=", proofFirst), func(t *testing.T) {
			s := stores(t)
			abandoned := seedAbandonedCreation(t, s, false)
			scene := residualReclaimScene{store: s, abandonedCreation: abandoned}
			root := seedControlledRoot(t, scene)
			proof := func(tx *transaction) Object {
				current := tx.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", root.S("id"))
				return controlledForceConfirm(tx, &ControlRequest{Action: "force_confirm", Body: Object{"epoch": 1, "version": current.N("version")}}, current)
			}
			producer := func(tx *transaction) Object {
				return enqueueExecutionWork(tx, scene.run, "agent_session", Object{"kind": "agent_session"}, Object{"workspaceId": scene.workspace, "sandboxInstanceId": newID(), "nodeId": "late-producer"}, "", 0, time.Time{})
			}
			first, second := producer, proof
			if proofFirst {
				first, second = proof, producer
			}
			a, b := seedResidualRace(t, scene, first, second)
			proved, rejected := b, a
			if proofFirst {
				proved, rejected = a, b
			}
			if proved.err != nil {
				t.Fatalf("closed, never-planned responsibility could not complete: %v", proved.err)
			}
			if rejected.err == nil {
				t.Fatal("old execution producer registered work against a closed residual")
			}
			// This scene is already terminal and ledger-protected before the controlled command.
			// It proves the real producer cannot invalidate the no-target proof in either order;
			// it does not claim that controlled closure is its only reason for rejection.
			seedResidualAssert(t, scene, `SELECT r.reclaim_state='cleared' AND r.disposition='terminated_data_retained'
 AND r.cleared_at IS NOT NULL AND NOT w.admission_open AND w.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM execution_work WHERE run_id=r.run_id)
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=r.workspace_id)
 AND NOT EXISTS(SELECT 1 FROM agent_run_reclamation_targets WHERE root_intent_id=$2)
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id WHERE r.run_id=$1`, scene.run, root.S("id"))
		})
	}
	for _, controlledFirst := range []bool{false, true} {
		t.Run("legacy_producer/"+controlledRaceName(controlledFirst), func(t *testing.T) {
			scene := seedControlledLegacyD8(t, stores(t))
			r := seedControlledRequest(scene, "legacy-dispatch-race")
			controlled := func(tx *transaction) Object { return manageResidualCommand(tx, &r) }
			producer := func(tx *transaction) Object {
				return enqueueExecutionWork(tx, scene.run, "agent_session", Object{"kind": "agent_session"}, Object{"workspaceId": scene.workspace, "sandboxInstanceId": scene.effect, "runtimeGeneration": 1, "nodeId": "legacy-node"}, "", 0, time.Time{})
			}
			first, second := producer, controlled
			if controlledFirst {
				first, second = controlled, producer
			}
			a, b := seedResidualRace(t, scene, first, second)
			registration, dispatch := b, a
			if controlledFirst {
				registration, dispatch = a, b
			}
			if err := controlledRaceError(registration); err != nil {
				t.Fatal(err)
			}
			if controlledFirst && dispatch.err == nil {
				t.Fatal("old D8 producer bypassed the committed controlled admission closure")
			}
			root := registration.out.O("intent")
			out, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
				return Object{"complete": completeControlledTermination(tx, tx.one("SELECT * FROM agent_run_reclamation_intents WHERE id=$1", root.S("id")))}
			})
			if err != nil || out.B("complete") {
				t.Fatalf("legacy allocating identity was treated as a no-target proof: %v / %v", out, err)
			}
			var valid bool
			if err := scene.store.Pool.QueryRow(`SELECT i.state<>'succeeded' AND i.completed_at IS NULL
 AND EXISTS(SELECT 1 FROM agent_run_reclamation_targets WHERE root_intent_id=i.id AND sandbox_id=$2 AND state='planned')
 AND (SELECT count(*) FROM execution_work WHERE run_id=i.run_id)=$3
 FROM agent_run_reclamation_intents i WHERE i.id=$1`, root.S("id"), scene.effect, map[bool]int{false: 0, true: 1}[dispatch.err == nil]).Scan(&valid); err != nil || !valid {
				t.Fatalf("D8 dispatch race lost original identity or accepted unproved completion: %v / %v", valid, err)
			}
		})
	}
}
