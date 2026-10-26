package integration

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// These scenes stage each ledger prestate to exercise the real fact entrypoint over bufconn and
// PostgreSQL. Staging reclaiming/cleared is not evidence that the recovery loop reaches them.
func TestResidualProvisioningFactsAcrossReclaimStatesOverGRPC(t *testing.T) {
	for _, state := range []string{"reconciling", "reclaiming", "cleared"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			created := f.create("residual-facts-" + state)
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
			planned, err := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: snapshot.GetOperation().GetVersion(), Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, WorkspaceId: workspace})
			must(t, err)
			effect := planned.GetEffect().GetId()
			// Older ensure plans may carry no Workspace on the effect itself. Its original
			// operation still binds the run Workspace, so intake must use that durable identity.
			_, err = f.store.Pool.Exec(`UPDATE external_effects SET workspace_id=NULL WHERE id=$1 AND operation_id=$2`, effect, operation)
			must(t, err)
			clone, plugin := "late-clone-"+state, "late-plugin-"+state
			// Only pre-seed the execution row; the entry under test is the real RPC.
			_, err = f.store.Pool.Exec(`INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id)
			 VALUES($1,$2,$3,'node-late','{"kind":"clone"}',1,$1::text)`, clone, operation, workspace)
			must(t, err)
			_, err = f.store.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id)
			 VALUES($1,'install_plugins',$2,$3,'node-late','{"kind":"install_plugins"}',1,$1::text)`, plugin, operation, workspace)
			must(t, err)
			_, err = c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: planned.GetOperation().GetVersion(), State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60})
			must(t, err)
			_, err = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
			must(t, err)
			must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
			if state != "reconciling" {
				_, err = f.store.Pool.Exec(`UPDATE agent_run_residuals SET reclaim_state=$2,reclaim_id=$3,reclaim_mode='controlled',
				 reclaim_authorization='{"source":"test-prestate"}',cleared_at=CASE WHEN $2='cleared' THEN now() END,
				 disposition=CASE WHEN $2='cleared' THEN 'externally_confirmed' END WHERE run_id=$1`, run, state, operation)
				must(t, err)
			}
			var responsibility string
			must(t, f.store.Pool.QueryRow(`SELECT responsibility::text FROM agent_run_residuals WHERE run_id=$1`, run).Scan(&responsibility))
			facts := []*controlpb.RecordProvisioningFactRequest{
				{
					Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT, EffectId: effect,
					State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED, ExternalId: "substrate-late",
					Evidence: &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: effect, NodeId: "node-late"}}},
				},
				{
					Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_CLONE, ExecutionId: clone,
					ExecutionResult: &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: "node-late", NodeIncarnationId: "inc-late"}, Outcome: &controlpb.ExecutionResult_CloneFailed{CloneFailed: &controlpb.CloneFailed{Reason: controlpb.CloneFailureReason_CLONE_FAILURE_REASON_SNAPSHOT_UNAVAILABLE}}},
				},
				{
					Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_PLUGIN, ExecutionId: plugin,
					ExecutionResult: &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: "node-late", NodeIncarnationId: "inc-late"}, Outcome: &controlpb.ExecutionResult_PluginsFailed{PluginsFailed: &controlpb.PluginsFailed{Reason: controlpb.PluginsFailureReason_PLUGINS_FAILURE_REASON_PLUGIN_ROOT_UNAVAILABLE}}},
				},
				{Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_CREATION, Outcome: controlpb.ProvisioningOutcome_PROVISIONING_OUTCOME_READY},
			}
			for _, fact := range facts {
				fact.SubmissionId = c.sub()
				_, err = c.operations.RecordProvisioningFact(c.ctx, fact)
				must(t, err)
				_, err = c.operations.RecordProvisioningFact(c.ctx, fact)
				must(t, err)
				replay := proto.Clone(fact).(*controlpb.RecordProvisioningFactRequest)
				replay.SubmissionId = c.sub()
				_, err = c.operations.RecordProvisioningFact(c.ctx, replay)
				must(t, err)
			}
			var canonicalClone, canonicalPlugin string
			must(t, f.store.Pool.QueryRow(`SELECT result::text FROM clone_executions WHERE execution_id=$1`, clone).Scan(&canonicalClone))
			must(t, f.store.Pool.QueryRow(`SELECT result::text FROM node_executions WHERE execution_id=$1`, plugin).Scan(&canonicalPlugin))
			conflicts := make([]*controlpb.RecordProvisioningFactRequest, len(facts))
			for i, fact := range facts {
				conflicts[i] = proto.Clone(fact).(*controlpb.RecordProvisioningFactRequest)
				conflicts[i].SubmissionId = c.sub()
			}
			failure := "late contradiction"
			conflicts[0].State, conflicts[0].Evidence, conflicts[0].Failure = controlpb.EffectState_EFFECT_STATE_FAILED, nil, &failure
			conflicts[1].ExecutionResult.Outcome = &controlpb.ExecutionResult_CloneFailed{CloneFailed: &controlpb.CloneFailed{Reason: controlpb.CloneFailureReason_CLONE_FAILURE_REASON_INTERRUPTED}}
			conflicts[2].ExecutionResult.Outcome = &controlpb.ExecutionResult_PluginsFailed{PluginsFailed: &controlpb.PluginsFailed{Reason: controlpb.PluginsFailureReason_PLUGINS_FAILURE_REASON_INTERRUPTED}}
			conflicts[3].Outcome = controlpb.ProvisioningOutcome_PROVISIONING_OUTCOME_FAILED
			for _, conflict := range conflicts {
				for _, submission := range []string{conflict.SubmissionId, conflict.SubmissionId, c.sub()} {
					conflict.SubmissionId = submission
					_, err = c.operations.RecordProvisioningFact(c.ctx, conflict)
					expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
					if status.Convert(err).Message() != "residual_fact_conflict" {
						t.Fatalf("conflict did not return its stable residual code: %v", err)
					}
				}
			}
			// Read after the rejected RPCs: all conflicts must already be committed, deduplicated,
			// and separate from the frozen responsibility and canonical execution results.
			var retained bool
			must(t, f.store.Pool.QueryRow(`SELECT r.responsibility=$2::jsonb AND r.reclaim_state=$3
			 AND (r.cleared_at IS NOT NULL)=($3='cleared') AND r.reclaim_conflicted_at IS NOT NULL
			 AND jsonb_array_length(r.reclaim_evidence)=8
			 AND (SELECT count(*) FROM jsonb_array_elements(r.reclaim_evidence) e WHERE e->'fact'->>'kind'='conflict'
			   AND e->'fact' ? 'incoming' AND e->'fact' ? 'original' AND e->>'source'<>'' AND e->>'receivedAt'<>'')=4
			 AND (SELECT count(DISTINCT e->>'key') FROM jsonb_array_elements(r.reclaim_evidence) e WHERE e->>'key'=ANY($7::text[]))=4
			 AND (SELECT result=$5::jsonb FROM clone_executions WHERE execution_id=$4)
			 AND (SELECT result=$6::jsonb FROM node_executions WHERE execution_id=$8)
			 AND (SELECT workspace_id IS NULL AND state='succeeded' AND external_id='substrate-late'
			   AND result=jsonb_build_object('sandboxInstanceId',$9::text,'nodeId','node-late') FROM external_effects WHERE id=$9::uuid)
			 AND (SELECT phase='done' AND status='failed' FROM issue_runs WHERE id=$1)
			 AND (SELECT NOT admission_open FROM workspaces WHERE id=$10)
			 FROM agent_run_residuals r WHERE r.run_id=$1`, run, responsibility, state, clone, canonicalClone, canonicalPlugin,
				"{effect:"+effect+",clone:"+clone+",plugin:"+plugin+",creation}", plugin, effect, workspace).Scan(&retained))
			if !retained {
				t.Fatal("late fact intake changed original state, lost durable conflict evidence, or duplicated a replay")
			}
		})
	}
}
