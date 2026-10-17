//go:build unix

package processgroup

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// descendantTree starts one background process, prints its pid and stays alive as its parent, which
// is the shape of every Git invocation: the process the caller starts is not the one doing the work.
const descendantTree = "sleep 60 &\necho $!\nwait\n"

func TestIsolateEndsTheDescendantsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "/bin/sh", "-c", descendantTree)
	Isolate(command)
	descendant := startTree(t, command)

	cancel()
	if err := command.Wait(); err == nil {
		t.Fatal("a command whose context ended was reported as having succeeded")
	}
	assertGone(t, descendant)
}

// TestDescendantOutlivesUnisolatedCommand is the control: without Isolate the descendant survives, so
// the first test measures Isolate rather than a property of the shell.
func TestDescendantOutlivesUnisolatedCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "/bin/sh", "-c", descendantTree)
	// Without Isolate the descendant holds stdout open; bound Wait so the control does not hang.
	command.WaitDelay = time.Second
	descendant := startTree(t, command)
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })

	cancel()
	if err := command.Wait(); err == nil {
		t.Fatal("a command whose context ended was reported as having succeeded")
	}
	if err := syscall.Kill(descendant, 0); err != nil {
		t.Fatalf("the descendant %d did not survive the end of an unisolated command: %v", descendant, err)
	}
}

func TestIsolateConfiguresTheCommand(t *testing.T) {
	cases := []struct {
		name        string
		command     func() *exec.Cmd
		wantIsolate bool
	}{
		{name: "context command", command: func() *exec.Cmd {
			return exec.CommandContext(context.Background(), "/bin/true")
		}, wantIsolate: true},
		{name: "context command with existing attributes", command: func() *exec.Cmd {
			c := exec.CommandContext(context.Background(), "/bin/true")
			c.SysProcAttr = &syscall.SysProcAttr{}
			c.WaitDelay = time.Hour
			return c
		}, wantIsolate: true},
		{name: "command without context is left alone", command: func() *exec.Cmd {
			return exec.Command("/bin/true")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.command()
			Isolate(command)
			isolated := command.SysProcAttr != nil && command.SysProcAttr.Setpgid
			if isolated != tc.wantIsolate {
				t.Fatalf("Setpgid = %v, want %v", isolated, tc.wantIsolate)
			}
			if tc.wantIsolate && command.WaitDelay != WaitDelay {
				t.Fatalf("WaitDelay = %v, want %v", command.WaitDelay, WaitDelay)
			}
			if !tc.wantIsolate && (command.WaitDelay != 0 || command.Cancel != nil) {
				t.Fatal("a command without a context was modified")
			}
		})
	}
	Isolate(nil) // must not panic
}

// TestIsolateKeepsTheExitStatusOfAFinishedCommand shows that ending the group after the command exited does
// not turn its success into a failure.
func TestIsolateKeepsTheExitStatusOfAFinishedCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "/bin/true")
	Isolate(command)
	if err := command.Run(); err != nil {
		t.Fatalf("a command that finished on its own failed: %v", err)
	}
	if err := endGroup(command); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("ending an already-reaped group = %v, want os.ErrProcessDone", err)
	}
}

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

// assertGone polls because a killed process is a zombie until reaped, and a zombie still answers
// signal 0. It is bounded so a survivor fails the test instead of hanging it.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the descendant %d is still running after its context ended", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
