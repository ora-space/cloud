package repoexec

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitserver"
)

const hookModeEnv = "ORA_REPOEXEC_TEST_HOOK"

// helperBinary is git-credential-ora built once for the whole test binary.
var helperBinary string

// TestMain lets the test binary double as the Git Server's pre-receive hook, so sync pushes run the
// production hook body and rules, and builds the real credential helper once.
func TestMain(m *testing.M) {
	if os.Getenv(hookModeEnv) == "1" {
		if gitserver.RunPreReceive(os.Stdin, os.Stderr, "git") != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "repoexec-helper-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	helperBinary = filepath.Join(dir, "git-credential-ora")
	build := exec.Command("go", "build", "-o", helperBinary, "github.com/wanglongan587/cloud/cmd/git-credential-ora")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build git-credential-ora: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// pki issues test identities from one CA.
type pki struct {
	t    *testing.T
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
	pool *x509.CertPool
	file string
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	p := &pki{t: t, ca: ca, key: key, dir: t.TempDir(), pool: pool}
	p.file = p.write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return p
}

func (p *pki) write(name string, data []byte) string {
	path := filepath.Join(p.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		p.t.Fatal(err)
	}
	return path
}

type identity struct {
	cert, key string
	pair      tls.Certificate
}

// issue creates a leaf with the given URI SANs; a server leaf also names 127.0.0.1.
func (p *pki) issue(name string, usage x509.ExtKeyUsage, uris ...string) identity {
	p.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	for _, uri := range uris {
		u, _ := url.Parse(uri)
		template.URIs = append(template.URIs, u)
	}
	if usage == x509.ExtKeyUsageServerAuth {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.DNSNames = []string{"localhost"}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.key)
	if err != nil {
		p.t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		p.t.Fatal(err)
	}
	certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		p.t.Fatal(err)
	}
	return identity{cert: p.write(name+".pem", certPEM), key: p.write(name+"-key.pem", keyPEM), pair: pair}
}

// fakeStore is the Git Server's policy: Operation grants keyed by ID; sandbox access always denied.
type fakeStore struct {
	mu         sync.Mutex
	operations map[string]core.GitGrant
	calls      int
	// denyAfter, when positive, denies every call after that many: the Operation ends between the
	// ref advertisement and pre-receive.
	denyAfter int
}

func (f *fakeStore) AuthorizeGitAccess(context.Context, core.GitRuntimeScope, string, string) (core.GitGrant, error) {
	return core.GitGrant{}, &core.Fault{Code: "git_access_denied", Status: 403}
}

func (f *fakeStore) AuthorizeGitOperation(_ context.Context, operation, tenant, project string) (core.GitGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	grant, ok := f.operations[operation]
	if !ok || grant.TenantID != tenant || grant.ProjectID != project || (f.denyAfter > 0 && f.calls > f.denyAfter) {
		return core.GitGrant{}, &core.Fault{Code: "git_access_denied", Status: 403}
	}
	return grant, nil
}

// fakeRedeemer stands in for the Credential Store's redemption listener.
type fakeRedeemer struct {
	mu       sync.Mutex
	calls    int
	username string
	token    string
	fail     bool
}

func (r *fakeRedeemer) Redeem(_ context.Context, authorization string) (Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail {
		return Credential{}, fmt.Errorf("credential store unavailable")
	}
	// The version is the one the authorization names, as the Store guarantees.
	payload, _, _ := strings.Cut(authorization, ".")
	var a struct {
		CredentialVersion int64 `json:"credentialVersion"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	_ = json.Unmarshal(raw, &a)
	return Credential{Username: r.username, Token: r.token, Version: a.CredentialVersion}, nil
}

func (r *fakeRedeemer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// fixture is a running Git Server service listener, an external smart HTTP server, the Cloud
// signing key and the executor's configuration.
type fixture struct {
	t        *testing.T
	store    *fakeStore
	gsRoot   string
	gs       *httptest.Server
	ext      *externalServer
	redeemer *fakeRedeemer
	key      ed25519.PrivateKey
	public   ed25519.PublicKey
	cfg      Config
	logs     *observer.ObservedLogs
	tenant   string
	project  string
	binding  string
	work     string
	token    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required: the executor is tested with the real client")
	}
	gitCA, executorCA := newPKI(t), newPKI(t)
	gsServer := gitCA.issue("git-server", x509.ExtKeyUsageServerAuth)
	executor := executorCA.issue("executor", x509.ExtKeyUsageClientAuth, cloneauth.ExecutorIdentity)
	cloud := gitCA.issue("cloud", x509.ExtKeyUsageClientAuth, "spiffe://ora.local/cloud/git-ref-query")
	f := &fixture{
		t: t, store: &fakeStore{operations: map[string]core.GitGrant{}}, gsRoot: filepath.Join(t.TempDir(), "repositories"),
		tenant: uuid.NewString(), project: uuid.NewString(), binding: uuid.NewString(), token: "ghp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
	f.redeemer = &fakeRedeemer{username: "x-access-token", token: f.token}

	// The real Git Server, service listener only.
	config := gitserver.Config{CertificateFile: gsServer.cert, PrivateKeyFile: gsServer.key, ExecutorCAFile: executorCA.file, SandboxCAFile: gitCA.file, CloudClientCertFile: cloud.cert}
	cloudDER, err := config.CloudClient()
	if err != nil {
		t.Fatal(err)
	}
	env, err := exec.LookPath("env")
	if err != nil {
		t.Fatal(err)
	}
	service, err := gitserver.New(&gitserver.Options{Store: f.store, RepositoryRoot: f.gsRoot, GitProgram: "git", CloudClient: cloudDER, Hook: []string{env, hookModeEnv + "=1", os.Args[0]}, Health: func(context.Context) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	serviceTLS, err := config.ServiceRepositoryTLS()
	if err != nil {
		t.Fatal(err)
	}
	f.gs = httptest.NewUnstartedServer(http.HandlerFunc(service.ServiceRepositoryHandler))
	f.gs.TLS = serviceTLS
	f.gs.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.gs.StartTLS()
	t.Cleanup(func() {
		f.gs.Close()
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})

	f.ext = newExternalServer(t)
	f.public, f.key, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	observed, logs := observer.New(zap.DebugLevel)
	f.logs = logs
	egress, err := NewEgress(net.DefaultResolver, []string{f.ext.hostPort()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = Config{
		PublicKeys: []ed25519.PublicKey{f.public}, Redeemer: f.redeemer, Egress: egress,
		GitServerURL: f.gs.URL, GitServerCAFile: gitCA.file, CertFile: executor.cert, KeyFile: executor.key,
		ExternalCAFile: f.ext.caFile, WorkRoot: filepath.Join(t.TempDir(), "work"), HelperPath: helperBinary, GitProgram: "git",
		MaxTaskBytes: 64 << 20, Limits: Limits{Sync: 30 * time.Minute, Publish: 5 * time.Minute, Probe: 5 * time.Minute},
		Logger: zap.New(observed),
	}
	f.work = t.TempDir()
	f.plain(f.work, "init", "--quiet", "-b", "main")
	return f
}

// executor builds the executor, optionally adjusting the configuration first.
func (f *fixture) executor(mutate ...func(*Config)) *Executor {
	f.t.Helper()
	cfg := f.cfg
	for _, m := range mutate {
		m(&cfg)
	}
	e, err := NewExecutor(&cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

// run executes one task and checks the invariants every task keeps: the result is classified, the
// work root holds nothing afterwards, and the secret appears in neither the result nor the logs.
func (f *fixture) run(e *Executor, task *Task) Result {
	f.t.Helper()
	result := e.Run(context.Background(), task)
	f.checkAfter(&result)
	return result
}

func (f *fixture) checkAfter(result *Result) {
	f.t.Helper()
	if result.Classification == "" {
		f.t.Fatalf("result has no classification: %+v", result)
	}
	if result.CleanupFailed {
		f.t.Fatalf("cleanup failed: %+v", result)
	}
	for _, sub := range []string{tasksDirectory, credentialsDirectory} {
		entries, err := os.ReadDir(filepath.Join(f.cfg.WorkRoot, sub))
		if err != nil {
			f.t.Fatal(err)
		}
		if len(entries) != 0 {
			f.t.Fatalf("%s still holds %d entries after the task", sub, len(entries))
		}
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), f.token) {
		f.t.Fatalf("the secret reached the result: %s", encoded)
	}
	for _, entry := range f.logs.All() {
		line := entry.Message + fmt.Sprint(entry.ContextMap())
		if strings.Contains(line, f.token) {
			f.t.Fatalf("the secret reached a log line: %s", line)
		}
	}
}

// authorization signs a grant valid now and returns it with the task that matches it exactly.
func (f *fixture) task(a *cloneauth.Authorization, timeout time.Duration) *Task {
	f.t.Helper()
	if a.AuthorizationID == "" {
		a.AuthorizationID = uuid.NewString()
	}
	a.ExecutorIdentity = cloneauth.ExecutorIdentity
	if a.IssuedAt.IsZero() {
		a.IssuedAt = time.Now()
		a.ExpiresAt = a.IssuedAt.Add(2 * time.Minute)
	}
	token, err := cloneauth.Sign(f.key, *a)
	if err != nil {
		f.t.Fatal(err)
	}
	return &Task{
		Authorization: token, Purpose: a.Purpose, OperationID: a.OperationID, VerificationID: a.VerificationID,
		TenantID: a.TenantID, ProjectID: a.ProjectID, BindingID: a.BindingID, Repository: a.Repository,
		Branch: a.Branch, ExpectedTrackingOID: a.ExpectedTrackingOID, SourceRef: a.SourceRef, SourceCommit: a.SourceCommit,
		TargetRef: a.TargetRef, ExpectedOldOID: a.ExpectedOldOID, Service: a.Service, CredentialID: a.CredentialID,
		CredentialVersion: a.CredentialVersion, TimeoutSeconds: int(timeout / time.Second),
	}
}

// credential pins a credential on a.
func credential(a *cloneauth.Authorization) *cloneauth.Authorization {
	a.CredentialID, a.CredentialVersion = uuid.NewString(), 3
	return a
}

// syncOperation registers a sync Operation's Git Server grant and returns its ID.
func (f *fixture) syncOperation(branch, expected string) string {
	id := uuid.NewString()
	old := expected
	if zeroOID(old) {
		old = ""
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	f.store.operations[id] = core.GitGrant{TenantID: f.tenant, ProjectID: f.project, Writable: []core.GitWritableRef{
		{Prefix: core.GitSnapshotRefPrefix(f.binding), Rule: core.GitCreateOnly},
		{Ref: core.GitUpstreamHeadPrefix(f.binding) + branch, Rule: core.GitTrackingCAS, ExpectedOld: old},
	}}
	return id
}

// readOperation registers a read-only (publish) Operation and returns its ID.
func (f *fixture) readOperation() string {
	id := uuid.NewString()
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	f.store.operations[id] = core.GitGrant{TenantID: f.tenant, ProjectID: f.project}
	return id
}

func (f *fixture) syncAuthorization(repo, branch, expected string) *cloneauth.Authorization {
	return &cloneauth.Authorization{Purpose: cloneauth.PurposeSync, OperationID: f.syncOperation(branch, expected), TenantID: f.tenant, ProjectID: f.project, BindingID: f.binding, Repository: repo, Branch: branch, ExpectedTrackingOID: expected}
}

// plain runs the test's own git (not the executor's) in dir.
func (f *fixture) plain(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit adds a commit with content on the work repository's current HEAD and returns its OID.
func (f *fixture) commit(content string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.work, "file"), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.plain(f.work, "add", "file")
	f.plain(f.work, "commit", "--quiet", "-m", content)
	return f.plain(f.work, "rev-parse", "HEAD")
}

// orphan starts an unrelated history in the work repository.
func (f *fixture) orphan(branch string) { f.plain(f.work, "checkout", "--quiet", "--orphan", branch) }

// gsRepository is the Project repository on disk, created as the Git Server would on first write.
func (f *fixture) gsRepository() string {
	path := filepath.Join(f.gsRoot, f.tenant, f.project+".git")
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			f.t.Fatal(err)
		}
		f.plain(filepath.Dir(path), "init", "--quiet", "--bare", path)
	}
	return path
}

// seedGitServer writes ref on the Git Server's disk (the preset tool's role in acceptance).
func (f *fixture) seedGitServer(commit, ref string) {
	f.plain(f.work, "push", "--quiet", "--force", f.gsRepository(), commit+":"+ref)
}

// gsRef reads a ref from the Git Server's disk; "" when absent.
func (f *fixture) gsRef(ref string) string {
	path := filepath.Join(f.gsRoot, f.tenant, f.project+".git")
	cmd := exec.Command("git", "--git-dir", path, "rev-parse", "--verify", "--quiet", ref)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// writeRandom writes n incompressible bytes into the work repository.
func (f *fixture) writeRandom(name string, n int) {
	f.t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.work, name), data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}
