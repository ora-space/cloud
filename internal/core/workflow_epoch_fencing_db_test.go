package core

// The epoch clause of the node-result obligation for specs decisions/cloud/workflow
// 0-cloud-side-scheduler-owns-workflow-execution.md D1 条件②: a result reaching Cloud from a
// Controller that has lost its lease is rejected before it writes anything, while the current epoch
// may submit the same execution identity — settlement is keyed by execution identity, not by the
// epoch that dispatched it. The scene is a real PostgreSQL database and the control plane's own
// lease actions.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// A node result submitted under an epoch the controller lease has already lost is rejected before it
// touches anything: no execution result, receipt or submission row, and no run or node-state change.
// The current epoch submitting the same execution identity is accepted, because settlement is keyed
// by execution identity rather than by the epoch that dispatched it.
func TestWorkflowSessionResultFromAnExpiredControllerEpochIsRejected(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store) // ctrl-a, epoch 1
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)

	var dispatchedEpoch int64
	if err := store.Pool.QueryRow(`SELECT dispatched_epoch FROM node_executions WHERE execution_id=$1`, execution).Scan(&dispatchedEpoch); err != nil {
		t.Fatalf("read dispatched epoch: %v", err)
	}
	if dispatchedEpoch != 1 {
		t.Fatalf("dispatched under epoch %d; want 1", dispatchedEpoch)
	}
	statusBefore, errBefore, statesBefore, versionBefore := readWorkflowRun(t, store, runID)

	// The old controller's lease expires; a replacement controller takes the next epoch.
	if _, err := store.Pool.Exec(`UPDATE controller_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE name='global'`); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	replacement := &Claims{Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-b"}}
	taken, err := store.Control(context.Background(), &ControlRequest{Action: "lease_acquire", Body: Object{}, Service: replacement})
	if err != nil {
		t.Fatalf("take the lease: %v", err)
	}
	if epoch := taken.N("epoch"); epoch != 2 {
		t.Fatalf("replacement epoch = %d; want 2", epoch)
	}

	result := Object{"outcome": "agent_session_ended", "reason": "idle_timeout", "node": Object{"nodeId": nodeService}}
	_, err = store.Control(context.Background(), &ControlRequest{
		Action:       "clone_takeover",
		SubmissionID: "expired-epoch-result",
		Service:      controllerClaims(), // ctrl-a, epoch 1
		Body: Object{
			"operationId": runID, "executionId": execution, "epoch": int64(1),
			"sequence": int64(1), "event": "ended", "result": result,
		},
	})
	var fault *Fault
	if !errors.As(err, &fault) || fault.Code != "stale_controller" || fault.Status != 409 {
		t.Fatalf("expired-epoch result returned %v; want 409 stale_controller", err)
	}

	// Rejection leaves no partial write: the execution holds no result and no receipt, the
	// submission was never recorded, and the run's committed state is byte-identical.
	var hasResult bool
	var sequence int64
	var receipts, submissions int
	if err := store.Pool.QueryRow(`SELECT result IS NOT NULL, last_event_sequence FROM node_executions WHERE execution_id=$1`, execution).
		Scan(&hasResult, &sequence); err != nil {
		t.Fatalf("read execution after the rejected result: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, execution).Scan(&receipts); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM control_submissions WHERE submission_id=$1`, "expired-epoch-result").Scan(&submissions); err != nil {
		t.Fatalf("count submissions: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if hasResult || sequence != 0 || receipts != 0 || submissions != 0 {
		t.Fatalf("rejected result wrote something: result=%v sequence=%d receipts=%d submissions=%d",
			hasResult, sequence, receipts, submissions)
	}
	if status != statusBefore || runErr != errBefore || version != versionBefore || fmt.Sprint(states) != fmt.Sprint(statesBefore) {
		t.Fatalf("rejected result changed the run: status=%q err=%q version=%d states=%v; want %q %q %d %v",
			status, runErr, version, states, statusBefore, errBefore, versionBefore, statesBefore)
	}

	// The current epoch submitting the same execution identity is accepted and settles the node.
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action:       "clone_takeover",
		SubmissionID: "current-epoch-result",
		Service:      replacement,
		Body: Object{
			"operationId": runID, "executionId": execution, "epoch": int64(2),
			"sequence": int64(1), "event": "ended", "result": result,
		},
	}); err != nil {
		t.Fatalf("current-epoch result: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the accepted result: %v", err)
	}
	status, runErr, states, _ = readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	outs, _ := asWorkflowObject(states["o"])
	if status != "succeeded" || runErr != "" || agent == nil || agent.S("status") != "succeeded" || outs == nil || outs.S("status") != "succeeded" {
		t.Fatalf("run after the accepted result: status=%q err=%q states=%v; want succeeded", status, runErr, states)
	}
}
