package core

import (
	"context"
	"strings"
	"testing"
)

// Direct manual/comment runs use the same production dispatcher as queue-created runs.
func TestTaskAgentClaimCannotBypassUnresolvedTaskMessage(t *testing.T) {
	for _, state := range []string{"failed", "waiting"} {
		t.Run(state, func(t *testing.T) {
			store := commandStore(t)
			seed := seedDispatchScene(t, store.Pool, false)
			caller, uid := taskConversationCaller(t, store, seed.tenant)
			message := sendTaskMessage(t, store, caller, seed, seed.agent, "earlier input", newID())
			_, err := store.transact(context.Background(), func(tx *transaction) Object {
				tx.exec(`UPDATE issue_runs SET status='completed',phase='done',result='{"deliveryState":"unchanged"}' WHERE id=$1`, seed.run)
				if state == "failed" {
					tx.exec("UPDATE task_conversation_messages SET state='failed',error_code='agent_unavailable' WHERE id=$1", message.S("id"))
				}
				return Object{}
			})
			if err != nil {
				t.Fatal(err)
			}
			_, httpStatus, err := store.Public(context.Background(), &PublicRequest{
				Method: "POST", Path: "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/runs",
				TenantID: seed.tenant, IssueID: seed.issue, Identity: caller, Key: newID(),
				Body: Object{"executorType": "agent", "executorId": seed.agent, "input": Object{}},
			})
			code := "task_handoff_pending"
			if state == "failed" {
				code = "task_paused"
			}
			if err == nil || ErrorCode(err).Status != 409 || ErrorCode(err).Code != code {
				t.Fatalf("direct creation bypassed %s input: status=%d err=%v", state, httpStatus, err)
			}
			var runs int
			if err := store.Pool.QueryRow("SELECT count(*) FROM issue_runs WHERE issue_id=$1", seed.issue).Scan(&runs); err != nil || runs != 1 {
				t.Fatalf("rejected creation left a Run behind: count=%d err=%v", runs, err)
			}
			if state == "waiting" {
				return
			}
			var direct string
			_, err = store.transact(context.Background(), func(tx *transaction) Object {
				// Stage a previously admitted Run so this test exercises the claim recheck, not creation.
				direct = enqueueAdmittedRun(tx, seed.tenant, seed.issue, "agent", seed.agent, sessionStartInput(), "", nil, "user", uid).S("id")
				return Object{}
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.agentRunDispatcher().Dispatch(context.Background(), direct); err != nil {
				t.Fatal(err)
			}
			status, phase := runClaim(t, store, seed.tenant, direct)
			if status != "queued" || phase.Valid {
				t.Fatalf("direct claim bypassed %s input: status=%s phase=%v", state, status, phase)
			}
			ids, err := store.scanQueuedAgentRuns(context.Background(), 100)
			if err != nil || len(ids) != 0 {
				t.Fatalf("scan bypassed %s input: %v, %v", state, ids, err)
			}
			if _, err := store.Pool.Exec("UPDATE task_conversation_messages SET state='cancelled',error_code=NULL WHERE id=$1", message.S("id")); err != nil {
				t.Fatal(err)
			}
			if err := store.agentRunDispatcher().Dispatch(context.Background(), direct); err != nil {
				t.Fatal(err)
			}
			if status, _ := runClaim(t, store, seed.tenant, direct); status != "dispatched" {
				t.Fatalf("resolved message still blocked direct claim: %s", status)
			}
		})
	}
}

// Accepted task input must make progress ahead of a later direct Run, without blocking an older Run.
func TestTaskAgentClaimPreservesMessageAndDirectRunOrder(t *testing.T) {
	store := commandStore(t)
	seed := seedDispatchScene(t, store.Pool, false)
	caller, uid := taskConversationCaller(t, store, seed.tenant)
	// Transaction-start timestamps are not acceptance order: an input can acquire the lock
	// after a transaction that began later. Existing admitted work must still drain first.
	if _, err := store.Pool.Exec("UPDATE issue_runs SET created_at=clock_timestamp()+interval '1 hour' WHERE id=$1", seed.run); err != nil {
		t.Fatal(err)
	}
	message := sendTaskMessage(t, store, caller, seed, seed.agent, "queued handoff", newID())
	if err := store.agentRunDispatcher().Dispatch(context.Background(), seed.run); err != nil {
		t.Fatal(err)
	}
	if status, _ := runClaim(t, store, seed.tenant, seed.run); status != "dispatched" {
		t.Fatal("later waiting message blocked the earlier direct input")
	}
	// Stage the old Run's independently covered successful release boundary, not a new live execution.
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`UPDATE operations SET state='succeeded',step='done' WHERE workspace_id=(SELECT workspace_id FROM issue_runs WHERE id=$1)`, seed.run)
		tx.exec(`UPDATE workspaces SET deleted_at=clock_timestamp() WHERE issue_run_id=$1`, seed.run)
		tx.exec(`UPDATE issue_runs SET status='completed',phase='done',result='{"deliveryState":"unchanged"}' WHERE id=$1`, seed.run)
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.transact(context.Background(), func(tx *transaction) Object {
		enqueueRun(tx, seed.tenant, seed.issue, "agent", seed.agent, sessionStartInput(), "", nil, "user", uid)
		return Object{}
	})
	if err == nil || !strings.Contains(err.Error(), "task_handoff_pending") {
		t.Fatalf("later direct input was not rejected before occupying the pending-executor slot: %v", err)
	}
	if err := store.DispatchTaskConversationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.dispatchTaskMessage(context.Background(), message.S("id")); err != nil {
		t.Fatalf("dispatch accepted message: %v", err)
	}
	var queuedRun string
	if err := store.Pool.QueryRow("SELECT COALESCE(run_id::text,'') FROM task_conversation_messages WHERE id=$1", message.S("id")).Scan(&queuedRun); err != nil {
		t.Fatal(err)
	}
	if queuedRun == "" {
		t.Fatal("earlier message deadlocked behind the later direct Run")
	}
	if status, _ := runClaim(t, store, seed.tenant, queuedRun); status != "dispatched" {
		t.Fatal("message-derived Run lost the original input order")
	}
}
