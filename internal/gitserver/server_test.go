package gitserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

const (
	testTenant     = "tenant-1"
	testRepository = "https://github.com/ora-space/demo"
	mirrorRef      = "refs/heads/ora-mirror"
)

// runGit runs a real git command, because the only useful evidence about a Git server is Git
// speaking to it.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=2026-10-13T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-13T00:00:00Z",
		"GIT_TERMINAL_PROMPT=0",
		// The loopback clone must not be routed through a proxy: a proxy in the path would make the
		// caller look like the proxy, which is exactly what the server refuses.
		"NO_PROXY=*", "no_proxy=*", "http_proxy=", "https_proxy=", "all_proxy=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// sourceRepository builds a real repository with one commit and returns its directory and commit.
func sourceRepository(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	return dir, runGit(t, dir, "rev-parse", "HEAD")
}

func newMirrorStore(t *testing.T) *mirror.Store {
	t.Helper()
	store, err := mirror.New(mirror.Config{
		Root:           t.TempDir(),
		MaxMirrorBytes: 64 << 20,
		MaxVolumeBytes: 256 << 20,
		Grace:          time.Hour,
	})
	if err != nil {
		t.Fatalf("mirror.New: %v", err)
	}
	return store
}

// publishMirror materializes a source repository as a mirror with exactly one ref and a HEAD that
// points at it, verifies that with git, and publishes it. The verification is the writer's
// obligation, reproduced here so the fixture cannot publish something the writer would have to
// reject.
func publishMirror(t *testing.T, store *mirror.Store, key mirror.Key, sourceDir string) mirror.Record {
	t.Helper()
	ctx := context.Background()
	prep, err := store.Begin(ctx, key)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer prep.Close()
	runGit(t, "", "init", "-q", "--bare", prep.Dir())
	runGit(t, "", "-C", prep.Dir(), "symbolic-ref", "HEAD", mirrorRef)
	runGit(t, "", "-C", prep.Dir(), "fetch", "--quiet", sourceDir, "+refs/heads/main:"+mirrorRef)
	if commit := runGit(t, "", "-C", prep.Dir(), "rev-parse", mirrorRef); commit != key.Commit {
		t.Fatalf("the fetched mirror resolves %s but the key pins %s", commit, key.Commit)
	}
	refs := runGit(t, "", "-C", prep.Dir(), "for-each-ref", "--format=%(refname)")
	if refs != mirrorRef {
		t.Fatalf("the mirror exposes %q, want exactly %q", refs, mirrorRef)
	}
	record, err := prep.Publish(ctx, mirror.PublishVerification{Commit: key.Commit, Ref: mirrorRef})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return record
}

// stubResolver is an origin resolver whose answer the test chooses. The real one asks
// sandbox-server, which asks the container engine; what matters here is the server's behavior for
// each of the three outcomes it can produce.
//
// It is synchronized because the test goroutine changes the answer while handler goroutines read it,
// which is the same reason the real resolver may not cache: the answer belongs to the request.
type stubResolver struct {
	mu     sync.Mutex
	origin gitread.Origin
	err    error
	peers  []netip.Addr
}

func (s *stubResolver) Resolve(_ context.Context, peer netip.Addr) (gitread.Origin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peers = append(s.peers, peer)
	if s.err != nil {
		return gitread.Origin{}, s.err
	}
	return s.origin, nil
}

func (s *stubResolver) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stubResolver) seenPeers() []netip.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]netip.Addr(nil), s.peers...)
}

// stubState is Cloud's authoritative state, as the test dictates it for one request.
type stubState struct {
	mu       sync.Mutex
	snapshot gitread.Snapshot
	err      error
	requests []string
}

func (s *stubState) Read(_ context.Context, _ gitread.Origin, digest string) (gitread.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, digest)
	if s.err != nil {
		return gitread.Snapshot{}, s.err
	}
	return s.snapshot, nil
}

func (s *stubState) set(snapshot gitread.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot = snapshot
}

func (s *stubState) mutate(f func(*gitread.Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.snapshot)
}

func (s *stubState) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func testOrigin() gitread.Origin {
	return gitread.Origin{
		WorkspaceID:   "workspace-1",
		ContainerID:   "container-1",
		ContainerName: "ora-workspace-1",
	}
}

func testKey(commit string) mirror.Key {
	return mirror.Key{ProjectID: "project-1", Repository: testRepository, Commit: commit}
}

type fixture struct {
	store    *mirror.Store
	server   *Server
	http     *httptest.Server
	resolver *stubResolver
	state    *stubState
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := newMirrorStore(t)
	resolver := &stubResolver{origin: testOrigin()}
	state := &stubState{}
	server, err := New(Config{Mirrors: store, Origins: resolver, State: state})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return &fixture{store: store, server: server, http: httpServer, resolver: resolver, state: state}
}

// authorize makes Cloud's answer match a published mirror for this fixture.
func (f *fixture) authorize(record mirror.Record) {
	origin := testOrigin()
	f.state.set(gitread.Snapshot{
		Found:              true,
		Key:                record.Key,
		MirrorGeneration:   record.Generation,
		MirrorReady:        true,
		ContainerLive:      true,
		WorkspaceID:        origin.WorkspaceID,
		ContainerID:        origin.ContainerID,
		RuntimeGeneration:  3,
		WorkspaceProjectID: record.Key.ProjectID,
		WorkspaceTenantID:  testTenant,
		ProjectTenantID:    testTenant,
	})
}

func (f *fixture) mirrorURL(key mirror.Key) string {
	return f.http.URL + "/mirror/v1/" + key.Digest() + ".git"
}

func TestCloneOverSmartHTTPSucceeds(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{
		"README.md":   "# demo\n",
		"src/main.go": "package main\n",
	})
	key := testKey(commit)
	record := publishMirror(t, f.store, key, source)
	f.authorize(record)

	dest := filepath.Join(t.TempDir(), "clone")
	out, err := gitCommand(t, "", "clone", "--quiet", f.mirrorURL(key), dest)
	if err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dest, "README.md"))
	if err != nil {
		t.Fatalf("read cloned file: %v", err)
	}
	if string(body) != "# demo\n" {
		t.Fatalf("cloned content = %q", body)
	}
	if got := runGit(t, dest, "rev-parse", "HEAD"); got != commit {
		t.Fatalf("cloned HEAD = %s, want %s", got, commit)
	}
	// A second clone proves the read is stateless: nothing about the first one was remembered.
	second := filepath.Join(t.TempDir(), "clone-2")
	if out, err := gitCommand(t, "", "clone", "--quiet", f.mirrorURL(key), second); err != nil {
		t.Fatalf("second clone failed: %v\n%s", err, out)
	}
}

func TestCloneWithProtocolV2Succeeds(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))

	dest := filepath.Join(t.TempDir(), "clone-v2")
	out, err := gitCommand(t, "", "-c", "protocol.version=2", "clone", "--quiet", f.mirrorURL(key), dest)
	if err != nil {
		t.Fatalf("protocol v2 clone failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("protocol v2 clone produced no content: %v", err)
	}
}

func TestWritesAreRefused(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))

	// A real push: git asks for the receive-pack advertisement, which is refused before anything else.
	work := filepath.Join(t.TempDir(), "work")
	if out, err := gitCommand(t, "", "clone", "--quiet", f.mirrorURL(key), work); err != nil {
		t.Fatalf("clone for the push attempt failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(work, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "attempted write")
	if out, err := gitCommand(t, work, "push", f.mirrorURL(key), "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("a push succeeded against the read-only server:\n%s", out)
	}

	// Each write-shaped service, asked for directly, is refused with the same response as any other
	// refusal.
	for _, service := range []string{"git-receive-pack", "git-upload-archive", "", "git-upload-pack-extra"} {
		response := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service="+service, nil)
		defer response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("service %q answered %d, want 403", service, response.StatusCode)
		}
	}
	// And the write endpoint is never mounted.
	response := f.post(t, "/mirror/v1/"+key.Digest()+".git/git-receive-pack", nil, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("git-receive-pack endpoint answered %d, want 404", response.StatusCode)
	}
}

func TestEveryRefusalIsTheSameResponse(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	record := publishMirror(t, f.store, key, source)
	f.authorize(record)
	path := "/mirror/v1/" + key.Digest() + ".git/info/refs?service=git-upload-pack"

	scenarios := map[string]func(){
		"baseline, refused by origin": func() { f.resolver.setErr(ErrOriginUnresolvable) },
		"unknown digest":              func() { path = "/mirror/v1/" + strings.Repeat("0", 64) + ".git/info/refs?service=git-upload-pack" },
		"cloud knows no mirror":       func() { f.state.set(gitread.Snapshot{}) },
		"cross tenant":                func() { f.state.mutate(func(s *gitread.Snapshot) { s.ProjectTenantID = "tenant-2" }) },
		"cross workspace":             func() { f.state.mutate(func(s *gitread.Snapshot) { s.WorkspaceID = "workspace-2" }) },
		"an earlier generation":       func() { f.state.mutate(func(s *gitread.Snapshot) { s.RuntimeGeneration = 2; s.ContainerLive = false }) },
		"container no longer live":    func() { f.state.mutate(func(s *gitread.Snapshot) { s.ContainerLive = false }) },
		"another project's mirror":    func() { f.state.mutate(func(s *gitread.Snapshot) { s.WorkspaceProjectID = "project-2" }) },
		"mirror not ready":            func() { f.state.mutate(func(s *gitread.Snapshot) { s.MirrorReady = false }) },
		"mirror withdrawn":            func() { f.state.mutate(func(s *gitread.Snapshot) { s.MirrorRevoked = true }) },
	}

	type observed struct {
		status int
		body   string
	}
	first := observed{}
	for name, arrange := range scenarios {
		// Each scenario starts from the authorized baseline.
		f.resolver.setErr(nil)
		f.authorize(record)
		path = "/mirror/v1/" + key.Digest() + ".git/info/refs?service=git-upload-pack"
		arrange()

		response := f.get(t, path, nil)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatalf("%s: read body: %v", name, err)
		}
		got := observed{status: response.StatusCode, body: string(body)}
		if got.status != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403", name, got.status)
		}
		if first.status == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("%s: response %+v differs from %+v; a caller could tell refusals apart", name, got, first)
		}
	}
	if first.body == "" {
		t.Fatal("no refusal response was observed")
	}
}

func TestUnavailabilityIsNotARefusal(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))
	path := "/mirror/v1/" + key.Digest() + ".git/info/refs?service=git-upload-pack"

	cases := map[string]func(){
		"origin resolver unavailable": func() { f.resolver.setErr(ErrOriginUnavailable) },
		"cloud state unreadable":      func() { f.state.setErr(errors.New("database is gone")) },
	}
	for name, arrange := range cases {
		f.resolver.setErr(nil)
		f.state.setErr(nil)
		arrange()
		response := f.get(t, path, nil)
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, response.StatusCode)
		}
		if strings.Contains(string(body), "forbidden") {
			t.Fatalf("%s: an undecidable request was reported as a refusal", name)
		}
	}
}

func TestAResolvedOriginIsTheOnlyIdentityConsidered(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	record := publishMirror(t, f.store, key, source)
	f.authorize(record)

	// A request that claims to be someone else, in every place a request can claim anything, is still
	// decided by the resolved origin. This caller *is* authorized, so the claims must change nothing.
	claims := http.Header{
		"Ora-Execution-Id": {"someone-elses-execution"},
		"X-Workspace-Id":   {"workspace-2"},
		"X-Forwarded-For":  {"10.0.0.1"},
		"X-Real-Ip":        {"10.0.0.1"},
		"X-Ora-Tenant-Id":  {"tenant-2"},
	}
	response := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack&workspaceId=workspace-2&tenantId=tenant-2&containerId=container-9", claims)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("self-asserted claims changed the decision: status = %d, want 200", response.StatusCode)
	}

	// The same claims cannot rescue a caller Cloud does not bind to the mirror.
	f.state.mutate(func(s *gitread.Snapshot) { s.WorkspaceID = "workspace-2" })
	response = f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack&workspaceId=workspace-2", claims)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a self-asserted workspace was accepted: status = %d, want 403", response.StatusCode)
	}
}

func TestAnAuthorizationForAnotherMirrorDoesNotReadThisOne(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	other, otherCommit := sourceRepository(t, map[string]string{"README.md": "# other\n"})
	key := testKey(commit)
	otherKey := testKey(otherCommit)
	f.authorize(publishMirror(t, f.store, key, source))
	otherRecord := publishMirror(t, f.store, otherKey, other)

	// Cloud is asked about the digest the caller requested; the answer names a different key, which
	// can only happen if Cloud's reader is wrong — and it must refuse rather than serve.
	f.state.set(gitread.Snapshot{
		Found: true, Key: otherRecord.Key, MirrorGeneration: otherRecord.Generation,
		MirrorReady: true, ContainerLive: true,
		WorkspaceID: testOrigin().WorkspaceID, ContainerID: testOrigin().ContainerID,
		RuntimeGeneration: 3, WorkspaceProjectID: otherRecord.Key.ProjectID,
		WorkspaceTenantID: testTenant, ProjectTenantID: testTenant,
	})
	response := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a digest answered with another key was served: status = %d", response.StatusCode)
	}
}

func TestTheStoreMustAgreeWithTheAuthorization(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	record := publishMirror(t, f.store, key, source)
	f.authorize(record)

	// Withdraw the mirror behind Cloud's back: the store is the one holding the bytes, so the read is
	// refused even though Cloud still says yes.
	if _, err := f.store.Revoke(key, "withdrawn during the test"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	response := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a mirror the store withdrew was served: status = %d", response.StatusCode)
	}
}

func TestAMirrorRepublishedAfterAuthorizationIsNotServed(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	record := publishMirror(t, f.store, key, source)
	f.authorize(record)

	// The mirror moves to a new generation while the authorization still names the old one.
	if _, err := f.store.Destroy(key); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	republished := publishMirror(t, f.store, key, source)
	if republished.Generation == record.Generation {
		t.Fatalf("the republished generation did not move: %d", republished.Generation)
	}
	response := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("content at a generation nobody authorized was served: status = %d", response.StatusCode)
	}
}

// TestServingAMirrorHasNoCredentialToLeak is the evidence that the read path is credential-free.
//
// The Git Server serves a commit the credentialed executor already fetched, so it must never hold,
// inherit or hand out an upstream credential. Two things are checked: the environment the child git
// process is given is a closed set built from scratch — nothing of the server's own environment
// survives into it, which is what stops a token that reached the server's process from reaching the
// reader — and a client clones anonymously, with no credential helper and no Authorization header,
// because the authorization this endpoint applies is the caller's source identity and not a secret.
func TestServingAMirrorHasNoCredentialToLeak(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))

	const planted = "ghp_thisMustNeverReachTheReader"
	t.Setenv("GITHUB_TOKEN", planted)
	t.Setenv("ORA_UPSTREAM_CREDENTIAL", planted)
	t.Setenv("http_proxy", "http://user:"+planted+"@127.0.0.1:1")

	expected := map[string]bool{
		"PATH": true, "HOME": true, "LC_ALL": true, "GIT_CONFIG_NOSYSTEM": true,
		"GIT_CONFIG_GLOBAL": true, "GIT_TERMINAL_PROMPT": true, "GIT_OPTIONAL_LOCKS": true,
		"GIT_PROTOCOL": true,
	}
	for _, entry := range f.server.gitEnv("version=2") {
		name, _, found := strings.Cut(entry, "=")
		if !found || !expected[name] {
			t.Fatalf("the child git process was given %q, which is not part of the closed environment", entry)
		}
		if strings.Contains(entry, planted) {
			t.Fatalf("a credential reached the child git process through %q", name)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		t.Fatalf("the closed environment is missing %v", expected)
	}

	// An anonymous clone: no credential helper, no Authorization header, no prompt.
	dest := filepath.Join(t.TempDir(), "anonymous")
	out, err := gitCommand(t, "", "-c", "credential.helper=", "clone", "--quiet", f.mirrorURL(key), dest)
	if err != nil {
		t.Fatalf("an anonymous clone failed: %v\n%s", err, out)
	}
	if body, err := os.ReadFile(filepath.Join(dest, "README.md")); err != nil || string(body) != "# demo\n" {
		t.Fatalf("the anonymous clone produced %q (%v)", body, err)
	}
	config, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
	if err != nil {
		t.Fatalf("read the cloned configuration: %v", err)
	}
	for _, forbidden := range []string{"extraheader", "credential", "Authorization", planted} {
		if strings.Contains(string(config), forbidden) {
			t.Fatalf("the clone stored %q in its configuration", forbidden)
		}
	}

	// A refusal must not ask the caller for a credential either: a challenge would be an invitation
	// to send one, and this endpoint never wants one.
	f.resolver.setErr(ErrOriginUnresolvable)
	refused := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack", nil)
	defer refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("a refusal answered %d, want 403", refused.StatusCode)
	}
	if challenge := refused.Header.Get("WWW-Authenticate"); challenge != "" {
		t.Fatalf("a refusal asked for credentials: %q", challenge)
	}
}

func TestOnlyTheMirrorPathsExist(t *testing.T) {
	f := newFixture(t)
	cases := map[string]int{
		"/":                                     http.StatusNotFound,
		"/mirror/v1/":                           http.StatusNotFound,
		"/mirror/v1/not-a-digest.git/info/refs": http.StatusNotFound,
		"/mirror/v1/" + strings.Repeat("a", 63) + ".git/info/refs": http.StatusNotFound,
		"/mirror/v1/" + strings.Repeat("A", 64) + ".git/info/refs": http.StatusNotFound,
		"/other/v1/" + strings.Repeat("a", 64) + ".git":            http.StatusNotFound,
	}
	for path, want := range cases {
		response := f.get(t, path, nil)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("GET %s = %d, want %d", path, response.StatusCode, want)
		}
	}
	posted := f.post(t, "/mirror/v1/"+strings.Repeat("a", 64)+".git/info/refs", nil, nil)
	defer posted.Body.Close()
	if posted.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST to the advertisement endpoint = %d, want 405", posted.StatusCode)
	}
	fetched := f.get(t, "/mirror/v1/"+strings.Repeat("a", 64)+".git/git-upload-pack", nil)
	defer fetched.Body.Close()
	if fetched.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET on the fetch endpoint = %d, want 405", fetched.StatusCode)
	}
}

func TestThePeerAddressComesFromTheConnection(t *testing.T) {
	f := newFixture(t)
	source, commit := sourceRepository(t, map[string]string{"README.md": "# demo\n"})
	key := testKey(commit)
	f.authorize(publishMirror(t, f.store, key, source))

	// The resolver is handed the connection's peer, and a forwarding header is not it: a deployment
	// behind a proxy would resolve the proxy and refuse, which is the fail-closed direction.
	response := f.get(t, "/mirror/v1/"+key.Digest()+".git/info/refs?service=git-upload-pack",
		http.Header{"X-Forwarded-For": {"203.0.113.9"}, "X-Real-Ip": {"203.0.113.9"}})
	response.Body.Close()
	peers := f.resolver.seenPeers()
	if len(peers) == 0 {
		t.Fatal("the resolver was never asked")
	}
	for _, peer := range peers {
		if !peer.IsLoopback() {
			t.Fatalf("the resolver was given %s, which the client supplied rather than the connection", peer)
		}
	}
}

// get issues a raw request, so a test can assert on the bytes a caller actually receives.
func (f *fixture) get(t *testing.T, path string, header http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.http.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := f.http.Client().Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return response
}

func (f *fixture) post(t *testing.T, path string, header http.Header, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, f.http.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := f.http.Client().Do(request)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return response
}

// gitCommand runs git and returns its combined output without failing the test, because several
// cases assert that git fails.
func gitCommand(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/true",
		"NO_PROXY=*", "no_proxy=*", "http_proxy=", "https_proxy=", "all_proxy=",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
