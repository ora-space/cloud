package core

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"
)

type residualRaceResult struct {
	out, observed Object
	err           error
}

// Wrap the two real, dedicated-connection transactions with the production transaction context.
// This deliberately mirrors Store.transact's hooks: the callbacks below call the same production
// state functions while the advisory lock is held, never substitutes built only for the race.
func seedResidualTransaction(ctx context.Context, s *Store, raw *sql.Tx) *transaction {
	tx := &transaction{tx: raw, ctx: ctx, store: s, pluginExecution: s.PluginExecution, directory: s.Directory, contextBuilder: s.Context, forms: s.Forms, assist: s.Assist, simulator: s.Simulator, objectStore: s.ObjectStore, onThreadEvents: s.OnThreadEvents, onSessionEnded: s.OnSessionEnded, onRunWorkspaceSettled: s.OnRunWorkspaceSettled, onRunWorkspaceDeleted: s.OnRunWorkspaceDeleted, onDeliverySettled: s.OnDeliverySettled}
	tx.ctx = context.WithValue(ctx, businessTransactionKey{}, tx)
	return tx
}

func seedResidualRaceApply(tx *transaction, run string, fn func(*transaction) Object) (result residualRaceResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			switch value := recovered.(type) {
			case *Fault:
				result.err = value
			case databaseFailure:
				result.err = value.err
			default:
				panic(recovered)
			}
		}
	}()
	// This statement happens after lock acquisition. A default READ COMMITTED transaction must
	// observe A's commit here, even though B began and tried the lock before that commit.
	result.observed = tx.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", run)
	result.out = fn(tx)
	return result
}

// A executes while holding the production lock and stays uncommitted. B proves contention with
// pg_try_advisory_xact_lock=false on its SAME transaction, signals the channel, then blocks on the
// lock. Only then may A commit/roll back; B subsequently re-reads and calls its production function.
// No sleep, external probe connection or elapsed-time inference substitutes for that ordering.
func seedResidualRace(t *testing.T, scene residualReclaimScene, first, second func(*transaction) Object) (residualRaceResult, residualRaceResult) {
	return seedResidualRaceStores(t, scene, scene.store, scene.store, first, second)
}

func seedResidualRaceStores(t *testing.T, scene residualReclaimScene, storeA, storeB *Store, first, second func(*transaction) Object) (residualRaceResult, residualRaceResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	a, err := scene.store.Pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := scene.store.Pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	rawA, err := a.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rawA.Rollback()
	rawB, err := b.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rawB.Rollback()
	if _, err = rawA.ExecContext(ctx, "SELECT pg_advisory_xact_lock(67420911)"); err != nil {
		t.Fatal(err)
	}
	txA, txB := seedResidualTransaction(ctx, storeA, rawA), seedResidualTransaction(ctx, storeB, rawB)
	resultA := seedResidualRaceApply(txA, scene.run, first)
	ready, finished := make(chan error, 1), make(chan residualRaceResult, 1)
	go func() {
		var acquired bool
		if err := rawB.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock(67420911)").Scan(&acquired); err != nil {
			ready <- err
			return
		}
		if acquired {
			ready <- fmt.Errorf("B acquired A's still-uncommitted lock")
			return
		}
		ready <- nil
		if _, err := rawB.ExecContext(ctx, "SELECT pg_advisory_xact_lock(67420911)"); err != nil {
			finished <- residualRaceResult{err: err}
			return
		}
		result := seedResidualRaceApply(txB, scene.run, second)
		if result.err != nil {
			_ = rawB.Rollback()
		} else {
			result.err = rawB.Commit()
			if result.err == nil && txB.residualFactConflict {
				result.err = &Fault{Status: 409, Code: "residual_fact_conflict"}
			}
		}
		finished <- result
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if resultA.err != nil {
		_ = rawA.Rollback()
	} else {
		resultA.err = rawA.Commit()
		if resultA.err == nil && txA.residualFactConflict {
			resultA.err = &Fault{Status: 409, Code: "residual_fact_conflict"}
		}
	}
	var resultB residualRaceResult
	select {
	case resultB = <-finished:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return resultA, resultB
}

// A delayed dispatch record is a staged precondition, not a new runtime invocation. Its real
// terminal is taken over by nodeExecutionResult. Session completion still leaves its independent
// deliver_revision target without verified storage evidence, which must block data deletion.
func seedResidualLateExecution(tx *transaction, scene residualReclaimScene, kind string) Object {
	work, execution := newID(), "late-"+kind+"-"+newID()
	target := Object{"workspaceId": scene.workspace, "sandboxInstanceId": scene.effect, "runtimeGeneration": 1, "nodeId": "residual-node"}
	tx.exec(`INSERT INTO execution_work(id,run_id,kind,input,target,execution_id) VALUES($1,$2,$3,'{}',$4,$5)`, work, scene.run, kind, jsonText(target), execution)
	tx.exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
 VALUES($1,$2,$3,$4,$5,'residual-node',$1,'{}',1)`, execution, kind, scene.run, work, scene.workspace)
	result := Object{"outcome": "revision_failed", "reason": "upload_failed", "node": Object{"nodeId": "residual-node", "nodeIncarnationId": "residual-incarnation"}}
	if kind == "agent_session" {
		result["outcome"], result["reason"] = "agent_session_ended", "agent_failed"
		out := nodeExecutionResult(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "sequence": 1, "event": "late-original-terminal", "result": result}}, true)
		pendingWork, pendingExecution := newID(), "pending-rescue-"+newID()
		tx.exec(`INSERT INTO execution_work(id,run_id,kind,input,target,execution_id) VALUES($1,$2,'deliver_revision','{}',$3,$4)`, pendingWork, scene.run, jsonText(target), pendingExecution)
		tx.exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
 VALUES($1,'deliver_revision',$2,$3,$4,'residual-node',$1,'{}',1)`, pendingExecution, scene.run, pendingWork, scene.workspace)
		return out
	}
	return nodeExecutionResult(tx, &ControlRequest{Body: Object{"operationId": scene.run, "executionId": execution, "sequence": 1, "event": "late-original-terminal", "result": result}}, true)
}

func TestResidualRaceCompletionVersusNewUnclosedExecutionTargets(t *testing.T) {
	for _, kind := range []string{"agent_session", "deliver_revision"} {
		for _, factFirst := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, "/fact_first=", factFirst), func(t *testing.T) {
				scene := seedResidualScene(t, true)
				op := seedResidualCleanup(t, scene)
				complete := func(tx *transaction) Object {
					return advance(tx, &ControlRequest{}, tx.one("SELECT * FROM operations WHERE id=$1", op.S("id")))
				}
				fact := func(tx *transaction) Object { return seedResidualLateExecution(tx, scene, kind) }
				first, second := complete, fact
				if factFirst {
					first, second = fact, complete
				}
				a, b := seedResidualRace(t, scene, first, second)
				if factFirst {
					if a.err != nil {
						t.Fatal(a.err)
					}
					if b.err == nil {
						t.Fatal("completion ignored the newly committed unclosed delivery target")
					}
					if b.observed.S("reclaimState") != "reclaiming" {
						t.Fatal("completion did not reread the retained responsibility")
					}
				} else if a.err != nil || b.err != nil {
					t.Fatalf("completed responsibility did not retain late original facts: %v / %v", a.err, b.err)
				}
				seedResidualAssert(t, scene, `SELECT (r.cleared_at IS NOT NULL)=$2 AND (w.deleted_at IS NOT NULL)=$2
 AND r.responsibility=$3::jsonb AND i.phase='done' AND i.status='failed'
 AND (SELECT count(*) FROM node_executions WHERE operation_id=r.run_id AND kind=$4 AND result IS NOT NULL)>=1
 AND (SELECT count(*) FROM revisions WHERE run_id=r.run_id)=1
 AND (SELECT count(*) FROM node_executions e WHERE e.workspace_id=r.workspace_id AND e.kind='deliver_revision'
  AND NOT EXISTS(SELECT 1 FROM revision_verifications v WHERE v.execution_id=e.execution_id AND v.outcome IN ('delivered','unchanged')))>=1
 AND (SELECT count(*) FROM operations WHERE workspace_id=r.workspace_id AND kind='delete_workspace')=1
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN issue_runs i ON i.id=r.run_id WHERE r.run_id=$1`, scene.run, !factFirst, scene.responsibility, kind)
			})
		}
	}
}

func TestResidualRaceRestartRebuildsTwoStoresWithoutNewResponsibility(t *testing.T) {
	for _, resumeFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("resume_first=", resumeFirst), func(t *testing.T) {
			scene := seedResidualScene(t, true)
			op := seedResidualCleanup(t, scene)
			var reclaim, authority, targets string
			if err := scene.store.Pool.QueryRow(`SELECT reclaim_id::text,reclaim_authorization::text,reclaim_targets::text FROM agent_run_residuals WHERE run_id=$1`, scene.run).Scan(&reclaim, &authority, &targets); err != nil {
				t.Fatal(err)
			}
			storeA, storeB := seedResidualRebuildStore(scene.store), seedResidualRebuildStore(scene.store)
			resume := func(tx *transaction) Object {
				return resumeResidualReclamation(tx, tx.one("SELECT * FROM agent_run_residuals WHERE run_id=$1", scene.run))
			}
			reclaimAgain := func(tx *transaction) Object { return authorizeResidualOwnedDelete(tx, scene.run) }
			first, second := reclaimAgain, resume
			if resumeFirst {
				first, second = resume, reclaimAgain
			}
			a, b := seedResidualRaceStores(t, scene, storeA, storeB, first, second)
			if a.err != nil || b.err != nil {
				t.Fatalf("restart contenders failed: %v / %v", a.err, b.err)
			}
			if a.out.O("operation").S("id") != op.S("id") || b.out.O("operation").S("id") != op.S("id") {
				t.Fatal("a rebuilt Store substituted a second operation")
			}
			seedResidualAssert(t, scene, `SELECT reclaim_id=$2 AND reclaim_operation_id=$3 AND reclaim_authorization=$4::jsonb
 AND reclaim_targets=$5::jsonb AND responsibility=$6::jsonb AND reclaim_state='reclaiming' AND cleared_at IS NULL
 AND (SELECT count(*) FROM operations WHERE workspace_id=$7 AND kind='delete_workspace')=1
 AND (SELECT count(*) FROM external_effects WHERE operation_id=$3)=2
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, reclaim, op.S("id"), authority, targets, scene.responsibility, scene.workspace)
		})
	}
}

func TestResidualRaceAuthorizationVersusLateFactsBothCommitOrders(t *testing.T) {
	for _, factFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("fact_first=", factFirst), func(t *testing.T) {
			scene := seedResidualScene(t, true)
			authorize := func(tx *transaction) Object { return authorizeResidualOwnedDelete(tx, scene.run) }
			fact := func(tx *transaction) Object {
				return provisioningCreationFact(tx, &ControlRequest{OperationID: scene.operation, Body: Object{"outcome": "failed"}}, scene.run)
			}
			first, second := authorize, fact
			if factFirst {
				first, second = fact, authorize
			}
			a, b := seedResidualRace(t, scene, first, second)
			if factFirst {
				assertResidualFactConflict(t, a.err)
				assertResidualFactConflict(t, b.err)
				if b.observed["reclaimConflictedAt"] == nil {
					t.Fatal("authorization did not reread the committed conflict")
				}
			} else {
				if a.err != nil {
					t.Fatal(a.err)
				}
				assertResidualFactConflict(t, b.err)
				if b.observed.S("reclaimMode") != "owned_delete" {
					t.Fatal("late fact did not observe the committed authorization")
				}
			}
			seedResidualAssert(t, scene, `SELECT reclaim_conflicted_at IS NOT NULL AND responsibility=$2::jsonb AND cleared_at IS NULL
 AND (SELECT count(*) FROM operations WHERE workspace_id=$3 AND kind='delete_workspace')=$4
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=$3 AND kind='workspace_data_delete')
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, scene.responsibility, scene.workspace, map[bool]int{false: 1, true: 0}[factFirst])
		})
	}
}

func TestResidualRaceAutomaticVersusControlledClaimBothCommitOrders(t *testing.T) {
	for _, controlledFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("controlled_first=", controlledFirst), func(t *testing.T) {
			scene := seedResidualScene(t, true)
			auto := func(tx *transaction) Object { return authorizeResidualOwnedDelete(tx, scene.run) }
			intent := newID()
			controlled := func(tx *transaction) Object {
				return claimResidualReclamation(tx, scene.run, "controlled", Object{"kind": "controlled_intent", "intentId": intent, "source": "authorized-test-intent"})
			}
			first, second := auto, controlled
			mode := "owned_delete"
			count := 1
			if controlledFirst {
				first, second = controlled, auto
				mode = "controlled"
				count = 0
			}
			a, b := seedResidualRace(t, scene, first, second)
			if a.err != nil {
				t.Fatal(a.err)
			}
			fault, ok := b.err.(*Fault)
			if !ok || fault.Status != 409 || fault.Code != "residual_mode_conflict" {
				t.Fatalf("losing claimant did not respect the committed mode: %v", b.err)
			}
			if b.observed.S("reclaimId") != a.out.O("residual").S("reclaimId") {
				t.Fatal("B did not reread A's durable responsibility")
			}
			seedResidualAssert(t, scene, `SELECT reclaim_state='reclaiming' AND reclaim_mode=$2 AND reclaim_id IS NOT NULL
 AND responsibility=$3::jsonb AND cleared_at IS NULL
 AND (SELECT count(*) FROM operations WHERE workspace_id=$4 AND kind='delete_workspace')=$5
 AND NOT EXISTS(SELECT 1 FROM external_effects WHERE workspace_id=$4 AND kind IN ('sandbox_terminate','workspace_data_delete'))
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, mode, scene.responsibility, scene.workspace, count)
		})
	}
}

func TestResidualRaceCompletionVersusLateDeliveryBothCommitOrders(t *testing.T) {
	for _, factFirst := range []bool{false, true} {
		t.Run(fmt.Sprint("fact_first=", factFirst), func(t *testing.T) {
			scene := seedResidualScene(t, true)
			op := seedResidualCleanup(t, scene)
			complete := func(tx *transaction) Object {
				return advance(tx, &ControlRequest{}, tx.one("SELECT * FROM operations WHERE id=$1", op.S("id")))
			}
			late := func(tx *transaction) Object {
				return seedResidualDelivery(tx, scene, Object{"outcome": "revision_failed", "reason": "upload_failed", "node": Object{"nodeId": "residual-node", "nodeIncarnationId": "residual-incarnation"}})
			}
			first, second := complete, late
			if factFirst {
				first, second = late, complete
			}
			a, b := seedResidualRace(t, scene, first, second)
			if factFirst {
				assertResidualFactConflict(t, a.err)
				if b.err == nil || b.observed["reclaimConflictedAt"] == nil {
					t.Fatalf("completion ignored committed delivery conflict: %v", b.err)
				}
			} else {
				if a.err != nil {
					t.Fatal(a.err)
				}
				assertResidualFactConflict(t, b.err)
				if b.observed.S("reclaimState") != "cleared" {
					t.Fatal("late delivery did not reread cleared state")
				}
			}
			seedResidualAssert(t, scene, `SELECT r.reclaim_conflicted_at IS NOT NULL AND r.responsibility=$2::jsonb
 AND (r.cleared_at IS NOT NULL)=$3 AND (w.deleted_at IS NOT NULL)=$3
 AND e.result=$4::jsonb AND (SELECT count(*) FROM revisions WHERE run_id=r.run_id)=1
 AND (SELECT count(*) FROM node_event_receipts WHERE execution_id=$5)=1
 FROM agent_run_residuals r JOIN workspaces w ON w.id=r.workspace_id JOIN node_executions e ON e.execution_id=$5 WHERE r.run_id=$1`, scene.run, scene.responsibility, !factFirst, jsonText(scene.deliveryResult), scene.delivery)
		})
	}
}

func TestResidualRaceClearedReplayVersusLateFactBothCommitOrders(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		for _, factFirst := range []bool{false, true} {
			t.Run(fmt.Sprint("conflict=", conflict, "/fact_first=", factFirst), func(t *testing.T) {
				scene := seedResidualScene(t, true)
				op := seedResidualCleanup(t, scene)
				seedResidualAction(t, scene, op, "advance", nil, "")
				var before int
				if err := scene.store.Pool.QueryRow(`SELECT jsonb_array_length(reclaim_evidence) FROM agent_run_residuals WHERE run_id=$1`, scene.run).Scan(&before); err != nil {
					t.Fatal(err)
				}
				replay := func(tx *transaction) Object {
					return Object{"complete": completeResidualReclamation(tx, tx.one("SELECT * FROM operations WHERE id=$1", op.S("id")))}
				}
				fact := func(tx *transaction) Object {
					outcome := "ready"
					if conflict {
						outcome = "failed"
					}
					return provisioningCreationFact(tx, &ControlRequest{OperationID: scene.operation, Body: Object{"outcome": outcome}}, scene.run)
				}
				first, second := replay, fact
				if factFirst {
					first, second = fact, replay
				}
				a, b := seedResidualRace(t, scene, first, second)
				if !conflict {
					if a.err != nil || b.err != nil {
						t.Fatalf("cleared replay failed: %v / %v", a.err, b.err)
					}
				} else if factFirst {
					assertResidualFactConflict(t, a.err)
					assertResidualFactConflict(t, b.err)
				} else {
					if a.err != nil {
						t.Fatal(a.err)
					}
					assertResidualFactConflict(t, b.err)
				}
				if b.observed.S("reclaimState") != "cleared" {
					t.Fatal("second transaction lost the cleared identity")
				}
				seedResidualAssert(t, scene, `SELECT reclaim_state='cleared' AND disposition='released' AND cleared_at IS NOT NULL
 AND responsibility=$2::jsonb AND jsonb_array_length(reclaim_evidence)=$3 AND (reclaim_conflicted_at IS NOT NULL)=$4
 AND (SELECT count(*) FROM operations WHERE workspace_id=$5 AND kind='delete_workspace')=1
 FROM agent_run_residuals WHERE run_id=$1`, scene.run, scene.responsibility, before+map[bool]int{false: 0, true: 1}[conflict], conflict, scene.workspace)
			})
		}
	}
}
