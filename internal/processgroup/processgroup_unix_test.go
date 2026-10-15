//go:build unix

package processgroup

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// descendantTree is a program that starts one background process and then stays alive, printing the
// descendant's pid on standard output.
//
// It is the shape every Git invocation in this repository has: the process the caller starts is not
// the process that does the work, and the descendant is the one that inherited the caller's pipes. The
// background process inherits standard output, and `wait` keeps the shell alive as its parent so
// nothing reparents the descendant before the test can look for it.
const descendantTree = "sleep 60 &\necho $!\nwait\n"

// TestIsolateEndsTheDescendantsOfTheCommandItLaunched is the direct evidence for what this package
// promises: the end of the command's context ends the process tree, and not only its head.
func TestIsolateEndsTheDescendantsOfTheCommandItLaunched(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", descendantTree)
	Isolate(command)
	descendant := startTree(t, command)

	cancel()
	// Waiting is not what ends the descendant — a wait for the head returns as soon as the head is
	// reaped — so this is here to leave nothing running before the assertion, and the assertion is
	// what the test is about.
	if err := command.Wait(); err == nil {
		t.Fatal("a command whose context ended was reported as having succeeded")
	}
	assertGone(t, descendant)
}

// TestADescendantOutlivesACommandThatWasNotIsolated is the control for the test above.
//
// It runs the same program with nothing isolating it, so that the isolation is what the first test is
// measuring rather than an inherent property of the shell: the context ends the process the caller
// started, and that process's own child is still running afterwards.
func TestADescendantOutlivesACommandThatWasNotIsolated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", descendantTree)
	descendant := startTree(t, command)
	// Nothing else will clean this up — that is the point of the case — so the test does.
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })

	cancel()
	if err := command.Wait(); err == nil {
		t.Fatal("a command whose context ended was reported as having succeeded")
	}
	if err := syscall.Kill(descendant, 0); err != nil {
		t.Fatalf("the descendant %d did not survive the end of the command: %v", descendant, err)
	}
}

// startTree starts a process tree and returns the pid of its descendant.
func startTree(t *testing.T, command *exec.Cmd) int {
	t.Helper()
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("connect to the command's standard output: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start the process tree: %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read the descendant's pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		t.Fatalf("the process tree printed %q, which is not a pid: %v", line, err)
	}
	return pid
}

// assertGone fails unless the process is gone.
//
// It polls, because a process that has been signaled is a zombie until something reaps it and a
// zombie still answers signal 0: the wait is for the reap, not for the signal. It is bounded, so a
// survivor fails the test instead of hanging it, and it does not leave the survivor behind.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the descendant %d of the command is still running after its context ended", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
