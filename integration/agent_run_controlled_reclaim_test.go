package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

func controlledTerminate(t *testing.T, f *fixture, tenant, run, workspace string) core.Object {
	t.Helper()
	out, err := f.store.ManageResidual(context.Background(), &core.ResidualManagementRequest{
		Action: core.ResidualTerminate, TenantID: tenant, RunID: run, WorkspaceID: workspace,
		Key: "controlled-terminate-" + run, Reason: "Close the original residual runtime and retain its data", ImpactConfirmed: true,
	})
	must(t, err)
	if out.O("intent").S("id") == "" {
		t.Fatalf("accepted command has no durable intent: %v", out)
	}
	return out
}

// controlledConfirmInjected supplies Controller termination evidence over the real bufconn
// contract. It proves Cloud's target/permit/transaction handling, not actual Docker termination.
func controlledConfirmInjected(t *testing.T, f *fixture, c *grpcController, intent string) map[string]bool {
	t.Helper()
	api := controlpb.NewRuntimeControlServiceClient(f.controlConn)
	pending, err := api.ListForceStops(c.ctx, &controlpb.ListForceStopsRequest{Epoch: c.epoch})
	must(t, err)
	var plan *controlpb.RuntimeForceStopPlan
	for _, candidate := range pending.GetPlans() {
		if candidate.GetId() == intent {
			plan = candidate
		}
	}
	if plan == nil {
		t.Fatalf("controlled intent %s was absent from ListForceStops: %v", intent, pending)
	}
	targets := map[string]bool{}
	version := plan.GetVersion()
	if len(plan.GetEffects()) == 0 {
		_, err = api.ConfirmForceStop(c.ctx, &controlpb.ConfirmForceStopRequest{SubmissionId: c.sub(), Epoch: c.epoch, ForceStopId: intent, Version: version})
		must(t, err)
	}
	for _, effect := range plan.GetEffects() {
		sandbox := effect.GetRequest().GetSandboxTerminate().GetSandboxInstanceId()
		if sandbox == "" || targets[sandbox] {
			t.Fatalf("force-stop plan lost or duplicated an original target: %v", plan)
		}
		targets[sandbox] = true
		if effect.GetState() == controlpb.EffectState_EFFECT_STATE_SUCCEEDED {
			continue
		}
		permit, permitErr := api.GetEffectPermit(c.ctx, &controlpb.GetEffectPermitRequest{Epoch: c.epoch, ForceStopId: intent, EffectId: effect.GetId()})
		must(t, permitErr)
		if permit.GetPermit().GetSandboxId() != sandbox || permit.GetPermit().GetWorkspaceId() != plan.GetWorkspaceId() || permit.GetPermit().GetExpiresAtMs() <= permit.GetPermit().GetIssuedAtMs() {
			t.Fatalf("permit did not bind the original target and a live lease: %v", permit)
		}
		_, err = api.ConfirmForceStop(c.ctx, &controlpb.ConfirmForceStopRequest{SubmissionId: c.sub(), Epoch: c.epoch, ForceStopId: intent, Version: version, EffectId: effect.GetId(), Terminated: true})
		if err == nil {
			t.Fatal("termination without durable late-ensure fencing was accepted")
		}
		confirmed, confirmErr := api.ConfirmForceStop(c.ctx, &controlpb.ConfirmForceStopRequest{SubmissionId: c.sub(), Epoch: c.epoch, ForceStopId: intent, Version: version, EffectId: effect.GetId(), Terminated: true, LateEnsureFenced: true})
		must(t, confirmErr)
		version = confirmed.GetVersion()
	}
	return targets
}

func controlledDeleteInjected(t *testing.T, f *fixture, c *grpcController, tenant, root, workspace string) core.Object {
	t.Helper()
	accepted, err := f.store.ManageResidual(context.Background(), &core.ResidualManagementRequest{
		Action: core.ResidualDeleteData, TenantID: tenant, IntentID: root, Key: "controlled-delete-" + root,
		Reason: "Delete retained residual data after separate confirmation", DataDeleteConfirmed: true,
	})
	must(t, err)
	op := c.claim().GetOperation()
	if op.GetId() != accepted.O("operation").S("id") || op.GetStep() != controlpb.OperationStep_OPERATION_STEP_CLEANUP {
		t.Fatalf("data deletion did not claim its exact cleanup-only operation: %v %v", accepted, op)
	}
	planned, err := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion(), WorkspaceId: workspace, Kind: controlpb.EffectKind_EFFECT_KIND_WORKSPACE_DATA_DELETE})
	must(t, err)
	permit, err := controlpb.NewRuntimeControlServiceClient(f.controlConn).GetEffectPermit(c.ctx, &controlpb.GetEffectPermitRequest{Epoch: c.epoch, EffectId: planned.GetEffect().GetId()})
	must(t, err)
	if permit.GetPermit().GetWorkspaceId() != workspace {
		t.Fatalf("data-delete permit names another Workspace: %v", permit)
	}
	// Explicit injected physical evidence; no fixture writes a succeeded operation or disposition.
	recorded, err := c.operations.RecordEffectResult(c.ctx, &controlpb.RecordEffectResultRequest{
		SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: planned.GetOperation().GetVersion(),
		EffectId: planned.GetEffect().GetId(), State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED,
		ExternalId: "injected-delete-" + planned.GetEffect().GetId(),
		Evidence:   &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_WorkspaceDataDeleted{WorkspaceDataDeleted: &controlpb.WorkspaceDataDeleted{}}},
	})
	must(t, err)
	finished, err := c.advance(recorded.GetOperation())
	must(t, err)
	if finished.GetState() != controlpb.OperationState_OPERATION_STATE_SUCCEEDED {
		t.Fatalf("real delete-result/advance path did not succeed: %v", finished)
	}
	return accepted
}

func TestControlledResidualRetainedThenReleasedOverGRPC(t *testing.T) {
	f := controlledFixtureFactory(t)(t)
	f.bindBusinessHooks()
	created := f.create("controlled-provisioning")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	provisioned, err := f.store.CreateRunWorkspace(context.Background(), run)
	must(t, err)
	workspace, operation := provisioned.O("workspace").S("id"), provisioned.O("operation").S("id")
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET phase='provisioning',status='dispatched',workspace_id=$2,version=version+1 WHERE id=$1`, run, workspace)
	must(t, err)
	c := newGRPCController(t, f)
	op := c.claim().GetOperation()
	if op.GetId() != operation {
		t.Fatalf("unexpected original provisioning operation: %v", op)
	}
	ensure, err := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: op.GetVersion(), WorkspaceId: workspace, Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE})
	must(t, err)
	// A legacy NULL-scope ensure belongs to this original operation but never produced a sandbox
	// row. The controlled target set must retain it instead of treating the missing row as absence.
	neverEnsured := uuid.NewString()
	_, err = f.store.Pool.Exec(`INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch)
 SELECT $1,id,project_id,NULL,'sandbox_ensure','planned',jsonb_build_object('kind','sandbox_ensure','workspaceId',$2::text),$3 FROM operations WHERE id=$4`, neverEnsured, workspace, c.epoch, operation)
	must(t, err)
	_, err = c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: ensure.GetOperation().GetVersion(), State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60})
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
	must(t, err)
	must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
	var historical string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object('run',to_jsonb(r),'reason',a.reason,'responsibility',a.responsibility,'operation',to_jsonb(o))::text
 FROM issue_runs r JOIN agent_run_residuals a ON a.run_id=r.id JOIN operations o ON o.id=$2 WHERE r.id=$1`, run, operation).Scan(&historical))
	accepted := controlledTerminate(t, f, f.tid, run, workspace)
	root := accepted.O("intent").S("id")
	targets := controlledConfirmInjected(t, f, c, root)
	if len(targets) != 2 || !targets[ensure.GetEffect().GetId()] || !targets[neverEnsured] {
		t.Fatalf("controlled termination omitted an original ensure identity: %v", targets)
	}
	var clearedAt string
	must(t, f.store.Pool.QueryRow(`SELECT cleared_at::text FROM agent_run_residuals WHERE run_id=$1 AND reclaim_state='cleared' AND disposition='terminated_data_retained'`, run).Scan(&clearedAt))
	if f.scalar(`SELECT count(*) FROM workspaces WHERE id=$1 AND deleted_at IS NULL AND NOT admission_open`, workspace) != 1 {
		t.Fatal("termination must preserve data and keep admission closed")
	}
	if f.scalar(`SELECT count(*) FROM external_effects WHERE workspace_id=$1 AND kind='workspace_data_delete'`, workspace) != 0 {
		t.Fatal("terminate implicitly planned data deletion")
	}
	deleted := controlledDeleteInjected(t, f, c, f.tid, root, workspace)
	if deleted.O("intent").S("id") == root {
		t.Fatal("data deletion did not retain its separate command intent")
	}
	var retained bool
	must(t, f.store.Pool.QueryRow(`SELECT reclaim_state='cleared' AND disposition='released' AND cleared_at::text=$2
 AND (SELECT deleted_at IS NOT NULL AND NOT admission_open FROM workspaces WHERE id=workspace_id)
 FROM agent_run_residuals WHERE run_id=$1`, run, clearedAt).Scan(&retained))
	if !retained {
		t.Fatal("released completion reset the first clear time or lost actual Workspace deletion")
	}
	var after string
	must(t, f.store.Pool.QueryRow(`SELECT jsonb_build_object('run',to_jsonb(r),'reason',a.reason,'responsibility',a.responsibility,'operation',to_jsonb(o))::text
 FROM issue_runs r JOIN agent_run_residuals a ON a.run_id=r.id JOIN operations o ON o.id=$2 WHERE r.id=$1`, run, operation).Scan(&after))
	if historical != after {
		t.Fatal("controlled cleanup rewrote the original business settlement, reason, responsibility or failed creation")
	}
	got, err := f.store.ManageResidual(context.Background(), &core.ResidualManagementRequest{Action: core.ResidualGet, TenantID: f.tid, IntentID: root})
	must(t, err)
	if got.O("intent").S("state") != "succeeded" || got.O("intent").S("disposition") != "terminated_data_retained" {
		t.Fatalf("root lost its original retained completion after the child released data: %v", got)
	}
}

func TestControlledResidualHiddenWorkspaceRemains404ForAdminAndMember(t *testing.T) {
	f := controlledFixtureFactory(t)(t)
	scene := seedLiveThreadScene(t, f)
	seedControlledTerminalIntake(t, f, scene)
	workspace := f.runWorkspaceID(scene.runID)
	controlledTerminate(t, f, scene.tenantID, scene.runID, workspace)
	f.tid = scene.tenantID
	admin := f.user
	member, _ := f.addUser(t, "controlled-member", "Controlled member")
	for _, user := range []core.Claims{admin, member} {
		f.user = user
		f.call("GET", f.path("/workspaces/"+workspace), nil, "", 404)
		f.call("POST", f.path("/workspaces/"+workspace+"/force-stop"), core.Object{"version": 1, "reason": "Hidden run Workspace", "impactConfirmed": true}, uuid.NewString(), 404)
		f.call("DELETE", f.path("/workspaces/"+workspace), core.Object{"version": 1, "sessionId": uuid.NewString()}, uuid.NewString(), 404)
	}
}

// The original work and target are production-dispatched; only the already terminal business
// failure is a fixture. All controlled states below are reached through commands and gRPC facts.
func seedControlledTerminalIntake(t *testing.T, f *fixture, scene liveThreadScene) {
	t.Helper()
	_, err := f.store.Pool.Exec(`UPDATE issue_runs SET phase='done',status='failed',thread_state='ended',idle_since=NULL,
 failure_reason='agent_start_failed',completed_at=clock_timestamp(),version=version+1,
 result='{"deliveryState":"failed","testBoundary":"preexisting_business_terminal"}'::jsonb WHERE id=$1`, scene.runID)
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE workspaces SET admission_open=false,admission_epoch=admission_epoch+1,version=version+1 WHERE issue_run_id=$1`, scene.runID)
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO agent_run_residuals(run_id,tenant_id,project_id,workspace_id,runtime_generation,responsibility,reason)
 SELECT $1::uuid,w.tenant_id,w.project_id,w.id,w.runtime_generation,
 jsonb_build_object('executions',(SELECT jsonb_agg(jsonb_build_object('executionId',e.execution_id)) FROM node_executions e WHERE e.workspace_id=w.id)),
 'agent_start_failed' FROM workspaces w WHERE w.issue_run_id=$1::uuid`, scene.runID)
	must(t, err)
}

func TestControlledResidualTerminalFactsAcrossRealStatesOverGRPC(t *testing.T) {
	newFixture := controlledFixtureFactory(t)
	for _, delivery := range []bool{false, true} {
		for _, state := range []string{"terminating", "retained", "released"} {
			name := "session/" + state
			if delivery {
				name = "delivery/" + state
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				var scene liveThreadScene
				var execution string
				sequence := uint64(2)
				if delivery {
					scene = deliveringScene(t, f)
					execution = f.deliverRevision(t, scene)
					sequence = 1
				} else {
					scene = seedLiveThreadScene(t, f)
					scene.start(t, f)
					f.runningThread(t, scene)
					execution = scene.executionID
				}
				seedControlledTerminalIntake(t, f, scene)
				workspace := f.runWorkspaceID(scene.runID)
				accepted := controlledTerminate(t, f, scene.tenantID, scene.runID, workspace)
				root := accepted.O("intent").S("id")
				// seedLiveThreadScene owns the existing ctrl-a lease used by fact takeover.
				c := &grpcController{t: t, f: f, ctx: asController("ctrl-a"), operations: controlpb.NewWorkspaceOperationServiceClient(f.controlConn), epoch: 1}
				if state != "terminating" {
					controlledConfirmInjected(t, f, c, root)
				}
				if state == "released" {
					controlledDeleteInjected(t, f, c, scene.tenantID, root, workspace)
				}
				controlledTerminalIntakeCase(t, f, scene, execution, sequence, delivery)
				_, err := f.store.ManageResidual(context.Background(), &core.ResidualManagementRequest{Action: core.ResidualDeleteData, TenantID: scene.tenantID, IntentID: root, Key: "after-late-conflict", Reason: "Cannot bypass a committed physical fact conflict", DataDeleteConfirmed: true})
				if err == nil {
					t.Fatal("a management command bypassed the durable late-fact conflict")
				}
			})
		}
	}
}

// A retained/released responsibility already contains physical disposal evidence. Rollback must
// preserve that complete baseline, rather than requiring an empty ledger as the step-1 scene did.
func controlledTerminalIntakeCase(t *testing.T, f *fixture, scene liveThreadScene, execution string, sequence uint64, delivery bool) {
	t.Helper()
	businessBefore := residualIntakeSnapshot(t, f, scene)
	const intakeSnapshot = `SELECT jsonb_build_object('execution',to_jsonb(e),'residual',to_jsonb(r),
 'receipts',(SELECT jsonb_agg(to_jsonb(n) ORDER BY sequence) FROM node_event_receipts n WHERE n.execution_id=e.execution_id),
 'verifications',(SELECT jsonb_agg(to_jsonb(v)) FROM revision_verifications v WHERE v.execution_id=e.execution_id),
 'revisions',(SELECT jsonb_agg(to_jsonb(v)) FROM revisions v WHERE v.execution_id=e.execution_id))::text
 FROM node_executions e JOIN agent_run_residuals r ON r.run_id=e.operation_id WHERE e.execution_id=$1`
	var before, after string
	must(t, f.store.Pool.QueryRow(intakeSnapshot, execution).Scan(&before))
	in := f.deliveryInputOf(execution)
	result := sessionEndResult(scene, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "")
	conflict := sessionEndResult(scene, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_INTERRUPTED, "")
	kind, outcome := "agent_session", "agent_session_ended"
	if delivery {
		kind, outcome = "deliver_revision", "revision_delivered"
		result = deliveredResult(scene, in, revisionFinalCommit)
		conflict = deliveredResult(scene, in, strings.Repeat("2", 40))
		f.push(in, revisionFinalCommit)
	}
	request := func(submission string, fact *controlpb.ExecutionResult) *controlpb.TakeOverNodeEventRequest {
		return &controlpb.TakeOverNodeEventRequest{
			SubmissionId: submission, Epoch: 1, OperationId: scene.runID,
			ExecutionId: execution, Sequence: sequence, Result: fact, Event: []byte("controlled original residual terminal event"),
		}
	}
	ctx := asController("ctrl-a")
	_, err := f.store.Pool.Exec(`CREATE FUNCTION reject_controlled_intake_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected controlled evidence persistence failure'; END $$;
 CREATE TRIGGER reject_controlled_intake_evidence BEFORE UPDATE OF reclaim_evidence ON agent_run_residuals
 FOR EACH ROW EXECUTE FUNCTION reject_controlled_intake_evidence()`)
	must(t, err)
	response, err := f.executions.TakeOverNodeEvent(ctx, request("controlled-terminal", result))
	if err == nil || response != nil {
		t.Fatalf("failed evidence persistence was acknowledged: response=%v error=%v", response, err)
	}
	must(t, f.store.Pool.QueryRow(intakeSnapshot, execution).Scan(&after))
	if before != after {
		t.Fatalf("failed intake did not restore its exact execution/receipt/Revision/ledger baseline:\nbefore=%s\nafter=%s", before, after)
	}
	_, err = f.store.Pool.Exec("DROP TRIGGER reject_controlled_intake_evidence ON agent_run_residuals; DROP FUNCTION reject_controlled_intake_evidence()")
	must(t, err)
	response, err = f.executions.TakeOverNodeEvent(ctx, request("controlled-terminal", result))
	must(t, err)
	if response.GetRecord().GetExecutionId() != execution || response.GetRecord().GetResult() == nil {
		t.Fatalf("successful ACK lost the original execution/result: %v", response)
	}
	var accepted bool
	must(t, f.store.Pool.QueryRow(`SELECT e.result->>'outcome'=$3 AND e.last_event_sequence=$2
 AND EXISTS(SELECT 1 FROM node_event_receipts WHERE execution_id=e.execution_id AND sequence=$2)
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(r.reclaim_evidence) x WHERE x->>'key'='execution:'||e.execution_id
 AND x->'fact'->>'kind'=$4 AND x->'fact'->>'state'='succeeded' AND x->>'receivedAt' IS NOT NULL)
 FROM node_executions e JOIN agent_run_residuals r ON r.run_id=e.operation_id WHERE e.execution_id=$1`, execution, sequence, outcome, kind).Scan(&accepted))
	if !accepted || f.receiptEvent(execution, int64(sequence)) != "controlled original residual terminal event" {
		t.Fatal("ACK preceded the durable canonical result, original event receipt or residual evidence")
	}
	if delivery {
		row := f.revisionRow(scene.runID)
		project, repository := f.runPlacement(scene.runID)
		if row == nil || row.RunID != scene.runID || row.TenantID != scene.tenantID || row.WorkspaceID != f.runWorkspaceID(scene.runID) || row.ProjectID != project || row.RepositoryURL != repository || row.FinalCommit != revisionFinalCommit || row.BaseCommit != in.S("baseCommit") || row.RevisionRef != in.S("revisionRef") || row.DeliveryRef != in.S("deliveryRef") || row.HistoryKey != in.S("historyKey") {
			t.Fatalf("late verified delivery registered the wrong original Revision: %#v", row)
		}
		if row.BundleKey != "" || row.BundleSize != nil {
			t.Fatalf("late Git delivery registered a retired bundle: %#v", row)
		}
		if verdict := f.revisionVerification(execution); verdict.S("outcome") != "delivered" {
			t.Fatalf("actual Git and history verification is missing: %v", verdict)
		}
		refs := f.refs.queried()
		if len(refs) != 2 || refs[0] != in.S("deliveryRef") || refs[1] != in.S("deliveryRef") {
			t.Fatalf("failed intake and successful retry did not verify the declared Git delivery ref: %v", refs)
		}
		probes := f.objects.probed()
		if len(probes) != 2 || probes[0].S("key") != in.S("historyKey") || probes[1].S("key") != in.S("historyKey") {
			t.Fatalf("failed intake and successful retry did not verify the declared history: %v", probes)
		}
	}
	var canonical string
	var evidenceVersion int64
	must(t, f.store.Pool.QueryRow(`SELECT e.result::text,r.reclaim_version FROM node_executions e
 JOIN agent_run_residuals r ON r.run_id=e.operation_id WHERE e.execution_id=$1`, execution).Scan(&canonical, &evidenceVersion))
	for _, submission := range []string{"controlled-terminal", "controlled-terminal-replay"} {
		_, err = f.executions.TakeOverNodeEvent(ctx, request(submission, result))
		must(t, err)
	}
	var replayStable bool
	must(t, f.store.Pool.QueryRow(`SELECT reclaim_version=$2 AND
 (SELECT count(*) FROM node_event_receipts WHERE execution_id=$1 AND sequence=$3)=1
 FROM agent_run_residuals WHERE run_id=$4`, execution, evidenceVersion, sequence, scene.runID).Scan(&replayStable))
	if !replayStable {
		t.Fatal("replayed original terminal duplicated residual evidence or its receipt")
	}
	response, err = f.executions.TakeOverNodeEvent(ctx, request("controlled-terminal-conflict", conflict))
	if response != nil || status.Convert(err).Message() != "residual_fact_conflict" {
		t.Fatalf("canonical contradiction did not return committed residual conflict: response=%v error=%v", response, err)
	}
	var audited bool
	must(t, f.store.Pool.QueryRow(`SELECT r.reclaim_conflicted_at IS NOT NULL AND e.result=$2::jsonb
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(r.reclaim_evidence) x WHERE x->>'key'='execution:'||e.execution_id
 AND x->'fact'->>'kind'='conflict' AND x->'fact'->'original'->'result'=$2::jsonb
 AND x->'fact'->'incoming'->'result'<>$2::jsonb)
 AND (SELECT count(*) FROM node_event_receipts WHERE execution_id=e.execution_id AND sequence=$3)=1
 FROM node_executions e JOIN agent_run_residuals r ON r.run_id=e.operation_id WHERE e.execution_id=$1`, execution, canonical, sequence).Scan(&audited))
	if !audited {
		t.Fatal("conflict response rolled back its audit or changed the canonical result/receipt")
	}
	if delivery && (len(f.refs.queried()) != 2 || len(f.objects.probed()) != 2) {
		t.Fatal("known replay or contradiction repeated Git or history verification")
	}
	if businessAfter := residualIntakeSnapshot(t, f, scene); businessAfter != businessBefore {
		t.Fatalf("late controlled facts resurrected business state, admission or dispatch:\nbefore=%s\nafter=%s", businessBefore, businessAfter)
	}
}

func TestControlledResidualProvisioningFactConflictCommitsBeforeGRPCResponse(t *testing.T) {
	f := controlledFixtureFactory(t)(t)
	f.bindBusinessHooks()
	created := f.create("controlled-late-create")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	provisioned, err := f.store.CreateRunWorkspace(context.Background(), run)
	must(t, err)
	workspace, operation := provisioned.O("workspace").S("id"), provisioned.O("operation").S("id")
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET phase='provisioning',status='dispatched',workspace_id=$2,version=version+1 WHERE id=$1`, run, workspace)
	must(t, err)
	c := newGRPCController(t, f)
	op := c.claim().GetOperation()
	planned, err := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: op.GetVersion(), WorkspaceId: workspace, Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE})
	must(t, err)
	_, err = c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: planned.GetOperation().GetVersion(), State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60})
	must(t, err)
	_, err = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
	must(t, err)
	must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
	accepted := controlledTerminate(t, f, f.tid, run, workspace)
	controlledConfirmInjected(t, f, c, accepted.O("intent").S("id"))
	fact := &controlpb.RecordProvisioningFactRequest{
		SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT,
		EffectId: planned.GetEffect().GetId(), State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED, ExternalId: "late-original-ensure",
		Evidence: &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: planned.GetEffect().GetId(), NodeId: "late-node"}}},
	}
	_, err = c.operations.RecordProvisioningFact(c.ctx, fact)
	must(t, err)
	fact.SubmissionId, fact.State, fact.Evidence = c.sub(), controlpb.EffectState_EFFECT_STATE_FAILED, nil
	failure := "contradictory original ensure result"
	fact.Failure = &failure
	_, err = c.operations.RecordProvisioningFact(c.ctx, fact)
	if status.Convert(err).Message() != "residual_fact_conflict" {
		t.Fatalf("late provisioning contradiction did not return its stable conflict: %v", err)
	}
	var durable bool
	must(t, f.store.Pool.QueryRow(`SELECT a.reclaim_conflicted_at IS NOT NULL AND a.reclaim_state='cleared'
 AND a.disposition='terminated_data_retained' AND a.cleared_at IS NOT NULL
 AND r.phase='done' AND r.status='failed' AND NOT w.admission_open AND w.deleted_at IS NULL
 AND e.state='succeeded' AND e.external_id='late-original-ensure'
 AND EXISTS(SELECT 1 FROM jsonb_array_elements(a.reclaim_evidence) x WHERE x->'fact'->>'kind'='conflict')
 FROM agent_run_residuals a JOIN issue_runs r ON r.id=a.run_id JOIN workspaces w ON w.id=a.workspace_id
 JOIN external_effects e ON e.id=$2 WHERE a.run_id=$1`, run, planned.GetEffect().GetId()).Scan(&durable))
	if !durable {
		t.Fatal("conflict response preceded durable evidence or resurrected original state")
	}
}
