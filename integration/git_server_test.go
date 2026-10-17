package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitrefquery"
	"github.com/wanglongan587/cloud/internal/gitserver"
)

// gitPKI writes one CA and the leaves the Git Server boundary distinguishes.
type gitPKI struct {
	dir  string
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newGitPKI(t *testing.T) *gitPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	must(t, err)
	ca, err := x509.ParseCertificate(der)
	must(t, err)
	p := &gitPKI{dir: t.TempDir(), ca: ca, key: key, pool: x509.NewCertPool()}
	p.pool.AddCert(ca)
	must(t, os.WriteFile(filepath.Join(p.dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return p
}

// issue writes <name>.pem and <name>-key.pem and returns the loaded pair.
func (p *gitPKI) issue(t *testing.T, name, uri string, usage x509.ExtKeyUsage) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	must(t, err)
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	if uri != "" {
		u, err := url.Parse(uri)
		must(t, err)
		template.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.key)
	must(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	must(t, os.WriteFile(filepath.Join(p.dir, name+".pem"), certPEM, 0o600))
	must(t, os.WriteFile(filepath.Join(p.dir, name+"-key.pem"), keyPEM, 0o600))
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	must(t, err)
	return pair
}

// The ADR's step-2 acceptance: a real git client pushes through a real Git Server whose decisions
// come from the real Cloud Store on PostgreSQL, for a delivery execution produced by the shipped
// session-end and dispatch paths. Ending the execution in Cloud ends repository access on the next
// request, and an unreachable database fails closed as unavailable.
func TestGitServerAuthorizesDeliveryPushFromCloudState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required for the Git Server boundary")
	}
	f := setup(t)
	scene := deliveringScene(t, f)
	execution := f.deliverRevision(t, scene)
	var workspace, tenant, project, work string
	var generation int64
	must(t, f.store.Pool.QueryRow(`SELECT w.id,w.tenant_id,w.project_id,w.runtime_generation,e.work_id FROM node_executions e JOIN workspaces w ON w.id=e.workspace_id WHERE e.execution_id=$1`, execution).Scan(&workspace, &tenant, &project, &generation, &work))
	f.acknowledgeRuntime(t, workspace)

	server := startGitServer(t, f, t.TempDir())
	server.sandbox(t, "sandbox", tenant, workspace, generation)
	server.sandbox(t, "stale", tenant, workspace, generation-1)
	remote := server.remote(tenant, project)
	git := server.git
	checkout := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet"}, {"commit", "--quiet", "--allow-empty", "-m", "delivered"}} {
		if out, err := git(checkout, "sandbox", "", args...); err != nil {
			t.Fatal(out)
		}
	}
	head, err := git(checkout, "sandbox", "", "rev-parse", "HEAD")
	must(t, err)
	head = strings.TrimSpace(head)
	ref := core.GitDeliveryRefPrefix + work

	if out, err := git(checkout, "stale", execution, "push", remote, "HEAD:"+ref); err == nil {
		t.Fatalf("a certificate from an earlier sandbox generation pushed: %s", out)
	}
	if out, err := git(checkout, "sandbox", execution, "push", remote, "HEAD:refs/ora/deliveries/"+scene.runID); err == nil {
		t.Fatalf("a push outside the execution's delivery ref was accepted: %s", out)
	}
	if out, err := git(checkout, "sandbox", execution, "push", remote, "HEAD:"+ref); err != nil {
		t.Fatalf("authorized delivery push failed: %s", out)
	}
	if got := queryDeliveryRef(t, server.refs.URL, server.pki.pool, server.cloud, tenant, project, ref); got != head {
		t.Fatalf("ref query = %q, want the pushed commit %q", got, head)
	}
	if out, err := git(checkout, "sandbox", execution, "push", remote, "HEAD:"+ref); err != nil || !strings.Contains(out, "up-to-date") {
		t.Fatalf("re-pushing the same commit to the delivery ref must be a no-op: %v %s", err, out)
	}

	// Contract D6: the delivery may also fast-forward its own Workspace branch, the one its input
	// names, and nothing else under any Workspace prefix.
	in := f.deliveryInputOf(execution)
	workspaceRef := in.S("workspaceRef")
	if workspaceRef != core.GitWorkspaceRefPrefix(workspace)+"main" {
		t.Fatalf("the delivery input names %q, want the run Workspace's branch", workspaceRef)
	}
	if out, err := git(checkout, "sandbox", execution, "push", remote, "HEAD:"+workspaceRef); err != nil {
		t.Fatalf("the delivery must be able to create its Workspace branch: %s", out)
	}
	for _, other := range []string{core.GitWorkspaceRefPrefix(workspace) + "other", core.GitWorkspaceRefPrefix(uuid.NewString()) + "main", "refs/heads/main"} {
		if out, err := git(checkout, "sandbox", execution, "push", remote, "HEAD:"+other); err == nil {
			t.Fatalf("the delivery wrote %s outside its grant: %s", other, out)
		}
	}
	if out, err := git(checkout, "sandbox", "", "commit", "--quiet", "--allow-empty", "-m", "descendant"); err != nil {
		t.Fatal(out)
	}
	if out, err := git(checkout, "sandbox", execution, "push", remote, "HEAD:"+workspaceRef); err != nil {
		t.Fatalf("a fast-forward of the Workspace branch must be accepted: %s", out)
	}
	if out, err := git(checkout, "sandbox", execution, "push", remote, "+"+head+":"+workspaceRef); err == nil {
		t.Fatalf("a non-fast-forward of the Workspace branch was accepted: %s", out)
	}

	// Revision D3 against the real ref query: Cloud verifies the pushed delivery ref through the Git
	// Server's ref listener with its pinned identity, then registers the Revision; the execution's
	// result ends its repository access on the next request.
	refClient, err := gitrefquery.New(server.refs.URL, &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: server.pki.pool, Certificates: []tls.Certificate{server.cloud}}}}, 5*time.Second)
	must(t, err)
	f.store.GitServer = &core.GitServer{RepositoryBaseURL: f.store.GitServer.RepositoryBaseURL, Refs: refClient}
	f.useObjectStore()
	if _, err := f.deliveryTerminal(scene, execution, "", 1, deliveredResult(scene, in, head), "revision_delivered"); err != nil {
		t.Fatalf("a delivery whose ref the Git Server confirms must register: %v", err)
	}
	if row := f.revisionRow(scene.runID); row == nil || row.DeliveryRef != ref || row.FinalCommit != head {
		t.Fatalf("the registered Revision must name the verified delivery ref and commit, got %+v", row)
	}
	if out, err := git(t.TempDir(), "sandbox", execution, "ls-remote", remote); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("an ended execution still reached the repository: %v %s", err, out)
	}

	// Without PostgreSQL nothing is authorized, and the answer is unavailable rather than denied.
	_, err = f.store.Pool.Exec(`UPDATE node_executions SET result=NULL WHERE execution_id=$1`, execution)
	must(t, err)
	must(t, f.store.Pool.Close())
	if out, err := git(t.TempDir(), "sandbox", execution, "ls-remote", remote); err == nil || !strings.Contains(out, "503") {
		t.Fatalf("a database outage did not fail closed as unavailable: %v %s", err, out)
	}
}

// acknowledgeRuntime confirms the run Workspace's Node binding through the Controller's real
// runtime_ack action, which is the evidence a production delivery already holds.
func (f *fixture) acknowledgeRuntime(t *testing.T, workspace string) {
	t.Helper()
	var epoch, version int64
	var node string
	must(t, f.store.Pool.QueryRow(`SELECT c.control_epoch,c.version,n.id FROM runtime_controls c JOIN node_instances n ON n.workspace_id=c.workspace_id AND n.ended_at IS NULL WHERE c.workspace_id=$1`, workspace).Scan(&epoch, &version, &node))
	_, err := f.store.Control(context.Background(), &core.ControlRequest{
		Action:  "runtime_ack",
		Body:    core.Object{"epoch": 1, "workspaceId": workspace, "controlEpoch": epoch, "controlVersion": version, "nodeInstanceId": node, "inputClosed": false, "unfinishedExecutionIds": []any{}},
		Service: controllerClaims(),
	})
	must(t, err)
}

// queryDeliveryRef calls the ref listener as Cloud.
func queryDeliveryRef(t *testing.T, base string, roots *x509.CertPool, cloud tls.Certificate, tenant, project, ref string) string {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cloud}}}}
	defer client.CloseIdleConnections()
	response, err := client.Get(base + "/internal/v1/refs?" + url.Values{"tenantId": {tenant}, "projectId": {project}, "ref": {ref}}.Encode())
	must(t, err)
	defer response.Body.Close()
	var body map[string]string
	must(t, json.NewDecoder(response.Body).Decode(&body))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ref query status %d: %v", response.StatusCode, body)
	}
	return body["commit"]
}

// gitServerHarness is a real Git Server on loopback TLS: the shipped pre-receive hook, policy from
// the fixture's Cloud Store on PostgreSQL, repositories under root, and certificates from one PKI.
type gitServerHarness struct {
	pki        *gitPKI
	repository *httptest.Server
	refs       *httptest.Server
	cloud      tls.Certificate
	caFile     string
}

func startGitServer(t *testing.T, f *fixture, root string) *gitServerHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required for the Git Server boundary")
	}
	pki := newGitPKI(t)
	pki.issue(t, "server", "", x509.ExtKeyUsageServerAuth)
	cloud := pki.issue(t, "cloud", "spiffe://ora.local/cloud/git-ref-query", x509.ExtKeyUsageClientAuth)
	deployment := gitserver.Config{CertificateFile: filepath.Join(pki.dir, "server.pem"), PrivateKeyFile: filepath.Join(pki.dir, "server-key.pem"), SandboxCAFile: filepath.Join(pki.dir, "ca.pem"), CloudClientCertFile: filepath.Join(pki.dir, "cloud.pem")}
	// The hook is the shipped command, so the pre-receive path under test is the deployed one.
	hook := filepath.Join(t.TempDir(), "git-server")
	build := exec.Command("go", "build", "-o", hook, "../cmd/git-server")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build git-server: %v: %s", err, out)
	}
	cloudLeaf, err := deployment.CloudClient()
	must(t, err)
	service, err := gitserver.New(&gitserver.Options{Store: f.store, RepositoryRoot: root, GitProgram: "git", CloudClient: cloudLeaf, Hook: []string{hook, "-pre-receive"}, Health: f.store.Pool.PingContext})
	must(t, err)
	repoTLS, err := deployment.RepositoryTLS()
	must(t, err)
	refTLS, err := deployment.RefTLS()
	must(t, err)
	repoServer := httptest.NewUnstartedServer(http.HandlerFunc(service.RepositoryHandler))
	repoServer.TLS = repoTLS
	repoServer.StartTLS()
	refServer := httptest.NewUnstartedServer(http.HandlerFunc(service.RefHandler))
	refServer.TLS = refTLS
	refServer.StartTLS()
	t.Cleanup(func() {
		repoServer.Close()
		refServer.Close()
		must(t, service.Close())
	})
	return &gitServerHarness{pki: pki, repository: repoServer, refs: refServer, cloud: cloud, caFile: deployment.SandboxCAFile}
}

// sandbox issues the git-access certificate a sandbox of workspace's generation holds, as who.
func (h *gitServerHarness) sandbox(t *testing.T, who, tenant, workspace string, generation int64) {
	t.Helper()
	h.pki.issue(t, who, "spiffe://ora.local/git-access/tenant/"+tenant+"/runtime/"+workspace+"/generation/"+strconv.FormatInt(generation, 10), x509.ExtKeyUsageClientAuth)
}

func (h *gitServerHarness) remote(tenant, project string) string {
	return h.repository.URL + "/" + tenant + "/" + project + ".git"
}

// git runs a real git client in dir as the sandbox certificate who, declaring executionID.
func (h *gitServerHarness) git(dir, who, executionID string, args ...string) (string, error) {
	base := []string{"-c", "http.sslCert=" + filepath.Join(h.pki.dir, who+".pem"), "-c", "http.sslKey=" + filepath.Join(h.pki.dir, who+"-key.pem"), "-c", "http.sslCAInfo=" + h.caFile, "-c", "http.extraHeader=" + gitserver.ExecutionHeader + ": " + executionID}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// serverRef reads one ref straight from the Git Server's repository on disk.
func serverRef(t *testing.T, root, tenant, project, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "--git-dir", gitserver.RepositoryPath(root, tenant, project), "for-each-ref", "--format=%(objectname)", ref).Output()
	must(t, err)
	return strings.TrimSpace(string(out))
}
