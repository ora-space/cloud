package cloneexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
)

// The identifiers used throughout these tests are canonical lowercase UUIDs because that is what the
// materialization layout accepts as a path segment; a fixture that used a friendlier spelling would
// test a path this package never takes.
const (
	testExecutionID  = "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31"
	testCredentialID = "2c9f4e18-7a35-4d61-8b02-5e7c1a9d3f40"
	testNodeID       = "1b7c9d02-4e5a-4f83-9c16-8d2a6b0e7f31"
	testNodeInstance = "9d3e5a17-2c84-4b60-a1f7-6e0b8c4d2a95"
	testNodeIncarn   = "4a6f8c20-1d73-4e59-b8a4-3c7d9e1f5b62"
	testRepository   = "https://git.example.com/ora/private-repo"
	testCommit       = "5c1e7a9b3d0f2e4a6c8b0d2f4a6c8e0b2d4f6a8c"
)

// testToken is the secret the fake Store delivers. It is generated per test run rather than fixed, so
// a test that accidentally printed it would print a value that is meaningless outside that run.
var testToken = "test-token-" + fmt.Sprint(time.Now().UnixNano())

var testNow = time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)

// recordingStore is a RedemptionClient that records what it was asked and answers what the test told
// it to. It exists so the ordering this package promises — verify locally, then spend — can be
// asserted directly: a test that expects a refusal before redemption checks that this was never
// called.
type recordingStore struct {
	credential Credential
	err        error
	calls      int
	gotID      string
	gotAuth    string
	gotRuntime Runtime
}

func (s *recordingStore) Resolve(_ context.Context, credentialID, authorization string, runtime Runtime) (Credential, error) {
	s.calls++
	s.gotID = credentialID
	s.gotAuth = authorization
	s.gotRuntime = runtime
	if s.err != nil {
		return Credential{}, s.err
	}
	return s.credential, nil
}

// testFixture is one ready executor with its own temporary deployment.
type testFixture struct {
	executor *Executor
	config   Config
	key      ed25519.PrivateKey
	store    *recordingStore
	dir      string
	// capture is the file the Git double appends its argv to.
	capture string
}

// newTestFixture builds a deployment in a temporary directory.
//
// The Git binary is a test double rather than real Git, because these tests are about the order in
// which this package does things — verify, spend, materialize, clone, destroy — and about what it
// refuses. That a real Git completes a real private clone through the real helper is the acceptance
// test's job, and it uses the real binary for exactly that reason.
func newTestFixture(t *testing.T, adjust func(*Config)) *testFixture {
	t.Helper()
	dir := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the deployment's authorization key: %v", err)
	}
	store := &recordingStore{credential: Credential{Token: testToken, Version: 3}}
	fixture := &testFixture{
		config: Config{
			MaterialRoot:     filepath.Join(dir, "material"),
			CheckoutRoot:     filepath.Join(dir, "checkout"),
			HomeDirectory:    filepath.Join(dir, "home"),
			HelperPath:       filepath.Join(dir, "git-credential-ora"),
			GitPath:          filepath.Join(dir, "git"),
			VerificationKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)},
			Store:            store,
			Timeout:          time.Minute,
			Now:              func() time.Time { return testNow },
			Log:              zap.NewNop(),
		},
		key:     key,
		store:   store,
		dir:     dir,
		capture: filepath.Join(dir, "git-invocations"),
	}
	if adjust != nil {
		adjust(&fixture.config)
	}
	// The deployment creates the materialization root, the checkout root and the home and owns them;
	// this package only writes inside them. The home is one of them because `New` refuses a deployment
	// whose home is missing or not 0700 — Git reads it as `HOME`.
	for _, root := range []string{fixture.config.MaterialRoot, fixture.config.CheckoutRoot, fixture.config.HomeDirectory} {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("create %s: %v", root, err)
		}
	}
	// A placeholder so a configuration check that insists on the helper existing would still pass; the
	// double never invokes it.
	if err := os.WriteFile(fixture.config.HelperPath, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("create the helper placeholder: %v", err)
	}
	writeGitDouble(t, fixture)
	executor, err := New(fixture.config)
	if err != nil {
		t.Fatalf("build the executor: %v", err)
	}
	fixture.executor = executor
	return fixture
}

// writeGitDouble writes the Git test double.
//
// The script is generated rather than checked in because every path it needs is the fixture's, and it
// records its own argv so a test can assert what this package actually executed rather than what it
// intended to.
func writeGitDouble(t *testing.T, fixture *testFixture) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
set -eu
printf '%%s\n' "$@" >> %[1]s
mode=other
for argument in "$@"; do
  case "$argument" in
    clone) mode=clone ;;
    rev-parse) mode=revparse ;;
  esac
done
if [ "$mode" = revparse ]; then
  printf '%%s\n' %[2]s
  exit 0
fi
if [ -f %[3]s ]; then
  printf '%%s\n' 'fatal: could not read from remote repository' >&2
  exit 128
fi
if [ -f %[4]s ]; then
  # Fault injection: the clone step breaks the credential material's cleanup. The executor must
  # report that as a failure rather than as a successful clone.
  chmod 0500 %[5]s
fi
target=
for argument in "$@"; do target="$argument"; done
mkdir -p "$target"
exit 0
`, shellQuote(fixture.capture),
		shellQuote(testCommit),
		shellQuote(filepath.Join(fixture.dir, "fail-clone")),
		shellQuote(filepath.Join(fixture.dir, "break-cleanup")),
		shellQuote(fixture.config.MaterialRoot))
	if err := os.WriteFile(fixture.config.GitPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write the git double: %v", err)
	}
}

// shellQuote single-quotes one path for the generated script. Every path here is a temporary
// directory this test created, so the quoting is for correctness rather than for safety.
func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// issue returns a job together with the authorization Cloud would have signed for it.
//
// `adjust` is applied to the binding *after* the job has been derived from it, so a case that adjusts
// the binding describes exactly the situation it means to: a job whose own claims and whose signed
// authority disagree. Deriving the job first is what makes the two independent — deriving it after
// would make every such case agree with itself and test nothing.
func (f *testFixture) issue(t *testing.T, adjust func(*cloneauth.Binding)) Job {
	t.Helper()
	binding := cloneauth.Binding{
		AuthorizationID:   "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46",
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
	job := Job{
		ExecutionID:       binding.ExecutionID,
		Repository:        binding.Repository,
		Capabilities:      binding.Capabilities,
		CredentialID:      binding.CredentialID,
		CredentialVersion: binding.CredentialVersion,
		Runtime: Runtime{
			NodeID:            binding.NodeID,
			NodeInstanceID:    binding.NodeInstanceID,
			NodeIncarnationID: binding.NodeIncarnationID,
			RuntimeGeneration: binding.RuntimeGeneration,
			ControlEpoch:      binding.ControlEpoch,
		},
	}
	if adjust != nil {
		adjust(&binding)
	}
	authorization, err := cloneauth.Issue(f.key, binding)
	if err != nil {
		t.Fatalf("issue the test authorization: %v", err)
	}
	job.Authorization = authorization
	return job
}

// reasonCode returns the stable code of a refusal, or the empty string for anything else.
func reasonCode(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func TestNewRefusesADeploymentThatCannotEstablishTheBoundary(t *testing.T) {
	blank := func(*Config) {}
	cases := []struct {
		name   string
		adjust func(*Config)
		accept bool
	}{
		{"no materialization root", func(c *Config) { c.MaterialRoot = "" }, false},
		{"relative materialization root", func(c *Config) { c.MaterialRoot = "material" }, false},
		{"no checkout root", func(c *Config) { c.CheckoutRoot = "" }, false},
		{"no home directory", func(c *Config) { c.HomeDirectory = "" }, false},
		{"a home directory that does not exist", func(c *Config) { c.HomeDirectory = filepath.Join(c.MaterialRoot, "absent") }, false},
		{"a home directory that is not a directory", func(c *Config) { c.HomeDirectory = c.HelperPath }, false},
		{"a home directory another identity could write", func(c *Config) {
			// Git reads this directory as `HOME`, so a home another account can write to is a
			// `.gitconfig` that account can plant.
			if err := os.Chmod(c.HomeDirectory, 0o755); err != nil {
				t.Fatalf("widen the home directory: %v", err)
			}
		}, false},
		{"no helper", func(c *Config) { c.HelperPath = "" }, false},
		{"relative helper", func(c *Config) { c.HelperPath = "git-credential-ora" }, false},
		{"helper path with whitespace", func(c *Config) { c.HelperPath = "/usr/local/bin/git credential ora" }, false},
		{"no git", func(c *Config) { c.GitPath = "" }, false},
		{"no verification key", func(c *Config) { c.VerificationKeys = nil }, false},
		{"verification key of the wrong size", func(c *Config) { c.VerificationKeys = []ed25519.PublicKey{make([]byte, 8)} }, false},
		{"no credential service", func(c *Config) { c.Store = nil }, false},
		{"materialization root is the checkout root", func(c *Config) { c.CheckoutRoot = c.MaterialRoot }, false},
		{"certificate authority file is not a path", func(c *Config) { c.SSLCAInfo = "/etc/ora/ca bundle.pem" }, false},
		{"a complete deployment", blank, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatalf("generate a key: %v", err)
			}
			config := Config{
				MaterialRoot:     filepath.Join(dir, "material"),
				CheckoutRoot:     filepath.Join(dir, "checkout"),
				HomeDirectory:    filepath.Join(dir, "home"),
				HelperPath:       filepath.Join(dir, "helper"),
				GitPath:          filepath.Join(dir, "git"),
				VerificationKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)},
				Store:            &recordingStore{},
			}
			// The deployment creates the three roots before the executor starts, so the cases above are
			// exercised against a deployment that is complete in every other respect.
			for _, root := range []string{config.MaterialRoot, config.CheckoutRoot, config.HomeDirectory} {
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatalf("create %s: %v", root, err)
				}
			}
			if testCase.adjust != nil {
				testCase.adjust(&config)
			}
			executor, err := New(config)
			if testCase.accept {
				if err != nil {
					t.Fatalf("a complete deployment was refused: %v", err)
				}
				if executor == nil {
					t.Fatal("a complete deployment produced no executor")
				}
				// Zero values are resolved rather than left to the caller: a deployment that configured
				// no timeout gets the default, not an instantly-expiring context.
				if executor.config.Timeout != DefaultCloneTimeout {
					t.Fatalf("default timeout = %s, want %s", executor.config.Timeout, DefaultCloneTimeout)
				}
				return
			}
			if err == nil {
				t.Fatalf("an incomplete deployment was accepted")
			}
			if executor != nil {
				t.Fatalf("an incomplete deployment produced an executor")
			}
		})
	}
}

// TestCloneArgumentsPinTheWholeConfiguration is the direct evidence for the transport, credential and
// egress constraints: the configuration validated and the configuration used are one command line.
func TestCloneArgumentsPinTheWholeConfiguration(t *testing.T) {
	helper := "/usr/local/bin/git-credential-ora"
	root := "/run/ora/credentials"
	target := "/var/lib/ora/clones/" + testExecutionID
	expected := func(extra ...string) []string {
		arguments := []string{
			"-c", "protocol.allow=never",
			"-c", "protocol.https.allow=always",
			"-c", "protocol.ssh.allow=never",
			"-c", "protocol.file.allow=never",
			"-c", "protocol.ext.allow=never",
			"-c", "core.hooksPath=/dev/null",
			"-c", "core.askPass=",
			"-c", "core.symlinks=false",
			"-c", "credential.helper=",
			"-c", "credential.helper=" + helper + " " + root + " " + testExecutionID,
			"-c", "credential.useHttpPath=true",
			"-c", "http.followRedirects=false",
			"-c", "http.proxy=",
			"-c", "http.sslVerify=true",
			"-c", "submodule.recurse=false",
			"-c", "fetch.recurseSubmodules=false",
			"-c", "filter.lfs.smudge=",
			"-c", "filter.lfs.process=",
			"-c", "filter.lfs.required=false",
		}
		arguments = append(arguments, extra...)
		return arguments
	}
	cases := []struct {
		name   string
		job    Job
		ca     string
		expect []string
	}{
		{
			name:   "the default branch",
			job:    Job{ExecutionID: testExecutionID, Repository: testRepository},
			expect: expected("clone", "--single-branch", "--no-tags", "--", testRepository, target),
		},
		{
			name:   "one named branch",
			job:    Job{ExecutionID: testExecutionID, Repository: testRepository, Branch: "release/2026.10"},
			expect: expected("clone", "--single-branch", "--no-tags", "--branch", "release/2026.10", "--", testRepository, target),
		},
		{
			name:   "a deployment certificate authority",
			job:    Job{ExecutionID: testExecutionID, Repository: testRepository},
			ca:     "/etc/ora/git-ca.pem",
			expect: expected("-c", "http.sslCAInfo=/etc/ora/git-ca.pem", "clone", "--single-branch", "--no-tags", "--", testRepository, target),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			arguments, err := cloneArguments(&testCase.job, helper, root, target, testCase.ca)
			if err != nil {
				t.Fatalf("cloneArguments refused a valid job: %v", err)
			}
			if !reflect.DeepEqual(arguments, testCase.expect) {
				t.Fatalf("clone arguments =\n%v\nwant\n%v", arguments, testCase.expect)
			}
		})
	}
}

func TestCloneArgumentsRefuseAJobGitWouldRunSomethingElse(t *testing.T) {
	const (
		helper = "/usr/local/bin/git-credential-ora"
		root   = "/run/ora/credentials"
		target = "/var/lib/ora/clones/" + testExecutionID
	)
	cases := []struct {
		name   string
		job    Job
		target string
	}{
		{"a branch that reads as an option", Job{ExecutionID: testExecutionID, Repository: testRepository, Branch: "--upload-pack=/bin/sh"}, target},
		{"a branch outside Git's ref rules", Job{ExecutionID: testExecutionID, Repository: testRepository, Branch: "release..2026"}, target},
		{"a branch ending in a lock suffix", Job{ExecutionID: testExecutionID, Repository: testRepository, Branch: "release.lock"}, target},
		{"a repository that is not https", Job{ExecutionID: testExecutionID, Repository: "ssh://git@git.example.com/ora/private-repo"}, target},
		{"a repository that is not a URL", Job{ExecutionID: testExecutionID, Repository: "git.example.com/ora/private-repo"}, target},
		{"an execution id that is not a uuid", Job{ExecutionID: "not-a-uuid", Repository: testRepository}, target},
		{"a target with whitespace", Job{ExecutionID: testExecutionID, Repository: testRepository}, "/var/lib/ora/clones/two words"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := cloneArguments(&testCase.job, helper, root, testCase.target, ""); err == nil {
				t.Fatal("a job Git could be made to run something else for was accepted")
			}
		})
	}
}

func TestHelperArgumentRefusesAPathGitCouldSplitOrExecute(t *testing.T) {
	cases := []struct {
		name       string
		helperPath string
		root       string
		execution  string
		accept     bool
	}{
		{"an unset helper", "", "/run/ora/credentials", testExecutionID, false},
		{"a relative helper", "bin/helper", "/run/ora/credentials", testExecutionID, false},
		{"a helper with a space", "/usr/local/bin/git credential", "/run/ora/credentials", testExecutionID, false},
		{"a helper with a command substitution", "/usr/local/bin/$(id)", "/run/ora/credentials", testExecutionID, false},
		{"a helper with a backtick", "/usr/local/bin/`id`", "/run/ora/credentials", testExecutionID, false},
		{"a root with a semicolon", "/usr/local/bin/helper", "/run/ora/cred;entials", testExecutionID, false},
		{"a root that is not absolute", "/usr/local/bin/helper", "run/ora/credentials", testExecutionID, false},
		{"an execution id with a path separator", "/usr/local/bin/helper", "/run/ora/credentials", "../../etc", false},
		{"an uppercase execution id", "/usr/local/bin/helper", "/run/ora/credentials", strings.ToUpper(testExecutionID), false},
		{"a valid deployment", "/usr/local/bin/helper", "/run/ora/credentials", testExecutionID, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			argument, err := helperArgument(testCase.helperPath, testCase.root, testCase.execution)
			if testCase.accept {
				want := testCase.helperPath + " " + testCase.root + " " + testCase.execution
				if err != nil {
					t.Fatalf("a valid deployment was refused: %v", err)
				}
				if argument != want {
					t.Fatalf("helper argument = %q, want %q", argument, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("a helper argument Git could split or execute was accepted: %q", argument)
			}
		})
	}
}

func TestRepositoryScopeMatchesWhatGitReportsToACredentialHelper(t *testing.T) {
	cases := []struct {
		repository string
		host       string
		path       string
	}{
		{"https://git.example.com/ora/private-repo", "git.example.com", "/ora/private-repo"},
		{"https://git.example.com:443/ora/private-repo", "git.example.com", "/ora/private-repo"},
		{"https://git.example.com:8443/ora/private-repo", "git.example.com:8443", "/ora/private-repo"},
		{"https://GIT.Example.COM/ora/private-repo", "git.example.com", "/ora/private-repo"},
	}
	for _, testCase := range cases {
		t.Run(testCase.repository, func(t *testing.T) {
			host, path, err := repositoryScope(testCase.repository)
			if err != nil {
				t.Fatalf("repositoryScope refused a canonical repository: %v", err)
			}
			if host != testCase.host || path != testCase.path {
				t.Fatalf("repositoryScope = (%q, %q), want (%q, %q)", host, path, testCase.host, testCase.path)
			}
			// The claim is not just that the two values are the host and the path: it is that the scope
			// the executor stages agrees with the request Git writes for the same repository, which
			// reports the path without its leading slash. Both sides go through one normalization.
			scope, err := credentialmaterial.NormalizeScope(host, path)
			if err != nil {
				t.Fatalf("normalize the staged scope: %v", err)
			}
			request, err := credentialmaterial.NormalizeScope(testCase.host, strings.TrimPrefix(testCase.path, "/"))
			if err != nil || request != scope {
				t.Fatalf("the staged scope %+v does not agree with Git's request spelling %+v (err %v)", scope, request, err)
			}
		})
	}
}

// TestExecuteRefusesBeforeSpendingAnything covers every refusal this executor can reach on its own.
// Each case asserts the Store was never asked, because the promise is that a job which cannot be
// executed consumes nothing — an authorization spent on a job that was going to be refused anyway is
// an authorization the caller cannot retry with.
func TestExecuteRefusesBeforeSpendingAnything(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		binding func(*cloneauth.Binding)
		job     func(*Job)
	}{
		{
			name: "a repository that is not https",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.Repository = "file:///etc/passwd" },
		},
		{
			name: "an execution id that is not a uuid",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.ExecutionID = "clone-1" },
		},
		{
			name: "no authorization",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.Authorization = "  " },
		},
		{
			name: "no credential",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.CredentialID = "" },
		},
		{
			name: "a job that asks for no capability at all",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.Capabilities = nil },
		},
		{
			name: "a job that asks for a capability outside the vocabulary",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.Capabilities = []string{"push"} },
		},
		{
			name: "a job that asks for the same capability twice",
			code: ReasonCodeInvalidJob,
			job:  func(job *Job) { job.Capabilities = []string{"clone", "clone"} },
		},
		{
			name: "an authorization that is not well formed",
			code: ReasonCodeAuthorizationRejected,
			job:  func(job *Job) { job.Authorization = "not-an-authorization" },
		},
		{
			name: "an authorization that has expired",
			code: ReasonCodeAuthorizationRejected,
			binding: func(binding *cloneauth.Binding) {
				binding.IssuedAt = testNow.Add(-9 * time.Minute)
				binding.ExpiresAt = testNow.Add(-time.Minute)
			},
		},
		{
			name: "an authorization for another repository",
			code: ReasonCodeBindingMismatch,
			binding: func(binding *cloneauth.Binding) {
				binding.Repository = "https://git.example.com/ora/another-repo"
			},
		},
		{
			name: "an authorization for another execution",
			code: ReasonCodeBindingMismatch,
			binding: func(binding *cloneauth.Binding) {
				binding.ExecutionID = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
			},
		},
		{
			name: "an authorization for another credential version",
			code: ReasonCodeBindingMismatch,
			binding: func(binding *cloneauth.Binding) {
				binding.CredentialVersion = 4
			},
		},
		{
			name: "an authorization for another node incarnation",
			code: ReasonCodeBindingMismatch,
			binding: func(binding *cloneauth.Binding) {
				binding.NodeIncarnationID = "ffffffff-1111-4222-8333-444444444444"
			},
		},
		{
			name: "an authorization for another runtime generation",
			code: ReasonCodeBindingMismatch,
			binding: func(binding *cloneauth.Binding) {
				binding.RuntimeGeneration = 8
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newTestFixture(t, nil)
			job := fixture.issue(t, testCase.binding)
			if testCase.job != nil {
				testCase.job(&job)
			}
			_, err := fixture.executor.Execute(context.Background(), job)
			if got := reasonCode(err); got != testCase.code {
				t.Fatalf("reason code = %q, want %q (error: %v)", got, testCase.code, err)
			}
			if fixture.store.calls != 0 {
				t.Fatalf("the credential service was asked %d times for a job refused before redemption", fixture.store.calls)
			}
			assertNoMaterial(t, fixture)
		})
	}
}

func TestExecuteRefusesAnAuthorizationSignedByAnotherKey(t *testing.T) {
	fixture := newTestFixture(t, nil)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a second key: %v", err)
	}
	job := fixture.issue(t, nil)
	forged, err := cloneauth.Issue(other, cloneauth.Binding{
		AuthorizationID:   "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46",
		Operation:         cloneauth.OperationClone,
		TenantID:          "tenant-b",
		OwnerUserID:       "user-b",
		ProjectID:         "project-b",
		WorkspaceID:       "workspace-b",
		ExecutionID:       testExecutionID,
		CredentialID:      testCredentialID,
		CredentialVersion: 3,
		Repository:        testRepository,
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		NodeID:            testNodeID,
		NodeInstanceID:    testNodeInstance,
		NodeIncarnationID: testNodeIncarn,
		RuntimeGeneration: 7,
		IssuedAt:          testNow.Add(-time.Minute),
		ExpiresAt:         testNow.Add(9 * time.Minute),
	})
	if err != nil {
		t.Fatalf("sign the forged authorization: %v", err)
	}
	job.Authorization = forged
	_, err = fixture.executor.Execute(context.Background(), job)
	if got := reasonCode(err); got != ReasonCodeAuthorizationRejected {
		t.Fatalf("reason code = %q, want %q", got, ReasonCodeAuthorizationRejected)
	}
	if fixture.store.calls != 0 {
		t.Fatalf("the credential service was asked %d times for a forged authorization", fixture.store.calls)
	}
}

func TestExecuteReportsARedemptionRefusalWithTheStoreCode(t *testing.T) {
	fixture := newTestFixture(t, nil)
	fixture.store.err = &StoreRefusal{Code: ReasonCodeAuthorizationConsumed, Status: 409}
	job := fixture.issue(t, nil)
	_, err := fixture.executor.Execute(context.Background(), job)
	if got := reasonCode(err); got != ReasonCodeRedemptionFailed {
		t.Fatalf("reason code = %q, want %q", got, ReasonCodeRedemptionFailed)
	}
	var refusal *StoreRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the Store's refusal is not in the cause chain: %v", err)
	}
	if refusal.ReasonCode() != ReasonCodeAuthorizationConsumed {
		t.Fatalf("the Store's code was replaced: %q", refusal.ReasonCode())
	}
	if fixture.store.calls != 1 {
		t.Fatalf("the credential service was asked %d times, want exactly 1", fixture.store.calls)
	}
	assertNoMaterial(t, fixture)
}

func TestExecuteClonesAndThenProvesTheCredentialGone(t *testing.T) {
	fixture := newTestFixture(t, nil)
	job := fixture.issue(t, nil)
	result, err := fixture.executor.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("a permitted clone was refused: %v", err)
	}
	if result.Commit != testCommit {
		t.Fatalf("commit = %q, want %q", result.Commit, testCommit)
	}
	if result.CredentialVersion != 3 {
		t.Fatalf("credential version = %d, want 3", result.CredentialVersion)
	}
	// The checkout is the handoff, and it is where the executor said it would be rather than where the
	// job asked.
	want := filepath.Join(fixture.config.CheckoutRoot, testExecutionID)
	if result.Directory != want {
		t.Fatalf("checkout = %q, want %q", result.Directory, want)
	}
	if info, err := os.Stat(result.Directory); err != nil || !info.IsDir() {
		t.Fatalf("the checkout was not created: %v", err)
	}
	if fixture.store.calls != 1 {
		t.Fatalf("the credential service was asked %d times, want exactly 1", fixture.store.calls)
	}
	if fixture.store.gotID != testCredentialID {
		t.Fatalf("redemption addressed %q, want %q", fixture.store.gotID, testCredentialID)
	}
	if fixture.store.gotAuth != job.Authorization {
		t.Fatal("redemption did not present the job's authorization")
	}
	if fixture.store.gotRuntime != job.Runtime {
		t.Fatalf("redemption declared runtime %+v, want %+v", fixture.store.gotRuntime, job.Runtime)
	}
	// The clone really ran, with the deployment's own helper named as the only one.
	invocations, err := os.ReadFile(fixture.capture)
	if err != nil {
		t.Fatalf("read the recorded Git invocations: %v", err)
	}
	recorded := string(invocations)
	if !strings.Contains(recorded, "credential.helper="+fixture.config.HelperPath+" "+fixture.config.MaterialRoot+" "+testExecutionID) {
		t.Fatalf("Git was not given the deployment's helper for this execution:\n%s", recorded)
	}
	if !strings.Contains(recorded, "rev-parse") {
		t.Fatalf("the cloned revision was never read:\n%s", recorded)
	}
	assertNoMaterial(t, fixture)
	assertTokenGone(t, fixture)
}

func TestExecuteDestroysTheMaterialWhenTheCloneFails(t *testing.T) {
	fixture := newTestFixture(t, nil)
	if err := os.WriteFile(filepath.Join(fixture.dir, "fail-clone"), nil, 0o600); err != nil {
		t.Fatalf("arm the clone failure: %v", err)
	}
	job := fixture.issue(t, nil)
	_, err := fixture.executor.Execute(context.Background(), job)
	if got := reasonCode(err); got != ReasonCodeCloneFailed {
		t.Fatalf("reason code = %q, want %q (error: %v)", got, ReasonCodeCloneFailed, err)
	}
	// A failed clone must not leave a partial tree in the checkout root either.
	if entries, err := os.ReadDir(fixture.config.CheckoutRoot); err == nil && len(entries) != 0 {
		t.Fatalf("a failed clone left %d entries in the checkout root", len(entries))
	}
	assertNoMaterial(t, fixture)
	assertTokenGone(t, fixture)
}

func TestExecuteReportsACleanupFailureRatherThanASuccess(t *testing.T) {
	if os.Geteuid() == 0 {
		// The fault this test injects is a directory the credential-executing identity cannot remove,
		// and root can remove anything; the case would pass for the wrong reason.
		t.Skip("the cleanup fault cannot be injected as root")
	}
	fixture := newTestFixture(t, nil)
	if err := os.WriteFile(filepath.Join(fixture.dir, "break-cleanup"), nil, 0o600); err != nil {
		t.Fatalf("arm the cleanup fault: %v", err)
	}
	t.Cleanup(func() { os.Chmod(fixture.config.MaterialRoot, 0o700) })
	job := fixture.issue(t, nil)
	result, err := fixture.executor.Execute(context.Background(), job)
	if got := reasonCode(err); got != ReasonCodeCleanupFailed {
		t.Fatalf("reason code = %q, want %q (error: %v)", got, ReasonCodeCleanupFailed, err)
	}
	// The clone itself succeeded, and the result is still withheld: a clone whose credential may still
	// be readable is not a clone the caller may treat as done.
	if result.Directory != "" || result.Commit != "" || result.CredentialVersion != 0 {
		t.Fatalf("a clone with unproven cleanup returned a result: %+v", result)
	}
}

func TestSweepRemovesMaterialLeftByAPreviousRun(t *testing.T) {
	fixture := newTestFixture(t, nil)
	for _, executionID := range []string{testExecutionID, "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"} {
		if err := credentialmaterial.Stage(fixture.config.MaterialRoot, executionID, testToken, "git.example.com", "/ora/private-repo"); err != nil {
			t.Fatalf("stage material for %s: %v", executionID, err)
		}
	}
	removed, err := fixture.executor.Sweep()
	if err != nil {
		t.Fatalf("sweep the materialization root: %v", err)
	}
	if removed != 2 {
		t.Fatalf("swept %d entries, want 2", removed)
	}
	entries, err := os.ReadDir(fixture.config.MaterialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the sweep left %d entries behind", len(entries))
	}
	assertTokenGone(t, fixture)
}

// assertNoMaterial proves the execution's material directory is gone.
func assertNoMaterial(t *testing.T, fixture *testFixture) {
	t.Helper()
	directory, err := credentialmaterial.ExecutionDirectory(fixture.config.MaterialRoot, testExecutionID)
	if err != nil {
		t.Fatalf("resolve the execution directory: %v", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential material is still present at %s (error: %v)", directory, err)
	}
}

// assertTokenGone walks everything the executor wrote and proves the token is nowhere in it. A
// directory that was removed is not by itself proof that the secret is: the check is over the whole
// tree, so a copy left anywhere under the executor's roots fails the test.
func assertTokenGone(t *testing.T, fixture *testFixture) {
	t.Helper()
	for _, root := range []string{fixture.config.MaterialRoot, fixture.config.CheckoutRoot, fixture.config.HomeDirectory} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
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
			if strings.Contains(string(raw), testToken) {
				return fmt.Errorf("the token is still readable at %s", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
