package cloneexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// The identifiers these tests use for one preparation. The operation and the authorization are
// canonical lowercase UUIDs because the materialization layout turns the authorization into a path
// segment, and the proof that a preparation refuses a value of another shape has to run against the
// same rule the deployment does.
const (
	testOperationID      = "3f7a1c85-9b26-4d03-8e57-1c4a6b8d2f90"
	testAuthorizationID  = "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46"
	testRef              = "refs/heads/main"
	testExecutorIdentity = "spiffe://ora/node/credential-executor"
	testMirrorContent    = "mirrored content\n"
)

// recordingMirrorStore is a MirrorRedemptionClient that records what it was asked and answers what the
// test told it to.
//
// It exists for the same reason the clone path's does: the ordering this package promises — verify
// locally, then spend — is only observable if a test can assert that redemption was never reached.
type recordingMirrorStore struct {
	credential Credential
	err        error
	calls      int
	gotID      string
	gotAuth    string
}

func (s *recordingMirrorStore) ResolveMirror(_ context.Context, credentialID, authorization string) (Credential, error) {
	s.calls++
	s.gotID = credentialID
	s.gotAuth = authorization
	if s.err != nil {
		return Credential{}, s.err
	}
	return s.credential, nil
}

// mirrorFixture is one ready mirror executor over a real mirror store, in a temporary deployment.
type mirrorFixture struct {
	executor *MirrorExecutor
	config   MirrorConfig
	key      ed25519.PrivateKey
	store    *recordingMirrorStore
	mirrors  *mirror.Store
	dir      string
	capture  string
	// failFetch is the marker file whose presence makes the Git double refuse the fetch.
	failFetch string
}

// newMirrorFixture builds a deployment in a temporary directory.
//
// The Git binary is a double, as it is for the clone path: these tests are about what this package
// validates, what it spends, what it publishes, and what it cleans up. That a real Git fetches a real
// private ref through the real helper is the acceptance test's job, and it uses the real binary for
// exactly that reason. The mirror store, by contrast, is the real one: publication is this package's
// product, and a double for it would prove nothing about what a reader can read afterwards.
func newMirrorFixture(t *testing.T, adjust func(*MirrorConfig)) *mirrorFixture {
	t.Helper()
	dir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the deployment's authorization key: %v", err)
	}
	mirrors, err := mirror.New(mirror.Config{
		Root:           filepath.Join(dir, "mirrors"),
		MaxMirrorBytes: 1 << 20,
		MaxVolumeBytes: 1 << 22,
		Grace:          time.Hour,
		Clock:          func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("open the mirror store: %v", err)
	}
	store := &recordingMirrorStore{credential: Credential{Token: testToken, Version: 3}}
	fixture := &mirrorFixture{
		config: MirrorConfig{
			MaterialRoot:     filepath.Join(dir, "material"),
			HomeDirectory:    filepath.Join(dir, "home"),
			HelperPath:       filepath.Join(dir, "git-credential-ora"),
			GitPath:          filepath.Join(dir, "git"),
			VerificationKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)},
			Mirrors:          mirrors,
			Store:            store,
			Timeout:          time.Minute,
			Now:              func() time.Time { return testNow },
			Log:              zap.NewNop(),
		},
		key:       key,
		store:     store,
		mirrors:   mirrors,
		dir:       dir,
		capture:   filepath.Join(dir, "git-invocations"),
		failFetch: filepath.Join(dir, "fail-fetch"),
	}
	if adjust != nil {
		adjust(&fixture.config)
	}
	// The deployment creates and owns the materialization root, the home and the helper; this package
	// only writes inside them.
	for _, root := range []string{fixture.config.MaterialRoot, fixture.config.HomeDirectory} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("create %s: %v", root, err)
		}
	}
	if err := os.WriteFile(fixture.config.HelperPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("create the helper placeholder: %v", err)
	}
	writeMirrorGitDouble(t, fixture)
	executor, err := NewMirrorExecutor(fixture.config)
	if err != nil {
		t.Fatalf("build the mirror executor: %v", err)
	}
	fixture.executor = executor
	return fixture
}

// writeMirrorGitDouble writes the Git double the mirror path runs.
//
// It records its own argv so a test asserts what this package executed rather than what it meant to,
// and it writes into the directory Git would have been started in, so the content that reaches the
// store is the content this double produced — which is what makes the publication assertions about
// the fetch and not about the fixture.
func writeMirrorGitDouble(t *testing.T, fixture *mirrorFixture) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$@" >> %[1]s
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
    if [ -f %[2]s ]; then
      printf '%%s\n' 'fatal: Authentication failed for the repository' >&2
      exit 128
    fi
    printf '%%s' %[3]s > fetched.txt
    ;;
  revparse)
    printf '%%s\n' %[4]s
    ;;
  refs)
    printf '%%s\n' %[5]s
    ;;
esac
exit 0
`, shellQuote(fixture.capture), shellQuote(fixture.failFetch),
		shellQuote(testMirrorContent), shellQuote(testCommit), shellQuote(mirrorpublish.MirrorRef))
	if err := os.WriteFile(fixture.config.GitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the git double: %v", err)
	}
}

// issue returns a job together with the authorization Cloud would have signed for it.
//
// The job is derived from the binding *before* either adjustment runs, so a case that changes the
// binding creates exactly the disagreement it names: a job whose own claims and whose signed authority
// no longer agree. Deriving the job afterwards would make every such case agree with itself and test
// nothing.
func (f *mirrorFixture) issue(t *testing.T, adjustBinding func(*cloneauth.Binding), adjustJob func(*MirrorJob)) MirrorJob {
	t.Helper()
	binding := cloneauth.Binding{
		AuthorizationID:       testAuthorizationID,
		Operation:             cloneauth.OperationMirrorPrepare,
		TenantID:              "tenant-a",
		OwnerUserID:           "user-a",
		ProjectID:             "project-a",
		WorkspaceID:           "workspace-a",
		CredentialID:          testCredentialID,
		CredentialVersion:     3,
		Repository:            testRepository,
		Ref:                   testRef,
		Capabilities:          []string{string(cloneauth.CapabilityClone)},
		OperationID:           testOperationID,
		PreparationGeneration: 2,
		ExecutorIdentity:      testExecutorIdentity,
		RuntimeGeneration:     7,
		ControlEpoch:          0,
		IssuedAt:              testNow.Add(-time.Minute),
		ExpiresAt:             testNow.Add(9 * time.Minute),
	}
	job := MirrorJob{
		OperationID:           binding.OperationID,
		PreparationGeneration: binding.PreparationGeneration,
		ProjectID:             binding.ProjectID,
		Repository:            binding.Repository,
		Ref:                   binding.Ref,
		CredentialID:          binding.CredentialID,
		CredentialVersion:     binding.CredentialVersion,
		RuntimeGeneration:     binding.RuntimeGeneration,
		ControlEpoch:          binding.ControlEpoch,
	}
	if adjustBinding != nil {
		adjustBinding(&binding)
	}
	authorization, err := cloneauth.Issue(f.key, binding)
	if err != nil {
		t.Fatalf("issue the test authorization: %v", err)
	}
	job.Authorization = authorization
	if adjustJob != nil {
		adjustJob(&job)
	}
	return job
}

// mirrorKey is the mirror key the fetch's commit and the authorization's project and repository produce.
func (f *mirrorFixture) mirrorKey() mirror.Key {
	return mirror.Key{ProjectID: "project-a", Repository: testRepository, Commit: testCommit}
}

// assertNoSpend checks that nothing was redeemed and that no material was written.
//
// It is the assertion every refusal case shares: a preparation that refuses before it spends leaves
// the authorization usable, which is what makes "this attempt failed" different from "this attempt
// burned the single use Cloud gave it".
func (f *mirrorFixture) assertNoSpend(t *testing.T) {
	t.Helper()
	if f.store.calls != 0 {
		t.Fatalf("redemption was attempted %d times for a job that was refused before spending", f.store.calls)
	}
	entries, err := os.ReadDir(f.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root holds %d entries after a refusal", len(entries))
	}
}

func TestNewMirrorExecutorRefusesADeploymentThatCannotEstablishTheBoundary(t *testing.T) {
	base := func(t *testing.T, dir string) MirrorConfig {
		t.Helper()
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate a key: %v", err)
		}
		mirrors, err := mirror.New(mirror.Config{
			Root:           filepath.Join(dir, "mirrors"),
			MaxMirrorBytes: 1 << 20,
			MaxVolumeBytes: 1 << 22,
			Grace:          time.Hour,
		})
		if err != nil {
			t.Fatalf("open the mirror store: %v", err)
		}
		config := MirrorConfig{
			MaterialRoot:     filepath.Join(dir, "material"),
			HomeDirectory:    filepath.Join(dir, "home"),
			HelperPath:       filepath.Join(dir, "helper"),
			GitPath:          filepath.Join(dir, "git"),
			VerificationKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)},
			Mirrors:          mirrors,
			Store:            &recordingMirrorStore{},
		}
		for _, root := range []string{config.MaterialRoot, config.HomeDirectory} {
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatalf("create %s: %v", root, err)
			}
		}
		return config
	}
	cases := []struct {
		name   string
		adjust func(*MirrorConfig)
		accept bool
	}{
		{"no materialization root", func(c *MirrorConfig) { c.MaterialRoot = "" }, false},
		{"relative materialization root", func(c *MirrorConfig) { c.MaterialRoot = "material" }, false},
		{"no home directory", func(c *MirrorConfig) { c.HomeDirectory = "" }, false},
		{"a home directory that is not 0700", func(c *MirrorConfig) {
			if err := os.Chmod(c.HomeDirectory, 0o755); err != nil {
				t.Fatalf("widen the home directory: %v", err)
			}
		}, false},
		{"no helper", func(c *MirrorConfig) { c.HelperPath = "" }, false},
		{"a helper path with whitespace", func(c *MirrorConfig) { c.HelperPath = "/usr/local/bin/git credential ora" }, false},
		{"no git", func(c *MirrorConfig) { c.GitPath = "" }, false},
		{"no verification key", func(c *MirrorConfig) { c.VerificationKeys = nil }, false},
		{"a verification key of the wrong size", func(c *MirrorConfig) { c.VerificationKeys = []ed25519.PublicKey{make([]byte, 8)} }, false},
		{"no credential service", func(c *MirrorConfig) { c.Store = nil }, false},
		{"no mirror store", func(c *MirrorConfig) { c.Mirrors = nil }, false},
		{"a certificate authority file that is not a path", func(c *MirrorConfig) { c.SSLCAInfo = "/etc/ora/ca bundle.pem" }, false},
		{"a complete deployment", nil, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base(t, t.TempDir())
			if testCase.adjust != nil {
				testCase.adjust(&config)
			}
			executor, err := NewMirrorExecutor(config)
			if testCase.accept {
				if err != nil {
					t.Fatalf("a complete deployment was refused: %v", err)
				}
				if executor.config.Timeout != DefaultMirrorTimeout {
					t.Fatalf("default timeout = %s, want %s", executor.config.Timeout, DefaultMirrorTimeout)
				}
				// The fetch root is inside the store's own root, because publication moves the fetched tree
				// into the store's staging directory and that move has to be a rename.
				if want := filepath.Join(config.Mirrors.Root(), "fetch"); executor.fetchRoot != want {
					t.Fatalf("fetch root = %q, want %q", executor.fetchRoot, want)
				}
				return
			}
			if err == nil {
				t.Fatal("an incomplete deployment was accepted")
			}
			if executor != nil {
				t.Fatal("an incomplete deployment produced an executor")
			}
		})
	}
}

// TestMirrorPrepareRefusesBeforeSpendingAnything is the negative matrix for the job and the binding.
//
// Every case asserts two things at once: the refusal code, and that the Credential Service was never
// asked for the secret. The second is the one that matters — an authorization is single-use, so a
// preparation that refuses *after* redeeming has destroyed the very thing a retry needs, and the
// ordering this package promises would be invisible if only the code were checked.
func TestMirrorPrepareRefusesBeforeSpendingAnything(t *testing.T) {
	cases := []struct {
		name          string
		adjustBinding func(*cloneauth.Binding)
		adjustJob     func(*MirrorJob)
		want          string
	}{
		{"the repository is not https", nil, func(j *MirrorJob) { j.Repository = "http://git.example.com/ora/private-repo" }, ReasonCodeInvalidJob},
		{"the operation is not an identifier", nil, func(j *MirrorJob) { j.OperationID = "operation-a" }, ReasonCodeInvalidJob},
		{"the job names no attempt", nil, func(j *MirrorJob) { j.PreparationGeneration = 0 }, ReasonCodeInvalidJob},
		{"the job names no project", nil, func(j *MirrorJob) { j.ProjectID = "" }, ReasonCodeInvalidJob},
		{"the job carries no authorization", nil, func(j *MirrorJob) { j.Authorization = "" }, ReasonCodeInvalidJob},
		{"the credential id is not canonical", nil, func(j *MirrorJob) { j.CredentialID = "credential-a" }, ReasonCodeInvalidJob},
		{"the ref is a tag", nil, func(j *MirrorJob) { j.Ref = "refs/tags/v1" }, ReasonCodeInvalidJob},
		{"the ref is a short name", nil, func(j *MirrorJob) { j.Ref = "main" }, ReasonCodeInvalidJob},
		{"the authorization is not one of ours", nil, func(j *MirrorJob) { j.Authorization = "not-an-authorization" }, ReasonCodeAuthorizationRejected},
		{"the authorization is for another operation", func(b *cloneauth.Binding) { b.OperationID = "0c4e7b19-2f68-4a95-83d1-6b9e0f2a7c58" }, nil, ReasonCodeBindingMismatch},
		{"the authorization is for another attempt", func(b *cloneauth.Binding) { b.PreparationGeneration = 3 }, nil, ReasonCodeBindingMismatch},
		{"the authorization is for another project", func(b *cloneauth.Binding) { b.ProjectID = "project-b" }, nil, ReasonCodeBindingMismatch},
		{"the authorization is for another repository", func(b *cloneauth.Binding) { b.Repository = "https://git.example.com/ora/other-repo" }, nil, ReasonCodeBindingMismatch},
		{"the authorization pins another ref", func(b *cloneauth.Binding) { b.Ref = "refs/heads/release" }, nil, ReasonCodeBindingMismatch},
		{"the authorization names another credential", func(b *cloneauth.Binding) {
			b.CredentialID = "7e4b1c93-5d28-4a07-9f61-3b8e0d5a2c74"
		}, nil, ReasonCodeBindingMismatch},
		{"the authorization names another version", func(b *cloneauth.Binding) { b.CredentialVersion = 4 }, nil, ReasonCodeBindingMismatch},
		{"the authorization is for another runtime generation", func(b *cloneauth.Binding) { b.RuntimeGeneration = 8 }, nil, ReasonCodeBindingMismatch},
		{"the authorization is for another control epoch", func(b *cloneauth.Binding) { b.ControlEpoch = 1 }, nil, ReasonCodeBindingMismatch},
		{"the authorization's identity is not a canonical uuid", func(b *cloneauth.Binding) {
			b.AuthorizationID = "authorization-a"
		}, nil, ReasonCodeInvalidJob},
		{"the authorization has expired", func(b *cloneauth.Binding) {
			b.IssuedAt = testNow.Add(-20 * time.Minute)
			b.ExpiresAt = testNow.Add(-10 * time.Minute)
		}, nil, ReasonCodeAuthorizationRejected},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newMirrorFixture(t, nil)
			job := fixture.issue(t, testCase.adjustBinding, testCase.adjustJob)
			_, err := fixture.executor.Prepare(context.Background(), job)
			if got := reasonCode(err); got != testCase.want {
				t.Fatalf("refusal code = %q, want %q (error: %v)", got, testCase.want, err)
			}
			fixture.assertNoSpend(t)
			// Nothing was published either: a refused preparation must not have created a record.
			if records, err := fixture.mirrors.Records(); err != nil {
				t.Fatalf("read the mirror records: %v", err)
			} else if len(records) != 0 {
				t.Fatalf("a refused preparation left %d mirror records", len(records))
			}
		})
	}
}

// TestMirrorPrepareRefusesACloneAuthorizationWithoutSpendingIt is one half of the purpose-confusion
// matrix the 2026-10-13 ruling requires: the mirror consumer accepts `mirror_prepare` and nothing
// else.
//
// The authorization is a genuine, validly signed clone authorization — the only thing wrong with it
// is its purpose — so a refusal here is a decision about the operation and not about the signature.
// That it is not spent is the reason the two routes exist: a clone that arrived at the mirror endpoint
// and was burned there would be a clone the Node's job could no longer perform.
func TestMirrorPrepareRefusesACloneAuthorizationWithoutSpendingIt(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	binding := cloneauth.Binding{
		AuthorizationID:   testAuthorizationID,
		Operation:         cloneauth.OperationClone,
		TenantID:          "tenant-a",
		OwnerUserID:       "user-a",
		ProjectID:         "project-a",
		WorkspaceID:       "workspace-a",
		ExecutionID:       testExecutionID,
		CredentialID:      testCredentialID,
		CredentialVersion: 3,
		Repository:        testRepository,
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		NodeID:            testNodeID,
		NodeInstanceID:    testNodeInstance,
		NodeIncarnationID: testNodeIncarn,
		RuntimeGeneration: 7,
		ControlEpoch:      0,
		IssuedAt:          testNow.Add(-time.Minute),
		ExpiresAt:         testNow.Add(9 * time.Minute),
	}
	authorization, err := cloneauth.Issue(fixture.key, binding)
	if err != nil {
		t.Fatalf("issue the clone authorization: %v", err)
	}
	// The job otherwise matches the binding field for field wherever the two shapes overlap, so the
	// refusal can only be the operation.
	_, err = fixture.executor.Prepare(context.Background(), MirrorJob{
		OperationID:           testOperationID,
		PreparationGeneration: 2,
		ProjectID:             "project-a",
		Repository:            testRepository,
		Ref:                   testRef,
		CredentialID:          testCredentialID,
		CredentialVersion:     3,
		RuntimeGeneration:     7,
		ControlEpoch:          0,
		Authorization:         authorization,
	})
	if got := reasonCode(err); got != ReasonCodeBindingMismatch {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, ReasonCodeBindingMismatch, err)
	}
	fixture.assertNoSpend(t)
}

// TestMirrorPreparePublishesWhatTheFetchProduced is the path's positive evidence.
//
// It asserts the whole product rather than the return value alone: the store really holds a readable
// record for the commit the fetch reported, the published tree really holds the bytes the fetch wrote,
// the fetch scratch directory is gone, and no credential material survives anywhere under the
// materialization root. A preparation that returned the right numbers while leaving the token on disk
// would fail the last of those, which is the one the decisions are about.
func TestMirrorPreparePublishesWhatTheFetchProduced(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	job := fixture.issue(t, nil, nil)

	result, err := fixture.executor.Prepare(context.Background(), job)
	if err != nil {
		t.Fatalf("the preparation was refused: %v", err)
	}
	if result.Commit != testCommit {
		t.Fatalf("commit = %q, want %q", result.Commit, testCommit)
	}
	if result.Ref != mirrorpublish.MirrorRef {
		t.Fatalf("ref = %q, want %q", result.Ref, mirrorpublish.MirrorRef)
	}
	if result.Repository != testRepository {
		t.Fatalf("repository = %q, want %q", result.Repository, testRepository)
	}
	if result.Generation != 1 {
		t.Fatalf("generation = %d, want 1", result.Generation)
	}
	if result.CredentialVersion != 3 {
		t.Fatalf("credential version = %d, want 3", result.CredentialVersion)
	}
	if fixture.store.gotID != testCredentialID {
		t.Fatalf("the redemption named credential %q, want %q", fixture.store.gotID, testCredentialID)
	}
	if fixture.store.gotAuth != job.Authorization {
		t.Fatal("the redemption did not present the job's authorization")
	}
	// The store, not the return value, is what a reader consults.
	handle, err := fixture.mirrors.Open(fixture.mirrorKey())
	if err != nil {
		t.Fatalf("the published mirror is not readable: %v", err)
	}
	if handle.Record.Ref != mirrorpublish.MirrorRef {
		t.Fatalf("the published record names ref %q, want %q", handle.Record.Ref, mirrorpublish.MirrorRef)
	}
	content, err := os.ReadFile(filepath.Join(handle.Dir, "fetched.txt"))
	if err != nil {
		t.Fatalf("read the published content: %v", err)
	}
	if string(content) != testMirrorContent {
		t.Fatalf("published content = %q, want %q", content, testMirrorContent)
	}
	// The fetch scratch is gone, and so is the material.
	fetches, err := os.ReadDir(fixture.executor.fetchRoot)
	if err != nil {
		t.Fatalf("read the fetch root: %v", err)
	}
	if len(fetches) != 0 {
		t.Fatalf("the fetch root holds %d entries after a successful preparation", len(fetches))
	}
	assertNoTokenUnder(t, fixture.config.MaterialRoot, testToken)
	entries, err := os.ReadDir(fixture.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root holds %d entries after a successful preparation", len(entries))
	}
	// And the fetch really ran with the hardening, through the deployment's own helper, scoped to the
	// authorization's own material directory.
	assertCaptured(t, fixture.capture, "init", "--quiet", "--bare", "--")
	assertCaptured(t, fixture.capture, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--",
		testRepository, "+"+testRef+":"+mirrorpublish.MirrorRef,
		"credential.helper="+fixture.config.HelperPath+" "+fixture.config.MaterialRoot+" "+testAuthorizationID,
		"credential.useHttpPath=true", "protocol.allow=never")
}

// TestMirrorPrepareDialsTheSpellingTheDeploymentSupplied covers the one difference between the address a
// fetch dials and the identity the authorization binds.
//
// `CanonicalRepository` folds the decorations a forge adds to one repository — a trailing `.git`, a
// trailing slash — into a single identity, which is what the credential's scope and the mirror's key
// need. A Git service is not obliged to answer both spellings: `git http-backend` serves `/repo.git` and
// 404s `/repo`, so a preparation that dialed the canonical identity would ask for a path that does not
// exist while holding a correct authorization. Measured against the private acceptance upstream, where
// it answered `remote: 404 page not found` and the operation failed with
// `credential_executor_fetch_failed`.
func TestMirrorPrepareDialsTheSpellingTheDeploymentSupplied(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	deployed := testRepository + ".git"
	job := fixture.issue(t, nil, func(j *MirrorJob) { j.Repository = deployed })

	result, err := fixture.executor.Prepare(context.Background(), job)
	if err != nil {
		t.Fatalf("the preparation was refused: %v", err)
	}
	// The identity stays canonical, so the mirror the preparation produced is the one the canonical
	// spelling names — which is the key the Git Server derives from the Workspace's baseline.
	if result.Repository != testRepository {
		t.Fatalf("result repository = %q, want the canonical %q", result.Repository, testRepository)
	}
	if _, err := fixture.mirrors.Open(fixture.mirrorKey()); err != nil {
		t.Fatalf("the mirror is not published under the canonical key: %v", err)
	}
	// The address is the deployment's spelling, and the canonical one was not dialed.
	assertCaptured(t, fixture.capture, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--",
		deployed, "+"+testRef+":"+mirrorpublish.MirrorRef)
	recorded, err := os.ReadFile(fixture.capture)
	if err != nil {
		t.Fatalf("read the recorded Git invocations: %v", err)
	}
	if strings.Contains(string(recorded), "\n"+testRepository+"\n") {
		t.Fatalf("the fetch dialed the canonical identity instead of the deployment's spelling:\n%s",
			recorded)
	}
}

// TestMirrorPrepareIsIdempotentForAnAlreadyPublishedCommit covers the retry Cloud will actually make.
//
// A preparation whose response was lost is retried with a new authorization and the same pinned ref.
// The second attempt fetches again — it must, because only the fetch can tell it which commit the ref
// now resolves to — and then finds the key already published. The right answer is the existing record
// and no second publication, which is what `Begin`'s already-published outcome exists for.
func TestMirrorPrepareIsIdempotentForAnAlreadyPublishedCommit(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	first, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil))
	if err != nil {
		t.Fatalf("the first preparation was refused: %v", err)
	}
	// A retry carries a new authorization: the first one was spent by the first attempt.
	second, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, func(b *cloneauth.Binding) {
		b.AuthorizationID = "0d5f8a31-6c94-4e27-b0a8-2f7c1d9e4b63"
	}, nil))
	if err != nil {
		t.Fatalf("the retried preparation was refused: %v", err)
	}
	if second.Commit != first.Commit || second.Generation != first.Generation {
		t.Fatalf("the retry published %s generation %d, want %s generation %d",
			second.Commit, second.Generation, first.Commit, first.Generation)
	}
	records, err := fixture.mirrors.Records()
	if err != nil {
		t.Fatalf("read the mirror records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("the store holds %d records after a retry, want 1", len(records))
	}
	if records[0].Generation != 1 {
		t.Fatalf("the record's generation = %d, want 1: a retry must not publish a second copy", records[0].Generation)
	}
	// The retry redeemed its own authorization, so the spend count is two — the point is that the store
	// was not written twice.
	if fixture.store.calls != 2 {
		t.Fatalf("redemptions = %d, want 2", fixture.store.calls)
	}
}

// TestMirrorPrepareDestroysTheMaterialWhenTheFetchFails is the failure path the ordering is for.
//
// The authorization is spent by the time the fetch runs, so the fetch's failure is this attempt's
// outcome and nothing retries. What must not happen is the credential outliving it: the token was
// materialized before the fetch, and a preparation that failed after that point still has to have
// destroyed it.
func TestMirrorPrepareDestroysTheMaterialWhenTheFetchFails(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	if err := os.WriteFile(fixture.failFetch, nil, 0o600); err != nil {
		t.Fatalf("arm the fetch failure: %v", err)
	}
	_, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil))
	if got := reasonCode(err); got != ReasonCodeFetchFailed {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, ReasonCodeFetchFailed, err)
	}
	if fixture.store.calls != 1 {
		t.Fatalf("redemptions = %d, want 1: the failure happens after the spend", fixture.store.calls)
	}
	assertNoTokenUnder(t, fixture.config.MaterialRoot, testToken)
	entries, err := os.ReadDir(fixture.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root holds %d entries after a failed fetch", len(entries))
	}
	if records, err := fixture.mirrors.Records(); err != nil {
		t.Fatalf("read the mirror records: %v", err)
	} else if len(records) != 0 {
		t.Fatalf("a failed fetch left %d mirror records", len(records))
	}
	fetches, err := os.ReadDir(fixture.executor.fetchRoot)
	if err != nil {
		t.Fatalf("read the fetch root: %v", err)
	}
	if len(fetches) != 0 {
		t.Fatalf("the fetch root holds %d entries after a failed fetch", len(fetches))
	}
}

// TestMirrorPrepareRefusesWhenTheStoreRefusesTheRedemption checks that a Store refusal is reported as
// a redemption failure rather than as a fetch or a publication.
//
// The code matters because Cloud's action differs: a refused redemption is a credential problem that a
// new authorization may or may not fix, while a failed fetch is an upstream problem. Collapsing them
// would leave Cloud retrying the wrong thing.
func TestMirrorPrepareRefusesWhenTheStoreRefusesTheRedemption(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	fixture.store.err = errors.New("the credential service is unreachable")
	_, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil))
	if got := reasonCode(err); got != ReasonCodeRedemptionFailed {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, ReasonCodeRedemptionFailed, err)
	}
	// The Store was reached and declined, so the material was never staged and there is nothing to
	// destroy.
	entries, err := os.ReadDir(fixture.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root holds %d entries after a refused redemption", len(entries))
	}
}

// TestMirrorSweepRemovesAbandonedFetches covers the one repair a start can make: a fetch directory left
// by a process that died between creating it and moving it into the store.
//
// It asserts the store is untouched at the same time, because the sweep's safety is that it only
// removes debris it owns — published content and a peer's staging directory both survive it.
func TestMirrorSweepRemovesAbandonedFetches(t *testing.T) {
	fixture := newMirrorFixture(t, nil)
	if _, err := fixture.executor.Prepare(context.Background(), fixture.issue(t, nil, nil)); err != nil {
		t.Fatalf("the preparation was refused: %v", err)
	}
	abandoned := filepath.Join(fixture.executor.fetchRoot, testOperationID+".crashed")
	if err := os.MkdirAll(abandoned, 0o700); err != nil {
		t.Fatalf("create an abandoned fetch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "partial"), []byte("half a repository"), 0o600); err != nil {
		t.Fatalf("fill the abandoned fetch: %v", err)
	}
	removed, err := fixture.executor.Sweep()
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d entries, want 1", removed)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the abandoned fetch survived the sweep: %v", err)
	}
	// The published mirror is not the sweep's business.
	if _, err := fixture.mirrors.Open(fixture.mirrorKey()); err != nil {
		t.Fatalf("the sweep disturbed the published mirror: %v", err)
	}
}

// TestMirrorArgumentsPinTheWholeConfiguration is the direct evidence for the mirror fetch's transport
// and credential constraints: the configuration validated and the configuration used are one command
// line, and the refspec is the one the authorization pinned.
func TestMirrorArgumentsPinTheWholeConfiguration(t *testing.T) {
	helper := "/usr/local/bin/git-credential-ora"
	arguments, err := mirrorArguments(helper, testRepository, testRef, gitTransport{sslCAInfo: "/etc/ora/repositories.pem"})
	if err != nil {
		t.Fatalf("build the mirror arguments: %v", err)
	}
	joined := strings.Join(arguments, "\n")
	for _, required := range []string{
		"protocol.allow=never",
		"protocol.https.allow=always",
		"protocol.ssh.allow=never",
		"protocol.file.allow=never",
		"protocol.ext.allow=never",
		"core.hooksPath=/dev/null",
		"core.askPass=",
		"core.symlinks=false",
		"credential.helper=" + helper,
		"credential.useHttpPath=true",
		"http.followRedirects=false",
		"http.proxy=",
		"http.sslVerify=true",
		"http.sslCAInfo=/etc/ora/repositories.pem",
		"submodule.recurse=false",
		"fetch.recurseSubmodules=false",
		"filter.lfs.smudge=",
		"filter.lfs.process=",
		"filter.lfs.required=false",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("the mirror arguments do not pin %q", required)
		}
	}
	// The two settings that keep the result to exactly one ref, and the separator that stops a
	// repository reference being read as an option.
	if !strings.Contains(joined, "fetch\n--quiet\n--no-tags\n--no-write-fetch-head\n--\n") {
		t.Fatalf("the fetch command is not the pinned one: %s", joined)
	}
	if last := arguments[len(arguments)-1]; last != "+"+testRef+":"+mirrorpublish.MirrorRef {
		t.Fatalf("refspec = %q, want %q", last, "+"+testRef+":"+mirrorpublish.MirrorRef)
	}
}

// TestMirrorArgumentsRefuseAnUnusableTarget covers the values that must never reach a command line:
// a target that is not https, one carrying whitespace, and a ref that is not a branch.
func TestMirrorArgumentsRefuseAnUnusableTarget(t *testing.T) {
	helper := "/usr/local/bin/git-credential-ora"
	cases := []struct {
		name       string
		repository string
		ref        string
	}{
		{"not a url", "not-a-url", testRef},
		{"plain http", "http://git.example.com/ora/private-repo", testRef},
		{"ssh", "ssh://git@git.example.com/ora/private-repo", testRef},
		{"whitespace in the target", "https://git.example.com/ora/private repo", testRef},
		{"a tag ref", "https://git.example.com/ora/private-repo", "refs/tags/v1"},
		{"a short ref", "https://git.example.com/ora/private-repo", "main"},
		{"a ref with a revision expression", "https://git.example.com/ora/private-repo", "refs/heads/../main"},
		{"a ref with a metacharacter", "https://git.example.com/ora/private-repo", "refs/heads/main~1"},
		{"a ref that is a lock file", "https://git.example.com/ora/private-repo", "refs/heads/main.lock"},
		{"a bare branch prefix", "https://git.example.com/ora/private-repo", "refs/heads/"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := mirrorArguments(helper, testCase.repository, testCase.ref, gitTransport{}); err == nil {
				t.Fatal("an unusable target was accepted")
			}
		})
	}
	// The controls: the shapes the route does accept.
	for _, accepted := range []string{"refs/heads/main", "refs/heads/release/2026-10", "refs/heads/feature.x"} {
		if _, err := mirrorArguments(helper, testRepository, accepted, gitTransport{}); err != nil {
			t.Fatalf("ref %q was refused: %v", accepted, err)
		}
	}
}

// assertNoTokenUnder walks a directory tree and proves one value appears nowhere in it.
//
// It is this package's own copy of the acceptance suite's assertion: that file is an external test
// package, so the helper there is not importable, and the property — a credential the executor staged
// is gone rather than merely unlinked — is one this package's tests have to state in their own words.
func assertNoTokenUnder(t *testing.T, root, token string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), token) {
			return fmt.Errorf("the credential is still readable at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertCaptured checks that the Git double recorded every one of these values in one argument list.
func assertCaptured(t *testing.T, capture string, want ...string) {
	t.Helper()
	recorded, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read the recorded Git invocations: %v", err)
	}
	var invocation []string
	found := false
	for _, line := range strings.Split(strings.TrimRight(string(recorded), "\n"), "\n") {
		if line == "" {
			// The double prints one argument per line, so a blank line separates two invocations.
			if containsAll(invocation, want) {
				found = true
				break
			}
			invocation = nil
			continue
		}
		invocation = append(invocation, line)
	}
	if !found && !containsAll(invocation, want) {
		t.Fatalf("no recorded Git invocation carried %v; recorded:\n%s", want, recorded)
	}
}

// containsAll reports whether every wanted value is present in the invocation.
func containsAll(invocation, want []string) bool {
	present := make(map[string]bool, len(invocation))
	for _, argument := range invocation {
		present[argument] = true
	}
	for _, wanted := range want {
		if !present[wanted] {
			return false
		}
	}
	return true
}
