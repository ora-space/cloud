package repoexec

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
)

// publishSetup prepares an external main at base and a Workspace branch on the Git Server at a
// descendant of it, and returns the authorization for fast-forwarding main to that descendant.
type publishSetup struct {
	repo, path, base, source, sourceRef string
}

func (f *fixture) publishSetup(name string) publishSetup {
	f.t.Helper()
	repo, path := f.ext.repository(f, name)
	base := f.commit("base")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	source := f.commit("work")
	sourceRef := core.GitWorkspaceRefPrefix(uuid.NewString()) + "main"
	f.seedGitServer(source, sourceRef)
	f.ext.requireToken("x-access-token", f.token)
	return publishSetup{repo: repo, path: path, base: base, source: source, sourceRef: sourceRef}
}

func (f *fixture) publishAuthorization(s *publishSetup, expectedOld string) *cloneauth.Authorization {
	return credential(&cloneauth.Authorization{
		Purpose: cloneauth.PurposePublish, OperationID: f.readOperation(), TenantID: f.tenant, ProjectID: f.project, BindingID: f.binding,
		Repository: s.repo, SourceRef: s.sourceRef, SourceCommit: s.source, TargetRef: "refs/heads/main", ExpectedOldOID: expectedOld,
	})
}

// extRef reads a ref of the external repository from disk; "" when absent.
func (f *fixture) extRef(path, ref string) string {
	f.t.Helper()
	out := f.plain(path, "for-each-ref", "--format=%(objectname)", ref)
	return strings.TrimSpace(out)
}

func TestPublishApplied(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("applied")
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), time.Minute))
	if result.Classification != Applied || result.ConfirmedByVerification || result.PreObservedOID != s.base || result.PostObservedOID != s.source {
		t.Fatalf("publish = %+v, want applied from %s to %s", result, s.base, s.source)
	}
	if got := f.extRef(s.path, "refs/heads/main"); got != s.source {
		t.Fatalf("external main = %q, want %q", got, s.source)
	}
	if _, _, headers, certs := f.ext.scoping(); headers != 0 || certs != 0 {
		t.Fatalf("the external server received %d Operation headers and %d client certificates", headers, certs)
	}
}

// A zero expected old creates the target ref.
func TestPublishCreatesTarget(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("create")
	a := f.publishAuthorization(&s, cloneauth.ZeroOID40)
	a.TargetRef = "refs/heads/feature"
	result := f.run(e, f.task(a, time.Minute))
	if result.Classification != Applied || f.extRef(s.path, "refs/heads/feature") != s.source {
		t.Fatalf("publish = %+v", result)
	}
}

// The external ref already moved: the pre-check reports conflict without opening receive-pack.
func TestPublishConflictBeforePush(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("conflict")
	f.orphan("other")
	other := f.commit("other")
	f.plain(f.work, "push", "--quiet", "--force", s.path, "HEAD:refs/heads/main")
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), time.Minute))
	if result.Classification != Conflict || result.PreObservedOID != other || result.ObservedOID != other {
		t.Fatalf("publish = %+v, want conflict observing %s", result, other)
	}
	if _, receive := f.ext.counts(); receive != 0 {
		t.Fatalf("receive-pack was contacted %d times", receive)
	}
}

// The external ref moves between the pre-check and the push: the lease refuses (stale info).
func TestPublishConflictByLease(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("moved")
	f.orphan("other")
	other := f.commit("other")
	f.plain(f.work, "push", "--quiet", s.path, "HEAD:refs/heads/other")
	f.ext.moveOnPush(func() { f.plain(s.path, "update-ref", "refs/heads/main", other) })
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), time.Minute))
	if result.Classification != Conflict || result.PreObservedOID != s.base || result.PostObservedOID != other {
		t.Fatalf("publish = %+v, want conflict by lease", result)
	}
	if got := f.extRef(s.path, "refs/heads/main"); got != other {
		t.Fatalf("external main = %q, want the concurrent %q kept", got, other)
	}
}

// The platform refuses the update: rejected, the post-check shows expected old, and the platform's
// message reaches the detail with the secret redacted.
func TestPublishRejected(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("refuse")
	f.ext.refuse(s.path, "protected branch; token "+f.token)
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), time.Minute))
	if result.Classification != Rejected || result.ErrorCode != CodeExternalRejected || result.PostObservedOID != s.base {
		t.Fatalf("publish = %+v, want rejected", result)
	}
	if !strings.Contains(result.Detail, "protected branch") || !strings.Contains(result.Detail, "[redacted]") {
		t.Fatalf("detail %q lacks the redacted platform reason", result.Detail)
	}
}

// expectedOld is not an ancestor of the source: decided from the Git Server's objects alone, with no
// request to the external repository at all.
func TestPublishNotFastForward(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("nff")
	f.orphan("other")
	other := f.commit("other")
	// The unrelated commit is on the Git Server too, so ancestry is really computed.
	f.seedGitServer(other, core.GitWorkspaceRefPrefix(uuid.NewString())+"other")
	for _, old := range []string{other, strings.Repeat("ab", 20)} {
		result := f.run(e, f.task(f.publishAuthorization(&s, old), time.Minute))
		if result.Classification != NotFastForward || result.ErrorCode != CodeNotFastForward {
			t.Fatalf("publish from %s = %+v, want not_fast_forward", old, result)
		}
	}
	if requests, _ := f.ext.counts(); requests != 0 {
		t.Fatalf("the external repository received %d requests", requests)
	}
}

func TestPublishSourceUnavailable(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("source")
	for name, mutate := range map[string]func(*cloneauth.Authorization){
		"commit not on the source ref": func(a *cloneauth.Authorization) { a.SourceCommit = strings.Repeat("cd", 20) },
		"source ref missing":           func(a *cloneauth.Authorization) { a.SourceRef = core.GitWorkspaceRefPrefix(uuid.NewString()) + "gone" },
	} {
		a := f.publishAuthorization(&s, s.base)
		mutate(a)
		result := f.run(e, f.task(a, time.Minute))
		if result.Classification != SourceUnavailable || result.ErrorCode != CodeSourceUnavailable {
			t.Fatalf("%s: publish = %+v, want source_unavailable", name, result)
		}
	}
	if requests, _ := f.ext.counts(); requests != 0 {
		t.Fatalf("the external repository received %d requests", requests)
	}
}

// The push takes effect but its answer is lost: the post-check confirms it.
func TestPublishDroppedResponseIsConfirmed(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("drop")
	f.ext.setMode(modeDrop)
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), time.Minute))
	if result.Classification != Applied || !result.ConfirmedByVerification || result.PostObservedOID != s.source {
		t.Fatalf("publish = %+v, want applied confirmed by verification", result)
	}
}

// The push never answers: after the push budget the process group is killed, the post-check still
// sees expected old, and the result is uncertain (never retried).
func TestPublishHangIsUncertain(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("hang")
	f.ext.setMode(modeHang)
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), 3*time.Second))
	if result.Classification != Uncertain || result.PostObservedOID != s.base {
		t.Fatalf("publish = %+v, want uncertain observing %s", result, s.base)
	}
	if hangs, ended := f.ext.hangCounts(); hangs != 1 || ended != 1 {
		t.Fatalf("pushes %d (ended by the client %d), want exactly one and no retry", hangs, ended)
	}
}

// A credential that may read but not push fails at ref discovery: nothing was sent.
func TestPublishPushDenied(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	s := f.publishSetup("denied")
	f.ext.denyPushes()
	result := f.run(e, f.task(f.publishAuthorization(&s, s.base), time.Minute))
	if result.Classification != Failed || result.ErrorCode != CodePushDenied {
		t.Fatalf("publish = %+v, want failed push_denied", result)
	}
}
