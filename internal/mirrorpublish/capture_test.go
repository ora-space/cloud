package mirrorpublish

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// TestAFactPrintedPastTheBoundIsNotAFact is the reading half of the bound on what a fetch may write
// into this process: `git for-each-ref` is asked what refs the mirror exposes, and an answer that
// stopped at the bound is a prefix of the answer rather than the answer.
//
// The double answers every earlier question correctly, prints the one permitted ref here, and then
// floods standard output past the bound. The ref that was read is therefore inside the capture and
// the stream did not end there — which is the shape of the case that matters: an upstream that got a
// second ref into the mirror would have it recorded as the only ref, because the listing that would
// have shown the other one was cut.
func TestAFactPrintedPastTheBoundIsNotAFact(t *testing.T) {
	gitPath := filepath.Join(t.TempDir(), "git")
	// The values below are a hex commit and a ref name, so Go's quoting is also the shell's. A value
	// that were not would have to be quoted for the shell instead.
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
  revparse)
    printf '%s\n' ` + strconv.Quote(testCommitA) + `
    ;;
  refs)
    printf '%s\n' ` + strconv.Quote(MirrorRef) + `
    i=0
    while [ $i -lt 2048 ]; do printf '%064d' 0; i=$((i+1)); done
    ;;
  head)
    printf '%s\n' ` + strconv.Quote(MirrorRef) + `
    ;;
esac
exit 0
`
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	fetcher, err := NewGitFetcher(GitConfig{
		GitPath:      gitPath,
		Home:         t.TempDir(),
		HelperPath:   "/usr/local/bin/git-credential-ora",
		MaterialRoot: t.TempDir(),
		Timeout:      30 * time.Second,
	}, stubMaterial{})
	if err != nil {
		t.Fatalf("NewGitFetcher: %v", err)
	}
	reference := &Reference{
		Key:      mirror.Key{ProjectID: testProject, Repository: testRepoA, Commit: testCommitA},
		TenantID: testTenant,
		Upstream: "main",
	}
	_, err = fetcher.Fetch(context.Background(), reference, filepath.Join(t.TempDir(), "mirror"))
	if err == nil {
		t.Fatal("a fetch whose ref listing was cut short was reported as a success")
	}
	if caused := err.Error(); !strings.Contains(caused, "not a fact it can read") {
		t.Fatalf("the refusal does not report the cut stream: %v", err)
	}
}
