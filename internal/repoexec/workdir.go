package repoexec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Work root layout: every task directory lives under tasks/, every staged credential under
// credentials/. Both are emptied at startup; nothing under them outlives a task.
const (
	tasksDirectory       = "tasks"
	credentialsDirectory = "credentials"
)

// sizePollInterval is how often the size monitor measures a task directory (ADR D4).
const sizePollInterval = 250 * time.Millisecond

// errTooLarge is the cancellation cause when a task directory exceeds the deployment limit.
var errTooLarge = errors.New("task directory exceeds the size limit")

// prepareWorkRoot creates the work root layout with private modes and removes everything a previous
// process left behind: a crashed task's repository and, more importantly, its staged credential.
func prepareWorkRoot(root string, sweepCredentials func(string) error) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !shellSafe(root) {
		return errors.New("work root must be an absolute, clean path of safe characters")
	}
	for _, dir := range []string{root, filepath.Join(root, tasksDirectory), filepath.Join(root, credentialsDirectory)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create work directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restrict work directory: %w", err)
		}
	}
	tasks := filepath.Join(root, tasksDirectory)
	entries, err := os.ReadDir(tasks)
	if err != nil {
		return fmt.Errorf("read task root: %w", err)
	}
	for _, entry := range entries {
		if err := removeVerified(filepath.Join(tasks, entry.Name())); err != nil {
			return err
		}
	}
	if err := sweepCredentials(filepath.Join(root, credentialsDirectory)); err != nil {
		return fmt.Errorf("sweep credential material: %w", err)
	}
	return nil
}

// newTaskDirectory creates one private task directory. It is the bare repository's parent and HOME.
func newTaskDirectory(root string) (string, error) {
	dir, err := os.MkdirTemp(filepath.Join(root, tasksDirectory), "task-")
	if err != nil {
		return "", fmt.Errorf("create task directory: %w", err)
	}
	return dir, nil
}

// removeVerified removes path and proves it is gone.
func removeVerified(path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove task directory: %w", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("task directory survived its removal")
	}
	return nil
}

// directorySize sums the sizes of regular files under dir. Entries vanishing during the walk are
// skipped: Git renames and deletes temporary files constantly.
func directorySize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // A vanished entry is not an error for a size estimate.
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

// watchSize cancels the task with errTooLarge as soon as dir exceeds limit. Git's fetch has no size
// bound of its own, so this is the executor's; the Git Server's receive.maxInputSize is the second.
// The returned stop function ends the monitor and waits for it.
func watchSize(ctx context.Context, dir string, limit int64, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(sizePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if directorySize(dir) > limit {
					cancel(errTooLarge)
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// shellSafe reports whether s consists only of characters that keep their meaning in a shell word.
// Git runs a credential helper through the shell, so the helper path and material directory that
// form its command must not be able to change what runs.
func shellSafe(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '/' && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
