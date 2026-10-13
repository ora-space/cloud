package mirrorpublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

const (
	testProject   = "11111111-1111-1111-1111-111111111111"
	testTenant    = "22222222-2222-2222-2222-222222222222"
	testOtherRepo = "33333333-3333-3333-3333-333333333333"
	testRepoA     = "https://git.example.invalid/team/alpha"
	testRepoB     = "https://git.example.invalid/team/beta"
	testCommitA   = "0123456789abcdef0123456789abcdef01234567"
	testCommitB   = "89abcdef0123456789abcdef0123456789abcdef"
)

// fakeFetcher materializes content the way a real fetch would: it writes something into the staging
// directory the caller owns and reports the ref and commit that are now there. What it reports is
// deliberately its own answer rather than the request echoed back, because that is the value the
// store verifies the key against.
type fakeFetcher struct {
	calls    []Reference
	commit   string
	ref      string
	failure  error
	contents string
	// partial makes the fetch write content and only then fail, which is what a fetch interrupted
	// mid-transfer does. Without it a failing fetch would not exercise the reclaim of staged bytes.
	partial bool
}

func (f *fakeFetcher) Fetch(_ context.Context, reference *Reference, destination string) (mirror.PublishVerification, error) {
	f.calls = append(f.calls, *reference)
	if f.failure != nil && !f.partial {
		return mirror.PublishVerification{}, f.failure
	}
	contents := f.contents
	if contents == "" {
		contents = "content for " + reference.Key.Commit
	}
	if err := os.WriteFile(filepath.Join(destination, "README.md"), []byte(contents), 0o600); err != nil {
		return mirror.PublishVerification{}, err
	}
	if f.failure != nil {
		return mirror.PublishVerification{}, f.failure
	}
	commit := f.commit
	if commit == "" {
		commit = reference.Key.Commit
	}
	ref := f.ref
	if ref == "" {
		ref = MirrorRef
	}
	return mirror.PublishVerification{Commit: commit, Ref: ref}, nil
}

// stubBinder is a Cloud answer a test chooses. It is what stands in for PostgreSQL in these tests:
// the package's only input besides the store is this answer.
type stubBinder struct {
	binding Binding
	err     error
}

func (s *stubBinder) Binding(context.Context) (Binding, error) { return s.binding, s.err }

// referenceFor is the reference a binding would carry for one project's repository and commit.
func referenceFor(projectID, repository, commit string) Reference {
	return Reference{
		Key:      mirror.Key{ProjectID: projectID, Repository: repository, Commit: commit},
		TenantID: testTenant,
		Upstream: "main",
	}
}

// harness is one reconciler over a real store on a temporary volume, with a clock the test moves.
type harness struct {
	store   *mirror.Store
	fetcher *fakeFetcher
	binder  *stubBinder
	now     time.Time
	subject *Reconciler
}

func newHarness(t *testing.T, grace time.Duration) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	store, err := mirror.New(mirror.Config{
		Root:           t.TempDir(),
		MaxMirrorBytes: 1 << 20,
		MaxVolumeBytes: 4 << 20,
		Grace:          grace,
		Clock:          func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatalf("mirror.New: %v", err)
	}
	h.store = store
	h.fetcher = &fakeFetcher{}
	h.binder = &stubBinder{}
	h.subject = &Reconciler{
		Mirrors:  store,
		Bindings: h.binder,
		Fetcher:  h.fetcher,
		Clock:    func() time.Time { return h.now },
	}
	return h
}

// bindingFor builds the answer Cloud would give for one project pointing at one repository, with the
// given references live.
func bindingFor(projectID, repository string, references ...Reference) Binding {
	return Binding{
		Projects: []Project{{ID: projectID, TenantID: testTenant, Repository: repository}},
		Live:     references,
	}
}

func TestReconcilePublishesWhatCloudReferences(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)

	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Published) != 1 || report.Published[0] != reference.Key {
		t.Fatalf("published %v, want exactly %v", report.Published, reference.Key)
	}
	if len(report.Ready) != 1 || report.Ready[0] != reference.Key {
		t.Fatalf("ready %v, want exactly %v", report.Ready, reference.Key)
	}
	if len(h.fetcher.calls) != 1 {
		t.Fatalf("the fetcher ran %d times, want 1", len(h.fetcher.calls))
	}
	handle, err := h.store.Open(reference.Key)
	if err != nil {
		t.Fatalf("the published mirror is not readable: %v", err)
	}
	if handle.Record.Ref != MirrorRef {
		t.Fatalf("the mirror exposes ref %q, want %q", handle.Record.Ref, MirrorRef)
	}
	content, err := os.ReadFile(filepath.Join(handle.Dir, "README.md"))
	if err != nil {
		t.Fatalf("read the mirror content: %v", err)
	}
	if !strings.Contains(string(content), testCommitA) {
		t.Fatalf("the mirror holds %q, want the content fetched for %s", content, testCommitA)
	}
}

// TestReconcileRefusesAKeyCloudDoesNotReference is D2's assertion in test form: the writer's power
// over the volume is not what authorizes a publication — Cloud's binding is. A key Cloud has no row
// for must not reach the store even when every other input would accept it.
func TestReconcileRefusesAKeyCloudDoesNotReference(t *testing.T) {
	h := newHarness(t, time.Hour)
	// The project is live, so the key is addressable; it is simply not referenced.
	h.binder.binding = bindingFor(testProject, testRepoA)
	reference := referenceFor(testProject, testRepoA, testCommitA)

	key, outcome, err := h.subject.materialize(context.Background(), h.binder.binding, &reference)
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("materialize answered %v, want ErrNotAuthorized", err)
	}
	if outcome != outcomeSkipped {
		t.Fatalf("outcome %v, want skipped", outcome)
	}
	if key != reference.Key {
		t.Fatalf("materialize named %v, want the key it refused", key)
	}
	if len(h.fetcher.calls) != 0 {
		t.Fatalf("the fetcher ran for an unreferenced key: %v", h.fetcher.calls)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("the refused key is readable: %v", err)
	}
}

// TestReconcileDoesNotPublishAnUnreferencedKeyIntoTheStore is the same assertion one level up: a
// full pass over an empty binding must leave the store as empty as it found it.
func TestReconcileDoesNotPublishAnUnreferencedKeyIntoTheStore(t *testing.T) {
	h := newHarness(t, time.Hour)
	h.binder.binding = bindingFor(testProject, testRepoA)

	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	records, err := h.store.Records()
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("a pass with no references recorded %d mirrors: %v", len(records), records)
	}
}

// TestReconcileReusesAnAlreadyPublishedMirror is D6's key reuse: a second Workspace of one project
// resolves to a key whose content is immutable, so the content is not fetched again.
func TestReconcileReusesAnAlreadyPublishedMirror(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)

	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(report.Published) != 0 {
		t.Fatalf("the second pass published %v, want nothing", report.Published)
	}
	if len(report.Ready) != 1 {
		t.Fatalf("ready %v, want the reused key", report.Ready)
	}
	if len(h.fetcher.calls) != 1 {
		t.Fatalf("the fetcher ran %d times for one key, want 1", len(h.fetcher.calls))
	}
}

// TestReconcileWithdrawsAMirrorWhoseProjectChangedUpstream is D6's first revocation trigger. The
// withdrawal is durable: it is not the sweep's grace-based reclaim, and the store refuses to
// prepare or serve the key afterwards.
func TestReconcileWithdrawsAMirrorWhoseProjectChangedUpstream(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// The project now points at another repository, and no longer references the old commit.
	h.binder.binding = bindingFor(testProject, testRepoB)
	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(report.Revoked) != 1 || report.Revoked[0] != reference.Key {
		t.Fatalf("revoked %v, want exactly %v", report.Revoked, reference.Key)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("a withdrawn mirror is still readable: %v", err)
	}
	record, found, err := h.store.Lookup(reference.Key)
	if err != nil || !found {
		t.Fatalf("Lookup after withdrawal: found=%v err=%v", found, err)
	}
	if record.State != mirror.StateRevoked {
		t.Fatalf("the record is %q, want %q", record.State, mirror.StateRevoked)
	}
	// The withdrawal outlives an ordinary pass, which is what makes it a withdrawal.
	if _, err := h.store.Begin(context.Background(), reference.Key); !errors.Is(err, mirror.ErrRevoked) {
		t.Fatalf("Begin on a withdrawn key answered %v, want ErrRevoked", err)
	}
}

// TestReconcileLeavesAKeyAloneWhenTheProjectStillNamesItsRepository keeps the withdrawal to a
// mismatch that was observed. An unreferenced key whose project still points at the same repository
// is a reclaim question, not a security one, and the sweep decides it after the grace period.
func TestReconcileLeavesAKeyAloneWhenTheProjectStillNamesItsRepository(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	h.binder.binding = bindingFor(testProject, testRepoA)
	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(report.Revoked) != 0 {
		t.Fatalf("revoked %v, want nothing: the project still names the same repository", report.Revoked)
	}
	if len(report.Sweep.ObservedUnreferenced) != 1 {
		t.Fatalf("the sweep observed %v, want the key to start its grace period", report.Sweep)
	}
	// The mirror still exists during the grace period: a Workspace that is being torn down does not
	// take its project's other Workspaces' mirror with it.
	if _, err := h.store.Open(reference.Key); err != nil {
		t.Fatalf("the mirror was reclaimed before its grace period: %v", err)
	}
}

// TestReconcileReclaimsAnUnreferencedMirrorAfterGrace is the destroy trigger: Cloud stopped
// referencing the key, the grace period passed, and the content is gone.
func TestReconcileReclaimsAnUnreferencedMirrorAfterGrace(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	h.binder.binding = bindingFor(testProject, testRepoA)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	h.now = h.now.Add(2 * time.Hour)
	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if len(report.Sweep.Destroyed) != 1 || report.Sweep.Destroyed[0] != reference.Key {
		t.Fatalf("destroyed %v, want exactly %v", report.Sweep.Destroyed, reference.Key)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("a destroyed mirror is still readable: %v", err)
	}
}

// TestReconcileFailsThePreparationWhenTheFetchFails is the first half of failure recovery: a fetch
// that fails leaves a record a reader refuses, not partial content a reader could be served.
func TestReconcileFailsThePreparationWhenTheFetchFails(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	h.fetcher.failure = errors.New("upstream refused the credential")

	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Failed) != 1 || report.Failed[0] != reference.Key {
		t.Fatalf("failed %v, want exactly %v", report.Failed, reference.Key)
	}
	if len(report.Ready) != 0 {
		t.Fatalf("ready %v, want nothing after a failed fetch", report.Ready)
	}
	record, found, err := h.store.Lookup(reference.Key)
	if err != nil || !found {
		t.Fatalf("Lookup after a failed fetch: found=%v err=%v", found, err)
	}
	if record.State != mirror.StateFailed {
		t.Fatalf("the record is %q, want %q", record.State, mirror.StateFailed)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("a failed mirror is readable: %v", err)
	}
}

// TestReconcileRecoversFromAFailedFetch is the second half: the next pass retries the key and
// succeeds. A failure is terminal for one attempt, not for the mirror.
func TestReconcileRecoversFromAFailedFetch(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	h.fetcher.failure = errors.New("upstream was unreachable")
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	h.fetcher.failure = nil
	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if len(report.Published) != 1 {
		t.Fatalf("published %v, want the retried key", report.Published)
	}
	record, _, err := h.store.Lookup(reference.Key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if record.State != mirror.StateReady {
		t.Fatalf("the record is %q, want %q", record.State, mirror.StateReady)
	}
}

// TestReconcileRefusesAFetchThatMaterializedAnotherCommit is why the fetcher's answer is verified
// against the key rather than trusted: a writer that fetched the wrong commit would otherwise
// publish content under a key that names a different one.
func TestReconcileRefusesAFetchThatMaterializedAnotherCommit(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	h.fetcher.commit = testCommitB

	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.Failed) != 1 {
		t.Fatalf("failed %v, want the key whose content was the wrong commit", report.Failed)
	}
	if _, err := h.store.Open(reference.Key); !errors.Is(err, mirror.ErrNotReady) {
		t.Fatalf("a mirror of the wrong commit is readable: %v", err)
	}
}

// TestReconcileReclaimsTheStagingLeftByAFailedFetch measures the failure-recovery path through the
// store: the preparation's directory is gone, and the sweep's own accounting agrees. A failed
// mirror must not leave content on the volume that nothing will ever reclaim.
func TestReconcileReclaimsTheStagingLeftByAFailedFetch(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	h.fetcher.failure = errors.New("the fetch died")
	h.fetcher.partial = true
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	staging, err := os.ReadDir(filepath.Join(h.store.Root(), "staging"))
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}
	if len(staging) != 0 {
		t.Fatalf("a failed fetch left %d staged entries behind", len(staging))
	}
}

// TestReconcileDoesNothingWhenCloudCannotAnswer is the fail-closed case. An unavailable Cloud must
// not read as "nothing is referenced": that would withdraw and reclaim the whole volume on the
// strength of a question that was never answered.
func TestReconcileDoesNothingWhenCloudCannotAnswer(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	h.binder.err = errors.New("the database is unreachable")
	h.now = h.now.Add(72 * time.Hour)
	report, err := h.subject.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile answered for a Cloud it could not read")
	}
	if len(report.Published) != 0 || len(report.Revoked) != 0 || len(report.Sweep.Destroyed) != 0 {
		t.Fatalf("a pass that could not read Cloud changed the store: %+v", report)
	}
	if _, err := h.store.Open(reference.Key); err != nil {
		t.Fatalf("the mirror was disturbed by an unanswerable pass: %v", err)
	}
}

// TestReconcileDeletesAStaleMirrorOfAProjectThatIsGone covers the third trigger: the project itself
// is gone, so nothing compares against its upstream, and the sweep alone decides after the grace
// period.
func TestReconcileDeletesAStaleMirrorOfAProjectThatIsGone(t *testing.T) {
	h := newHarness(t, time.Hour)
	reference := referenceFor(testProject, testRepoA, testCommitA)
	h.binder.binding = bindingFor(testProject, testRepoA, reference)
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	h.binder.binding = Binding{}
	if _, err := h.subject.Reconcile(context.Background()); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	h.now = h.now.Add(2 * time.Hour)
	report, err := h.subject.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("third Reconcile: %v", err)
	}
	if len(report.Revoked) != 0 {
		t.Fatalf("revoked %v, want nothing: a project that is gone has no upstream to have changed", report.Revoked)
	}
	if len(report.Sweep.Destroyed) != 1 {
		t.Fatalf("destroyed %v, want the unreferenced key", report.Sweep.Destroyed)
	}
}

func TestReconcileRequiresItsDependencies(t *testing.T) {
	h := newHarness(t, time.Hour)
	for name, subject := range map[string]*Reconciler{
		"no store":   {Bindings: h.binder, Fetcher: h.fetcher},
		"no binder":  {Mirrors: h.store, Fetcher: h.fetcher},
		"no fetcher": {Mirrors: h.store, Bindings: h.binder},
	} {
		if _, err := subject.Reconcile(context.Background()); err == nil {
			t.Fatalf("%s: Reconcile accepted an incomplete reconciler", name)
		}
	}
}

func TestUpstreamBranchRejectsWhatGitWouldNotTake(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		upstream string
		want     string
	}{
		{"a branch name", "main", "main"},
		{"a branch name with a slash", "release/v2", "release/v2"},
		{"surrounding whitespace", "  main  ", "main"},
		{"empty", "", ""},
		{"an option", "--upload-pack=touch /tmp/x", ""},
		{"a path traversal", "../heads/main", ""},
		{"a double dot", "main..next", ""},
		{"a revision suffix", "main~1", ""},
		{"a ref expression", "main@{1}", ""},
		{"a colon", "main:other", ""},
		{"a glob", "main*", ""},
		{"a lock suffix", "main.lock", ""},
		{"a trailing slash", "main/", ""},
		{"a leading slash", "/main", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			branch, err := upstreamBranch(testCase.upstream)
			if testCase.want == "" {
				if err == nil {
					t.Fatalf("upstreamBranch(%q) accepted it as %q", testCase.upstream, branch)
				}
				return
			}
			if err != nil {
				t.Fatalf("upstreamBranch(%q): %v", testCase.upstream, err)
			}
			if branch != testCase.want {
				t.Fatalf("upstreamBranch(%q) = %q, want %q", testCase.upstream, branch, testCase.want)
			}
		})
	}
}

func TestHelperArgumentRefusesAnIdentityTheHelperCannotRead(t *testing.T) {
	const root = "/var/lib/ora/mirror-material"
	if _, err := helperArgument("/usr/local/bin/git-credential-ora", root, "not-a-uuid"); err == nil {
		t.Fatal("helperArgument accepted an identity that is not a canonical UUID")
	}
	argument, err := helperArgument("/usr/local/bin/git-credential-ora", root, "44444444-4444-4444-4444-444444444444")
	if err != nil {
		t.Fatalf("helperArgument: %v", err)
	}
	want := "/usr/local/bin/git-credential-ora " + root + " 44444444-4444-4444-4444-444444444444"
	if argument != want {
		t.Fatalf("helperArgument = %q, want %q", argument, want)
	}
}

func TestNewGitFetcherRefusesAnIncompleteDeployment(t *testing.T) {
	home := t.TempDir()
	complete := GitConfig{
		GitPath:      "/usr/bin/git",
		Home:         home,
		HelperPath:   "/usr/local/bin/git-credential-ora",
		MaterialRoot: "/var/lib/ora/mirror-material",
		Timeout:      time.Minute,
	}
	if _, err := NewGitFetcher(complete, &stubMaterial{}); err != nil {
		t.Fatalf("NewGitFetcher refused a complete deployment: %v", err)
	}
	for name, mutate := range map[string]func(*GitConfig){
		"no git":              func(c *GitConfig) { c.GitPath = "" },
		"no helper":           func(c *GitConfig) { c.HelperPath = "" },
		"no material root":    func(c *GitConfig) { c.MaterialRoot = "" },
		"no home":             func(c *GitConfig) { c.Home = "" },
		"a relative path":     func(c *GitConfig) { c.GitPath = "git" },
		"a shell metachar":    func(c *GitConfig) { c.MaterialRoot = "/var/lib/ora/x;y" },
		"no timeout":          func(c *GitConfig) { c.Timeout = 0 },
		"a missing home":      func(c *GitConfig) { c.Home = filepath.Join(home, "absent") },
		"a relative CA file":  func(c *GitConfig) { c.SSLCAInfo = "ca.pem" },
		"an unclean helper":   func(c *GitConfig) { c.HelperPath = "/usr/local/bin/../bin/helper" },
		"a metachar in git":   func(c *GitConfig) { c.GitPath = "/usr/bin/git $HOME" },
		"a metachar in home":  func(c *GitConfig) { c.Home = home + "/sp ace" },
		"an unconfigured CA":  func(c *GitConfig) { c.SSLCAInfo = " " },
		"a quote in material": func(c *GitConfig) { c.MaterialRoot = "/var/lib/ora/'x'" },
	} {
		t.Run(name, func(t *testing.T) {
			config := complete
			mutate(&config)
			if _, err := NewGitFetcher(config, &stubMaterial{}); err == nil {
				t.Fatalf("NewGitFetcher accepted %+v", config)
			}
		})
	}
	if _, err := NewGitFetcher(complete, nil); err == nil {
		t.Fatal("NewGitFetcher accepted a deployment with no credential source")
	}
}

// TestGitFetcherCommandLineIsTheHardenedOne pins the exact fetch Git is run with. The command line is
// the security boundary here — it is what closes the protocol set, refuses redirects and fixes the
// credential helper — so a change to it should have to be made in front of this assertion.
func TestGitFetcherCommandLineIsTheHardenedOne(t *testing.T) {
	fetcher := &GitFetcher{config: GitConfig{
		GitPath:      "/usr/bin/git",
		Home:         "/var/empty/ora-mirror",
		HelperPath:   "/usr/local/bin/git-credential-ora",
		MaterialRoot: "/var/lib/ora/mirror-material",
		SSLCAInfo:    "/etc/ora/ca.pem",
		Timeout:      time.Minute,
	}}
	helper := "/usr/local/bin/git-credential-ora /var/lib/ora/mirror-material 44444444-4444-4444-4444-444444444444"
	arguments, err := fetcher.arguments(helper, testRepoA, "main")
	if err != nil {
		t.Fatalf("arguments: %v", err)
	}
	line := strings.Join(arguments, " ")
	for _, required := range []string{
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ssh.allow=never",
		"protocol.file.allow=never",
		"protocol.ext.allow=never",
		"core.hooksPath=/dev/null",
		"credential.helper=" + helper,
		"credential.useHttpPath=true",
		"http.followRedirects=false",
		"http.sslVerify=true",
		"http.sslCAInfo=/etc/ora/ca.pem",
		"fetch --quiet --no-tags --no-write-fetch-head",
		"-- " + testRepoA + " +refs/heads/main:" + MirrorRef,
	} {
		if !strings.Contains(line, required) {
			t.Fatalf("the fetch command line is missing %q:\n%s", required, line)
		}
	}
	if strings.HasPrefix(line, "-c") == false {
		t.Fatalf("the configuration must precede the subcommand:\n%s", line)
	}
}

// TestGitFetcherRefusesAnUnusableRepository checks the one input that reaches the command line from a
// reference. A repository URL is canonicalized before it arrives here, and this is the second check
// on it: a value that is not an https URL with a host never reaches Git.
func TestGitFetcherRefusesAnUnusableRepository(t *testing.T) {
	fetcher := &GitFetcher{config: GitConfig{}}
	const helper = "/usr/local/bin/git-credential-ora /var/lib/ora/mirror-material 44444444-4444-4444-4444-444444444444"
	for _, repository := range []string{
		"--upload-pack=touch /tmp/x",
		"http://git.example.invalid/team/alpha",
		"https:///team/alpha",
		"git@git.example.invalid:team/alpha",
		"https://git.example.invalid/team/alpha with a space",
		"",
	} {
		if _, err := fetcher.arguments(helper, repository, "main"); err == nil {
			t.Fatalf("arguments accepted the repository %q", repository)
		}
	}
	if _, err := fetcher.arguments(helper, testRepoA, "main"); err != nil {
		t.Fatalf("arguments refused a canonical repository: %v", err)
	}
}

// TestAGitThatHangsDoesNotOutliveTheFetchersTimeout pins that GitConfig.Timeout is what ends a fetch
// and not Git's own patience.
//
// The fake git starts a descendant that inherits its standard output and then replaces itself with
// another sleep, so killing the process the fetcher started leaves the pipe open. A wait that ends
// only when the pipe closes then waits for that descendant, and the declared timeout becomes
// advisory: without the wait delay this fetch returns in about a minute, in about the timeout with
// it. The bound is what the caller of Fetch relies on when it decides a repository is unreachable.
func TestAGitThatHangsDoesNotOutliveTheFetchersTimeout(t *testing.T) {
	gitPath := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(gitPath, []byte("#!/bin/sh\nsleep 60 &\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatalf("write the fake git: %v", err)
	}
	fetcher, err := NewGitFetcher(GitConfig{
		GitPath:      gitPath,
		Home:         t.TempDir(),
		HelperPath:   "/usr/local/bin/git-credential-ora",
		MaterialRoot: t.TempDir(),
		Timeout:      250 * time.Millisecond,
	}, stubMaterial{})
	if err != nil {
		t.Fatalf("NewGitFetcher: %v", err)
	}
	reference := &Reference{
		Key:      mirror.Key{ProjectID: testProject, Repository: testRepoA, Commit: testCommitA},
		TenantID: testTenant,
		Upstream: "main",
	}
	started := time.Now()
	_, err = fetcher.Fetch(context.Background(), reference, filepath.Join(t.TempDir(), "mirror"))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("the fetch reported success against a git that never exits")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the fetch took %s; the 250ms timeout did not end the wait", elapsed)
	}
}

// stubMaterial is a credential source that stages nothing, for the tests that only exercise
// configuration and command-line construction.
type stubMaterial struct{}

func (stubMaterial) Stage(context.Context, *Reference) (Material, error) {
	return Material{Identity: "44444444-4444-4444-4444-444444444444"}, nil
}
