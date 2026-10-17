package integration

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitserver"
)

func expectDenied(t *testing.T, err error, why string) {
	t.Helper()
	var fault *core.Fault
	if !errors.As(err, &fault) || fault.Status != 403 || fault.Code != "git_access_denied" {
		t.Fatalf("%s: error=%v, want uniform 403 git_access_denied", why, err)
	}
}

// An Operation's Git access follows its current state and its Project's lifecycle: running in a
// provisioning, active or deleting Project grants access; anything else, a deleted Project, or a
// mismatched tenant or Project is the same denial.
func TestGitOperationAccessFollowsOperationAndProjectLifecycle(t *testing.T) {
	f := setup(t)
	f.withoutSnapshot = true
	pid := f.create("operation-access").O("resource").S("id")
	bid := f.bindingOf(pid).S("id")
	ctx := context.Background()
	var sync string
	must(t, f.store.Pool.QueryRow("SELECT id FROM repository_operations WHERE binding_id=$1", bid).Scan(&sync))

	_, err := f.store.AuthorizeGitOperation(ctx, sync, f.tid, pid)
	expectDenied(t, err, "a pending sync has no Git access")
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='running' WHERE id=$1", sync)))
	want := core.GitGrant{TenantID: f.tid, ProjectID: pid, Writable: []core.GitWritableRef{{Prefix: core.GitSnapshotRefPrefix(bid), Rule: core.GitCreateOnly}}}
	for _, lifecycle := range []string{"provisioning", "active", "deleting"} {
		must(t, firstErr(f.store.Pool.Exec("UPDATE projects SET lifecycle=$2 WHERE id=$1", pid, lifecycle)))
		got, err := f.store.AuthorizeGitOperation(ctx, sync, f.tid, pid)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("running sync in a %s Project = %+v, %v; want the Binding's snapshot prefix", lifecycle, got, err)
		}
	}
	_, err = f.store.AuthorizeGitOperation(ctx, sync, uuid.NewString(), pid)
	expectDenied(t, err, "another tenant")
	_, err = f.store.AuthorizeGitOperation(ctx, sync, f.tid, uuid.NewString())
	expectDenied(t, err, "another Project")
	_, err = f.store.AuthorizeGitOperation(ctx, "not-a-uuid", f.tid, pid)
	expectDenied(t, err, "a malformed Operation ID")

	publish, err := f.repositoryOperation(pid, bid, core.RepositoryPublish, "running")
	must(t, err)
	if got, err := f.store.AuthorizeGitOperation(ctx, publish, f.tid, pid); err != nil || got.Writes() {
		t.Fatalf("running publish = %+v, %v; want read-only", got, err)
	}

	for _, state := range []string{"succeeded", "cancelled"} {
		must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state=$2,finished_at=clock_timestamp(),error_code=NULL WHERE id=$1", sync, state)))
		_, err = f.store.AuthorizeGitOperation(ctx, sync, f.tid, pid)
		expectDenied(t, err, "an ended sync ("+state+")")
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE projects SET lifecycle='deleted',deleted_at=now() WHERE id=$1", pid)))
	_, err = f.store.AuthorizeGitOperation(ctx, publish, f.tid, pid)
	expectDenied(t, err, "a deleted Project")
}

// The contract's acceptance on real boundaries: the Repo Executor pushes a snapshot through a real
// Git Server whose decisions come from the real Store, sandbox and executor identities each work
// only on their own listener, and ending the Operation in Cloud ends its access.
func TestGitServerAuthorizesExecutorSyncFromCloudState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required for the Git Server boundary")
	}
	f := setup(t)
	f.withoutSnapshot = true
	pid := f.create("executor-sync").O("resource").S("id")
	bid := f.bindingOf(pid).S("id")
	var sync string
	must(t, f.store.Pool.QueryRow("SELECT id FROM repository_operations WHERE binding_id=$1", bid).Scan(&sync))
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='running' WHERE id=$1", sync)))

	pki, executorPKI := newGitPKI(t), newGitPKI(t)
	pki.issue(t, "server", "", x509.ExtKeyUsageServerAuth)
	pki.issue(t, "cloud", "spiffe://ora.local/cloud/git-ref-query", x509.ExtKeyUsageClientAuth)
	executorPKI.issue(t, "executor", gitserver.ExecutorIdentity, x509.ExtKeyUsageClientAuth)
	deployment := gitserver.Config{CertificateFile: filepath.Join(pki.dir, "server.pem"), PrivateKeyFile: filepath.Join(pki.dir, "server-key.pem"), SandboxCAFile: filepath.Join(pki.dir, "ca.pem"), CloudClientCertFile: filepath.Join(pki.dir, "cloud.pem"), ExecutorCAFile: filepath.Join(executorPKI.dir, "ca.pem")}
	hook := filepath.Join(t.TempDir(), "git-server")
	if out, err := exec.Command("go", "build", "-o", hook, "../cmd/git-server").CombinedOutput(); err != nil {
		t.Fatalf("build git-server: %v: %s", err, out)
	}
	cloudLeaf, err := deployment.CloudClient()
	must(t, err)
	service, err := gitserver.New(&gitserver.Options{Store: f.store, RepositoryRoot: filepath.Join(t.TempDir(), "repositories"), GitProgram: "git", CloudClient: cloudLeaf, Hook: []string{hook, "-pre-receive"}, Health: f.store.Pool.PingContext})
	must(t, err)
	serviceTLS, err := deployment.ServiceRepositoryTLS()
	must(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(service.ServiceRepositoryHandler))
	server.TLS = serviceTLS
	server.StartTLS()
	t.Cleanup(func() {
		server.Close()
		must(t, service.Close())
	})
	remote := server.URL + "/" + f.tid + "/" + pid + ".git"
	git := func(dir, operation string, args ...string) (string, error) {
		base := []string{"-c", "http.sslCert=" + filepath.Join(executorPKI.dir, "executor.pem"), "-c", "http.sslKey=" + filepath.Join(executorPKI.dir, "executor-key.pem"), "-c", "http.sslCAInfo=" + deployment.SandboxCAFile, "-c", "http.extraHeader=" + gitserver.OperationHeader + ": " + operation}
		cmd := exec.Command("git", append(base, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	checkout := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet"}, {"commit", "--quiet", "--allow-empty", "-m", "upstream"}} {
		if out, err := git(checkout, sync, args...); err != nil {
			t.Fatal(out)
		}
	}
	head, err := git(checkout, sync, "rev-parse", "HEAD")
	must(t, err)
	head = strings.TrimSpace(head)

	// The first sync runs while the Project is still provisioning.
	must(t, firstErr(f.store.Pool.Exec("UPDATE projects SET lifecycle='provisioning' WHERE id=$1", pid)))
	if out, err := git(checkout, sync, "push", remote, "HEAD:"+core.GitSnapshotRef(bid, head)); err != nil {
		t.Fatalf("executor snapshot push failed: %s", out)
	}
	if out, err := git(checkout, sync, "push", remote, "HEAD:"+core.GitSnapshotRef(uuid.NewString(), head)); err == nil {
		t.Fatalf("a snapshot outside the Operation's Binding was accepted: %s", out)
	}
	if out, err := git(checkout, sync, "push", remote, "HEAD:"+core.GitWorkspaceRefPrefix(uuid.NewString())+"main"); err == nil {
		t.Fatalf("the executor wrote a Workspace branch: %s", out)
	}
	must(t, firstErr(f.store.Pool.Exec("UPDATE repository_operations SET state='succeeded',finished_at=clock_timestamp() WHERE id=$1", sync)))
	if out, err := git(t.TempDir(), sync, "ls-remote", remote); err == nil || !strings.Contains(out, "403") {
		t.Fatalf("an ended Operation still reached the repository: %v %s", err, out)
	}
}
