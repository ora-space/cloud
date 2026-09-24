package pluginmarket

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"go.uber.org/zap"
)

// fakeSink records the sink calls so sync tests can assert the transaction
// boundary behavior without PostgreSQL.
type fakeSink struct {
	mu        sync.Mutex
	seeds     int
	replaced  int
	lastCount int
	syncErrs  []string
	// blockSeed, when non-nil, is closed by the test to release SeedPluginSource.
	blockSeed chan struct{}
}

func (f *fakeSink) SeedPluginSource(_ context.Context, _ Source) error {
	f.mu.Lock()
	f.seeds++
	f.mu.Unlock()
	if f.blockSeed != nil {
		<-f.blockSeed
	}
	return nil
}

func (f *fakeSink) ReplacePluginCatalog(_ context.Context, _ Source, entries []Entry, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaced++
	f.lastCount = len(entries)
	return nil
}

func (f *fakeSink) RecordPluginSyncError(_ context.Context, _ string, err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncErrs = append(f.syncErrs, err.Error())
	return nil
}

func (f *fakeSink) snapshot() (seeds, replaced, lastCount int, syncErrs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seeds, f.replaced, f.lastCount, append([]string{}, f.syncErrs...)
}

// fixtureMarketplace builds a local git repository with the given listings
// (directory → orax.toml) on branch main, returning the repo directory.
func fixtureMarketplace(t *testing.T, listings map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, e := git.PlainInitWithOptions(dir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: plumbing.Main}})
	if e != nil {
		t.Fatal(e)
	}
	wt, e := repo.Worktree()
	if e != nil {
		t.Fatal(e)
	}
	for rel, manifest := range listings {
		path := filepath.Join(dir, rel)
		if e := os.MkdirAll(filepath.Dir(path), 0o700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte(manifest), 0o600); e != nil {
			t.Fatal(e)
		}
		if _, e := wt.Add(rel); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := wt.Commit("fixture", &git.CommitOptions{Author: signature()}); e != nil {
		t.Fatal(e)
	}
	return dir
}

func signature() *object.Signature {
	return &object.Signature{Name: "Marketplace Fixture", Email: "fixture@example.invalid", When: time.Unix(1700000000, 0)}
}

// commitFile adds or replaces one file in an already-checked-out repository.
func commitFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	repo, e := git.PlainOpen(dir)
	if e != nil {
		t.Fatal(e)
	}
	wt, e := repo.Worktree()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, rel)
	if e := os.MkdirAll(filepath.Dir(path), 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(content), 0o600); e != nil {
		t.Fatal(e)
	}
	if _, e := wt.Add(rel); e != nil {
		t.Fatal(e)
	}
	if _, e := wt.Commit("update", &git.CommitOptions{Author: signature()}); e != nil {
		t.Fatal(e)
	}
}

// commitAll stages every change (including deletions) in a checked-out
// repository and commits it, leaving a clean worktree.
func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	repo, e := git.PlainOpen(dir)
	if e != nil {
		t.Fatal(e)
	}
	wt, e := repo.Worktree()
	if e != nil {
		t.Fatal(e)
	}
	if e := wt.AddWithOptions(&git.AddOptions{All: true}); e != nil {
		t.Fatal(e)
	}
	if _, e := wt.Commit(message, &git.CommitOptions{Author: signature()}); e != nil {
		t.Fatal(e)
	}
}

func fileURL(dir string) string {
	slashed := filepath.ToSlash(dir)
	if strings.HasPrefix(slashed, "/") {
		return "file://" + slashed
	}
	return "file:///" + slashed
}

func newTestSyncer(t *testing.T, origin string, sink *fakeSink) *Syncer {
	t.Helper()
	return NewSyncer(
		Source{Namespace: "official", URL: fileURL(origin), Branch: "main"},
		filepath.Join(t.TempDir(), "checkout"),
		sink, zap.NewNop(),
	)
}

const oneListing = "registry/a/hello-world/orax.toml"

var helloManifest = "resolver = 1\nidentifier = \"hello-world\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Hello.\"\nurl = \"https://example.invalid/h.orax\"\nsha256 = \"" + strings.Repeat("ab", 32) + "\"\n"

func TestSyncClonesAndReplacesCatalog(t *testing.T) {
	origin := fixtureMarketplace(t, map[string]string{oneListing: helloManifest})
	sink := &fakeSink{}
	syncer := newTestSyncer(t, origin, sink)
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	seeds, replaced, lastCount, syncErrs := sink.snapshot()
	if seeds != 1 || replaced != 1 || lastCount != 1 || len(syncErrs) != 0 {
		t.Fatalf("sink = seeds:%d replaced:%d count:%d errors:%v", seeds, replaced, lastCount, syncErrs)
	}
	if !isRepository(syncer.WorkDir) {
		t.Fatal("first sync must leave a cloned repository behind")
	}
}

func TestSyncFastForwardsOnLaterRuns(t *testing.T) {
	origin := fixtureMarketplace(t, map[string]string{oneListing: helloManifest})
	sink := &fakeSink{}
	syncer := newTestSyncer(t, origin, sink)
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	commitFile(t, origin, "registry/b/tool/orax.toml", "resolver = 1\nidentifier = \"tool\"\nkind = \"hook\"\nversion = \"2.0.0\"\ndescription = \"Tool.\"\n[[targets]]\ntarget = \"x86_64-unknown-linux-gnu\"\nurl = \"https://example.invalid/t.orax\"\nsha256 = \""+strings.Repeat("cd", 32)+"\"\n")
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	_, replaced, lastCount, _ := sink.snapshot()
	if replaced != 2 || lastCount != 2 {
		t.Fatalf("second sync must replace the catalog with the fast-forwarded content: replaced:%d count:%d", replaced, lastCount)
	}
}

func TestSyncNonFastForwardKeepsCatalog(t *testing.T) {
	origin := fixtureMarketplace(t, map[string]string{oneListing: helloManifest})
	sink := &fakeSink{}
	syncer := newTestSyncer(t, origin, sink)
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	// Divergent local history: the checkout commits on its own, the origin
	// moves on; the pull must refuse as a non-fast-forward update.
	commitFile(t, syncer.WorkDir, "local.txt", "local divergence\n")
	commitFile(t, origin, "registry/c/skill/orax.toml", "resolver = 1\nidentifier = \"skill\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Skill.\"\n")
	e := syncer.Sync(context.Background())
	if e == nil || !strings.Contains(e.Error(), "non-fast-forward") {
		t.Fatalf("sync must report the non-fast-forward update, got %v", e)
	}
	_, replaced, _, syncErrs := sink.snapshot()
	if replaced != 1 {
		t.Fatalf("a refused pull must not replace the catalog, replaced:%d", replaced)
	}
	if len(syncErrs) != 1 {
		t.Fatalf("the failure must be recorded once, errors:%v", syncErrs)
	}
}

func TestSyncScanFailureKeepsCatalog(t *testing.T) {
	origin := fixtureMarketplace(t, map[string]string{oneListing: helloManifest})
	sink := &fakeSink{}
	syncer := newTestSyncer(t, origin, sink)
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := os.RemoveAll(filepath.Join(syncer.WorkDir, "registry")); e != nil {
		t.Fatal(e)
	}
	// Commit the removal so the checkout is clean and the pull succeeds; the
	// failure then happens at the scan, which is what this test targets.
	commitAll(t, syncer.WorkDir, "drop registry")
	if e := syncer.Sync(context.Background()); e == nil {
		t.Fatal("a checkout without registry must fail the scan")
	}
	_, replaced, _, syncErrs := sink.snapshot()
	if replaced != 1 {
		t.Fatalf("a failed scan must keep the previous catalog, replaced:%d", replaced)
	}
	if len(syncErrs) != 1 || !strings.Contains(syncErrs[0], "registry") {
		t.Fatalf("the scan failure must be recorded, errors:%v", syncErrs)
	}
}

func TestSyncSingleFlightTurnsAwayConcurrentCallers(t *testing.T) {
	origin := fixtureMarketplace(t, map[string]string{oneListing: helloManifest})
	sink := &fakeSink{blockSeed: make(chan struct{})}
	syncer := newTestSyncer(t, origin, sink)
	firstDone := make(chan error, 1)
	go func() { firstDone <- syncer.Sync(context.Background()) }()
	// Give the first call time to enter the slot, then attempt a second.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.mu.Lock()
		entered := sink.seeds > 0
		sink.mu.Unlock()
		if entered || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if e := syncer.Sync(context.Background()); !errors.Is(e, ErrSyncInFlight) {
		t.Fatalf("concurrent sync must be turned away, got %v", e)
	}
	close(sink.blockSeed)
	if e := <-firstDone; e != nil {
		t.Fatal(e)
	}
	// The slot is released: a later sync succeeds.
	if e := syncer.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
}
