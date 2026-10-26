package integration

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// Only the preexisting business failure is staged. Both sources acquire controlled
// authority through ManageResidual, and both clone events use the real takeover RPC.
func TestControlledResidualCloneReceiptConflictThroughRegisteredIntentsOverGRPC(t *testing.T) {
	newFixture := controlledFixtureFactory(t)
	for _, source := range []string{"legacy_d8", "residual_ledger"} {
		t.Run(source, func(t *testing.T) {
			f, c, run, workspace, operation, original := controlledCloneReceiptScene(t, newFixture(t))
			ctx := context.Background()
			legacyOperation := ""
			if source == "residual_ledger" {
				_, err := c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{
					SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation.GetId(), Version: operation.GetVersion(),
					State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60,
				})
				must(t, err)
				_, err = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
				must(t, err)
				must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(ctx))
			} else {
				// A D8 Workspace has a failed historical delete and no residual ledger.
				// Do not create a ledger and then delete it to manufacture this source.
				_, err := f.store.Pool.Exec(`UPDATE issue_runs SET phase='done',status='failed',thread_state='ended',idle_since=NULL,
 failure_reason='agent_start_failed',completed_at=clock_timestamp(),version=version+1,
 result='{"deliveryState":"failed","testBoundary":"preexisting_business_terminal"}'::jsonb WHERE id=$1`, run)
				must(t, err)
				_, err = f.store.Pool.Exec(`UPDATE workspaces SET admission_open=false,admission_epoch=admission_epoch+1,
 version=version+1 WHERE id=$1`, workspace)
				must(t, err)
				_, err = f.store.Pool.Exec(`UPDATE operations SET state='failed',error_code='node_unavailable',
 version=version+1,updated_at=clock_timestamp() WHERE id=$1`, operation.GetId())
				must(t, err)
				legacyOperation = uuid.NewString()
				_, err = f.store.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,
 kind,state,step,request,idempotency_key,request_hash,error_code,controller_epoch)
 SELECT $2::uuid,w.tenant_id,w.creator_user_id,w.project_id,w.id,'delete_workspace','failed','terminate',
 '{}','controlled-clone-legacy-delete','controlled-clone-legacy-fixture','node_unavailable',$3
 FROM workspaces w WHERE w.id=$1`, workspace, legacyOperation, c.epoch)
				must(t, err)
			}

			termination := core.ResidualManagementRequest{
				Action: core.ResidualTerminate, TenantID: f.tid, RunID: run, WorkspaceID: workspace, OperationID: legacyOperation,
				Key: "registered-clone-terminate", Reason: "Terminate the original residual while retaining its data", ImpactConfirmed: true,
			}
			accepted, err := f.store.ManageResidual(ctx, &termination)
			must(t, err)
			root := accepted.O("intent").S("id")
			if root == "" {
				t.Fatalf("production termination command returned no intent: %v", accepted)
			}
			var registered bool
			must(t, f.store.Pool.QueryRow(`SELECT i.source=$2 AND i.command_kind='terminate' AND i.root_intent_id IS NULL
 AND i.run_id=$3 AND i.workspace_id=$4 AND i.impact_confirmed
 AND EXISTS(SELECT 1 FROM agent_run_reclamation_command_receipts cr WHERE cr.intent_id=i.id AND cr.idempotency_key=$5)
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(i.audit) a WHERE a->>'kind'='registered')
 AND CASE WHEN $2='legacy_d8' THEN i.legacy_delete_operation_id::text=$6
 AND NOT EXISTS(SELECT 1 FROM agent_run_residuals r WHERE r.run_id=i.run_id)
 ELSE EXISTS(SELECT 1 FROM agent_run_residuals r WHERE r.run_id=i.run_id
 AND r.reclaim_mode='controlled' AND r.reclaim_state='reclaiming' AND r.reclaim_id=i.reclaim_id) END
 FROM agent_run_reclamation_intents i WHERE i.id=$1`, root, source, run, workspace, termination.Key,
				legacyOperation).Scan(&registered))
			if !registered {
				t.Fatal("controlled intent did not come from the requested real source and production command")
			}

			before := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId())
			conflict := proto.Clone(original).(*controlpb.TakeOverNodeEventRequest)
			conflict.SubmissionId, conflict.Event = c.sub(), []byte("clone event B")
			response, err := f.executions.TakeOverNodeEvent(c.ctx, conflict)
			expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
			if response != nil || status.Convert(err).Message() != "residual_fact_conflict" {
				t.Fatalf("registered intent lost receipt-only conflict: response=%v error=%v", response, err)
			}

			// The legacy adapter must persist through legacyIntentId into the root intent;
			// ledger-backed authority must persist into its existing residual row instead.
			var audited bool
			must(t, f.store.Pool.QueryRow(`SELECT
 CASE WHEN i.source='legacy_d8' THEN i.conflicted_at IS NOT NULL AND r.run_id IS NULL
 ELSE r.reclaim_conflicted_at IS NOT NULL END
 AND (SELECT count(*) FROM jsonb_array_elements(
 CASE WHEN i.source='legacy_d8' THEN i.evidence ELSE r.reclaim_evidence END) x
 WHERE x->>'key'=$2 AND x->'fact'->>'kind'='conflict'
 AND x->'fact'->'original'->>'kind'='receipt' AND x->'fact'->'original'->>'event'=$3
 AND x->'fact'->'incoming'->>'kind'='receipt' AND x->'fact'->'incoming'->>'event'=$4
 AND x->>'receivedAt'<>'' AND x->'source'->>'submissionId'=$5)=1
 AND EXISTS(SELECT 1 FROM control_submissions WHERE submission_id=$5 AND response->>'residualFactConflict'='true')
 FROM agent_run_reclamation_intents i LEFT JOIN agent_run_residuals r ON r.run_id=i.run_id WHERE i.id=$1`,
				root, fmt.Sprintf("clone_receipt:%s:%d", original.GetExecutionId(), original.GetSequence()),
				base64.StdEncoding.EncodeToString(original.GetEvent()), base64.StdEncoding.EncodeToString(conflict.GetEvent()),
				conflict.GetSubmissionId()).Scan(&audited))
			if !audited {
				t.Fatal("conflict response preceded committed receipt evidence, conflict time or submission response on its source")
			}
			if after := cloneReceiptCanonicalSnapshot(t, f, original.GetExecutionId()); after != before {
				t.Fatalf("receipt-only conflict changed original clone result or receipt:\nbefore=%s\nafter=%s", before, after)
			}

			// Fresh command keys force the production validation path instead of replaying
			// the successful registration receipt. Both actions must fence the conflict first.
			termination.Key = "registered-clone-terminate-after-conflict"
			for _, request := range []*core.ResidualManagementRequest{
				&termination,
				{
					Action: core.ResidualDeleteData, TenantID: f.tid, IntentID: root,
					Key: "registered-clone-delete-after-conflict", Reason: "Separately authorize retained data deletion", DataDeleteConfirmed: true,
				},
			} {
				out, commandErr := f.store.ManageResidual(ctx, request)
				var fault *core.Fault
				if out != nil || !errors.As(commandErr, &fault) || fault.Status != 409 || fault.Code != "residual_fact_conflict" {
					t.Fatalf("%s bypassed registered receipt conflict: response=%v error=%v", request.Action, out, commandErr)
				}
			}
			if f.scalar(`SELECT count(*) FROM agent_run_reclamation_intents WHERE root_intent_id=$1`, root) != 0 ||
				f.scalar(`SELECT count(*) FROM external_effects WHERE workspace_id=$1 AND kind='workspace_data_delete'`, workspace) != 0 {
				t.Fatal("blocked controlled commands created a data-delete intent or effect")
			}
		})
	}
}

// controlledCloneReceiptScene preserves the original clone receipt setup and assertions while
// accepting this matrix's fresh fixture. The residual test helper remains independently owned.
func controlledCloneReceiptScene(t *testing.T, f *fixture) (*fixture, *grpcController, string, string, *controlpb.Operation, *controlpb.TakeOverNodeEventRequest) {
	t.Helper()
	f.bindBusinessHooks()
	project := seedControlledCloneProject(t, f)
	run := f.insertAgentRun(t, project)
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
	taken, err := f.executions.TakeOverNodeEvent(c.ctx, original)
	must(t, err)
	if taken.GetRecord().GetExecutionId() != execution || !proto.Equal(taken.GetRecord().GetResult(), original.GetResult()) {
		t.Fatalf("initial clone takeover did not preserve its original result: %v", taken)
	}
	return f, c, run, workspace, op, original
}

// seedControlledCloneProject stages only the already available Project and repository snapshot.
// Project/main-Workspace provisioning is outside the receipt-conflict scenario; the run Workspace,
// its sandbox plan, Node registration, frozen clone input, dispatch and receipts remain real below.
func seedControlledCloneProject(t *testing.T, f *fixture) string {
	t.Helper()
	project, binding, workspace, sync := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	var space string
	must(t, f.store.Pool.QueryRow("SELECT id::text FROM collab_workspaces WHERE tenant_id=$1", f.tid).Scan(&space))
	tx, err := f.store.Pool.Begin()
	must(t, err)
	defer func() { _ = tx.Rollback() }()
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := tx.Exec(query, args...)
		must(t, err)
	}
	exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle)
 VALUES($1,$2,$3,$4,'Controlled clone','active')`, project, f.tid, f.uid, space)
	exec(`INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch)
 VALUES($1,$2,$3,'https://example.invalid/repo.git','main')`, binding, f.tid, project)
	// The Project and its required main Workspace commit together under the deferred constraint.
	exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,
 project_id,kind,desired_state,observed_state,admission_open,runtime_generation,requested_ref)
 VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',true,1,'main')`, workspace, f.tid, f.uid, project)
	exec(`INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,finished_at)
 VALUES($1,'repository_sync',$2,$3,$4,'succeeded','fixture',clock_timestamp())`, sync, f.tid, project, binding)
	snapshot := core.GitSnapshotRef(binding, f.commit)
	exec(`INSERT INTO repository_snapshots(binding_id,tenant_id,project_id,commit_oid,snapshot_ref,sync_operation_id)
 VALUES($1,$2,$3,$4,$5,$6)`, binding, f.tid, project, f.commit, snapshot, sync)
	must(t, tx.Commit())
	// This is the existing ref-query fixture for a prepared snapshot, not a physical clone result.
	f.refs.pin(f.tid, project, snapshot, f.commit)
	return project
}
