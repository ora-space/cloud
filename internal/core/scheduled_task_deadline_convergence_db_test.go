package core

import (
	"context"
	"testing"
)

// TestScheduledDeadlineConvergesAfterANonSuccessfulTurn pins scheduled-task D3/D4 together: a turn
// that ends `cancelled` is not end evidence, so the headless session stays alive with no
// EndSession{idle_timeout}; the 60-minute deadline is what converges it. The deadline writer records
// the cancel request and the deadline fact, and the existing cancel reaction then ends the Thread with
// one EndSession{cancelled} instead of the scheduler forging a terminal state.
func TestScheduledDeadlineConvergesAfterANonSuccessfulTurn(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	markScheduledThread(t, store, scene.run)
	ended := threadRecord("turnEnded", 1, "")
	ended["stopReason"] = "cancelled"
	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(1, threadRecord("update", 0, "a"), ""),
		threadEvent(2, ended, ""),
	}); err != nil {
		t.Fatalf("take over the non-successful turn: %v", err)
	}

	idlePass(t, store)
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "active" {
		t.Fatalf("a non-successful turn must leave the scheduled session alive, got %v", state)
	}
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 0 {
		t.Fatalf("a non-successful turn must not end the scheduled session, got EndSession %v", reasons)
	}

	if _, err := store.Pool.Exec(`UPDATE scheduled_task_occurrences SET deadline_at = now() - interval '1 minute' WHERE run_id=$1`, scene.run); err != nil {
		t.Fatalf("expire the deadline: %v", err)
	}
	if err := store.WriteScheduledTaskDeadlinesOnce(context.Background()); err != nil {
		t.Fatalf("deadline pass: %v", err)
	}
	var cancelRequested, deadlineFact bool
	if err := store.Pool.QueryRow(`SELECT r.cancel_requested_at IS NOT NULL, o.deadline_cancelled_at IS NOT NULL
		FROM issue_runs r JOIN scheduled_task_occurrences o ON o.run_id=r.id WHERE r.id=$1`, scene.run).Scan(&cancelRequested, &deadlineFact); err != nil {
		t.Fatalf("read the deadline facts: %v", err)
	}
	if !cancelRequested || !deadlineFact {
		t.Fatalf("the deadline must cancel the live session, got cancel=%v fact=%v", cancelRequested, deadlineFact)
	}

	cancelPass(t, store)
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
		t.Fatalf("the deadline cancel must end the Thread, got %v", state)
	}
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "cancelled" {
		t.Fatalf("the deadline must converge through exactly one EndSession{cancelled}, got %v", reasons)
	}
}
