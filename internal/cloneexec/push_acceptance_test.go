// This file is the acceptance test for the controlled-push boundary, and like the credentialed-clone
// acceptance test next to it, it is built out of real components: a real private HTTPS Git repository
// served by `git-http-backend` behind HTTP Basic authentication, a real Credential Service with a real
// volume and a real key-encryption key, real mutually authenticated TLS between all three parties, the
// real `git-credential-ora` helper binary, and — for the closed loop — the real `core.PushDispatcher`
// that Cloud runs.
//
// A double would prove that the executor calls the things it calls. Only the real chain proves the
// obligations the decisions actually state for a write: that an authorization Cloud minted for one
// approval is spent exactly once, reaches Git through the deployment-fixed helper, updates exactly one
// ref of exactly one upstream and only if that ref still holds the authorized old commit, and is gone
// afterwards. `20261012-push-execution-authority` D3, D4, D6 and D7 and
// `20261013-push-capability-vocabulary-extension` D5 and D8 require exactly that before a push may be
// called established.
//
// The evidence for "exactly one write" is deliberately not the executor's own report. The upstream in
// these tests carries a `pre-receive` hook that writes Git's own `<old> <new> <ref>` lines to a file and
// then either allows or refuses the update, so what a test asserts about a write is what the server was
// asked to do — from the server's side, where the executor cannot reach. The request counter on the
// repository server plays the same role for the read-only reconciliation: a read-back after a refusal
// is visible as a second upload-pack exchange, and a second *write* would be visible as a second
// receive-pack exchange.
//
// What this file cannot establish is the operating-system separation between the workload identity and
// the credential-executing identity, for the reason the clone acceptance test gives: the whole chain
// runs inside one test process under one uid, and that separation is a deployment property proven by
// the container-level acceptance run.
package cloneexec_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/core"
)

// The identifiers one push in these tests is about. They are canonical lowercase UUIDs because the
// Store and the executor both turn them into path segments, and they are the ones Cloud's own rows
// would carry: the authorization id names the approval, the operation id is the approval's identity.
const (
	pushCredentialID    = "3d5f7a91-2c48-4e60-b7a3-9f1c5e8d2b04"
	pushAuthorizationID = "5a7c9e13-4d26-4f80-9b51-2e8a0c6f4d37"
	pushOperationID     = "8e0a2c46-7f13-4b95-8d27-1a3c5e7f9b02"
	pushSecondAuthID    = "6b8d0f24-5e37-4a19-8c62-3f9b1d7e5a48"
	pushSecondOperID    = "9f1b3d57-8a24-4c06-9e38-2b4d6f8a0c13"
	pushTenantID        = "1f3d5b79-0a24-4c68-9e51-7b2d4f6a8c03"
	pushOwnerUserID     = "2a4c6e80-1b35-4d79-8f62-9c3e5a7b1d04"
	pushProjectID       = "3b5d7f91-2c46-4e8a-9b73-1d4f6a8c2e05"
	pushWorkspaceID     = "4c6e80a2-3d57-4f9b-8c84-2e5a7b9d3f06"
)

// The one ref a push in these tests updates, and the branch the upstream already carries the authorized
// new commit on. The second is not decoration: the executor's only content channel is the authorized
// upstream itself, so a commit that this repository does not already hold cannot be published to it.
const (
	pushRef        = "refs/heads/main"
	pushStagingRef = "refs/heads/staging"
)

// pushUnrelatedCommit is a well-formed object name that no repository in these tests holds. It is used
// where a value has to be *valid* and *not the authorized one*: the refusal must come from the binding
// comparison rather than from a malformed job, and no test wants the executor to fetch anything to find
// that out.
const pushUnrelatedCommit = "0123456789abcdef0123456789abcdef01234567"

// storePurposeRefusalCode is what the Credential Service answers when an authorization is presented to a
// purpose it was not minted for. The executor has no constant for it, and does not need one: C never
// presents a foreign purpose on purpose, so the code reaches a test only by driving the Store's route
// directly, which is what the test below does. Pinning the Store's own spelling here is the point.
const storePurposeRefusalCode = "authorization_purpose_mismatch"

// repositoryRequests counts what authenticated callers asked the private repository server to do, by
// smart-HTTP service.
//
// The two counts are the two questions a push's evidence turns on. `receive` is how many times a client
// opened a write: a second push under one authorization would show up here, and a reconciliation would
// not. `upload` is how many times a client opened a read: a fetch is one, and a read-only ref
// reconciliation after a refusal is another, so `upload - <fetches this attempt made>` is the number of
// read-backs — which is what makes "the reconciliation is a read and not a second write" observable at
// all.
type repositoryRequests struct {
	mutex   sync.Mutex
	receive int
	upload  int
}

// count records one authenticated smart-HTTP request.
//
// The classification is by service path rather than by the `service` query alone, because the
// advertisement request and the command request are different events: `GET /info/refs?service=X` is an
// advertisement, while `POST /git-receive-pack` is a client actually sending updates.
func (c *repositoryRequests) count(r *http.Request) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if r.Method != http.MethodPost {
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/git-receive-pack"):
		c.receive++
	case strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		c.upload++
	}
}

// snapshot returns the two counts.
func (c *repositoryRequests) snapshot() (receive, upload int) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.receive, c.upload
}

// pushRepository is the isolated upstream a controlled push writes to: the private repository plus the
// two commits the scenario needs and a hook that records what the server was asked to apply.
type pushRepository struct {
	*privateRepository
	// Work is a local clone of the bare repository. It stands in for whoever operates the upstream: it
	// is how a test creates a commit the repository already carries, and how it moves a ref behind the
	// executor's back, which are the two things only the remote's own operator can do.
	Work string
	// Old is the commit refs/heads/main holds when the scene is built.
	Old string
	// New is a descendant of Old that the upstream already carries on `staging`: the commit a push is
	// authorized to publish to main.
	New string
	// hookLog is where the repository's `pre-receive` hook records each invocation.
	hookLog string
}

// newPushRepository builds one writable upstream.
//
// Compared with the clone acceptance test's repository it adds three things, and all three are
// arrangements only the remote's operator can make: receive-pack is enabled over HTTP, so an
// authenticated writer can push at all; a `pre-receive` hook records what each push delivered; and a
// work tree exists so a test can create the commit the authorization will name and can move a ref the
// way a third party would.
func newPushRepository(t *testing.T, pki *testPKI) *pushRepository {
	t.Helper()
	repository := newPrivateRepository(t, pki)
	// `git-http-backend` serves receive-pack only for a repository that says so. Enabling it here is the
	// fixture, not the boundary under test: the authorization, the lease and the Store are what these
	// tests are about, and a transport that refused every write would make all of them unreachable.
	runGit(t, repository.Bare, nil, "config", "http.receivepack", "true")

	directory := t.TempDir()
	work := filepath.Join(directory, "work")
	runGit(t, directory, nil, "clone", "--quiet", repository.Bare, work)
	scene := &pushRepository{
		privateRepository: repository,
		Work:              work,
		Old:               repository.Commit,
		hookLog:           filepath.Join(directory, "pre-receive.log"),
	}
	// The hook allows everything by default and records what it was asked to apply. The scenarios that
	// need a server-side refusal install the refusing variant instead.
	scene.installHook(t, false)
	scene.commit(t, "the change the upstream already carries")
	scene.New = scene.head(t)
	runGit(t, work, nil, "push", "--quiet", "origin", "HEAD:"+pushStagingRef)
	return scene
}

// installHook writes the repository's `pre-receive` hook.
//
// The hook is the upstream's own record of what a push delivered, and it is the only record that
// matters: the client's report of what it sent is the client's claim, and the executor's classification
// is derived from that claim. The logging variant allows the update and leaves Git's own
// `<old> <new> <ref>` lines behind; the refusing variant leaves the same lines and then exits non-zero,
// which is what a repository does when it declines an update for a reason of its own.
func (r *pushRepository) installHook(t *testing.T, refuse bool) {
	t.Helper()
	outcome := "exit 0"
	if refuse {
		outcome = `echo "the acceptance test refuses every update" >&2` + "\nexit 1"
	}
	script := "#!/bin/sh\n" +
		"# Written by the push acceptance test: record what this push delivered, then decide.\n" +
		"cat >> " + r.hookLog + "\n" + outcome + "\n"
	path := filepath.Join(r.Bare, "hooks", "pre-receive")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("install the pre-receive hook: %v", err)
	}
}

// hookUpdates returns the updates the upstream was actually asked to apply, one per line, in Git's own
// `<old> <new> <ref>` spelling.
func (r *pushRepository) hookUpdates(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(r.hookLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read the pre-receive log: %v", err)
	}
	var updates []string
	for _, line := range strings.Split(string(raw), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			updates = append(updates, trimmed)
		}
	}
	return updates
}

// forgetHookUpdates discards what the harness's own pushes recorded, so a test asserts about the
// executor's push alone. The harness pushes over the local path while it arranges the repository, and
// those pushes are setup rather than evidence.
func (r *pushRepository) forgetHookUpdates(t *testing.T) {
	t.Helper()
	if err := os.Remove(r.hookLog); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear the pre-receive log: %v", err)
	}
}

// commit adds one file to the work tree and commits it, as a person working on the repository would.
func (r *pushRepository) commit(t *testing.T, message string) {
	t.Helper()
	name := strings.ReplaceAll(message, " ", "-") + ".txt"
	if err := os.WriteFile(filepath.Join(r.Work, name), []byte(message+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	runGit(t, r.Work, nil, "add", name)
	runGit(t, r.Work, []string{
		"GIT_AUTHOR_NAME=ora test", "GIT_AUTHOR_EMAIL=test@ora.invalid",
		"GIT_COMMITTER_NAME=ora test", "GIT_COMMITTER_EMAIL=test@ora.invalid",
	}, "commit", "--quiet", "-m", message)
}

// head is the work tree's current commit.
func (r *pushRepository) head(t *testing.T) string {
	t.Helper()
	return runGit(t, r.Work, nil, "rev-parse", "HEAD")
}

// publish commits one more change and pushes it to a branch, the way a third party with access to the
// upstream would. It returns the new commit.
func (r *pushRepository) publish(t *testing.T, message, ref string) string {
	t.Helper()
	r.commit(t, message)
	commit := r.head(t)
	runGit(t, r.Work, nil, "push", "--quiet", "origin", commit+":"+ref)
	return commit
}

// ref reads one ref of the bare repository directly, which is what the upstream holds as opposed to
// what any participant says it holds.
func (r *pushRepository) ref(t *testing.T, name string) string {
	t.Helper()
	return runGit(t, r.Bare, nil, "rev-parse", "--verify", name)
}

// pushWrite is one authorized write: the four values that describe it, the credential version it is
// against, and the identities Cloud's rows would carry for it.
type pushWrite struct {
	Repository      string
	Ref             string
	Old             string
	New             string
	Version         int64
	AuthorizationID string
	OperationID     string
}

// write is the authorized write a deployment's scene is about, under the default identities.
func (d *pushDeployment) write(version int64, old, new string) pushWrite {
	return pushWrite{
		Repository:      d.repository.URL,
		Ref:             pushRef,
		Old:             old,
		New:             new,
		Version:         version,
		AuthorizationID: pushAuthorizationID,
		OperationID:     pushOperationID,
	}
}

// binding is the signed payload this write is authorized by.
func (w pushWrite) binding(now time.Time) cloneauth.Binding {
	return cloneauth.Binding{
		AuthorizationID:   w.AuthorizationID,
		Operation:         cloneauth.OperationPush,
		TenantID:          pushTenantID,
		OwnerUserID:       pushOwnerUserID,
		ProjectID:         pushProjectID,
		WorkspaceID:       pushWorkspaceID,
		OperationID:       w.OperationID,
		ExecutorIdentity:  brokerResolverIdentity,
		CredentialID:      pushCredentialID,
		CredentialVersion: w.Version,
		Repository:        w.Repository,
		Ref:               w.Ref,
		OldCommit:         w.Old,
		NewCommit:         w.New,
		Capabilities:      []string{string(cloneauth.CapabilityPush)},
		IssuedAt:          now,
		ExpiresAt:         now.Add(5 * time.Minute),
	}
}

// sign mints the authorization, optionally adjusted first so a case can sign something other than what
// this build would.
func (w pushWrite) sign(t *testing.T, key ed25519.PrivateKey, adjust func(*cloneauth.Binding)) string {
	t.Helper()
	binding := w.binding(time.Now().UTC())
	if adjust != nil {
		adjust(&binding)
	}
	token, err := cloneauth.Issue(key, binding)
	if err != nil {
		t.Fatalf("issue a push authorization: %v", err)
	}
	return token
}

// job is the write as the executor receives it.
func (w pushWrite) job(authorization string) cloneexec.PushJob {
	return cloneexec.PushJob{
		OperationID:       w.OperationID,
		ProjectID:         pushProjectID,
		WorkspaceID:       pushWorkspaceID,
		Repository:        w.Repository,
		Ref:               w.Ref,
		OldCommit:         w.Old,
		NewCommit:         w.New,
		CredentialID:      pushCredentialID,
		CredentialVersion: w.Version,
		Authorization:     authorization,
	}
}

// execution is the write as Cloud's dispatcher receives it.
func (w pushWrite) execution() core.PushExecution {
	return core.PushExecution{
		AuthorizationID:   w.AuthorizationID,
		TenantID:          pushTenantID,
		OwnerUserID:       pushOwnerUserID,
		ProjectID:         pushProjectID,
		WorkspaceID:       pushWorkspaceID,
		OperationID:       w.OperationID,
		CredentialID:      pushCredentialID,
		CredentialVersion: w.Version,
		Repository:        w.Repository,
		Ref:               w.Ref,
		OldCommit:         w.Old,
		NewCommit:         w.New,
		Capabilities:      []string{string(cloneauth.CapabilityPush)},
	}
}

// logCollector keeps the executor's own log lines so a test can prove a secret never reached them, and
// forwards them to the test's output so a failure is diagnosable.
type logCollector struct {
	t      *testing.T
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (c *logCollector) Write(p []byte) (int, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.buffer.Write(p)
	c.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func (c *logCollector) String() string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.buffer.String()
}

// pushDeployment is one fully wired controlled-push boundary, from Cloud's dispatcher to the upstream.
type pushDeployment struct {
	pki        *testPKI
	repository *pushRepository
	service    *credentialService
	// storeClient is the executor's own `broker-resolver` client, exposed so the tests that are about
	// redemption rather than about pushing can drive the real boundary directly.
	storeClient *cloneexec.StoreClient
	executor    *cloneexec.PushExecutor
	client      *cloneexec.Client
	dispatcher  *core.PushDispatcher
	signingKey  ed25519.PrivateKey
	material    string
	scratch     string
	logs        *logCollector
}

// newPushDeployment wires the chain.
//
// A zero timeout leaves the executor's own default in place. The signing key is Cloud's, and the Store
// and the executor hold only its public half — which is what makes the authorization the only thing
// that can bind a write: neither component could mint one.
func newPushDeployment(t *testing.T, pushTimeout time.Duration) *pushDeployment {
	t.Helper()
	return newPushDeploymentWith(t, pushTimeout, nil)
}

// newPushDeploymentWith wires the same chain with the executor's listener decorated, which is how a case
// reaches the one condition the chain cannot produce by itself: an executor that ran and whose answer
// never arrived. A nil decorator is the undecorated chain, so every other case in this file keeps
// meeting exactly the listener it met before.
func newPushDeploymentWith(t *testing.T, pushTimeout time.Duration, decorate func(http.Handler) http.Handler) *pushDeployment {
	t.Helper()
	pki := newTestPKI(t)
	signingPublic, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Cloud's authorization key: %v", err)
	}
	repository := newPushRepository(t, pki)
	service := newCredentialService(t, pki, signingPublic, nil)
	storeClient, err := cloneexec.NewStoreClient(cloneexec.StoreClientConfig{
		Endpoint: service.server.URL,
		TLS:      pki.clientTLS(t, brokerResolverIdentity),
	})
	if err != nil {
		t.Fatalf("build the executor's credential service client: %v", err)
	}

	directory := t.TempDir()
	material := filepath.Join(directory, "material")
	scratch := filepath.Join(directory, "scratch")
	home := filepath.Join(directory, "home")
	for _, path := range []string{material, home} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	logs := &logCollector{t: t}
	logger := zap.New(zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		zapcore.AddSync(logs), zapcore.DebugLevel))
	// The clone executor is built only because C's server serves all three capabilities from one
	// listener and refuses to start without the clone one. This file never dispatches a clone.
	cloneExecutor, err := cloneexec.New(cloneexec.Config{
		MaterialRoot:     material,
		CheckoutRoot:     filepath.Join(directory, "checkout"),
		HomeDirectory:    home,
		HelperPath:       buildHelper(t),
		GitPath:          gitPath(t),
		SSLCAInfo:        pki.CAFile,
		VerificationKeys: []ed25519.PublicKey{signingPublic},
		Store:            storeClient,
		Timeout:          2 * time.Minute,
		Log:              logger,
	})
	if err != nil {
		t.Fatalf("build the clone executor: %v", err)
	}
	pusher, err := cloneexec.NewPushExecutor(cloneexec.PushConfig{
		MaterialRoot:     material,
		ScratchRoot:      scratch,
		HomeDirectory:    home,
		HelperPath:       buildHelper(t),
		GitPath:          gitPath(t),
		SSLCAInfo:        pki.CAFile,
		VerificationKeys: []ed25519.PublicKey{signingPublic},
		Store:            storeClient,
		Timeout:          pushTimeout,
		Log:              logger,
	})
	if err != nil {
		t.Fatalf("build the push executor: %v", err)
	}
	server, err := cloneexec.NewServer(cloneExecutor, cloneexec.ServerConfig{
		TLS: &tls.Config{
			Certificates: []tls.Certificate{pki.issue(t, executorServerIdentity, "127.0.0.1")},
			ClientCAs:    pki.pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
		Caller: cloneDispatcherIdentity,
		Push:   pusher,
		Log:    logger,
	})
	if err != nil {
		t.Fatalf("build the executor server: %v", err)
	}
	handler := server.Handler()
	if decorate != nil {
		handler = decorate(handler)
	}
	executorServer := pki.startServer(t, handler, executorServerIdentity, pki.pool)
	client, err := cloneexec.NewClient(cloneexec.ClientConfig{
		Endpoint: executorServer.URL,
		TLS:      pki.clientTLS(t, cloneDispatcherIdentity),
		Timeout:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build Cloud's dispatch client: %v", err)
	}
	return &pushDeployment{
		pki:         pki,
		repository:  repository,
		service:     service,
		storeClient: storeClient,
		executor:    pusher,
		client:      client,
		signingKey:  signingKey,
		material:    material,
		scratch:     scratch,
		logs:        logs,
		dispatcher: &core.PushDispatcher{
			Client:           client,
			SigningKey:       signingKey,
			ExecutorIdentity: brokerResolverIdentity,
			Lifetime:         core.PushDispatchLifetime,
		},
	}
}

// storeCredential writes one token into the real Credential Service and returns its version.
func (d *pushDeployment) storeCredential(t *testing.T, token string) int64 {
	t.Helper()
	_, version, err := d.service.store.Write(context.Background(), pushCredentialID, token)
	if err != nil {
		t.Fatalf("store a credential: %v", err)
	}
	return version
}

// assertNothingWasLeftBehind proves the credential is gone from everything the executor wrote.
//
// This is the cleanup half of every scenario, and it is asserted on the filesystem rather than on the
// executor's own report of having cleaned up: the material root holds staged secrets and the scratch
// root holds a plaintext copy of a private upstream, so "absent from both" is the property, and a
// report that says so is not.
func (d *pushDeployment) assertNothingWasLeftBehind(t *testing.T) {
	t.Helper()
	assertNoTokenUnder(t, d.material, d.repository.Token)
	assertNoTokenUnder(t, d.scratch, d.repository.Token)
	for _, root := range []string{d.material, d.scratch} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%s still holds %d entries after a push", root, len(entries))
		}
	}
}

// assertNoSecretInLog proves the executor's log carries neither the credential nor the authorization.
//
// It is the log half of the same rule the filesystem check covers: the executor deliberately keeps the
// upstream's own words out of its replies, so its log is where they go, and a log that carried the
// staged token or the bearer authorization would be a durable copy of a secret outside the Store.
func (d *pushDeployment) assertNoSecretInLog(t *testing.T, secrets ...string) {
	t.Helper()
	log := d.logs.String()
	for _, secret := range append(secrets, d.repository.Token) {
		if secret == "" {
			continue
		}
		if strings.Contains(log, secret) {
			t.Fatalf("the executor's log carries a secret")
		}
	}
}

// refusedCode is the stable code of a refusal the client reported, or the empty string when the failure
// was not a refusal at all.
func refusedCode(err error) string { return reasonCodeOf(err) }

// TestAControlledPushMovesOneRefOfARealUpstream is the closed loop: Cloud's dispatcher signs one
// approval, C redeems the authorization at the real Credential Service, Git authenticates with the
// token through the deployment-fixed helper, one ref of the isolated upstream moves, and the credential
// is gone.
func TestAControlledPushMovesOneRefOfARealUpstream(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	repository.forgetHookUpdates(t)

	outcome, err := deployment.dispatcher.Execute(context.Background(), deployment.write(version, repository.Old, repository.New).execution())
	if err != nil {
		t.Fatalf("the controlled push was refused: %v", err)
	}
	if outcome.Verdict != core.PushConfirmed {
		t.Fatalf("verdict = %q, want %q", outcome.Verdict, core.PushConfirmed)
	}
	if outcome.ObservedCommit != repository.New {
		t.Fatalf("observed commit = %q, want the authorized new commit %q", outcome.ObservedCommit, repository.New)
	}

	// The upstream's own state, read from the bare repository rather than from anything a participant
	// said: the authorized ref holds the authorized commit, and the branch the commit came from is
	// untouched.
	if got := repository.ref(t, pushRef); got != repository.New {
		t.Fatalf("the upstream's %s = %q, want %q", pushRef, got, repository.New)
	}
	if got := repository.ref(t, pushStagingRef); got != repository.New {
		t.Fatalf("the push moved %s as well: %q", pushStagingRef, got)
	}
	// And it moved for the reason the authorization gives: one update, to the authorized ref, from the
	// authorized old commit to the authorized new one.
	updates := repository.hookUpdates(t)
	want := repository.Old + " " + repository.New + " " + pushRef
	if len(updates) != 1 || updates[0] != want {
		t.Fatalf("the upstream was asked to apply %v, want exactly one update %q", updates, want)
	}
	// One write exchange, and it is the update the hook logged above. The read side is deliberately not
	// asserted as a number: how many exchanges one fetch takes is a property of the negotiated Git
	// protocol rather than of this executor, and asserting it would pin the wire's round trips instead of
	// the push's behavior. The write side has no such ambiguity — receive-pack is one exchange per
	// attempt — which is why the count that matters is this one.
	if receive, _ := repository.requests.snapshot(); receive != 1 {
		t.Fatalf("the upstream served %d write exchanges, want exactly 1", receive)
	}
	deployment.assertNothingWasLeftBehind(t)
	deployment.assertNoSecretInLog(t)
}

// loseThePushAnswer runs the push route and then breaks the connection instead of answering.
//
// The handler runs to completion against a discarded writer, so everything the job does really happens —
// the authorization is redeemed and spent, the upstream is read, the update is applied, the material is
// destroyed — and only the reply is lost. That is the shape of an executor killed between accepting the
// write and answering, and it is the ambiguity the settlement rule is about: nothing about the write can
// be established from the caller's side, so the caller must not conclude anything.
func loseThePushAnswer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/push-executions" {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(httptest.NewRecorder(), r)
		// `ErrAbortHandler` is the server's own signal for "close this connection without a response and
		// without a stack trace". It is how the write below lands while the answer does not.
		panic(http.ErrAbortHandler)
	})
}

// answerWithoutACode stands in for whatever sits between Cloud and the executor and answers in its place.
//
// The job never runs, and the reply is still not evidence of that: a caller cannot tell this from a write
// whose answer was lost, which is why it must not be read as the executor having refused anything.
func answerWithoutACode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/push-executions" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1></body></html>"))
	})
}

// TestAnInterruptedWriteIsNotAConcludedFailure is the real-boundary half of the interruption rule, and
// it is the case a container-level run found first.
//
// The two shapes are the two ways a reply goes missing around a write: the executor ran and its answer
// was lost, and something else answered in its place. What the test asserts is deliberately *not* a
// verdict — this layer has no approval row and cannot reach one, so Cloud's settlement is measured in
// `internal/core`. What it asserts is what the chain actually hands over: for the first shape that the
// write really landed (proved from the upstream's side, and from the executor having got far enough to
// destroy its material), and for both shapes that the error Cloud receives is **not** a refusal. An
// error carrying a code is what a caller settles a concluded failure with, so a code here is the defect:
// it would record a write that may have landed as one that did not.
func TestAnInterruptedWriteIsNotAConcludedFailure(t *testing.T) {
	cases := []struct {
		name     string
		decorate func(http.Handler) http.Handler
		// wrote says whether the job reached the upstream, which is what makes the first shape the
		// dangerous one: there is a real write behind an answer nobody received.
		wrote bool
	}{
		{name: "the executor accepted the write and the answer was lost", decorate: loseThePushAnswer, wrote: true},
		{name: "an intermediary answered in the executor's place", decorate: answerWithoutACode},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			deployment := newPushDeploymentWith(t, 0, testCase.decorate)
			repository := deployment.repository
			version := deployment.storeCredential(t, repository.Token)
			repository.forgetHookUpdates(t)

			_, err := deployment.dispatcher.Execute(context.Background(), deployment.write(version, repository.Old, repository.New).execution())
			if err == nil {
				t.Fatal("an interrupted dispatch reported an outcome")
			}
			// The two shapes a coded error can take, both of which would settle the approval as a
			// concluded failure. Neither may appear here.
			var fault *core.Fault
			if errors.As(err, &fault) {
				t.Fatalf("an unreadable answer was reported as Cloud's own refusal %q", fault.Code)
			}
			var refusal *cloneexec.DispatcherRefusal
			if errors.As(err, &refusal) {
				t.Fatalf("a reply no component vouched for was reported as the executor's refusal %q", refusal.Code)
			}

			updates := repository.hookUpdates(t)
			if testCase.wrote {
				// The write is real: the upstream was asked to apply exactly the authorized update, and the
				// ref holds the authorized new commit. This is what makes the settlement direction matter
				// rather than theoretical.
				want := repository.Old + " " + repository.New + " " + pushRef
				if len(updates) != 1 || updates[0] != want {
					t.Fatalf("the upstream was asked to apply %v, want exactly one update %q", updates, want)
				}
				if got := repository.ref(t, pushRef); got != repository.New {
					t.Fatalf("the upstream's %s = %q, want the authorized commit %q", pushRef, got, repository.New)
				}
				// And the executor got far enough to do everything a completed push does, including
				// destroying the material: the answer is the only thing that went missing, so a caller
				// reading the failure as "nothing happened" would be reading it wrong.
				deployment.assertNothingWasLeftBehind(t)
				deployment.assertNoSecretInLog(t)
			} else {
				// Nothing ran, so there is nothing at the upstream. Cloud still cannot conclude that from
				// the reply it got, which is the whole point of the case.
				if len(updates) != 0 {
					t.Fatalf("a job that never ran reached the upstream: %v", updates)
				}
				if got := repository.ref(t, pushRef); got != repository.Old {
					t.Fatalf("the upstream's %s = %q, want it untouched at %q", pushRef, got, repository.Old)
				}
			}
		})
	}
}

// TestThePushDoesNotOverwriteARefAnotherWriterMoved is the compare-and-swap's first refusal.
//
// The upstream's ref moved after the approval was recorded and before the write ran, so the lease
// cannot hold. The executor must not write, must not report either success or failure — the ref holds a
// commit nobody in this exchange authorized — and must leave the third party's commit in place.
func TestThePushDoesNotOverwriteARefAnotherWriterMoved(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	third := repository.publish(t, "a third party's change", pushRef)
	repository.forgetHookUpdates(t)

	outcome, err := deployment.dispatcher.Execute(context.Background(), deployment.write(version, repository.Old, repository.New).execution())
	if err != nil {
		t.Fatalf("the push was refused rather than answered: %v", err)
	}
	if outcome.Verdict != core.PushTargetConflict {
		t.Fatalf("verdict = %q, want %q", outcome.Verdict, core.PushTargetConflict)
	}
	// The observation is the third value, and it is what makes the conflict legible to an operator.
	if outcome.ObservedCommit != third {
		t.Fatalf("observed commit = %q, want the third party's %q", outcome.ObservedCommit, third)
	}
	if got := repository.ref(t, pushRef); got != third {
		t.Fatalf("the upstream's %s = %q, want the third party's commit %q", pushRef, got, third)
	}
	if updates := repository.hookUpdates(t); len(updates) != 0 {
		t.Fatalf("a write reached the upstream despite the lease: %v", updates)
	}
	deployment.assertNothingWasLeftBehind(t)
}

// TestThePushReportsATargetAnotherWriterAlreadyReached is the cell D7 separates from success: the ref
// holds the authorized commit, and this attempt is not what put it there.
//
// Somebody else fast-forwarded the ref to the authorized commit first. Git reports the ref as already
// up to date — or refuses the lease, in which case the read-only reconciliation finds the same thing —
// and either way the executor must answer that the target state is reached and that this approval is
// not credited with reaching it.
func TestThePushReportsATargetAnotherWriterAlreadyReached(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	runGit(t, repository.Work, nil, "push", "--quiet", "origin", repository.New+":"+pushRef)
	repository.forgetHookUpdates(t)

	outcome, err := deployment.dispatcher.Execute(context.Background(), deployment.write(version, repository.Old, repository.New).execution())
	if err != nil {
		t.Fatalf("the push was refused rather than answered: %v", err)
	}
	if outcome.Verdict != core.PushTargetSatisfied {
		t.Fatalf("verdict = %q, want %q", outcome.Verdict, core.PushTargetSatisfied)
	}
	if outcome.ObservedCommit != repository.New {
		t.Fatalf("observed commit = %q, want %q", outcome.ObservedCommit, repository.New)
	}
	if got := repository.ref(t, pushRef); got != repository.New {
		t.Fatalf("the upstream's %s = %q, want %q", pushRef, got, repository.New)
	}
	// This attempt delivered nothing: whatever moved the ref, it was not this write.
	if updates := repository.hookUpdates(t); len(updates) != 0 {
		t.Fatalf("this attempt delivered an update after all: %v", updates)
	}
	// The verdict is the evidence that the read-only reconciliation ran and was readable: a refusal
	// whose read-back failed is `unknown`, not `target_satisfied`.
	deployment.assertNothingWasLeftBehind(t)
}

// TestAnUpstreamThatRefusesTheUpdateIsNotAppliedAndNothingIsRetried is the confirmed failure.
//
// The upstream declines the update for a reason of its own — a server-side hook, which is what a
// protected branch or a policy check is — and the ref does not hold the authorized commit. The executor
// must report the failure, must read the ref back once to establish that nothing was applied, and must
// not attempt the write again: the authorization it spent cannot be spent twice, and a retry would be a
// second write to the same ref.
func TestAnUpstreamThatRefusesTheUpdateIsNotAppliedAndNothingIsRetried(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	// The hook records and refuses, which is the only way a test can make the *server* the party that
	// declines an update.
	repository.installHook(t, true)
	repository.forgetHookUpdates(t)

	outcome, err := deployment.dispatcher.Execute(context.Background(), deployment.write(version, repository.Old, repository.New).execution())
	if err != nil {
		t.Fatalf("the push was refused rather than answered: %v", err)
	}
	if outcome.Verdict != core.PushNotApplied {
		t.Fatalf("verdict = %q, want %q", outcome.Verdict, core.PushNotApplied)
	}
	if outcome.ObservedCommit != repository.Old {
		t.Fatalf("observed commit = %q, want the unchanged %q", outcome.ObservedCommit, repository.Old)
	}
	if got := repository.ref(t, pushRef); got != repository.Old {
		t.Fatalf("the upstream's %s = %q, want the unchanged %q", pushRef, got, repository.Old)
	}
	updates := repository.hookUpdates(t)
	want := repository.Old + " " + repository.New + " " + pushRef
	if len(updates) != 1 || updates[0] != want {
		t.Fatalf("the upstream was asked to apply %v, want exactly one refused update %q", updates, want)
	}
	// Exactly one write exchange, which is what "nothing is retried" means at the wire. The read-back is
	// asserted by its *effect* rather than by a count: `not_applied` with the old commit is a state only
	// the read-only reconciliation can produce, because the classification reaches it by parsing the
	// answer to one `ls-remote` and by nothing else. A second write would raise this count, and it is 1.
	if receive, _ := repository.requests.snapshot(); receive != 1 {
		t.Fatalf("the upstream served %d write exchanges, want exactly 1", receive)
	}
	deployment.assertNothingWasLeftBehind(t)
}

// TestAWriteTheUpstreamDoesNotCarryIsRefusedWithoutReachingTheNetwork covers the two local
// determinations a push makes after the credential is redeemed and before any network write.
//
// The first is that the authorized new commit exists in the authorized upstream: this executor's only
// content channel is that upstream, so a commit that exists nowhere in it cannot be published. The
// second is that the authorized old commit is an ancestor of the new one, which is what makes a force
// unrepresentable: a rewrite of a published branch is refused here, against objects this process holds,
// rather than inferred from the lease.
//
// Both are refusals and not `not_applied`, because the write was never attempted: an operator has to be
// able to tell "the upstream said no" from "the job named a commit that is not there". And both cost one
// authorization, which is stated rather than engineered around — the objects are what need the
// credential, so the determinations cannot be made before it is spent.
func TestAWriteTheUpstreamDoesNotCarryIsRefusedWithoutReachingTheNetwork(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)

	// A commit that exists only in the work tree: it was never pushed anywhere.
	unpublished := func() string {
		repository.commit(t, "a commit the upstream has never seen")
		return repository.head(t)
	}()

	cases := []struct {
		name   string
		old    string
		new    string
		authID string
	}{
		{name: "the new commit is not in the upstream", old: repository.Old, new: unpublished, authID: pushAuthorizationID},
		{name: "the old commit is not an ancestor of the new one", old: repository.New, new: repository.Old, authID: pushSecondAuthID},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository.forgetHookUpdates(t)
			write := deployment.write(version, testCase.old, testCase.new)
			// Each case is one approval: an authorization is spent by the attempt that presents it, and
			// the Store records consumption under the authorization's identifier, so two cases under one
			// identifier would be a replay rather than a second determination.
			write.AuthorizationID = testCase.authID
			authorization := write.sign(t, deployment.signingKey, nil)

			_, err := deployment.client.Push(context.Background(), write.job(authorization))
			if code := refusedCode(err); code != cloneexec.ReasonCodePushNotApplicable {
				t.Fatalf("refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodePushNotApplicable, err)
			}
			// The refusal is local: no write reached the upstream, and the ref is where it was.
			if updates := repository.hookUpdates(t); len(updates) != 0 {
				t.Fatalf("a write reached the upstream despite the refusal: %v", updates)
			}
			if got := repository.ref(t, pushRef); got != repository.Old {
				t.Fatalf("the upstream's %s = %q, want the unchanged %q", pushRef, got, repository.Old)
			}
			// And the authorization is spent, which is the cost the executor documents: redeeming it is
			// what makes the objects available, so this refusal came after the Store delivered. A retry
			// under the same authorization is not available, and this is the evidence that it is not.
			_, replayErr := deployment.storeClient.ResolvePush(context.Background(), pushCredentialID, authorization)
			if code := refusedCode(replayErr); code != cloneexec.ReasonCodeAuthorizationConsumed {
				t.Fatalf("the refused attempt's authorization read back as %q, want it spent (error: %v)", code, replayErr)
			}
			deployment.assertNothingWasLeftBehind(t)
		})
	}
}

// TestThePushPathRefusesAJobThatWouldWidenTheWriteAndSpendsNothingDoingIt is the boundary half of the
// write's limits, driven through the real client and the real server.
//
// Every case hands C a job that is wider than, or different from, what the authorization names: a ref
// that is not a branch, a pattern, a force prefix, a list, a deletion and a creation spelled with the
// null object id, an address that is not an HTTPS repository, and a repository, ref, commit pair,
// credential or credential version that the authorization does not name. Each must be refused with a
// stable code, must reach neither the network nor the upstream, and — the assertion the last step makes
// — must leave the authorization unspent, because a refusal that happened before redemption is the only
// kind that does not cost an approval.
func TestThePushPathRefusesAJobThatWouldWidenTheWriteAndSpendsNothingDoingIt(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	// A second live upstream on the same credential. It is what makes "a repository the authorization
	// does not name" a real target rather than a string: the case asserts that nothing reached *it*.
	elsewhere := newPushRepository(t, deployment.pki)

	write := deployment.write(version, repository.Old, repository.New)
	authorization := write.sign(t, deployment.signingKey, nil)
	// A clone authorization for the same credential. It cannot be confused with a push by accident —
	// `validateClone` refuses a binding that carries a ref, a commit pair, an operation id or an
	// executor identity — so the only check that can refuse it on this route is the purpose.
	cloneAuthorization := issueAuthorization(t, deployment.signingKey, pushCredentialID, repository.URL, nil)

	cases := []struct {
		name    string
		widen   func(job *cloneexec.PushJob)
		want    string
		against *pushRepository
	}{
		{name: "a tag is not a branch", widen: func(job *cloneexec.PushJob) { job.Ref = "refs/tags/v1.0.0" }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "a wildcard names no ref", widen: func(job *cloneexec.PushJob) { job.Ref = "refs/heads/*" }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "a force prefix is not a ref name", widen: func(job *cloneexec.PushJob) { job.Ref = "+" + pushRef }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "two refs are not one", widen: func(job *cloneexec.PushJob) { job.Ref = pushRef + " " + pushStagingRef }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "a traversal spelling", widen: func(job *cloneexec.PushJob) { job.Ref = "refs/heads/../../main" }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "a deletion, spelled as the null object", widen: func(job *cloneexec.PushJob) { job.OldCommit = strings.Repeat("0", 40) }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "a creation, spelled as the null object", widen: func(job *cloneexec.PushJob) { job.NewCommit = strings.Repeat("0", 40) }, want: cloneexec.ReasonCodeInvalidJob},
		{name: "an address that is not an https repository", widen: func(job *cloneexec.PushJob) { job.Repository = "git@github.example.invalid:group/private.git" }, want: cloneexec.ReasonCodeInvalidJob},
		{
			// The victim is a real repository that this credential can reach, so the assertion that it
			// was untouched is an assertion about a place the write could have landed.
			name: "a repository the authorization does not name", against: elsewhere,
			widen: func(job *cloneexec.PushJob) { job.Repository = elsewhere.URL }, want: cloneexec.ReasonCodeBindingMismatch,
		},
		{name: "a ref the authorization does not name", widen: func(job *cloneexec.PushJob) { job.Ref = pushStagingRef }, want: cloneexec.ReasonCodeBindingMismatch},
		{name: "a commit pair the authorization does not name", widen: func(job *cloneexec.PushJob) { job.NewCommit = pushUnrelatedCommit }, want: cloneexec.ReasonCodeBindingMismatch},
		{name: "another credential", widen: func(job *cloneexec.PushJob) { job.CredentialID = acceptanceOtherCredentialID }, want: cloneexec.ReasonCodeBindingMismatch},
		{name: "another credential version", widen: func(job *cloneexec.PushJob) { job.CredentialVersion = version + 1 }, want: cloneexec.ReasonCodeBindingMismatch},
		{name: "an authorization minted for a clone", widen: func(job *cloneexec.PushJob) { job.Authorization = cloneAuthorization }, want: cloneexec.ReasonCodeBindingMismatch},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repository.forgetHookUpdates(t)
			elsewhere.forgetHookUpdates(t)
			job := write.job(authorization)
			testCase.widen(&job)

			_, err := deployment.client.Push(context.Background(), job)
			if code := refusedCode(err); code != testCase.want {
				t.Fatalf("refusal code = %q, want %q (error: %v)", code, testCase.want, err)
			}
			if updates := repository.hookUpdates(t); len(updates) != 0 {
				t.Fatalf("a widened job reached the authorized upstream: %v", updates)
			}
			if updates := elsewhere.hookUpdates(t); len(updates) != 0 {
				t.Fatalf("a widened job reached a repository the authorization does not name: %v", updates)
			}
			if got := repository.ref(t, pushRef); got != repository.Old {
				t.Fatalf("the upstream's %s = %q, want the unchanged %q", pushRef, got, repository.Old)
			}
		})
	}

	// The last step is what turns "each case was refused" into "each case was refused before anything
	// was spent": the same authorization, presented with the write it was actually minted for, still
	// performs exactly one update. If any case above had consumed it, the Store would refuse this one.
	repository.forgetHookUpdates(t)
	job := write.job(authorization)
	result, err := deployment.client.Push(context.Background(), job)
	if err != nil {
		t.Fatalf("the authorization survived the refusals but the authorized write did not: %v", err)
	}
	if result.State != cloneexec.PushConfirmed {
		t.Fatalf("the authorized write answered %q, want %q", result.State, cloneexec.PushConfirmed)
	}
	if got := repository.ref(t, pushRef); got != repository.New {
		t.Fatalf("the upstream's %s = %q, want %q", pushRef, got, repository.New)
	}
	if updates := repository.hookUpdates(t); len(updates) != 1 {
		t.Fatalf("the authorized write delivered %v, want exactly one update", updates)
	}
	deployment.assertNothingWasLeftBehind(t)
	deployment.assertNoSecretInLog(t, authorization)
}

// TestOneApprovalCannotWriteTwice is the single-use rule measured at the upstream.
//
// The authorized write runs and succeeds. The ref is then deliberately moved back to the authorized old
// commit, so that a second write under the same authorization would *succeed* and be visible — a replay
// that were merely refused by the lease would prove nothing about the authorization. The second attempt
// must be refused at redemption, and the ref must be exactly where the third party left it.
func TestOneApprovalCannotWriteTwice(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	write := deployment.write(version, repository.Old, repository.New)
	authorization := write.sign(t, deployment.signingKey, nil)
	repository.forgetHookUpdates(t)

	result, err := deployment.client.Push(context.Background(), write.job(authorization))
	if err != nil {
		t.Fatalf("the authorized write was refused: %v", err)
	}
	if result.State != cloneexec.PushConfirmed {
		t.Fatalf("the authorized write answered %q, want %q", result.State, cloneexec.PushConfirmed)
	}
	// Back to where the first attempt found it, so a second write would have somewhere to land.
	runGit(t, repository.Bare, nil, "update-ref", pushRef, repository.Old)

	_, err = deployment.client.Push(context.Background(), write.job(authorization))
	if code := refusedCode(err); code != cloneexec.ReasonCodeRedemptionFailed {
		t.Fatalf("replay refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeRedemptionFailed, err)
	}
	if got := repository.ref(t, pushRef); got != repository.Old {
		t.Fatalf("the replay wrote after all: %s = %q", pushRef, got)
	}
	if updates := repository.hookUpdates(t); len(updates) != 1 {
		t.Fatalf("the upstream was asked to apply %v, want the first attempt's single update", updates)
	}
	// The same thing said through the Store directly: the authorization is consumed, and that is the
	// reason the retry could not have written.
	if _, err := deployment.storeClient.ResolvePush(context.Background(), pushCredentialID, authorization); refusedCode(err) != cloneexec.ReasonCodeAuthorizationConsumed {
		t.Fatalf("the authorization read back as %q, want it spent (error: %v)", refusedCode(err), err)
	}
	deployment.assertNothingWasLeftBehind(t)
}

// TestARedemptionCannotBeSpentOnAnotherPurpose is the Store's half of the purpose separation, driven
// through the executor's own clients.
//
// The push authorization is presented in the clone shape and on the mirror route, and a clone
// authorization is presented as a push. All three must be refused without being spent, and the push
// authorization must still be redeemable afterwards — a refusal that consumed it would be a denial of
// service against the one approval it names.
func TestARedemptionCannotBeSpentOnAnotherPurpose(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	write := deployment.write(version, repository.Old, repository.New)
	pushAuthorization := write.sign(t, deployment.signingKey, nil)

	t.Run("a push authorization is not a clone authorization", func(t *testing.T) {
		_, err := deployment.storeClient.Resolve(context.Background(), pushCredentialID, pushAuthorization, acceptanceRuntime())
		if code := refusedCode(err); code != cloneexec.ReasonCodeAuthorizationBinding {
			t.Fatalf("refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeAuthorizationBinding, err)
		}
	})

	t.Run("a push authorization is not a mirror preparation", func(t *testing.T) {
		_, err := deployment.storeClient.ResolveMirror(context.Background(), pushCredentialID, pushAuthorization)
		if code := refusedCode(err); code != storePurposeRefusalCode {
			t.Fatalf("refusal code = %q, want %q (error: %v)", code, storePurposeRefusalCode, err)
		}
	})

	t.Run("a clone authorization is not a push authorization", func(t *testing.T) {
		cloneAuthorization := issueAuthorization(t, deployment.signingKey, pushCredentialID, repository.URL, nil)
		_, err := deployment.storeClient.ResolvePush(context.Background(), pushCredentialID, cloneAuthorization)
		if code := refusedCode(err); code != cloneexec.ReasonCodeAuthorizationBinding {
			t.Fatalf("refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeAuthorizationBinding, err)
		}
	})

	// None of the three refusals spent the write's authorization, which is what makes them refusals
	// rather than losses.
	if _, err := deployment.storeClient.ResolvePush(context.Background(), pushCredentialID, pushAuthorization); err != nil {
		t.Fatalf("the push authorization was spent by a request that was refused: %v", err)
	}
}

// TestAPushThatCannotReachTheUpstreamIsReapedAndLeavesNoSecretBehind covers the failure a write is
// never allowed to hide: an attempt that hangs.
//
// The address is a listener that accepts connections and never answers, so the fetch blocks until the
// executor's own timeout cancels it — which is the subprocess reaping this asserts, because a Git that
// outlived its context would hold the process open and the scratch directory occupied. The credential
// has already been redeemed by then, so the assertion that matters is the one about what is left on
// disk.
func TestAPushThatCannotReachTheUpstreamIsReapedAndLeavesNoSecretBehind(t *testing.T) {
	const timeout = 3 * time.Second
	deployment := newPushDeployment(t, timeout)
	repository := deployment.repository
	version := deployment.storeCredential(t, repository.Token)
	stalled := stalledRemote(t)

	write := deployment.write(version, repository.Old, repository.New)
	write.Repository = stalled
	authorization := write.sign(t, deployment.signingKey, nil)

	started := time.Now()
	_, err := deployment.client.Push(context.Background(), write.job(authorization))
	elapsed := time.Since(started)

	if code := refusedCode(err); code != cloneexec.ReasonCodePushFetchFailed {
		t.Fatalf("refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodePushFetchFailed, err)
	}
	// Bounded by the configured timeout rather than by anything Git or the operating system would have
	// chosen: the attempt was killed and reaped, not waited out.
	if elapsed > 30*time.Second {
		t.Fatalf("a push against an unanswering upstream took %s, which is not the configured %s", elapsed, timeout)
	}
	deployment.assertNothingWasLeftBehind(t)
	deployment.assertNoSecretInLog(t, authorization)
	if removed, err := deployment.executor.Sweep(); err != nil || removed != 0 {
		t.Fatalf("the aborted push left %d scratch entries behind (error: %v)", removed, err)
	}

	// And the executor is still usable, which is the part a leaked process would break: the same
	// deployment performs a real push immediately afterwards, reusing the materialization root and the
	// scratch root the aborted attempt had just used.
	repository.forgetHookUpdates(t)
	next := deployment.write(version, repository.Old, repository.New)
	next.AuthorizationID, next.OperationID = pushSecondAuthID, pushSecondOperID
	result, err := deployment.client.Push(context.Background(), next.job(next.sign(t, deployment.signingKey, nil)))
	if err != nil {
		t.Fatalf("the push after an aborted one was refused: %v", err)
	}
	if result.State != cloneexec.PushConfirmed {
		t.Fatalf("the push after an aborted one answered %q, want %q", result.State, cloneexec.PushConfirmed)
	}
	if got := repository.ref(t, pushRef); got != repository.New {
		t.Fatalf("the upstream's %s = %q, want %q", pushRef, got, repository.New)
	}
	deployment.assertNothingWasLeftBehind(t)
}

// TestTheStartupSweepRemovesAnAbandonedScratchRepository covers the one failure a running process
// cannot repair.
//
// A scratch repository has no life of its own — it is created, filled and removed inside one call — so
// anything under the scratch root at startup is debris from a process that died, and it is debris that
// matters more here than on the clone path, because it holds a plaintext copy of a private upstream.
func TestTheStartupSweepRemovesAnAbandonedScratchRepository(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	abandoned := filepath.Join(deployment.scratch, pushOperationID+".abandoned")
	if err := os.MkdirAll(abandoned, 0o700); err != nil {
		t.Fatalf("stage an abandoned scratch directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write into the abandoned scratch directory: %v", err)
	}
	removed, err := deployment.executor.Sweep()
	if err != nil {
		t.Fatalf("sweep the scratch root: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d entries, want 1", removed)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("the abandoned scratch directory survived the sweep: %v", err)
	}
}

// TestThePushExecutorRefusesAnIncompleteDeployment is the configuration boundary.
//
// Each case removes one thing a push cannot work without. None of them may produce an executor: a
// deployment that cannot verify an authorization, cannot redeem one, cannot write a scratch repository
// somewhere only this identity can reach, or cannot keep that repository out of the directory staged
// tokens live in must refuse to start rather than start and push.
func TestThePushExecutorRefusesAnIncompleteDeployment(t *testing.T) {
	deployment := newPushDeployment(t, 0)
	base := func() cloneexec.PushConfig {
		return cloneexec.PushConfig{
			MaterialRoot:     deployment.material,
			ScratchRoot:      deployment.scratch,
			HomeDirectory:    t.TempDir(),
			HelperPath:       buildHelper(t),
			GitPath:          gitPath(t),
			VerificationKeys: []ed25519.PublicKey{make([]byte, ed25519.PublicKeySize)},
			Store:            deployment.storeClient,
		}
	}
	cases := []struct {
		name    string
		disable func(config *cloneexec.PushConfig)
	}{
		{name: "no scratch root", disable: func(config *cloneexec.PushConfig) { config.ScratchRoot = "" }},
		{name: "the scratch root is the materialization root", disable: func(config *cloneexec.PushConfig) { config.ScratchRoot = config.MaterialRoot }},
		{name: "no credential service", disable: func(config *cloneexec.PushConfig) { config.Store = nil }},
		{name: "no verification key", disable: func(config *cloneexec.PushConfig) { config.VerificationKeys = nil }},
		{name: "a verification key that is not an Ed25519 key", disable: func(config *cloneexec.PushConfig) {
			config.VerificationKeys = []ed25519.PublicKey{{1, 2, 3}}
		}},
		{name: "no credential helper", disable: func(config *cloneexec.PushConfig) { config.HelperPath = "" }},
		{name: "no git binary", disable: func(config *cloneexec.PushConfig) { config.GitPath = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := base()
			testCase.disable(&config)
			if executor, err := cloneexec.NewPushExecutor(config); err == nil {
				t.Fatalf("an incomplete deployment produced a push executor: %+v", executor)
			}
		})
	}
}

// stalledRemote is a repository address that accepts connections and never answers.
//
// It is how a test produces a push that hangs rather than one that fails: the TCP connection succeeds,
// so Git proceeds to the TLS handshake, and nothing ever completes it. The connections are held open
// until the test ends, because closing them would turn the hang into a connection reset — a different
// failure with a different code.
func stalledRemote(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	var connections sync.WaitGroup
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				<-done
				_ = connection.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		close(done)
		_ = listener.Close()
		connections.Wait()
	})
	return fmt.Sprintf("https://%s/isolated/unanswering", listener.Addr().String())
}
