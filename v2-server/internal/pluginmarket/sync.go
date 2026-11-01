package pluginmarket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// CatalogSink atomically replaces the persisted catalog snapshot.
type CatalogSink interface {
	ReplacePluginCatalog(context.Context, []Entry, time.Time) error
}

// Source identifies one marketplace Git repository.
type Source struct {
	Namespace string
	URL       string
	Branch    string
}

// Syncer owns the marketplace checkout and synchronizes it into PostgreSQL.
type Syncer struct {
	mu      sync.Mutex
	Source  Source
	WorkDir string
}

// ValidBranch accepts the Git branch grammar used by the marketplace configuration while
// rejecting option-like values, traversal and characters Git would reinterpret.
func ValidBranch(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || strings.Contains(value, "..") ||
		strings.Contains(value, "//") || strings.ContainsAny(value, "\\ ~^:?*[") {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e || char == '@' || char == '{' || char == '}' {
			return false
		}
	}
	return true
}

// Sync fast-forwards the marketplace checkout, validates every listing and atomically replaces
// the catalog snapshot. A Git, scan or database failure keeps the previous successful snapshot.
func (s *Syncer) Sync(ctx context.Context, sink CatalogSink, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.gitSync(ctx); e != nil {
		return fmt.Errorf("sync marketplace checkout: %w", e)
	}
	entries, skipped, e := Scan(filepath.Join(s.WorkDir, "registry"), s.Source.Namespace, s.Source.URL)
	if e != nil {
		return fmt.Errorf("scan marketplace: %w", e)
	}
	for _, listing := range skipped {
		slog.Warn("skipped marketplace listing", "listing", listing)
	}
	if e := sink.ReplacePluginCatalog(ctx, entries, now.UTC()); e != nil {
		return fmt.Errorf("replace marketplace catalog: %w", e)
	}
	slog.Info("plugin marketplace synced", "entries", len(entries), "branch", s.Source.Branch)
	return nil
}

// Loop refreshes the snapshot on each interval until ctx is cancelled.
func (s *Syncer) Loop(ctx context.Context, interval time.Duration, sink CatalogSink) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if e := s.Sync(ctx, sink, now); e != nil && !errors.Is(e, context.Canceled) {
				slog.Warn("plugin marketplace refresh failed; retaining previous snapshot", "error", e)
			}
		}
	}
}

func (s *Syncer) gitSync(ctx context.Context) error {
	if _, e := os.Stat(filepath.Join(s.WorkDir, ".git")); e == nil {
		if _, e := runGit(ctx, "git", "-C", s.WorkDir, "fetch", "--depth=1", "origin", s.Source.Branch); e != nil {
			return e
		}
		if _, e := runGit(ctx, "git", "-C", s.WorkDir, "checkout", "--detach", "--force", "FETCH_HEAD"); e != nil {
			return e
		}
		return nil
	}
	tmp := s.WorkDir + ".tmp"
	if e := os.RemoveAll(tmp); e != nil {
		return fmt.Errorf("clear marketplace temp checkout: %w", e)
	}
	if _, e := runGit(ctx, "git", "clone", "--depth=1", "--branch", s.Source.Branch, "--single-branch", s.Source.URL, tmp); e != nil {
		_ = os.RemoveAll(tmp)
		return e
	}
	if e := os.RemoveAll(s.WorkDir); e != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("clear marketplace checkout: %w", e)
	}
	if e := os.Rename(tmp, s.WorkDir); e != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("install marketplace checkout: %w", e)
	}
	return nil
}

func runGit(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	output, e := command.CombinedOutput()
	if e != nil {
		message := strings.TrimSpace(string(output))
		if len(message) > 8<<10 {
			message = message[:8<<10]
		}
		if message == "" {
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), e)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), e, message)
	}
	return output, nil
}
