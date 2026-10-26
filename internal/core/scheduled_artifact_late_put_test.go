package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// The object store can admit a PUT before expiry and finish writing after Cloud's first DELETE.
// Synchronize that exact ordering; a successful first sweep is never permanent proof of absence.
func TestScheduledArtifactGCRemovesPUTThatFinishesAfterFirstSweep(t *testing.T) {
	store := dispatcherDB(t)
	started, finish := make(chan struct{}), make(chan struct{})
	var release sync.Once
	var mu sync.Mutex
	exists := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			close(started)
			<-finish
			mu.Lock()
			exists = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			mu.Lock()
			exists = false
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { release.Do(func() { close(finish) }) })
	store.ObjectStore = &objectstore.Config{
		Endpoint: server.URL, Region: "us-east-1", Bucket: "artifacts", PathStyle: true,
		AccessKeyID: "test-access", SecretAccessKey: "test-secret",
	}
	scene := seedTakeoverScene(t, store)
	stageScheduledOccurrence(t, store, scene.run)
	_, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_takeover", Service: scene.claims, Body: Object{
			"epoch": 1, "operationId": scene.run, "executionId": scene.execution, "sequence": 1, "event": "declared",
			"result": Object{"node": Object{"nodeId": scene.node}, "outcome": "agent_session_ended", "reason": "idle_timeout", "artifactManifest": scheduledManifest()},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	work := claimAgentWork(t, store, scene.claims)
	if _, err := dispatchAgentWork(t, store, scene.claims, work, "late-put-upload", "", nil); err != nil {
		t.Fatal(err)
	}
	granted, err := store.Control(context.Background(), &ControlRequest{
		Action: "grant_scheduled_artifact_upload", Service: scene.claims, Body: Object{
			"epoch": 1, "executionId": "late-put-upload",
			"artifacts": []Object{{"index": 0, "size": 5, "sha256": strings.Repeat("ab", 32)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := objectRows(granted["grants"])[0]
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, grant.S("url"), strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	headers, ok := grant["headers"].(map[string]string)
	if !ok {
		t.Fatal("grant must carry the signed headers")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	done := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				err = fmt.Errorf("upload status %d", resp.StatusCode)
			}
		}
		done <- err
	}()
	<-started
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries SET state='failed',
		settled_at=now(), deadline_at=now()-interval '1 second' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if err := store.CollectScheduledArtifactObjectsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(finish) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	lateObject := exists
	mu.Unlock()
	if !lateObject {
		t.Fatal("the admitted PUT must finish after the first sweep")
	}
	// Make the next periodic pass due without sleeps or wall-clock races.
	if _, err := store.Pool.Exec(`UPDATE scheduled_run_artifact_deliveries
		SET artifacts_gc_at=now()-interval '2 minutes' WHERE issue_run_id=$1`, scene.run); err != nil {
		t.Fatal(err)
	}
	if err := store.CollectScheduledArtifactObjectsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if exists {
		t.Fatal("a later sweep must remove the object written after the first DELETE")
	}
}
