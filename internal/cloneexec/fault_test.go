package cloneexec

import (
	"context"
	"os"
	"testing"
	"time"
)

// descendantLog is where a hanging Git double records the pid of the process it leaves behind.
//
// The background sleeper is the descendant every Git invocation here has: it inherits the double's
// standard output, exactly as a transport helper inherits Git's, and it is the process that holds the
// staged token in its own memory after the executor has given up. Recording the pid is what lets a
// test ask whether that process is gone rather than only whether the call returned; the answer is read
// in `descendant_unix_test.go`, which is where the platform's signal API lives.
func descendantLog(gitPath string) string { return gitPath + ".descendants" }

// hangingMirrorGitDouble writes a Git double that behaves like the ordinary one for every command
// except the fetch, which leaves a descendant holding its output.
//
// The descendant is the point. A deadline kills the process the executor started, but not the pipes
// that process handed to anything it spawned, so a wait for the fetch waits for whatever still holds
// the write end. `exec` in the fetch arm replaces the shell with the sleeper, and the background job
// inherits the output, which is exactly the shape `git fetch` has when it starts a helper.
func hangingMirrorGitDouble(t *testing.T, fixture *mirrorFixture) {
	t.Helper()
	script := `#!/bin/sh
mode=other
for argument in "$@"; do
  case "$argument" in
    init) mode=init ;;
    fetch) mode=fetch ;;
  esac
done
case "$mode" in
  init)
    target=
    for argument in "$@"; do target="$argument"; done
    mkdir -p "$target"
    ;;
  fetch)
    sleep 60 &
    echo $! >> ` + shellQuote(descendantLog(fixture.config.GitPath)) + `
    exec sleep 60
    ;;
esac
exit 0
`
	if err := os.WriteFile(fixture.config.GitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the hanging git double: %v", err)
	}
}

// TestAFetchThatNeverAnswersIsCutOffAndTheMaterialIsDestroyed is the C-side bound.
//
// The executor's timeout is not only a scheduling nicety here. The cleanup that destroys the staged
// credential material runs after the fetch returns, so a fetch that never returns is also a token
// that outlives the execution it was staged for. What this asserts is therefore both halves: the call
// comes back on its own, and the material is gone when it does.
func TestAFetchThatNeverAnswersIsCutOffAndTheMaterialIsDestroyed(t *testing.T) {
	fixture := newMirrorFixture(t, func(c *MirrorConfig) { c.Timeout = 250 * time.Millisecond })
	hangingMirrorGitDouble(t, fixture)

	started := time.Now()
	_, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil))
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a fetch that never answered was reported as a success")
	}
	// The double sleeps for a minute, so anything at or above that means the timeout did not bound
	// the wait; the bound below is well clear of it.
	if elapsed > 15*time.Second {
		t.Fatalf("Prepare took %s; the 250ms timeout did not bound the wait", elapsed)
	}
	if got := reasonCode(err); got != ReasonCodeFetchFailed {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, ReasonCodeFetchFailed, err)
	}
	if fixture.store.calls != 1 {
		t.Fatalf("redemptions = %d, want 1: the timeout happens after the spend", fixture.store.calls)
	}

	// The security half: the spend came back, so the material must not have come back with it.
	assertNoTokenUnder(t, fixture.config.MaterialRoot, testToken)
	entries, err := os.ReadDir(fixture.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root holds %d entries after a fetch that never answered", len(entries))
	}
	fetches, err := os.ReadDir(fixture.executor.fetchRoot)
	if err != nil {
		t.Fatalf("read the fetch root: %v", err)
	}
	if len(fetches) != 0 {
		t.Fatalf("the fetch root holds %d entries after a fetch that never answered", len(fetches))
	}
	if records, err := fixture.mirrors.Records(); err != nil {
		t.Fatalf("read the mirror records: %v", err)
	} else if len(records) != 0 {
		t.Fatalf("a fetch that never answered left %d mirror records", len(records))
	}
}

// hangingCloneGitDouble writes a Git double that hangs on the clone and answers everything else the
// way the ordinary double does. Like the mirror double's fetch arm, it leaves a descendant holding
// the output, which is the shape a real clone has once it has started its transport helper.
func hangingCloneGitDouble(t *testing.T, fixture *testFixture) {
	t.Helper()
	script := `#!/bin/sh
mode=other
for argument in "$@"; do
  case "$argument" in
    clone) mode=clone ;;
    rev-parse) mode=revparse ;;
  esac
done
if [ "$mode" = revparse ]; then
  printf '%s\n' ` + shellQuote(testCommit) + `
  exit 0
fi
sleep 60 &
echo $! >> ` + shellQuote(descendantLog(fixture.config.GitPath)) + `
exec sleep 60
`
	if err := os.WriteFile(fixture.config.GitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the hanging git double: %v", err)
	}
}

// TestACloneThatNeverAnswersIsCutOffAndTheMaterialIsDestroyed is the same bound on the clone path,
// which is the one the materialization obligation is written about.
//
// Both halves matter. A clone that outlives its deadline is a clone the caller cannot retry, and
// because the destroy is deferred it is also a token still on disk after the execution that staged
// it was supposed to have ended.
func TestACloneThatNeverAnswersIsCutOffAndTheMaterialIsDestroyed(t *testing.T) {
	fixture := newTestFixture(t, func(c *Config) { c.Timeout = 250 * time.Millisecond })
	hangingCloneGitDouble(t, fixture)

	started := time.Now()
	_, err := fixture.executor.Execute(context.Background(), fixture.issue(t, nil))
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a clone that never answered was reported as a success")
	}
	if elapsed > 15*time.Second {
		t.Fatalf("Execute took %s; the 250ms timeout did not bound the wait", elapsed)
	}
	if got := reasonCode(err); got != ReasonCodeCloneFailed {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, ReasonCodeCloneFailed, err)
	}
	if fixture.store.calls != 1 {
		t.Fatalf("redemptions = %d, want 1: the timeout happens after the spend", fixture.store.calls)
	}

	assertNoTokenUnder(t, fixture.config.MaterialRoot, testToken)
	entries, err := os.ReadDir(fixture.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root holds %d entries after a clone that never answered", len(entries))
	}
}
