package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// cleanupObjectStore is a private-endpoint stand-in that records every deletion Cloud asks for.
func cleanupObjectStore(t *testing.T) (*objectstore.Config, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Query().Get("X-Amz-Signature") == "" {
			t.Error("cleanup must delete through a signed private request")
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	config := &objectstore.Config{
		Endpoint: server.URL, Region: "us-east-1", Bucket: "scheduled", PathStyle: true,
		AccessKeyID: "test-access", SecretAccessKey: "test-secret",
	}
	return config, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// A delivery that settled without registering its declaration leaves "uploaded but unregistered"
// objects behind. The sweep deletes exactly those deterministic keys - never a registered one - and
// remembers that it ran, so a second pass asks the store for nothing.
func TestCollectScheduledArtifactObjectsRemovesOnlyUnregisteredKeys(t *testing.T) {
	store := dispatcherDB(t)
	config, deleted := cleanupObjectStore(t)
	store.ObjectStore = config
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	ended := Object{
		"outcome": "agent_session_ended", "reason": "idle_timeout",
		"node":             Object{"nodeId": scene.node},
		"artifactManifest": scheduledManifest(),
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		executionResult(tx, &ControlRequest{Action: "clone_takeover", Body: Object{
			"operationId": scene.run, "executionId": scene.execution,
			"sequence": int64(1), "event": "artifacts-declared", "result": ended,
		}}, true)
		return Object{}
	}); err != nil {
		t.Fatalf("declare the artifacts: %v", err)
	}
	// Wind the delivery forward to the state the sweep reacts to, with nothing registered.
	if _, err := store.Pool.Exec(`
		UPDATE scheduled_run_artifact_deliveries
		SET state='failed', settled_at=now(), settled_reason='upload_failed'
		WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatalf("settle the delivery: %v", err)
	}
	var tenantID string
	if err := store.Pool.QueryRow(`SELECT tenant_id FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1`, scene.run).Scan(&tenantID); err != nil {
		t.Fatalf("read the tenant: %v", err)
	}

	if err := store.CollectScheduledArtifactObjectsOnce(context.Background()); err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := scheduledArtifactKey(tenantID, scene.run, 0)
	if paths := deleted(); len(paths) != 1 || !strings.HasSuffix(paths[0], want) {
		t.Fatalf("deleted = %v, want only the unregistered key %s", paths, want)
	}
	var swept bool
	if err := store.Pool.QueryRow(`SELECT artifacts_gc_at IS NOT NULL FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1`, scene.run).Scan(&swept); err != nil || !swept {
		t.Fatalf("swept = %v (err %v), want the pass to remember it ran", swept, err)
	}
	// The second pass has nothing left to ask about.
	if err := store.CollectScheduledArtifactObjectsOnce(context.Background()); err != nil {
		t.Fatalf("second collect: %v", err)
	}
	if paths := deleted(); len(paths) != 1 {
		t.Fatalf("deletions = %v, want the second pass to delete nothing", paths)
	}
	// Delivered runs also remain eligible after retention removes their index. A PUT that finished
	// after pruning must not become permanently unreachable to the collector.
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries
		SET state='delivered', artifacts_gc_at=now()-interval '2 minutes' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if err := store.CollectScheduledArtifactObjectsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if paths := deleted(); len(paths) != 2 || !strings.HasSuffix(paths[1], want) {
		t.Fatalf("a later pass must revisit a pruned delivered run: %v", paths)
	}
}

// Retention keeps the ten newest artifact-bearing runs of a task and drops the rest, objects first.
func TestPruneScheduledRunArtifactsKeepsTheTenNewestRuns(t *testing.T) {
	store := dispatcherDB(t)
	config, deleted := cleanupObjectStore(t)
	store.ObjectStore = config
	taskID := ""
	var runs []string
	for index := 0; index <= scheduledArtifactRunsPerTask; index++ {
		seed := seedDispatchScene(t, store.Pool, false)
		runs = append(runs, seed.run)
		var tenantID, issueID string
		if err := store.Pool.QueryRow(`SELECT tenant_id, issue_id FROM issue_runs WHERE id=$1`, seed.run).Scan(&tenantID, &issueID); err != nil {
			t.Fatalf("read the run: %v", err)
		}
		if taskID == "" {
			if err := store.Pool.QueryRow(`
				INSERT INTO scheduled_tasks
					(id,tenant_id,issue_id,creator_user_id,agent_id,name,prompt,schedule,timezone,revision,version,enabled)
				SELECT gen_random_uuid(), $1, $2, (SELECT id FROM users ORDER BY id LIMIT 1),
				       (SELECT id FROM space_agents ORDER BY id LIMIT 1),
				       'retention','review','{"kind":"daily","hour":0,"minute":0}'::jsonb,'UTC',1,1,false
				RETURNING id`, tenantID, issueID).Scan(&taskID); err != nil {
				t.Fatalf("stage the task: %v", err)
			}
		}
		if _, err := store.Pool.Exec(`
			INSERT INTO scheduled_run_artifacts
				(tenant_id, issue_run_id, task_id, scheduled_at, artifact_index, object_key, display_name, kind, size_bytes, sha256)
			VALUES($1,$2,$3, now() - make_interval(secs => $4), 0, $5, 'report.md', 'markdown', 5, $6)`,
			tenantID, seed.run, taskID, (scheduledArtifactRunsPerTask-index)*60, scheduledArtifactKey(tenantID, seed.run, 0),
			strings.Repeat("ab", 32)); err != nil {
			t.Fatalf("stage the artifact: %v", err)
		}
	}

	if err := store.PruneScheduledRunArtifactsOnce(context.Background()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	// The oldest run is gone; the ten newest still hold their index.
	var remaining int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM scheduled_run_artifacts WHERE issue_run_id=$1`, runs[0]).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("oldest run artifacts = %d (err %v), want pruned", remaining, err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM scheduled_run_artifacts WHERE task_id=$1`, taskID).Scan(&remaining); err != nil || remaining != scheduledArtifactRunsPerTask {
		t.Fatalf("task artifacts = %d (err %v), want the ten newest runs", remaining, err)
	}
	if paths := deleted(); len(paths) != 1 {
		t.Fatalf("deleted = %v, want the pruned run's object", paths)
	}
}
