package core

import (
	"context"
	"strings"
	"testing"
)

func TestScheduledManifestCompletenessWithoutAcceptedFiles(t *testing.T) {
	for _, manifest := range []Object{
		{"items": []Object{}, "rejections": []Object{}, "omittedRejections": 12, "walkTruncated": true},
		{"rejections": []Object{{"displayName": "unsafe:name.txt", "reason": "path_unsafe"}, {"displayName": ".ora-scheduled-artifacts", "reason": "candidate_limit"}}, "omittedRejections": 448, "walkTruncated": true},
	} {
		store := dispatcherDB(t)
		store.ObjectStore, _ = cleanupObjectStore(t)
		scene := seedTakeoverScene(t, store)
		stageScheduledOccurrence(t, store, scene.run)
		_, err := store.Control(context.Background(), &ControlRequest{Action: "clone_takeover", Service: scene.claims, Body: Object{
			"epoch": 1, "operationId": scene.run, "executionId": scene.execution, "sequence": 1, "event": "truncated",
			"result": Object{"node": Object{"nodeId": scene.node}, "outcome": "agent_session_ended", "reason": "idle_timeout", "artifactManifest": manifest},
		}})
		if err != nil {
			t.Fatal(err)
		}
		var omitted int64
		var truncated bool
		var state string
		if err := store.Pool.QueryRow(`SELECT omitted_rejections,walk_truncated,state FROM scheduled_run_artifact_deliveries
			WHERE issue_run_id=$1`, scene.run).Scan(&omitted, &truncated, &state); err != nil {
			t.Fatal(err)
		}
		if omitted != manifest.N("omittedRejections") || !truncated || state != "failed" {
			t.Fatalf("completeness facts = %d/%v/%s", omitted, truncated, state)
		}
		var pending int
		if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE run_id=$1 AND kind='deliver_scheduled_artifacts'`, scene.run).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("a declaration without accepted files must not wait for an upload: %d, %v", pending, err)
		}
	}
}

func TestScheduledDuplicateIndexRollsBackSessionTakeover(t *testing.T) {
	store := dispatcherDB(t)
	store.ObjectStore, _ = cleanupObjectStore(t)
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	manifest := Object{"items": []Object{
		{"index": 0, "displayName": "first.txt", "kind": "text", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32)},
		{"index": 0, "displayName": "second.txt", "kind": "text", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32)},
	}}
	_, err := store.Control(context.Background(), &ControlRequest{Action: "clone_takeover", Service: scene.claims, Body: Object{
		"epoch": 1, "operationId": scene.run, "executionId": scene.execution, "sequence": 1, "event": "invalid-index",
		"result": Object{"node": Object{"nodeId": scene.node}, "outcome": "agent_session_ended", "reason": "idle_timeout", "artifactManifest": manifest},
	}})
	if err == nil {
		t.Fatal("duplicate index must be refused before freezing the declaration")
	}
	var changes int
	if err := store.Pool.QueryRow(`SELECT
		(SELECT count(*) FROM node_event_receipts WHERE execution_id=$1) +
		(SELECT count(*) FROM node_executions WHERE execution_id=$1 AND result IS NOT NULL) +
		(SELECT count(*) FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$2) +
		(SELECT count(*) FROM execution_work WHERE run_id=$2 AND kind IN ('deliver_revision','deliver_scheduled_artifacts'))`, scene.execution, scene.run).Scan(&changes); err != nil || changes != 0 {
		t.Fatalf("invalid declaration must leave no partial terminal, receipt or deliveries: %d, %v", changes, err)
	}
}
