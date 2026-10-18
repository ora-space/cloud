package gitserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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

	"github.com/wanglongan587/cloud/internal/core"
)

const hookModeEnv = "ORA_GIT_SERVER_TEST_HOOK"

// TestMain lets the test binary double as the pre-receive hook, so pushes in these tests run the
// production hook body against the production hook channel.
func TestMain(m *testing.M) {
	if os.Getenv(hookModeEnv) == "1" {
		if RunPreReceive(os.Stdin, os.Stderr, "git") != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeStore answers from mutable test state; denials use core's Fault, outages a plain error.
type fakeStore struct {
	mu     sync.Mutex
	grants map[string]core.GitGrant
	// operations are the current grants of repository Operations, keyed by Operation ID.
	operations map[string]core.GitGrant
	down       bool
	calls      int
	// denyAfter, when positive, denies every call after that many calls: it models the execution
	// ending between a push's ref advertisement and its pre-receive check.
	denyAfter int
	seen      []core.GitRuntimeScope
}

func (f *fakeStore) AuthorizeGitAccess(_ context.Context, scope core.GitRuntimeScope, execution, project string) (core.GitGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, scope)
	if f.down {
		return core.GitGrant{}, errors.New("database unavailable")
	}
	access, ok := f.grants[execution]
	if !ok || access.ProjectID != project || (f.denyAfter > 0 && f.calls > f.denyAfter) {
		return core.GitGrant{}, &core.Fault{Code: "git_access_denied", Status: 403}
	}
	return access, nil
}

func (f *fakeStore) AuthorizeGitOperation(_ context.Context, operation, tenant, project string) (core.GitGrant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		return core.GitGrant{}, errors.New("database unavailable")
	}
	grant, ok := f.operations[operation]
	if !ok || grant.TenantID != tenant || grant.ProjectID != project || (f.denyAfter > 0 && f.calls > f.denyAfter) {
		return core.GitGrant{}, &core.Fault{Code: "git_access_denied", Status: 403}
	}
	return grant, nil
}

func (f *fakeStore) set(fn func(*fakeStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// pki issues test identities from one CA, as the deployment does for every purpose.
type pki struct {
	t    *testing.T
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
	pool *x509.CertPool
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
	p.write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return p
}

func (p *pki) write(name string, data []byte) string {
	path := filepath.Join(p.dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		p.t.Fatal(err)
	}
	return path
}

// identity issues a leaf and writes it as <name>.pem / <name>-key.pem.
type identity struct {
	cert, key string
	der       []byte
	pair      tls.Certificate
}

func (p *pki) issue(name, uri string, usage x509.ExtKeyUsage) identity {
	p.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	if uri != "" {
		u, _ := url.Parse(uri)
		template.URIs = []*url.URL{u}
	}
	if usage == x509.ExtKeyUsageServerAuth {
		template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		template.DNSNames = []string{"git-server"}
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
	return identity{cert: p.write(name+".pem", certPEM), key: p.write(name+"-key.pem", keyPEM), der: der, pair: pair}
}

// harness is a running git-server with both listeners on real TLS.
type harness struct {
	t                 *testing.T
	store             *fakeStore
	pki               *pki
	service           *Service
	repo, ref, svc    *httptest.Server
	executorPKI       *pki
	executor          identity
	root              string
	tenant, workspace string
	project           string
	sandbox, cloud    identity
	config            Config
	hookOf            []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required: the repository boundary is tested with the real client and http-backend")
	}
	h := &harness{t: t, store: &fakeStore{grants: map[string]core.GitGrant{}, operations: map[string]core.GitGrant{}}, pki: newPKI(t), executorPKI: newPKI(t), root: filepath.Join(t.TempDir(), "repositories"), tenant: uuid.NewString(), workspace: uuid.NewString(), project: uuid.NewString()}
	server := h.pki.issue("server", "", x509.ExtKeyUsageServerAuth)
	h.sandbox = h.pki.issue("sandbox", "spiffe://ora.local/git-access/tenant/"+h.tenant+"/runtime/"+h.workspace+"/generation/3", x509.ExtKeyUsageClientAuth)
	h.cloud = h.pki.issue("cloud", "spiffe://ora.local/cloud/git-ref-query", x509.ExtKeyUsageClientAuth)
	h.executor = h.executorPKI.issue("executor", ExecutorIdentity, x509.ExtKeyUsageClientAuth)
	h.config = Config{CertificateFile: server.cert, PrivateKeyFile: server.key, SandboxCAFile: filepath.Join(h.pki.dir, "ca.pem"), CloudClientCertFile: h.cloud.cert, ExecutorCAFile: filepath.Join(h.executorPKI.dir, "ca.pem")}
	env, err := exec.LookPath("env")
	if err != nil {
		t.Fatal(err)
	}
	h.hookOf = []string{env, hookModeEnv + "=1", os.Args[0]}
	h.start()
	return h
}

// start (re)opens the service on the same repository root, as a process restart does.
func (h *harness) start() {
	h.t.Helper()
	cloud, err := h.config.CloudClient()
	if err != nil {
		h.t.Fatal(err)
	}
	service, err := New(&Options{Store: h.store, RepositoryRoot: h.root, GitProgram: "git", CloudClient: cloud, Hook: h.hookOf, Health: func(context.Context) error { return nil }})
	if err != nil {
		h.t.Fatal(err)
	}
	repoTLS, err := h.config.RepositoryTLS()
	if err != nil {
		h.t.Fatal(err)
	}
	refTLS, err := h.config.RefTLS()
	if err != nil {
		h.t.Fatal(err)
	}
	serviceTLS, err := h.config.ServiceRepositoryTLS()
	if err != nil {
		h.t.Fatal(err)
	}
	h.service = service
	h.svc = httptest.NewUnstartedServer(http.HandlerFunc(service.ServiceRepositoryHandler))
	h.svc.TLS = serviceTLS
	h.svc.Config.ErrorLog = log.New(io.Discard, "", 0)
	h.svc.StartTLS()
	h.repo = httptest.NewUnstartedServer(http.HandlerFunc(service.RepositoryHandler))
	h.repo.TLS = repoTLS
	h.repo.Config.ErrorLog = log.New(io.Discard, "", 0)
	h.repo.StartTLS()
	h.ref = httptest.NewUnstartedServer(http.HandlerFunc(service.RefHandler))
	h.ref.TLS = refTLS
	h.ref.StartTLS()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.service == nil {
		return
	}
	h.repo.Close()
	h.svc.Close()
	h.ref.Close()
	if err := h.service.Close(); err != nil {
		h.t.Error(err)
	}
	h.service = nil
}

func (h *harness) url() string { return h.repo.URL + "/" + h.tenant + "/" + h.project + ".git" }

// serviceURL is the same Project repository on the Repo Executor's listener.
func (h *harness) serviceURL() string {
	return h.svc.URL + "/" + h.tenant + "/" + h.project + ".git"
}

// operation registers a repository Operation's current grant and returns its ID.
func (h *harness) operation(writable ...core.GitWritableRef) string {
	id := uuid.NewString()
	h.store.set(func(f *fakeStore) {
		f.operations[id] = core.GitGrant{TenantID: h.tenant, ProjectID: h.project, Writable: writable}
	})
	return id
}

// executorGit runs the real client as the Repo Executor on the service listener.
func (h *harness) executorGit(dir, operation string, args ...string) (string, error) {
	h.t.Helper()
	base := []string{"-c", "http.sslCert=" + h.executor.cert, "-c", "http.sslKey=" + h.executor.key, "-c", "http.sslCAInfo=" + h.config.SandboxCAFile}
	if operation != "" {
		base = append(base, "-c", "http.extraHeader="+OperationHeader+": "+operation)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnvironment(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// grant registers a delivery execution's current permission in the fake policy and returns the
// one delivery ref it may create.
func (h *harness) grant(execution string) string {
	ref := core.GitDeliveryRefPrefix + uuid.NewString()
	h.allow(execution, core.GitWritableRef{Ref: ref, Rule: core.GitCreateOnly})
	return ref
}

// allow registers an execution whose grant is exactly the given rule set; none is read-only.
func (h *harness) allow(execution string, writable ...core.GitWritableRef) {
	h.store.set(func(f *fakeStore) {
		f.grants[execution] = core.GitGrant{TenantID: h.tenant, ProjectID: h.project, Writable: writable}
	})
}

// git runs the real client exactly as the Node will: identity, CA and execution passed per
// command, nothing written to any Git configuration.
func (h *harness) git(dir string, who identity, execution string, args ...string) (string, error) {
	h.t.Helper()
	base := []string{"-c", "http.sslCert=" + who.cert, "-c", "http.sslKey=" + who.key, "-c", "http.sslCAInfo=" + h.config.SandboxCAFile}
	if execution != "" {
		base = append(base, "-c", "http.extraHeader="+ExecutionHeader+": "+execution)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(gitEnvironment(), "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// checkout creates a local repository with n commits and returns its directory and HEAD.
func (h *harness) checkout(commits int) (string, string) {
	h.t.Helper()
	dir := h.t.TempDir()
	if out, err := h.git(dir, h.sandbox, "", "init", "--quiet"); err != nil {
		h.t.Fatal(out)
	}
	for i := range commits {
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte(strings.Repeat("x", i+1)), 0o600); err != nil {
			h.t.Fatal(err)
		}
		if out, err := h.git(dir, h.sandbox, "", "add", "file"); err != nil {
			h.t.Fatal(out)
		}
		if out, err := h.git(dir, h.sandbox, "", "commit", "--quiet", "-m", "c"); err != nil {
			h.t.Fatal(out)
		}
	}
	if commits == 0 {
		return dir, ""
	}
	head, err := h.git(dir, h.sandbox, "", "rev-parse", "HEAD")
	if err != nil {
		h.t.Fatal(head)
	}
	return dir, strings.TrimSpace(head)
}

// remoteRef reads a ref straight from the bare repository on disk.
func (h *harness) remoteRef(ref string) string {
	h.t.Helper()
	commit, err := h.service.repos.resolve(context.Background(), h.tenant, h.project, ref)
	if err != nil {
		h.t.Fatal(err)
	}
	return commit
}

// queryRef calls the ref listener with the given client identity.
func (h *harness) queryRef(who *identity, tenant, project, ref string) (int, map[string]any) {
	h.t.Helper()
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: h.pki.pool}
	if who != nil {
		config.Certificates = []tls.Certificate{who.pair}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	defer client.CloseIdleConnections()
	q := url.Values{"tenantId": {tenant}, "projectId": {project}, "ref": {ref}}
	response, err := client.Get(h.ref.URL + "/internal/v1/refs?" + q.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, body
}

func TestDeliveryPushCreatesOnlyItsRefAndReplayIsUpToDate(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, head := h.checkout(2)
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err != nil {
		t.Fatalf("first delivery push failed: %s", out)
	}
	if got := h.remoteRef(ref); got != head {
		t.Fatalf("delivery ref = %q, want %q", got, head)
	}
	// Replaying the same attempt sends no update: Git reports up-to-date instead of failing.
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", "--porcelain", h.url(), "HEAD:"+ref); err != nil || !strings.Contains(out, "[up to date]") {
		t.Fatalf("replayed delivery push = %v: %s", err, out)
	}
	status, body := h.queryRef(&h.cloud, h.tenant, h.project, ref)
	if status != http.StatusOK || body["commit"] != head {
		t.Fatalf("ref query = %d %v, want the pushed commit", status, body)
	}
	if status, _ := h.queryRef(&h.cloud, h.tenant, h.project, core.GitDeliveryRefPrefix+uuid.NewString()); status != http.StatusNotFound {
		t.Fatalf("unknown delivery ref query = %d, want 404", status)
	}
}

// Every push that is not "create exactly my delivery ref" is refused whole, and nothing is written.
func TestPushRulesRejectEveryOtherUpdate(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, first := h.checkout(1)
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err != nil {
		t.Fatalf("delivery push failed: %s", out)
	}
	if out, err := h.git(dir, h.sandbox, "", "commit", "--quiet", "--allow-empty", "-m", "next"); err != nil {
		t.Fatal(out)
	}
	other := core.GitDeliveryRefPrefix + uuid.NewString()
	h.store.set(func(f *fakeStore) {
		f.grants["deliver-2"] = core.GitGrant{TenantID: h.tenant, ProjectID: h.project, Writable: []core.GitWritableRef{{Ref: other, Rule: core.GitCreateOnly}}}
	})
	tree, _ := h.git(dir, h.sandbox, "", "rev-parse", "HEAD^{tree}")
	for _, c := range []struct {
		name      string
		execution string
		args      []string
	}{
		{"update an existing delivery ref", "deliver-1", []string{"push", "--force", h.url(), "HEAD:" + ref}},
		{"delete a delivery ref", "deliver-1", []string{"push", h.url(), ":" + ref}},
		{"create a ref the execution does not own", "deliver-1", []string{"push", h.url(), "HEAD:" + other}},
		{"create a branch", "deliver-1", []string{"push", h.url(), "HEAD:refs/heads/main"}},
		{"own ref together with another ref", "deliver-2", []string{"push", "--atomic", h.url(), "HEAD:" + other, "HEAD:refs/heads/side"}},
		{"own ref together with another ref, non-atomic client", "deliver-2", []string{"push", h.url(), "HEAD:" + other, "HEAD:refs/tags/side"}},
		{"point the delivery ref at a tree", "deliver-2", []string{"push", h.url(), strings.TrimSpace(tree) + ":" + other}},
		{"push options", "deliver-2", []string{"push", "-o", "x", h.url(), "HEAD:" + other}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out, err := h.git(dir, h.sandbox, c.execution, c.args...); err == nil {
				t.Fatalf("push was accepted: %s", out)
			}
			if got := h.remoteRef(ref); got != first {
				t.Fatalf("existing delivery ref changed to %q", got)
			}
			for _, r := range []string{other, "refs/heads/main", "refs/heads/side", "refs/tags/side"} {
				if got := h.remoteRef(r); got != "" {
					t.Fatalf("rejected push wrote %s", r)
				}
			}
		})
	}
}

func TestReadOnlyExecutionFetchesButCannotPush(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, head := h.checkout(1)
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err != nil {
		t.Fatalf("delivery push failed: %s", out)
	}
	h.allow("restore-1")
	fresh, _ := h.checkout(0)
	if out, err := h.git(fresh, h.sandbox, "restore-1", "fetch", h.url(), ref); err != nil {
		t.Fatalf("restore fetch failed: %s", out)
	}
	if got, _ := h.git(fresh, h.sandbox, "", "rev-parse", "FETCH_HEAD"); strings.TrimSpace(got) != head {
		t.Fatalf("restore fetched %q, want %q", got, head)
	}
	if out, err := h.git(fresh, h.sandbox, "restore-1", "push", h.url(), "FETCH_HEAD:"+core.GitDeliveryRefPrefix+uuid.NewString()); err == nil {
		t.Fatalf("read-only execution pushed: %s", out)
	}
}

// The authorization that admitted the ref advertisement is decided again in pre-receive, so an
// execution that ends while the pack is in flight writes nothing.
func TestPreReceiveRechecksCurrentState(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, _ := h.checkout(1)
	// info/refs and the receive-pack POST are authorized; the hook's check is the third call.
	h.store.set(func(f *fakeStore) { f.calls, f.denyAfter = 0, 2 })
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err == nil {
		t.Fatalf("push after the execution ended was accepted: %s", out)
	}
	if got := h.remoteRef(ref); got != "" {
		t.Fatalf("rejected push wrote the delivery ref: %s", got)
	}
	if h.store.calls != 3 {
		t.Fatalf("authorization calls = %d, want advertisement, request and pre-receive", h.store.calls)
	}
}

// A restart replaces the hook directory; existing repositories keep enforcing the rules.
func TestRestartKeepsRepositoriesAndRules(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, head := h.checkout(1)
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err != nil {
		t.Fatalf("delivery push failed: %s", out)
	}
	h.stop()
	h.start()
	if got := h.remoteRef(ref); got != head {
		t.Fatalf("delivery ref after restart = %q", got)
	}
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:refs/heads/main"); err == nil {
		t.Fatalf("push after restart bypassed pre-receive: %s", out)
	}
	ref2 := h.grant("deliver-2")
	if out, err := h.git(dir, h.sandbox, "deliver-2", "push", h.url(), "HEAD:"+ref2); err != nil {
		t.Fatalf("delivery push after restart failed: %s", out)
	}
}

// Raw requests pin status codes the Git client does not surface.
func TestRepositoryBoundaryResponses(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, _ := h.checkout(1)
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err != nil {
		t.Fatalf("delivery push failed: %s", out)
	}
	model := h.pki.issue("model", "spiffe://ora.local/model-access/tenant/"+h.tenant+"/runtime/"+h.workspace+"/generation/3", x509.ExtKeyUsageClientAuth)
	other := uuid.NewString()
	get := func(who *identity, path, execution string) int {
		t.Helper()
		config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: h.pki.pool}
		if who != nil {
			config.Certificates = []tls.Certificate{who.pair}
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
		defer client.CloseIdleConnections()
		request, _ := http.NewRequest(http.MethodGet, h.repo.URL+path, nil)
		if execution != "" {
			request.Header.Set(ExecutionHeader, execution)
		}
		response, err := client.Do(request)
		if err != nil {
			return 0
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	advertise := "/" + h.tenant + "/" + h.project + ".git/info/refs?service=git-upload-pack"
	for _, c := range []struct {
		name      string
		who       *identity
		path      string
		execution string
		want      int
	}{
		{"authorized advertisement", &h.sandbox, advertise, "deliver-1", http.StatusOK},
		{"missing execution", &h.sandbox, advertise, "", http.StatusBadRequest},
		{"unknown execution", &h.sandbox, advertise, "nobody", http.StatusForbidden},
		{"model-access certificate", &model, advertise, "deliver-1", http.StatusUnauthorized},
		{"Cloud ref-query certificate", &h.cloud, advertise, "deliver-1", http.StatusUnauthorized},
		{"no client certificate", nil, advertise, "deliver-1", 0},
		{"another tenant's path", &h.sandbox, "/" + other + "/" + h.project + ".git/info/refs?service=git-upload-pack", "deliver-1", http.StatusForbidden},
		{"dumb HTTP", &h.sandbox, "/" + h.tenant + "/" + h.project + ".git/HEAD", "deliver-1", http.StatusNotFound},
		{"non-canonical ID", &h.sandbox, "/" + strings.ToUpper(h.tenant) + "/" + h.project + ".git/info/refs?service=git-upload-pack", "deliver-1", http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := get(c.who, c.path, c.execution); got != c.want {
				t.Fatalf("status = %d, want %d", got, c.want)
			}
		})
	}
	// A read of a Project with no repository yet looks exactly like an unauthorized read.
	h.store.set(func(f *fakeStore) { f.grants["restore-2"] = core.GitGrant{TenantID: h.tenant, ProjectID: other} })
	if got := get(&h.sandbox, "/"+h.tenant+"/"+other+".git/info/refs?service=git-upload-pack", "restore-2"); got != http.StatusForbidden {
		t.Fatalf("absent repository read = %d, want 403", got)
	}
	if _, err := os.Stat(filepath.Join(h.root, h.tenant, other+".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a read created a repository")
	}
	h.store.set(func(f *fakeStore) { f.down = true })
	if got := get(&h.sandbox, advertise, "deliver-1"); got != http.StatusServiceUnavailable {
		t.Fatalf("database outage = %d, want 503", got)
	}
	for _, scope := range h.store.seen {
		if scope != (core.GitRuntimeScope{TenantID: h.tenant, WorkspaceID: h.workspace, Generation: 3}) {
			t.Fatalf("policy saw scope %+v, not the certificate's", scope)
		}
	}
}

func TestRefQueryAcceptsOnlyTheCloudIdentity(t *testing.T) {
	h := newHarness(t)
	ref := core.GitDeliveryRefPrefix + uuid.NewString()
	sameCA := h.pki.issue("cloud-impostor", "spiffe://ora.local/cloud/git-ref-query", x509.ExtKeyUsageClientAuth)
	for _, c := range []struct {
		name string
		who  *identity
		ref  string
		want int
	}{
		{"Cloud identity, absent repository", &h.cloud, ref, http.StatusNotFound},
		{"sandbox identity", &h.sandbox, ref, http.StatusUnauthorized},
		{"another leaf from the same CA", &sameCA, ref, http.StatusUnauthorized},
		{"no client certificate", nil, ref, http.StatusUnauthorized},
		{"ref outside deliveries", &h.cloud, "refs/heads/main", http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			if status, _ := h.queryRef(c.who, h.tenant, h.project, c.ref); status != c.want {
				t.Fatalf("status = %d, want %d", status, c.want)
			}
		})
	}
}

// A git without http-backend (Alpine's git package alone) must stop startup, not answer 500.
func TestNewRefusesGitWithoutHTTPBackend(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho "+dir+"\n"), 0o700); err != nil { //nolint:gosec // G306: the fake git must be executable.
		t.Fatal(err)
	}
	_, err := New(&Options{Store: &fakeStore{}, RepositoryRoot: filepath.Join(dir, "repositories"), GitProgram: fake, CloudClient: []byte{1}, Hook: []string{"true"}, Health: func(context.Context) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "http-backend") {
		t.Fatalf("New with a git lacking http-backend = %v, want a startup error", err)
	}
}

func TestRuntimeScopeAcceptsOnlyGitAccessPurpose(t *testing.T) {
	tenant, workspace := uuid.NewString(), uuid.NewString()
	for _, c := range []struct {
		uri string
		ok  bool
	}{
		{"spiffe://ora.local/git-access/tenant/" + tenant + "/runtime/" + workspace + "/generation/7", true},
		{"spiffe://ora.local/model-access/tenant/" + tenant + "/runtime/" + workspace + "/generation/7", false},
		{"spiffe://ora.local/tenant/" + tenant + "/runtime/" + workspace + "/generation/7", false},
		{"spiffe://ora.local/git-access/tenant/" + tenant + "/runtime/" + workspace + "/generation/0", false},
		{"spiffe://ora.local/git-access/tenant/" + tenant + "/runtime/" + workspace + "/generation/07", false},
		{"spiffe://example.invalid/git-access/tenant/" + tenant + "/runtime/" + workspace + "/generation/7", false},
		{"spiffe://ora.local/git-access/tenant/x/runtime/" + workspace + "/generation/7", false},
	} {
		u, _ := url.Parse(c.uri)
		r := &http.Request{TLS: &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{}}, PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{u}}}}}
		scope, err := RuntimeScope(r)
		if (err == nil) != c.ok {
			t.Fatalf("%s: err=%v, want ok=%v", c.uri, err, c.ok)
		}
		if c.ok && scope != (core.GitRuntimeScope{TenantID: tenant, WorkspaceID: workspace, Generation: 7}) {
			t.Fatalf("scope = %+v", scope)
		}
	}
}
