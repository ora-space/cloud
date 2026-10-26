package core

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A failed preparation can finish without a delivery result because no Agent session began.
// It must remain paused until the owner explicitly acknowledges the missing result.
func TestTaskConversationRecoversReleasedFailureWithoutDeliveryResult(t *testing.T) {
	for _, status := range []string{"failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			store := commandStore(t)
			seed := seedDispatchScene(t, store.Pool, false)
			caller, uid := taskConversationCaller(t, store, seed.tenant)
			setTaskRunInitiator(t, store, seed.run, uid)
			if _, err := store.Pool.Exec("UPDATE issue_runs SET status=$2,phase='done',result=NULL WHERE id=$1", seed.run, status); err != nil {
				t.Fatal(err)
			}
			message := sendTaskMessage(t, store, caller, seed, seed.agent, "retry after preparation repair", uuid.NewString())
			if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			var state string
			if err := store.Pool.QueryRow("SELECT state FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&state); err != nil || state != "waiting" {
				t.Fatalf("missing delivery silently resumed the queue: %q %v", state, err)
			}
			request := &PublicRequest{
				Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/discard",
				TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Key: uuid.NewString(),
				Body: Object{"runId": seed.run, "acknowledgeLostWork": true},
			}
			out, code, err := store.Public(context.Background(), request)
			if err != nil || code != 200 || out.S("acknowledgedRunId") != seed.run {
				t.Fatalf("explicit recovery of a preparation failure: %d %v %v", code, out, err)
			}
			var nextRun, originalStatus string
			if err := store.Pool.QueryRow("SELECT run_id FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&nextRun); err != nil || nextRun == "" || nextRun == seed.run {
				t.Fatalf("recovery did not dispatch the original queued message: %q %v", nextRun, err)
			}
			if err := store.Pool.QueryRow("SELECT status FROM issue_runs WHERE id=$1", seed.run).Scan(&originalStatus); err != nil || originalStatus != status {
				t.Fatalf("recovery rewrote the failed run: %q %v", originalStatus, err)
			}
		})
	}
}

func TestTaskConversationMissingDeliveryRetainsRecoveryGuards(t *testing.T) {
	for _, scenario := range []struct {
		name, status, phase, want string
		acknowledge, other        bool
	}{
		{"acknowledgement", "failed", "done", "lost_work_acknowledgement_required", false, false},
		{"releasing", "failed", "releasing", "run_not_resumable", true, false},
		{"completed", "completed", "done", "stale_run", true, false},
		{"another-member", "failed", "done", "model_run_end_forbidden", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store := commandStore(t)
			seed := seedDispatchScene(t, store.Pool, false)
			caller, uid := taskConversationCaller(t, store, seed.tenant)
			setTaskRunInitiator(t, store, seed.run, uid)
			if scenario.other {
				caller, _ = taskConversationCaller(t, store, seed.tenant)
			}
			if _, err := store.Pool.Exec("UPDATE issue_runs SET status=$2,phase=$3,result=NULL WHERE id=$1", seed.run, scenario.status, scenario.phase); err != nil {
				t.Fatal(err)
			}
			_, _, err := store.Public(context.Background(), &PublicRequest{
				Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/discard",
				TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Key: uuid.NewString(),
				Body: Object{"runId": seed.run, "acknowledgeLostWork": scenario.acknowledge},
			})
			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("recovery guard %s: %v; want %s", scenario.name, err, scenario.want)
			}
		})
	}
}
