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

	"github.com/wanglongan587/cloud/internal/core"
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

	pki := newGitPKI(t)
	pki.issue(t, "server", "", x509.ExtKeyUsageServerAuth)
	pki.issue(t, "sandbox", "spiffe://ora.local/git-access/tenant/"+tenant+"/runtime/"+workspace+"/generation/"+strconv.FormatInt(generation, 10), x509.ExtKeyUsageClientAuth)
	pki.issue(t, "stale", "spiffe://ora.local/git-access/tenant/"+tenant+"/runtime/"+workspace+"/generation/"+strconv.FormatInt(generation-1, 10), x509.ExtKeyUsageClientAuth)
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
	service, err := gitserver.New(&gitserver.Options{
		GitProgram: "git",
		Repository: &gitserver.RepositoryOptions{
			Store: f.store, RepositoryRoot: filepath.Join(t.TempDir(), "repositories"),
			CloudClient: cloudLeaf, Hook: []string{hook, "-pre-receive"},
		},
		Health: f.store.Pool.PingContext,
	})
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

	remote := repoServer.URL + "/" + tenant + "/" + project + ".git"
	git := func(dir, who, executionID string, args ...string) (string, error) {
		base := []string{"-c", "http.sslCert=" + filepath.Join(pki.dir, who+".pem"), "-c", "http.sslKey=" + filepath.Join(pki.dir, who+"-key.pem"), "-c", "http.sslCAInfo=" + deployment.SandboxCAFile, "-c", "http.extraHeader=" + gitserver.ExecutionHeader + ": " + executionID}
		cmd := exec.Command("git", append(base, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
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
	if got := queryDeliveryRef(t, refServer.URL, pki.pool, cloud, tenant, project, ref); got != head {
		t.Fatalf("ref query = %q, want the pushed commit %q", got, head)
	}

	// The delivery execution ending in Cloud ends its repository access on the next request.
	_, err = f.store.Pool.Exec(`UPDATE node_executions SET result='{"outcome":"revision_delivered"}' WHERE execution_id=$1`, execution)
	must(t, err)
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
