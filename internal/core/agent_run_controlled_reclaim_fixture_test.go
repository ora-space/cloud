package core

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

var (
	controlledPrepared *Store
	controlledCleanup  func() error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if controlledCleanup != nil {
		if err := controlledCleanup(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	if err := closeCoreFixtureTemplate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// Controlled tests are serial and share only a migrated database copy. Cleanup belongs to TestMain,
// not the first test. Every leaf starts empty so schema-wide claims/scans remain isolated.
func controlledStores(t *testing.T) func(*testing.T) *Store {
	t.Helper()
	if controlledPrepared == nil {
		controlledPrepared = dispatcherDBWithCleanup(t, func(cleanup func() error) { controlledCleanup = cleanup })
	}
	prepared := controlledPrepared
	var tables string
	if err := prepared.Pool.QueryRow(`SELECT string_agg(format('%I.%I',schemaname,tablename), ',' ORDER BY tablename)
 FROM pg_tables WHERE schemaname=current_schema()
 AND tablename NOT IN ('schema_migrations','task_conversation_scan_state')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	reset := "TRUNCATE " + tables
	return func(t *testing.T) *Store {
		t.Helper()
		if _, err := prepared.Pool.Exec(reset); err != nil {
			t.Fatal(err)
		}
		// 0046's scan cursor is the only non-migration row in an empty migrated schema.
		if _, err := prepared.Pool.Exec("UPDATE task_conversation_scan_state SET last_created_at=NULL,last_id=NULL"); err != nil {
			t.Fatal(err)
		}
		store := &Store{Pool: prepared.Pool, Signals: NewControlHub()}
		BindBusinessHooks(store)
		return store
	}
}

// Keep #112's scene helper unchanged; only this controlled adapter supplies an already migrated
// Store. Its original provisioning, delivery and runtime evidence still enters through the same helpers.
func seedControlledScene(t *testing.T, s *Store, ready bool) residualReclaimScene {
	t.Helper()
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
