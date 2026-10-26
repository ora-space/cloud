package core

import (
	"testing"

	"github.com/google/uuid"
)

// A delivery deadline ends dispatch authority without inventing a Node terminal fact.
func TestExpiredArtifactExecutionClosesDispatchWithoutInventingResult(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	var closed, unknown bool
	var sequence, receipts int
	if err := store.Pool.QueryRow(`SELECT
		(to_jsonb(e)->>'business_settled_at') IS NOT NULL,
		result IS NULL, last_event_sequence,
		(SELECT count(*) FROM node_event_receipts WHERE execution_id=e.execution_id)
		FROM node_executions e WHERE execution_id=$1`, execution).Scan(&closed, &unknown, &sequence, &receipts); err != nil {
		t.Fatal(err)
	}
	if !closed || !unknown || sequence != 0 || receipts != 0 {
		t.Fatalf("deadline must close dispatch while retaining unknown execution: closed=%v unknown=%v sequence=%d receipts=%d", closed, unknown, sequence, receipts)
	}
	if !pendingExecution(t, store, scene, execution) {
		t.Fatal("business-only settlement must retain unknown execution responsibility")
	}
}

// declaredDelivery uses the actual declaration and work claim seams, with no invented Node result.
func declaredDelivery(t *testing.T) (*Store, takeoverScene, Object) {
	t.Helper()
	store := dispatcherDB(t)
	store.ObjectStore, _ = cleanupObjectStore(t)
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	_, err := store.Control(t.Context(), &ControlRequest{
		Action: "clone_takeover", Service: scene.claims, Body: Object{
			"epoch": 1, "operationId": scene.run, "executionId": scene.execution,
			"sequence": 1, "event": "declared", "result": Object{
				"node": Object{"nodeId": scene.node}, "outcome": "agent_session_ended",
				"reason": "idle_timeout", "artifactManifest": scheduledManifest(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	work := claimAgentWork(t, store, scene.claims)
	if work.S("kind") != "deliver_scheduled_artifacts" {
		t.Fatalf("claimed %q", work.S("kind"))
	}
	return store, scene, work
}

// expiredDeliveryExecution reproduces the registered-but-unaccepted upload's deadline path.
func expiredDeliveryExecution(t *testing.T) (*Store, takeoverScene, string) {
	t.Helper()
	store, scene, work := declaredDelivery(t)
	const execution = "deadline-unaccepted-artifacts"
	if _, err := dispatchAgentWork(t, store, scene.claims, work, execution, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireScheduledArtifactDeliveriesOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store, scene, execution
}

// pendingExecution observes the production recovery list rather than interpreting a settlement.
func pendingExecution(t *testing.T, store *Store, scene takeoverScene, execution string) bool {
	t.Helper()
	out, err := store.Control(t.Context(), &ControlRequest{Action: "clone_pending", Service: scene.claims, Body: Object{"nodeId": scene.node}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range objectRows(out["executions"]) {
		if row.S("executionId") == execution {
			return true
		}
	}
	return false
}

// originalProof represents the trusted Node's separately tested durable fence response.
func originalProof(t *testing.T, store *Store, scene takeoverScene) Object {
	t.Helper()
	out, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_execution_fences", Service: scene.claims, Body: Object{"epoch": 1, "nodeId": scene.node}})
	if err != nil {
		t.Fatal(err)
	}
	fences := objectRows(out["fences"])
	if len(fences) != 1 {
		t.Fatalf("original execution fence = %v", out)
	}
	return Object{"proofId": uuid.NewString(), "fence": fences[0]}
}

// A committed Node proof removes only its own responsibility, without a terminal result or receipt.
func TestNonAcceptanceProofSettlesResponsibilityAndProtectsHistory(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	proof := originalProof(t, store, scene)
	var before, after string
	if err := store.Pool.QueryRow("SELECT to_jsonb(r)::text FROM issue_runs r WHERE id=$1", scene.run).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.Control(t.Context(), &ControlRequest{
			Action: "runtime_execution_non_acceptance", Service: scene.claims,
			SubmissionID: uuid.NewString(), Body: Object{"epoch": 1, "proof": proof},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if pendingExecution(t, store, scene, execution) {
		t.Fatal("committed proof must remove its exact pending responsibility")
	}
	if err := store.Pool.QueryRow("SELECT to_jsonb(r)::text FROM issue_runs r WHERE id=$1", scene.run).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("non-acceptance proof rewrote historical Run")
	}
	var unknown bool
	var receipts int
	if err := store.Pool.QueryRow(`SELECT result IS NULL,(SELECT count(*) FROM node_event_receipts WHERE execution_id=$1)
		FROM node_executions WHERE execution_id=$1`, execution).Scan(&unknown, &receipts); err != nil {
		t.Fatal(err)
	}
	if !unknown || receipts != 0 {
		t.Fatal("non-acceptance proof invented an execution terminal or receipt")
	}
}

// Frozen work, input and every original identity stay authoritative even under the right lease.
func TestNonAcceptanceProofRefusesWrongOriginalIdentity(t *testing.T) {
	for _, field := range []string{"workId", "inputJson", "tenantId", "workspaceId", "sandboxId", "nodeId", "nodeInstanceId", "nodeIncarnationId", "executionId", "nodeOperationId", "runtimeGeneration"} {
		t.Run(field, func(t *testing.T) {
			store, scene, execution := expiredDeliveryExecution(t)
			proof := originalProof(t, store, scene)
			origin := proof.O("fence").O("origin")
			switch field {
			case "workId", "inputJson":
				origin[field] = "different"
			case "runtimeGeneration":
				origin.O("binding")[field] = int64(999)
			default:
				origin.O("binding")[field] = uuid.NewString()
			}
			_, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_execution_non_acceptance", Service: scene.claims, Body: Object{"epoch": 1, "proof": proof}})
			if err == nil {
				t.Fatal("wrong original identity was accepted")
			}
			if !pendingExecution(t, store, scene, execution) {
				t.Fatal("identity refusal discarded pending responsibility")
			}
		})
	}
}

// A real terminal contradicting a committed absence proof is retained as conflict, never ACKed.
func TestNonAcceptanceContradictoryTerminalIsAuditedWithoutACK(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	proof := originalProof(t, store, scene)
	if _, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_execution_non_acceptance", Service: scene.claims, Body: Object{"epoch": 1, "proof": proof}}); err != nil {
		t.Fatal(err)
	}
	submission := uuid.NewString()
	for range 2 {
		_, err := store.Control(t.Context(), &ControlRequest{Action: "clone_takeover", Service: scene.claims, SubmissionID: submission, Body: Object{
			"epoch": 1, "operationId": scene.run, "executionId": execution, "sequence": 1, "event": "real-terminal",
			"result": Object{"node": Object{"nodeId": scene.node}, "outcome": "scheduled_artifacts_failed", "reason": "interrupted"},
		}})
		if err == nil {
			t.Fatal("contradictory terminal must not authorize ACK")
		}
	}
	var conflicts, receipts int
	if err := store.Pool.QueryRow(`SELECT (SELECT count(*) FROM execution_non_acceptance_conflicts WHERE execution_id=$1),
		(SELECT count(*) FROM node_event_receipts WHERE execution_id=$1)`, execution).Scan(&conflicts, &receipts); err != nil {
		t.Fatal(err)
	}
	if conflicts != 1 || receipts != 0 {
		t.Fatalf("conflicts=%d receipts=%d; want 1/0", conflicts, receipts)
	}
}

// A different absence proof cannot become successful merely by replaying a cached submission.
func TestConflictingNonAcceptanceProofSubmissionReplayStaysRejected(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	proof := originalProof(t, store, scene)
	if _, err := store.Control(t.Context(), &ControlRequest{
		Action: "runtime_execution_non_acceptance", Service: scene.claims,
		Body: Object{"epoch": 1, "proof": proof},
	}); err != nil {
		t.Fatal(err)
	}
	conflicting := mustObject(t, []byte(jsonText(proof)))
	conflicting["proofId"] = uuid.NewString()
	submission := uuid.NewString()
	for range 2 {
		if _, err := store.Control(t.Context(), &ControlRequest{
			Action: "runtime_execution_non_acceptance", Service: scene.claims,
			SubmissionID: submission, Body: Object{"epoch": 1, "proof": conflicting},
		}); err == nil {
			t.Fatal("conflicting proof submission replay returned success")
		}
	}
	var canonical []byte
	if err := store.Pool.QueryRow("SELECT non_acceptance_proof FROM node_executions WHERE execution_id=$1", execution).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if jsonText(mustObject(t, canonical)) != jsonText(proof) {
		t.Fatal("conflicting proof replaced canonical evidence")
	}
	if err := store.view(t.Context(), func(tx *transaction) {
		requireNoResidualConflict(tx, proof.O("fence").O("origin").O("binding").S("workspaceId"))
	}); err == nil {
		t.Fatal("contradictory proof was treated as idle")
	}
}

// A previously claimed upload cannot be registered after the absolute deadline, before scanning.
func TestArtifactDeadlineFencesStaleClaimBeforeExpiryScan(t *testing.T) {
	store, scene, work := declaredDelivery(t)
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAgentWork(t, store, scene.claims, work, "stale-claim", "", nil); err == nil {
		t.Fatal("stale claim admitted after artifact deadline")
	}
	claimed, err := store.Control(t.Context(), &ControlRequest{Action: "clone_claim", Service: scene.claims, Body: Object{"epoch": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.O("work").S("kind") == "deliver_scheduled_artifacts" {
		t.Fatal("expired work still occupies the claim queue")
	}
}

// A registered upload is permitted before its deadline and refused at the boundary, without a scan.
func TestArtifactDeadlineClosesFreshPermitBeforeExpiryScan(t *testing.T) {
	store, scene, work := declaredDelivery(t)
	const execution = "permit-bound-upload"
	if _, err := dispatchAgentWork(t, store, scene.claims, work, execution, "", nil); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := store.Pool.QueryRow("SELECT dispatch_identity FROM node_executions WHERE execution_id=$1", execution).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	binding := mustObject(t, raw).O("binding")
	bindings, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_pending", Service: scene.claims, Body: Object{"epoch": 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range objectRows(bindings["bindings"]) {
		if current.S("workspaceId") == binding.S("workspaceId") {
			if _, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_ack", Service: scene.claims, Body: Object{
				"epoch": 1, "workspaceId": current.S("workspaceId"), "nodeInstanceId": current.S("nodeInstanceId"),
				"controlEpoch": current.N("controlEpoch"), "controlVersion": current.N("controlVersion"),
				"inputClosed": false, "unfinishedExecutionIds": []any{},
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	request := &ControlRequest{Action: "runtime_permit", Service: scene.claims, Body: Object{
		"epoch": 1, "workspaceId": binding.S("workspaceId"), "nodeInstanceId": binding.S("nodeInstanceId"),
		"controlEpoch": binding.N("controlEpoch"), "executionId": execution,
	}}
	if _, err := store.Control(t.Context(), request); err != nil {
		t.Fatalf("fresh upload permit refused: %v", err)
	}
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Control(t.Context(), request); err == nil {
		t.Fatal("a fresh permit crossed the absolute deadline")
	}
	if !pendingExecution(t, store, scene, execution) {
		t.Fatal("refusing a permit lost the unresolved execution")
	}
}

// Losing a proof-write response is recoverable from explicit committed facts, never list absence.
func TestCommittedNonAcceptanceProofIsReadBackAfterLeaseReplacement(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	proof := originalProof(t, store, scene)
	if _, err := store.Control(t.Context(), &ControlRequest{
		Action: "runtime_execution_non_acceptance", Service: scene.claims,
		Body: Object{"epoch": 1, "proof": proof},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec("UPDATE controller_leases SET epoch=2,expires_at=clock_timestamp()+interval '30 seconds' WHERE name='global'"); err != nil {
		t.Fatal(err)
	}
	out, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_execution_fences", Service: scene.claims, Body: Object{"epoch": 2, "nodeId": scene.node}})
	if err != nil {
		t.Fatal(err)
	}
	committed := objectRows(out["committed"])
	if len(committed) != 1 || committed[0].S("executionId") != execution || len(objectRows(out["fences"])) != 0 {
		t.Fatalf("lost response recovery = %v", out)
	}
}

// A failed persistence transaction cannot remove pending responsibility or return a proof receipt.
func TestNonAcceptanceWriteFailureRetainsPendingResponsibility(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	proof := originalProof(t, store, scene)
	if _, err := store.Pool.Exec(`CREATE FUNCTION fail_non_acceptance() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected'; END$$;
		CREATE TRIGGER fail_non_acceptance BEFORE UPDATE ON node_executions FOR EACH ROW WHEN (NEW.non_acceptance_proof IS NOT NULL) EXECUTE FUNCTION fail_non_acceptance()`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Control(t.Context(), &ControlRequest{
		Action: "runtime_execution_non_acceptance", Service: scene.claims,
		Body: Object{"epoch": 1, "proof": proof},
	}); err == nil {
		t.Fatal("failed proof write returned success")
	}
	if !pendingExecution(t, store, scene, execution) {
		t.Fatal("failed proof write removed the execution")
	}
}

// A's business conclusion and original dispatch closure roll back together on database failure.
func TestArtifactDeadlineSettlementRollsBackAtomically(t *testing.T) {
	store, scene, work := declaredDelivery(t)
	const execution = "atomic-deadline-upload"
	if _, err := dispatchAgentWork(t, store, scene.claims, work, execution, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`CREATE FUNCTION fail_business_settlement() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected'; END$$;
		CREATE TRIGGER fail_business_settlement BEFORE UPDATE ON node_executions FOR EACH ROW WHEN (NEW.business_settled_at IS NOT NULL) EXECUTE FUNCTION fail_business_settlement()`); err != nil {
		t.Fatal(err)
	}
	if err := store.ExpireScheduledArtifactDeliveriesOnce(t.Context()); err == nil {
		t.Fatal("partial settlement committed")
	}
	var state string
	var cut bool
	if err := store.Pool.QueryRow(`SELECT d.state,e.business_settled_at IS NOT NULL FROM scheduled_run_artifact_deliveries d
		JOIN node_executions e ON e.execution_id=$2 WHERE d.issue_run_id=$1`, scene.run, execution).Scan(&state, &cut); err != nil {
		t.Fatal(err)
	}
	if state != "delivering" || cut {
		t.Fatalf("partial settlement: state=%s closed=%v", state, cut)
	}
}

// Legacy history gains an explicit administrative source, never an invented original Node scope.
func TestDeliverySettlementMigrationPreservesLegacyHistoryAndRepeats(t *testing.T) {
	store, scene, execution := expiredDeliveryExecution(t)
	const historical = `(to_jsonb(e)-'business_settled_at'-'business_settlement_source'-'dispatch_identity'-'non_acceptance_proof')::text`
	var before, after, runBefore, runAfter string
	if err := store.Pool.QueryRow("SELECT "+historical+" FROM node_executions e WHERE execution_id=$1", execution).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow("SELECT to_jsonb(r)::text FROM issue_runs r WHERE id=$1", scene.run).Scan(&runBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`DROP TABLE execution_non_acceptance_conflicts;
		ALTER TABLE node_executions DROP CONSTRAINT delivery_business_settlement_pair,
		DROP CONSTRAINT delivery_business_settlement_kind,DROP CONSTRAINT execution_non_acceptance_proof_shape,
		DROP COLUMN business_settled_at,DROP COLUMN business_settlement_source,DROP COLUMN dispatch_identity,DROP COLUMN non_acceptance_proof;
		DELETE FROM schema_migrations WHERE version='0072_delivery_execution_settlement.sql'`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.Migrate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CheckSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow("SELECT "+historical+" FROM node_executions e WHERE execution_id=$1", execution).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow("SELECT to_jsonb(r)::text FROM issue_runs r WHERE id=$1", scene.run).Scan(&runAfter); err != nil {
		t.Fatal(err)
	}
	if before != after || runBefore != runAfter {
		t.Fatal("migration rewrote original execution or Run history")
	}
	var source string
	var missingOrigin bool
	if err := store.Pool.QueryRow("SELECT business_settlement_source,dispatch_identity IS NULL FROM node_executions WHERE execution_id=$1", execution).Scan(&source, &missingOrigin); err != nil {
		t.Fatal(err)
	}
	if source != "artifact_deadline" || !missingOrigin {
		t.Fatalf("legacy settlement source=%s missingOrigin=%v", source, missingOrigin)
	}
	if !pendingExecution(t, store, scene, execution) {
		t.Fatal("legacy backfill erased unknown responsibility")
	}
	out, err := store.Control(t.Context(), &ControlRequest{Action: "runtime_execution_fences", Service: scene.claims, Body: Object{"epoch": 1, "nodeId": scene.node}})
	if err != nil {
		t.Fatal(err)
	}
	if len(objectRows(out["fences"])) != 0 {
		t.Fatal("current Node identity fabricated legacy proof eligibility")
	}
}
