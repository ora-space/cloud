package core

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// DB tests for the scheduled-run artifact delivery (specs decisions/cloud/scheduled-task/
// 20261022-run-verdict-and-artifacts.md D3). They drive the shipped settlement on a real isolated
// schema; the object-store side is covered where a deployment can be given one.

// scheduledManifest is the declaration an executor reports for one run.
func scheduledManifest() Object {
	return Object{
		"items": []Object{{
			"index": 0, "displayName": "reports/review.md", "kind": "markdown",
			"sizeBytes": 5, "sha256": strings.Repeat("ab", 32),
		}},
		"rejections": []Object{{"displayName": "notes.pdf", "reason": "unsupported_type"}},
	}
}

func TestScheduledArtifactManifestParsesDecodedJSON(t *testing.T) {
	manifest := scheduledManifest()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Object
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	want, err := parseScheduledArtifactManifest("tenant", "run", manifest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseScheduledArtifactManifest("tenant", "run", decoded)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded declaration = %+v, %v; want %+v", got, err, want)
	}
}

// A deployment without object storage cannot upload anything, so the delivery converges as skipped
// in the session-terminal transaction: the declaration is still recorded, the refused candidate stays
// visible, no upload work item is declared, and the run releases rather than holding its Workspace
// against a limit that can never be reached.
func TestScheduledArtifactsSkipWithoutObjectStorage(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	ended := Object{
		"outcome": "agent_session_ended", "reason": "idle_timeout",
		"node":             Object{"nodeId": scene.node},
		"artifactManifest": scheduledManifest(),
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		// The real takeover: it persists the session's terminal result, which is what the release
		// path reads its end reason from, and it runs the business hooks in this transaction.
		executionResult(tx, &ControlRequest{Action: "clone_takeover", Body: Object{
			"operationId": scene.run, "executionId": scene.execution,
			"sequence": int64(1), "event": "scheduled-artifacts-ended", "result": ended,
		}}, true)
		return Object{}
	}); err != nil {
		t.Fatalf("settle transaction: %v", err)
	}

	var state, reason string
	var settled bool
	if err := store.Pool.QueryRow(`
		SELECT state, COALESCE(settled_reason,''), settled_at IS NOT NULL
		FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1`, scene.run).Scan(&state, &reason, &settled); err != nil {
		t.Fatalf("read the delivery: %v", err)
	}
	if state != "skipped" || reason != scheduledArtifactSkippedObjectStore || !settled {
		t.Fatalf("delivery = %q/%q settled=%v, want skipped/%s", state, reason, settled, scheduledArtifactSkippedObjectStore)
	}
	// The refused candidate is a fact the reader must see, whatever happened to the uploads.
	var rejections int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM scheduled_run_artifact_rejections
		WHERE issue_run_id=$1 AND display_name='notes.pdf' AND reason='unsupported_type'`, scene.run).Scan(&rejections); err != nil {
		t.Fatalf("count rejections: %v", err)
	}
	if rejections != 1 {
		t.Fatalf("rejections = %d, want the declared refusal recorded", rejections)
	}
	// Nothing was uploaded, so nothing may be registered, and no Node may be asked to try.
	var registered, work int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1`, scene.run).Scan(&registered); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if registered != 0 {
		t.Fatalf("registered = %d, want none without object storage", registered)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work
		WHERE run_id=$1 AND kind='deliver_scheduled_artifacts'`, scene.run).Scan(&work); err != nil {
		t.Fatalf("count work: %v", err)
	}
	if work != 0 {
		t.Fatalf("upload work items = %d, want none without object storage", work)
	}
	var phase string
	if err := store.Pool.QueryRow(`SELECT phase FROM issue_runs WHERE id=$1`, scene.run).Scan(&phase); err != nil || phase != "releasing" {
		t.Fatalf("run phase = %q (err %v), want releasing", phase, err)
	}
}

// A declaration outside the frozen bounds is refused instead of stored: half a manifest would let a
// run look like it had artifacts while the facts were incomplete.
func TestScheduledArtifactManifestIsValidatedBeforeStorage(t *testing.T) {
	for name, manifest := range map[string]Object{
		"unsafe path": {"items": []Object{{
			"index": 0, "displayName": "../escape.md", "kind": "markdown", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32),
		}}},
		"unknown kind": {"items": []Object{{
			"index": 0, "displayName": "report.sh", "kind": "shell", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32),
		}}},
		"over the size bound": {"items": []Object{{
			"index": 0, "displayName": "report.md", "kind": "markdown", "sizeBytes": maxScheduledArtifactBytes + 1,
			"sha256": strings.Repeat("ab", 32),
		}}},
		"declared twice": {"items": []Object{
			{"index": 0, "displayName": "report.md", "kind": "markdown", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32)},
			{"index": 1, "displayName": "report.md", "kind": "markdown", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32)},
		}},
		"declared and rejected": {"items": []Object{{
			"index": 0, "displayName": "report.md", "kind": "markdown", "sizeBytes": 5, "sha256": strings.Repeat("ab", 32),
		}}, "rejections": []Object{{"displayName": "report.md", "reason": "too_large"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseScheduledArtifactManifest("4b3c2d1e-0f9a-4b8c-9d7e-6f5a4b3c2d1e", "5d4c3b2a-1f0e-4d8c-9b7a-6f5e4d3c2b1a", manifest); err == nil {
				t.Fatal("an invalid declaration must be refused")
			}
		})
	}
}
