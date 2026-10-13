package mirror

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// The sweep's memory and its races. The grace period is only a protection if the instant it is
// measured from survives the process that observed it and if a reclaim cannot take content away from a
// live consumer, so both are asserted here rather than assumed from the code that reads them.

// reopen opens a second store over the same volume, which is what a restart is: a new process reading
// the same directory with no memory of what the previous one was doing.
func reopen(t *testing.T, root string, clock *testClock, grace time.Duration) *Store {
	t.Helper()
	store, err := New(Config{
		Root:           root,
		MaxMirrorBytes: 1 << 20,
		MaxVolumeBytes: 8 << 20,
		Grace:          grace,
		Clock:          clock.Now,
	})
	if err != nil {
		t.Fatalf("reopen %s: %v", root, err)
	}
	return store
}

// TestSweepKeepsTheUnreferencedMarkAcrossARestart pins the mark to the volume rather than to the
// process. An unreferenced instant held only in memory would restart its grace period on every
// restart, so a deployment that restarts more often than its grace period would never reclaim
// anything — a leak that looks exactly like a store with nothing to reclaim.
func TestSweepKeepsTheUnreferencedMarkAcrossARestart(t *testing.T) {
	store, clock := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)
	publish(t, store, key, "content")

	// First process: the key is unreferenced, so its countdown starts here and nowhere else.
	report, err := store.Sweep(ctx, nil, clock.Now())
	if err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	if len(report.ObservedUnreferenced) != 1 || report.ObservedUnreferenced[0] != key {
		t.Fatalf("first sweep observed %v, want the key", report.ObservedUnreferenced)
	}

	// Second process, same volume, past the grace period measured from that first observation. It must
	// destroy rather than observe: a second observation would mean the first one was never durable.
	clock.Advance(2 * time.Hour)
	restarted := reopen(t, store.Root(), clock, time.Hour)
	final, err := restarted.Sweep(ctx, nil, clock.Now())
	if err != nil {
		t.Fatalf("Sweep after restart: %v", err)
	}
	if len(final.ObservedUnreferenced) != 0 {
		t.Fatalf("the restarted process observed %v again, so the mark did not survive it", final.ObservedUnreferenced)
	}
	if len(final.Destroyed) != 1 || final.Destroyed[0] != key {
		t.Fatalf("Sweep after restart destroyed %v, want exactly the marked key", final.Destroyed)
	}
	if _, err := restarted.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("the reclaimed mirror is still readable: %v", err)
	}
}

// TestSweepKeepsAMirrorAConsumerCameBackFor is the race the grace period exists for, in its sharpest
// form: the key was observed unreferenced, the grace period has fully elapsed, and a consumer names it
// again before the pass that would destroy it. The grace period is a claim about nobody wanting the
// content, so a consumer that wants it must win at any point — otherwise a Workspace whose mirror was
// swept while its clone was in flight would be left cloning a key that no longer exists.
func TestSweepKeepsAMirrorAConsumerCameBackFor(t *testing.T) {
	store, clock := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)
	publish(t, store, key, "content")

	if _, err := store.Sweep(ctx, nil, clock.Now()); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	clock.Advance(24 * time.Hour)
	// Past grace, and referenced again. The referenced branch is decided before the grace comparison,
	// which is what makes a late consumer safe rather than merely lucky.
	report, err := store.Sweep(ctx, []Key{key}, clock.Now())
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if len(report.Destroyed) != 0 {
		t.Fatalf("a referenced mirror was destroyed past its grace period: %v", report.Destroyed)
	}
	if len(report.ReclaimedReferences) != 1 || report.ReclaimedReferences[0] != key {
		t.Fatalf("ReclaimedReferences = %v, want the key whose countdown stopped", report.ReclaimedReferences)
	}
	record, found, err := store.Lookup(key)
	if err != nil || !found {
		t.Fatalf("Lookup: found=%v err=%v", found, err)
	}
	if record.UnreferencedSince != nil {
		t.Fatal("a referenced mirror kept a countdown running, so a later pass could destroy it by drift")
	}
	if _, err := store.Open(key); err != nil {
		t.Fatalf("the mirror is not readable: %v", err)
	}
}

// TestSweepSkipsAMirrorWhoseWriterHoldsTheLock is the publication half of the race. A key can be
// unreferenced past its grace period and still have a writer on it — the per-Operation worker
// republishing a key Cloud asked for again, or a publication that started just as the reference set
// changed. Removing content out from under that writer would destroy a mirror the writer is about to
// record as ready, so the reclaim yields and the next pass decides again.
func TestSweepSkipsAMirrorWhoseWriterHoldsTheLock(t *testing.T) {
	store, clock := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)
	publish(t, store, key, "content")

	if _, err := store.Sweep(ctx, nil, clock.Now()); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	clock.Advance(2 * time.Hour)

	// A writer in flight, as the store itself represents one: the key's lock is held for the whole
	// preparation. Taking it directly is the same file the store locks, so what the sweep tests against
	// is the real mechanism rather than a stand-in for it.
	writer := flock.New(filepath.Join(store.locksDir(), key.Digest()+".lock"))
	if ok, err := writer.TryLock(); err != nil || !ok {
		t.Fatalf("take the writer's lock: ok=%v err=%v", ok, err)
	}
	report, err := store.Sweep(ctx, nil, clock.Now())
	if err != nil {
		t.Fatalf("Sweep with a writer in flight: %v", err)
	}
	if len(report.Destroyed) != 0 {
		t.Fatalf("a mirror was destroyed under its writer: %v", report.Destroyed)
	}
	if _, err := store.Open(key); err != nil {
		t.Fatalf("the mirror is gone after a sweep that must have yielded: %v", err)
	}
	// The reclaim is deferred, not abandoned: the mark is untouched, so the next pass destroys it as
	// soon as the writer is out of the way. Losing no grace period is the whole reason for waiting.
	if err := writer.Unlock(); err != nil {
		t.Fatalf("release the writer's lock: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close the writer's lock: %v", err)
	}
	after, err := store.Sweep(ctx, nil, clock.Now())
	if err != nil {
		t.Fatalf("Sweep after the writer finished: %v", err)
	}
	if len(after.Destroyed) != 1 || after.Destroyed[0] != key {
		t.Fatalf("the deferred reclaim did not happen: %v", after.Destroyed)
	}
}

// TestARevokedMirrorIsReclaimedAfterItsGracePeriod is the other end of a withdrawal. `Revoke` makes the
// content unreadable but leaves the bytes where they are, so a deployment that only ever withdrew would
// keep paying for content no consumer may read. The sweep is what eventually removes them, and the
// record stays behind saying why.
func TestARevokedMirrorIsReclaimedAfterItsGracePeriod(t *testing.T) {
	store, clock := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)
	publish(t, store, key, "content")
	if _, err := store.Revoke(key, ReasonWithdrawn); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// Unreadable immediately, whatever the sweep has or has not done.
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open on a withdrawn mirror = %v, want ErrNotReady", err)
	}
	first, err := store.Sweep(ctx, nil, clock.Now())
	if err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	if len(first.Destroyed) != 0 {
		t.Fatalf("a withdrawn mirror was reclaimed inside its grace period: %v", first.Destroyed)
	}
	clock.Advance(2 * time.Hour)
	final, err := store.Sweep(ctx, nil, clock.Now())
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if len(final.Destroyed) != 1 || final.Destroyed[0] != key {
		t.Fatalf("a withdrawn mirror's bytes were not reclaimed: %v", final.Destroyed)
	}
	record, found, err := store.Lookup(key)
	if err != nil || !found {
		t.Fatalf("Lookup after reclaiming a withdrawn mirror: found=%v err=%v", found, err)
	}
	if record.State != StateDestroyed || record.Reason != ReasonWithdrawn {
		t.Fatalf("record = %+v, want a destroyed record that still says it was withdrawn", record)
	}
}
