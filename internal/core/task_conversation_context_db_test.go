package core

import (
	"context"
	"strings"
	"testing"
)

func TestTaskConversationCarriesOnlyNodeProjectedAssistantText(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	var owner string
	if err := store.Pool.QueryRow("SELECT creator_user_id FROM issues WHERE id=$1", seed.issue).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	var snapshot Object
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		turn := newID()
		tx.exec("UPDATE issue_runs SET status='completed',phase='done' WHERE id=$1", seed.run)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,1,'system','user_turn','{"content":"original task"}',$2)`, seed.run, turn)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id,visible_text) VALUES($1,2,'node','update','{"type":"update","update":{"sessionUpdate":"agent_message_chunk"}}',$2,'The login is fixed')`, seed.run, turn)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,3,'node','update','{"type":"update","update":{"sessionUpdate":"tool_call"}}',$2)`, seed.run, turn)
		tx.exec("INSERT INTO task_conversations(issue_id,tenant_id,last_seq) VALUES($1,$2,1)", seed.issue, seed.tenant)
		tx.exec("INSERT INTO task_conversation_messages(id,tenant_id,issue_id,seq,user_id,agent_id,body,state,run_id) VALUES($1,$2,$3,1,$4,$5,'Please fix login','submitted',$6)", newID(), seed.tenant, seed.issue, owner, seed.agent, seed.run)
		snapshot = buildRunContext(tx, issue(tx, seed.tenant, seed.issue), "Now add tests", "agent", seed.agent, Object{}, nil)
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	history, ok := snapshot["conversationHistory"].([]Object)
	if !ok || len(history) != 1 || history[0].S("user") != "Please fix login" || history[0].S("assistant") != "The login is fixed" {
		t.Fatalf("frozen handoff lost the user/assistant turn: %v", snapshot["conversationHistory"])
	}
	text := renderAgentInitialTurn(snapshot, nil)
	if strings.Contains(text, "tool_call") || !strings.Contains(text, "The login is fixed") || !strings.Contains(text, "Now add tests") {
		t.Fatalf("handoff used raw tool data or lost text: %s", text)
	}
}

func TestTaskConversationSignalsMissingLegacyProjectionAndCapsPrompt(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	var snapshot Object
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("UPDATE issue_runs SET status='completed',phase='done' WHERE id=$1", seed.run)
		turn := newID()
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,1,'system','user_turn','{"content":"old"}',$2)`, seed.run, turn)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,2,'node','update','{"type":"update"}',$2)`, seed.run, turn)
		snapshot = buildRunContext(tx, issue(tx, seed.tenant, seed.issue), "new work", "agent", seed.agent, Object{}, nil)
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.B("conversationHistoryIncomplete") || snapshot["conversationHistory"] != nil {
		t.Fatalf("legacy record was falsely presented as recovered: %v", snapshot)
	}
	if len(renderAgentInitialTurn(snapshot, nil)) >= taskConversationPromptBudget {
		t.Fatal("empty historical projection exceeded the prompt budget")
	}
}

func TestTaskConversationRestoresLegacyInitialTaskWithoutInventingLaterTurns(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	var snapshot Object
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		initialTurn, laterTurn := newID(), newID()
		tx.exec(`UPDATE issue_runs SET status='completed',phase='done',input='{"task":"Original review request"}' WHERE id=$1`, seed.run)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,1,'system','user_turn','{"content":"Original review request"}',$2)`, seed.run, initialTurn)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id,visible_text) VALUES($1,2,'node','update','{"type":"update"}',$2,'Review complete')`, seed.run, initialTurn)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id,visible_text) VALUES($1,3,'node','update','{"type":"update"}',$2,'Later answer')`, seed.run, laterTurn)
		snapshot = buildRunContext(tx, issue(tx, seed.tenant, seed.issue), "Next question", "agent", seed.agent, Object{}, nil)
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	history := snapshot["conversationHistory"].([]Object)
	if len(history) != 2 || history[0].S("user") != "Original review request" || history[0].S("assistant") != "Review complete" {
		t.Fatalf("legacy initial turn was not restored: %v", history)
	}
	if history[1]["user"] != nil || !snapshot.B("conversationHistoryIncomplete") {
		t.Fatalf("later legacy turn was given an invented user message: %v", snapshot)
	}
}

func TestTaskConversationMarksAPartialTurnWhenItsProjectionWindowOverflows(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	var snapshot Object
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		turn := newID()
		tx.exec(`UPDATE issue_runs SET status='completed',phase='done',input='{"task":"Long answer"}' WHERE id=$1`, seed.run)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id) VALUES($1,1,'system','user_turn','{"content":"Long answer"}',$2)`, seed.run, turn)
		tx.exec(`INSERT INTO thread_entries(run_id,seq,source,kind,record,turn_id,visible_text)
			SELECT $1,n,'node','update','{"type":"update"}',$2,'x' FROM generate_series(2,502) n`, seed.run, turn)
		snapshot = buildRunContext(tx, issue(tx, seed.tenant, seed.issue), "new work", "agent", seed.agent, Object{}, nil)
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	history := snapshot["conversationHistory"].([]Object)
	if len(history) != 1 || len(history[0].S("assistant")) != 500 || !history[0].B("assistantTruncated") || !snapshot.B("conversationHistoryIncomplete") {
		t.Fatalf("partial assistant turn was presented as complete: %v", snapshot)
	}
}

func TestTaskConversationProjectionSurvivesReceiptReplayWithoutChangingHistory(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	record := threadRecord("update", 1, "original assistant answer")
	event := threadEvent(1, record, "")
	event["visibleText"] = "original assistant answer"
	if _, err := takeOver(t, store, scene, scene.execution, []Object{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := takeOver(t, store, scene, scene.execution, []Object{event}); err != nil {
		t.Fatalf("same event receipt must replay cleanly: %v", err)
	}
	var raw []byte
	var visible string
	if err := store.Pool.QueryRow(`SELECT record,visible_text FROM thread_entries WHERE run_id=$1 AND node_execution_id=$2 AND node_sequence=1`, scene.run, scene.execution).Scan(&raw, &visible); err != nil {
		t.Fatal(err)
	}
	if visible != "original assistant answer" || !strings.Contains(string(raw), "original assistant answer") {
		t.Fatalf("projection and unchanged raw record diverged: %q, %s", visible, raw)
	}
	changed := threadEvent(1, record, "")
	changed["visibleText"] = "different projection"
	if _, err := takeOver(t, store, scene, scene.execution, []Object{changed}); err == nil {
		t.Fatal("same receipt with different assistant projection must conflict")
	}
}
