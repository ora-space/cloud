// This file is the acceptance test for the mirror-preparation boundary, and it is built the same way
// the clone boundary's is: real components rather than doubles. A real private HTTPS Git repository
// behind HTTP Basic authentication, a real Credential Service with a real volume and a real
// key-encryption key, real mutually authenticated TLS, the real `git-credential-ora` helper binary,
// the real mirror store, and the real executor and dispatch client the deployment runs.
//
// What it establishes is the mirror half of `20261010-controlled-node-git-authentication-isolation`
// D11 and of the 2026-10-13 ruling: one Cloud-signed `mirror_prepare` authorization, redeemed once as
// the `broker-resolver` identity, used to fetch exactly one branch of exactly one private repository,
// published as an internal mirror that a Git client can clone, with the credential gone afterwards.
//
// It also carries the purpose-confusion matrix in both directions on the real wire. Each route is given
// the other's authorization and must refuse it *without spending it*, which is the property the two
// operations exist for: a clone burned at the mirror endpoint is a clone the Node can no longer
// perform. The refusal is proven by spending the authorization afterwards through the route that does
// own it.
package cloneexec_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// The identifiers one preparation uses. They are canonical lowercase UUIDs because both the Store and
// the materialization layout turn them into path segments.
const (
	mirrorOperationID     = "3f7a1c85-9b26-4d03-8e57-1c4a6b8d2f90"
	mirrorAuthorizationID = "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46"
	mirrorRef             = "refs/heads/main"
)

// mirrorDeployment is one fully wired mirror-preparation boundary.
//
// Both capabilities are configured on one server here, and that is deliberate: the two routes exist to
// be distinguishable, and a test that ran them in separate deployments could not show that the same
// dispatcher presenting the same certificate is answered differently by each.
type mirrorDeployment struct {
	repository   *privateRepository
	service      *credentialService
	mirrors      *mirror.Store
	mirror       *cloneexec.MirrorExecutor
	client       *cloneexec.Client
	storeClient  *cloneexec.StoreClient
	signingKey   ed25519.PrivateKey
	materialRoot string
}

func newMirrorDeployment(t *testing.T, configureStore func(*credentialstore.Config)) *mirrorDeployment {
	t.Helper()
	pki := newTestPKI(t)
	signingPublic, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Cloud's authorization key: %v", err)
	}
	repository := newPrivateRepository(t, pki)
	store := newCredentialService(t, pki, signingPublic, configureStore)
	storeClient, err := cloneexec.NewStoreClient(cloneexec.StoreClientConfig{
		Endpoint: store.server.URL,
		TLS:      pki.clientTLS(t, brokerResolverIdentity),
	})
	if err != nil {
		t.Fatalf("build the executor's credential service client: %v", err)
	}
	directory := t.TempDir()
	materialRoot := filepath.Join(directory, "material")
	home := filepath.Join(directory, "home")
	for _, path := range []string{materialRoot, home} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
	}
	mirrors, err := mirror.New(mirror.Config{
		Root:           filepath.Join(directory, "mirrors"),
		MaxMirrorBytes: 64 << 20,
		MaxVolumeBytes: 256 << 20,
		Grace:          time.Hour,
	})
	if err != nil {
		t.Fatalf("open the mirror store: %v", err)
	}
	mirrorExecutor, err := cloneexec.NewMirrorExecutor(cloneexec.MirrorConfig{
		MaterialRoot:     materialRoot,
		HomeDirectory:    home,
		HelperPath:       buildHelper(t),
		GitPath:          gitPath(t),
		SSLCAInfo:        pki.CAFile,
		VerificationKeys: []ed25519.PublicKey{signingPublic},
		Mirrors:          mirrors,
		Store:            storeClient,
		Timeout:          2 * time.Minute,
		Log:              testLogger(t),
	})
	if err != nil {
		t.Fatalf("build the mirror executor: %v", err)
	}
	// The clone executor is configured as well, so both routes answer on one listener and the
	// purpose-confusion matrix can be run against a single dispatcher.
	cloneExecutor, err := cloneexec.New(cloneexec.Config{
		MaterialRoot:     materialRoot,
		CheckoutRoot:     filepath.Join(directory, "checkout"),
		HomeDirectory:    home,
		HelperPath:       buildHelper(t),
		GitPath:          gitPath(t),
		SSLCAInfo:        pki.CAFile,
		VerificationKeys: []ed25519.PublicKey{signingPublic},
		Store:            storeClient,
		Timeout:          2 * time.Minute,
		Log:              testLogger(t),
	})
	if err != nil {
		t.Fatalf("build the clone executor: %v", err)
	}
	server, err := cloneexec.NewServer(cloneExecutor, cloneexec.ServerConfig{
		TLS: &tls.Config{
			Certificates: []tls.Certificate{pki.issue(t, executorServerIdentity, "127.0.0.1")},
			ClientCAs:    pki.pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
		Caller: cloneDispatcherIdentity,
		Mirror: mirrorExecutor,
		Log:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("build the executor server: %v", err)
	}
	executorServer := pki.startServer(t, server.Handler(), executorServerIdentity, pki.pool)
	client, err := cloneexec.NewClient(cloneexec.ClientConfig{
		Endpoint: executorServer.URL,
		TLS:      pki.clientTLS(t, cloneDispatcherIdentity),
		Timeout:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build Cloud's dispatch client: %v", err)
	}
	return &mirrorDeployment{
		repository:   repository,
		service:      store,
		mirrors:      mirrors,
		mirror:       mirrorExecutor,
		client:       client,
		storeClient:  storeClient,
		signingKey:   signingKey,
		materialRoot: materialRoot,
	}
}

// storeCredential writes one token into the real Credential Service and returns its version.
func (d *mirrorDeployment) storeCredential(t *testing.T, credentialID, token string) int64 {
	t.Helper()
	_, version, err := d.service.store.Write(context.Background(), credentialID, token)
	if err != nil {
		t.Fatalf("store a credential: %v", err)
	}
	return version
}

// issueMirrorAuthorization mints one `mirror_prepare` authorization, the way Cloud does.
//
// The executor identity is the SPIFFE URI the executor's own `broker-resolver` certificate carries,
// because that is what the Store compares the presenter against: the authorization names who may spend
// it, and the certificate is how that party proves it is who the authorization named.
func issueMirrorAuthorization(t *testing.T, key ed25519.PrivateKey, credentialID, repository string, generation int64, adjust func(*cloneauth.Binding)) string {
	t.Helper()
	now := time.Now().UTC()
	binding := cloneauth.Binding{
		AuthorizationID:       mirrorAuthorizationID,
		Operation:             cloneauth.OperationMirrorPrepare,
		TenantID:              "tenant-a",
		OwnerUserID:           "user-a",
		ProjectID:             "project-a",
		WorkspaceID:           "workspace-a",
		CredentialID:          credentialID,
		CredentialVersion:     1,
		Repository:            repository,
		Ref:                   mirrorRef,
		Capabilities:          []string{string(cloneauth.CapabilityClone)},
		OperationID:           mirrorOperationID,
		PreparationGeneration: generation,
		ExecutorIdentity:      brokerResolverIdentity,
		RuntimeGeneration:     7,
		ControlEpoch:          0,
		IssuedAt:              now,
		ExpiresAt:             now.Add(5 * time.Minute),
	}
	if adjust != nil {
		adjust(&binding)
	}
	token, err := cloneauth.Issue(key, binding)
	if err != nil {
		t.Fatalf("issue a mirror authorization: %v", err)
	}
	return token
}

// mirrorJobFor is the job Cloud would dispatch for one authorization.
func mirrorJobFor(authorization, repository, credentialID string, version int64) cloneexec.MirrorJob {
	return cloneexec.MirrorJob{
		OperationID:           mirrorOperationID,
		PreparationGeneration: 1,
		ProjectID:             "project-a",
		Repository:            repository,
		Ref:                   mirrorRef,
		CredentialID:          credentialID,
		CredentialVersion:     version,
		RuntimeGeneration:     7,
		ControlEpoch:          0,
		Authorization:         authorization,
	}
}

// TestMirrorPreparationOfARealPrivateRepository is the acceptance evidence for this half of the
// boundary.
func TestMirrorPreparationOfARealPrivateRepository(t *testing.T) {
	deployment := newMirrorDeployment(t, nil)
	repository := deployment.repository
	version := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)

	// The control: the repository really is private. An unauthenticated request is refused, so a mirror
	// that appeared below appeared because of the credential and not because the repository was open.
	assertRepositoryRefusesAnonymousAccess(t, repository)

	authorization := issueMirrorAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, 1, nil)
	result, err := deployment.client.PrepareMirror(context.Background(),
		mirrorJobFor(authorization, repository.URL, acceptanceCredentialID, version))
	if err != nil {
		t.Fatalf("the preparation was refused: %v", err)
	}
	if result.Commit != repository.Commit {
		t.Fatalf("prepared commit = %q, want %q", result.Commit, repository.Commit)
	}
	if result.Ref != mirrorpublish.MirrorRef {
		t.Fatalf("prepared ref = %q, want %q", result.Ref, mirrorpublish.MirrorRef)
	}
	if result.Repository != repository.URL {
		t.Fatalf("prepared repository = %q, want %q", result.Repository, repository.URL)
	}
	if result.Generation != 1 {
		t.Fatalf("generation = %d, want 1", result.Generation)
	}
	if result.CredentialVersion != version {
		t.Fatalf("credential version = %d, want %d", result.CredentialVersion, version)
	}
	// The store, not the response, is what a reader consults: the mirror is readable and its published
	// tree is the repository the fetch produced.
	handle, err := deployment.mirrors.Open(mirror.Key{
		ProjectID: "project-a", Repository: repository.URL, Commit: repository.Commit,
	})
	if err != nil {
		t.Fatalf("the published mirror is not readable: %v", err)
	}
	if handle.Record.Ref != mirrorpublish.MirrorRef {
		t.Fatalf("the published record names ref %q, want %q", handle.Record.Ref, mirrorpublish.MirrorRef)
	}
	// A client can clone it, and what it gets is the file the upstream repository holds. This is the
	// step that proves the published tree is a working bare repository rather than a directory of
	// objects nothing can read.
	clientCheckout := filepath.Join(t.TempDir(), "checkout")
	runGit(t, filepath.Dir(clientCheckout), nil, "clone", "--quiet", handle.Dir, clientCheckout)
	content, err := os.ReadFile(filepath.Join(clientCheckout, acceptanceFileName))
	if err != nil {
		t.Fatalf("read the mirrored file: %v", err)
	}
	if !strings.Contains(string(content), "private repository") {
		t.Fatalf("the mirror does not hold the repository's content: %q", content)
	}
	// And the published repository advertises exactly one ref, at exactly the commit Cloud was told. The
	// `HEAD` line is the same commit seen through a symbolic ref — it is what makes a client that clones
	// without naming a ref get the mirror instead of an unborn branch — so it is the mirror ref appearing
	// twice, not a second ref.
	refs := runGit(t, t.TempDir(), nil, "ls-remote", handle.Dir)
	want := repository.Commit + "\tHEAD\n" + repository.Commit + "\t" + mirrorpublish.MirrorRef
	if refs != want {
		t.Fatalf("the mirror advertises:\n%s\nwant:\n%s", refs, want)
	}
	// The store's own view of the mirror's shape is the ref list, not `ls-remote`: HEAD is not a ref, so
	// the published tree holds exactly one.
	if handle.Record.Ref != mirrorpublish.MirrorRef {
		t.Fatalf("the record names ref %q", handle.Record.Ref)
	}
	// The authorization was consumed exactly once, so the same one cannot prepare a second mirror.
	if _, err := deployment.client.PrepareMirror(context.Background(),
		mirrorJobFor(authorization, repository.URL, acceptanceCredentialID, version)); err == nil {
		t.Fatal("a spent authorization prepared a second mirror")
	}
	// And the credential is gone: not merely unlinked, but absent from everything the executor wrote.
	assertNoTokenUnder(t, deployment.materialRoot, repository.Token)
	entries, err := os.ReadDir(deployment.materialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root still holds %d entries after a successful preparation", len(entries))
	}
}

// TestTheMirrorRouteRefusesACloneAuthorizationWithoutSpendingIt is one direction of the
// purpose-confusion matrix on the real wire: the mirror consumer accepts `mirror_prepare` and nothing
// else.
//
// The authorization is a genuine, validly signed clone authorization. That the refusal does not spend
// it is proven by spending it afterwards through the route that does own it — an assertion about the
// Store's own record rather than about the executor's promise.
func TestTheMirrorRouteRefusesACloneAuthorizationWithoutSpendingIt(t *testing.T) {
	deployment := newMirrorDeployment(t, nil)
	repository := deployment.repository
	version := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	authorization := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)

	_, err := deployment.client.PrepareMirror(context.Background(),
		mirrorJobFor(authorization, repository.URL, acceptanceCredentialID, version))
	var refusal *cloneexec.DispatcherRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a clone authorization on the mirror route was not refused: %v", err)
	}
	if refusal.Code != cloneexec.ReasonCodeBindingMismatch || refusal.Status != 400 {
		t.Fatalf("refusal = %s/%d, want %s/400", refusal.Code, refusal.Status, cloneexec.ReasonCodeBindingMismatch)
	}
	// Nothing was published, either.
	records, err := deployment.mirrors.Records()
	if err != nil {
		t.Fatalf("read the mirror records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("a refused preparation left %d mirror records", len(records))
	}
	// The authorization is still spendable, which is the whole point: it was refused, not burned.
	delivered, err := deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, authorization, acceptanceRuntime())
	if err != nil {
		t.Fatalf("the clone authorization was spent by the mirror route: %v", err)
	}
	if delivered.Token != repository.Token {
		t.Fatal("the clone authorization delivered a different secret")
	}
}

// TestTheCloneRouteRefusesAMirrorAuthorizationWithoutSpendingIt is the other direction: the clone
// consumer accepts `clone` and nothing else.
//
// The job is shaped exactly like a clone job, so the refusal cannot come from the decoder — it comes
// from the signed operation. The mirror authorization is then spent through the mirror route, which
// proves the clone route did not consume it on the way to refusing.
func TestTheCloneRouteRefusesAMirrorAuthorizationWithoutSpendingIt(t *testing.T) {
	deployment := newMirrorDeployment(t, nil)
	repository := deployment.repository
	version := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	authorization := issueMirrorAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, 1, nil)

	_, err := deployment.client.Dispatch(context.Background(), cloneexec.Job{
		ExecutionID:       acceptanceExecutionID,
		Repository:        repository.URL,
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		CredentialID:      acceptanceCredentialID,
		CredentialVersion: version,
		Runtime:           acceptanceRuntime(),
		Authorization:     authorization,
	})
	var refusal *cloneexec.DispatcherRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a mirror authorization on the clone route was not refused: %v", err)
	}
	if refusal.Code != cloneexec.ReasonCodeBindingMismatch || refusal.Status != 400 {
		t.Fatalf("refusal = %s/%d, want %s/400", refusal.Code, refusal.Status, cloneexec.ReasonCodeBindingMismatch)
	}
	// The authorization is still spendable at the route that owns it.
	delivered, err := deployment.storeClient.ResolveMirror(context.Background(), acceptanceCredentialID, authorization)
	if err != nil {
		t.Fatalf("the mirror authorization was spent by the clone route: %v", err)
	}
	if delivered.Token != repository.Token {
		t.Fatal("the mirror authorization delivered a different secret")
	}
}

// TestTheMirrorRouteReportsAnAbsentCapabilityRatherThanARoutingMistake covers the approved default: a
// deployment with no mirror root serves no mirror capability at all.
//
// It is asserted at the wire because the difference is what a dispatcher sees: a capability the
// deployment did not establish must be distinguishable from having sent the request to the wrong path,
// or Cloud would have to guess whether to stop preparing mirrors or to fix an address.
func TestTheMirrorRouteReportsAnAbsentCapabilityRatherThanARoutingMistake(t *testing.T) {
	deployment := newMirrorDeployment(t, nil)
	repository := deployment.repository
	version := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)

	// A server built without the mirror capability, over the same TLS identity and dispatcher.
	pki := newTestPKI(t)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatalf("create the home directory: %v", err)
	}
	executor, err := cloneexec.New(cloneexec.Config{
		MaterialRoot:     filepath.Join(t.TempDir(), "material"),
		CheckoutRoot:     filepath.Join(t.TempDir(), "checkout"),
		HomeDirectory:    home,
		HelperPath:       buildHelper(t),
		GitPath:          gitPath(t),
		VerificationKeys: []ed25519.PublicKey{deployment.signingKey.Public().(ed25519.PublicKey)},
		Store:            deployment.storeClient,
		Log:              testLogger(t),
	})
	if err != nil {
		t.Fatalf("build the clone executor: %v", err)
	}
	server, err := cloneexec.NewServer(executor, cloneexec.ServerConfig{
		TLS: &tls.Config{
			Certificates: []tls.Certificate{pki.issue(t, executorServerIdentity, "127.0.0.1")},
			ClientCAs:    pki.pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
		Caller: cloneDispatcherIdentity,
		Log:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("build the executor server: %v", err)
	}
	listener := pki.startServer(t, server.Handler(), executorServerIdentity, pki.pool)
	client, err := cloneexec.NewClient(cloneexec.ClientConfig{
		Endpoint: listener.URL,
		TLS:      pki.clientTLS(t, cloneDispatcherIdentity),
	})
	if err != nil {
		t.Fatalf("build the dispatch client: %v", err)
	}
	authorization := issueMirrorAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, 1, nil)
	_, err = client.PrepareMirror(context.Background(),
		mirrorJobFor(authorization, repository.URL, acceptanceCredentialID, version))
	var refusal *cloneexec.DispatcherRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("an unconfigured deployment did not refuse the mirror route: %v", err)
	}
	if refusal.Code != cloneexec.ReasonCodeUnavailable || refusal.Status != 500 {
		t.Fatalf("refusal = %s/%d, want %s/500", refusal.Code, refusal.Status, cloneexec.ReasonCodeUnavailable)
	}
	// And the refusal did not spend anything: the capability was absent, so nothing ran.
	if _, err := deployment.storeClient.ResolveMirror(context.Background(), acceptanceCredentialID, authorization); err != nil {
		t.Fatalf("the authorization was spent by a deployment with no mirror capability: %v", err)
	}
}
