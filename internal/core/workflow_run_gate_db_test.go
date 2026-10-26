package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// gateGraph is a snapshot graph referencing default and strong via two agent nodes.
func gateGraph() Object {
	g := wfGraph(
		[]Object{
			wfNode("s", "start", ""),
			wfAgentNode("a1", Object{"prompt": "explore", "executorSlot": "default"}),
			wfAgentNode("a2", Object{"prompt": "implement", "executorSlot": "strong"}),
			wfNode("o", "output", ""),
		},
		[][3]string{{"s", "a1", ""}, {"a1", "a2", ""}, {"a2", "o", ""}},
	)
	g["executorSlots"] = []any{wfSlot("strong", "Heavy lifting")}
	return g
}

// seedSlotGateScene loads a user (via the real /me provisioning), a tenant with membership, a
// workflow whose newest snapshot is gateGraph, plus -- when withExecutor is true -- a configured
// connection (credential + model), the personal default and a strong-slot mapping.
func seedSlotGateScene(t *testing.T, store *Store, withExecutor bool) (tid, wid, uid string) {
	t.Helper()
	_, uid = modelUser(t, store, "gate-user")
	tid, wid, sid, rid := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	graph := jsonText(gateGraph())
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		exec := func(q string, args ...any) {
			tx.exec(q, args...)
		}
		exec(`INSERT INTO tenants(id,name,status) VALUES($1,'gate','active')`, tid)
		exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tid, uid)
		exec(`INSERT INTO workflows(id,tenant_id,name,graph) VALUES($1,$2,'gate',$3::jsonb)`, wid, tid, graph)
		exec(`INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,1,'v1',$4::jsonb)`, sid, tid, wid, graph)
		exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,created_by) VALUES($1,$2,$3,$4,'gate','pending','{}'::jsonb,$5)`, rid, tid, wid, sid, uid)
		if withExecutor {
			conn := uuid.NewString()
			cred := uuid.NewString()
			exec(`INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id) VALUES($1,$2,repeat('c',64),'test-key')`, cred, uid)
			exec(`INSERT INTO personal_model_connections(id,user_id,credential_id,name,protocol,base_url,auth_mode,models,enabled) VALUES($1,$2,$4,'Personal Provider','openai-completions','https://models.example.invalid/v1','bearer',$3::jsonb,true)`, conn, uid, jsonText([]any{map[string]any{"id": "vendor/model-v1", "name": "Model V1", "contextWindow": int64(128000), "maxTokens": int64(8192)}}), cred)
			exec(`INSERT INTO personal_model_defaults(user_id,connection_id,model_id,version) VALUES($1,$2,'vendor/model-v1',1)`, uid, conn)
			exec(`INSERT INTO executor_slot_mappings(user_id,slot_name,agent_cli,connection_id,model_id,version) VALUES($1,'strong','official/ora-space.opencode',$2,'vendor/model-v1',1)`, uid, conn)
		}
		return Object{}
	}); err != nil {
		t.Fatalf("seed scene: %v", err)
	}
	return tid, wid, uid
}

// gateAndFreeze is the two halves of the run-creation gate in the order createWorkflowRun runs them:
// admission resolves every referenced slot and refuses the start if one is a gap, then freezing
// records what admission admitted. The graph comes off the snapshot, so both halves read the
// jsonb-decoded document a real run reads rather than a hand-built one.
func gateAndFreeze(t *transaction, tid, wid, runID, uid string) {
	s := admitWorkflowRun(t, tid, wid, "", uid)
	freezeWorkflowRunExecutors(t, runID, uid, s.O("graph"))
}

// With the executor configuration in place, the gate freezes one binding per referenced slot --
// including the implicit default -- carrying the frozen connection facts.
func TestWorkflowRunGateFreezesBindings(t *testing.T) {
	store := dispatcherDB(t)
	tid, wid, uid := seedSlotGateScene(t, store, true)
	runID := runIDOf(t, store, tid)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		gateAndFreeze(tx, tid, wid, runID, uid)
		return Object{}
	}); err != nil {
		t.Fatalf("gate: %v", err)
	}
	var total, defaults, owned int
	if err := store.Pool.QueryRow(`SELECT count(*), count(*) FILTER (WHERE slot_name='default'), count(*) FILTER (WHERE workflow_run_id=$1) FROM run_model_bindings WHERE workflow_run_id=$1`, runID).Scan(&total, &defaults, &owned); err != nil {
		t.Fatal(err)
	}
	if total != 2 || defaults != 1 || owned != 2 {
		t.Fatalf("bindings total=%d default=%d owned=%d; want 2/1/2", total, defaults, owned)
	}
	var connectionName, modelID string
	if err := store.Pool.QueryRow(`SELECT connection_name, model->>'id' FROM run_model_bindings WHERE workflow_run_id=$1 AND slot_name='strong'`, runID).Scan(&connectionName, &modelID); err != nil {
		t.Fatal(err)
	}
	if connectionName != "Personal Provider" || modelID != "vendor/model-v1" {
		t.Fatalf("strong binding = %q/%q", connectionName, modelID)
	}
}

// A scene with no executor configuration at all: admission refuses, every gap travels in the one
// structured fault (the slot, the nodes that depend on it and why it cannot be resolved), and the
// rollback leaves no frozen binding behind -- the run cannot survive with a slot it could not resolve.
func TestWorkflowRunGateRefusesUnresolvedSlots(t *testing.T) {
	store := dispatcherDB(t)
	tid, wid, uid := seedSlotGateScene(t, store, false)
	runID := runIDOf(t, store, tid)
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		gateAndFreeze(tx, tid, wid, runID, uid)
		return Object{}
	})
	f := ErrorCode(err)
	if f.Code != "workflow_executor_unconfigured" {
		t.Fatalf("err=%v; want workflow_executor_unconfigured", err)
	}
	slots, _ := f.Params["slots"].([]any)
	if len(slots) != 2 {
		t.Fatalf("slots = %v; want default and strong", slots)
	}
	first, _ := asWorkflowObject(slots[0])
	second, _ := asWorkflowObject(slots[1])
	if first.S("slot") != "default" || first.S("reason") != slotReasonDefaultMissing {
		t.Fatalf("default gap = %v", first)
	}
	// The unmapped named slot is the one gap a mapping fixes, and it carries the description the
	// author wrote -- the only thing the runner has to decide what to map it to.
	if second.S("slot") != "strong" || second.S("reason") != slotReasonNotConfigured || second.S("description") != "Heavy lifting" {
		t.Fatalf("strong gap = %v", second)
	}
	if nodes, _ := second["nodeIds"].([]any); len(nodes) != 1 || nodes[0] != "a2" {
		t.Fatalf("strong gap nodes = %v", second["nodeIds"])
	}
	var bindings int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM run_model_bindings WHERE workflow_run_id=$1`, runID).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("rollback left %d bindings", bindings)
	}

	// Configure only the personal default: the one remaining gap is the unmapped named slot, and the
	// default slot is no longer reported at all.
	conn := uuid.NewString()
	cred := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id) VALUES($1,$2,repeat('c',64),'test-key')`, cred, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO personal_model_connections(id,user_id,credential_id,name,protocol,base_url,auth_mode,models,enabled) VALUES($1,$2,$4,'Personal Provider','openai-completions','https://models.example.invalid/v1','bearer',$3::jsonb,true)`, conn, uid, jsonText([]any{map[string]any{"id": "vendor/model-v1", "name": "Model V1"}}), cred); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO personal_model_defaults(user_id,connection_id,model_id,version) VALUES($1,$2,'vendor/model-v1',1)`, uid, conn); err != nil {
		t.Fatal(err)
	}
	_, err = store.transact(context.Background(), func(tx *transaction) Object {
		gateAndFreeze(tx, tid, wid, runID, uid)
		return Object{}
	})
	f = ErrorCode(err)
	if f.Code != "workflow_executor_unconfigured" {
		t.Fatalf("err=%v; want workflow_executor_unconfigured", err)
	}
	slots, _ = f.Params["slots"].([]any)
	if len(slots) != 1 {
		t.Fatalf("slots = %v; want exactly the strong slot", slots)
	}
	gap, _ := asWorkflowObject(slots[0])
	if gap.S("slot") != "strong" || gap.S("reason") != slotReasonNotConfigured {
		t.Fatalf("gap = %v", gap)
	}
}

// runIDOf reads back the single run the scene created.
func runIDOf(t *testing.T, store *Store, tid string) string {
	t.Helper()
	var id string
	if err := store.Pool.QueryRow(`SELECT id FROM workflow_runs WHERE tenant_id=$1`, tid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
