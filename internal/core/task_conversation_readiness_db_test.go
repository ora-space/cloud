package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestTaskConversationReadinessUsesTheCurrentUsersConfiguration(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, _ := taskConversationCaller(t, store, seed.tenant)
	personal := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) SELECT $1,space_id,tenant_id,'official/ora-space.opencode','OpenCode','active' FROM space_agents WHERE id=$2`, personal, seed.agent); err != nil {
		t.Fatal(err)
	}
	request := &PublicRequest{Method: "GET", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation", TenantID: seed.tenant, IssueID: seed.issue, Identity: caller}
	page, _, err := store.Public(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, readiness := range page["agentReadiness"].([]Object) {
		if readiness.S("agentId") == personal {
			found = true
			if !readiness.B("requiresPersonalModel") || readiness.S("errorCode") != "model_default_required" {
				t.Fatalf("missing personal model was not surfaced: %v", readiness)
			}
		} else if readiness["errorCode"] != nil {
			t.Fatalf("unrelated Agent inherited another model requirement: %v", readiness)
		}
	}
	if !found {
		t.Fatal("personal-model Agent was missing from advisory state")
	}
}
