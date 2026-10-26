package integration

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

func TestLateDeliveryFactCurrentWorkspaceGenerationFenceOverGRPC(t *testing.T) {
	f := setup(t)
	scene, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, scene, false)
	// Production freezes the sandbox identity without an explicit runtimeGeneration target
	// field. Advancing the Workspace generation must still fence that original sandbox.
	var explicitGeneration bool
	must(t, f.store.Pool.QueryRow(`SELECT target ? 'runtimeGeneration' FROM execution_work WHERE execution_id=$1`, req.ExecutionId).Scan(&explicitGeneration))
	if explicitGeneration {
		t.Fatal("production delivery unexpectedly carries an explicit target runtimeGeneration")
	}
	_, err := f.store.Pool.Exec("UPDATE workspaces SET runtime_generation=runtime_generation+1,version=version+1 WHERE issue_run_id=$1", scene.runID)
	must(t, err)
	response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), req)
	if response != nil || status.Convert(err).Message() != "dispatch_conflict" {
		t.Fatalf("a newer Workspace generation must refuse the original late fact: %v %v", response, err)
	}
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	lateDeliveryNoFact(t, f, req)
	if len(f.refs.queried()) != 0 || len(f.objects.probed()) != 0 {
		t.Fatal("a stale sandbox generation reached external evidence")
	}
}

func TestLateDeliveryFactOriginalResourceFencesOverGRPC(t *testing.T) {
	for _, field := range []string{"workspaceId", "sandboxInstanceId", "nodeId", "runtimeGeneration", "nodeOperationId", "workBinding"} {
		t.Run(field, func(t *testing.T) {
			f := setup(t)
			scene, req := lateDeliveryRegistered(t, f)
			lateDeliveryGiveUp(t, f, scene, false)
			var target, operation string
			must(t, f.store.Pool.QueryRow("SELECT w.target::text,e.node_operation_id FROM execution_work w JOIN node_executions e ON e.work_id=w.id WHERE e.execution_id=$1", req.ExecutionId).Scan(&target, &operation))
			switch field {
			case "nodeOperationId":
				_, err := f.store.Pool.Exec("UPDATE node_executions SET node_operation_id='wrong-original-operation' WHERE execution_id=$1", req.ExecutionId)
				must(t, err)
			case "workBinding":
				_, err := f.store.Pool.Exec("UPDATE execution_work SET execution_id=NULL WHERE execution_id=$1", req.ExecutionId)
				must(t, err)
			case "runtimeGeneration":
				_, err := f.store.Pool.Exec(`UPDATE execution_work SET target=jsonb_set(target,'{runtimeGeneration}','999'::jsonb) WHERE execution_id=$1`, req.ExecutionId)
				must(t, err)
			default:
				_, err := f.store.Pool.Exec(`UPDATE execution_work SET target=jsonb_set(target,ARRAY[$2::text],to_jsonb('00000000-0000-4000-8000-000000000001'::text)) WHERE execution_id=$1`, req.ExecutionId, field)
				must(t, err)
			}
			response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), req)
			if response != nil {
				t.Fatal("mismatched original resource became ACKable")
			}
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			lateDeliveryNoFact(t, f, req)
			if len(f.refs.queried()) != 0 || len(f.objects.probed()) != 0 {
				t.Fatal("mismatched original resource reached external evidence")
			}
			_, err = f.store.Pool.Exec(`UPDATE execution_work SET target=$2::jsonb,execution_id=$1 WHERE id=(SELECT work_id FROM node_executions WHERE execution_id=$1)`, req.ExecutionId, target)
			must(t, err)
			_, err = f.store.Pool.Exec("UPDATE node_executions SET node_operation_id=$2 WHERE execution_id=$1", req.ExecutionId, operation)
			must(t, err)
			lateDeliveryAccepted(t, f, req)
		})
	}
}

func TestLateDeliveryFactDoesNotBroadenLiveDispatchOrCancellationOverGRPC(t *testing.T) {
	for _, state := range []string{"admission-only", "cancelled", "replaced-node"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			scene, req := lateDeliveryRegistered(t, f)
			originalStatus, _ := f.runStatusVersion(scene.runID)
			switch state {
			case "admission-only":
				_, err := f.store.Pool.Exec("UPDATE workspaces SET admission_open=false WHERE issue_run_id=$1", scene.runID)
				must(t, err)
			case "cancelled":
				_, err := f.store.Pool.Exec("UPDATE issue_runs SET status='cancelled' WHERE id=$1", scene.runID)
				must(t, err)
			case "replaced-node":
				_, err := f.store.Pool.Exec("UPDATE node_instances SET node_id='replacement-node' WHERE node_id=$1", scene.nodeID)
				must(t, err)
			}
			response, err := f.executions.TakeOverNodeEvent(asController("ctrl-a"), req)
			if response != nil {
				t.Fatal("an ordinary closed/cancelled/replaced runtime acquired late-fact eligibility")
			}
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			lateDeliveryNoFact(t, f, req)
			_, err = f.store.Pool.Exec("UPDATE workspaces SET admission_open=true WHERE issue_run_id=$1", scene.runID)
			must(t, err)
			_, err = f.store.Pool.Exec("UPDATE issue_runs SET status=$2 WHERE id=$1", scene.runID, originalStatus)
			must(t, err)
			if state == "replaced-node" {
				_, err = f.store.Pool.Exec("UPDATE node_instances SET node_id=$1 WHERE node_id='replacement-node'", scene.nodeID)
				must(t, err)
			}
			lateDeliveryGiveUp(t, f, scene, false)
			original, err := f.executions.GetDispatch(asController("ctrl-a"), &controlpb.GetDispatchRequest{ExecutionId: req.ExecutionId})
			must(t, err)
			_, err = f.executions.RecordDispatch(asController("ctrl-a"), &controlpb.RecordDispatchRequest{
				SubmissionId: "new-dispatch-after-giveup", Epoch: 1, OperationId: scene.runID, ExecutionId: "new-delivery-after-giveup", NodeId: scene.nodeID, Input: original.GetRecord().GetInput(),
			})
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			lateDeliveryAccepted(t, f, req)
			if f.scalar("SELECT count(*) FROM node_executions WHERE execution_id='new-delivery-after-giveup'") != 0 || len(f.deliveryAttempts(scene.runID)) != 1 {
				t.Fatal("accepting an original late fact granted a new dispatch or retry")
			}
		})
	}
}

func TestLateDeliveryFactLeavesAnotherLiveExecutionUsableOverGRPC(t *testing.T) {
	f := setup(t)
	original, req := lateDeliveryRegistered(t, f)
	lateDeliveryGiveUp(t, f, original, false)
	other := seedLiveThreadScene(t, f)
	other.start(t, f)
	f.runningThread(t, other)
	lateDeliveryAccepted(t, f, req)
	if f.runPhase(other.runID) != "running" {
		t.Fatal("old late fact changed another live Run")
	}
	f.sessionEndOK(t, other, "other-live-terminal", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	execution := f.deliverRevision(t, other)
	in := f.deliveryInputOf(execution)
	f.push(in, resumedFinalCommit)
	_, err := f.deliveryTerminal(other, execution, "other-live-delivery", 1, deliveredResult(other, in, resumedFinalCommit), "other live delivery")
	must(t, err)
	if f.runPhase(other.runID) != "releasing" || f.revisionRow(other.runID) == nil || f.nodeSequence(execution) != 1 {
		t.Fatal("another live execution lost normal receipt, verified delivery or business settlement")
	}
	// The independent release scanner may still run; the receipt never changes its authority.
	must(t, f.store.RedeclareRunWorkspaceDeletesOnce(context.Background()))
}
