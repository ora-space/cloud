package integration

import (
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// An audited artifact contradiction blocks physical cleanup, not a peer Revision's real receipt.
func TestArtifactConflictDoesNotRollBackRevisionTakeover(t *testing.T) {
	f := setup(t)
	f.useObjectStore()
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	in := f.deliveryInputOf(execution)
	f.push(in, revisionFinalCommit)
	artifact, work := uuid.NewString(), uuid.NewString()
	// Stage the already committed contradiction as the precondition, then use the production
	// gRPC takeover and idle paths; this fixture does not claim to validate Node proof creation.
	_, err := f.store.Pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target,execution_id)
 SELECT $2,run_id,'deliver_scheduled_artifacts','{}',target,$3 FROM execution_work WHERE execution_id=$1`, execution, work, artifact)
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch,business_settled_at,business_settlement_source,dispatch_identity,non_acceptance_proof)
 SELECT $2,'deliver_scheduled_artifacts',operation_id,$3,workspace_id,node_id,$2,'{}',dispatched_epoch,clock_timestamp(),'artifact_deadline','{}','{}'
 FROM node_executions WHERE execution_id=$1`, execution, artifact, work)
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO execution_non_acceptance_conflicts(execution_id,fact_key,fact,source) VALUES($1,'contradiction','{}','{}')`, artifact)
	must(t, err)
	_, err = f.deliveryTerminal(scene, execution, "peer-revision", 1, deliveredResult(scene, in, revisionFinalCommit), "revision_delivered")
	if err != nil {
		t.Fatalf("artifact contradiction rolled back peer Revision takeover: %v", err)
	}
	if f.runPhase(scene.runID) != "releasing" || f.runResult(scene.runID).S("deliveryState") != "saved" || f.revisionRow(scene.runID) == nil {
		t.Fatal("real Revision and release intent were not committed")
	}
	if f.receiptEvent(execution, 1) != "revision_delivered" {
		t.Fatal("real Revision receipt missing")
	}
	wid := f.runWorkspaceID(scene.runID)
	f.bindRunWorkspaceSandboxForNode(t, scene.runID, scene.nodeID)
	ctx := asController("ctrl-a")
	claimed, err := controlpb.NewWorkspaceOperationServiceClient(f.controlConn).ClaimOperation(ctx, &controlpb.ClaimOperationRequest{Epoch: 1})
	must(t, err)
	op := claimed.GetSnapshot().GetOperation()
	if op.GetStep() != controlpb.OperationStep_OPERATION_STEP_QUIESCE {
		t.Fatalf("expected quiesce intent: %v", op)
	}
	_, err = controlpb.NewNodeReportServiceClient(f.controlConn).ReportNodeIdle(ctx, &controlpb.ReportNodeIdleRequest{SubmissionId: uuid.NewString(), Epoch: 1, NodeInstanceId: scene.nodeID, Version: int64(f.scalar("SELECT version FROM node_instances WHERE id=$1", scene.nodeID)), OperationId: op.GetId(), AdmissionEpoch: f.workspaceFields(wid).N("admissionEpoch"), Idle: true})
	if err == nil {
		t.Fatal("artifact contradiction allowed physical quiesce")
	}
	if f.scalar("SELECT count(*) FROM execution_non_acceptance_conflicts WHERE execution_id=$1", artifact) != 1 {
		t.Fatal("contradiction evidence lost")
	}
	if f.scalar("SELECT count(*) FROM external_effects WHERE workspace_id=$1 AND kind IN ('sandbox_terminate','workspace_data_delete')", wid) != 0 {
		t.Fatal("physical cleanup started despite contradiction")
	}
}
