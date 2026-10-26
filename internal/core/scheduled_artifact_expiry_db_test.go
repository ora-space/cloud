package core

import "testing"

// The deadline applies even when no Node has claimed the upload. Rejections must come from the
// frozen work/session binding, not depend on a delivery execution or any terminal receipt existing.
func TestScheduledArtifactExpiryBeforeDispatchRecordsRejections(t *testing.T) {
	store := dispatcherDB(t)
	store.ObjectStore, _ = cleanupObjectStore(t)
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	_, err := store.Control(t.Context(), &ControlRequest{
		Action: "clone_takeover", Service: scene.claims, Body: Object{
			"epoch": 1, "operationId": scene.run, "executionId": scene.execution, "sequence": 1, "event": "declared",
			"result": Object{"node": Object{"nodeId": scene.node}, "outcome": "agent_session_ended", "reason": "idle_timeout", "artifactManifest": scheduledManifest()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries
		SET deadline_at=clock_timestamp()-interval '1 second' WHERE issue_run_id=$1`, scene.run)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := store.ExpireScheduledArtifactDeliveriesOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		var refusals, work, executions, artifacts int
		if err := store.Pool.QueryRow(`SELECT
			(SELECT count(*) FROM scheduled_run_artifact_rejections WHERE issue_run_id=$1),
			(SELECT count(*) FROM execution_work WHERE run_id=$1 AND kind='deliver_scheduled_artifacts' AND execution_id IS NULL),
			(SELECT count(*) FROM node_executions WHERE operation_id=$1 AND kind='deliver_scheduled_artifacts'),
			(SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1)`, scene.run).Scan(&refusals, &work, &executions, &artifacts); err != nil {
			t.Fatal(err)
		}
		if refusals != 2 || work != 1 || executions != 0 || artifacts != 0 {
			t.Fatalf("rejections/work/executions/artifacts = %d/%d/%d/%d; want 2/1/0/0", refusals, work, executions, artifacts)
		}
		var reason string
		if err := store.Pool.QueryRow(`SELECT reason FROM scheduled_run_artifact_rejections
			WHERE issue_run_id=$1 AND display_name='reports/review.md'`, scene.run).Scan(&reason); err != nil || reason != scheduledArtifactDeadlineExpired {
			t.Fatalf("undeployed upload rejection = %q, %v; want deadline_expired", reason, err)
		}
	}
}
