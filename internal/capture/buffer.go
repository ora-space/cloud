// Package capture holds what a child process wrote, in memory, under a bound.
//
// The output of every Git process the Repo Executor starts is decided by somebody else: `git fetch`
// forwards whatever the remote sends over the sideband, `git ls-remote` prints every ref a server
// advertises, and a failing command's diagnostics are the remote's own words. An unbounded buffer in
// front of that hands this process's memory budget to whoever chose the repository.
//
// Buffer keeps the first Limit bytes and counts the rest, and it never fails a write. The second half
// matters because a child's stream reaches the buffer through `io.Copy`: a short write is an error
// there, and a copy that ends early closes the pipe it was reading, so a child that filled the bound
// would be killed by SIGPIPE for printing too much. Reporting the full length and dropping what does
// not fit keeps the bound a property of this process's memory instead of a new failure mode for the
// child.
//
// Whether the bound was reached is part of what the buffer reports, because callers must react
// differently: a log line states the truncation, while a fact (a ref list, a push report) cannot be
// read out of a stream that was cut.
package capture

import (
	"bytes"
	"fmt"
)

// Buffer is a bounded capture of one stream.
//
// It is an io.Writer and may be handed to exec.Cmd directly. A zero Limit keeps nothing, which is the
// honest reading of a bound of zero. A Buffer is not safe for concurrent writers; exec.Cmd gives each
// stream its own copying goroutine, so one Buffer per stream is the intended use.
type Buffer struct {
	// Limit is how many bytes are kept. Everything written past it is counted in Dropped.
	Limit int
	// Dropped is how many bytes were written past the bound and not kept.
	Dropped int
	buf     bytes.Buffer
}

// Write keeps what fits, counts the rest, and reports the write as complete.
//
// The returned length is always len(p) and the error is always nil: see the package comment for why
// a shorter write would end the child rather than the capture.
func (b *Buffer) Write(p []byte) (int, error) {
	given := len(p)
	remaining := b.Limit - b.buf.Len()
	switch {
	case remaining <= 0:
		b.Dropped += given
	case given > remaining:
		b.buf.Write(p[:remaining])
		b.Dropped += given - remaining
	default:
		b.buf.Write(p)
	}
	return given, nil
}

// Len reports how many bytes were written in total, kept and dropped together, so that a caller can
// tell a short stream from one that was cut.
func (b *Buffer) Len() int { return b.buf.Len() + b.Dropped }

// Truncated reports whether the stream was longer than the bound.
func (b *Buffer) Truncated() bool { return b.Dropped > 0 }

// Bytes returns the kept bytes without any marker. The slice aliases the buffer and is valid only
// until the next Write; callers that parse a stream use it after checking Truncated.
func (b *Buffer) Bytes() []byte { return b.buf.Bytes() }

// String returns what was kept, saying in the text that the stream did not end there.
//
// The marker is part of the rendering rather than of the capture, so diagnostics that stop
// mid-sentence look cut instead of looking like a command that printed exactly this much.
func (b *Buffer) String() string {
	if b.Dropped == 0 {
		return b.buf.String()
	}
	return fmt.Sprintf("%s…(%d bytes dropped)", b.buf.String(), b.Dropped)
}
