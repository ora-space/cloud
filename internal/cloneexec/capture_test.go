package cloneexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// The tests here are about what an executor keeps of what a Git invocation printed, which is a bound
// on this process's memory that a repository gets to push against: `git fetch` forwards whatever a
// remote sends over the sideband, and `git ls-remote` prints whatever a server advertises.

// TestAStreamPastTheBoundIsDroppedRatherThanEndingTheCommand is the memory half, and the half that
// makes the bound safe to have.
//
// A bound on a child's output is easy to get wrong in a way that turns it into a failure the child
// suffers: `exec` copies the pipe into the writer it was handed, `io.Copy` treats a short write as an
// error, and a copy that ends early closes the pipe — which is a Git killed by SIGPIPE for printing
// too much. So the assertion is both that the streams stop at the bound and that a command which
// printed far past it is still reported as having succeeded.
func TestAStreamPastTheBoundIsDroppedRatherThanEndingTheCommand(t *testing.T) {
	// A shell rather than a Git: what is under test is how this package consumes a child's output, and
	// the real binary is the acceptance test's business.
	script := `i=0
while [ $i -lt 2048 ]; do
  printf '%064d' 0
  printf '%064d' 0 >&2
  i=$((i+1))
done
exit 0
`
	written := 2048 * 64
	output, err := runGit(context.Background(), "/bin/sh", t.TempDir(), []string{"-c", script}, t.TempDir())
	if err != nil {
		t.Fatalf("a command that printed %d bytes per stream was reported as failed: %v", written, err)
	}
	if !output.stdoutCut {
		t.Fatalf("a command that printed %d bytes per stream does not report the cut on standard output", written)
	}
	// The marker is part of `String()`, so the kept text is the bound plus a short tail; anything close
	// to what the command wrote would mean the bound is not one. Standard error carries no flag of its
	// own — nothing reads it as a fact — so its truncation is what the text says.
	for name, kept := range map[string]string{"stdout": output.stdout, "stderr": output.stderr} {
		if len(kept) > captureLimit+64 {
			t.Fatalf("%s kept %d bytes of %d written: the capture is not bounded", name, len(kept), written)
		}
	}
	if !strings.Contains(output.stderr, "bytes dropped") {
		t.Fatalf("the kept standard error does not say that it stopped early: %d bytes kept", len(output.stderr))
	}
	if !strings.HasPrefix(output.stdout, strings.Repeat("0", captureLimit)) {
		t.Fatalf("stdout kept something other than the first %d bytes the command printed", captureLimit)
	}
}

// TestAnErrorStreamInsideTheBoundIsRedacted is the credential rule in its ordinary shape: the remote
// echoed the secret into a message Git printed, and what the executor logs does not carry it.
func TestAnErrorStreamInsideTheBoundIsRedacted(t *testing.T) {
	echo := "fatal: Authentication failed for " + testToken
	script := fmt.Sprintf("printf '%%s' %s >&2\nexit 128\n", shellQuote(echo))
	output, err := runGit(context.Background(), "/bin/sh", t.TempDir(), []string{"-c", script}, t.TempDir())
	if err == nil {
		t.Fatal("a command that exited 128 was reported as having succeeded")
	}
	detail := redactFailure(output.stderr, testToken)
	if strings.Contains(detail.Error(), testToken) {
		t.Fatalf("the credential reached the failure this executor logs: %v", detail)
	}
	if !strings.Contains(detail.Error(), "[redacted]") {
		t.Fatalf("the credential was removed without a trace, so this is not redaction: %v", detail)
	}
}

// TestACredentialEchoWhereTheLogWindowEndsIsStillRedacted is the rule at the other bound: the secret
// is printed where the log's own 4096-byte cut falls, so a redaction applied after that cut would
// keep the credential's first bytes and drop only the rest of it.
//
// The order is redact-then-cut for exactly this reason, and the capture bound is what is below the
// window rather than above it: the remote wrote ~4 KiB here and this executor kept all of it, so the
// replacement saw the whole secret even though the log will not.
func TestACredentialEchoWhereTheLogWindowEndsIsStillRedacted(t *testing.T) {
	// 4090 bytes of filler put the echo across the 4096th byte of the detail.
	const filler = 4090
	script := fmt.Sprintf(`i=0
while [ $i -lt %d ]; do
  printf 'p' >&2
  i=$((i+1))
done
printf '%%s' %s >&2
exit 128
`, filler, shellQuote(testToken))
	output, err := runGit(context.Background(), "/bin/sh", t.TempDir(), []string{"-c", script}, t.TempDir())
	if err == nil {
		t.Fatal("a command that exited 128 was reported as having succeeded")
	}
	if !strings.Contains(output.stderr, testToken) {
		// Without this the case would be about a capture that dropped the credential rather than about
		// redaction seeing it, and would pass for the wrong reason.
		t.Fatalf("the capture does not hold the credential, so this is not the case it claims to be")
	}
	detail := redactFailure(output.stderr, testToken).Error()
	if strings.Contains(detail, testToken) || strings.Contains(detail, testToken[:10]) {
		t.Fatalf("the log kept part of the credential: %q", detail)
	}
	// What the window kept after the filler is the beginning of the replacement rather than the
	// beginning of the credential, which is the order this case exists for: the secret was replaced
	// before the window was applied and not after it.
	prefix := "git reported: " + strings.Repeat("p", filler)
	if !strings.HasPrefix(detail, prefix) {
		t.Fatalf("the detail does not begin with what the remote printed before the echo: %q", detail)
	}
	tail := detail[len(prefix):]
	if !strings.HasSuffix(tail, "…") {
		t.Fatalf("the detail was not cut at the log's window: %q", tail)
	}
	if body := strings.TrimSuffix(tail, "…"); !strings.HasPrefix("[redacted]", body) {
		t.Fatalf("the log kept %q where the credential was printed", tail)
	}
	if len(detail) > 4096+len("git reported: ")+8 {
		t.Fatalf("the failure detail is %d bytes, so it is not bounded", len(detail))
	}
}

// overflowingMirrorGitDouble writes a Git double whose read back prints the commit and then far more
// than the executor keeps.
//
// It is the shape a hostile upstream has: the value being read is the first thing printed, and what
// follows is there to move the capture bound into the middle of the stream.
func overflowingMirrorGitDouble(t *testing.T, fixture *mirrorFixture) {
	t.Helper()
	script := `#!/bin/sh
mode=other
for argument in "$@"; do
  case "$argument" in
    init) mode=init ;;
    fetch) mode=fetch ;;
    rev-parse) mode=revparse ;;
    for-each-ref) mode=refs ;;
    symbolic-ref) mode=head ;;
  esac
done
case "$mode" in
  init)
    target=
    for argument in "$@"; do target="$argument"; done
    mkdir -p "$target"
    ;;
  fetch)
    printf '%s' ` + shellQuote(testMirrorContent) + ` > fetched.txt
    ;;
  revparse)
    printf '%s\n' ` + shellQuote(testCommit) + `
    i=0
    while [ $i -lt 2048 ]; do printf '%064d' 0; i=$((i+1)); done
    ;;
  refs)
    printf '%s\n' ` + shellQuote(mirrorpublish.MirrorRef) + `
    ;;
esac
exit 0
`
	if err := os.WriteFile(fixture.config.GitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the overflowing git double: %v", err)
	}
}

// TestAFactPrintedPastTheBoundIsNotAFact is the other use of the bound: a stream that was cut is a
// prefix, and a prefix is not what a command stated.
//
// The double prints a perfectly good commit and then floods, so the value is inside the capture and
// the stream did not end there. Reading it anyway would be recording a fact Git never finished
// printing — and would say nothing about what a hostile upstream could put after it.
func TestAFactPrintedPastTheBoundIsNotAFact(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	overflowingMirrorGitDouble(t, fixture)

	if _, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil)); err == nil {
		t.Fatal("a preparation whose read back was cut short was reported as a success")
	} else {
		if got := reasonCode(err); got != ReasonCodeFetchFailed {
			t.Fatalf("refusal code = %q, want %q (error: %v)", got, ReasonCodeFetchFailed, err)
		}
		if detail := failureText(err); !strings.Contains(detail, "not a fact it can read") {
			t.Fatalf("the refusal does not report the cut stream: %s", detail)
		}
	}
	// A read that could not be completed publishes nothing, and the fetch that produced it is not a
	// mirror any reader may be handed.
	records, err := fixture.mirrors.Records()
	if err != nil {
		t.Fatalf("read the mirror records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("a preparation whose read back was cut short left %d mirror records", len(records))
	}
}

// failureText is the whole cause chain of a refusal, which is where this executor's own diagnostics
// live: `Failure.Error` reports the reason code and deliberately not the cause.
func failureText(err error) string {
	var text strings.Builder
	for err != nil {
		text.WriteString(err.Error())
		text.WriteString(" | ")
		err = errors.Unwrap(err)
	}
	return text.String()
}
