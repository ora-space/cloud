package core

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestTaskConversationConcurrentWindowsPreserveOrderAndIdempotency(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, _ := taskConversationCaller(t, store, seed.tenant)
	key := uuid.NewString()
	type result struct {
		item   Object
		status int
		err    error
	}
	results := make([]result, 12)
	var group sync.WaitGroup
	for index := range results {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			requestKey := uuid.NewString()
			body := "parallel instruction"
			if index < 2 {
				requestKey = key
				body = "same retried instruction"
			}
			out, status, err := store.Public(context.Background(), &PublicRequest{
				Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/messages",
				TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Key: requestKey,
				Body: Object{"agentId": seed.agent, "body": body},
			})
			results[index] = result{item: out.O("resource"), status: status, err: err}
		}(index)
	}
	group.Wait()
	for index, outcome := range results {
		if outcome.err != nil || outcome.status != 201 {
			t.Fatalf("window %d failed: status=%d err=%v", index, outcome.status, outcome.err)
		}
	}
	if results[0].item.S("id") != results[1].item.S("id") {
		t.Fatalf("same retry key created two messages: %v / %v", results[0], results[1])
	}
	var total, distinctSeq int
	if err := store.Pool.QueryRow(`SELECT count(*),count(DISTINCT seq) FROM task_conversation_messages WHERE issue_id=$1`, seed.issue).Scan(&total, &distinctSeq); err != nil {
		t.Fatal(err)
	}
	if total != 11 || distinctSeq != total {
		t.Fatalf("concurrent windows lost or duplicated an ordered message: total=%d distinct=%d", total, distinctSeq)
	}
}

func taskConversationCaller(t *testing.T, store *Store, tid string) (*Claims, string) {
	t.Helper()
	caller, uid := modelUser(t, store, "task-conversation-"+uuid.NewString())
	if _, err := store.Pool.Exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')", tid, uid); err != nil {
		t.Fatal(err)
	}
	return caller, uid
}

func setTaskRunInitiator(t *testing.T, store *Store, runID, userID string) {
	t.Helper()
	if _, err := store.Pool.Exec(`UPDATE issue_activities SET actor_id=$2 WHERE action='run.enqueued' AND details->>'runId'=$1`, runID, userID); err != nil {
		t.Fatal(err)
	}
}

func sendTaskMessage(t *testing.T, store *Store, caller *Claims, seed dispSeed, agentID, body, key string) Object {
	t.Helper()
	out, status, err := store.Public(context.Background(), &PublicRequest{
		Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/messages",
		TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Key: key,
		Body: Object{"agentId": agentID, "body": body},
	})
	if err != nil || status != 201 {
		t.Fatalf("send task message: status=%d err=%v", status, err)
	}
	return out.O("resource")
}

func TestTaskConversationMessageOrderIdempotencyAndCancellation(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, _ := taskConversationCaller(t, store, seed.tenant)
	key := uuid.NewString()
	first := sendTaskMessage(t, store, caller, seed, seed.agent, "first instruction", key)
	replayed := sendTaskMessage(t, store, caller, seed, seed.agent, "first instruction", key)
	if first.S("id") != replayed.S("id") || first.N("seq") != 1 || first.S("state") != "waiting" {
		t.Fatalf("retry changed the durable first message: %v / %v", first, replayed)
	}
	second := sendTaskMessage(t, store, caller, seed, seed.agent, "  second instruction\n", uuid.NewString())
	if second.N("seq") != 2 || second.S("state") != "waiting" || second.S("body") != "  second instruction\n" {
		t.Fatalf("second message overtook the queued run: %v", second)
	}
	path := "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation"
	read, status, err := store.Public(context.Background(), &PublicRequest{Method: "GET", Path: path, TenantID: seed.tenant, IssueID: seed.issue, Identity: caller})
	if err != nil || status != 200 || len(read["items"].([]Object)) != 2 || read.N("nextCursor") != 2 {
		t.Fatalf("ordered conversation read: %d, %v, %v", status, read, err)
	}
	cancel, status, err := store.Public(context.Background(), &PublicRequest{Method: "POST", Path: path + "/messages/" + first.S("id") + "/cancel", TenantID: seed.tenant, IssueID: seed.issue, ConversationMessageID: first.S("id"), Identity: caller, Key: uuid.NewString(), Body: Object{}})
	if err != nil || status != 200 || cancel.O("resource").S("state") != "cancelled" {
		t.Fatalf("cancel queued message: %d, %v, %v", status, cancel, err)
	}
	window, _, err := store.Public(context.Background(), &PublicRequest{Method: "GET", Path: path, TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, After: "1"})
	if err != nil || len(window["items"].([]Object)) != 1 || window["items"].([]Object)[0].S("id") != second.S("id") {
		t.Fatalf("forward conversation cursor: %v, %v", window, err)
	}
}

func TestTaskConversationFailedMessageHoldsFollowingMessagesUntilResolved(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, _ := taskConversationCaller(t, store, seed.tenant)
	first := sendTaskMessage(t, store, caller, seed, seed.agent, "first", uuid.NewString())
	if _, err := store.Pool.Exec("UPDATE task_conversation_messages SET state='failed',error_code='agent_unavailable' WHERE id=$1", first.S("id")); err != nil {
		t.Fatal(err)
	}
	second := sendTaskMessage(t, store, caller, seed, seed.agent, "second", uuid.NewString())
	if second.S("state") != "waiting" {
		t.Fatalf("new message skipped an unresolved failure: %v", second)
	}
	window, err := store.nextTaskMessages(context.Background())
	if err != nil || len(window) != 0 {
		t.Fatalf("scanner advanced past an unresolved failure: %v, %v", window, err)
	}
	path := "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/messages/" + first.S("id") + "/cancel"
	_, status, err := store.Public(context.Background(), &PublicRequest{Method: "POST", Path: path, TenantID: seed.tenant, IssueID: seed.issue, ConversationMessageID: first.S("id"), Identity: caller, Key: uuid.NewString(), Body: Object{}})
	if err != nil || status != 200 {
		t.Fatalf("cancel failed message: %d, %v", status, err)
	}
	window, err = store.nextTaskMessages(context.Background())
	if err != nil || len(window) != 1 || window[0] != second.S("id") {
		t.Fatalf("scanner did not resume from next message: %v, %v", window, err)
	}
}

func TestTaskConversationReusesActiveThreadWithoutMakingAnotherRun(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
	setTaskRunInitiator(t, store, scene.seed.run, uid)
	if _, err := store.Pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,1,'system','user_turn','{"content":"start"}',$2)`, scene.seed.run, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec("UPDATE issue_runs SET status='running',phase='running',thread_state='idle' WHERE id=$1", scene.seed.run); err != nil {
		t.Fatal(err)
	}
	message := sendTaskMessage(t, store, caller, scene.seed, scene.seed.agent, "continue", uuid.NewString())
	if message.S("state") != "submitted" || message.S("runId") != scene.seed.run || message.N("threadSeq") != 2 {
		t.Fatalf("active Thread was not reused: %v", message)
	}
	var runs, turns int
	if err := store.Pool.QueryRow("SELECT count(*) FROM issue_runs WHERE issue_id=$1", scene.seed.issue).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow("SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user'", scene.seed.run).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || turns != 1 {
		t.Fatalf("duplicate execution or lost user turn: runs=%d turns=%d", runs, turns)
	}
}

func TestTaskConversationMemberCannotAppendOrEndAnotherInitiatorsRun(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	otherMember, _ := taskConversationCaller(t, store, scene.seed.tenant)
	if _, err := store.Pool.Exec("UPDATE issue_runs SET status='running',phase='running',thread_state='idle' WHERE id=$1", scene.seed.run); err != nil {
		t.Fatal(err)
	}
	message := sendTaskMessage(t, store, otherMember, scene.seed, scene.seed.agent, "check this separately", uuid.NewString())
	if message.S("state") != "waiting" || message["runId"] != nil {
		t.Fatalf("another member appended to the run owner's Thread: %v", message)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.Pool.QueryRow("SELECT thread_state FROM issue_runs WHERE id=$1", scene.seed.run).Scan(&state); err != nil || state != "idle" {
		t.Fatalf("another member ended the owner session: %q, %v", state, err)
	}
	var turns int
	if err := store.Pool.QueryRow("SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user'", scene.seed.run).Scan(&turns); err != nil || turns != 0 {
		t.Fatalf("another member reached the Thread: %d, %v", turns, err)
	}
}

func TestTaskConversationWaitsForHandoffThenStartsNextAgent(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
	setTaskRunInitiator(t, store, scene.seed.run, uid)
	otherAgent := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) SELECT $1,space_id,tenant_id,'official/second-agent','Second','active' FROM space_agents WHERE id=$2`, otherAgent, scene.seed.agent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec("UPDATE issue_runs SET status='running',phase='running',thread_state='active' WHERE id=$1", scene.seed.run); err != nil {
		t.Fatal(err)
	}
	message := sendTaskMessage(t, store, caller, scene.seed, otherAgent, "check the result", uuid.NewString())
	if message.S("state") != "waiting" {
		t.Fatalf("switch must wait for the current turn: %v", message)
	}
	if _, err := store.Pool.Exec("UPDATE issue_runs SET thread_state='idle' WHERE id=$1", scene.seed.run); err != nil {
		t.Fatal(err)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.Pool.QueryRow("SELECT thread_state FROM issue_runs WHERE id=$1", scene.seed.run).Scan(&state); err != nil || state != "ending" {
		t.Fatalf("handoff did not request an orderly end: %s, %v", state, err)
	}
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='completed',phase='done',result='{"deliveryState":"unchanged"}' WHERE id=$1`, scene.seed.run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec("UPDATE workspaces SET admission_open=false,deleted_at=clock_timestamp() WHERE id=$1", scene.ws); err != nil {
		t.Fatal(err)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var nextRun string
	if err := store.Pool.QueryRow("SELECT run_id FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&nextRun); err != nil || nextRun == "" || nextRun == scene.seed.run {
		t.Fatalf("waiting message did not start its own run: %q, %v", nextRun, err)
	}
}

func TestTaskConversationPausesAfterFailureUntilExplicitResume(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, uid := taskConversationCaller(t, store, seed.tenant)
	setTaskRunInitiator(t, store, seed.run, uid)
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='failed',phase='done',result='{"deliveryState":"unchanged"}' WHERE id=$1`, seed.run); err != nil {
		t.Fatal(err)
	}
	message := sendTaskMessage(t, store, caller, seed, seed.agent, "please continue", uuid.NewString())
	if message.S("state") != "waiting" {
		t.Fatalf("a failed run must pause the next message: %v", message)
	}
	path := "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/resume"
	result, status, err := store.Public(context.Background(), &PublicRequest{Method: "POST", Path: path, TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Key: uuid.NewString(), Body: Object{"runId": seed.run}})
	if err != nil || status != 200 || result.S("acknowledgedRunId") != seed.run {
		t.Fatalf("explicit resume: %d, %v, %v", status, result, err)
	}
	var runID string
	if err := store.Pool.QueryRow("SELECT run_id FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&runID); err != nil || runID == "" {
		t.Fatalf("resume did not release the queued message: %q, %v", runID, err)
	}
}

func TestTaskConversationUndeliveredRunRequiresExplicitDiscard(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, uid := taskConversationCaller(t, store, seed.tenant)
	setTaskRunInitiator(t, store, seed.run, uid)
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='completed',phase='done',result='{"deliveryState":"failed"}' WHERE id=$1`, seed.run); err != nil {
		t.Fatal(err)
	}
	message := sendTaskMessage(t, store, caller, seed, seed.agent, "continue from saved files", uuid.NewString())
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := store.Pool.QueryRow("SELECT state FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&state); err != nil || state != "waiting" {
		t.Fatalf("delivery failure did not pause handoff: %q, %v", state, err)
	}
	path := "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/conversation/discard"
	base := &PublicRequest{Method: "POST", Path: path, TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Body: Object{"runId": seed.run}}
	base.Key = uuid.NewString()
	if _, status, err := store.Public(context.Background(), base); err == nil || !strings.Contains(err.Error(), "lost_work_acknowledgement_required") {
		t.Fatalf("missing explicit acknowledgement accepted: %d, %v", status, err)
	}
	base.Key = uuid.NewString()
	base.Body["acknowledgeLostWork"] = true
	if _, status, err := store.Public(context.Background(), base); err != nil || status != 200 {
		t.Fatalf("explicitly discard failed result: %d, %v", status, err)
	}
	var nextRun string
	if err := store.Pool.QueryRow("SELECT run_id FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&nextRun); err != nil || nextRun == "" || nextRun == seed.run {
		t.Fatalf("explicit recovery did not continue waiting task: %q, %v", nextRun, err)
	}
}

func TestTaskConversationScanPassesPausedTasksInsteadOfStarvingLaterWork(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	var owner string
	if err := store.Pool.QueryRow("SELECT creator_user_id FROM issues WHERE id=$1", seed.issue).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO issues(id,tenant_id,creator_user_id,project_ref,title,number)
		SELECT gen_random_uuid(),$1,$2,$3,'scan task',n+1 FROM generate_series(1,101) n`, seed.tenant, owner, seed.project); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO task_conversation_messages(id,tenant_id,issue_id,seq,user_id,agent_id,body,state,created_at)
		SELECT gen_random_uuid(),$1,id,1,$2,$3,'resume this task','waiting',clock_timestamp()-interval '1 hour'+number*interval '1 second'
		FROM issues WHERE tenant_id=$1 AND number BETWEEN 2 AND 102`, seed.tenant, owner, seed.agent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,phase,result,created_at)
		SELECT gen_random_uuid(),$1,id,'agent',$2,'failed','done','{"deliveryState":"failed"}',clock_timestamp()-interval '2 hours'
		FROM issues WHERE tenant_id=$1 AND number BETWEEN 2 AND 101`, seed.tenant, seed.agent); err != nil {
		t.Fatal(err)
	}
	first, err := store.nextTaskMessages(context.Background())
	if err != nil || len(first) != taskConversationBatchSize {
		t.Fatalf("first bounded window: %d, %v", len(first), err)
	}
	second, err := store.nextTaskMessages(context.Background())
	if err != nil || len(second) != 1 {
		t.Fatalf("paused first window starved a later task: %v, %v", second, err)
	}
	var issueNumber int
	if err := store.Pool.QueryRow(`SELECT i.number FROM task_conversation_messages m JOIN issues i ON i.id=m.issue_id WHERE m.id=$1`, second[0]).Scan(&issueNumber); err != nil || issueNumber != 102 {
		t.Fatalf("second window did not reach the later task: %d, %v", issueNumber, err)
	}
	third, err := store.nextTaskMessages(context.Background())
	if err != nil || len(third) != taskConversationBatchSize {
		t.Fatalf("scanner did not wrap for newly unblocked old tasks: %d, %v", len(third), err)
	}
}
