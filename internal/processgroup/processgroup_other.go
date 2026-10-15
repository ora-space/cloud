//go:build !unix

// Package processgroup ends a child process together with everything that child started.
//
// On this platform there is no process group to address, so `Isolate` is a no-op and a command runs
// exactly as `exec.CommandContext` produced it: the end of the context kills the process that was
// started, and a descendant that inherited its pipes keeps running until its own work ends.
//
// This is a build boundary rather than a runtime condition so that a build for a platform without the
// mechanism cannot compile against a guarantee it does not have. The components that launch Git — the
// credential executor, the internal Git Server and the mirror publisher — are deployed as Linux
// containers, and the mechanism exists for them. See the Unix file for what they rely on and
// README.md for the trade this makes.
package processgroup

import "os/exec"

// Isolate does nothing on this platform; see the package comment.
func Isolate(*exec.Cmd) {}
