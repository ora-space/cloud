package repoexec

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

func (f *fixture) probeAuthorization(repo, service, target string) *cloneauth.Authorization {
	return credential(&cloneauth.Authorization{Purpose: cloneauth.PurposeProbe, VerificationID: uuid.NewString(), TenantID: f.tenant, Repository: repo, Service: service, TargetRef: target})
}

func TestProbeUploadPackListsRefs(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "probe")
	head := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main", "HEAD:refs/heads/other")
	f.ext.requireToken("x-access-token", f.token)

	all := f.run(e, f.task(f.probeAuthorization(repo, cloneauth.ServiceUploadPack, ""), time.Minute))
	if all.Classification != Applied || len(all.Refs) != 2 || all.Refs["refs/heads/main"] != head {
		t.Fatalf("probe = %+v", all)
	}
	one := f.run(e, f.task(f.probeAuthorization(repo, cloneauth.ServiceUploadPack, "refs/heads/main"), time.Minute))
	if one.Classification != Applied || len(one.Refs) != 1 || one.Refs["refs/heads/main"] != head {
		t.Fatalf("probe of one ref = %+v", one)
	}
	if _, receive := f.ext.counts(); receive != 0 {
		t.Fatalf("an upload-pack probe contacted receive-pack %d times", receive)
	}
}

func TestProbeReceivePack(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, path := f.ext.repository(f, "probe")
	f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	f.ext.requireToken("x-access-token", f.token)

	ok := f.run(e, f.task(f.probeAuthorization(repo, cloneauth.ServiceReceivePack, ""), time.Minute))
	if ok.Classification != Applied || !ok.Advertised || ok.HTTPStatus != http.StatusOK {
		t.Fatalf("probe = %+v, want an advertisement", ok)
	}
	f.redeemer.token = "a-token-without-push"
	denied := f.run(e, f.task(f.probeAuthorization(repo, cloneauth.ServiceReceivePack, ""), time.Minute))
	if denied.Classification != Failed || denied.ErrorCode != CodePushDenied || denied.HTTPStatus != http.StatusUnauthorized || denied.Advertised {
		t.Fatalf("probe = %+v, want push_denied 401", denied)
	}
	if got := f.plain(path, "rev-parse", "refs/heads/main"); got == "" {
		t.Fatal("the repository lost its branch")
	}
}

// A redirect is never followed, by Git or by the probe's own client: the target never sees a
// request.
func TestRedirectIsNotFollowed(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	target := newExternalServer(t)
	repo, _ := f.ext.repository(f, "redirect")
	f.ext.mu.Lock()
	f.ext.mode, f.ext.redirect = modeRedirect, target.srv.URL
	f.ext.mu.Unlock()

	syncResult := f.run(e, f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute))
	if syncResult.Classification != Failed || syncResult.ErrorCode != CodeFetchFailed {
		t.Fatalf("sync through a redirect = %+v", syncResult)
	}
	probe := f.run(e, f.task(f.probeAuthorization(repo, cloneauth.ServiceReceivePack, ""), time.Minute))
	if probe.Classification != Failed || probe.ErrorCode != CodeVerificationFailed || probe.HTTPStatus != http.StatusFound {
		t.Fatalf("probe through a redirect = %+v", probe)
	}
	if requests, _ := target.counts(); requests != 0 {
		t.Fatalf("the redirect target received %d requests", requests)
	}
}
