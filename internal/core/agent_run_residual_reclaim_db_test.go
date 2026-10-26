package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type residualReclaimScene struct {
	store *Store
	abandonedCreation
	node, delivery, work          string
	deliveryInput, deliveryResult Object
}

func seedResidualExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.Pool.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// The ref/history-verification outcome is an explicit fixture. The actual delivery applier,
// receipt, Revision insert and residual fact write run together below; this does not test Git or storage.
func seedResidualScene(t *testing.T, ready bool) residualReclaimScene {
	t.Helper()
	s := commandStore(t)
	scene := residualReclaimScene{store: s, abandonedCreation: seedAbandonedCreation(t, s, true), node: newID(), delivery: "residual-delivery-" + newID(), work: newID()}
	seedResidualExec(t, s, `INSERT INTO node_instances(id,workspace_id,sandbox_instance_id,service_subject,connection_state,protocol_version,initialized,node_id,node_incarnation_id)
 VALUES($1,$2,$3,'residual-node','connected',1,true,'residual-node','residual-incarnation')`, scene.node, scene.workspace, scene.effect)
	seedResidualExec(t, s, `UPDATE sandbox_instances SET observed_state='running' WHERE id=$1`, scene.effect)
	if ready {
		_, err := submitFact(t, s, scene.abandonedCreation, "residual-ensure", Object{"epoch": 1, "kind": "effect", "effectId": scene.effect, "state": "succeeded", "externalId": "residual-substrate", "result": Object{"sandboxInstanceId": scene.effect, "nodeId": "residual-node"}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = submitFact(t, s, scene.abandonedCreation, "residual-creation", Object{"epoch": 1, "kind": "creation", "outcome": "ready"})
		if err != nil {
			t.Fatal(err)
		}
	}
	scene.deliveryInput = Object{"kind": "deliver_revision", "baseCommit": baseCommit, "revisionRef": "refs/ora/revisions/" + scene.run, "deliveryRef": GitDeliveryRefPrefix + scene.work, "historyKey": "revisions/" + scene.run + "/history"}
	scene.deliveryResult = Object{"outcome": "revision_unchanged", "node": Object{"nodeId": "residual-node", "nodeIncarnationId": "residual-incarnation"}, "baseCommit": baseCommit, "finalCommit": baseCommit, "revisionRef": scene.deliveryInput.S("revisionRef"), "history": Object{"key": scene.deliveryInput.S("historyKey"), "size": 0, "sha256": strings.Repeat("a", 64)}}
	seedResidualExec(t, s, `INSERT INTO execution_work(id,run_id,kind,input,target,execution_id) VALUES($1,$2,'deliver_revision',$3,$4,$5)`, scene.work, scene.run, jsonText(scene.deliveryInput), jsonText(Object{"workspaceId": scene.workspace, "sandboxInstanceId": scene.effect, "runtimeGeneration": 1, "nodeId": "residual-node"}), scene.delivery)
	seedResidualExec(t, s, `INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
 VALUES($1,'deliver_revision',$2,$3,$4,'residual-node',$1,$5,1)`, scene.delivery, scene.run, scene.work, scene.workspace, jsonText(scene.deliveryInput))
	if ready {
		_, err := s.transact(t.Context(), func(tx *transaction) Object { return seedResidualDelivery(tx, scene, scene.deliveryResult) })
		if err != nil {
			t.Fatal(err)
		}
		seedResidualRuntimeAck(t, scene, true)
	}
	return scene
}

func seedResidualDelivery(tx *transaction, scene residualReclaimScene, result Object) Object {
	return nodeExecutionResult(tx, &ControlRequest{Action: "clone_takeover", Body: Object{"operationId": scene.run, "executionId": scene.delivery, "sequence": 1, "event": "residual-delivery-terminal", "result": result}, verification: &revisionVerification{input: jsonText(scene.deliveryInput), valid: true}}, true)
}

func seedResidualRuntimeAck(t *testing.T, scene residualReclaimScene, closed bool) {
	t.Helper()
	_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
		c := runtimeControl(tx, scene.workspace)
		return runtimeControlCommand(tx, &ControlRequest{Action: "runtime_ack", Body: Object{"workspaceId": scene.workspace, "nodeInstanceId": scene.node, "controlEpoch": c.N("controlEpoch"), "controlVersion": c.N("version"), "inputClosed": closed, "unfinishedExecutionIds": []any{}}})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedResidualClaim(t *testing.T, scene residualReclaimScene) Object {
	t.Helper()
	if err := scene.store.ReconcileAgentRunResidualsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err := scene.store.Control(t.Context(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	op := out.O("operation")
	if op.S("kind") != "delete_workspace" || op.O("request").S("reclaimId") == "" {
		t.Fatalf("no owned residual delete claimed: %v", out)
	}
	return op
}

func seedResidualAction(t *testing.T, scene residualReclaimScene, op Object, action string, body Object, effect string) Object {
	t.Helper()
	if body == nil {
		body = Object{}
	}
	body["epoch"], body["version"] = int64(1), op.N("version")
	out, err := scene.store.Control(t.Context(), &ControlRequest{Action: action, OperationID: op.S("id"), EffectID: effect, Body: body, Service: scene.claims})
	if err != nil {
		t.Fatalf("residual %s at %s: %v", action, op.S("step"), err)
	}
	if out.S("id") != "" {
		return Object{"operation": out}
	}
	return out
}

// Drive the shipped quiesce -> terminate -> cleanup topology, leaving cleanup -> done to callers.
func seedResidualCleanup(t *testing.T, scene residualReclaimScene) Object {
	t.Helper()
	op, effect := seedResidualCleanupPlan(t, scene)
	return seedResidualAction(t, scene, op, "effect_result", Object{"state": "succeeded", "externalId": "residual-cleanup", "result": Object{"removed": true}}, effect.S("id")).O("operation")
}

func seedResidualCleanupPlan(t *testing.T, scene residualReclaimScene) (Object, Object) {
	t.Helper()
	op, termination := seedResidualTerminationPlan(t, scene)
	op = seedResidualAction(t, scene, op, "effect_result", Object{"state": "succeeded", "externalId": "residual-termination", "result": Object{"terminated": true, "lateEnsureFenced": true}}, termination.S("id")).O("operation")
	op = seedResidualAction(t, scene, op, "advance", nil, "").O("operation")
	planned := seedResidualAction(t, scene, op, "plan", Object{"kind": "workspace_data_delete", "workspaceId": scene.workspace}, "")
	return planned.O("operation"), planned.O("effect")
}

func seedResidualTerminationPlan(t *testing.T, scene residualReclaimScene) (Object, Object) {
	t.Helper()
	op := seedResidualClaim(t, scene)
	seedResidualRuntimeAck(t, scene, false)
	_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
		n := tx.one("SELECT * FROM node_instances WHERE id=$1", scene.node)
		w := tx.one("SELECT * FROM workspaces WHERE id=$1", scene.workspace)
		return nodeIdle(tx, &ControlRequest{Body: Object{"version": n.N("version"), "operationId": op.S("id"), "admissionEpoch": w.N("admissionEpoch"), "idle": true}}, n, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	op = seedResidualAction(t, scene, op, "advance", nil, "").O("operation")
	planned := seedResidualAction(t, scene, op, "plan", Object{"kind": "sandbox_terminate", "workspaceId": scene.workspace}, "")
	return planned.O("operation"), planned.O("effect")
}

func TestResidualOwnedCleanupRetriesSameFailedEffectAcrossControllerEpoch(t *testing.T) {
	for _, kind := range []string{"sandbox_terminate", "workspace_data_delete"} {
		t.Run(kind, func(t *testing.T) { seedResidualRetryCleanupEffect(t, kind) })
	}
}

func seedResidualRetryCleanupEffect(t *testing.T, kind string) {
	t.Helper()
	scene := seedResidualScene(t, true)
	var op, cleanup Object
	success := Object{"removed": true}
	if kind == "sandbox_terminate" {
		op, cleanup = seedResidualTerminationPlan(t, scene)
		success = Object{"terminated": true, "lateEnsureFenced": true}
	} else {
		op, cleanup = seedResidualCleanupPlan(t, scene)
	}
	cleanupID := cleanup.S("id")
	var reclaimID, terminationID string
	if err := scene.store.Pool.QueryRow(`SELECT r.reclaim_id::text,e.id::text FROM agent_run_residuals r JOIN external_effects e ON e.operation_id=r.reclaim_operation_id AND e.kind='sandbox_terminate' WHERE r.run_id=$1`, scene.run).Scan(&reclaimID, &terminationID); err != nil {
		t.Fatal(err)
	}
	failed := Object{"state": "failed", "externalId": "residual-cleanup-retry", "result": Object{"error": "temporary_storage_unavailable"}}
	op = seedResidualAction(t, scene, op, "effect_result", failed, cleanupID).O("operation")
	deferredState, deferredCode := "retry_wait", "node_unavailable"
	if kind == "sandbox_terminate" {
		// The existing Controller uses blocked for an unconfirmed termination. A residual
		// responsibility must normalize that request to a claimable retry_wait, not strand
		// the hidden Workspace behind an unavailable public/operator retry action.
		deferredState, deferredCode = "blocked", "termination_unconfirmed"
	}
	op = seedResidualAction(t, scene, op, "defer", Object{"state": deferredState, "errorCode": deferredCode, "retrySeconds": 1}, "").O("operation")
	if op.S("state") != "retry_wait" || op.S("errorCode") != deferredCode {
		t.Fatalf("owned cleanup defer is not retryable: %v", op)
	}
	seedResidualExec(t, scene.store, `UPDATE operations SET retry_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.S("id"))
	_, err := scene.store.Control(t.Context(), &ControlRequest{Action: "lease_release", Body: Object{"epoch": 1}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	restarted := seedResidualRebuildStore(scene.store)
	lease, err := restarted.Control(t.Context(), &ControlRequest{Action: "lease_acquire", Body: Object{}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	epoch := lease.N("epoch")
	if epoch != 2 {
		t.Fatalf("replacement Controller epoch=%d, want 2", epoch)
	}
	if err := restarted.ReconcileAgentRunResidualsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	claimed, err := restarted.Control(t.Context(), &ControlRequest{Action: "claim", Body: Object{"epoch": epoch}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	if current := claimed.O("operation"); current.S("id") != op.S("id") || current.N("controllerEpoch") != epoch {
		t.Fatalf("retry replaced its owned operation: %v", claimed)
	} else {
		op = current
	}
	control := func(action, effect string, body Object) (Object, error) {
		body["epoch"], body["version"] = epoch, op.N("version")
		out, err := restarted.Control(t.Context(), &ControlRequest{Action: action, OperationID: op.S("id"), EffectID: effect, Body: body, Service: scene.claims})
		if err == nil {
			if current := out.O("operation"); len(current) > 0 {
				op = current
			} else if out.S("id") != "" {
				op = out
			}
		}
		return out, err
	}
	// Reconcile each original effect under the replacement epoch before reusing its plan. These
	// are fresh accepted observations, not mutation of the old effect or invention of a new one.
	if kind == "workspace_data_delete" {
		if _, err := control("effect_result", terminationID, Object{"state": "succeeded", "externalId": "residual-termination", "result": Object{"terminated": true, "lateEnsureFenced": true}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := control("effect_result", cleanupID, Object{"state": "failed", "externalId": "residual-cleanup-retry", "result": Object{"error": "temporary_storage_unavailable"}}); err != nil {
		t.Fatal(err)
	}
	planned, err := control("plan", "", Object{"kind": kind, "workspaceId": scene.workspace})
	if err != nil {
		t.Fatal(err)
	}
	if planned.O("effect").S("id") != cleanupID {
		t.Fatalf("retry substituted a new cleanup effect: %v", planned)
	}
	if _, err := control("effect_result", cleanupID, Object{"state": "succeeded", "externalId": "residual-cleanup-retry", "result": success}); err != nil {
		t.Fatalf("same-effect failed-to-success retry was rejected: %v", err)
	}
	if kind == "sandbox_terminate" {
		if _, err := control("advance", "", Object{}); err != nil {
			t.Fatal(err)
		}
		dataPlan, err := control("plan", "", Object{"kind": "workspace_data_delete", "workspaceId": scene.workspace})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := control("effect_result", dataPlan.O("effect").S("id"), Object{"state": "succeeded", "externalId": "post-termination-cleanup", "result": Object{"removed": true}}); err != nil {
			t.Fatal(err)
		}
	}
	finished, err := control("advance", "", Object{})
	if err != nil {
		t.Fatal(err)
	}
	if finished.S("state") != "succeeded" {
		t.Fatalf("retry did not finish cleanup: %v", finished)
	}
	completedVersion := finished.N("version")
	if completedVersion <= 1 {
		t.Fatalf("cleanup did not advance its operation version: %v", finished)
	}
	lateResult := func(submission, message string) (Object, error) {
		// The succeeded operation is no longer executable. Its original binding still accepts
		// facts through Control with the current lease and a deliberately stale operation version.
		return restarted.Control(t.Context(), &ControlRequest{
			Action: "effect_result", OperationID: op.S("id"), EffectID: cleanupID, SubmissionID: submission,
			Body: Object{"epoch": epoch, "version": 1, "state": "failed", "externalId": "residual-cleanup-retry", "result": Object{"error": message}}, Service: scene.claims,
		})
	}
	// A previously accepted failed observation can replay after success, even after clearing. It
	// must neither downgrade the effect nor manufacture a conflict or another evidence entry.
	for attempt := 0; attempt < 2; attempt++ {
		replayed, err := lateResult("cleared-prior-failure-"+kind, "temporary_storage_unavailable")
		if err != nil {
			t.Fatalf("consistent historical failed observation did not replay through Control: %v", err)
		}
		if replayed.O("operation").N("version") != completedVersion || replayed.O("effect").S("state") != "succeeded" {
			t.Fatalf("cleared fact replay changed canonical state/version: %v", replayed)
		}
	}
	seedResidualAssert(t, scene, `SELECT r.reclaim_id=$2::uuid AND r.reclaim_operation_id=$3::uuid AND r.reclaim_state='cleared'
 AND r.disposition='released' AND r.reclaim_conflicted_at IS NULL AND r.responsibility=$5::jsonb
 AND e.state='succeeded' AND e.reconciled_epoch=$6 AND e.result=$7::jsonb
 AND (SELECT version=$8 AND state='succeeded' FROM operations WHERE id=r.reclaim_operation_id)
 AND (SELECT count(*) FROM external_effects WHERE operation_id=r.reclaim_operation_id)=2
 AND (SELECT count(*) FROM operations WHERE workspace_id=r.workspace_id AND kind='delete_workspace')=1
 AND (SELECT string_agg(entry->'fact'->>'state',',' ORDER BY ordinal) FROM jsonb_array_elements(r.reclaim_evidence) WITH ORDINALITY facts(entry,ordinal)
      WHERE entry->>'key'='effect:'||$4::text)='failed,succeeded'
 FROM agent_run_residuals r JOIN external_effects e ON e.id=$4::uuid WHERE r.run_id=$1`, scene.run, reclaimID, op.S("id"), cleanupID, scene.responsibility, epoch, jsonText(success), completedVersion)
	// A changed post-success conclusion remains an A4 contradiction, durably audited even after
	// the business/physical cleanup completed. It cannot rewrite the accepted success.
	for attempt := 0; attempt < 2; attempt++ {
		_, err = lateResult("cleared-conflicting-failure-"+kind, "different_post_success_failure")
		assertResidualFactConflict(t, err)
	}
	seedResidualAssert(t, scene, `SELECT r.reclaim_state='cleared' AND r.reclaim_conflicted_at IS NOT NULL
 AND r.disposition='released' AND r.cleared_at IS NOT NULL
 AND (SELECT version=$4 AND state='succeeded' FROM operations WHERE id=r.reclaim_operation_id)
 AND e.state='succeeded' AND e.result=$3::jsonb
 AND (SELECT count(*) FROM jsonb_array_elements(r.reclaim_evidence) entry WHERE entry->>'key'='effect:'||$2::text
   AND entry->'fact'->>'kind'='conflict' AND entry->'fact'->'original'->>'state'='succeeded'
   AND entry->'fact'->'incoming'->'result'->>'error'='different_post_success_failure')=1
 FROM agent_run_residuals r JOIN external_effects e ON e.id=$2::uuid WHERE r.run_id=$1`, scene.run, cleanupID, jsonText(success), completedVersion)
}

func seedResidualAssert(t *testing.T, scene residualReclaimScene, query string, args ...any) {
	t.Helper()
	var ok bool
	if err := scene.store.Pool.QueryRowContext(t.Context(), query, args...).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("residual invariant failed: %s", query)
	}
}

func TestResidualOwnedDeleteRunsCloudTopologyAndPreservesBusinessTerminal(t *testing.T) {
	scene := seedResidualScene(t, true)
	op := seedResidualCleanup(t, scene)
	out := seedResidualAction(t, scene, op, "advance", nil, "")
	if out.O("operation").S("state") != "succeeded" {
		t.Fatalf("cleanup did not finish: %v", out)
	}
	seedResidualAssert(t, scene, `SELECT r.reclaim_state='cleared' AND r.disposition='released' AND r.cleared_at IS NOT NULL
 AND r.responsibility=$2::jsonb AND r.reason='workspace_unavailable' AND w.deleted_at IS NOT NULL AND NOT w.admission_open
 AND i.phase='done' AND i.status='failed' AND i.failure_reason='workspace_unavailable'
 AND (SELECT count(*) FROM operations WHERE workspace_id=w.id AND kind='delete_workspace')=1
 AND (SELECT count(*) FROM external_effects WHERE operation_id=r.reclaim_operation_id AND kind IN ('sandbox_terminate','workspace_data_delete') AND state='succeeded')=2
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN issue_runs i ON i.id=r.run_id WHERE r.run_id=$1`, scene.run, scene.responsibility)
}

func TestResidualUnknownAndDeliveryFailureNeverAuthorizeDataDeletion(t *testing.T) {
	for _, delivery := range []string{"unknown", "failed", "skipped", "timeout"} {
		t.Run(delivery, func(t *testing.T) {
			scene := seedResidualScene(t, false)
			if delivery != "unknown" {
				_, err := submitFact(t, scene.store, scene.abandonedCreation, "negative-ready-ensure", Object{"epoch": 1, "kind": "effect", "effectId": scene.effect, "state": "succeeded", "externalId": "negative-substrate", "result": Object{"sandboxInstanceId": scene.effect, "nodeId": "residual-node"}})
				if err != nil {
					t.Fatal(err)
				}
				_, err = scene.store.transact(t.Context(), func(tx *transaction) Object {
					return seedResidualDelivery(tx, scene, Object{"outcome": "revision_failed", "reason": "upload_failed", "node": Object{"nodeId": "residual-node", "nodeIncarnationId": "residual-incarnation"}})
				})
				if err != nil {
					t.Fatal(err)
				}
				seedResidualRuntimeAck(t, scene, true)
				seedResidualExec(t, scene.store, `UPDATE issue_runs SET result=jsonb_set(COALESCE(result,'{}'),'{deliveryState}',to_jsonb($2::text)) WHERE id=$1`, scene.run, map[string]string{"failed": "failed", "skipped": "skipped", "timeout": "failed"}[delivery])
			}
			if delivery == "timeout" {
				seedResidualExec(t, scene.store, `UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '10 days' WHERE id=$1`, scene.run)
			}
			if err := scene.store.ReconcileAgentRunResidualsOnce(t.Context()); err != nil {
				t.Fatal(err)
			}
			seedResidualAssert(t, scene, `SELECT reclaim_state='reconciling' AND reclaim_id IS NULL AND cleared_at IS NULL
 AND responsibility=$2::jsonb AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=$3 AND kind='delete_workspace')
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=$3 AND kind IN ('sandbox_terminate','workspace_data_delete'))
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, scene.responsibility, scene.workspace)
		})
	}
}

func TestResidualPartialAndNullScopeTargetsRemainAUnion(t *testing.T) {
	scene := seedResidualScene(t, true)
	extra, missing := newID(), newID()
	seedResidualExec(t, scene.store, `INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch)
 VALUES($1,$2,$3,NULL,'sandbox_ensure','failed','{}',1)`, extra, scene.operation, scene.project)
	seedResidualExec(t, scene.store, `UPDATE agent_run_residuals SET reclaim_targets=$2 WHERE run_id=$1`, scene.run, jsonText([]Object{{"kind": "sandbox", "id": missing, "workspaceId": scene.workspace}}))
	if err := scene.store.ReconcileAgentRunResidualsOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	seedResidualAssert(t, scene, `SELECT reclaim_state='reconciling' AND reclaim_id IS NULL
 AND reclaim_targets @> jsonb_build_array(jsonb_build_object('kind','sandbox','id',$2::text))
 AND reclaim_targets @> jsonb_build_array(jsonb_build_object('kind','effect','id',$3::text))
 AND reclaim_targets @> jsonb_build_array(jsonb_build_object('kind','sandbox','id',$3::text))
 AND responsibility=$4::jsonb FROM agent_run_residuals WHERE run_id=$1`, scene.run, missing, extra, scene.responsibility)
}

func TestResidualCleanupRequiresEveryDualTerminationAndRemovedProof(t *testing.T) {
	for _, proof := range []string{"terminated_only", "wrong_sandbox", "not_removed", "historical_target"} {
		t.Run(proof, func(t *testing.T) {
			scene := seedResidualScene(t, true)
			op := seedResidualCleanup(t, scene)
			switch proof {
			case "terminated_only":
				seedResidualExec(t, scene.store, `UPDATE external_effects SET result='{"terminated":true}' WHERE operation_id=$1 AND kind='sandbox_terminate'`, op.S("id"))
			case "wrong_sandbox":
				seedResidualExec(t, scene.store, `UPDATE external_effects SET request=jsonb_set(request,'{sandboxInstanceId}',to_jsonb($2::text)) WHERE operation_id=$1 AND kind='sandbox_terminate'`, op.S("id"), newID())
			case "not_removed":
				seedResidualExec(t, scene.store, `UPDATE external_effects SET result='{"removed":false}' WHERE operation_id=$1 AND kind='workspace_data_delete'`, op.S("id"))
			case "historical_target":
				seedResidualExec(t, scene.store, `UPDATE agent_run_residuals SET reclaim_targets=reclaim_targets||$2::jsonb WHERE run_id=$1`, scene.run, jsonText([]Object{{"kind": "sandbox", "id": newID(), "workspaceId": scene.workspace}}))
			}
			_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
				return Object{"complete": completeResidualReclamation(tx, tx.one("SELECT * FROM operations WHERE id=$1", op.S("id")))}
			})
			if err == nil {
				t.Fatal("incomplete cleanup proof cleared the residual")
			}
			seedResidualAssert(t, scene, `SELECT r.cleared_at IS NULL AND r.reclaim_state='reclaiming' AND w.deleted_at IS NULL FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id WHERE r.run_id=$1`, scene.run)
		})
	}
}

func TestResidualClaimRollbackAndRestartKeepOneBoundOperation(t *testing.T) {
	scene := seedResidualScene(t, true)
	_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
		authorizeResidualOwnedDelete(tx, scene.run)
		reject(409, "fixture_rollback")
		return nil
	})
	var fault *Fault
	if !errors.As(err, &fault) || fault.Code != "fixture_rollback" {
		t.Fatalf("rollback boundary: %v", err)
	}
	seedResidualAssert(t, scene, `SELECT reclaim_state='reconciling' AND reclaim_id IS NULL AND NOT EXISTS(SELECT 1 FROM operations WHERE workspace_id=$2 AND kind='delete_workspace') FROM agent_run_residuals WHERE run_id=$1`, scene.run, scene.workspace)
	op := seedResidualClaim(t, scene)
	seedResidualExec(t, scene.store, `UPDATE operations SET state='retry_wait',retry_at=clock_timestamp()-interval '1 second',error_code='node_unavailable' WHERE id=$1`, op.S("id"))
	var authority, targets string
	if err := scene.store.Pool.QueryRow(`SELECT reclaim_authorization::text,reclaim_targets::text FROM agent_run_residuals WHERE run_id=$1`, scene.run).Scan(&authority, &targets); err != nil {
		t.Fatal(err)
	}
	// A new process-owned Store value shares the persisted database, not any in-memory claim state.
	restarted := seedResidualRebuildStore(scene.store)
	if err := restarted.ReconcileAgentRunResidualsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = restarted.transact(t.Context(), func(tx *transaction) Object { return authorizeResidualOwnedDelete(tx, scene.run) })
	if err != nil {
		t.Fatal(err)
	}
	// Run Workspaces are intentionally hidden from public retryOperation. The server resumes
	// their due retry_wait operation through the Controller's existing claim path instead.
	reclaimed, err := restarted.Control(t.Context(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: scene.claims})
	if err != nil {
		t.Fatal(err)
	}
	if retried := reclaimed.O("operation"); retried.S("id") != op.S("id") || retried.S("state") != "running" {
		t.Fatalf("restart did not retry the original bound operation: %v", reclaimed)
	}
	seedResidualAssert(t, scene, `SELECT reclaim_operation_id=$2 AND reclaim_authorization=$3::jsonb AND reclaim_targets=$4::jsonb
 AND (SELECT count(*) FROM operations WHERE workspace_id=$5 AND kind='delete_workspace')=1 FROM agent_run_residuals WHERE run_id=$1`, scene.run, op.S("id"), authority, targets, scene.workspace)
}

func seedResidualRebuildStore(original *Store) *Store {
	restarted := &Store{Pool: original.Pool, Signals: NewControlHub()}
	BindBusinessHooks(restarted)
	return restarted
}

func TestResidualCompletionRollbackKeepsLedgerAndWorkspaceAtomic(t *testing.T) {
	scene := seedResidualScene(t, true)
	op := seedResidualCleanup(t, scene)
	_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
		advance(tx, &ControlRequest{}, tx.one("SELECT * FROM operations WHERE id=$1", op.S("id")))
		reject(409, "fixture_completion_rollback")
		return nil
	})
	var fault *Fault
	if !errors.As(err, &fault) || fault.Code != "fixture_completion_rollback" {
		t.Fatalf("completion rollback boundary: %v", err)
	}
	seedResidualAssert(t, scene, `SELECT r.reclaim_state='reclaiming' AND r.cleared_at IS NULL AND r.disposition IS NULL
 AND w.deleted_at IS NULL AND o.state='running' AND o.step='cleanup'
 AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(r.reclaim_evidence) e WHERE e->'fact'->>'kind'='reclaim_complete')
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN operations o ON o.id=r.reclaim_operation_id WHERE r.run_id=$1`, scene.run)
}

func TestResidualOrdinaryLifecycleGuardsAndUnrelatedD8StayProtected(t *testing.T) {
	scene := seedResidualScene(t, true)
	if _, err := scene.store.DeleteRunWorkspace(t.Context(), scene.run); err == nil {
		t.Fatal("ordinary run delete bypassed the residual")
	}
	for _, kind := range []string{"stop", "delete_workspace", "delete_project"} {
		_, err := scene.store.transact(t.Context(), func(tx *transaction) Object {
			w := tx.one("SELECT * FROM workspaces WHERE id=$1", scene.workspace)
			if kind == "delete_project" {
				for _, item := range tx.list("SELECT * FROM workspaces WHERE project_id=$1", scene.project) {
					checkActivities(tx, item)
				}
			} else {
				checkActivities(tx, w)
			}
			return nil
		})
		var fault *Fault
		if !errors.As(err, &fault) || fault.Code != "resource_in_use" {
			t.Fatalf("%s guard bypass: %v", kind, err)
		}
	}
	// D8 remains on its existing releasing-only scan: this done residual never enters that path.
	if err := scene.store.GiveUpStaleWorkspaceReleasesOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	seedResidualAssert(t, scene, `SELECT i.phase='done' AND i.status='failed' AND i.failure_reason='workspace_unavailable'
 AND r.reason='workspace_unavailable' AND r.responsibility=$2::jsonb AND r.cleared_at IS NULL
 FROM issue_runs i JOIN agent_run_residuals r ON r.run_id=i.id WHERE i.id=$1`, scene.run, scene.responsibility)
}
