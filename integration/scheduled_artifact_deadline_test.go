package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// HEAD finishes before the takeover waits on the real control-plane lock. Move the fixture's
// deadline between that transaction's start and lock acquisition, using PostgreSQL time only.
// No elapsed sleep or guessed request latency decides which side of the deadline was exercised.
func TestScheduledArtifactGRPCDeadlineWhileWaitingForLock(t *testing.T) {
	f := setup(t)
	scene, execution := scheduledArtifactScene(t, f)
	ctx, cancel := context.WithTimeout(asController("ctrl-a"), 30*time.Second)
	defer cancel()
	headStarted, resumeHEAD := make(chan struct{}), make(chan struct{})
	var resume sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(headStarted)
		select {
		case <-resumeHEAD:
			f.objects.head(w, r)
		case <-ctx.Done():
			http.Error(w, "test canceled", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	defer resume.Do(func() { close(resumeHEAD) })
	config := *f.objects.config
	config.Endpoint = server.URL
	f.store.ObjectStore = &config
	key := "scheduled-runs/" + scene.tenantID + "/" + scene.runID + "/3"
	done := make(chan error, 1)
	go func() {
		_, err := f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{
			Epoch: 1, SubmissionId: "wait-across-deadline", OperationId: scene.runID, ExecutionId: execution,
			Sequence: 1, Event: []byte("uploaded"), Result: scheduledUploadResult(scene, key),
		})
		done <- err
	}()
	select {
	case <-headStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	blocker, err := f.store.Pool.BeginTx(ctx, nil)
	must(t, err)
	defer blocker.Rollback()
	var blockerPID int
	must(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid() FROM pg_advisory_xact_lock(67420911)").Scan(&blockerPID))
	resume.Do(func() { close(resumeHEAD) })
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var crossed bool
	for !crossed {
		// The deadline is changed only after the final transaction demonstrably waits for our
		// lock. Its now() remains before this instant even after our commit releases it.
		must(t, f.store.Pool.QueryRowContext(ctx, `WITH waiter AS (
			SELECT xact_start FROM pg_stat_activity WHERE datname=current_database()
			AND $2::integer=ANY(pg_blocking_pids(pid))
		), changed AS (
			UPDATE scheduled_run_artifact_deliveries SET deadline_at=clock_timestamp()
			WHERE issue_run_id=$1 AND EXISTS (SELECT 1 FROM waiter)
			RETURNING deadline_at
		) SELECT EXISTS (SELECT 1 FROM changed, waiter WHERE deadline_at > xact_start)`, scene.runID, blockerPID).Scan(&crossed))
		if !crossed {
			select {
			case err := <-done:
				t.Fatalf("takeover returned before the lock wait: %v", err)
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	must(t, blocker.Commit())
	select {
	case err := <-done:
		must(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if f.scalar(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, execution) != 1 ||
		f.scalar(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1`, scene.runID) != 0 ||
		f.scalar(`SELECT count(*) FROM scheduled_run_artifact_deliveries
			WHERE issue_run_id=$1 AND state='failed' AND settled_reason='deadline_expired'`, scene.runID) != 1 {
		t.Fatal("a takeover that acquires the lock after its deadline must ACK without registration and settle as expired")
	}
	if len(f.objects.probed()) != 1 {
		t.Fatal("the object must have verified before the final transaction waited")
	}
}
