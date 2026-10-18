package gitserver

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// commit adds one commit to dir and returns it.
func (h *harness) commit(dir, message string) string {
	h.t.Helper()
	if out, err := h.git(dir, h.sandbox, "", "commit", "--quiet", "--allow-empty", "-m", message); err != nil {
		h.t.Fatal(out)
	}
	head, err := h.git(dir, h.sandbox, "", "rev-parse", "HEAD")
	if err != nil {
		h.t.Fatal(head)
	}
	return strings.TrimSpace(head)
}

// A Workspace branch may be created and fast-forwarded, never rewound, rewritten or deleted, and
// a grant for one Workspace's prefix writes nothing under another's.
func TestFastForwardRuleOnWorkspaceBranches(t *testing.T) {
	h := newHarness(t)
	workspace := uuid.NewString()
	branch := core.GitWorkspaceRefPrefix(workspace) + "feature/x"
	h.allow("session-1", core.GitWritableRef{Prefix: core.GitWorkspaceRefPrefix(workspace), Rule: core.GitFastForward})
	dir, first := h.checkout(1)
	if out, err := h.git(dir, h.sandbox, "session-1", "push", h.url(), "HEAD:"+branch); err != nil {
		t.Fatalf("creating a Workspace branch failed: %s", out)
	}
	second := h.commit(dir, "second")
	if out, err := h.git(dir, h.sandbox, "session-1", "push", h.url(), "HEAD:"+branch); err != nil {
		t.Fatalf("fast-forward failed: %s", out)
	}
	if got := h.remoteRef(branch); got != second {
		t.Fatalf("branch = %q, want %q", got, second)
	}
	if out, err := h.git(dir, h.sandbox, "", "checkout", "--quiet", "--detach", first); err != nil {
		t.Fatal(out)
	}
	sibling := h.commit(dir, "sibling")
	for _, c := range []struct{ name, spec string }{
		{"rewind", "+" + first + ":" + branch},
		{"non-fast-forward", "+" + sibling + ":" + branch},
		{"delete", ":" + branch},
		{"another Workspace", sibling + ":" + core.GitWorkspaceRefPrefix(uuid.NewString()) + "main"},
		{"invalid branch name", sibling + ":" + core.GitWorkspaceRefPrefix(workspace) + "a..b"},
		{"upstream namespace", sibling + ":" + core.GitSnapshotRef(uuid.NewString(), sibling)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out, err := h.git(dir, h.sandbox, "session-1", "push", h.url(), c.spec); err == nil {
				t.Fatalf("push was accepted: %s", out)
			}
			if got := h.remoteRef(branch); got != second {
				t.Fatalf("branch moved to %q", got)
			}
		})
	}
}

// A snapshot ref is created once and must name the commit it points at.
func TestCreateOnlyRuleOnSnapshots(t *testing.T) {
	h := newHarness(t)
	binding := uuid.NewString()
	h.allow("sync-1", core.GitWritableRef{Prefix: core.GitSnapshotRefPrefix(binding), Rule: core.GitCreateOnly})
	dir, first := h.checkout(1)
	second := h.commit(dir, "second")
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", h.url(), first+":"+core.GitSnapshotRef(binding, second)); err == nil {
		t.Fatalf("a snapshot named after another commit was accepted: %s", out)
	}
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", h.url(), second+":"+core.GitSnapshotRef(binding, second)); err != nil {
		t.Fatalf("snapshot push failed: %s", out)
	}
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", "--porcelain", h.url(), second+":"+core.GitSnapshotRef(binding, second)); err != nil || !strings.Contains(out, "[up to date]") {
		t.Fatalf("replayed snapshot = %v: %s", err, out)
	}
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", h.url(), second+":"+core.GitSnapshotRef(uuid.NewString(), second)); err == nil {
		t.Fatalf("a snapshot of another Binding was accepted: %s", out)
	}
	// A grant whose rule disagrees with the namespace is malformed and admits nothing.
	h.allow("bad", core.GitWritableRef{Prefix: core.GitSnapshotRefPrefix(binding), Rule: core.GitFastForward})
	if out, err := h.git(dir, h.sandbox, "bad", "push", h.url(), first+":"+core.GitSnapshotRef(binding, first)); err == nil {
		t.Fatalf("a grant with the wrong rule wrote: %s", out)
	}
}

// A tracking ref moves only from the expected old value and only to a commit with a snapshot,
// whether the snapshot already exists or arrives in the same push.
func TestTrackingCASRule(t *testing.T) {
	h := newHarness(t)
	binding := uuid.NewString()
	head := core.GitUpstreamHeadPrefix(binding) + "main"
	snapshots := core.GitWritableRef{Prefix: core.GitSnapshotRefPrefix(binding), Rule: core.GitCreateOnly}
	dir, first := h.checkout(1)
	second := h.commit(dir, "second")

	h.allow("sync-1", snapshots, core.GitWritableRef{Ref: head, Rule: core.GitTrackingCAS})
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", h.url(), first+":"+head); err == nil {
		t.Fatalf("a tracking ref without a snapshot was accepted: %s", out)
	}
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", "--atomic", h.url(), first+":"+core.GitSnapshotRef(binding, first), first+":"+head); err != nil {
		t.Fatalf("snapshot and tracking ref in one push failed: %s", out)
	}
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", h.url(), second+":"+core.GitSnapshotRef(binding, second)); err != nil {
		t.Fatalf("second snapshot failed: %s", out)
	}
	// The grant still expects the ref to be absent, so moving it is a CAS failure.
	if out, err := h.git(dir, h.sandbox, "sync-1", "push", h.url(), second+":"+head); err == nil {
		t.Fatalf("a tracking update with a stale expected old was accepted: %s", out)
	}
	h.allow("sync-2", snapshots, core.GitWritableRef{Ref: head, Rule: core.GitTrackingCAS, ExpectedOld: first})
	if out, err := h.git(dir, h.sandbox, "sync-2", "push", h.url(), second+":"+head); err != nil {
		t.Fatalf("tracking update from the expected old failed: %s", out)
	}
	if got := h.remoteRef(head); got != second {
		t.Fatalf("tracking ref = %q, want %q", got, second)
	}
	if out, err := h.git(dir, h.sandbox, "sync-2", "push", h.url(), ":"+head); err == nil {
		t.Fatalf("a tracking ref was deleted: %s", out)
	}
}

// listRefs calls the ref listener's prefix form as Cloud.
func (h *harness) listRefs(prefix string) (int, []RefCommit) {
	h.t.Helper()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: h.pki.pool, Certificates: []tls.Certificate{h.cloud.pair}}}}
	defer client.CloseIdleConnections()
	q := url.Values{"tenantId": {h.tenant}, "projectId": {h.project}, "prefix": {prefix}}
	response, err := client.Get(h.ref.URL + "/internal/v1/refs?" + q.Encode())
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		Refs []RefCommit `json:"refs"`
	}
	_ = json.NewDecoder(response.Body).Decode(&body)
	return response.StatusCode, body.Refs
}

// The ref query answers every contract namespace, singly or by a prefix inside one namespace
// instance, and nothing outside them.
func TestRefQueryCoversEveryNamespaceAndListsByPrefix(t *testing.T) {
	h := newHarness(t)
	binding, workspace := uuid.NewString(), uuid.NewString()
	if status, refs := h.listRefs(core.GitWorkspaceRefPrefix(workspace)); status != http.StatusOK || len(refs) != 0 {
		t.Fatalf("listing an absent repository = %d %v, want 200 and none", status, refs)
	}
	dir, first := h.checkout(1)
	second := h.commit(dir, "second")
	h.allow("all", core.GitWritableRef{Prefix: core.GitSnapshotRefPrefix(binding), Rule: core.GitCreateOnly}, core.GitWritableRef{Prefix: core.GitUpstreamHeadPrefix(binding), Rule: core.GitTrackingCAS}, core.GitWritableRef{Prefix: core.GitWorkspaceRefPrefix(workspace), Rule: core.GitFastForward})
	pushed := map[string]string{
		core.GitSnapshotRef(binding, first):              first,
		core.GitUpstreamHeadPrefix(binding) + "main":     first,
		core.GitWorkspaceRefPrefix(workspace) + "main":   second,
		core.GitWorkspaceRefPrefix(workspace) + "feat/a": first,
	}
	args := []string{"push", "--atomic", h.url()}
	for ref, commit := range pushed {
		args = append(args, commit+":"+ref)
	}
	if out, err := h.git(dir, h.sandbox, "all", args...); err != nil {
		t.Fatalf("seeding refs failed: %s", out)
	}
	for ref, commit := range pushed {
		if status, body := h.queryRef(&h.cloud, h.tenant, h.project, ref); status != http.StatusOK || body["commit"] != commit {
			t.Fatalf("query %s = %d %v, want %s", ref, status, body, commit)
		}
	}
	status, refs := h.listRefs(core.GitWorkspaceRefPrefix(workspace))
	want := []RefCommit{{Ref: core.GitWorkspaceRefPrefix(workspace) + "feat/a", Commit: first}, {Ref: core.GitWorkspaceRefPrefix(workspace) + "main", Commit: second}}
	if status != http.StatusOK || !reflect.DeepEqual(refs, want) {
		t.Fatalf("workspace listing = %d %v, want %v", status, refs, want)
	}
	if status, refs := h.listRefs("refs/ora/upstreams/" + binding + "/"); status != http.StatusOK || len(refs) != 2 {
		t.Fatalf("binding listing = %d %v, want the snapshot and the tracking ref", status, refs)
	}
	for _, prefix := range []string{"refs/", "refs/heads/", "refs/heads/workspaces/", core.GitWorkspaceRefPrefix(workspace) + "feat/", "refs/ora/upstreams/"} {
		if status, _ := h.listRefs(prefix); status != http.StatusBadRequest {
			t.Errorf("prefix %q = %d, want 400", prefix, status)
		}
	}
	if status, _ := h.queryRef(&h.cloud, h.tenant, h.project, "refs/heads/main"); status != http.StatusBadRequest {
		t.Fatalf("a ref outside the namespaces = %d, want 400", status)
	}
}
