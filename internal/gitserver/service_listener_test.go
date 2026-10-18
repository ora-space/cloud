package gitserver

import (
	"crypto/tls"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// The Repo Executor syncs over its own listener: snapshots are created once, and the tracking ref
// follows the external branch even through a force-push, which the sandbox listener never allows.
func TestServiceListenerFollowsExternalForcePush(t *testing.T) {
	h := newHarness(t)
	binding := uuid.NewString()
	head := core.GitUpstreamHeadPrefix(binding) + "main"
	snapshots := core.GitWritableRef{Prefix: core.GitSnapshotRefPrefix(binding), Rule: core.GitCreateOnly}
	dir, first := h.checkout(1)
	second := h.commit(dir, "second")
	if out, err := h.git(dir, h.sandbox, "", "checkout", "--quiet", "--detach", first); err != nil {
		t.Fatal(out)
	}
	rewritten := h.commit(dir, "rewritten")

	sync := h.operation(snapshots, core.GitWritableRef{Ref: head, Rule: core.GitTrackingCAS})
	if out, err := h.executorGit(dir, sync, "push", "--atomic", h.serviceURL(), second+":"+core.GitSnapshotRef(binding, second), second+":"+head); err != nil {
		t.Fatalf("first sync failed: %s", out)
	}
	resync := h.operation(snapshots, core.GitWritableRef{Ref: head, Rule: core.GitTrackingCAS, ExpectedOld: second})
	if out, err := h.executorGit(dir, resync, "push", "--atomic", "--force", h.serviceURL(), rewritten+":"+core.GitSnapshotRef(binding, rewritten), rewritten+":"+head); err != nil {
		t.Fatalf("sync after an external force-push failed: %s", out)
	}
	if got := h.remoteRef(head); got != rewritten {
		t.Fatalf("tracking ref = %q, want the force-pushed %q", got, rewritten)
	}
	if got := h.remoteRef(core.GitSnapshotRef(binding, second)); got != second {
		t.Fatalf("the earlier snapshot changed to %q", got)
	}

	// Turning off Git's own refusal on this listener does not make other namespaces rewritable:
	// the hook still applies their rule.
	workspace := core.GitWorkspaceRefPrefix(uuid.NewString()) + "main"
	h.allow("session-1", core.GitWritableRef{Ref: workspace, Rule: core.GitFastForward})
	if out, err := h.git(dir, h.sandbox, "session-1", "push", h.url(), second+":"+workspace); err != nil {
		t.Fatalf("workspace branch push failed: %s", out)
	}
	wide := h.operation(core.GitWritableRef{Ref: workspace, Rule: core.GitFastForward})
	if out, err := h.executorGit(dir, wide, "push", "--force", h.serviceURL(), rewritten+":"+workspace); err == nil {
		t.Fatalf("a non-fast-forward on the service listener was accepted: %s", out)
	}
	if got := h.remoteRef(workspace); got != second {
		t.Fatalf("workspace branch moved to %q", got)
	}

	// A read-only Operation (publish) fetches but writes nothing.
	publish := h.operation()
	fresh, _ := h.checkout(0)
	if out, err := h.executorGit(fresh, publish, "fetch", h.serviceURL(), workspace); err != nil {
		t.Fatalf("publish read failed: %s", out)
	}
	if out, err := h.executorGit(dir, publish, "push", h.serviceURL(), first+":"+core.GitSnapshotRef(binding, first)); err == nil {
		t.Fatalf("a read-only Operation pushed: %s", out)
	}
}

// An Operation that ends between the ref advertisement and pre-receive writes nothing.
func TestServiceListenerRechecksOperationInPreReceive(t *testing.T) {
	h := newHarness(t)
	binding := uuid.NewString()
	sync := h.operation(core.GitWritableRef{Prefix: core.GitSnapshotRefPrefix(binding), Rule: core.GitCreateOnly})
	dir, head := h.checkout(1)
	h.store.set(func(f *fakeStore) { f.calls, f.denyAfter = 0, 2 })
	if out, err := h.executorGit(dir, sync, "push", h.serviceURL(), head+":"+core.GitSnapshotRef(binding, head)); err == nil {
		t.Fatalf("push after the Operation ended was accepted: %s", out)
	}
	if got := h.remoteRef(core.GitSnapshotRef(binding, head)); got != "" {
		t.Fatalf("rejected push wrote the snapshot: %s", got)
	}
}

// Each listener accepts only its own identity and exactly its own context header.
func TestListenersRequireTheirOwnIdentityAndContext(t *testing.T) {
	h := newHarness(t)
	ref := h.grant("deliver-1")
	dir, _ := h.checkout(1)
	if out, err := h.git(dir, h.sandbox, "deliver-1", "push", h.url(), "HEAD:"+ref); err != nil {
		t.Fatalf("delivery push failed: %s", out)
	}
	operation := h.operation()
	executorOfSandboxCA := h.pki.issue("executor-wrong-ca", ExecutorIdentity, 0)
	sandboxOfExecutorCA := h.executorPKI.issue("sandbox-wrong-ca", "spiffe://ora.local/git-access/tenant/"+h.tenant+"/runtime/"+h.workspace+"/generation/3", 0)
	otherService := h.executorPKI.issue("other-service", "spiffe://ora.local/controller/local-controller", 0)
	advertise := "/" + h.tenant + "/" + h.project + ".git/info/refs?service=git-upload-pack"
	get := func(base string, who *identity, headers map[string]string) int {
		t.Helper()
		config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: h.pki.pool}
		if who != nil {
			config.Certificates = []tls.Certificate{who.pair}
		}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
		defer client.CloseIdleConnections()
		request, _ := http.NewRequest(http.MethodGet, base+advertise, nil)
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		response, err := client.Do(request)
		if err != nil {
			return 0
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	execution := map[string]string{ExecutionHeader: "deliver-1"}
	op := map[string]string{OperationHeader: operation}
	both := map[string]string{ExecutionHeader: "deliver-1", OperationHeader: operation}
	for _, c := range []struct {
		name    string
		base    string
		who     *identity
		headers map[string]string
		want    int
	}{
		{"executor with its Operation", h.svc.URL, &h.executor, op, http.StatusOK},
		{"executor without context", h.svc.URL, &h.executor, nil, http.StatusBadRequest},
		{"executor naming an execution", h.svc.URL, &h.executor, execution, http.StatusBadRequest},
		{"executor naming both", h.svc.URL, &h.executor, both, http.StatusBadRequest},
		{"executor, non-UUID Operation", h.svc.URL, &h.executor, map[string]string{OperationHeader: "op-1"}, http.StatusBadRequest},
		{"executor, unknown Operation", h.svc.URL, &h.executor, map[string]string{OperationHeader: uuid.NewString()}, http.StatusForbidden},
		{"another service of the executor CA", h.svc.URL, &otherService, op, http.StatusUnauthorized},
		{"sandbox certificate on the service listener", h.svc.URL, &h.sandbox, op, 0},
		{"sandbox identity from the executor CA on the sandbox listener", h.repo.URL, &sandboxOfExecutorCA, execution, 0},
		{"executor certificate on the sandbox listener", h.repo.URL, &h.executor, op, 0},
		{"executor identity from the sandbox CA", h.repo.URL, &executorOfSandboxCA, op, http.StatusUnauthorized},
		{"sandbox naming an Operation", h.repo.URL, &h.sandbox, op, http.StatusBadRequest},
		{"sandbox naming both", h.repo.URL, &h.sandbox, both, http.StatusBadRequest},
		{"sandbox with its execution", h.repo.URL, &h.sandbox, execution, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := get(c.base, c.who, c.headers); got != c.want {
				t.Fatalf("status = %d, want %d", got, c.want)
			}
		})
	}
}
