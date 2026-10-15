//go:build unix

package cloneexec

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestATimeoutLeavesNoDescendantOfTheCloneProcessBehind is the security half of the timeout.
//
// The tests in `fault_test.go` measure that the call returns and that the material is destroyed. They
// cannot measure whether the process that was running Git is gone, and that is the fact the
// credential turns on: the helper hands the token to the process Git is running, and the descendant
// that inherited the pipes — here the background sleeper the double starts, in a real clone the
// transport helper — holds it in its own memory. A deadline that ends the wait without ending that
// process leaves a token alive in a process nobody is tracking any more.
func TestATimeoutLeavesNoDescendantOfTheCloneProcessBehind(t *testing.T) {
	fixture := newTestFixture(t, func(c *Config) { c.Timeout = 250 * time.Millisecond })
	hangingCloneGitDouble(t, fixture)

	if _, err := fixture.executor.Execute(context.Background(), fixture.issue(t, nil)); err == nil {
		t.Fatal("a clone that never answered was reported as a success")
	}
	assertNoDescendantSurvives(t, fixture.config.GitPath)
}

// TestATimeoutLeavesNoDescendantOfTheMirrorFetchBehind is the same obligation on the mirror path,
// which stages its own material for its own fetch.
func TestATimeoutLeavesNoDescendantOfTheMirrorFetchBehind(t *testing.T) {
	fixture := newMirrorFixture(t, func(c *MirrorConfig) { c.Timeout = 250 * time.Millisecond })
	hangingMirrorGitDouble(t, fixture)

	if _, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil)); err == nil {
		t.Fatal("a fetch that never answered was reported as a success")
	}
	assertNoDescendantSurvives(t, fixture.config.GitPath)
}

// assertNoDescendantSurvives fails if any process a hanging Git double left behind is still running.
//
// The double records the pid of the descendant it started, so what is asserted is the process rather
// than a proxy for it. The check polls, because a killed process is a zombie until something reaps it
// and a zombie still answers signal 0: a signal that was delivered and a process that is gone are two
// different facts, and this one is about the second.
func assertNoDescendantSurvives(t *testing.T, gitPath string) {
	t.Helper()
	raw, err := os.ReadFile(descendantLog(gitPath))
	if err != nil {
		t.Fatalf("read the descendants the Git double recorded: %v", err)
	}
	pids := strings.Fields(string(raw))
	if len(pids) == 0 {
		// Without a recorded pid the assertions below would pass by having nothing to check, which
		// would make this test agree with a double that no longer starts a descendant at all.
		t.Fatal("the Git double recorded no descendant, so this test proved nothing")
	}
	for _, field := range pids {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			t.Fatalf("the Git double recorded %q, which is not a pid: %v", field, err)
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
			t.Fatalf("the descendant %d of the Git process outlived the execution", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
