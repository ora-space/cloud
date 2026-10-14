package gitserver

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// fakeGit writes an executable script standing in for the upload-pack binary and returns its path.
//
// GitPath is the whole fault surface this server owns: upload-pack is its only child process, and
// which binary that is comes from deployment configuration rather than from a request. Injecting a
// git that is absent, that hangs, or that dies partway therefore needs no seam inside the server —
// the deployment setting is the seam, and this is what makes it a real one.
func fakeGit(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	return path
}

// realGitPath is the absolute path of the git the scripts below delegate to. The child is started
// with a constructed PATH, so a script that execs a bare "git" would depend on there being one there.
func realGitPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the test needs a real git to delegate to: %v", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return absolute
}

// authorizedFixture publishes one real mirror and makes Cloud's answer name it, so every request
// below reaches the git process instead of stopping at a refusal. It returns the mirror's key as
// well, because a test that recovers goes on to clone it.
func authorizedFixture(t *testing.T, gitPath string, timeout time.Duration) (*fixture, string, mirror.Key) {
	t.Helper()
	f := newFixtureWithGit(t, gitPath, timeout)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))
	return f, "/mirror/v1/" + key.Digest() + ".git", key
}

// TestAGitThatCannotStartIsUnavailableAndNeverARefusal pins the distinction the whole read path
// rests on: a git the server cannot run is an infrastructure fault, so it must answer 500 and must
// never answer 403. A 403 would tell an authorized caller its authorization was wrong and would stop
// it retrying, which is the opposite of what a broken deployment needs.
func TestAGitThatCannotStartIsUnavailableAndNeverARefusal(t *testing.T) {
	// A path inside a fresh temp directory: nothing was ever written there.
	missing := filepath.Join(t.TempDir(), "git")
	f, base, _ := authorizedFixture(t, missing, 0)

	response := f.post(t, base+"/git-upload-pack", nil, nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST with an unstartable git: status = %d, want 500 (body %q)", response.StatusCode, body)
	}
	if strings.Contains(string(body), "forbidden") {
		t.Fatalf("an unstartable git was reported as a refusal: %q", body)
	}

	// The advertisement writes its status before it runs git, so the preamble arrives and the ref
	// advertisement does not. The caller sees a truncated protocol stream, never an allow.
	response = f.get(t, base+"/info/refs?service=git-upload-pack", nil)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET with an unstartable git: status = %d, want 200", response.StatusCode)
	}
	if want := pktLine("# service=git-upload-pack\n") + "0000"; string(body) != want {
		t.Fatalf("advertisement body = %q, want just the preamble %q", body, want)
	}
}

// TestAGitThatHangsIsCutOffByTheTimeout pins that Timeout actually bounds the handler. Without it a
// single unresponsive child holds a connection, a goroutine and a request slot for as long as it
// likes, and the sandbox's clone never learns why it is waiting.
func TestAGitThatHangsIsCutOffByTheTimeout(t *testing.T) {
	// `exec` matters: it replaces the shell, so the process the server started is the process that
	// sleeps. A script that left a grandchild holding stdout would be testing a different fault.
	gitPath := fakeGit(t, "#!/bin/sh\nexec sleep 60\n")
	f, base, _ := authorizedFixture(t, gitPath, 250*time.Millisecond)

	cases := map[string]func() *http.Response{
		"negotiation": func() *http.Response { return f.post(t, base+"/git-upload-pack", nil, nil) },
		"advertisement": func() *http.Response {
			return f.get(t, base+"/info/refs?service=git-upload-pack", nil)
		},
	}
	for name, request := range cases {
		started := time.Now()
		response := request()
		elapsed := time.Since(started)
		response.Body.Close()
		// The bound is the assertion, so it is checked against a value far below the 60 seconds the
		// child asked for: anything that let the child finish would take at least that long.
		if elapsed > 10*time.Second {
			t.Fatalf("%s: the request took %s; the 250ms timeout did not end it", name, elapsed)
		}
		// The negotiation has written no status yet, so the fault still becomes a 500. The
		// advertisement has already sent 200; what matters there is only that it returns.
		if name == "negotiation" && response.StatusCode != http.StatusInternalServerError {
			t.Fatalf("%s: status = %d, want 500", name, response.StatusCode)
		}
	}
}

// TestAGitThatDiesWithoutAnsweringIsUnavailable covers the child that fails immediately rather than
// hanging: the first byte is read before any header is written precisely so this becomes a 500 the
// client can act on, instead of a 200 carrying an empty protocol stream.
func TestAGitThatDiesWithoutAnsweringIsUnavailable(t *testing.T) {
	gitPath := fakeGit(t, "#!/bin/sh\nexit 1\n")
	f, base, _ := authorizedFixture(t, gitPath, 0)

	response := f.post(t, base+"/git-upload-pack", nil, nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", response.StatusCode, body)
	}
	if strings.Contains(string(body), "forbidden") {
		t.Fatalf("a git that died was reported as a refusal: %q", body)
	}
}

// TestAChildFailureIsNotStickyAndTheNextReadIsServed is the recovery half: the injected git fails its
// first invocation and delegates to the real one after. A failure that outlived its request — a
// poisoned cache, a wedged mutex, a server that stopped trying — would show up here as the second
// read failing too.
func TestAChildFailureIsNotStickyAndTheNextReadIsServed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "failed-once")
	gitPath := fakeGit(t, "#!/bin/sh\nif [ ! -f '"+marker+"' ]; then\n  : > '"+marker+"'\n  exit 1\nfi\nexec "+realGitPath(t)+" \"$@\"\n")
	f, base, key := authorizedFixture(t, gitPath, 0)

	first := f.post(t, base+"/git-upload-pack", nil, nil)
	first.Body.Close()
	if first.StatusCode != http.StatusInternalServerError {
		t.Fatalf("the injected failure answered %d, want 500", first.StatusCode)
	}

	// The second read runs the real git against the same published mirror, so a successful clone is
	// the whole assertion: the content was still there and the server still served it.
	response := f.get(t, base+"/info/refs?service=git-upload-pack", nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the read after the failure answered %d, want 200", response.StatusCode)
	}
	// The ref name is the mirror's own, not the upstream's: the advertisement names what the store
	// published, which is the point of serving content by digest rather than by upstream identity.
	if !strings.Contains(string(body), "refs/heads/") {
		t.Fatalf("the read after the failure carried no ref advertisement: %q", body)
	}

	destination := filepath.Join(t.TempDir(), "clone")
	if out, err := gitCommand(t, "", "clone", "--quiet", f.mirrorURL(key), destination); err != nil {
		t.Fatalf("the clone after the failure failed: %v\n%s", err, out)
	}
}

// TestTheGitChildGetsNoPartOfTheServersEnvironment pins the constructed environment: a child process
// that inherits its parent's environment is how a deployment variable nobody meant to expose reaches
// a program that then logs it. The canary is set on the test process, so finding it in the child's
// output would mean the environment was inherited rather than built.
func TestTheGitChildGetsNoPartOfTheServersEnvironment(t *testing.T) {
	const canary = "ORA_TEST_CHILD_ENV_CANARY"
	t.Setenv(canary, "must-not-reach-the-child")

	// `env` dumps the whole environment to stdout, which is the response body.
	gitPath := fakeGit(t, "#!/bin/sh\nenv\n")
	f, base, _ := authorizedFixture(t, gitPath, 0)

	response := f.post(t, base+"/git-upload-pack", nil, nil)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", response.StatusCode, body)
	}
	if strings.Contains(string(body), "must-not-reach-the-child") {
		t.Fatalf("the child inherited the server's environment:\n%s", body)
	}
	// The dump has to be the real one, or the absence above would prove nothing.
	if !strings.Contains(string(body), "GIT_OPTIONAL_LOCKS=0") {
		t.Fatalf("the child's environment is not the constructed one:\n%s", body)
	}
}

// TestAGitThatLeavesADescendantHoldingItsOutputIsStillCutOffByTheTimeout covers the fault the
// timeout above cannot reach on its own. Killing the process the server started does not close the
// pipe: a descendant that inherited stdout keeps the write end open, so a read that waits for EOF
// waits for the descendant. The bound has to end the read, not only the process.
func TestAGitThatLeavesADescendantHoldingItsOutputIsStillCutOffByTheTimeout(t *testing.T) {
	// The background job inherits stdout and outlives the shell, which `exec` then replaces.
	gitPath := fakeGit(t, "#!/bin/sh\nsleep 60 &\nexec sleep 60\n")
	f, base, _ := authorizedFixture(t, gitPath, 250*time.Millisecond)

	started := time.Now()
	response := f.post(t, base+"/git-upload-pack", nil, nil)
	elapsed := time.Since(started)
	response.Body.Close()
	if elapsed > 10*time.Second {
		t.Fatalf("the request took %s; the 250ms timeout did not end the read", elapsed)
	}
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
}
