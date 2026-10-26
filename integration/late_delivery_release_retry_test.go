package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// The real-stack cycle behind circus #407 is a release retry, not a second delivery. D5 has
// already made the delivery outcome failed and declared the delete. The first quiesce observes
// the unresolved Controller execution and restores the Workspace; the original terminal then
// becomes a late fact, and the next ordinary delete attempt is allowed to finish the release.
func TestLateDeliveryAfterRefusedQuiesceConvergesThroughReleaseRetryOverGRPC(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "same-incarnation"
		if restart {
			name = "new-incarnation"
		}
		t.Run(name, func(t *testing.T) {
			runLateDeliveryReleaseRetry(t, restart)
		})
	}
}

func runLateDeliveryReleaseRetry(t *testing.T, restart bool) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, scene, false)
	wid := f.runWorkspaceID(scene.runID)

	// This is the production Node-control path. Binding the seeded sandbox stages the same external
	// identity the real create_workspace operation records before its Controller reports quiesce.
	f.bindRunWorkspaceSandboxForNode(t, scene.runID, scene.nodeID)
	deleteOps := deleteOperations(f.runOperations(scene.runID))
	if len(deleteOps) != 1 || deleteOps[0].S("step") != "quiesce" || deleteOps[0].S("state") != "queued" {
		t.Fatalf("D5 must leave one queued quiesce operation, got %v", deleteOps)
	}
	deleteID := deleteOps[0].S("id")

	// The Node is still busy with the unresolved terminal. Claim the operation and report the
	// refusal through the production gRPC NodeReportService; Cloud restores running/ready with
	// admission open, exactly as in the 90-second deployment trace.
	ctx := asController("ctrl-a")
	claimed, err := controlpb.NewWorkspaceOperationServiceClient(f.controlConn).ClaimOperation(ctx, &controlpb.ClaimOperationRequest{Epoch: 1})
	must(t, err)
	claimedOp := claimed.GetSnapshot().GetOperation()
	if claimedOp.GetId() != deleteID || claimedOp.GetStep() != controlpb.OperationStep_OPERATION_STEP_QUIESCE || claimedOp.GetState() != controlpb.OperationState_OPERATION_STATE_RUNNING {
		t.Fatalf("the first delete must be claimed at quiesce, got %v", claimed.GetSnapshot())
	}
	nodeVersion := f.scalar("SELECT version FROM node_instances WHERE id=$1", scene.nodeID)
	idle, err := controlpb.NewNodeReportServiceClient(f.controlConn).ReportNodeIdle(ctx, &controlpb.ReportNodeIdleRequest{
		SubmissionId: uuid.NewString(), Epoch: 1, NodeInstanceId: scene.nodeID, Version: int64(nodeVersion),
		OperationId: deleteID, AdmissionEpoch: f.workspaceFields(wid).N("admissionEpoch"), Idle: false,
	})
	must(t, err)
	if idle.GetAccepted() {
		t.Fatalf("the first quiesce must be refused by the busy Node, got %v", idle)
	}
	if ws := f.workspaceFields(wid); ws.S("desiredState") != "running" || ws.S("observedState") != "ready" || !ws.B("admissionOpen") {
		t.Fatalf("a refused quiesce must restore running/ready admission, got %v", ws)
	}
	deleteOps = deleteOperations(f.runOperations(scene.runID))
	if len(deleteOps) != 1 || deleteOps[0].S("state") != "failed" || deleteOps[0].S("errorCode") != "resource_in_use" {
		t.Fatalf("the first release operation must be failed/resource_in_use, got %v", deleteOps)
	}

	if restart {
		// The original Node identity is preserved across a container restart: only its incarnation
		// changes. End and register through the real NodeReportService, then report the new handshake
		// as connected before submitting the original outbox terminal. The terminal retains the old
		// incarnation because it was formed before the restart.
		nodes := controlpb.NewNodeReportServiceClient(f.controlConn)
		ended, err := nodes.EndNode(ctx, &controlpb.EndNodeRequest{
			SubmissionId: uuid.NewString(), Epoch: 1, NodeInstanceId: scene.nodeID, Version: int64(nodeVersion),
		})
		must(t, err)
		if ended.GetNode().GetConnection() != controlpb.NodeConnection_NODE_CONNECTION_ENDED {
			t.Fatalf("old Node incarnation did not end: %v", ended.GetNode())
		}
		registered, err := nodes.RegisterNode(ctx, &controlpb.RegisterNodeRequest{
			SubmissionId: uuid.NewString(), Epoch: 1, SandboxInstanceId: sandboxIDForRun(t, f, scene.runID),
			Generation: 1, Node: &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "new-incarnation"}, ProtocolVersion: 1,
		})
		must(t, err)
		connected, err := nodes.ReportNodeStatus(ctx, &controlpb.ReportNodeStatusRequest{
			SubmissionId: uuid.NewString(), Epoch: 1, NodeInstanceId: registered.GetNode().GetId(),
			Version: registered.GetNode().GetVersion(), Connection: controlpb.NodeConnection_NODE_CONNECTION_CONNECTED, Initialized: true,
		})
		must(t, err)
		if connected.GetNode().GetIdentity().GetNodeIncarnationId() != "new-incarnation" {
			t.Fatalf("new Node incarnation did not reconnect: %v", connected.GetNode())
		}
		if ws := f.workspaceFields(wid); ws.S("desiredState") != "running" || ws.S("observedState") != "ready" || !ws.B("admissionOpen") {
			t.Fatalf("reconnected Node must restore running/ready admission, got %v", ws)
		}
	}

	// The late terminal is accepted while the Workspace is running again. Its acceptance must not
	// settle or rewrite the Run, the failed delete, or admission; only the durable execution fact,
	// receipt and the valid Revision declaration are new.
	beforeFact := lateDeliveryBusinessSnapshot(t, f, scene)
	statusBefore, versionBefore := f.runStatusVersion(scene.runID)
	lateDeliveryNoSettlement(f)
	lateDeliveryAccepted(t, f, req)
	if afterFact := lateDeliveryBusinessSnapshot(t, f, scene); afterFact != beforeFact {
		t.Fatalf("late fact changed the Run, restored Workspace or failed delete:\n%s\n%s", beforeFact, afterFact)
	}
	if phase := f.runPhase(scene.runID); phase != "releasing" {
		t.Fatalf("late fact must leave the Run releasing, got %q", phase)
	}
	if got, version := f.runStatusVersion(scene.runID); got != statusBefore || version != versionBefore {
		t.Fatalf("late fact changed the Run status/version: %q/%d -> %q/%d", statusBefore, versionBefore, got, version)
	}
	result := f.runResult(scene.runID)
	if result.S("deliveryState") != "failed" || result["revisionId"] != nil {
		t.Fatalf("late fact must preserve deliveryState=failed and revisionId=null, got %v", result)
	}
	if row := f.revisionRow(scene.runID); row == nil {
		t.Fatal("the valid late success must register the original Revision")
	}

	// The release reconciliation pass declares a fresh operation for the same durable delete
	// intent. The normal Controller operation driver then reports idle, terminates the sandbox and
	// records Workspace-data deletion through the real gRPC and simulator boundaries.
	must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
	deleteOps = deleteOperations(f.runOperations(scene.runID))
	if len(deleteOps) != 2 || deleteOps[1].S("state") != "queued" || deleteOps[1].S("step") != "quiesce" {
		t.Fatalf("release retry must declare one fresh queued quiesce, got %v", deleteOps)
	}
	if ws := f.workspaceFields(wid); ws.S("desiredState") != "deleted" || ws.B("admissionOpen") {
		t.Fatalf("release retry must close admission for deletion, got %v", ws)
	}

	f.driveDelete(t, scene.runID)
	if got := f.runPhase(scene.runID); got != "done" {
		t.Fatalf("the accepted late fact must let the next release reach done, got %q", got)
	}
	if ws := f.workspaceFields(wid); !ws.B("deleted") || ws.S("desiredState") != "deleted" || ws.B("admissionOpen") {
		t.Fatalf("the release retry must delete the Workspace, got %v", ws)
	}
	if got := f.scalar("SELECT count(*) FROM sandbox_instances WHERE workspace_id=$1 AND terminated_at IS NOT NULL", wid); got != 1 {
		t.Fatalf("the successful release must terminate the sandbox, got %d rows", got)
	}
	expectedEndedNodes := 1
	if restart {
		expectedEndedNodes = 2
	}
	if got := f.scalar("SELECT count(*) FROM node_instances WHERE workspace_id=$1 AND ended_at IS NOT NULL", wid); got != expectedEndedNodes {
		t.Fatalf("the successful release must end every Node incarnation, got %d rows want %d", got, expectedEndedNodes)
	}
	for _, kind := range []string{"sandbox_terminate", "workspace_data_delete"} {
		if got := f.scalar("SELECT count(*) FROM external_effects WHERE workspace_id=$1 AND kind=$2 AND state='succeeded'", wid, kind); got != 1 {
			t.Fatalf("the successful release must record one succeeded %s effect, got %d", kind, got)
		}
	}
}

func sandboxIDForRun(t *testing.T, f *fixture, runID string) string {
	t.Helper()
	var sandboxID string
	must(t, f.store.Pool.QueryRow(`SELECT s.id FROM sandbox_instances s JOIN workspaces w ON w.id=s.workspace_id WHERE w.issue_run_id=$1 AND s.terminated_at IS NULL`, runID).Scan(&sandboxID))
	return sandboxID
}

// bindRunWorkspaceSandboxForNode stages the same external sandbox identity as the production
// ensure effect, while keeping the seeded NodeId stable enough for the real replacement-incarnation
// registration to pass the ensure-evidence check. The Cloud fixture still performs the normal HTTP
// effect replay and all Node operations below use production gRPC.
func (f *fixture) bindRunWorkspaceSandboxForNode(t *testing.T, runID, nodeID string) {
	t.Helper()
	wid := f.runWorkspaceID(runID)
	sandboxID := sandboxIDForRun(t, f, runID)
	var projectID, operationID string
	must(t, f.store.Pool.QueryRow(`SELECT w.project_id,o.id FROM workspaces w JOIN operations o ON o.workspace_id=w.id AND o.kind='create_workspace' WHERE w.id=$1 ORDER BY o.created_at,o.id LIMIT 1`, wid).Scan(&projectID, &operationID))
	request := map[string]any{"kind": "sandbox_ensure", "projectId": projectID, "workspaceId": wid}
	result := map[string]any{"sandboxInstanceId": sandboxID, "nodeId": nodeID}
	requestJSON, err := json.Marshal(request)
	must(t, err)
	resultJSON, err := json.Marshal(result)
	must(t, err)
	// The seeded scene already has a live sandbox row but no create_workspace effect row. Add the
	// committed ensure evidence that the production RegisterNode route requires; this is external
	// fixture evidence, not a mutation of the Node or execution identity.
	_, err = f.store.Pool.Exec(`INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,external_id,request,result,reconciled_epoch)
		VALUES($1,$2,$3,$4,'sandbox_ensure','succeeded',$5,$6::jsonb,$7::jsonb,1)`, sandboxID, operationID, projectID, wid, "sim-"+sandboxID, string(requestJSON), string(resultJSON))
	must(t, err)
	effect := map[string]any{
		"id": sandboxID, "externalId": "sim-" + sandboxID, "state": "succeeded",
		"request": request,
		"result":  result,
	}
	raw, err := json.Marshal(effect)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(f.substrate.Root, "effects", sandboxID+".json"), raw, 0o600))
	sandbox := map[string]any{"id": sandboxID, "workspaceId": wid, "nodeId": nodeID, "terminated": false}
	sandboxRaw, err := json.Marshal(sandbox)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(f.substrate.Root, "effects", sandboxID+".sandbox.json"), sandboxRaw, 0o600))
	f.bindRunWorkspaceSandbox(t, runID)
}
