//go:build unix

// Package processgroup ends a child process together with everything that child started.
//
// A context that ends is supposed to end the work it was given, and the default cancellation kills
// exactly one process: the one `exec` started. None of the processes this repository launches is
// alone. `git fetch` starts a transport helper and an indexer, `git push` starts a send-pack that
// starts a receive side, `git upload-pack` starts a packer — and every one of them inherits the
// pipes. Killing the parent leaves the descendants running with the parent's file descriptors, and
// with whatever the parent had in memory: in the credential-executing component that includes the
// staged Git token, which the helper only ever hands to the process Git is running.
//
// Putting the child at the head of its own process group is what makes the caller's cancellation
// reach them, because a group is addressed by the negated pid and a signal to it reaches every member.
// The group is the child's own rather than the caller's, so nothing here can signal the caller or a
// sibling job.
//
// See README.md for why the mechanism is a build-tagged package and what it does not claim.
package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Isolate makes command the head of a new process group, and makes the end of that group — rather
// than the end of command alone — what its context does.
//
// It must be called before the command is started: both the group and the cancellation are settled
// as the process is created, and a command started without them runs exactly as it would have.
//
// It replaces any cancellation the caller had set. The two would otherwise answer the same event, and
// there is no reading of "the caller wanted this process to end differently" that survives the
// credential argument: a child that is asked to stop and does not is the case this exists for.
//
// A command built with `exec.Command` rather than `exec.CommandContext` is left alone. It has no
// context, so the group would be a signal nobody sends — `Cancel` is nil, which is what tells the two
// apart — while taking the child out of the process group the caller's own shell or service manager
// signals when it wants the whole tree stopped.
func Isolate(command *exec.Cmd) {
	if command == nil || command.Cancel == nil {
		return
	}
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
	command.Cancel = func() error { return endGroup(command) }
}

// endGroup kills the process group command heads.
//
// SIGKILL rather than SIGTERM, because this runs only on the paths where the caller has already
// decided the work is over — a deadline, a cancelled request, a shutdown — and a Git process asked
// politely may spend an unbounded time finishing a transfer it was told to abandon. What is being
// protected is the credential in its descendants' memory, and Git has no shutdown path that clears
// it first.
//
// A group that is already gone is not an error. The context can expire in the same instant the
// command exits on its own, and reporting ESRCH would turn a command that finished into a failure;
// wrapping `os.ErrProcessDone` is what tells `exec.Cmd.Wait` to keep the command's own exit status.
func endGroup(command *exec.Cmd) error {
	// Cancel runs only after a successful Start, so the process is there. The check is for this
	// package's exported surface, where a caller could install it on a command it starts another way.
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
