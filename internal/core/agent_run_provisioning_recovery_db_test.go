package core

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// An abandoned operation is no longer claimable. Its unknown effects must remain independently
// discoverable after a provisional receipt, a lost response, and a replacement lease holder.
func TestPendingProvisioningEffectsSurviveReceiptsAndLeaseReplacement(t *testing.T) {
	store := commandStore(t)
	scene := seedAbandonedCreation(t, store, true)
	read := func(s *Store, epoch int) []Object {
		t.Helper()
		out, err := s.Control(context.Background(), &ControlRequest{Action: "provisioning_pending_effects", Service: scene.claims, Body: Object{"epoch": epoch}})
		if err != nil {
			t.Fatalf("recover original effect independently of failed-operation claim: %v", err)
		}
		return out["items"].([]Object)
	}
	initial := read(store, 1)
	if len(initial) != 1 || initial[0].S("operationId") != scene.operation || initial[0].O("effect").S("id") != scene.effect {
		t.Fatalf("original recovery identity missing: %v", initial)
	}
	for _, state := range []string{"absent", "running"} {
		_, err := submitFact(t, store, scene, "provisional-"+state, Object{"epoch": 1, "kind": "effect", "effectId": scene.effect, "state": state, "externalId": "substrate-original"})
		if err != nil {
			t.Fatal(err)
		}
		if got := read(store, 1); len(got) != 1 || got[0].O("effect").S("id") != scene.effect {
			t.Fatalf("%s ACK discarded the future query responsibility: %v", state, got)
		}
	}
	// A fresh Store represents process replacement, and the database is the only recovery owner.
	if _, err := store.Pool.Exec("UPDATE controller_leases SET epoch=2 WHERE name='global'"); err != nil {
		t.Fatal(err)
	}
	replacement := *store
	if got := read(&replacement, 2); len(got) != 1 || got[0].S("operationId") != scene.operation {
		t.Fatalf("replacement lost the original responsibility: %v", got)
	}
	if _, err := store.Control(context.Background(), &ControlRequest{Action: "provisioning_pending_effects", Service: scene.claims, Body: Object{"epoch": 1}}); err == nil {
		t.Fatal("stale lease read accepted")
	}
	terminal := Object{"epoch": 2, "kind": "effect", "effectId": scene.effect, "state": "succeeded", "externalId": "substrate-original", "result": Object{"sandboxInstanceId": scene.effect, "nodeId": "node-late"}}
	if _, err := submitFact(t, &replacement, scene, "terminal", terminal); err != nil {
		t.Fatal(err)
	}
	if got := read(&replacement, 2); len(got) != 0 {
		t.Fatalf("terminal ACK left a completed recovery item: %v", got)
	}
	// Lost-reply replay still refers to one fact. It never reclaims or advances the operation.
	if _, err := submitFact(t, &replacement, scene, "terminal", terminal); err != nil {
		t.Fatal(err)
	}
	var actual []string
	var status, phase, operation string
	var admission bool
	if err := store.Pool.QueryRow(`SELECT r.status,r.phase,o.state,w.admission_open FROM issue_runs r
	 JOIN workspaces w ON w.id=r.workspace_id JOIN operations o ON o.id=$2 WHERE r.id=$1`, scene.run, scene.operation).Scan(&status, &phase, &operation, &admission); err != nil {
		t.Fatal(err)
	}
	actual = []string{status, phase, operation}
	if !reflect.DeepEqual(actual, []string{"failed", "done", "failed"}) || admission {
		t.Fatalf("recovery resurrected the creation: %v admission=%v", actual, admission)
	}
}

// Paging is bounded and lossless, and only uncleared D5 creations are query responsibilities.
func TestPendingProvisioningEffectsPagingScopeAndReadOnly(t *testing.T) {
	store := commandStore(t)
	scene := seedAbandonedCreation(t, store, true)
	for _, kind := range []string{"sandbox_terminate", "workspace_data_delete"} {
		if _, err := store.Pool.Exec(`INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,result,reconciled_epoch)
		 SELECT gen_random_uuid(),operation_id,project_id,workspace_id,$2,'planned',request,'{}',1 FROM external_effects WHERE id=$1`, scene.effect, kind); err != nil {
			t.Fatal(err)
		}
	}
	read := func(body Object, service *Claims) (Object, error) {
		t.Helper()
		return store.Control(context.Background(), &ControlRequest{Action: "provisioning_pending_effects", Service: service, Body: body})
	}
	var got []string
	after := ""
	for {
		page, err := read(Object{"epoch": 1, "pageSize": 1, "afterEffectId": after}, scene.claims)
		if err != nil {
			t.Fatal(err)
		}
		items := page["items"].([]Object)
		if len(items) != 1 {
			t.Fatalf("expected one item per page: %v", page)
		}
		got = append(got, items[0].O("effect").S("id"))
		after = page.S("nextAfterEffectId")
		if after == "" {
			break
		}
	}
	if len(got) != 3 || !sort.StringsAreSorted(got) || got[0] == got[1] || got[1] == got[2] {
		t.Fatalf("cursor omitted or repeated a responsibility: %v", got)
	}
	for _, body := range []Object{{"epoch": 1, "pageSize": 101}, {"epoch": 1, "pageSize": -1}, {"epoch": 1, "afterEffectId": "bad-cursor"}} {
		if _, err := read(body, scene.claims); err == nil {
			t.Fatalf("invalid cursor/page accepted: %v", body)
		}
	}
	other := controllerClaims()
	other.Subject = "another-controller"
	if _, err := read(Object{"epoch": 1}, other); err == nil {
		t.Fatal("nonholder read accepted")
	}
	node := controllerClaims()
	node.Role = "node"
	if _, err := read(Object{"epoch": 1}, node); err == nil {
		t.Fatal("Node queried the Controller's responsibilities")
	}
	// Reads must not make the failed creation claimable or grant a new permit.
	claimed, err := store.Control(context.Background(), &ControlRequest{Action: "claim", Service: scene.claims, Body: Object{"epoch": 1}})
	if err != nil || claimed["operation"] != nil {
		t.Fatalf("read reopened a failed operation: %v %v", claimed, err)
	}
	for _, change := range []string{
		"UPDATE agent_run_residuals SET cleared_at=clock_timestamp() WHERE run_id=$1",
		"UPDATE agent_run_residuals SET cleared_at=NULL,reason='agent_start_failed' WHERE run_id=$1",
	} {
		if _, err := store.Pool.Exec(change, scene.run); err != nil {
			t.Fatal(err)
		}
		out, err := read(Object{"epoch": 1}, scene.claims)
		if err != nil || len(out["items"].([]Object)) != 0 {
			t.Fatalf("non-D5 or cleared residual leaked into recovery: %v %v", out, err)
		}
	}
}
