package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestThreadPreviewHubConcurrentPublishReadAndCancel(t *testing.T) {
	hub := NewThreadPreviewHub()
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for version := int64(1); version <= 100; version++ {
				_, cancel := hub.Subscribe("run")
				hub.Publish("run", preview(version, "concurrent"))
				_ = hub.Snapshot("run")
				cancel()
			}
		}()
	}
	workers.Wait()
	if hub.Snapshot("run").Version != 100 || hub.listeners != 0 {
		t.Fatal("concurrent preview lifecycle lost its final version or subscription")
	}
}

func preview(version int64, text string) ThreadPreviewSnapshot {
	return ThreadPreviewSnapshot{Available: true, ExecutionID: "execution", Version: version, Blocks: []ThreadPreviewBlock{{HistorySequence: 3, Record: json.RawMessage(fmt.Sprintf(`{"text":%q}`, text))}}}
}

func TestThreadPreviewHubReplacementIsolationAndBoundedWakeups(t *testing.T) {
	hub := NewThreadPreviewHub()
	now := time.Now()
	hub.now = func() time.Time { return now }
	wake, cancel := hub.Subscribe("run")
	defer cancel()
	<-wake
	first := preview(2, "newest")
	hub.Publish("run", first)
	first.Blocks[0].Record[0] = 'x'
	hub.Publish("run", preview(1, "late"))
	hub.Publish("run", preview(2, "same version cannot rewrite"))
	if string(hub.Snapshot("run").Blocks[0].Record) != `{"text":"newest"}` || len(wake) != 1 {
		t.Fatal("preview mutated, regressed or queued unbounded wakeups")
	}
	for index := range threadPreviewMaxRuns + 1 {
		hub.Publish(fmt.Sprint(index), preview(1, "bounded"))
	}
	if len(hub.runs) != threadPreviewMaxRuns {
		t.Fatalf("unbounded run cache: %d", len(hub.runs))
	}
	now = now.Add(threadPreviewTTL + time.Second)
	if hub.Snapshot("0").Available {
		t.Fatal("expired preview remained available")
	}
}

func TestThreadPreviewSubscriptionsAreBoundedAndCancellationIsIdempotent(t *testing.T) {
	hub := NewThreadPreviewHub()
	var cancellations []func()
	for range 64 {
		wake, cancel := hub.Subscribe("run")
		if wake == nil {
			t.Fatal("subscriber rejected below the bound")
		}
		cancellations = append(cancellations, cancel)
	}
	wake, cancel := hub.Subscribe("run")
	cancel()
	if wake != nil {
		t.Fatal("unbounded run subscribers")
	}
	for _, cancel := range cancellations {
		cancel()
		cancel()
	}
	if hub.listeners != 0 || len(hub.subs) != 0 {
		t.Fatal("cancel retained subscriber state")
	}
}

func TestThreadPreviewAuthorityAndFinalRecordDominance(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	store.ThreadPreviews = NewThreadPreviewHub()
	snapshot := preview(1, "working")
	snapshot.ExecutionID = scene.execution
	snapshot.Blocks[0].TurnID = scene.initialTurnID
	ctx := context.Background()
	if err := store.PublishThreadPreview(ctx, scene.claims, 1, scene.run, snapshot); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := store.Pool.QueryRow("SELECT count(*) FROM thread_entries WHERE run_id=$1", scene.run).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 1 {
		t.Fatal("preview created a durable Thread entry")
	}
	read := func() Object {
		out, err := store.transact(ctx, func(tx *transaction) Object {
			return threadPreviewRead(tx, store, &PublicRequest{TenantID: scene.seed.seed.tenant, IssueID: scene.seed.seed.issue, RunID: scene.run})
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if len(read()["blocks"].([]ThreadPreviewBlock)) != 1 {
		t.Fatal("live preview absent")
	}
	event := threadEvent(1, threadRecord("update", 3, "finished"), scene.initialTurnID)
	event["historySequence"] = int64(3)
	if _, err := takeOver(t, store, scene, scene.execution, []Object{event}); err != nil {
		t.Fatal(err)
	}
	snapshot.Version = 2
	if err := store.PublishThreadPreview(ctx, scene.claims, 1, scene.run, snapshot); err != nil {
		t.Fatal(err)
	}
	if len(read()["blocks"].([]ThreadPreviewBlock)) != 0 {
		t.Fatal("late preview resurrected a settled record")
	}
	if err := store.PublishThreadPreview(ctx, scene.claims, 0, scene.run, snapshot); err == nil {
		t.Fatal("stale lease published private content")
	}
	snapshot.ExecutionID = "foreign-execution"
	if err := store.PublishThreadPreview(ctx, scene.claims, 1, scene.run, snapshot); err == nil {
		t.Fatal("foreign execution published private content")
	}
}
