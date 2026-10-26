package core

import (
	"context"
	"testing"
	"time"
)

func TestScheduledArtifactUploadIsClaimedAndDispatched(t *testing.T) {
	store := dispatcherDB(t)
	store.ObjectStore, _ = cleanupObjectStore(t)
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	_, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_takeover", SubmissionID: "declare-artifacts", Service: scene.claims,
		Body: Object{
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
		t.Fatalf("claimed %q, want artifact delivery", work.S("kind"))
	}
	execution := "upload-execution"
	first, err := dispatchAgentWork(t, store, scene.claims, work, execution, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := dispatchAgentWork(t, store, scene.claims, work, execution, "", nil)
	if err != nil || jsonText(first) != jsonText(replayed) {
		t.Fatalf("dispatch replay must return the original execution: %v", err)
	}
	pending, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_pending", Service: scene.claims, Body: Object{"nodeId": scene.node},
	})
	if err != nil || len(objectRows(pending["executions"])) != 1 {
		t.Fatalf("recovery must locate the pending upload: %v, %v", pending, err)
	}
	// A new work item cannot admit a second concurrent upload of the same run.
	_, err = store.EnqueueExecutionWork(context.Background(), scene.run, work.S("kind"), work.O("input"), work.O("target"), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchAgentWork(t, store, scene.claims, work, "second-upload", "", nil); err == nil {
		t.Fatal("a second pending upload must be refused")
	}
}
