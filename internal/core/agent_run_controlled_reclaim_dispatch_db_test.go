package core

import (
	"testing"
	"time"
)

// A durable delivery skip must remain idempotent even when a retry carries a legacy non-UUID target.
func TestControlledResidualSkippedDeliveryWithInvalidWorkspaceID(t *testing.T) {
	store := controlledStores(t)(t)
	seed := seedDispatchScene(t, store.Pool, false)
	createdAt := time.Date(2026, time.October, 20, 12, 0, 0, 0, time.UTC)
	if _, err := store.Pool.Exec(`INSERT INTO revision_delivery_skips(run_id,created_at) VALUES($1,$2)`, seed.run, createdAt); err != nil {
		t.Fatalf("seed durable delivery skip: %v", err)
	}

	work, err := store.EnqueueExecutionWork(t.Context(), seed.run, "deliver_revision",
		Object{"kind": "deliver_revision"},
		Object{"workspaceId": "w", "sandboxInstanceId": "s", "nodeId": "n"}, time.Time{})
	if err != nil {
		t.Fatalf("retry skipped delivery with a non-UUID workspace target: %v", err)
	}
	if !work.B("skipped") {
		t.Fatalf("retry must return the durable skip, got %v", work)
	}

	var workCount int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE run_id=$1`, seed.run).Scan(&workCount); err != nil {
		t.Fatalf("count execution work after skipped retry: %v", err)
	}
	if workCount != 0 {
		t.Fatalf("skipped retry created %d execution work items; want none", workCount)
	}
	var skipRunID string
	var skipCreatedAt time.Time
	if err := store.Pool.QueryRow(`SELECT run_id,created_at FROM revision_delivery_skips WHERE run_id=$1`, seed.run).Scan(&skipRunID, &skipCreatedAt); err != nil {
		t.Fatalf("read original durable delivery skip: %v", err)
	}
	if skipRunID != seed.run || !skipCreatedAt.Equal(createdAt) {
		t.Fatalf("durable skip changed: run=%q created_at=%v; want run=%q created_at=%v", skipRunID, skipCreatedAt, seed.run, createdAt)
	}
}
