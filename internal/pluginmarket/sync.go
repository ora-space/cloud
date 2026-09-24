package pluginmarket

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"go.uber.org/zap"
)

// ErrSyncInFlight reports that another Sync holds the single-flight slot. The
// caller is turned away rather than queued, matching the desktop rebuild
// admission: the in-flight sync is already producing the result they asked for.
var ErrSyncInFlight = errors.New("plugin catalog sync already in flight")

// Source identifies the configured marketplace git repository. The namespace is
// deployment-owned (the default source keeps 'official') and never comes from
// the repository content.
type Source struct {
	Namespace string
	URL       string
	Branch    string
}

// CatalogSink is the PostgreSQL boundary the sync writes through. It is defined
// where it is consumed so unit tests substitute a fake and integration tests
// exercise the real transactional behavior. core.Store implements it.
type CatalogSink interface {
	// SeedPluginSource upserts the source row before the first sync, so a fresh
	// deployment serves catalog rows that always reference their source.
	SeedPluginSource(ctx context.Context, source Source) error
	// ReplacePluginCatalog atomically replaces the source's catalog snapshot
	// and stamps its synced_at. A failure leaves the previous snapshot intact.
	ReplacePluginCatalog(ctx context.Context, source Source, entries []Entry, indexedAt time.Time) error
	// RecordPluginSyncError keeps the previous snapshot and records the
	// failure reason on the source row for the next tick to retry.
	RecordPluginSyncError(ctx context.Context, namespace string, syncErr error) error
}

// Syncer clones or fast-forwards the marketplace repository and replaces the
// catalog snapshot. It owns no database handle and no goroutines; RunSyncLoop
// drives it from the process lifecycle.
type Syncer struct {
	Source  Source
	WorkDir string
	Sink    CatalogSink
	Log     *zap.Logger

	syncing atomic.Bool
}

// NewSyncer returns a ready-to-use syncer. WorkDir is the checkout directory;
// it is ephemeral cache — an interrupted clone is replaced on the next sync.
func NewSyncer(source Source, workDir string, sink CatalogSink, log *zap.Logger) *Syncer {
	return &Syncer{Source: source, WorkDir: workDir, Sink: sink, Log: log}
}

// Sync performs one bounded catalog refresh: seed, git fast-forward, scan,
// transactional replace. Network and filesystem work all happen outside any
// database transaction. Concurrent callers are turned away, not queued.
func (s *Syncer) Sync(ctx context.Context) error {
	if !s.syncing.CompareAndSwap(false, true) {
		return ErrSyncInFlight
	}
	defer s.syncing.Store(false)
	if e := s.Sink.SeedPluginSource(ctx, s.Source); e != nil {
		return fmt.Errorf("seed plugin source: %w", e)
	}
	if e := s.gitSync(ctx); e != nil {
		if recordErr := s.Sink.RecordPluginSyncError(ctx, s.Source.Namespace, e); recordErr != nil {
			return errors.Join(fmt.Errorf("sync marketplace: %w", e), fmt.Errorf("record sync error: %w", recordErr))
		}
		return fmt.Errorf("sync marketplace: %w", e)
	}
	entries, skipped, e := Scan(filepath.Join(s.WorkDir, "registry"), s.Source.Namespace, s.Source.URL)
	if e != nil {
		if recordErr := s.Sink.RecordPluginSyncError(ctx, s.Source.Namespace, e); recordErr != nil {
			return errors.Join(fmt.Errorf("scan marketplace: %w", e), fmt.Errorf("record sync error: %w", recordErr))
		}
		return fmt.Errorf("scan marketplace: %w", e)
	}
	for _, skip := range skipped {
		s.Log.Warn("skipped marketplace listing", zap.String("listing", skip))
	}
	if e := s.Sink.ReplacePluginCatalog(ctx, s.Source, entries, time.Now()); e != nil {
		return fmt.Errorf("replace plugin catalog: %w", e)
	}
	s.Log.Info("plugin catalog synced", zap.String("namespace", s.Source.Namespace), zap.Int("entries", len(entries)))
	return nil
}

// gitSync fast-forwards the checkout to the source branch. The first run
// clones; later runs fetch, check out the branch and pull --ff-only. A
// non-fast-forward update (the marketplace rewrote history) fails the sync and
// keeps the previous catalog, exactly like the desktop gitlancer pull.
func (s *Syncer) gitSync(ctx context.Context) error {
	ref := plumbing.NewBranchReferenceName(s.Source.Branch)
	if !isRepository(s.WorkDir) {
		return s.clone(ctx, ref)
	}
	repo, e := git.PlainOpen(s.WorkDir)
	if e != nil {
		return e
	}
	if e = repo.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin"}); e != nil && !errors.Is(e, git.NoErrAlreadyUpToDate) {
		return e
	}
	worktree, e := repo.Worktree()
	if e != nil {
		return e
	}
	if checkoutErr := worktree.Checkout(&git.CheckoutOptions{Branch: ref}); checkoutErr != nil {
		return checkoutErr
	}
	// go-git only supports fast-forward merges: a diverged branch surfaces as
	// ErrNonFastForwardMerge, which is exactly the --ff-only contract. An
	// unchanged origin reports NoErrAlreadyUpToDate, which is a clean no-op.
	if pullErr := worktree.PullContext(ctx, &git.PullOptions{RemoteName: "origin", ReferenceName: ref, SingleBranch: true}); pullErr != nil && !errors.Is(pullErr, git.NoErrAlreadyUpToDate) {
		return pullErr
	}
	return nil
}

// clone performs the first checkout into a temporary sibling and renames it
// over WorkDir, so a failed or interrupted clone never leaves a half-written
// directory the next sync would mistake for a repository.
func (s *Syncer) clone(ctx context.Context, ref plumbing.ReferenceName) error {
	tmp := s.WorkDir + ".tmp"
	if e := os.RemoveAll(tmp); e != nil {
		return e
	}
	if _, e := git.PlainCloneContext(ctx, tmp, false, &git.CloneOptions{
		URL:           s.Source.URL,
		ReferenceName: ref,
		SingleBranch:  true,
	}); e != nil {
		_ = os.RemoveAll(tmp)
		return e
	}
	if e := os.RemoveAll(s.WorkDir); e != nil {
		return e
	}
	return os.Rename(tmp, s.WorkDir)
}

func isRepository(dir string) bool {
	info, e := os.Stat(filepath.Join(dir, ".git"))
	return e == nil && info.IsDir()
}
