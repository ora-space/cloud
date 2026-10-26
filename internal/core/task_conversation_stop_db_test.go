package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestTaskConversationManualStopPersistsPauseAcrossCompletedDelivery(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
	setTaskRunInitiator(t, store, scene.seed.run, uid)
	if _, err := store.Pool.Exec("UPDATE issue_runs SET status='running',phase='running',thread_state='active' WHERE id=$1", scene.seed.run); err != nil {
		t.Fatal(err)
	}
	otherAgent := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) SELECT $1,space_id,tenant_id,'official/stop-next','Next','active' FROM space_agents WHERE id=$2`, otherAgent, scene.seed.agent); err != nil {
		t.Fatal(err)
	}
	waiting := sendTaskMessage(t, store, caller, scene.seed, otherAgent, "keep this queued", uuid.NewString())
	path := "/api/v1/tenants/" + scene.seed.tenant + "/issues/" + scene.seed.issue + "/conversation"
	request := &PublicRequest{Method: "POST", Path: path + "/stop", TenantID: scene.seed.tenant, IssueID: scene.seed.issue, Identity: caller, Key: uuid.NewString(), Body: Object{"runId": scene.seed.run}}
	out, status, err := store.Public(context.Background(), request)
	if err != nil || status != 202 || out.S("state") != "stopping" {
		t.Fatalf("manual stop: %d %v %v", status, out, err)
	}
	var state, reason string
	if err := store.Pool.QueryRow("SELECT thread_state FROM issue_runs WHERE id=$1", scene.seed.run).Scan(&state); err != nil || state != "ending" {
		t.Fatalf("stop did not end live Thread: %s %v", state, err)
	}
	if err := store.Pool.QueryRow("SELECT body->>'reason' FROM thread_commands WHERE run_id=$1 AND kind='end_session'", scene.seed.run).Scan(&reason); err != nil || reason != "cancelled" {
		t.Fatalf("stop reason: %s %v", reason, err)
	}
	// A session may already have ended normally when the stop lands. The independent task pause
	// must survive that completed business status and the subsequent confirmed resource release.
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='completed',phase='done',result='{"deliveryState":"unchanged"}' WHERE id=$1`, scene.seed.run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec("UPDATE workspaces SET admission_open=false,deleted_at=clock_timestamp() WHERE id=$1", scene.ws); err != nil {
		t.Fatal(err)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow("SELECT state FROM task_conversation_messages WHERE id=$1", waiting.S("id")).Scan(&state); err != nil || state != "waiting" {
		t.Fatalf("manual stop silently resumed queue: %s %v", state, err)
	}
	request.Path, request.Key = path+"/resume", uuid.NewString()
	if _, status, err := store.Public(context.Background(), request); err != nil || status != 200 {
		t.Fatalf("explicit resume after saved stop: %d %v", status, err)
	}
	if err := store.Pool.QueryRow("SELECT state FROM task_conversation_messages WHERE id=$1", waiting.S("id")).Scan(&state); err != nil || state != "submitted" {
		t.Fatalf("explicit resume lost queued message: %s %v", state, err)
	}
}

func TestTaskConversationStopRejectsAnotherInitiatorAndCancelsUnclaimedRun(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	owner, uid := taskConversationCaller(t, store, seed.tenant)
	other, _ := taskConversationCaller(t, store, seed.tenant)
	setTaskRunInitiator(t, store, seed.run, uid)
	request := &PublicRequest{Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/stop", TenantID: seed.tenant, IssueID: seed.issue, Identity: other, Key: uuid.NewString(), Body: Object{"runId": seed.run}}
	if _, _, err := store.Public(context.Background(), request); err == nil {
		t.Fatal("another member stopped the model owner's run")
	}
	request.Identity, request.Key = owner, uuid.NewString()
	if _, status, err := store.Public(context.Background(), request); err != nil || status != 202 {
		t.Fatalf("stop unclaimed run: %d %v", status, err)
	}
	var state, phase string
	if err := store.Pool.QueryRow("SELECT status,phase FROM issue_runs WHERE id=$1", seed.run).Scan(&state, &phase); err != nil || state != "cancelled" || phase != "done" {
		t.Fatalf("unclaimed cancellation: %s/%s %v", state, phase, err)
	}
}

func TestTaskConversationRecoveryCannotSkipAnOlderUndeliveredRun(t *testing.T) {
	store := commandStore(t)
	first := seedDispatchScene(t, store.Pool, false)
	caller, uid := taskConversationCaller(t, store, first.tenant)
	setTaskRunInitiator(t, store, first.run, uid)
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='completed',phase='done',result='{"deliveryState":"failed"}' WHERE id=$1`, first.run); err != nil {
		t.Fatal(err)
	}
	second := seedSecondTaskAgent(t, store, first)
	path := "/api/v1/tenants/" + first.tenant + "/issues/" + first.issue + "/conversation"
	request := &PublicRequest{Method: "GET", Path: path, TenantID: first.tenant, IssueID: first.issue, Identity: caller}
	page, _, err := store.Public(context.Background(), request)
	if err != nil || page.S("blockedRunId") != first.run || page.B("canStop") {
		t.Fatalf("later queued Run hid the earlier delivery barrier: %v %v", page, err)
	}
	request.Method, request.Path, request.Key, request.Body = "POST", path+"/stop", uuid.NewString(), Object{"runId": second}
	if _, _, err := store.Public(context.Background(), request); err == nil {
		t.Fatal("stopping a newer queue item bypassed the prior delivery failure")
	}
	request.Path, request.Key, request.Body = path+"/discard", uuid.NewString(), Object{"runId": first.run, "acknowledgeLostWork": true}
	if _, status, err := store.Public(context.Background(), request); err != nil || status != 200 {
		t.Fatalf("could not explicitly recover the older barrier: %d %v", status, err)
	}
	ids, err := store.scanQueuedAgentRuns(context.Background(), 100)
	if err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatalf("explicit recovery did not release the queued direct Run: %v %v", ids, err)
	}
}
