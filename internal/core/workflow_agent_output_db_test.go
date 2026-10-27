package core

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func workflowAnswerEvent(sequence int64, turn, text string) Object {
	return threadEvent(sequence, Object{"type": "update", "update": Object{
		"sessionUpdate": "agent_message_chunk", "content": Object{"type": "text", "text": text},
	}}, turn)
}

// The production dispatch and receipt paths must carry the rendered input into one
// attempt and publish only its final settled answer after terminal confirmation.
func TestWorkflowAgentOutputAndPrompt(t *testing.T) {
	store := dispatcherDB(t)
	output := wfNode("o", "output", "")
	output.O("data")["outputs"] = []any{Object{"name": "answer", "variableSelector": []any{"a", "output"}}}
	graph := wfGraph([]Object{wfNode("s", "start", ""), wfAgentNode("a", Object{"prompt": "Topic: {{#s.input#}}"}), output},
		[][3]string{{"s", "a", ""}, {"a", "o", ""}})
	runID, tid, uid, _, _ := seedWorkflowSessionSceneGraph(t, store, graph)
	issueID, issueRunID := uuid.NewString(), uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO issues(id,tenant_id,creator_user_id,title,number) VALUES($1,$2,$3,'output',1)`, issueID, tid, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,workflow_invocation_ref)
		VALUES($1,$2,$3,'workflow',$4,'queued',$5)`, issueRunID, tid, issueID, workflowRunWorkflowID(t, store, runID), runID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET input='{"input":"release"}' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	node, spec := startSessionNode(t, store, runID, wsID, "a")
	if got := objectsOf(spec.O("initialTurn")["content"]); !reflect.DeepEqual(got, []Object{{"text": "Topic: release"}}) {
		t.Fatalf("dispatched prompt = %#v", got)
	}
	execution := dispatchSession(t, store, runID, "a", node, spec)
	turn := spec.O("initialTurn").S("turnId")
	events := []Object{
		workflowAnswerEvent(1, "unrelated", "wrong turn"),
		workflowAnswerEvent(2, turn, "I will inspect the files"),
		workflowAnswerEvent(3, turn, "Released successfully"),
		threadEvent(4, turnEndedRecord("end_turn"), turn),
	}
	takeOverWorkflowEvents(t, store, runID, execution, events)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, before, _ := readWorkflowRun(t, store, runID)
	if before.O("a")["output"] != nil {
		t.Fatalf("published before session terminal: %#v", before)
	}
	reportSessionEndAt(t, store, runID, execution, node, "idle_timeout", 5)
	if _, _, err := store.transactWorkflowRun(context.Background(), runID, func(tx *transaction) Object {
		advanceWorkflowRun(tx, runID, store.DeliveryUnreachableAfter)
		return Object{}
	}); err != nil {
		t.Fatal(err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" || states.O("a").S("sessionEndReason") != "idle_timeout" {
		t.Fatalf("terminal = %s/%s %#v", status, runErr, states)
	}
	if got := states.O("a").O("output"); !reflect.DeepEqual(got, Object{"output": "Released successfully"}) {
		t.Fatalf("agent roots = %#v", got)
	}
	if got := states.O("o").O("output"); !reflect.DeepEqual(got, Object{"answer": "Released successfully"}) {
		t.Fatalf("output bindings = %#v", got)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, replayed, _ := readWorkflowRun(t, store, runID)
	if !reflect.DeepEqual(states, replayed) {
		t.Fatal("terminal replay rewrote the output")
	}
	var count int
	var message string
	if err := store.Pool.QueryRow(`SELECT count(*), MAX(details->>'message') FROM issue_activities WHERE issue_id=$1 AND action='run.message'`, issueID).Scan(&count, &message); err != nil {
		t.Fatal(err)
	}
	if count != 1 || message != `{"o":{"answer":"Released successfully"}}` {
		t.Fatalf("issue output message count=%d text=%q", count, message)
	}
}

func TestWorkflowAgentOutputRetryIsolation(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 1, 1))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := settleSessionWorkspace(t, store, runID)
	node, first := startSessionNode(t, store, runID, ws, "a")
	dispatchSessionAttempt(t, store, runID, "a", node, "first", first)
	turn := first.O("initialTurn").S("turnId")
	takeOverWorkflowEvents(t, store, runID, "first", []Object{
		workflowAnswerEvent(1, turn, "old answer"), threadEvent(2, turnEndedRecord("end_turn"), turn),
	})
	reportSessionEndAt(t, store, runID, "first", node, "agent_failed", 3)
	if got := readWorkflowNodeState(t, store, runID, "a"); got["output"] != nil || got.S("status") != "retrying" {
		t.Fatalf("retry published stale output: %#v", got)
	}
	makeWorkflowRetryDue(t, store, runID, "a")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := sessionWorkSpec(t, store, runID, "a")
	dispatchSessionAttempt(t, store, runID, "a", node, "second", second)
	turn = second.O("initialTurn").S("turnId")
	takeOverWorkflowEvents(t, store, runID, "second", []Object{
		workflowAnswerEvent(1, first.O("initialTurn").S("turnId"), "wrong turn"),
		workflowAnswerEvent(2, turn, "new answer"), threadEvent(3, turnEndedRecord("end_turn"), turn),
	})
	reportSessionEndAt(t, store, runID, "second", node, "idle_timeout", 4)
	got := readWorkflowNodeState(t, store, runID, "a")
	if got.S("status") != "succeeded" || got.N("attempt") != 2 || !reflect.DeepEqual(got.O("output"), Object{"output": "new answer"}) {
		t.Fatalf("retry result = %#v", got)
	}
}

func TestWorkflowAgentPromptRejectsUnresolvedTemplate(t *testing.T) {
	for _, prompt := range []string{"{{#missing.output#}}", "{{#s.input#", "{{#a.output#}}"} {
		t.Run(prompt, func(t *testing.T) {
			store := dispatcherDB(t)
			graph := wfGraph([]Object{wfNode("s", "start", ""), wfAgentNode("a", Object{"prompt": prompt}), wfNode("o", "output", "")},
				[][3]string{{"s", "a", ""}, {"a", "o", ""}})
			runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, graph)
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			ws := settleSessionWorkspace(t, store, runID)
			bindConnectedNode(t, store, ws)
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			status, reason, _, _ := readWorkflowRun(t, store, runID)
			if status != "failed" || reason != "agent_prompt_template_invalid" {
				t.Fatalf("unresolved prompt = %s/%s", status, reason)
			}
			var work int
			if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1`, runID).Scan(&work); err != nil || work != 0 {
				t.Fatalf("invalid prompt dispatched: %d, %v", work, err)
			}
		})
	}
}

func TestWorkflowAgentOutputRequiresSuccessfulTurn(t *testing.T) {
	for _, test := range []struct{ name, stop, reason string }{
		{"cancelled turn", "cancelled", "idle_timeout"},
		{"cancelled session", "end_turn", "cancelled"},
		{"failed session", "end_turn", "agent_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := dispatcherDB(t)
			runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowSessionNoRetryGraph())
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			wsID := settleSessionWorkspace(t, store, runID)
			node, spec := startSessionNode(t, store, runID, wsID, "a")
			execution := dispatchSession(t, store, runID, "a", node, spec)
			turn := spec.O("initialTurn").S("turnId")
			takeOverWorkflowEvents(t, store, runID, execution, []Object{
				workflowAnswerEvent(1, turn, "partial answer"), threadEvent(2, turnEndedRecord(test.stop), turn),
			})
			reportSessionEndAt(t, store, runID, execution, node, test.reason, 3)
			_, _, states, _ := readWorkflowRun(t, store, runID)
			if states.O("a")["output"] != nil {
				t.Fatalf("unsuccessful turn published output: %#v", states)
			}
		})
	}
}

func TestWorkflowAgentOutputUsesCompleteHistory(t *testing.T) {
	for _, test := range []struct {
		name      string
		text      string
		truncated bool
	}{
		{"projection clipped", strings.Repeat("x", 70000), false},
		{"history clipped", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := dispatcherDB(t)
			runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			ws := settleSessionWorkspace(t, store, runID)
			node, spec := startSessionNode(t, store, runID, ws, "a")
			execution := dispatchSession(t, store, runID, "a", node, spec)
			turn := spec.O("initialTurn").S("turnId")
			event := workflowAnswerEvent(1, turn, test.text)
			event["visibleText"] = "clipped preview"
			event["visibleTextTruncated"] = true
			if test.truncated {
				event["truncated"] = true
				event["record"] = jsonText(Object{"type": "update", "seq": 1})
			}
			takeOverWorkflowEvents(t, store, runID, execution, []Object{event, threadEvent(2, turnEndedRecord("end_turn"), turn)})
			reportSessionEndAt(t, store, runID, execution, node, "idle_timeout", 3)
			status, reason, states, _ := readWorkflowRun(t, store, runID)
			if test.truncated {
				if status != "failed" || reason != "agent_output_truncated" || states.O("a")["output"] != nil {
					t.Fatalf("truncated answer published: %s/%s %#v", status, reason, states)
				}
			} else if status != "succeeded" || states.O("a").O("output").S("output") != test.text {
				t.Fatal("full history was replaced by the clipped projection")
			}
		})
	}
}

func TestWorkflowAgentOutputSurvivesRunCancellation(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := settleSessionWorkspace(t, store, runID)
	node, spec := startSessionNode(t, store, runID, ws, "a")
	execution := dispatchSession(t, store, runID, "a", node, spec)
	turn := spec.O("initialTurn").S("turnId")
	takeOverWorkflowEvents(t, store, runID, execution, []Object{workflowAnswerEvent(1, turn, "completed answer"), threadEvent(2, turnEndedRecord("end_turn"), turn)})
	mustCancelWorkflowRun(t, store, tid, workflowRunWorkflowID(t, store, runID), runID, "cancel-output", cancelCaller(t, store, tid))
	reportSessionEndAt(t, store, runID, execution, node, "idle_timeout", 3)
	status, _, states, _ := readWorkflowRun(t, store, runID)
	if status != "cancelled" || states.O("a").O("output").S("output") != "completed answer" || states.O("o").S("status") == "succeeded" {
		t.Fatalf("cancel lost committed answer or ran downstream: %s %#v", status, states)
	}
}
