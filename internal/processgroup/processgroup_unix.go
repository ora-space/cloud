//go:build unix

// Package processgroup ends a child process together with everything that child started.
//
// The default exec.CommandContext cancellation kills exactly one process. No Git invocation is one
// process: `git fetch` starts a transport helper and an indexer, `git push` starts send-pack, and all
// of them inherit the pipes and, in the Repo Executor, hold the redeemed credential in memory. Killing
// only the parent would leave the credential alive after the task that needed it ended.
//
// Putting the child at the head of its own process group makes cancellation reach every descendant,
// because a signal to the negated pid reaches every member. The group is the child's own, so nothing
// here can signal the caller or a sibling task.
package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay bounds how long Wait keeps waiting for the child's I/O after cancellation or exit.
//
// A descendant that escaped the group (it called setsid) could otherwise keep the pipes open and turn
// a timeout into an unbounded wait; after this delay exec closes the pipes and Wait returns.
const WaitDelay = 5 * time.Second

// Isolate makes command the head of a new process group, makes cancellation of its context kill that
// whole group with SIGKILL, and sets WaitDelay.
//
// It must be called before Start: the group is settled when the process is created. It replaces any
// Cancel and WaitDelay the caller set, because a child asked to stop that does not stop is exactly the
// case this exists for.
//
// A command built with exec.Command (Cancel == nil) is left alone: it has no context, so the group
// would be a signal nobody sends, while moving the child out of the group the caller's own service
// manager signals when it stops the whole tree.
func Isolate(command *exec.Cmd) {
	if command == nil || command.Cancel == nil {
		return
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	command.Cancel = func() error { return endGroup(command) }
	command.WaitDelay = WaitDelay
}

// endGroup kills the process group command heads.
//
// SIGKILL rather than SIGTERM: this runs only after the caller decided the work is over, and Git has
// no shutdown path that clears a credential from memory first. A group that is already gone is not an
// error — the context may end in the instant the command exits on its own — and os.ErrProcessDone
// tells exec.Cmd.Wait to keep the command's own exit status.
func endGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
