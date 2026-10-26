package integration

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// seedResidualTerminalIntake fixes only the test's pre-existing business/ledger state. The original
// execution, frozen work input/target and receipt sequence were registered through production
// dispatch before this fixture runs. In particular this does not claim to execute or prove the
// reclamation completion topology: the cleared branch is a fact-intake precondition only.
func seedResidualTerminalIntake(t *testing.T, f *fixture, scene liveThreadScene, state string) {
	t.Helper()
	reclaimID, operationID := uuid.NewString(), uuid.NewString()
	tx, err := f.store.Pool.Begin()
	must(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec(`UPDATE issue_runs SET phase='done',status='failed',thread_state='ended',idle_since=NULL,
 failure_reason='agent_start_failed',completed_at=clock_timestamp(),version=version+1,
 result='{"deliveryState":"failed","testBoundary":"preexisting_business_terminal"}'::jsonb WHERE id=$1`, scene.runID)
	must(t, err)
	_, err = tx.Exec(`UPDATE workspaces SET admission_open=false,admission_epoch=admission_epoch+1,
 desired_state='deleted',observed_state=CASE WHEN $2='cleared' THEN 'deleted' ELSE 'deleting' END,
 deleted_at=CASE WHEN $2='cleared' THEN clock_timestamp() ELSE NULL END,version=version+1 WHERE issue_run_id=$1`, scene.runID, state)
	must(t, err)
	_, err = tx.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,
 request,idempotency_key,request_hash,controller_epoch)
 SELECT $2::uuid,w.tenant_id,w.creator_user_id,w.project_id,w.id,'delete_workspace',
 CASE WHEN $4='cleared' THEN 'succeeded' ELSE 'running' END,
 CASE WHEN $4='cleared' THEN 'done' ELSE 'quiesce' END,
 jsonb_build_object('reclaimId',$3::uuid::text),'terminal-intake-'||$3::uuid::text,'terminal-intake-fixture',1
 FROM workspaces w WHERE w.issue_run_id=$1::uuid`, scene.runID, operationID, reclaimID, state)
	must(t, err)
	_, err = tx.Exec(`INSERT INTO agent_run_residuals(run_id,tenant_id,project_id,workspace_id,runtime_generation,
 responsibility,reason,reclaim_state,reclaim_id,reclaim_mode,reclaim_operation_id,reclaim_authorization,
 reclaim_targets,cleared_at,disposition)
 SELECT $1::uuid,w.tenant_id,w.project_id,w.id,w.runtime_generation,
 jsonb_build_object('executions',(SELECT jsonb_agg(jsonb_build_object('executionId',e.execution_id))
 FROM node_executions e WHERE e.workspace_id=w.id)),
 'agent_start_failed',$2,$3::uuid,'owned_delete',$4::uuid,
 jsonb_build_object('kind','preexisting_intake_fixture','runId',$1::uuid::text,'workspaceId',w.id),
 (SELECT jsonb_agg(jsonb_build_object('kind','sandbox','id',s.id,'workspaceId',w.id,'generation',s.generation))
 FROM sandbox_instances s WHERE s.workspace_id=w.id),
 CASE WHEN $2='cleared' THEN clock_timestamp() ELSE NULL END,
 CASE WHEN $2='cleared' THEN 'released' ELSE NULL END
 FROM workspaces w WHERE w.issue_run_id=$1::uuid`, scene.runID, state, reclaimID, operationID)
	must(t, err)
	must(t, tx.Commit())
}

// residualIntakeSnapshot deliberately excludes the ledger/effect/result facts being accepted. It
// covers the whole business row and every dispatch counter that a late fact must not resurrect.
func residualIntakeSnapshot(t *testing.T, f *fixture, scene liveThreadScene) string {
	t.Helper()
	var snapshot string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object(
 'run',to_jsonb(r),'workspace',jsonb_build_object('admission',w.admission_open,'desired',w.desired_state,
 'observed',w.observed_state,'deleted',w.deleted_at,'generation',w.runtime_generation,'version',w.version),
 'operations',(SELECT count(*) FROM operations WHERE workspace_id=w.id),
 'ensures',(SELECT count(*) FROM external_effects WHERE workspace_id=w.id AND kind='sandbox_ensure'),
 'sandboxes',(SELECT count(*) FROM sandbox_instances WHERE workspace_id=w.id),
 'executions',(SELECT count(*) FROM node_executions WHERE workspace_id=w.id),
 'work',(SELECT count(*) FROM execution_work WHERE run_id=r.id),
 'activities',(SELECT count(*) FROM issue_activities WHERE issue_id=r.issue_id))::text
 FROM issue_runs r JOIN workspaces w ON w.id=r.workspace_id WHERE r.id=$1`, scene.runID).Scan(&snapshot))
	return snapshot
}

func TestResidualSessionTerminalTakeoverOverGRPC(t *testing.T) {
	for _, state := range []string{"reclaiming", "cleared"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			scene := seedLiveThreadScene(t, f)
			scene.start(t, f)
			f.runningThread(t, scene)
			seedResidualTerminalIntake(t, f, scene, state)
			residualTerminalIntakeCase(t, f, scene, scene.executionID, 2, false)
		})
	}
}

func TestResidualVerifiedDeliveryTakeoverOverGRPC(t *testing.T) {
	for _, state := range []string{"reclaiming", "cleared"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			scene := deliveringScene(t, f)
			execution := f.deliverRevision(t, scene)
			seedResidualTerminalIntake(t, f, scene, state)
			residualTerminalIntakeCase(t, f, scene, execution, 1, true)
		})
	}
}

func residualTerminalIntakeCase(t *testing.T, f *fixture, scene liveThreadScene, execution string, sequence uint64, delivery bool) {
	t.Helper()
	before := residualIntakeSnapshot(t, f, scene)
	in := f.deliveryInputOf(execution)
	result := sessionEndResult(scene, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "")
	conflict := sessionEndResult(scene, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_INTERRUPTED, "")
	kind := "agent_session"
	if delivery {
		kind = "deliver_revision"
		result = deliveredResult(scene, in, revisionFinalCommit)
		conflict = deliveredResult(scene, in, strings.Repeat("2", 40))
		f.push(in, revisionFinalCommit)
	}
	request := func(submission string, fact *controlpb.ExecutionResult) *controlpb.TakeOverNodeEventRequest {
		return &controlpb.TakeOverNodeEventRequest{
			SubmissionId: submission, Epoch: 1,
			OperationId: scene.runID, ExecutionId: execution, Sequence: sequence,
			Result: fact, Event: []byte("original residual terminal event"),
		}
	}
	ctx := asController("ctrl-a")

	// Naming a real execution is insufficient: its original work target must still resolve to
	// the original stable sandbox. An unrelated UUID is rejected before any result or receipt.
	var target string
	must(t, f.store.Pool.QueryRow("SELECT target::text FROM execution_work WHERE execution_id=$1", execution).Scan(&target))
	_, err := f.store.Pool.Exec(`UPDATE execution_work SET target=jsonb_set(target,'{sandboxInstanceId}',to_jsonb($2::text)) WHERE execution_id=$1`, execution, uuid.NewString())
	must(t, err)
	response, err := f.executions.TakeOverNodeEvent(ctx, request("residual-wrong-target", result))
	if err == nil || response != nil {
		t.Fatalf("foreign original target was acknowledged: response=%v error=%v", response, err)
	}
	_, err = f.store.Pool.Exec("UPDATE execution_work SET target=$2::jsonb WHERE execution_id=$1", execution, target)
	must(t, err)
	if got := f.scalar(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1 AND sequence=$2`, execution, sequence); got != 0 {
		t.Fatalf("invalid target created a receipt: %d", got)
	}
	if delivery && (len(f.refs.queried()) != 0 || len(f.objects.probed()) != 0) {
		t.Fatal("invalid original target reached Git or object verification")
	}

	// A successful transport response is the ACK. Force the ledger write to fail, proving the
	// canonical result, event receipt and (for delivery) verified Revision cannot commit before it.
	_, err = f.store.Pool.Exec(`CREATE FUNCTION reject_residual_intake_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected residual evidence persistence failure'; END $$;
 CREATE TRIGGER reject_residual_intake_evidence BEFORE UPDATE OF reclaim_evidence ON agent_run_residuals
 FOR EACH ROW EXECUTE FUNCTION reject_residual_intake_evidence()`)
	must(t, err)
	response, err = f.executions.TakeOverNodeEvent(ctx, request("residual-terminal", result))
	if err == nil || response != nil {
		t.Fatalf("failed evidence persistence was acknowledged: response=%v error=%v", response, err)
	}
	var rolledBack bool
	must(t, f.store.Pool.QueryRow(`SELECT e.result IS NULL
 AND NOT EXISTS(SELECT 1 FROM node_event_receipts WHERE execution_id=e.execution_id AND sequence=$2)
 AND NOT EXISTS(SELECT 1 FROM revision_verifications WHERE execution_id=e.execution_id)
 AND NOT EXISTS(SELECT 1 FROM revisions WHERE execution_id=e.execution_id)
 AND r.reclaim_evidence='[]'::jsonb
 FROM node_executions e JOIN agent_run_residuals r ON r.run_id=e.operation_id
 WHERE e.execution_id=$1`, execution, sequence).Scan(&rolledBack))
	if !rolledBack {
		t.Fatal("failed evidence persistence left a result, receipt, Revision or evidence")
	}
	_, err = f.store.Pool.Exec("DROP TRIGGER reject_residual_intake_evidence ON agent_run_residuals; DROP FUNCTION reject_residual_intake_evidence()")
	must(t, err)

	response, err = f.executions.TakeOverNodeEvent(ctx, request("residual-terminal", result))
	must(t, err)
	if response.GetRecord().GetExecutionId() != execution || response.GetRecord().GetResult() == nil {
		t.Fatalf("successful ACK lost its original execution/result: %v", response)
	}
	var accepted bool
	must(t, f.store.Pool.QueryRow(`SELECT e.result->>'outcome'=$3 AND e.last_event_sequence=$2
 AND EXISTS(SELECT 1 FROM node_event_receipts WHERE execution_id=e.execution_id AND sequence=$2)
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(r.reclaim_evidence) x
 WHERE x->>'key'='execution:'||e.execution_id AND x->'fact'->>'kind'=$4
 AND x->'fact'->>'state'='succeeded' AND x->>'receivedAt' IS NOT NULL)
 FROM node_executions e JOIN agent_run_residuals r ON r.run_id=e.operation_id
 WHERE e.execution_id=$1`, execution, sequence, map[bool]string{false: "agent_session_ended", true: "revision_delivered"}[delivery], kind).Scan(&accepted))
	if !accepted {
		t.Fatal("ACK preceded durable canonical result, matching receipt, or residual evidence")
	}
	if got := f.receiptEvent(execution, int64(sequence)); got != "original residual terminal event" {
		t.Fatalf("original event bytes changed: %q", got)
	}

	if delivery {
		row := f.revisionRow(scene.runID)
		project, repository := f.runPlacement(scene.runID)
		if row == nil || row.RunID != scene.runID || row.TenantID != scene.tenantID || row.WorkspaceID != f.runWorkspaceID(scene.runID) || row.ProjectID != project || row.RepositoryURL != repository || row.FinalCommit != revisionFinalCommit || row.BaseCommit != in.S("baseCommit") || row.RevisionRef != in.S("revisionRef") || row.DeliveryRef != in.S("deliveryRef") {
			t.Fatalf("late verified delivery registered wrong original Revision: %#v", row)
		}
		if row.HistoryKey != in.S("historyKey") || row.HistorySize != revisionHistorySize || row.HistorySHA256 != revisionHistorySHA {
			t.Fatalf("late verified delivery lost its declared history: %#v", row)
		}
		if row.BundleKey != "" || row.BundleSize != nil || row.BundleSHA256 != "" {
			t.Fatalf("late Git delivery registered a retired bundle: %#v", row)
		}
		if verdict := f.revisionVerification(execution); verdict.S("outcome") != "delivered" {
			t.Fatalf("actual Git and history verification missing: %v", verdict)
		}
		// Both the failed transaction's attempt and its successful retry queried the attempt's own
		// delivery ref and verified its history via the real-HTTP HEAD fixture. The pinned Git ref
		// stands for an accepted Node push; no pre-seeded Cloud verification verdict is used.
		refs := f.refs.queried()
		if len(refs) != 2 || refs[0] != in.S("deliveryRef") || refs[1] != in.S("deliveryRef") {
			t.Fatalf("wrong actual Git verification requests: %v", refs)
		}
		probes := f.objects.probed()
		if len(probes) != 2 {
			t.Fatalf("wrong actual storage verification requests: %v", probes)
		}
		for _, probe := range probes {
			if probe.S("key") != in.S("historyKey") || probe.N("size") != revisionHistorySize || probe.S("sha256") != revisionHistorySHA {
				t.Fatalf("wrong actual history verification request: %v", probe)
			}
		}
		if got := f.objects.nonHeadRequests(); got != 0 {
			t.Fatalf("late verification sent %d non-HEAD object-store requests", got)
		}
	}

	var evidenceVersion int64
	var canonical string
	must(t, f.store.Pool.QueryRow(`SELECT r.reclaim_version,e.result::text FROM agent_run_residuals r
 JOIN node_executions e ON e.operation_id=r.run_id WHERE e.execution_id=$1`, execution).Scan(&evidenceVersion, &canonical))
	// Replays use the same persisted event identity. A different submission must also be idempotent,
	// and successful delivery replay must not contact the Git Server or object storage again.
	_, err = f.executions.TakeOverNodeEvent(ctx, request("residual-terminal", result))
	must(t, err)
	_, err = f.executions.TakeOverNodeEvent(ctx, request("residual-terminal-replay", result))
	must(t, err)
	var replayStable bool
	must(t, f.store.Pool.QueryRow(`SELECT reclaim_version=$2 AND
 (SELECT count(*) FROM node_event_receipts WHERE execution_id=$1 AND sequence=$3)=1
 FROM agent_run_residuals WHERE run_id=$4`, execution, evidenceVersion, sequence, scene.runID).Scan(&replayStable))
	if !replayStable {
		t.Fatal("same original terminal duplicated evidence or receipt")
	}

	response, err = f.executions.TakeOverNodeEvent(ctx, request("residual-terminal-conflict", conflict))
	if response != nil || status.Convert(err).Message() != "residual_fact_conflict" {
		t.Fatalf("canonical conflict did not return committed residual conflict: response=%v error=%v", response, err)
	}
	var audited bool
	must(t, f.store.Pool.QueryRow(`SELECT r.reclaim_conflicted_at IS NOT NULL AND e.result=$2::jsonb
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(r.reclaim_evidence) x
 WHERE x->>'key'='execution:'||e.execution_id AND x->'fact'->>'kind'='conflict'
 AND x->'fact'->'original'->'result'=$2::jsonb AND x->'fact'->'incoming'->'result'<>$2::jsonb)
 AND (SELECT count(*) FROM node_event_receipts WHERE execution_id=e.execution_id AND sequence=$3)=1
 FROM node_executions e JOIN agent_run_residuals r ON r.run_id=e.operation_id
 WHERE e.execution_id=$1`, execution, canonical, sequence).Scan(&audited))
	if !audited {
		t.Fatal("conflict response rolled back audit or rewrote canonical result/receipt")
	}
	if delivery && (len(f.refs.queried()) != 2 || len(f.objects.probed()) != 2) {
		t.Fatal("known replay or canonical conflict repeated Git or object verification")
	}
	if after := residualIntakeSnapshot(t, f, scene); after != before {
		t.Fatalf("late facts changed business terminal/version, admission, resource identity or dispatch counts:\nbefore=%s\nafter=%s", before, after)
	}
}
