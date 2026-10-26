//go:build !unix

// Package processgroup ends a child process together with everything that child started.
//
// This platform has no process group to address, so Isolate is a no-op and a command runs exactly as
// exec.CommandContext produced it. The split is a build boundary rather than a runtime condition so a
// build for such a platform does not compile against a guarantee it does not have; the Repo Executor
// is deployed only as a Linux container, where the Unix implementation applies.
package processgroup

import (
	"os/exec"
	"time"
)

// WaitDelay is the delay the Unix implementation sets; it is declared here so callers compile on every
// platform. Isolate does not apply it on this platform.
const WaitDelay = 5 * time.Second

// Isolate does nothing on this platform; see the package comment.
func Isolate(*exec.Cmd) {}
