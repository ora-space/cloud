package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// These cases use real Controller reports to distinguish a disconnected Node from one that has
// been unknown for the configured window. Database time is aged by a wide margin; no case sleeps.
func TestNodeUnknownDisconnectedWithinWindowKeepsDeliveryAndRelease(t *testing.T) {
	for _, phase := range []string{"delivering", "releasing"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			f.store.DeliveryUnreachableAfter = time.Hour
			scene := nodeUnknownScene(t, f, phase)
			version := int64(f.scalar("SELECT version FROM node_instances WHERE id=$1", scene.nodeID))
			version = reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
			must(t, f.ageRunWorkspaceNodeBy(scene.runID, 15*time.Minute))
			reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_DISCONNECTED)
			runVersion := f.runVersion(scene.runID)

			passNodeUnknownWindow(t, f, phase)

			assertNodeUnknownStillWaiting(t, f, scene.runID, phase, runVersion)
		})
	}
}

func TestNodeUnknownDisconnectedPastWindowSettlesDeliveryAndRelease(t *testing.T) {
	for _, phase := range []string{"delivering", "releasing"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			f.store.DeliveryUnreachableAfter = time.Hour
			scene := nodeUnknownScene(t, f, phase)
			version := int64(f.scalar("SELECT version FROM node_instances WHERE id=$1", scene.nodeID))
			version = reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
			must(t, f.ageRunWorkspaceNodeBy(scene.runID, 2*time.Hour))
			reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_DISCONNECTED)

			passNodeUnknownWindow(t, f, phase)

			assertNodeUnknownExpired(t, f, scene.runID, phase)
		})
	}
}

func TestNodeUnknownReconnectRestartsWindowButRepeatedDisconnectDoesNot(t *testing.T) {
	for _, phase := range []string{"delivering", "releasing"} {
		t.Run(phase, func(t *testing.T) {
			f := setup(t)
			f.store.DeliveryUnreachableAfter = time.Hour
			scene := nodeUnknownScene(t, f, phase)
			version := int64(f.scalar("SELECT version FROM node_instances WHERE id=$1", scene.nodeID))
			version = reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
			must(t, f.ageRunWorkspaceNodeBy(scene.runID, 15*time.Minute))
			previous := nodeUnknownLastSeen(t, f, scene.nodeID)
			version = reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_DISCONNECTED)
			if got := nodeUnknownLastSeen(t, f, scene.nodeID); !got.Equal(previous) {
				t.Fatalf("disconnect must preserve the last connected instant: %s -> %s", previous, got)
			}

			version = reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
			reconnected := nodeUnknownLastSeen(t, f, scene.nodeID)
			if !reconnected.After(previous) {
				t.Fatalf("reconnect must restart the window on database time: %s -> %s", previous, reconnected)
			}
			version = reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_DISCONNECTED)
			if got := nodeUnknownLastSeen(t, f, scene.nodeID); !got.Equal(reconnected) {
				t.Fatalf("a new disconnect must retain the reconnected instant: %s -> %s", reconnected, got)
			}
			runVersion := f.runVersion(scene.runID)
			passNodeUnknownWindow(t, f, phase)
			assertNodeUnknownStillWaiting(t, f, scene.runID, phase, runVersion)

			// Expire the new window, then deliver another fresh disconnect report. Receiving that
			// report is not evidence that Cloud can reach the Node and must not postpone give-up.
			must(t, f.ageRunWorkspaceNodeBy(scene.runID, 2*time.Hour))
			expired := nodeUnknownLastSeen(t, f, scene.nodeID)
			reportNodeUnknownConnection(t, f, scene.nodeID, version, controlpb.NodeConnection_NODE_CONNECTION_DISCONNECTED)
			if got := nodeUnknownLastSeen(t, f, scene.nodeID); !got.Equal(expired) {
				t.Fatalf("repeated disconnect must not refresh last_seen_at: %s -> %s", expired, got)
			}
			passNodeUnknownWindow(t, f, phase)
			assertNodeUnknownExpired(t, f, scene.runID, phase)
		})
	}
}

// A terminated sandbox is immediately unknown to delivery, but release must still finish its
// Node-independent cleanup. D8 must not terminalize that real delete while it is at cleanup.
func TestNodeUnknownNoLiveSandboxReleasesDeliveryButPreservesCleanup(t *testing.T) {
	f := setup(t)
	f.store.DeliveryUnreachableAfter = time.Hour
	scene := deliveringScene(t, f)
	_, err := f.store.Pool.Exec(`
		WITH ended AS (
		  UPDATE sandbox_instances SET observed_state='terminated',terminated_at=clock_timestamp()
		  WHERE workspace_id=(SELECT workspace_id FROM issue_runs WHERE id=$1) AND terminated_at IS NULL
		  RETURNING id
		)
		UPDATE node_instances SET connection_state='ended',ended_at=clock_timestamp(),version=version+1
		WHERE sandbox_instance_id IN (SELECT id FROM ended) AND ended_at IS NULL`, scene.runID)
	must(t, err)

	// No heartbeat is aged: absence of a live sandbox is the immediate D5 condition.
	must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
	assertNodeUnknownExpired(t, f, scene.runID, "delivering")
	c := &grpcController{
		t: t, f: f, ctx: asController("ctrl-a"), epoch: 1,
		operations: controlpb.NewWorkspaceOperationServiceClient(f.controlConn),
	}
	op := c.claim().GetOperation()
	op, err = c.advance(op)
	must(t, err)
	op, err = c.advance(op)
	must(t, err)
	if op.GetStep() != controlpb.OperationStep_OPERATION_STEP_CLEANUP || op.GetState() != controlpb.OperationState_OPERATION_STATE_RUNNING {
		t.Fatalf("the real delete must reach cleanup without a Node, got %v", op)
	}
	version := f.runVersion(scene.runID)

	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))

	if got := f.runPhase(scene.runID); got != "releasing" {
		t.Fatalf("D8 must leave Node-independent cleanup releasing, got %q", got)
	}
	if got := f.runVersion(scene.runID); got != version {
		t.Fatalf("D8 must not rewrite a cleanup run: version %d -> %d", version, got)
	}
	if got := f.runFailureReason(scene.runID); got != "" {
		t.Fatalf("cleanup must not acquire a give-up reason, got %q", got)
	}
	var step, state, code string
	var opVersion int64
	must(t, f.store.Pool.QueryRow(`SELECT step,state,COALESCE(error_code,''),version FROM operations WHERE id=$1`, op.GetId()).Scan(&step, &state, &code, &opVersion))
	if step != "cleanup" || state != "running" || code != "" || opVersion != op.GetVersion() {
		t.Fatalf("D8 must preserve the running cleanup: step=%s state=%s error=%s version=%d, want %d", step, state, code, opVersion, op.GetVersion())
	}
}

// nodeUnknownScene reaches each tested phase through the real session/delivery path and gives its
// seeded sandbox the external identity required by the real Controller report route.
func nodeUnknownScene(t *testing.T, f *fixture, phase string) liveThreadScene {
	t.Helper()
	var scene liveThreadScene
	switch phase {
	case "delivering":
		scene = deliveringScene(t, f)
	case "releasing":
		scene = driveToReleasing(t, f, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED).liveThreadScene
	default:
		t.Fatalf("unsupported scene phase %q", phase)
	}
	// driveToReleasing uses continuous failure to reach its starting point. Only the unknown-Node
	// window may decide any subsequent pass in these tests.
	f.store.DeliveryGiveUpAfter = 0
	f.bindRunWorkspaceSandbox(t, scene.runID)
	return scene
}

func reportNodeUnknownConnection(t *testing.T, f *fixture, nodeID string, version int64, connection controlpb.NodeConnection) int64 {
	t.Helper()
	out, err := controlpb.NewNodeReportServiceClient(f.controlConn).ReportNodeStatus(asController("ctrl-a"), &controlpb.ReportNodeStatusRequest{
		SubmissionId: uuid.NewString(), Epoch: 1, NodeInstanceId: nodeID,
		Version: version, Connection: connection, Initialized: true,
	})
	must(t, err)
	return out.GetNode().GetVersion()
}

func nodeUnknownLastSeen(t *testing.T, f *fixture, nodeID string) time.Time {
	t.Helper()
	var seen time.Time
	must(t, f.store.Pool.QueryRow(`SELECT last_seen_at FROM node_instances WHERE id=$1`, nodeID).Scan(&seen))
	return seen
}

func passNodeUnknownWindow(t *testing.T, f *fixture, phase string) {
	t.Helper()
	if phase == "delivering" {
		must(t, f.store.GiveUpStaleDeliveriesOnce(context.Background()))
		return
	}
	must(t, f.store.GiveUpStaleWorkspaceReleasesOnce(context.Background()))
}

func assertNodeUnknownStillWaiting(t *testing.T, f *fixture, runID, phase string, version int64) {
	t.Helper()
	if got := f.runPhase(runID); got != phase {
		t.Fatalf("a disconnect within the window must stay %s, got %q", phase, got)
	}
	if got := f.runVersion(runID); got != version {
		t.Fatalf("waiting must not rewrite the run: version %d -> %d", version, got)
	}
	if got := f.runFailureReason(runID); got != "" {
		t.Fatalf("waiting must not record give-up, got %q", got)
	}
	ops := deleteOperations(f.runOperations(runID))
	if phase == "delivering" {
		if len(ops) != 0 {
			t.Fatalf("waiting delivery must not declare deletion, got %v", ops)
		}
		return
	}
	if len(ops) != 1 || ops[0].S("state") != "queued" || ops[0].S("errorCode") != "" {
		t.Fatalf("waiting release must preserve its queued delete without failure, got %v", ops)
	}
}

func assertNodeUnknownExpired(t *testing.T, f *fixture, runID, phase string) {
	t.Helper()
	if phase == "releasing" {
		assertGiveUpSettlement(t, f, runID, "completed")
		return
	}
	if got := f.runPhase(runID); got != "releasing" {
		t.Fatalf("D5 must release an unknown Node's delivery, got %q", got)
	}
	if got := f.runResult(runID).S("deliveryState"); got != "failed" {
		t.Fatalf("D5 must record deliveryState=failed, got %q", got)
	}
	if got, _ := f.runStatusVersion(runID); got != "completed" {
		t.Fatalf("D5 must retain the session's completed outcome, got %q", got)
	}
	if ops := deleteOperations(f.runOperations(runID)); len(ops) != 1 || ops[0].S("state") != "queued" {
		t.Fatalf("D5 must declare a queued delete, got %v", ops)
	}
}
