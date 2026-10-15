//go:build unix

package gitserver

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestTheTimeoutLeavesNoDescendantOfTheGitProcessBehind covers the other half of the bound the fault
// test above measures.
//
// `upload-pack` is not the process that produces the stream: it starts a packer, and the packer
// inherits the handler's output. Ending the read ends the request, but a packer left running is a
// child of the service that keeps working for a caller that is gone — and on a service whose whole
// job is to serve repository content to callers whose authorization it checked, the child that
// outlives the request is also the child nobody is accounting for.
func TestTheTimeoutLeavesNoDescendantOfTheGitProcessBehind(t *testing.T) {
	descendants := filepath.Join(t.TempDir(), "descendants")
	// The background sleeper inherits the shell's standard output, which `exec` then replaces: the
	// descendant is what holds the write end, exactly as a packer does.
	gitPath := fakeGit(t, "#!/bin/sh\nsleep 60 &\necho $! >> "+quoteForShell(descendants)+"\nexec sleep 60\n")
	f, base, _ := authorizedFixture(t, gitPath, 250*time.Millisecond)

	response := f.post(t, base+"/git-upload-pack", nil, nil)
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
	assertNoDescendantSurvives(t, descendants)
}

// quoteForShell wraps a path so a shell reads it as one word. Nothing in a temporary directory needs
// it today; it is here because the alternative is a test that silently stops observing anything if a
// path ever gains a character a shell would split on.
func quoteForShell(path string) string { return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'" }

// assertNoDescendantSurvives fails if any process the fake git recorded is still running.
//
// What is asserted is the process rather than a proxy for it: the double records the pid of the
// descendant it started. The check polls, because a killed process is a zombie until something reaps
// it and a zombie still answers signal 0 — a signal that was delivered and a process that is gone are
// two different facts, and this one is about the second.
func assertNoDescendantSurvives(t *testing.T, log string) {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read the descendants the fake git recorded: %v", err)
	}
	pids := strings.Fields(string(raw))
	if len(pids) == 0 {
		// Without a recorded pid the loop below would agree with a double that no longer leaves a
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
			t.Fatalf("the descendant %d of the git process outlived the request", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
