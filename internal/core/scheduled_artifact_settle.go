package core

import "fmt"

// The artifact-delivery settlement behind the control plane's OnScheduledArtifactsSettled hook
// (scheduled-task verdict/artifacts ADR D3).
//
// It runs inside the transaction that takes the delivery execution's terminal event over, and the
// object verification it consumes was computed outside that transaction: the control plane asks the
// object store whether every reported object exists with the declared size and digest, binds the
// answer to the frozen execution input, and this function refuses an answer bound to any other input.
// Nothing here performs external I/O.
//
// Two rules from the ADR decide the outcome, and neither depends on the order things arrive in:
//
//	deadline   the database's own clock decides. A result that lands at or after `deadline_at`
//	           confirms the receipt, registers nothing and settles the delivery as failed, no
//	           matter whether the expiry pass has already run.
//	per item   each declared artifact is registered only when its own object verified. Successes are
//	           registered, everything else becomes a visible rejection, and the run's aggregate state
//	           says whether all, some or none of the declared artifacts made it.

const scheduledArtifactDeadlineExpired = "deadline_expired"

// scheduledArtifactsFailureReasons is the closed set an executor may report. Cloud's own
// verification and deadline verdicts are not part of it: they never come from the wire.
var scheduledArtifactsFailureReasons = map[string]bool{
	"workspace_unavailable": true, "upload_failed": true,
	"declaration_unavailable": true, "interrupted": true,
}

// settleScheduledArtifacts registers one scheduled run's verified artifacts and finishes its release
// when the Revision delivery has also settled.
func (s *Store) settleScheduledArtifacts(t *transaction, runID, executionID string, settled Object) error {
	if !validID(runID) {
		return fmt.Errorf("scheduledArtifactsSettled: invalid run id %q", runID)
	}
	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if o == nil {
		return fmt.Errorf("scheduledArtifactsSettled: issue_run %s not found", runID)
	}
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1 AND operation_id=$2", executionID, runID)
	if e == nil {
		return fmt.Errorf("scheduledArtifactsSettled: execution %s is not registered for run %s", executionID, runID)
	}
	if e.S("kind") != "deliver_scheduled_artifacts" {
		return fmt.Errorf("scheduledArtifactsSettled: execution %s is kind=%q, not deliver_scheduled_artifacts", executionID, e.S("kind"))
	}
	delivery := t.one("SELECT * FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1", runID)
	if delivery == nil {
		return fmt.Errorf("scheduledArtifactsSettled: run %s has no artifact delivery", runID)
	}
	// A replay, or a receipt that lands after the window already settled, decides nothing: the
	// receipt itself was committed by the takeover, which is what the Node needs to stop replaying.
	if delivery.S("state") != "delivering" {
		return nil
	}
	switch outcome := settled.S("outcome"); outcome {
	case "delivered":
		expected := settled.S("input")
		if expected == "" || expected != jsonText(e.O("input")) {
			return fmt.Errorf("scheduledArtifactsSettled: run %s execution %s carries a verification for another input", runID, executionID)
		}
	case "failed":
		if reason := settled.S("reason"); reason != "" && !scheduledArtifactsFailureReasons[reason] {
			return fmt.Errorf("scheduledArtifactsSettled: run %s carries failure reason %q, outside the closed set", runID, reason)
		}
	case "expired":
	default:
		return fmt.Errorf("scheduledArtifactsSettled: run %s carries unknown outcome %q", runID, settled.S("outcome"))
	}
	// The absolute deadline is judged on the database's clock, in this transaction, whatever the
	// expiry pass has done so far. now() is the transaction start, before waiting for the control-
	// plane lock, so only the actual clock after acquiring that lock can fence registration.
	open := t.one("SELECT clock_timestamp() < deadline_at AS open FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1", runID)
	if !open.B("open") {
		return s.closeArtifactDelivery(t, o, "failed", scheduledArtifactDeadlineExpired)
	}
	if settled.S("outcome") != "delivered" {
		return s.closeArtifactDelivery(t, o, "failed", settled.S("reason"))
	}
	declaration, err := scheduledArtifactDeclarationOf(t, delivery.S("tenantId"), e)
	if err != nil {
		return err
	}
	verified := map[string]bool{}
	for _, object := range objectRows(settled["objects"]) {
		verified[object.S("key")] = object.B("verified")
	}
	registered := 0
	for _, artifact := range declaration.Artifacts {
		reason := "upload_failed"
		if _, reported := verified[artifact.Key]; reported {
			reason = "verification_failed"
		}
		if verified[artifact.Key] {
			t.exec(`
				INSERT INTO scheduled_run_artifacts
					(tenant_id, issue_run_id, task_id, occurrence_id, scheduled_at, artifact_index, object_key, display_name, kind, size_bytes, sha256)
				SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11
				ON CONFLICT (issue_run_id, artifact_index) DO NOTHING`,
				delivery.S("tenantId"), runID, delivery.S("taskId"), delivery.S("occurrenceId"), delivery.S("scheduledAt"),
				artifact.Index, artifact.Key, artifact.DisplayName, artifact.Kind, artifact.Size, artifact.Sha256)
			registered++
			continue
		}
		rejectScheduledArtifact(t, delivery, runID, artifact.DisplayName, reason)
	}
	state, reason := "failed", "upload_failed"
	switch {
	case registered == len(declaration.Artifacts) && registered > 0:
		state, reason = "delivered", ""
	case registered > 0:
		state, reason = "partial", "upload_failed"
	}
	return s.closeArtifactDelivery(t, o, state, reason)
}

// closeArtifactDelivery settles the delivery row and releases the run when its Revision delivery has
// already settled as well.
func (s *Store) closeArtifactDelivery(t *transaction, o Object, state, reason string) error {
	if reason == scheduledArtifactDeadlineExpired {
		settleDeliveryExecutions(t, o.S("id"), "deliver_scheduled_artifacts", "artifact_deadline")
	}
	if state == "failed" || state == "partial" {
		// A whole-batch failure (including expiry before dispatch) has no per-object result.
		// The work item's frozen session binding still names the complete accepted manifest.
		// Preserve collection/verification refusals and registered successes; fill only missing
		// failure facts in the same transaction as the first settlement and release.
		t.exec(`
			INSERT INTO scheduled_run_artifact_rejections
				(tenant_id, issue_run_id, task_id, occurrence_id, scheduled_at, display_name, reason)
			SELECT d.tenant_id, d.issue_run_id, d.task_id, d.occurrence_id, d.scheduled_at,
				item->>'displayName', COALESCE(NULLIF($2,''),'upload_failed')
			FROM scheduled_run_artifact_deliveries d
			JOIN execution_work w ON w.run_id=d.issue_run_id AND w.kind='deliver_scheduled_artifacts'
			JOIN node_executions e ON e.execution_id=w.input->>'sessionExecutionId'
				AND e.operation_id=d.issue_run_id AND e.kind='agent_session'
			CROSS JOIN LATERAL jsonb_array_elements(e.result->'artifactManifest'->'items') AS item
			WHERE d.issue_run_id=$1 AND d.state='delivering'
				AND NOT EXISTS (SELECT 1 FROM scheduled_run_artifacts a
					WHERE a.issue_run_id=d.issue_run_id AND a.artifact_index=(item->>'index')::int)
			ON CONFLICT (issue_run_id, display_name) DO NOTHING`, o.S("id"), reason)
	}
	t.exec(`
		UPDATE scheduled_run_artifact_deliveries
		SET state=$2, settled_at=clock_timestamp(), settled_reason=NULLIF($3,'')
		WHERE issue_run_id=$1 AND state='delivering'`, o.S("id"), state, reason)
	// D6: the index this transaction just changed is what a reader re-reads, and the notice carries
	// only the identity of the run - never an artifact fact, which stays in the tables.
	invalidateScheduledArtifacts(t, o.S("id"))
	if !deliveryOutcomeRecorded(t, o.S("id")) {
		return nil
	}
	return s.finishRelease(t, o)
}

// rejectScheduledArtifact records one declared artifact that did not become a registered object.
func rejectScheduledArtifact(t *transaction, delivery Object, runID, displayName, reason string) {
	t.exec(`
		INSERT INTO scheduled_run_artifact_rejections
			(tenant_id, issue_run_id, task_id, occurrence_id, scheduled_at, display_name, reason)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (issue_run_id, display_name) DO NOTHING`,
		delivery.S("tenantId"), runID, delivery.S("taskId"), delivery.S("occurrenceId"), delivery.S("scheduledAt"),
		displayName, reason)
}

// invalidateScheduledArtifacts tells the run's space that its artifact index changed. The notice
// carries no artifact data: a reader re-reads the REST resources (D6), and nothing may treat the
// event as a fact source.
func invalidateScheduledArtifacts(t *transaction, runID string) {
	o := t.one(`SELECT r.issue_id, r.tenant_id FROM issue_runs r WHERE r.id=$1`, runID)
	space := t.one(`SELECT id FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL`, o.S("tenantId"))
	if space == nil {
		return
	}
	t.events = append(t.events, SpaceEvent{
		Type: "scheduled_task.artifacts_updated", SpaceID: space.S("id"), IssueID: o.S("issueId"), RunID: runID,
	})
}
