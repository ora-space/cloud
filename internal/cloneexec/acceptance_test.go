// This file is the acceptance test for the credentialed-clone boundary, and it is deliberately built
// out of real components rather than doubles: a real private HTTPS Git repository served by
// `git http-backend` behind HTTP Basic authentication, a real Credential Service with a real volume
// and a real key-encryption key, real mutually authenticated TLS between all three parties, the real
// `git-credential-ora` helper binary, and the real `core.CloneDispatcher` that Cloud runs.
//
// A double would prove that the executor calls the things it calls. Only the real chain proves the
// obligation the decisions actually state — that a token a user stored can be redeemed once, delivered
// to Git through the deployment-fixed helper, used to clone a repository that refuses anonymous
// access, and then be gone — which is what `20261010-controlled-node-git-authentication-isolation`
// D11 and D14-B and `20261010-user-managed-git-secret-store` D3 and D10 require before the
// `repository_credential_executor_unavailable` refusal may be lifted.
//
// The one property this test cannot establish by itself is the operating-system separation between
// the workload identity and the credential-executing identity, because the whole chain runs inside one
// test process under one uid. That separation is a deployment property — two identities, two
// volumes, a directory the workload cannot traverse — and it is established by the deployment
// configuration and proven by the container-level acceptance run, not by a unit test.
package cloneexec_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/credentialmaterial"
	"github.com/wanglongan587/cloud/internal/credentialstore"
)

// The deployment's fixed service identities, spelled the way the rest of the platform spells them.
const (
	storeServerIdentity     = "spiffe://ora/cloud/credential-store"
	executorServerIdentity  = "spiffe://ora/node/clone-executor"
	ingressWriterIdentity   = "spiffe://ora/cloud/credential-ingress"
	brokerResolverIdentity  = "spiffe://ora/node/credential-executor"
	kekOperatorIdentity     = "spiffe://ora/ops/kek-operator"
	cloneDispatcherIdentity = "spiffe://ora/cloud/clone-dispatcher"
)

// The identifiers the test uses for one clone. They are canonical lowercase UUIDs because both the
// Store and the materialization layout turn them into path segments.
const (
	acceptanceExecutionID       = "6f1d0a52-1c3b-4c7e-9a10-2b8f4d6e0c31"
	acceptanceCredentialID      = "2c9f4e18-7a35-4d61-8b02-5e7c1a9d3f40"
	acceptanceOtherCredentialID = "7e4b1c93-5d28-4a07-9f61-3b8e0d5a2c74"
	acceptanceNodeID            = "1b7c9d02-4e5a-4f83-9c16-8d2a6b0e7f31"
	acceptanceNodeInstance      = "9d3e5a17-2c84-4b60-a1f7-6e0b8c4d2a95"
	acceptanceNodeIncarn        = "4a6f8c20-1d73-4e59-b8a4-3c7d9e1f5b62"
	acceptanceOtherIncarn       = "ffffffff-1111-4222-8333-444444444444"
)

const (
	// helperTokenUsername is the username `git-credential-ora` presents. It is duplicated rather than
	// imported because the helper is a separate binary in package main, and pinning the spelling here is
	// what makes the test fail if the helper ever starts sending something else.
	helperTokenUsername = "x-access-token"
	acceptanceRepoName  = "private"
	acceptanceFileName  = "README.md"
)

// testPKI is a throwaway certificate authority for one test.
//
// One authority signs every certificate here rather than the three the deployment separates, because
// what these tests exercise is the identity comparison each server performs after the chain has been
// validated, not the deployment's decision to split its authorities. The deployment's own split is
// configuration, and `clone_executor_test.go` covers the configuration.
type testPKI struct {
	certificate *x509.Certificate
	key         ed25519.PrivateKey
	pool        *x509.CertPool
	// CAFile is the PEM file Git verifies the repository server against.
	CAFile string
	serial int64
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the test authority key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ora test authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
	if err != nil {
		t.Fatalf("create the test authority: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the test authority: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	caFile := filepath.Join(dir, "authority.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(caFile, encoded, 0o644); err != nil {
		t.Fatalf("write the test authority: %v", err)
	}
	return &testPKI{certificate: certificate, key: key, pool: pool, CAFile: caFile}
}

// issue returns one leaf certificate.
//
// `identity` becomes a SPIFFE URI subject alternative name, which is how every service on this
// platform proves who it is. A non-empty `host` also makes the certificate usable as a server
// certificate for 127.0.0.1, which is the address every test listener binds.
func (p *testPKI) issue(t *testing.T, identity, host string) tls.Certificate {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a leaf key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(atomic.AddInt64(&p.serial, 1) + 1),
		Subject:      pkix.Name{CommonName: identity},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if identity != "" {
		parsed, err := url.Parse(identity)
		if err != nil {
			t.Fatalf("parse the service identity %q: %v", identity, err)
		}
		template.URIs = []*url.URL{parsed}
	}
	if host != "" {
		template.DNSNames = []string{host}
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.ExtKeyUsage = append(template.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.certificate, public, p.key)
	if err != nil {
		t.Fatalf("create a leaf certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// clientTLS is the configuration one service uses to call another: its own identity, and the
// authority the peer's certificate chains to.
func (p *testPKI) clientTLS(t *testing.T, identity string) *tls.Config {
	t.Helper()
	return &tls.Config{
		Certificates: []tls.Certificate{p.issue(t, identity, "")},
		RootCAs:      p.pool,
		MinVersion:   tls.VersionTLS12,
	}
}

// startServer serves one handler over TLS.
//
// A nil client authority means the listener accepts anonymous callers, which is what the private Git
// server does — Git authenticates with HTTP Basic, not with a client certificate. Every other
// listener here requires and verifies a client certificate, because every other listener either
// delivers a secret or spends an authorization.
func (p *testPKI) startServer(t *testing.T, handler http.Handler, identity string, clientCAs *x509.CertPool) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		Certificates: []tls.Certificate{p.issue(t, identity, "127.0.0.1")},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAs != nil {
		server.TLS.ClientCAs = clientCAs
		server.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// buildHelper builds the deployment-fixed credential helper once per test process.
//
// The binary is the real one: it is the program the deployment installs at a fixed path and the
// program Git executes. Building it rather than calling its `run` function is what makes this an
// acceptance test — the helper is reached the way Git reaches it, as a separate process with argv
// the executor chose and a request on standard input.
func buildHelper(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		directory, err := os.MkdirTemp("", "ora-credential-helper-")
		if err != nil {
			helperBuildErr = err
			return
		}
		path := filepath.Join(directory, "git-credential-ora")
		// The binary is built into a process-lifetime temporary directory rather than a test's, because
		// every test in this package uses the same build.
		command := exec.Command("go", "build", "-o", path, "github.com/wanglongan587/cloud/cmd/git-credential-ora")
		if output, err := command.CombinedOutput(); err != nil {
			helperBuildErr = fmt.Errorf("build the credential helper: %w: %s", err, output)
			return
		}
		helperBuildPath = path
	})
	if helperBuildErr != nil {
		t.Fatal(helperBuildErr)
	}
	return helperBuildPath
}

var (
	helperOnce      sync.Once
	helperBuildPath string
	helperBuildErr  error
)

// testLogger routes the executor's own log into the test's output.
//
// The executor deliberately keeps a refusal's cause out of its response — a Git error and a Store code
// are for its log, not for the caller — so a failing acceptance run is only diagnosable if that log
// reaches the test. It never carries a credential: the executor logs an execution id, a repository,
// and a reason code, and nothing derived from the token or the authorization.
func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	encoder := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	return zap.New(zapcore.NewCore(encoder, zapcore.AddSync(&testLogWriter{t: t}), zapcore.DebugLevel))
}

// testLogWriter forwards one log line at a time to the test's output.
type testLogWriter struct{ t *testing.T }

func (w *testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// gitPath returns the absolute path of the Git binary, which is what the executor is configured with.
func gitPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is not installed: %v", err)
	}
	return path
}

// runGit runs one Git command for the test harness itself.
//
// This is the harness setting up a repository, not the executor cloning one, so it uses the ambient
// environment. The executor never calls anything in this file.
func runGit(t *testing.T, directory string, environment []string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

// privateRepository is a real bare repository served over HTTPS behind HTTP Basic authentication.
type privateRepository struct {
	server *httptest.Server
	// URL is the repository's canonical https URL, which is what the executor clones and what an
	// authorization is bound to.
	URL string
	// Commit is the revision the repository's default branch points at.
	Commit string
	// Bare is the path of the bare repository the HTTP server exports. A test that needs to arrange the
	// remote's own state — a ref moved by a third party, a server-side hook that refuses an update —
	// reaches it here rather than over the protocol, because both of those are things only the remote's
	// operator can do.
	Bare string
	// Token is the password the server accepts. It is the value a user would have stored, and it is
	// generated per test run so a test that accidentally printed it would print a value that means
	// nothing outside that run.
	Token string
	// requests counts the authenticated smart-HTTP traffic this server served, by service. Part of what
	// a push must prove is what it did *not* do — one write, and a read-only reconciliation rather than a
	// second write — and the wire is the only place that is observable from outside the executor.
	requests *repositoryRequests
	// authorities is what a client verifies the repository's certificate against.
	authorities *x509.CertPool
}

func newPrivateRepository(t *testing.T, pki *testPKI) *privateRepository {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not installed: %v", err)
	}
	directory := t.TempDir()
	projectRoot := filepath.Join(directory, "repositories")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatalf("create the repository root: %v", err)
	}
	bare := filepath.Join(projectRoot, acceptanceRepoName)
	runGit(t, directory, nil, "init", "--bare", "--initial-branch=main", bare)
	work := filepath.Join(directory, "work")
	runGit(t, directory, nil, "init", "--initial-branch=main", work)
	content := "# private repository\n\nThis file exists only inside the test.\n"
	if err := os.WriteFile(filepath.Join(work, acceptanceFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write the repository content: %v", err)
	}
	runGit(t, work, nil, "add", acceptanceFileName)
	runGit(t, work, []string{
		"GIT_AUTHOR_NAME=ora test", "GIT_AUTHOR_EMAIL=test@ora.invalid",
		"GIT_COMMITTER_NAME=ora test", "GIT_COMMITTER_EMAIL=test@ora.invalid",
	}, "commit", "-m", "the only commit")
	runGit(t, work, nil, "remote", "add", "origin", bare)
	runGit(t, work, nil, "push", "origin", "main")
	commit := runGit(t, work, nil, "rev-parse", "HEAD")

	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("generate the repository token: %v", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)

	execPath := runGit(t, directory, nil, "--exec-path")
	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + projectRoot, "GIT_HTTP_EXPORT_ALL=1"},
	}
	requests := &repositoryRequests{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, password, ok := r.BasicAuth()
		// The password is the credential, and the comparison is constant-time. The username is the
		// fixed label `git-credential-ora` documents — the same label GitHub accepts for token
		// authentication — and requiring it here is what proves the credential arrived through the real
		// helper binary rather than from anything else on the machine. The realm is always demanded, so
		// an unauthenticated request is refused rather than served.
		if !ok || presented != helperTokenUsername ||
			subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ora private repositories"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		// Only authenticated traffic is counted, so a test's arithmetic is about what a credential was
		// used for rather than about how many times something was refused at the door.
		requests.count(r)
		backend.ServeHTTP(w, r)
	})
	server := pki.startServer(t, handler, "", nil)
	return &privateRepository{
		server:      server,
		URL:         server.URL + "/" + acceptanceRepoName,
		Commit:      commit,
		Bare:        bare,
		Token:       token,
		requests:    requests,
		authorities: pki.pool,
	}
}

// credentialService is a real Credential Service: a real volume, a real key-encryption key, and its
// own mutually authenticated listener.
type credentialService struct {
	server *httptest.Server
	store  *credentialstore.Store
}

func newCredentialService(t *testing.T, pki *testPKI, signingPublic ed25519.PublicKey, configure func(*credentialstore.Config)) *credentialService {
	t.Helper()
	directory := t.TempDir()
	volume := filepath.Join(directory, "volume")
	if err := os.Mkdir(volume, 0o700); err != nil {
		t.Fatalf("create the credential store volume: %v", err)
	}
	// A generated key-encryption key, in a temporary path, for the duration of this test. It is the
	// deployment's key in shape and nothing else: it is never logged and never leaves the test's
	// temporary directory.
	kek := make([]byte, 32)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("generate the key-encryption key: %v", err)
	}
	keyFile := filepath.Join(directory, "keyring.json")
	keyring, err := json.Marshal(map[string]any{
		"primary": "kek-test-1",
		"keys":    []map[string]string{{"id": "kek-test-1", "key": base64.StdEncoding.EncodeToString(kek)}},
	})
	if err != nil {
		t.Fatalf("encode the keyring: %v", err)
	}
	if err := os.WriteFile(keyFile, keyring, 0o600); err != nil {
		t.Fatalf("write the keyring: %v", err)
	}
	publicKeyFile := filepath.Join(directory, "authorization-key.pem")
	encoded, err := x509.MarshalPKIXPublicKey(signingPublic)
	if err != nil {
		t.Fatalf("encode the authorization public key: %v", err)
	}
	if err := os.WriteFile(publicKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatalf("write the authorization public key: %v", err)
	}
	config := credentialstore.Config{
		Volume:                     volume,
		KeyFile:                    keyFile,
		AuthorizationPublicKeyFile: publicKeyFile,
	}
	if configure != nil {
		configure(&config)
	}
	store, err := credentialstore.Open(config)
	if err != nil {
		t.Fatalf("open the credential store: %v", err)
	}
	server, err := credentialstore.NewServer(store, credentialstore.ServerConfig{
		TLS: &tls.Config{
			Certificates: []tls.Certificate{pki.issue(t, storeServerIdentity, "127.0.0.1")},
			ClientCAs:    pki.pool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
		Identities: credentialstore.IdentityConfig{
			IngressWriter:  ingressWriterIdentity,
			BrokerResolver: brokerResolverIdentity,
			KEKOperator:    kekOperatorIdentity,
		},
	})
	if err != nil {
		t.Fatalf("build the credential store server: %v", err)
	}
	return &credentialService{
		server: pki.startServer(t, server.Handler(), storeServerIdentity, pki.pool),
		store:  store,
	}
}

// executorDeployment is one fully wired credentialed-clone boundary.
type executorDeployment struct {
	repository *privateRepository
	service    *credentialService
	executor   *cloneexec.Executor
	// storeClient is the executor's own `broker-resolver` client, exposed so the tests that are about
	// redemption rather than about cloning can drive the real boundary directly.
	storeClient  *cloneexec.StoreClient
	dispatcher   *core.CloneDispatcher
	signingKey   ed25519.PrivateKey
	materialRoot string
	checkoutRoot string
}

func newExecutorDeployment(t *testing.T, configureStore func(*credentialstore.Config)) *executorDeployment {
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
	executor, err := cloneexec.New(cloneexec.Config{
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
		t.Fatalf("build the executor: %v", err)
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
	executorServer := pki.startServer(t, server.Handler(), executorServerIdentity, pki.pool)
	client, err := cloneexec.NewClient(cloneexec.ClientConfig{
		Endpoint: executorServer.URL,
		TLS:      pki.clientTLS(t, cloneDispatcherIdentity),
		Timeout:  3 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build Cloud's dispatch client: %v", err)
	}
	return &executorDeployment{
		repository:   repository,
		service:      store,
		executor:     executor,
		storeClient:  storeClient,
		signingKey:   signingKey,
		materialRoot: materialRoot,
		checkoutRoot: filepath.Join(directory, "checkout"),
		dispatcher: &core.CloneDispatcher{
			Client:     client,
			SigningKey: signingKey,
			Lifetime:   5 * time.Minute,
		},
	}
}

// runtime is the fencing set every authorization and redemption in these tests is bound to.
func acceptanceRuntime() cloneexec.Runtime {
	return cloneexec.Runtime{
		NodeID:            acceptanceNodeID,
		NodeInstanceID:    acceptanceNodeInstance,
		NodeIncarnationID: acceptanceNodeIncarn,
		RuntimeGeneration: 7,
		ControlEpoch:      0,
	}
}

// issueAuthorization mints one authorization, the way Cloud does.
func issueAuthorization(t *testing.T, key ed25519.PrivateKey, credentialID, repository string, adjust func(*cloneauth.Binding)) string {
	t.Helper()
	now := time.Now().UTC()
	runtime := acceptanceRuntime()
	binding := cloneauth.Binding{
		AuthorizationID:   "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46",
		Operation:         cloneauth.OperationClone,
		TenantID:          "tenant-a",
		OwnerUserID:       "user-a",
		ProjectID:         "project-a",
		WorkspaceID:       "workspace-a",
		ExecutionID:       acceptanceExecutionID,
		CredentialID:      credentialID,
		CredentialVersion: 1,
		Repository:        repository,
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		NodeID:            runtime.NodeID,
		NodeInstanceID:    runtime.NodeInstanceID,
		NodeIncarnationID: runtime.NodeIncarnationID,
		RuntimeGeneration: runtime.RuntimeGeneration,
		ControlEpoch:      runtime.ControlEpoch,
		IssuedAt:          now,
		ExpiresAt:         now.Add(5 * time.Minute),
	}
	if adjust != nil {
		adjust(&binding)
	}
	token, err := cloneauth.Issue(key, binding)
	if err != nil {
		t.Fatalf("issue an authorization: %v", err)
	}
	return token
}

// storeCredential writes one token into the real Credential Service and returns its version.
func (d *executorDeployment) storeCredential(t *testing.T, credentialID, token string) int64 {
	t.Helper()
	_, version, err := d.service.store.Write(context.Background(), credentialID, token)
	if err != nil {
		t.Fatalf("store a credential: %v", err)
	}
	return version
}

// reasonCodeOf returns the refusal code a client reported, or the empty string for an infrastructure
// failure. The two are deliberately distinguished: a refusal is an outcome of this clone, while an
// unreachable service is not.
func reasonCodeOf(err error) string {
	var refusal interface{ ReasonCode() string }
	if errors.As(err, &refusal) {
		return refusal.ReasonCode()
	}
	return ""
}

// TestCredentialedCloneOfARealPrivateRepository is the acceptance evidence for the whole boundary.
func TestCredentialedCloneOfARealPrivateRepository(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	version := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)

	// The control: the repository really is private. An unauthenticated request is refused, so a clone
	// that succeeds below succeeded because of the credential and not because the repository was open.
	assertRepositoryRefusesAnonymousAccess(t, repository)

	outcome, err := deployment.dispatcher.Execute(context.Background(), core.CredentialedClone{
		AuthorizationID:   "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46",
		TenantID:          "tenant-a",
		OwnerUserID:       "user-a",
		ProjectID:         "project-a",
		WorkspaceID:       "workspace-a",
		ExecutionID:       acceptanceExecutionID,
		CredentialID:      acceptanceCredentialID,
		CredentialVersion: version,
		Repository:        repository.URL,
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		Runtime: core.CredentialedCloneRuntime{
			NodeID:            acceptanceNodeID,
			NodeInstanceID:    acceptanceNodeInstance,
			NodeIncarnationID: acceptanceNodeIncarn,
			RuntimeGeneration: 7,
			ControlEpoch:      0,
		},
	})
	if err != nil {
		t.Fatalf("the credentialed clone was refused: %v", err)
	}
	if outcome.Commit != repository.Commit {
		t.Fatalf("cloned commit = %q, want %q", outcome.Commit, repository.Commit)
	}
	if outcome.CredentialVersion != version {
		t.Fatalf("credential version = %d, want %d", outcome.CredentialVersion, version)
	}
	// The checkout is real and holds the repository's content, which is what the workload reads.
	content, err := os.ReadFile(filepath.Join(outcome.Directory, acceptanceFileName))
	if err != nil {
		t.Fatalf("read the cloned file: %v", err)
	}
	if !strings.Contains(string(content), "private repository") {
		t.Fatalf("the checkout does not hold the repository's content: %q", content)
	}
	// And the credential is gone: not merely unlinked, but absent from everything the executor wrote.
	assertNoTokenUnder(t, deployment.materialRoot, repository.Token)
	assertNoTokenUnder(t, deployment.checkoutRoot, repository.Token)
	entries, err := os.ReadDir(deployment.materialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the materialization root still holds %d entries after a successful clone", len(entries))
	}
}

// TestTheAuthorizationIsConsumedOnceAndReplayIsRefused drives the real Credential Service directly.
func TestTheAuthorizationIsConsumedOnceAndReplayIsRefused(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	runtime := acceptanceRuntime()
	authorization := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)

	delivered, err := deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, authorization, runtime)
	if err != nil {
		t.Fatalf("the first redemption was refused: %v", err)
	}
	if delivered.Token != repository.Token {
		t.Fatal("the first redemption delivered a different secret")
	}
	_, err = deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, authorization, runtime)
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeAuthorizationConsumed {
		t.Fatalf("replay refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeAuthorizationConsumed, err)
	}
}

// TestARedemptionIsRefusedWhenTheAuthorizationNamesAnotherCredential is the cross-tenant case: the
// credential id in the authorization is the scope, so an authorization Cloud issued for one tenant's
// credential cannot be spent on another's even by a caller holding a valid `broker-resolver`
// certificate.
func TestARedemptionIsRefusedWhenTheAuthorizationNamesAnotherCredential(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	deployment.storeCredential(t, acceptanceOtherCredentialID, "another-tenant-token")
	runtime := acceptanceRuntime()
	authorization := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)

	_, err := deployment.storeClient.Resolve(context.Background(), acceptanceOtherCredentialID, authorization, runtime)
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeAuthorizationBinding {
		t.Fatalf("cross-credential refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeAuthorizationBinding, err)
	}
}

// TestARedemptionIsRefusedUnderAnotherRuntime proves the fencing values are checked and not merely
// carried: an authorization issued for one Node incarnation cannot be spent by a workload declaring
// another.
func TestARedemptionIsRefusedUnderAnotherRuntime(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	authorization := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)
	other := acceptanceRuntime()
	other.NodeIncarnationID = acceptanceOtherIncarn

	_, err := deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, authorization, other)
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeAuthorizationBinding {
		t.Fatalf("runtime-mismatch refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeAuthorizationBinding, err)
	}
}

// TestARedemptionIsRefusedAfterRevocation covers the revoke half: once the user withdraws a token,
// no authorization can obtain it.
func TestARedemptionIsRefusedAfterRevocation(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	if err := deployment.service.store.Revoke(context.Background(), acceptanceCredentialID); err != nil {
		t.Fatalf("revoke the credential: %v", err)
	}
	authorization := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)

	_, err := deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, authorization, acceptanceRuntime())
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeRevoked {
		t.Fatalf("revoked refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeRevoked, err)
	}
}

// TestARedemptionIsRefusedForAVersionTheServiceNoLongerHolds is the version-conflict case, and it also
// covers the positive half: after a rotation the superseded version stays deliverable for its
// retention window, so a clone already admitted under it settles instead of failing midway.
func TestARedemptionIsRefusedForAVersionTheServiceNoLongerHolds(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	first := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	if _, err := deployment.service.store.Rotate(context.Background(), acceptanceCredentialID, "the-replacement-token"); err != nil {
		t.Fatalf("rotate the credential: %v", err)
	}
	// The version that existed is still deliverable inside the retention window, and the version that
	// never existed is refused.
	held := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)
	delivered, err := deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, held, acceptanceRuntime())
	if err != nil {
		t.Fatalf("the superseded version was refused inside its retention window: %v", err)
	}
	if delivered.Version != first {
		t.Fatalf("delivered version = %d, want %d", delivered.Version, first)
	}
	missing := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, func(binding *cloneauth.Binding) {
		binding.AuthorizationID = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
		binding.CredentialVersion = first + 99
	})
	_, err = deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, missing, acceptanceRuntime())
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeVersionUnavailable {
		t.Fatalf("version-conflict refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeVersionUnavailable, err)
	}
}

// TestTheDefaultDeploymentRefusesEveryRedemption is the fail-closed default: a Credential Service
// configured with no verification key cannot decide whether an authorization is genuine, so it
// refuses rather than trusting the caller. This is the shipped state, and it is the reason the
// `repository_credential_executor_unavailable` refusal may stay in force.
func TestTheDefaultDeploymentRefusesEveryRedemption(t *testing.T) {
	deployment := newExecutorDeployment(t, func(config *credentialstore.Config) {
		config.AuthorizationPublicKeyFile = ""
	})
	repository := deployment.repository
	deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	authorization := issueAuthorization(t, deployment.signingKey, acceptanceCredentialID, repository.URL, nil)

	_, err := deployment.storeClient.Resolve(context.Background(), acceptanceCredentialID, authorization, acceptanceRuntime())
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeAuthorizationUnverifiable {
		t.Fatalf("unconfigured refusal code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeAuthorizationUnverifiable, err)
	}
}

// TestAFailedCloneStillDestroysTheCredential covers failure cleanup through the real chain: the clone
// is admitted, the authorization is spent, the token is materialized — and Git then fails, because the
// repository does not exist. The executor must report the failure and must still prove the credential
// gone.
func TestAFailedCloneStillDestroysTheCredential(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	repository := deployment.repository
	version := deployment.storeCredential(t, acceptanceCredentialID, repository.Token)
	// The same host and the same credential, but a repository that is not there. Everything up to the
	// clone succeeds, so the failure path is the one under test.
	//
	// The reference goes through the same canonicalization admission applies before it signs, because
	// the executor re-derives the repository identity from the job and compares it to the binding. A
	// `.git` suffix here would be stripped by the executor and kept by the binding, and the clone would
	// be refused as a binding mismatch before Git ever ran — which is a different test.
	missing, err := cloneauth.CanonicalRepository(strings.TrimSuffix(repository.URL, "/"+acceptanceRepoName) + "/absent")
	if err != nil {
		t.Fatalf("canonicalize a repository that does not exist: %v", err)
	}

	_, err = deployment.dispatcher.Execute(context.Background(), core.CredentialedClone{
		AuthorizationID:   "8b2d4f60-3a17-4c85-9e01-7f3b5d9a1c46",
		TenantID:          "tenant-a",
		OwnerUserID:       "user-a",
		ProjectID:         "project-a",
		WorkspaceID:       "workspace-a",
		ExecutionID:       acceptanceExecutionID,
		CredentialID:      acceptanceCredentialID,
		CredentialVersion: version,
		Repository:        missing,
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		Runtime: core.CredentialedCloneRuntime{
			NodeID:            acceptanceNodeID,
			NodeInstanceID:    acceptanceNodeInstance,
			NodeIncarnationID: acceptanceNodeIncarn,
			RuntimeGeneration: 7,
		},
	})
	if err == nil {
		t.Fatal("a clone of a repository that does not exist succeeded")
	}
	if code := reasonCodeOf(err); code != cloneexec.ReasonCodeCloneFailed {
		t.Fatalf("failure code = %q, want %q (error: %v)", code, cloneexec.ReasonCodeCloneFailed, err)
	}
	assertNoTokenUnder(t, deployment.materialRoot, repository.Token)
	entries, err := os.ReadDir(deployment.materialRoot)
	if err != nil {
		t.Fatalf("read the materialization root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed clone left %d entries in the materialization root", len(entries))
	}
}

// TestTheStartupSweepRemovesWhatACrashLeftBehind covers the one failure a running process cannot
// repair: material that survived a crash, which only the next start can destroy.
func TestTheStartupSweepRemovesWhatACrashLeftBehind(t *testing.T) {
	deployment := newExecutorDeployment(t, nil)
	if err := credentialmaterial.Stage(deployment.materialRoot, acceptanceExecutionID, deployment.repository.Token, "127.0.0.1", "/"+acceptanceRepoName); err != nil {
		t.Fatalf("stage material as a crashed run would have: %v", err)
	}
	removed, err := deployment.executor.Sweep()
	if err != nil {
		t.Fatalf("sweep the materialization root: %v", err)
	}
	if removed != 1 {
		t.Fatalf("swept %d entries, want 1", removed)
	}
	assertNoTokenUnder(t, deployment.materialRoot, deployment.repository.Token)
}

// assertRepositoryRefusesAnonymousAccess proves the repository is private.
//
// It checks the wire directly rather than by running Git, because the point is a property of the
// server: without credentials the smart-HTTP endpoint answers 401 and names a realm. A Git run would
// additionally depend on the harness's own credential configuration, which is not what is being
// asserted.
func assertRepositoryRefusesAnonymousAccess(t *testing.T, repository *privateRepository) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, repository.URL+"/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatalf("build the anonymous request: %v", err)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    repository.authorities,
			MinVersion: tls.VersionTLS12,
		}},
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("reach the repository without credentials: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous request answered %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if response.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("an anonymous refusal named no authentication realm")
	}
}

// assertNoTokenUnder walks a directory tree and proves one value appears nowhere in it.
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
