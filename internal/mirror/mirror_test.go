package mirror

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testRepository = "https://github.com/ora-space/demo"
	testCommitA    = "1111111111111111111111111111111111111111"
	testCommitB    = "2222222222222222222222222222222222222222"
	testRef        = "refs/ora/mirror"
)

// testClock is a clock a test moves by hand, so grace periods and generations are judged on the
// store's own notion of time rather than on how long the test took to run.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore(t *testing.T, mutate func(*Config)) (*Store, *testClock) {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)}
	cfg := Config{
		Root:           t.TempDir(),
		MaxMirrorBytes: 1 << 20,
		MaxVolumeBytes: 8 << 20,
		Grace:          time.Hour,
		Clock:          clock.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	store, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store, clock
}

func testKey(commit string) Key {
	return Key{ProjectID: "project-1", Repository: testRepository, Commit: commit}
}

// publish stages and publishes content, failing the test if either step does.
func publish(t *testing.T, store *Store, key Key, content string) Record {
	t.Helper()
	prep, err := store.Begin(context.Background(), key)
	if err != nil {
		t.Fatalf("Begin(%s): %v", key, err)
	}
	defer prep.Close()
	writeFile(t, filepath.Join(prep.Dir(), "objects", "payload"), content)
	record, err := prep.Publish(context.Background(), PublishVerification{Commit: key.Commit, Ref: testRef})
	if err != nil {
		t.Fatalf("Publish(%s): %v", key, err)
	}
	return record
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	cases := map[string]Config{
		"no root":                 {MaxMirrorBytes: 1, MaxVolumeBytes: 1},
		"relative root":           {Root: "mirrors", MaxMirrorBytes: 1, MaxVolumeBytes: 1},
		"no per-mirror limit":     {Root: "/tmp/mirrors", MaxVolumeBytes: 1},
		"volume below per-mirror": {Root: "/tmp/mirrors", MaxMirrorBytes: 10, MaxVolumeBytes: 9},
		"negative grace":          {Root: "/tmp/mirrors", MaxMirrorBytes: 1, MaxVolumeBytes: 1, Grace: -time.Second},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Fatalf("%s: New accepted an unusable configuration", name)
		}
	}
}

func TestKeyValidate(t *testing.T) {
	cases := map[string]Key{
		"no project":             {Repository: testRepository, Commit: testCommitA},
		"non-canonical host":     {ProjectID: "p", Repository: "https://GitHub.com/ora-space/demo", Commit: testCommitA},
		"trailing .git":          {ProjectID: "p", Repository: testRepository + ".git", Commit: testCommitA},
		"trailing slash":         {ProjectID: "p", Repository: testRepository + "/", Commit: testCommitA},
		"ssh repository":         {ProjectID: "p", Repository: "git@github.com:ora-space/demo", Commit: testCommitA},
		"short commit":           {ProjectID: "p", Repository: testRepository, Commit: testCommitA[:12]},
		"uppercase commit":       {ProjectID: "p", Repository: testRepository, Commit: strings.ToUpper("abcdef" + testCommitA[:34])},
		"empty commit":           {ProjectID: "p", Repository: testRepository},
		"credentials in the url": {ProjectID: "p", Repository: "https://user:token@github.com/ora-space/demo", Commit: testCommitA},
	}
	for name, key := range cases {
		if err := key.Validate(); err == nil {
			t.Fatalf("%s: key %+v was accepted", name, key)
		}
	}
	if err := testKey(testCommitA).Validate(); err != nil {
		t.Fatalf("a canonical key was rejected: %v", err)
	}
}

func TestKeyDigestSeparatesFields(t *testing.T) {
	// Moving a character across a field boundary must not collide, which is why the digest
	// length-prefixes each field.
	left := Key{ProjectID: "ab", Repository: testRepository, Commit: testCommitA}
	right := Key{ProjectID: "a", Repository: "b" + testRepository, Commit: testCommitA}
	if left.Digest() == right.Digest() {
		t.Fatal("two distinct keys digested to the same name")
	}
	if left.Digest() != (Key{ProjectID: "ab", Repository: testRepository, Commit: testCommitA}).Digest() {
		t.Fatal("the digest is not stable for one key")
	}
}

func TestContentBecomesVisibleOnlyAfterPublish(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)
	ctx := context.Background()

	prep, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer prep.Close()
	staging := prep.Dir()
	writeFile(t, filepath.Join(staging, "objects", "payload"), "mirror content")

	// While the preparation is open the key is not readable, whatever is on disk.
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open during preparation = %v, want ErrNotReady", err)
	}
	record, found, err := store.Lookup(key)
	if err != nil || !found {
		t.Fatalf("Lookup during preparation = (%+v, %v, %v)", record, found, err)
	}
	if record.Readable() {
		t.Fatalf("a preparing record is readable: %+v", record)
	}

	published, err := prep.Publish(ctx, PublishVerification{Commit: key.Commit, Ref: testRef})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.State != StateReady || published.Generation != 1 || published.Ref != testRef {
		t.Fatalf("published record = %+v", published)
	}
	if published.Bytes != int64(len("mirror content")) {
		t.Fatalf("published bytes = %d, want %d", published.Bytes, len("mirror content"))
	}
	if published.PublishedAt == nil {
		t.Fatal("a published record must record when it was published")
	}

	handle, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open after publish: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(handle.Dir, "objects", "payload"))
	if err != nil {
		t.Fatalf("read published content: %v", err)
	}
	if string(body) != "mirror content" {
		t.Fatalf("published content = %q", body)
	}
	// The staging directory is gone: content moved rather than copied, so nothing is double-charged.
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging directory survived publication: %v", err)
	}
	if leftovers := recordTempFiles(t, store); len(leftovers) != 0 {
		t.Fatalf("record writes left temporary files behind: %v", leftovers)
	}
}

func TestPublishRefusesAVerificationThatDoesNotMatchTheKey(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)
	cases := map[string]PublishVerification{
		"other commit": {Commit: testCommitB, Ref: testRef},
		"short ref":    {Commit: key.Commit, Ref: "mirror"},
		"ref with ..":  {Commit: key.Commit, Ref: "refs/ora/../mirror"},
		"empty ref":    {Commit: key.Commit},
	}
	for name, verification := range cases {
		prep, err := store.Begin(context.Background(), key)
		if err != nil {
			t.Fatalf("%s: Begin: %v", name, err)
		}
		writeFile(t, filepath.Join(prep.Dir(), "payload"), "content")
		if _, err := prep.Publish(context.Background(), verification); err == nil {
			t.Fatalf("%s: Publish accepted %+v", name, verification)
		}
		if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
			t.Fatalf("%s: the key became readable anyway: %v", name, err)
		}
		if err := prep.Close(); err != nil {
			t.Fatalf("%s: Close: %v", name, err)
		}
	}
}

func TestBeginRefusesAKeyThatIsAlreadyReadable(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)
	first := publish(t, store, key, "content")

	_, err := store.Begin(context.Background(), key)
	if !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("Begin on a readable key = %v, want ErrAlreadyPublished", err)
	}
	var published *AlreadyPublishedError
	if !errors.As(err, &published) {
		t.Fatalf("Begin error does not carry the existing record: %v", err)
	}
	if published.Record.Generation != first.Generation || published.Record.Bytes != first.Bytes {
		t.Fatalf("carried record = %+v, want the published one %+v", published.Record, first)
	}
	// The refusal must not have disturbed the mirror it refused to replace.
	if _, err := store.Open(key); err != nil {
		t.Fatalf("Open after a refused Begin: %v", err)
	}
}

func TestBeginReplacesAReadyRecordWhoseContentIsGone(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)
	first := publish(t, store, key, "content")

	// Damage the store the way a lost volume would: the record stays, the bytes do not.
	if err := os.RemoveAll(store.contentDir(key.Digest(), first.Generation)); err != nil {
		t.Fatalf("remove published content: %v", err)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Open on a readable record with no content = %v, want ErrUnavailable", err)
	}

	second := publish(t, store, key, "replacement")
	if second.Generation != first.Generation+1 {
		t.Fatalf("replacement generation = %d, want %d", second.Generation, first.Generation+1)
	}
	handle, err := store.Open(key)
	if err != nil {
		t.Fatalf("Open after replacement: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(handle.Dir, "objects", "payload"))
	if err != nil {
		t.Fatalf("read replacement: %v", err)
	}
	if string(body) != "replacement" {
		t.Fatalf("replacement content = %q", body)
	}
}

func TestBeginRefusesARevokedKeyDurably(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)
	record := publish(t, store, key, "content")

	revoked, err := store.Revoke(key, "upstream rewritten")
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if revoked.State != StateRevoked || revoked.Reason != "upstream rewritten" {
		t.Fatalf("revoked record = %+v", revoked)
	}
	if revoked.Generation != record.Generation {
		t.Fatalf("revocation changed the generation: %d -> %d", record.Generation, revoked.Generation)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open after revoke = %v, want ErrNotReady", err)
	}
	// Reads stop at the record; the bytes stay until something destroys them.
	if !store.contentPresent(&revoked) {
		t.Fatal("revocation deleted content, which is the sweep's job and happens after the grace period")
	}
	if _, err := store.Begin(context.Background(), key); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Begin on a revoked key = %v, want ErrRevoked", err)
	}
	if _, err := store.Revoke(testKey(testCommitB), ""); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Revoke on an unknown key = %v, want ErrNotReady", err)
	}
}

func TestDestroyedKeyCanBePublishedAgain(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)
	first := publish(t, store, key, "content")
	destroyed, err := store.Destroy(key)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if destroyed.State != StateDestroyed || destroyed.Bytes != 0 {
		t.Fatalf("destroyed record = %+v", destroyed)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open after destroy = %v, want ErrNotReady", err)
	}
	if _, err := store.Destroy(key); err != nil {
		t.Fatalf("Destroy must be idempotent: %v", err)
	}
	second := publish(t, store, key, "content again")
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation after destroy = %d, want %d", second.Generation, first.Generation+1)
	}
}

func TestPerMirrorSizeLimitIsTerminalAndReclaimsTheStaging(t *testing.T) {
	store, _ := newTestStore(t, func(cfg *Config) { cfg.MaxMirrorBytes = 16 })
	key := testKey(testCommitA)
	ctx := context.Background()

	prep, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer prep.Close()
	staging := prep.Dir()
	writeFile(t, filepath.Join(staging, "payload"), strings.Repeat("x", 64))
	if _, err := prep.Publish(ctx, PublishVerification{Commit: key.Commit, Ref: testRef}); err == nil {
		t.Fatal("Publish accepted content over the per-mirror limit")
	}
	record, found, err := store.Lookup(key)
	if err != nil || !found {
		t.Fatalf("Lookup after size refusal = (%+v, %v, %v)", record, found, err)
	}
	if record.State != StateFailed || record.Reason != ReasonSizeLimit {
		t.Fatalf("record after size refusal = %+v, want a failed record with %s", record, ReasonSizeLimit)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized staging survived: %v", err)
	}
	// A limit is not a dead end: the same key can be prepared again, and this time it can succeed.
	smaller := testKey(testCommitB)
	if record := publish(t, store, smaller, "small"); record.State != StateReady {
		t.Fatalf("record after a smaller publish = %+v", record)
	}
}

func TestVolumeLimitRefusesWithoutTouchingAnExistingMirror(t *testing.T) {
	store, _ := newTestStore(t, func(cfg *Config) {
		cfg.MaxMirrorBytes = 100
		cfg.MaxVolumeBytes = 100
	})
	ctx := context.Background()
	first := testKey(testCommitA)
	second := testKey(testCommitB)
	publish(t, store, first, strings.Repeat("a", 100))

	prep, err := store.Begin(ctx, second)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer prep.Close()
	writeFile(t, filepath.Join(prep.Dir(), "payload"), strings.Repeat("b", 100))
	if _, err := prep.Publish(ctx, PublishVerification{Commit: second.Commit, Ref: testRef}); err == nil {
		t.Fatal("Publish accepted content over the volume limit")
	}
	record, _, err := store.Lookup(second)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record.State != StateFailed || record.Reason != ReasonVolumeLimit {
		t.Fatalf("record after volume refusal = %+v, want a failed record with %s", record, ReasonVolumeLimit)
	}
	// The mirror that already existed is untouched: a refused publication is not a destructive act.
	if _, err := store.Open(first); err != nil {
		t.Fatalf("the existing mirror stopped being readable: %v", err)
	}
	// Freeing the space makes the same publication possible, which proves the cap is about space and
	// not about the key.
	if _, err := store.Destroy(first); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	publish(t, store, second, strings.Repeat("b", 100))
}

func TestConcurrentPreparationsSerializeAndOneOfThemReuses(t *testing.T) {
	store, _ := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)

	first, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("Begin (first): %v", err)
	}
	defer first.Close()
	writeFile(t, filepath.Join(first.Dir(), "payload"), "content")

	// The second caller blocks on the key's lock rather than preparing the same commit twice.
	type result struct {
		prep *Preparation
		err  error
	}
	secondDone := make(chan result, 1)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		prep, err := store.Begin(ctx, key)
		secondDone <- result{prep: prep, err: err}
	}()
	<-secondStarted
	select {
	case got := <-secondDone:
		t.Fatalf("the second Begin returned before the first finished: (%v, %v)", got.prep, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	published, err := first.Publish(ctx, PublishVerification{Commit: key.Commit, Ref: testRef})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case got := <-secondDone:
		if !errors.Is(got.err, ErrAlreadyPublished) {
			t.Fatalf("the second Begin = (%v, %v), want ErrAlreadyPublished", got.prep, got.err)
		}
		var carried *AlreadyPublishedError
		if !errors.As(got.err, &carried) || carried.Record.Generation != published.Generation {
			t.Fatalf("the second Begin did not carry the published record: %v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second Begin never returned after the first published")
	}
}

func TestConcurrentPublicationsOfDifferentKeysBothSucceed(t *testing.T) {
	store, _ := newTestStore(t, nil)
	ctx := context.Background()
	keys := []Key{testKey(testCommitA), testKey(testCommitB)}

	var wg sync.WaitGroup
	errs := make([]error, len(keys))
	for i, key := range keys {
		wg.Add(1)
		go func(i int, key Key) {
			defer wg.Done()
			prep, err := store.Begin(ctx, key)
			if err != nil {
				errs[i] = err
				return
			}
			defer prep.Close()
			if err := os.MkdirAll(filepath.Join(prep.Dir(), "objects"), 0o700); err != nil {
				errs[i] = err
				return
			}
			if err := os.WriteFile(filepath.Join(prep.Dir(), "objects", "payload"), []byte("content"), 0o600); err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = prep.Publish(ctx, PublishVerification{Commit: key.Commit, Ref: testRef})
		}(i, key)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("publication %d failed: %v", i, err)
		}
		if _, err := store.Open(keys[i]); err != nil {
			t.Fatalf("publication %d is not readable: %v", i, err)
		}
	}
}

func TestSweepAppliesTheGracePeriodToUnreferencedMirrorsOnly(t *testing.T) {
	store, _ := newTestStore(t, nil)
	ctx := context.Background()
	referenced := testKey(testCommitA)
	unreferenced := testKey(testCommitB)
	publish(t, store, referenced, "content a")
	publish(t, store, unreferenced, "content b")

	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	live := []Key{referenced}

	report, err := store.Sweep(ctx, live, base)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(report.ObservedUnreferenced) != 1 || report.ObservedUnreferenced[0] != unreferenced {
		t.Fatalf("first sweep observed %+v, want the unreferenced key", report.ObservedUnreferenced)
	}
	if len(report.Destroyed) != 0 {
		t.Fatalf("first sweep destroyed %+v, want nothing", report.Destroyed)
	}

	// Referenced keys carry no countdown at all, so no future sweep can destroy them by drift.
	record, _, err := store.Lookup(referenced)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record.UnreferencedSince != nil {
		t.Fatalf("a referenced key was marked unreferenced: %+v", record)
	}

	// Just inside the grace period the mirror is still there and still readable.
	if _, err := store.Sweep(ctx, live, base.Add(time.Minute)); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := store.Open(unreferenced); err != nil {
		t.Fatalf("the mirror was destroyed inside the grace period: %v", err)
	}

	// Coming back to a key stops its countdown rather than leaving a stale one running.
	reclaimed, err := store.Sweep(ctx, []Key{referenced, unreferenced}, base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(reclaimed.ReclaimedReferences) != 1 || reclaimed.ReclaimedReferences[0] != unreferenced {
		t.Fatalf("re-referencing reported %+v", reclaimed.ReclaimedReferences)
	}
	if _, err := store.Sweep(ctx, live, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := store.Open(unreferenced); err != nil {
		t.Fatalf("a re-referenced mirror was destroyed: %v", err)
	}

	// Past the grace period it goes, and the grace period is measured from the observation that
	// restarted the countdown, not from the mirror's creation.
	final, err := store.Sweep(ctx, live, base.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(final.Destroyed) != 1 || final.Destroyed[0] != unreferenced {
		t.Fatalf("final sweep destroyed %+v", final.Destroyed)
	}
	if _, err := store.Open(unreferenced); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open after destruction = %v, want ErrNotReady", err)
	}
	if _, err := store.Open(referenced); err != nil {
		t.Fatalf("the referenced mirror was destroyed too: %v", err)
	}
}

func TestSweepRefusesAKeyThatIsNotUsable(t *testing.T) {
	store, _ := newTestStore(t, nil)
	if _, err := store.Sweep(context.Background(), []Key{{ProjectID: "p"}}, time.Now()); err == nil {
		t.Fatal("Sweep accepted a key that names no mirror")
	}
}

func TestSweepReclaimsStagingFromAWriterThatDied(t *testing.T) {
	store, _ := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)

	prep, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	staging := prep.Dir()
	writeFile(t, filepath.Join(staging, "payload"), "half-written")
	// Simulate the writer dying: the lock is released by the operating system, and nothing cleans up.
	prep.release()

	// A healthy preparation in flight must not be mistaken for debris, so start one and leave it.
	live, err := store.Begin(ctx, testKey(testCommitB))
	if err != nil {
		t.Fatalf("Begin (live): %v", err)
	}
	defer live.Close()

	report, err := store.Sweep(ctx, nil, time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.StagingRemoved != 1 {
		t.Fatalf("sweep removed %d staging directories, want 1", report.StagingRemoved)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the dead writer's staging survived: %v", err)
	}
	if _, err := os.Stat(live.Dir()); err != nil {
		t.Fatalf("the live writer's staging was reclaimed: %v", err)
	}
	// Reclaiming it also unblocks the key, so the same commit can be prepared again.
	if _, err := store.Begin(ctx, key); err != nil {
		t.Fatalf("Begin after reclaiming a dead writer: %v", err)
	}
}

func TestBeginReclaimsAStalePreparingRecord(t *testing.T) {
	store, _ := newTestStore(t, nil)
	key := testKey(testCommitA)

	// A writer that died after recording "preparing" but before publishing anything.
	preparing := Record{Key: key, State: StatePreparing, CreatedAt: time.Now(), ChangedAt: time.Now()}
	if err := store.writeRecord(&preparing); err != nil {
		t.Fatalf("write preparing record: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(store.stagingDir(), key.Digest()+".stale"), 0o700); err != nil {
		t.Fatalf("create stale staging: %v", err)
	}
	if record := publish(t, store, key, "content"); record.Generation != 1 {
		t.Fatalf("record after reclaiming a stale preparation = %+v", record)
	}
	entries, err := os.ReadDir(store.stagingDir())
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("stale staging survived a fresh preparation: %v", entries)
	}
}

func TestSweepReclaimsAPublishedTreeNoRecordNames(t *testing.T) {
	store, _ := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)
	record := publish(t, store, key, "content")

	// The interruption window: content renamed into place, process died before the record was written.
	orphan := store.contentDir(key.Digest(), record.Generation+7)
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("create orphan: %v", err)
	}
	writeFile(t, filepath.Join(orphan, "payload"), "content")
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(orphan, base, base); err != nil {
		t.Fatalf("age the orphan: %v", err)
	}

	// Inside the grace period it waits, because age is the only signal an unreferenced tree carries.
	report, err := store.Sweep(ctx, []Key{key}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.OrphansRemoved != 0 {
		t.Fatalf("a fresh tree was reclaimed: %+v", report)
	}
	report, err = store.Sweep(ctx, []Key{key}, base.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.OrphansRemoved != 1 {
		t.Fatalf("sweep removed %d orphans, want 1", report.OrphansRemoved)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the orphan survived: %v", err)
	}
	// The named generation is never touched: the sweep only reclaims what nothing points at.
	if handle, err := store.Open(key); err != nil {
		t.Fatalf("the named generation was reclaimed: %v", err)
	} else if _, err := os.ReadFile(filepath.Join(handle.Dir, "objects", "payload")); err != nil {
		t.Fatalf("read named generation: %v", err)
	}
}

func TestSweepLeavesForeignDirectoriesAlone(t *testing.T) {
	store, _ := newTestStore(t, nil)
	// A directory that is not this store's staging shape must survive: a cleanup that deletes what it
	// does not recognize is how a reclaim becomes an outage.
	foreign := filepath.Join(store.stagingDir(), "not-a-digest")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatalf("create foreign directory: %v", err)
	}
	if _, err := store.Sweep(context.Background(), nil, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("the sweep removed a directory it did not create: %v", err)
	}
}

func TestOpenReportsDamageSeparatelyFromRefusal(t *testing.T) {
	store, _ := newTestStore(t, nil)
	ctx := context.Background()
	key := testKey(testCommitA)

	// Every state that is not "ready and present" is one uniform refusal, because the Git Server must
	// not let a caller tell "no such mirror" from "not yours to read".
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open on an unknown key = %v, want ErrNotReady", err)
	}
	prep, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open while preparing = %v, want ErrNotReady", err)
	}
	writeFile(t, filepath.Join(prep.Dir(), "payload"), "content")
	if _, err := prep.Publish(ctx, PublishVerification{Commit: key.Commit, Ref: testRef}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := store.Revoke(key, ""); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open on a revoked key = %v, want ErrNotReady", err)
	}
	if _, err := store.Destroy(key); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := store.Open(key); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Open on a destroyed key = %v, want ErrNotReady", err)
	}
}

// recordTempFiles lists the temporary files an interrupted atomic write could leave in the records
// directory. A clean store has none.
func recordTempFiles(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(store.recordsDir())
	if err != nil {
		t.Fatalf("read records: %v", err)
	}
	var leftovers []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".record-") {
			leftovers = append(leftovers, entry.Name())
		}
	}
	return leftovers
}
