package core

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Task-level continuity keeps the established live Thread idle window and its Workspace binding.
func TestTaskConversationReusesWorkspaceInsideExistingIdleWindow(t *testing.T) {
	store := commandStore(t)
	store.ThreadIdleTimeout = 15 * time.Minute
	scene := seedStartingRun(t, store, sessionStartInput())
	caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
	setTaskRunInitiator(t, store, scene.seed.run, uid)
	if _, err := store.Pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id)
		VALUES($1,1,'system','user_turn','{"content":"initial request"}',$2)`, scene.seed.run, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='running',phase='running' WHERE id=$1`, scene.seed.run); err != nil {
		t.Fatal(err)
	}
	stageIdleThread(t, store, scene.seed.run, time.Minute)
	idlePass(t, store)
	message := sendTaskMessage(t, store, caller, scene.seed, scene.seed.agent, "continue in this session", uuid.NewString())
	if message.S("state") != "submitted" || message.S("runId") != scene.seed.run {
		t.Fatalf("live conversation created a replacement run: %v", message)
	}
	var workspace, state string
	var total int
	if err := store.Pool.QueryRow(`SELECT workspace_id,thread_state FROM issue_runs WHERE id=$1`, scene.seed.run).Scan(&workspace, &state); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE issue_id=$1`, scene.seed.issue).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if workspace != scene.ws || state != "active" || total != 1 || len(endSessionRequests(t, store, scene.seed.run)) != 0 {
		t.Fatalf("reply changed the live lifecycle: workspace=%s state=%s runs=%d", workspace, state, total)
	}
}

// An expired Thread stays terminal; subsequent conversation waits for delivery and confirmed
// Workspace release, then receives a fresh execution and the projected prior conversation.
func TestTaskConversationIdleExpiryWaitsForReleaseBeforeFreshRun(t *testing.T) {
	store := commandStore(t)
	store.ThreadIdleTimeout = 15 * time.Minute
	scene := seedStartingRun(t, store, sessionStartInput())
	caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
	setTaskRunInitiator(t, store, scene.seed.run, uid)
	turn := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id)
		VALUES($1,1,'system','user_turn','{"content":"initial request"}',$2)`, scene.seed.run, turn); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id,visible_text)
		VALUES($1,2,'node','update','{"type":"update"}',$2,'Previous result')`, scene.seed.run, turn); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='running',phase='running' WHERE id=$1`, scene.seed.run); err != nil {
		t.Fatal(err)
	}
	stageIdleThread(t, store, scene.seed.run, 20*time.Minute)
	idlePass(t, store)
	if reasons := endSessionRequests(t, store, scene.seed.run); len(reasons) != 1 || reasons[0] != "idle_timeout" {
		t.Fatalf("task conversation disabled the existing idle timeout: %v", reasons)
	}
	message := sendTaskMessage(t, store, caller, scene.seed, scene.seed.agent, "continue after expiry", uuid.NewString())
	for _, stage := range []struct{ phase, thread, status string }{
		{"running", "ending", "running"},
		{"delivering", "ended", "running"},
		{"releasing", "ended", "completed"},
		{"done", "ended", "completed"},
	} {
		if _, err := store.Pool.Exec(`UPDATE issue_runs SET phase=$2,thread_state=$3,status=$4,
			result='{"deliveryState":"unchanged"}' WHERE id=$1`, scene.seed.run, stage.phase, stage.thread, stage.status); err != nil {
			t.Fatal(err)
		}
		if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		var state string
		var runs int
		if err := store.Pool.QueryRow(`SELECT state FROM task_conversation_messages WHERE id=$1`, message.S("id")).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if err := store.Pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE issue_id=$1`, scene.seed.issue).Scan(&runs); err != nil {
			t.Fatal(err)
		}
		if state != "waiting" || runs != 1 {
			t.Fatalf("phase %s bypassed the old Workspace release: message=%s runs=%d", stage.phase, state, runs)
		}
	}
	// Stage the release evidence owned by the existing operation path. This test exercises task
	// scheduling around that boundary, not a replacement implementation of sandbox deletion.
	if _, err := store.Pool.Exec(`UPDATE workspaces SET admission_open=false,deleted_at=clock_timestamp() WHERE id=$1`, scene.ws); err != nil {
		t.Fatal(err)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var runID, workspaceID, oldThread string
	var input []byte
	if err := store.Pool.QueryRow(`SELECT r.id,r.workspace_id,r.input FROM task_conversation_messages m
		JOIN issue_runs r ON r.id=m.run_id WHERE m.id=$1`, message.S("id")).Scan(&runID, &workspaceID, &input); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(`SELECT thread_state FROM issue_runs WHERE id=$1`, scene.seed.run).Scan(&oldThread); err != nil {
		t.Fatal(err)
	}
	if runID == scene.seed.run || workspaceID == scene.ws || oldThread != "ended" {
		t.Fatalf("continuation reused an ended execution: run=%s workspace=%s oldThread=%s", runID, workspaceID, oldThread)
	}
	history := objectsOf(mustObject(t, input)["conversationHistory"])
	if len(history) != 1 || history[0].S("assistant") != "Previous result" || history[0].S("user") != "Fix the auth flow" {
		t.Fatalf("fresh execution did not carry projected conversation: %v", history)
	}
}
