package core

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Old clients must not keep the outgoing Agent busy after another target owns the queue head.
func TestTaskConversationLegacyAppendRejectsHandoffAndPause(t *testing.T) {
	for _, kind := range []string{"agent_handoff", "initiator_handoff", "paused", "failed_message"} {
		t.Run(kind, func(t *testing.T) {
			store := commandStore(t)
			scene := seedStartingRun(t, store, sessionStartInput())
			caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
			setTaskRunInitiator(t, store, scene.seed.run, uid)
			if _, err := store.Pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id)
				VALUES($1,1,'system','user_turn','{"content":"start"}',$2)`, scene.seed.run, uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='running',phase='running',thread_state='active' WHERE id=$1`, scene.seed.run); err != nil {
				t.Fatal(err)
			}
			want := "task_handoff_pending"
			switch kind {
			case "agent_handoff":
				other := uuid.NewString()
				if _, err := store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status)
					SELECT $1,space_id,tenant_id,'official/second-agent','Second','active' FROM space_agents WHERE id=$2`, other, scene.seed.agent); err != nil {
					t.Fatal(err)
				}
				sendTaskMessage(t, store, caller, scene.seed, other, "next Agent", uuid.NewString())
			case "initiator_handoff":
				other, _ := taskConversationCaller(t, store, scene.seed.tenant)
				sendTaskMessage(t, store, other, scene.seed, scene.seed.agent, "my own execution", uuid.NewString())
			case "paused":
				want = "task_paused"
				if _, err := store.Pool.Exec(`INSERT INTO task_conversations(issue_id,tenant_id,paused_run_id) VALUES($1,$2,$3)`, scene.seed.issue, scene.seed.tenant, scene.seed.run); err != nil {
					t.Fatal(err)
				}
			case "failed_message":
				want = "task_paused"
				if _, err := store.Pool.Exec(`INSERT INTO task_conversation_messages(id,tenant_id,issue_id,seq,user_id,agent_id,body,state,error_code)
					VALUES($1,$2,$3,1,$4,$5,'failed earlier input','failed','agent_unavailable')`, uuid.NewString(), scene.seed.tenant, scene.seed.issue, uid, scene.seed.agent); err != nil {
					t.Fatal(err)
				}
			}
			path := "/api/v1/tenants/" + scene.seed.tenant + "/issues/" + scene.seed.issue + "/runs/" + scene.seed.run + "/thread/messages"
			for attempt := 0; attempt < 3; attempt++ {
				_, status, err := store.Public(context.Background(), &PublicRequest{
					Method: "POST", Path: path, TenantID: scene.seed.tenant, IssueID: scene.seed.issue,
					RunID: scene.seed.run, Identity: caller, Key: uuid.NewString(),
					Body: Object{"content": []any{map[string]any{"type": "text", "text": "old client keeps appending"}}},
				})
				if err == nil || ErrorCode(err).Status != 409 || ErrorCode(err).Code != want {
					t.Fatalf("old append bypassed %s: status=%d error=%v", kind, status, err)
				}
			}
			var entries, commands int
			if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, scene.seed.run).Scan(&entries); err != nil {
				t.Fatal(err)
			}
			if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, scene.seed.run).Scan(&commands); err != nil {
				t.Fatal(err)
			}
			if entries != 1 || commands != 0 {
				t.Fatalf("rejected appends wrote durable work: entries=%d commands=%d", entries, commands)
			}
			view, _, err := store.Public(context.Background(), &PublicRequest{
				Method: "GET", Path: "/api/v1/tenants/" + scene.seed.tenant + "/issues/" + scene.seed.issue + "/runs/" + scene.seed.run + "/thread",
				TenantID: scene.seed.tenant, IssueID: scene.seed.issue, RunID: scene.seed.run, Identity: caller,
			})
			if err != nil || view.B("canAppend") {
				t.Fatalf("Thread read still advertises blocked append access: %v %v", view, err)
			}
			if kind == "paused" {
				queued := sendTaskMessage(t, store, caller, scene.seed, scene.seed.agent, "preserve input during pause", uuid.NewString())
				if queued.S("state") != "waiting" || queued["runId"] != nil {
					t.Fatalf("task send did not preserve input behind the pause: %v", queued)
				}
			}
			if kind == "agent_handoff" {
				stageIdleThread(t, store, scene.seed.run, 0)
				if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				if state := runThreadState(t, store, scene.seed.run); !state.Valid || state.String != "ending" {
					t.Fatalf("rejected late appends delayed the handoff: %v", state)
				}
			}
		})
	}
}

// A queued same-Agent message accepted before the switch still executes before the barrier.
func TestTaskConversationQueuedPrefixDrainsBeforeHandoff(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	caller, uid := taskConversationCaller(t, store, scene.seed.tenant)
	setTaskRunInitiator(t, store, scene.seed.run, uid)
	// The messages arrive while the Thread is winding up, then the existing session becomes ready.
	first := sendTaskMessage(t, store, caller, scene.seed, scene.seed.agent, "accepted before switch", uuid.NewString())
	other := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status)
		SELECT $1,space_id,tenant_id,'official/second-agent','Second','active' FROM space_agents WHERE id=$2`, other, scene.seed.agent); err != nil {
		t.Fatal(err)
	}
	second := sendTaskMessage(t, store, caller, scene.seed, other, "switch after earlier work", uuid.NewString())
	if _, err := store.Pool.Exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id)
		VALUES($1,1,'system','user_turn','{"content":"start"}',$2)`, scene.seed.run, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET status='running',phase='running',thread_state='active' WHERE id=$1`, scene.seed.run); err != nil {
		t.Fatal(err)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state, runID string
	if err := store.Pool.QueryRow(`SELECT state,run_id FROM task_conversation_messages WHERE id=$1`, first.S("id")).Scan(&state, &runID); err != nil {
		t.Fatal(err)
	}
	if state != "submitted" || runID != scene.seed.run {
		t.Fatalf("a later barrier rejected an already-accepted prefix: %s %s", state, runID)
	}
	if err := store.Pool.QueryRow(`SELECT state FROM task_conversation_messages WHERE id=$1`, second.S("id")).Scan(&state); err != nil || state != "waiting" {
		t.Fatalf("handoff overtook its accepted prefix: %s %v", state, err)
	}
}
