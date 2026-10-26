package core

import (
	"fmt"
	"time"
)

// The artifact-delivery half of a scheduled run: what the executor declared is persisted and given a
// delivery window in the session-terminal transaction, and the upload work item is released there.
//
// Cloud does not scan the Workspace and does not copy files: the Node uploads each declared artifact
// to a key Cloud chose, and a row only reaches `scheduled_run_artifacts` after Cloud verified the
// stored object's size and SHA-256. What is stored here is therefore the delivery state, the refused
// candidates and the frozen window - never the declaration itself, which stays in the immutable
// session terminal result.

// scheduledArtifactSkippedObjectStore is the settlement reason of a deployment without object
// storage: the run keeps its facts and releases, because no attempt could ever succeed.
const scheduledArtifactSkippedObjectStore = "object_store_unconfigured"

// recordScheduledArtifacts persists the artifacts a scheduled run declared, freezes the run's
// non-renewable delivery window and releases the upload work item.
//
// It runs in the caller's terminal-event transaction, so the declaration, the window and the work
// item commit or roll back with the Thread's terminal state. A run that is not an admitted scheduled
// occurrence, or a declaration outside the frozen bounds, is refused rather than stored: the executor
// was told to report artifacts Cloud cannot attribute or verify.
func recordScheduledArtifacts(t *transaction, runID, executionID string, ended Object) error {
	manifest := ended.O("artifactManifest")
	if len(manifest) == 0 || !scheduledRunResultIsReportable(ended.S("reason")) {
		return nil
	}
	occurrence := t.one(`
		SELECT id, tenant_id, task_id, scheduled_at FROM scheduled_task_occurrences
		WHERE run_id=$1 AND outcome='admitted'`, runID)
	if occurrence == nil {
		return fmt.Errorf("sessionEnded: run %s reported scheduled artifacts but is not an admitted scheduled run", runID)
	}
	run := t.one("SELECT workspace_id FROM issue_runs WHERE id=$1", runID)
	if run == nil {
		return fmt.Errorf("sessionEnded: run %s disappeared while recording its artifacts", runID)
	}
	declaration, err := parseScheduledArtifactManifest(occurrence.S("tenantId"), runID, manifest)
	if err != nil {
		return err
	}
	if len(declaration.Artifacts) == 0 && len(declaration.Rejections) == 0 && declaration.OmittedRejections == 0 && !declaration.WalkTruncated {
		return nil
	}
	// No object storage means no upload could ever succeed: the delivery converges as skipped in the
	// same transaction instead of holding the Workspace against a limit that can never be reached.
	state, reason := "delivering", ""
	if t.objectStore == nil {
		state, reason = "skipped", scheduledArtifactSkippedObjectStore
	} else if len(declaration.Artifacts) == 0 {
		state, reason = "failed", "no_accepted_artifacts"
	}
	var settledAt any
	if state != "delivering" {
		settledAt = time.Now()
	}
	// The window is fixed here and never renewed: a restart, a replay or a re-dispatch reuses the same
	// row and therefore the same deadline.
	t.exec(`
		INSERT INTO scheduled_run_artifact_deliveries
			(issue_run_id, tenant_id, task_id, occurrence_id, scheduled_at, state, deadline_at, settled_at, settled_reason, omitted_rejections, walk_truncated)
		VALUES($1,$2,$3,$4,$5,$6, now() + make_interval(secs => $7), $8, NULLIF($9,''),$10,$11)
		ON CONFLICT (issue_run_id) DO NOTHING`,
		runID, occurrence.S("tenantId"), occurrence.S("taskId"), occurrence.S("id"), occurrence.S("scheduledAt"),
		state, scheduledArtifactDeadline.Seconds(), settledAt, reason, declaration.OmittedRejections, declaration.WalkTruncated)
	for _, rejection := range declaration.Rejections {
		t.exec(`
			INSERT INTO scheduled_run_artifact_rejections
				(tenant_id, issue_run_id, task_id, occurrence_id, scheduled_at, display_name, reason)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (issue_run_id, display_name) DO NOTHING`,
			occurrence.S("tenantId"), runID, occurrence.S("taskId"), occurrence.S("id"), occurrence.S("scheduledAt"),
			rejection.DisplayName, rejection.Reason)
	}
	if len(declaration.Artifacts) == 0 || state == "skipped" {
		return nil
	}
	// The upload is its own work item: it settles independently of the Revision delivery, and the run
	// stays `delivering` until both converge or this window expires.
	enqueueExecutionWork(t, runID, "deliver_scheduled_artifacts", Object{
		"kind":               "deliver_scheduled_artifacts",
		"sessionExecutionId": executionID,
	}, sessionStartTarget(t, run.S("workspaceId")), "", 0, time.Time{})
	return nil
}
