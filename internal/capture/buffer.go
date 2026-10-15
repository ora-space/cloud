// Package capture holds what a child process wrote, in memory, under a bound.
//
// Every Git invocation in this repository is a child process whose output a person's repository gets
// to influence: `git fetch` forwards whatever the remote sends over the sideband, `git ls-remote`
// prints every ref a server advertises, and a failing command's diagnostics are the remote's own
// words. A `strings.Builder` in front of that is a memory budget handed to whoever chose the
// repository, in a process that is running someone else's work.
//
// Buffer keeps the first Limit bytes and counts the rest, and it never fails a write. The second half
// is not a detail, because the way a child's stream is consumed is `io.Copy`: a short write is an
// error there, and a copy that ends early closes the pipe it was reading, so a child that filled the
// bound would be ended by the bound rather than by anything it did wrong — a Git killed by SIGPIPE
// because it printed too much. Returning the length the writer was given, and dropping what does not
// fit, is what keeps the bound a property of this process's memory instead of a new failure mode for
// the child.
//
// Whether the bound was reached is part of what the buffer reports. Callers differ in what they must
// do about it — a log line states the truncation, a fact cannot be read out of a stream that was cut,
// and a text that has to be redacted against a secret cannot be redacted with confidence once the
// secret may have been cut in half — so the buffer counts and lets each caller decide.
package capture

import (
	"bytes"
	"fmt"
)

// Buffer is a bounded capture of one stream.
//
// It is an `io.Writer` and may be handed to `exec.Cmd` directly. A zero Limit keeps nothing, which is
// the honest reading of a bound of zero; callers that want the stream set one.
type Buffer struct {
	// Limit is how many bytes are kept. Everything written past it is counted in Dropped.
	Limit int
	// Dropped is how many bytes were written past the bound and not kept.
	Dropped int
	buf     bytes.Buffer
}

// Write keeps what fits, counts the rest, and reports the write as complete.
//
// The value returned is the length the caller passed, whatever was kept: the package comment says why
// a shorter one would end the child rather than the capture. There is no path that returns an error,
// because a capture that cannot keep its stream has no way to tell the child that would not be a
// failure the child did not cause.
func (b *Buffer) Write(p []byte) (int, error) {
	given := len(p)
	remaining := b.Limit - b.buf.Len()
	if remaining <= 0 {
		b.Dropped += given
		return given, nil
	}
	if given > remaining {
		b.buf.Write(p[:remaining])
		b.Dropped += given - remaining
		return given, nil
	}
	b.buf.Write(p)
	return given, nil
}

// Len reports how many bytes were written in total, kept and dropped together, so that a caller can
// tell a short stream from one that was cut.
func (b *Buffer) Len() int { return b.buf.Len() + b.Dropped }

// Truncated reports whether the stream was longer than the bound.
func (b *Buffer) Truncated() bool { return b.Dropped > 0 }

// String returns what was kept, saying in the text that the stream did not end there.
//
// The marker is part of the rendering rather than of what was captured, so it cannot be mistaken for
// the child's own output: diagnostics that stop mid-sentence should look like diagnostics that stop
// mid-sentence rather than like a command that printed exactly this much.
func (b *Buffer) String() string {
	if b.Dropped == 0 {
		return b.buf.String()
	}
	return fmt.Sprintf("%s…(%d bytes dropped)", b.buf.String(), b.Dropped)
}
