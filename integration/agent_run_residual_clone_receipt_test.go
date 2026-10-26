package integration

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// Each state is a staged ledger precondition, not a claim that recovery reached it. The
// original result and receipt enter through the same real takeover RPC as the contradiction.
func TestResidualCloneReceiptConflictAcrossReclaimStatesOverGRPC(t *testing.T) {
	for _, state := range []string{"reconciling", "reclaiming", "cleared"} {
		t.Run(state, func(t *testing.T) {
			f, c, run, workspace, original := cloneReceiptResidualScene(t)

			// A running disposal operation gives PlanEffect a valid operation/version. Its
			// rejection must come from the newly committed conflict, not stale-operation fencing.
			disposal := uuid.NewString()
			_, err := f.store.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,
 kind,state,step,request,idempotency_key,request_hash,controller_epoch)
 SELECT $2::uuid,w.tenant_id,w.creator_user_id,w.project_id,w.id,'delete_workspace','running','terminate',
 '{}','clone-receipt-disposal','clone-receipt-fixture',$3 FROM workspaces w WHERE w.id=$1`, workspace, disposal, c.epoch)
			must(t, err)
			if state != "reconciling" {
				_, err = f.store.Pool.Exec(`UPDATE agent_run_residuals SET reclaim_state=$2,reclaim_id=$3,
 reclaim_mode='controlled',reclaim_operation_id=$3,reclaim_authorization='{"source":"test-prestate"}',
 cleared_at=CASE WHEN $2='cleared' THEN now() END,
 disposition=CASE WHEN $2='cleared' THEN 'externally_confirmed' END WHERE run_id=$1`, run, state, disposal)
				must(t, err)
			}
			before := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId())
			var responsibility string
			must(t, f.store.Pool.QueryRow(`SELECT responsibility::text FROM agent_run_residuals WHERE run_id=$1`, run).Scan(&responsibility))
			conflict := proto.Clone(original).(*controlpb.TakeOverNodeEventRequest)
			conflict.SubmissionId, conflict.Event = c.sub(), []byte("clone event B")
			response, err := f.executions.TakeOverNodeEvent(c.ctx, conflict)
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			if response != nil || status.Convert(err).Message() != "residual_fact_conflict" {
				t.Fatalf("receipt-only conflict was not durably rejected: response=%v error=%v", response, err)
			}

			var retained bool
			must(t, f.store.Pool.QueryRow(`SELECT r.reclaim_conflicted_at IS NOT NULL
 AND r.reclaim_state=$2 AND (r.cleared_at IS NOT NULL)=($2='cleared') AND r.responsibility=$3::jsonb
 AND (SELECT count(*) FROM jsonb_array_elements(r.reclaim_evidence) x
 WHERE x->>'key'=$4 AND x->'fact'->>'kind'='conflict'
 AND x->'fact'->'original'->>'kind'='receipt' AND x->'fact'->'original'->>'event'=$5
 AND x->'fact'->'incoming'->>'kind'='receipt' AND x->'fact'->'incoming'->>'event'=$6
 AND x->>'receivedAt'<>'' AND x->'source'->>'submissionId'=$7)=1
 AND EXISTS(SELECT 1 FROM control_submissions WHERE submission_id=$7 AND response->>'residualFactConflict'='true')
 FROM agent_run_residuals r WHERE r.run_id=$1`, run, state, responsibility,
				"clone_receipt:"+original.GetExecutionId()+":1", base64.StdEncoding.EncodeToString(original.GetEvent()),
				base64.StdEncoding.EncodeToString(conflict.GetEvent()), conflict.GetSubmissionId()).Scan(&retained))
			if !retained {
				t.Fatal("conflict error rolled back receipt evidence, replay response, or original ledger state")
			}
			if after := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId()); after != before {
				t.Fatalf("receipt conflict changed original clone result or event receipt:\nbefore=%s\nafter=%s", before, after)
			}

			var evidence string
			must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object('evidence',reclaim_evidence,
 'version',reclaim_version,'conflictedAt',reclaim_conflicted_at)::text FROM agent_run_residuals WHERE run_id=$1`, run).Scan(&evidence))
			for _, submission := range []string{conflict.GetSubmissionId(), c.sub()} {
				conflict.SubmissionId = submission
				response, err = f.executions.TakeOverNodeEvent(c.ctx, conflict)
				expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
				if response != nil || status.Convert(err).Message() != "residual_fact_conflict" {
					t.Fatalf("conflicting receipt replay lost its stable result: response=%v error=%v", response, err)
				}
			}
			var replayStable bool
			must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object('evidence',reclaim_evidence,
 'version',reclaim_version,'conflictedAt',reclaim_conflicted_at)=$2::jsonb FROM agent_run_residuals WHERE run_id=$1`, run, evidence).Scan(&replayStable))
			if !replayStable {
				t.Fatal("receipt conflict replay duplicated evidence or moved conflict/version metadata")
			}

			var disposalVersion int64
			var disposalBefore string
			must(t, f.store.Pool.QueryRow(`SELECT version,jsonb_build_object('state',state,'step',step,
 'version',version,'updatedAt',updated_at,'effects',(SELECT count(*) FROM external_effects WHERE operation_id=$1))::text
 FROM operations WHERE id=$1`, disposal).Scan(&disposalVersion, &disposalBefore))
			planned, err := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{
				SubmissionId: c.sub(), Epoch: c.epoch, OperationId: disposal, Version: disposalVersion,
				Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_TERMINATE, WorkspaceId: workspace,
			})
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			if planned != nil || status.Convert(err).Message() != "residual_fact_conflict" {
				t.Fatalf("committed receipt conflict did not block disposal: response=%v error=%v", planned, err)
			}
			var disposalAfter string
			must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object('state',state,'step',step,
 'version',version,'updatedAt',updated_at,'effects',(SELECT count(*) FROM external_effects WHERE operation_id=$1))::text
 FROM operations WHERE id=$1`, disposal).Scan(&disposalAfter))
			if disposalAfter != disposalBefore {
				t.Fatalf("blocked disposal changed its operation or planned an effect:\nbefore=%s\nafter=%s", disposalBefore, disposalAfter)
			}
		})
	}
}

func TestResidualCloneReceiptConflictDoesNotImportLegacyResultOverGRPC(t *testing.T) {
	f, c, run, _, original := cloneReceiptResidualScene(t)
	// Model a pre-migration terminal fact: its result exists only in lateFacts,
	// with no accepted clone fact in the new evidence ledger.
	_, err := f.store.Pool.Exec(`UPDATE agent_run_residuals r SET
 responsibility=jsonb_set(r.responsibility,'{lateFacts}',jsonb_build_object(
 'clone:'||e.execution_id,jsonb_build_object('kind','clone','state','failed','result',e.result))),
 reclaim_version=7 FROM clone_executions e WHERE r.run_id=$1 AND e.execution_id=$2`, run, original.GetExecutionId())
	must(t, err)
	var responsibility string
	var version int64
	var emptyEvidence bool
	must(t, f.store.Pool.QueryRow(`SELECT responsibility::text,reclaim_version,
 reclaim_evidence='[]'::jsonb FROM agent_run_residuals WHERE run_id=$1`, run).Scan(&responsibility, &version, &emptyEvidence))
	if !emptyEvidence {
		t.Fatal("legacy fixture already has evidence in the reclaim ledger")
	}
	before := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId())
	conflict := proto.Clone(original).(*controlpb.TakeOverNodeEventRequest)
	conflict.SubmissionId, conflict.Event = c.sub(), []byte("clone event B")
	response, err := f.executions.TakeOverNodeEvent(c.ctx, conflict)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if response != nil || status.Convert(err).Message() != "residual_fact_conflict" {
		t.Fatalf("legacy receipt conflict was not durably rejected: response=%v error=%v", response, err)
	}

	var retained bool
	must(t, f.store.Pool.QueryRow(`SELECT r.responsibility=$2::jsonb
 AND r.reclaim_version=$3+1 AND r.reclaim_conflicted_at IS NOT NULL
 AND jsonb_array_length(r.reclaim_evidence)=1
 AND r.reclaim_evidence->0->>'key'=$4
 AND r.reclaim_evidence->0->'fact'=jsonb_build_object('kind','conflict','state','conflict',
 'original',jsonb_build_object('kind','receipt','event',$5::text),
 'incoming',jsonb_build_object('kind','receipt','event',$6::text))
 AND r.reclaim_evidence->0->>'receivedAt'<>''
 AND r.reclaim_evidence->0->'source'->>'submissionId'=$7
 AND EXISTS(SELECT 1 FROM control_submissions WHERE submission_id=$7 AND response->>'residualFactConflict'='true')
 FROM agent_run_residuals r WHERE r.run_id=$1`, run, responsibility, version,
		"clone_receipt:"+original.GetExecutionId()+":1", base64.StdEncoding.EncodeToString(original.GetEvent()),
		base64.StdEncoding.EncodeToString(conflict.GetEvent()), conflict.GetSubmissionId()).Scan(&retained))
	if !retained {
		t.Fatal("rejected receipt must append exactly one conflict and one version, preserving lateFacts without importing an ordinary clone fact")
	}
	if after := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId()); after != before {
		t.Fatalf("legacy receipt conflict changed original clone result or event receipt:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCloneReceiptConflictWithoutResidualWritesNothingOverGRPC(t *testing.T) {
	f, c, _, _, _, original := cloneReceiptScene(t)
	before := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId())
	var bookkeepingBefore string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object(
 'residuals',(SELECT jsonb_agg(to_jsonb(r) ORDER BY run_id) FROM agent_run_residuals r),
 'submissions',(SELECT jsonb_agg(jsonb_build_object('id',submission_id,'holder',holder_id,
 'response',response,'createdAt',created_at) ORDER BY submission_id) FROM control_submissions))::text`).Scan(&bookkeepingBefore))
	conflict := proto.Clone(original).(*controlpb.TakeOverNodeEventRequest)
	conflict.SubmissionId, conflict.Event = c.sub(), []byte("clone event B")
	response, err := f.executions.TakeOverNodeEvent(c.ctx, conflict)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if response != nil || status.Convert(err).Message() != "receipt_conflict" {
		t.Fatalf("ordinary receipt conflict changed behavior: response=%v error=%v", response, err)
	}
	if after := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId()); after != before {
		t.Fatalf("ordinary receipt conflict changed canonical data:\nbefore=%s\nafter=%s", before, after)
	}
	var bookkeepingAfter string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object(
 'residuals',(SELECT jsonb_agg(to_jsonb(r) ORDER BY run_id) FROM agent_run_residuals r),
 'submissions',(SELECT jsonb_agg(jsonb_build_object('id',submission_id,'holder',holder_id,
 'response',response,'createdAt',created_at) ORDER BY submission_id) FROM control_submissions))::text`).Scan(&bookkeepingAfter))
	if bookkeepingAfter != bookkeepingBefore {
		t.Fatalf("ordinary receipt conflict wrote residual evidence or a control submission:\nbefore=%s\nafter=%s", bookkeepingBefore, bookkeepingAfter)
	}
}

func cloneReceiptResidualScene(t *testing.T) (*fixture, *grpcController, string, string, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	f, c, run, workspace, operation, original := cloneReceiptScene(t)
	_, err := c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{
		SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation.GetId(), Version: operation.GetVersion(),
		State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60,
	})
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
	must(t, err)
	must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
	return f, c, run, workspace, original
}

func cloneReceiptScene(t *testing.T) (*fixture, *grpcController, string, string, *controlpb.Operation, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	f, c, run, workspace, op, original := dispatchedCloneReceiptScene(t)
	taken, err := f.executions.TakeOverNodeEvent(c.ctx, original)
	must(t, err)
	if taken.GetRecord().GetExecutionId() != original.GetExecutionId() || !proto.Equal(taken.GetRecord().GetResult(), original.GetResult()) {
		t.Fatalf("initial clone takeover did not preserve its original result: %v", taken)
	}
	return f, c, run, workspace, op, original
}

// dispatchedCloneReceiptScene leaves the original dispatch pending so tests can order its result and defer independently.
func dispatchedCloneReceiptScene(t *testing.T) (*fixture, *grpcController, string, string, *controlpb.Operation, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	f := setup(t)
	f.bindBusinessHooks()
	created := f.create("clone-receipt-conflict")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	provisioned, err := f.store.CreateRunWorkspace(context.Background(), run)
	must(t, err)
	workspace, operation := provisioned.O("workspace").S("id"), provisioned.O("operation").S("id")
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET phase='provisioning',status='dispatched',workspace_id=$2,version=version+1 WHERE id=$1`, run, workspace)
	must(t, err)
	c := newGRPCController(t, f)
	snapshot := c.claim()
	if snapshot.GetOperation().GetId() != operation {
		t.Fatalf("claim picked %s, want %s", snapshot.GetOperation().GetId(), operation)
	}
	op, ensured := c.effect(snapshot.GetOperation(), controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, workspace)
	op, err = c.advance(op)
	must(t, err)
	nodeID := ensured.GetEvidence().GetSandboxEnsured().GetNodeId()
	node, err := c.register(ensured.GetId(), 1, nodeID, "inc-late")
	must(t, err)
	_, err = c.status(node, controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
	must(t, err)
	_, err = c.advance(op)
	must(t, err)
	snapshot = c.claim()
	op = snapshot.GetOperation()
	if op.GetStep() != controlpb.OperationStep_OPERATION_STEP_CLONE || snapshot.GetCloneInput() == nil {
		t.Fatalf("receipt fixture must dispatch the Workspace's frozen clone step: %v", snapshot)
	}
	execution := "clone-receipt-execution"
	_, err = f.executions.RecordDispatch(c.ctx, &controlpb.RecordDispatchRequest{
		SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, ExecutionId: execution, NodeId: nodeID,
		Input: &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: snapshot.GetCloneInput()}},
	})
	must(t, err)
	original := &controlpb.TakeOverNodeEventRequest{
		SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, ExecutionId: execution, Sequence: 1,
		Result: &controlpb.ExecutionResult{
			Node:    &controlpb.NodeIdentity{NodeId: nodeID, NodeIncarnationId: "inc-late"},
			Outcome: &controlpb.ExecutionResult_CloneFailed{CloneFailed: &controlpb.CloneFailed{Reason: controlpb.CloneFailureReason_CLONE_FAILURE_REASON_SNAPSHOT_UNAVAILABLE}},
		},
		Event: []byte("clone event A"),
	}
	return f, c, run, workspace, op, original
}

func cloneReceiptCanonicalSnapshot(t *testing.T, f *fixture, execution string) string {
	t.Helper()
	var snapshot string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object('clone',to_jsonb(e),
 'receipts',(SELECT jsonb_agg(to_jsonb(r) ORDER BY sequence) FROM clone_event_receipts r WHERE r.execution_id=e.execution_id))::text
 FROM clone_executions e WHERE e.execution_id=$1`, execution).Scan(&snapshot))
	return snapshot
}
