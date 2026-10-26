package repoexec

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
)

// The executor's ref names are the contract's: a drift would make every sync a Git Server rejection.
func TestRefNamesMatchTheContract(t *testing.T) {
	binding, commit := "1b4e28ba-2fa1-41d2-883f-0016d3cca427", strings.Repeat("a", 40)
	if got, want := snapshotRef(binding, commit), core.GitSnapshotRef(binding, commit); got != want {
		t.Fatalf("snapshot ref %q, contract %q", got, want)
	}
	if got, want := trackingRef(binding, "main"), core.GitUpstreamHeadPrefix(binding)+"main"; got != want {
		t.Fatalf("tracking ref %q, contract %q", got, want)
	}
}

// A first sync (zero expected) creates the snapshot and the tracking ref in one push; replaying the
// same commit is a no-op reported as applied.
func TestSyncFirstTimeAndReplay(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "public")
	a := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")

	result := f.run(e, f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute))
	want := Result{Classification: Applied, CommitOID: a, SnapshotRef: core.GitSnapshotRef(f.binding, a), TrackingRef: core.GitUpstreamHeadPrefix(f.binding) + "main"}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("first sync = %+v, want %+v", result, want)
	}
	if f.gsRef(want.SnapshotRef) != a || f.gsRef(want.TrackingRef) != a {
		t.Fatalf("Git Server refs = %q / %q, want %q", f.gsRef(want.SnapshotRef), f.gsRef(want.TrackingRef), a)
	}
	if _, _, headers, certs := f.ext.scoping(); headers != 0 || certs != 0 {
		t.Fatalf("the external server received %d Operation headers and %d client certificates", headers, certs)
	}

	// Replay with the expected value the Operation recorded: nothing to send.
	replay := f.run(e, f.task(f.syncAuthorization(repo, "main", a), time.Minute))
	if !reflect.DeepEqual(replay, want) {
		t.Fatalf("replay = %+v, want %+v", replay, want)
	}
}

// The tracking ref follows an external force-push; the earlier snapshot stays.
func TestSyncFollowsExternalForcePush(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "forced")
	a := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	if r := f.run(e, f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute)); r.Classification != Applied {
		t.Fatalf("first sync = %+v", r)
	}
	f.orphan("rewrite")
	b := f.commit("rewritten")
	f.plain(f.work, "push", "--quiet", "--force", path, "HEAD:refs/heads/main")

	result := f.run(e, f.task(f.syncAuthorization(repo, "main", a), time.Minute))
	if result.Classification != Applied || result.CommitOID != b || !result.Forced {
		t.Fatalf("sync after force-push = %+v, want applied %s forced", result, b)
	}
	if got := f.gsRef(result.TrackingRef); got != b {
		t.Fatalf("tracking ref = %q, want %q", got, b)
	}
	if got := f.gsRef(core.GitSnapshotRef(f.binding, a)); got != a {
		t.Fatalf("old snapshot = %q, want it kept at %q", got, a)
	}
	// A fast-forward is not forced.
	f.plain(f.work, "checkout", "--quiet", "--detach", b)
	c := f.commit("next")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	if r := f.run(e, f.task(f.syncAuthorization(repo, "main", b), time.Minute)); r.Classification != Applied || r.CommitOID != c || r.Forced {
		t.Fatalf("fast-forward sync = %+v", r)
	}
}

// A stale expected value is refused by the lease (no '+'): superseded with the observed value, and
// the tracking ref is unchanged. When the tracking ref already holds the commit, it is applied.
func TestSyncStaleLease(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "stale")
	a := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	b := f.commit("two")
	// Someone else already moved the tracking ref to b (with its snapshot).
	f.seedGitServer(b, core.GitSnapshotRef(f.binding, b))
	f.seedGitServer(b, core.GitUpstreamHeadPrefix(f.binding)+"main")

	result := f.run(e, f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute))
	if result.Classification != Superseded || result.ObservedOID != b || result.CommitOID != a {
		t.Fatalf("stale sync = %+v, want superseded observing %s", result, b)
	}
	if got := f.gsRef(core.GitUpstreamHeadPrefix(f.binding) + "main"); got != b {
		t.Fatalf("tracking ref moved to %q", got)
	}
	if got := f.gsRef(core.GitSnapshotRef(f.binding, a)); got != "" {
		t.Fatalf("atomic push wrote the snapshot %q", got)
	}

	// The external branch is at b too: the tracking ref already holds the result.
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	same := f.run(e, f.task(f.syncAuthorization(repo, "main", a), time.Minute))
	if same.Classification != Applied || same.CommitOID != b {
		t.Fatalf("sync whose result is already written = %+v, want applied", same)
	}
}

// A Git Server refusal (Operation not running, or ended before pre-receive) is git_server_rejected,
// never superseded, and writes nothing.
func TestSyncGitServerRejects(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(f *fixture, a *cloneauth.Authorization)
	}{
		{"unknown Operation", func(f *fixture, a *cloneauth.Authorization) {
			f.store.mu.Lock()
			delete(f.store.operations, a.OperationID)
			f.store.mu.Unlock()
		}},
		{"Operation ends before pre-receive", func(f *fixture, _ *cloneauth.Authorization) {
			// Advertisement and POST are admitted; the hook's re-check is not.
			f.store.mu.Lock()
			f.store.calls, f.store.denyAfter = 0, 2
			f.store.mu.Unlock()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			e := f.executor()
			repo, path := f.ext.repository(f, "rejected")
			a := f.commit("one")
			f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
			auth := f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)
			c.setup(f, auth)
			result := f.run(e, f.task(auth, time.Minute))
			if result.Classification != Failed || result.ErrorCode != CodeGitServerRejected || result.CommitOID != a {
				t.Fatalf("sync = %+v, want failed git_server_rejected", result)
			}
			if got := f.gsRef(core.GitSnapshotRef(f.binding, a)); got != "" {
				t.Fatalf("rejected sync wrote %q", got)
			}
		})
	}
}

func TestSyncExternalFailures(t *testing.T) {
	for _, c := range []struct {
		name   string
		branch string
		auth   func(*cloneauth.Authorization) *cloneauth.Authorization
		token  string // what the redeemer hands out; "" keeps the right one
		want   string
	}{
		{"branch missing", "nope", func(a *cloneauth.Authorization) *cloneauth.Authorization { return a }, "", CodeUpstreamBranchMissing},
		{"anonymous on a private repository", "main", func(a *cloneauth.Authorization) *cloneauth.Authorization { return a }, "", CodeRequiresCredential},
		{"wrong token", "main", credential, "wrong-token-value", CodeCredentialRejected},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			e := f.executor()
			repo, path := f.ext.repository(f, "private")
			f.commit("one")
			f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
			if c.name != "branch missing" {
				f.ext.requireToken("x-access-token", f.token)
			}
			if c.token != "" {
				f.redeemer.token = c.token
			}
			result := f.run(e, f.task(c.auth(f.syncAuthorization(repo, c.branch, cloneauth.ZeroOID40)), time.Minute))
			if result.Classification != Failed || result.ErrorCode != c.want {
				t.Fatalf("sync = %+v, want failed %s", result, c.want)
			}
		})
	}
}

// A private repository syncs with the redeemed credential, handed to Git only through the helper.
func TestSyncPrivateRepositoryWithCredential(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "private")
	a := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	f.ext.requireToken("x-access-token", f.token)
	result := f.run(e, f.task(credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)), time.Minute))
	if result.Classification != Applied || result.CommitOID != a {
		t.Fatalf("private sync = %+v", result)
	}
	if f.redeemer.count() != 1 {
		t.Fatalf("redemptions = %d, want 1", f.redeemer.count())
	}
}

func TestSyncTooLarge(t *testing.T) {
	f := newFixture(t)
	e := f.executor(func(c *Config) { c.MaxTaskBytes = 256 << 10 })
	repo, path := f.ext.repository(f, "large")
	f.writeRandom("blob", 2<<20)
	f.plain(f.work, "add", "blob")
	f.plain(f.work, "commit", "--quiet", "-m", "large")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	result := f.run(e, f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute))
	if result.Classification != Failed || result.ErrorCode != CodeRepositoryTooLarge {
		t.Fatalf("sync = %+v, want repository_too_large", result)
	}
	if got := f.gsRef(core.GitUpstreamHeadPrefix(f.binding) + "main"); got != "" {
		t.Fatalf("an oversized sync wrote the tracking ref %q", got)
	}
}

// A task that exceeds its budget kills Git's whole process group, and the task directory and the
// staged credential are gone afterwards. Shutdown (cancellation) behaves the same.
func TestSyncTimeoutAndShutdownClean(t *testing.T) {
	for _, c := range []struct {
		name string
		want string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{"timeout", CodeTimeout, func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }},
		{"shutdown", CodeInternal, func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 1500*time.Millisecond)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			e := f.executor()
			repo, path := f.ext.repository(f, "slow")
			f.commit("one")
			f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
			f.ext.setMode(modeHangRead)
			timeout := time.Second
			if c.name == "shutdown" {
				timeout = time.Minute
			}
			ctx, cancel := c.ctx()
			defer cancel()
			result := e.Run(ctx, f.task(credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)), timeout))
			f.checkAfter(&result)
			if result.Classification != Failed || result.ErrorCode != c.want {
				t.Fatalf("sync = %+v, want failed %s", result, c.want)
			}
			if hangs, ended := f.ext.hangCounts(); hangs == 0 || hangs != ended {
				t.Fatalf("hung requests %d, ended by the client %d: the Git process outlived the task", hangs, ended)
			}
		})
	}
}

// The Binding URL is used as entered: a repository served at ".../name.git" syncs through that URL,
// and the credential, scoped to the normalized form, still matches Git's request.
func TestSyncRepositoryURLAsEntered(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "dotgit.git")
	if !strings.HasSuffix(repo, ".git") {
		t.Fatalf("fixture URL %q", repo)
	}
	a := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	f.ext.requireToken("x-access-token", f.token)
	result := f.run(e, f.task(credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)), time.Minute))
	if result.Classification != Applied || result.CommitOID != a {
		t.Fatalf("sync of %s = %+v", repo, result)
	}
}
