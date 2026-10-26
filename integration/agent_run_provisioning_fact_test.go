package integration

import (
	"context"
	"database/sql"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// D5's late-fact takeover over the real gRPC contract: a Controller that learns a blocked creation's
// real Substrate outcome after Cloud abandoned the run submits it under its current lease, gets it
// receipted on the original records and the residual ledger, and nothing reopens.
func TestRecordProvisioningFactReceiptsLateFactsOverGRPCWithoutResurrectingRun(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	created := f.create("d5-provisioning-fact")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	provisioned, e := f.store.CreateRunWorkspace(context.Background(), run)
	must(t, e)
	workspace := provisioned.O("workspace").S("id")
	operation := provisioned.O("operation").S("id")
	_, e = f.store.Pool.Exec(`UPDATE issue_runs SET phase='provisioning', status='dispatched', workspace_id=$2, version=version+1 WHERE id=$1`, run, workspace)
	must(t, e)

	c := newGRPCController(t, f)
	snap := c.claim()
	if snap.GetOperation().GetId() != operation {
		t.Fatalf("claim picked %s, not the run creation %s", snap.GetOperation().GetId(), operation)
	}
	// The sandbox step plans its ensure; the Substrate result stays unknown when the creation blocks.
	planned, e := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: snap.GetOperation().GetVersion(), Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, WorkspaceId: workspace})
	must(t, e)
	effectID := planned.GetEffect().GetId()
	_, e = c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: planned.GetOperation().GetVersion(), State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60})
	must(t, e)

	// Before the abandonment the entry is closed: an unfinished creation is not a residual.
	_, e = c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_CREATION, Outcome: controlpb.ProvisioningOutcome_PROVISIONING_OUTCOME_READY})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	_, e = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
	must(t, e)
	must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
	var responsibility string
	must(t, f.store.Pool.QueryRow(`SELECT responsibility::text FROM agent_run_residuals WHERE run_id=$1`, run).Scan(&responsibility))
	var phase, status, threadState sql.NullString
	must(t, f.store.Pool.QueryRow(`SELECT phase, status, thread_state FROM issue_runs WHERE id=$1`, run).Scan(&phase, &status, &threadState))
	if phase.String != "done" || status.String != "failed" || threadState.Valid {
		t.Fatalf("blocked creation was not abandoned: %s/%s/%v", phase.String, status.String, threadState.Valid)
	}

	// The late Substrate fact: the ensure really did create a sandbox, on a Node Cloud never saw.
	fact, e := c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{
		SubmissionId: "fact-effect", Epoch: c.epoch, OperationId: operation,
		Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT, EffectId: effectID,
		State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED, ExternalId: "substrate-sandbox-1",
		Evidence: &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: effectID, NodeId: "node-late"}}},
	})
	must(t, e)
	if fact.GetOperation().GetState() != controlpb.OperationState_OPERATION_STATE_FAILED || fact.GetEffect().GetState() != controlpb.EffectState_EFFECT_STATE_SUCCEEDED {
		t.Fatalf("late effect fact not receipted: operation=%s effect=%s", fact.GetOperation().GetState(), fact.GetEffect().GetState())
	}
	// The same submission replays its recorded response.
	replayed, e := c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{
		SubmissionId: "fact-effect", Epoch: c.epoch, OperationId: operation,
		Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT, EffectId: effectID,
		State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED, ExternalId: "substrate-sandbox-1",
		Evidence: &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: effectID, NodeId: "node-late"}}},
	})
	must(t, e)
	if replayed.GetEffect().GetId() != fact.GetEffect().GetId() {
		t.Fatal("submission replay returned a different fact")
	}

	// The creation's own real outcome is evidence only.
	_, e = c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{SubmissionId: "fact-creation", Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_CREATION, Outcome: controlpb.ProvisioningOutcome_PROVISIONING_OUTCOME_READY})
	must(t, e)

	// Nothing resurrected: the business terminal stands, no delete was queued, the Workspace keeps
	// its admission fence, and the ledger holds exactly the two receipted facts.
	must(t, f.store.Pool.QueryRow(`SELECT phase, status FROM issue_runs WHERE id=$1`, run).Scan(&phase, &status))
	if phase.String != "done" || status.String != "failed" {
		t.Fatalf("late facts resurrected the run: %s/%s", phase.String, status.String)
	}
	if n := f.scalar(`SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='delete_workspace'`, workspace); n != 0 {
		t.Fatalf("late facts queued a delete: %d", n)
	}
	if open := f.scalar(`SELECT count(*) FROM workspaces WHERE id=$1 AND admission_open`, workspace); open != 0 {
		t.Fatal("late creation success reopened admission")
	}
	if n := f.scalar(`SELECT jsonb_array_length(reclaim_evidence) FROM agent_run_residuals WHERE run_id=$1`, run); n != 2 {
		t.Fatalf("ledger receipts wrong: %d", n)
	}
	if n := f.scalar(`SELECT count(*) FROM agent_run_residuals WHERE run_id=$1 AND responsibility=$2::jsonb`, run, responsibility); n != 1 {
		t.Fatal("late facts changed the original residual responsibility")
	}
}

// The gRPC contract carries a fact's target in dedicated fields, so the submission fingerprint
// binds them: a replay naming a different operation or effect is a conflict, never a replay for
// the original target. The same contract lets a provisional observation (running) progress to
// the real terminal under a new submission.
func TestRecordProvisioningFactBindsTargetAndProgressesOverGRPC(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	created := f.create("d5-fact-bind-target")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	provisioned, e := f.store.CreateRunWorkspace(context.Background(), run)
	must(t, e)
	workspace := provisioned.O("workspace").S("id")
	operation := provisioned.O("operation").S("id")
	_, e = f.store.Pool.Exec(`UPDATE issue_runs SET phase='provisioning', status='dispatched', workspace_id=$2, version=version+1 WHERE id=$1`, run, workspace)
	must(t, e)

	c := newGRPCController(t, f)
	snap := c.claim()
	planned, e := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: snap.GetOperation().GetVersion(), Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, WorkspaceId: workspace})
	must(t, e)
	effectID := planned.GetEffect().GetId()
	_, e = c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: operation, Version: planned.GetOperation().GetVersion(), State: controlpb.DeferState_DEFER_STATE_BLOCKED, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 60})
	must(t, e)
	_, e = f.store.Pool.Exec(`UPDATE issue_runs SET workspace_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1`, run)
	must(t, e)
	must(t, f.store.GiveUpBlockedWorkspaceCreationsOnce(context.Background()))
	var responsibility string
	must(t, f.store.Pool.QueryRow(`SELECT responsibility::text FROM agent_run_residuals WHERE run_id=$1`, run).Scan(&responsibility))

	// A recorded creation fact...
	_, e = c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{SubmissionId: "bound-creation", Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_CREATION, Outcome: controlpb.ProvisioningOutcome_PROVISIONING_OUTCOME_READY})
	must(t, e)
	// ...replayed against another operation is a conflict, not a replay for the original target.
	_, e = c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{SubmissionId: "bound-creation", Epoch: c.epoch, OperationId: "6f1d2b34-0000-4000-8000-000000000001", Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_CREATION, Outcome: controlpb.ProvisioningOutcome_PROVISIONING_OUTCOME_READY})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// A provisional effect observation, then the same submission against another effect target.
	_, e = c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{SubmissionId: "bound-effect", Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT, EffectId: effectID, State: controlpb.EffectState_EFFECT_STATE_RUNNING, ExternalId: "sub-bound"})
	must(t, e)
	_, e = c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{SubmissionId: "bound-effect", Epoch: c.epoch, OperationId: operation, Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT, EffectId: "6f1d2b34-0000-4000-8000-000000000002", State: controlpb.EffectState_EFFECT_STATE_RUNNING, ExternalId: "sub-bound"})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// The real terminal arrives under a new submission and takes over from the observation.
	terminal, e := c.operations.RecordProvisioningFact(c.ctx, &controlpb.RecordProvisioningFactRequest{
		SubmissionId: "bound-terminal", Epoch: c.epoch, OperationId: operation,
		Kind: controlpb.ProvisioningFactKind_PROVISIONING_FACT_KIND_EFFECT, EffectId: effectID,
		State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED, ExternalId: "sub-bound",
		Evidence: &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: effectID, NodeId: "node-late"}}},
	})
	must(t, e)
	if terminal.GetEffect().GetState() != controlpb.EffectState_EFFECT_STATE_SUCCEEDED {
		t.Fatalf("terminal after running not receipted: %s", terminal.GetEffect().GetState())
	}
	// Provisional and terminal observations remain independent append-only evidence entries.
	var states string
	var entries int
	must(t, f.store.Pool.QueryRow(`SELECT string_agg(e->'fact'->>'state', ',' ORDER BY ordinal), count(*)
	 FROM agent_run_residuals r, jsonb_array_elements(r.reclaim_evidence) WITH ORDINALITY AS facts(e,ordinal)
	 WHERE r.run_id=$1 AND e->>'key'='effect:'||$2`, run, effectID).Scan(&states, &entries))
	if states != "running,succeeded" || entries != 2 {
		t.Fatalf("ledger observations wrong: states=%s entries=%d", states, entries)
	}
	if n := f.scalar(`SELECT count(*) FROM agent_run_residuals WHERE run_id=$1 AND responsibility=$2::jsonb`, run, responsibility); n != 1 {
		t.Fatal("late observations changed the original residual responsibility")
	}
	if n := f.scalar(`SELECT count(*) FROM workspaces WHERE id=$1 AND admission_open`, workspace); n != 0 {
		t.Fatal("late terminal reopened admission")
	}
}
