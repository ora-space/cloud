//go:build unix

package mirrorpublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// TestTheTimeoutLeavesNoDescendantOfTheFetchProcessBehind covers the other half of the bound the
// timeout test above measures.
//
// The fetcher starts the deployment-fixed credential helper, and the helper is a descendant: the
// staged token is handed to the process Git is running, so a deadline that ends the wait without
// ending that process leaves the token alive in a process the publisher is no longer tracking. The
// fake git here records the pid of the descendant it leaves behind, so the assertion is about the
// process rather than about a proxy for it.
func TestTheTimeoutLeavesNoDescendantOfTheFetchProcessBehind(t *testing.T) {
	descendants := filepath.Join(t.TempDir(), "descendants")
	gitPath := filepath.Join(t.TempDir(), "git")
	// The background sleeper inherits the shell's standard output, which `exec` then replaces: the
	// descendant is what holds the write end, exactly as the transport helper does.
	script := "#!/bin/sh\nsleep 60 &\necho $! >> " + quoteForShell(descendants) + "\nexec sleep 60\n"
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	fetcher, err := NewGitFetcher(GitConfig{
		GitPath:      gitPath,
		Home:         t.TempDir(),
		HelperPath:   "/usr/local/bin/git-credential-ora",
		MaterialRoot: t.TempDir(),
		Timeout:      250 * time.Millisecond,
	}, stubMaterial{})
	if err != nil {
		t.Fatalf("NewGitFetcher: %v", err)
	}
	reference := &Reference{
		Key:      mirror.Key{ProjectID: testProject, Repository: testRepoA, Commit: testCommitA},
		TenantID: testTenant,
		Upstream: "main",
	}
	if _, err := fetcher.Fetch(context.Background(), reference, filepath.Join(t.TempDir(), "mirror")); err == nil {
		t.Fatal("the fetch reported success against a git that never exits")
	}
	assertNoDescendantSurvives(t, descendants)
}

// quoteForShell wraps a path so a shell reads it as one word, which is what a fake git's script needs
// when it is handed a path from outside itself.
func quoteForShell(path string) string { return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'" }

// assertNoDescendantSurvives fails if any process the fake git recorded is still running.
//
// The check polls, because a killed process is a zombie until something reaps it and a zombie still
// answers signal 0: a signal that was delivered and a process that is gone are two different facts,
// and this one is about the second.
func assertNoDescendantSurvives(t *testing.T, log string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the descendants the fake git recorded: %v", err)
	}
	pids := strings.Fields(string(raw))
	if len(pids) == 0 {
		// Without a recorded pid the loop below would agree with a fake git that no longer leaves a
		// descendant at all, which would make this test prove nothing.
		t.Fatal("the fake git recorded no descendant, so this test proved nothing")
	}
	for _, field := range pids {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			t.Fatalf("the fake git recorded %q, which is not a pid: %v", field, err)
		}
		assertProcessGone(t, pid)
	}
}

// assertProcessGone fails unless the process is gone, and does not leave it behind if it is not.
func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the descendant %d of the git process outlived the fetch", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
