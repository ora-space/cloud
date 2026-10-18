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
