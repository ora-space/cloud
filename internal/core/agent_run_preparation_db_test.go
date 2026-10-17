package core

import (
	"context"
	"testing"
)

// syncErrorCode is filled only when the clone step ended with repository_sync_failed, and then
// from the Binding's latest failed sync.
func TestPreparationSyncErrorCodeNamesTheFailedSync(t *testing.T) {
	s := commandStore(t)
	seed := seedDispatchScene(t, s.Pool, false)
	var workspace, binding string
	if err := s.Pool.QueryRow("SELECT w.id,b.id FROM workspaces w JOIN repository_bindings b ON b.project_id=w.project_id WHERE w.project_id=$1", seed.project).Scan(&workspace, &binding); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,error_code,finished_at) VALUES(gen_random_uuid(),'repository_sync',$1,$2,$3,'failed','test','upstream_unreachable',clock_timestamp()-interval '1 minute')",
		"INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,error_code,finished_at) VALUES(gen_random_uuid(),'repository_sync',$1,$2,$3,'failed','test','credential_rejected',clock_timestamp())",
	} {
		if _, err := s.Pool.Exec(q, seed.tenant, seed.project, binding); err != nil {
			t.Fatal(err)
		}
	}
	run := Object{"workspaceId": workspace}
	for _, c := range []struct {
		errorCode string
		want      any
	}{
		{"", nil},
		{"clone_failed", nil},
		{repositorySyncFailed, "credential_rejected"},
	} {
		var got any
		if err := s.view(context.Background(), func(tx *transaction) {
			got = preparationSyncErrorCode(tx, run, Object{"errorCode": c.errorCode})
		}); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("errorCode %q: syncErrorCode = %v, want %v", c.errorCode, got, c.want)
		}
	}
}

// Operation D2: a run whose Workspace waits for a snapshot shows the wait, and one whose clone step
// failed on the Binding's sync shows that failure and the exact sync's code it recorded.
func TestPreparationShowsSnapshotWaitsAndSyncFailures(t *testing.T) {
	s := commandStore(t)
	seed := seedDispatchScene(t, s.Pool, false)
	var workspace string
	if err := s.Pool.QueryRow("SELECT id FROM workspaces WHERE project_id=$1 ORDER BY created_at LIMIT 1", seed.project).Scan(&workspace); err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := s.Pool.QueryRow("SELECT user_id FROM tenant_memberships WHERE tenant_id=$1 LIMIT 1", seed.tenant).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	operation := "11111111-1111-4111-8111-111111111111"
	if _, err := s.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,error_code,retry_at)
	 VALUES($1,$2,$3,$4,$5,'create_workspace','retry_wait','clone','{}','prep','prep','snapshot_not_ready',now())`, operation, seed.tenant, actor, seed.project, workspace); err != nil {
		t.Fatal(err)
	}
	run := Object{"workspaceId": workspace, "phase": "provisioning", "status": "dispatched", "executorType": "agent"}
	read := func() Object {
		t.Helper()
		var out Object
		if err := s.view(context.Background(), func(tx *transaction) {
			out = agentRunPreparation(tx, run)
			out["syncErrorCode"] = preparationSyncErrorCode(tx, run, out)
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := read(); got.S("stage") != "clone" || got.S("errorCode") != snapshotNotReady || got["syncErrorCode"] != nil {
		t.Fatalf("a waiting clone step = %v, want stage clone with snapshot_not_ready", got)
	}
	if _, err := s.Pool.Exec(`UPDATE operations SET state='failed',error_code='repository_sync_failed',retry_at=NULL,result='{"syncOperationId":"x","syncErrorCode":"branch_not_found"}' WHERE id=$1`, operation); err != nil {
		t.Fatal(err)
	}
	run["status"] = "failed"
	if got := read(); got.S("stage") != "failed" || got.S("errorCode") != repositorySyncFailed || got["syncErrorCode"] != "branch_not_found" {
		t.Fatalf("a sync failure = %v, want failed repository_sync_failed with the recorded code", got)
	}
}
