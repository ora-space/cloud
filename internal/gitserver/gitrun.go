package gitserver

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/processgroup"
)

// stderrLimit bounds how much of git's standard error is kept for the log. The limit exists because
// the stream is a child process's diagnostics, and an unbounded buffer fed by a child is a memory
// budget handed to whatever the child decides to print.
const stderrLimit = 8 << 10

// reapDelay bounds how long Wait may spend on I/O after the process it waited for is gone.
//
// The process exiting does not close its pipes if a descendant inherited them, and Wait waits for
// the pipes. Killing git is therefore not enough on its own, and this is the backstop that keeps a
// descendant that outlives its parent from holding the handler open.
const reapDelay = 5 * time.Second

// gitStream is one running upload-pack: its stdout, a bounded copy of its stderr, and the process.
type gitStream struct {
	*bufio.Reader
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *boundedBuffer
	// release stops the context callback that ends the read. It is called by Close so a stream that
	// finished on its own does not fire it afterwards.
	release func() bool
}

// Close kills and reaps the process. It is safe to call on a stream that already finished.
func (g *gitStream) Close() error {
	if g.release != nil {
		g.release()
	}
	_ = g.stdout.Close()
	if g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
	}
	err := g.cmd.Wait()
	if err != nil && g.stderr.Len() > 0 {
		return fmt.Errorf("%w: stderr: %s", err, strings.TrimSpace(g.stderr.String()))
	}
	return err
}

// runGit runs one bounded upload-pack invocation and streams its stdout to out.
func (m *mirrorRead) runGit(ctx context.Context, repoDir, protocol string, out io.Writer, args ...string) error {
	stream, err := m.startGit(ctx, repoDir, protocol, nil, args...)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, stream)
	// Close reaps the process, so it runs before the outcome is decided: a nonzero exit is only
	// visible once git has been waited for.
	closeErr := stream.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// startGit starts upload-pack against a directory the store produced.
//
// Two things about the invocation matter more than the arguments. The repository path comes from the
// mirror store — never from the request — so no request can steer the read at another directory. And
// the environment is constructed rather than inherited, because an inherited environment is how a
// caller's request ends up influencing a child process through a variable nobody meant to expose.
func (m *mirrorRead) startGit(ctx context.Context, repoDir, protocol string, stdin io.Reader, args ...string) (*gitStream, error) {
	argv := append([]string{"upload-pack"}, args...)
	argv = append(argv, repoDir)
	//nolint:gosec // G204: the binary is the service's resolved git program and every argument is
	// built from a digest the mirror store validated plus fixed service names; no request text
	// reaches the argv.
	cmd := exec.CommandContext(ctx, m.git, argv...)
	cmd.Dir = m.home
	cmd.Env = m.gitEnv(protocol)
	cmd.Stdin = stdin
	stderr := &boundedBuffer{limit: stderrLimit}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("connect to git's stdout: %w", err)
	}
	// The delay is set before Start because exec reads it when it builds the reaping path.
	cmd.WaitDelay = reapDelay
	// The deadline ends the process *tree*. `upload-pack` starts a packer, so killing the process
	// started here leaves the process actually doing the work running with this handler's output,
	// and the close below only ends the read.
	processgroup.Isolate(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git: %w", err)
	}
	// Ending the deadline has to end the read, not only the process. CommandContext kills git, but a
	// descendant that inherited stdout keeps the write end open, so a reader that waits for EOF waits
	// for that descendant — which is how a 250ms bound turns into a minute-long request. Closing the
	// read end is what makes the bound real: the copy fails immediately instead of waiting.
	release := context.AfterFunc(ctx, func() { _ = stdout.Close() })
	return &gitStream{
		Reader: bufio.NewReaderSize(stdout, 32<<10), cmd: cmd, stdout: stdout, stderr: stderr,
		release: release,
	}, nil
}

// gitEnv is the whole environment a git child process gets.
//
// GIT_OPTIONAL_LOCKS=0 is not a performance choice: it tells git not to take its optional write
// locks, so a read can never touch the published tree. A mirror that a read could modify would not
// be immutable, and immutability is what lets an authorization bind a commit.
func (m *mirrorRead) gitEnv(protocol string) []string {
	env := []string{
		"PATH=" + m.cfg.Path,
		"HOME=" + m.home,
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if protocol != "" {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	return env
}

// gitProtocol accepts only the two protocol versions git defines. The header is written by the
// client and ends up in a child process's environment, so it is matched against a closed set rather
// than forwarded: a caller does not get to choose what the server's git process is told.
func gitProtocol(header string) string {
	switch strings.TrimSpace(header) {
	case "version=1":
		return "version=1"
	case "version=2":
		return "version=2"
	default:
		return ""
	}
}

// boundedBuffer keeps at most limit bytes and counts the rest, so a child that writes without end
// cannot grow the parent's memory. It reports the truncation rather than hiding it: diagnostics that
// stop mid-sentence should look like diagnostics that stop mid-sentence.
type boundedBuffer struct {
	limit   int
	dropped int
	buf     bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.dropped += len(p)
		return len(p), nil
	}
	if len(p) > remaining {
		b.dropped += len(p) - remaining
		p = p[:remaining]
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) Len() int { return b.buf.Len() + b.dropped }

func (b *boundedBuffer) String() string {
	if b.dropped == 0 {
		return b.buf.String()
	}
	return fmt.Sprintf("%s…(%d bytes dropped)", b.buf.String(), b.dropped)
}
