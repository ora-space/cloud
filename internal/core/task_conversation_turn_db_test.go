package core

import (
	"context"
	"testing"
)

// Public responses, rather than database rows, must carry the Node's actual turn association.
func TestTaskConversationPublicNodeEntryRetainsTurnIdentity(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(1, threadRecord("update", 0, "answer"), scene.initialTurnID),
	}); err != nil {
		t.Fatal(err)
	}
	caller, _ := taskConversationCaller(t, store, scene.seed.seed.tenant)
	out, status, err := store.Public(context.Background(), &PublicRequest{
		Method: "GET", Path: "/api/v1/tenants/" + scene.seed.seed.tenant + "/issues/" + scene.seed.seed.issue + "/runs/" + scene.run + "/thread",
		TenantID: scene.seed.seed.tenant, IssueID: scene.seed.seed.issue, RunID: scene.run, Identity: caller,
	})
	if err != nil || status != 200 {
		t.Fatalf("public Thread: status=%d err=%v", status, err)
	}
	for _, row := range out["items"].([]Object) {
		if row.S("source") == "node" {
			if row.S("turnId") != scene.initialTurnID || row["status"] != nil {
				t.Fatalf("public Node entry lost its turn or gained user delivery status: %v", row)
			}
			return
		}
	}
	t.Fatal("public Thread lost the actual Node entry")
}
