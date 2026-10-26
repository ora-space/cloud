package integration

import (
	"context"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// admitScheduledRun admits one occurrence for a fresh task and returns the task and run IDs.
func admitScheduledRun(t *testing.T, store *core.Store, s skeletonSeed, agentID, name string) (string, string) {
	t.Helper()
	taskID := seedEnabledScheduledTask(t, store, s, agentID, name, 65)
	must(t, store.EvaluateScheduledTasksOnce(context.Background()))
	var runID string
	must(t, store.Pool.QueryRow(`SELECT run_id FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&runID))
	if runID == "" {
		t.Fatal("admission must create a run")
	}
	return taskID, runID
}

// expireScheduledDeadline moves the persisted deadline into the past so the next writer pass sees it.
func expireScheduledDeadline(t *testing.T, store *core.Store, taskID string) {
	t.Helper()
	_, err := store.Pool.Exec(`UPDATE scheduled_task_occurrences SET deadline_at = now() - interval '1 minute' WHERE task_id=$1`, taskID)
	must(t, err)
}

// deadlineFacts reports the run's cancel request and the occurrence's deadline fact as text (empty
// when absent), so a test can prove a pass left both untouched.
func deadlineFacts(t *testing.T, store *core.Store, taskID string) (cancelRequested, deadlineCancelled string) {
	t.Helper()
	must(t, store.Pool.QueryRow(`SELECT COALESCE(r.cancel_requested_at::text,''), COALESCE(o.deadline_cancelled_at::text,'')
		FROM issue_runs r JOIN scheduled_task_occurrences o ON o.run_id=r.id WHERE o.task_id=$1`, taskID).Scan(&cancelRequested, &deadlineCancelled))
	return cancelRequested, deadlineCancelled
}

// scheduledOccurrenceIdentity is the occurrence's admission identity: which run, at which task revision.
type scheduledOccurrenceIdentity struct {
	scheduledAt, runID, outcome string
	taskRevision                int64
}

func readScheduledOccurrenceIdentity(t *testing.T, store *core.Store, taskID string) scheduledOccurrenceIdentity {
	t.Helper()
	var identity scheduledOccurrenceIdentity
	must(t, store.Pool.QueryRow(`SELECT scheduled_at::text, COALESCE(run_id::text,''), outcome, task_revision
		FROM scheduled_task_occurrences WHERE task_id=$1`, taskID).Scan(&identity.scheduledAt, &identity.runID, &identity.outcome, &identity.taskRevision))
	return identity
}

// TestScheduledTaskHistoryProjectsTimeoutOnlyForDeadlineCancelledRuns pins the D4 projection: history
// shows `failed · timeout` when, and only when, the deadline fact exists AND the IssueRun ended
// `cancelled`. Every other combination projects the run's own terminal state; in particular a user
// cancel (cancel_requested_at without the deadline fact) is never reinterpreted as a timeout.
func TestScheduledTaskHistoryProjectsTimeoutOnlyForDeadlineCancelledRuns(t *testing.T) {
	cases := []struct {
		name          string
		deadlineFact  bool
		cancelRequest bool
		status        string
		failureReason string
		wantOutcome   string
		wantReason    string
	}{
		{name: "deadline fact and cancelled", deadlineFact: true, cancelRequest: true, status: "cancelled", wantOutcome: "failed", wantReason: "timeout"},
		{name: "deadline fact but completed", deadlineFact: true, cancelRequest: true, status: "completed", wantOutcome: "succeeded"},
		{name: "deadline fact but failed", deadlineFact: true, cancelRequest: true, status: "failed", failureReason: "agent_failed", wantOutcome: "failed", wantReason: "agent_failed"},
		{name: "deadline fact but not terminal", deadlineFact: true, cancelRequest: true, status: "running", wantOutcome: "running"},
		{name: "user cancel without deadline fact", cancelRequest: true, status: "cancelled", wantOutcome: "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			taskID, runID := admitScheduledRun(t, store, s, agentID, "Projection "+tc.name)
			if tc.deadlineFact {
				_, err := store.Pool.Exec(`UPDATE scheduled_task_occurrences SET deadline_cancelled_at=now() WHERE task_id=$1`, taskID)
				must(t, err)
			}
			if tc.cancelRequest {
				_, err := store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1`, runID)
				must(t, err)
			}
			_, err := store.Pool.Exec(`UPDATE issue_runs SET status=$1, failure_reason=$2 WHERE id=$3`, tc.status, tc.failureReason, runID)
			must(t, err)

			caller := scheduledTaskCaller(t, store, s.tenantID)
			out, status, err := store.Public(context.Background(), &core.PublicRequest{
				Method: "GET", Key: "projection-history",
				Path:     "/api/v1/tenants/" + s.tenantID + "/scheduled-tasks/" + taskID + "/history",
				TenantID: s.tenantID, ScheduledTaskID: taskID, Identity: caller,
			})
			must(t, err)
			entries, ok := out["entries"].([]core.Object)
			if status != 200 || !ok || len(entries) != 1 {
				t.Fatalf("history = %d %v, want one entry", status, out)
			}
			entry := entries[0]
			if entry.S("outcome") != tc.wantOutcome || entry.S("failureReason") != tc.wantReason {
				t.Fatalf("projection = outcome %q reason %q, want outcome %q reason %q (entry %v)",
					entry.S("outcome"), entry.S("failureReason"), tc.wantOutcome, tc.wantReason, entry)
			}
			if entry.S("runId") != runID {
				t.Fatalf("projection must keep the run identity, got %q want %q", entry.S("runId"), runID)
			}
		})
	}
}

// TestScheduledTaskDeadlineWriterSkipsUserCancelledRun pins the D4 race with a user cancel: a cancel
// request recorded before the deadline means the writer adds no deadline fact and leaves the user's
// cancel request exactly as written, so history keeps showing a plain cancellation.
func TestScheduledTaskDeadlineWriterSkipsUserCancelledRun(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID, runID := admitScheduledRun(t, store, s, agentID, "User cancel first")
	_, err := store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now() - interval '5 minutes' WHERE id=$1`, runID)
	must(t, err)
	expireScheduledDeadline(t, store, taskID)
	before, _ := deadlineFacts(t, store, taskID)

	must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))

	after, fact := deadlineFacts(t, store, taskID)
	if fact != "" {
		t.Fatalf("a user cancel before the deadline must not produce a deadline fact, got %q", fact)
	}
	if after != before {
		t.Fatalf("the deadline pass must not rewrite the user's cancel request, got %q -> %q", before, after)
	}
}

// TestScheduledTaskDeadlineWriterSkipsEndingOrEndedThread pins the D4 session boundary: once the
// session has entered `ending` or `ended`, an expired deadline writes neither the cancel request nor
// the deadline fact, even though the IssueRun has not reached its terminal state yet.
func TestScheduledTaskDeadlineWriterSkipsEndingOrEndedThread(t *testing.T) {
	for _, state := range []string{"ending", "ended"} {
		t.Run(state, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			taskID, runID := admitScheduledRun(t, store, s, agentID, "Thread "+state)
			_, err := store.Pool.Exec(`UPDATE issue_runs SET status='running', phase='running', thread_state=$1 WHERE id=$2`, state, runID)
			must(t, err)
			expireScheduledDeadline(t, store, taskID)

			must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))

			if cancel, fact := deadlineFacts(t, store, taskID); cancel != "" || fact != "" {
				t.Fatalf("a %s session must not receive a deadline cancel, got cancel=%q fact=%q", state, cancel, fact)
			}
		})
	}
}

// TestScheduledTaskAutoDisableKeepsIdentityBinding pins D1: automatic disabling only flips the
// enabled state and records the reason. The creator, bound issue, agent and definition stay exactly
// as saved, so nothing is transferred or re-bound.
func TestScheduledTaskAutoDisableKeepsIdentityBinding(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		invalidate func(t *testing.T, store *core.Store, s skeletonSeed)
	}{
		{name: "creator inactive", reason: "creator_inactive", invalidate: func(t *testing.T, store *core.Store, s skeletonSeed) {
			_, err := store.Pool.Exec(`
				WITH backup AS (
					INSERT INTO users(id, display_name, status)
					VALUES(gen_random_uuid(), 'Scheduled-task backup administrator', 'active')
					RETURNING id
				)
				INSERT INTO tenant_memberships(tenant_id, user_id, role, status)
				SELECT $1, id, 'admin', 'active' FROM backup`, s.tenantID)
			must(t, err)
			_, err = store.Pool.Exec(`UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, s.tenantID, s.userID)
			must(t, err)
		}},
		{name: "issue deleted", reason: "issue_deleted", invalidate: func(t *testing.T, store *core.Store, s skeletonSeed) {
			_, err := store.Pool.Exec(`UPDATE issues SET deleted_at=now() WHERE id=$1`, s.issueID)
			must(t, err)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, s, agentID := scheduledTaskStore(t)
			taskID := seedEnabledScheduledTask(t, store, s, agentID, "Binding "+tc.name, 65)
			before := readScheduledTaskState(t, store, taskID)
			tc.invalidate(t, store, s)

			must(t, store.EvaluateScheduledTasksOnce(context.Background()))

			after := readScheduledTaskState(t, store, taskID)
			if after.enabled || after.disabledReason != tc.reason {
				t.Fatalf("task must auto-disable with %q, got enabled=%v reason=%q", tc.reason, after.enabled, after.disabledReason)
			}
			if after.issueID != before.issueID || after.creatorUserID != before.creatorUserID || after.agentID != before.agentID {
				t.Fatalf("auto-disable must keep the identity binding: before=%+v after=%+v", before, after)
			}
			if after.name != before.name || after.prompt != before.prompt || after.schedule != before.schedule ||
				after.timezone != before.timezone || after.revision != before.revision {
				t.Fatalf("auto-disable must not rewrite the definition: before=%+v after=%+v", before, after)
			}
		})
	}
}

// TestScheduledTaskDeadlineWriteKeepsIdentityBinding pins D4: the deadline writer only adds the
// cancel request and the deadline fact. The task's binding and the occurrence's admission identity
// (due point, run, task revision, outcome) are not touched.
func TestScheduledTaskDeadlineWriteKeepsIdentityBinding(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID, _ := admitScheduledRun(t, store, s, agentID, "Deadline binding")
	taskBefore := readScheduledTaskState(t, store, taskID)
	occurrenceBefore := readScheduledOccurrenceIdentity(t, store, taskID)
	expireScheduledDeadline(t, store, taskID)

	must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))

	if cancel, fact := deadlineFacts(t, store, taskID); cancel == "" || fact == "" {
		t.Fatalf("precondition: the deadline pass must write both facts, got cancel=%q fact=%q", cancel, fact)
	}
	if taskAfter := readScheduledTaskState(t, store, taskID); taskAfter != taskBefore {
		t.Fatalf("the deadline write must not touch the task: before=%+v after=%+v", taskBefore, taskAfter)
	}
	if occurrenceAfter := readScheduledOccurrenceIdentity(t, store, taskID); occurrenceAfter != occurrenceBefore {
		t.Fatalf("the deadline write must keep the occurrence identity: before=%+v after=%+v", occurrenceBefore, occurrenceAfter)
	}
}

// TestScheduledTaskDeadlineWriteRollsBackAsOneTransaction pins D4 atomicity: the cancel request and
// the deadline fact commit together or not at all. A failure writing the fact must roll back the
// cancel request, and the next pass, once the failure is gone, writes both.
func TestScheduledTaskDeadlineWriteRollsBackAsOneTransaction(t *testing.T) {
	store, s, agentID := scheduledTaskStore(t)
	taskID, _ := admitScheduledRun(t, store, s, agentID, "Deadline rollback")
	expireScheduledDeadline(t, store, taskID)
	// Fail the second statement of the writer's transaction, after the cancel request was written.
	_, err := store.Pool.Exec(`
		CREATE FUNCTION scheduled_deadline_fact_fault() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected deadline fact failure';
		END $$;
		CREATE TRIGGER scheduled_deadline_fact_fault
			BEFORE UPDATE OF deadline_cancelled_at ON scheduled_task_occurrences
			FOR EACH ROW EXECUTE FUNCTION scheduled_deadline_fact_fault();`)
	must(t, err)

	if err := store.WriteScheduledTaskDeadlinesOnce(context.Background()); err == nil {
		t.Fatal("the injected failure must surface to the writer loop")
	}
	if cancel, fact := deadlineFacts(t, store, taskID); cancel != "" || fact != "" {
		t.Fatalf("a failed deadline write must leave neither fact behind, got cancel=%q fact=%q", cancel, fact)
	}

	_, err = store.Pool.Exec(`DROP TRIGGER scheduled_deadline_fact_fault ON scheduled_task_occurrences`)
	must(t, err)
	must(t, store.WriteScheduledTaskDeadlinesOnce(context.Background()))
	if cancel, fact := deadlineFacts(t, store, taskID); cancel == "" || fact == "" {
		t.Fatalf("the next pass must write both facts once the failure is gone, got cancel=%q fact=%q", cancel, fact)
	}
}
