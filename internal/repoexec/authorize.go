package repoexec

import (
	"crypto/ed25519"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// admit decides whether a task may start: the repository is a usable URL, the token verifies under
// the deployment's keys and clock, and every bound field equals the task's. It returns the verified
// authorization or the failure to report. Nothing is redeemed or contacted before it passes.
func admit(task *Task, keys []ed25519.PublicKey, now time.Time) (cloneauth.Authorization, *Result) {
	// A URL carrying userinfo is refused with its own code before anything else: the authorization
	// could never have named it, and the code tells Cloud the Binding itself is unusable.
	if u, err := url.Parse(task.Repository); err != nil || u.User != nil {
		r := failed(CodeRepositoryURLInvalid, "repository URL is not usable")
		return cloneauth.Authorization{}, &r
	}
	// The repository is the Binding URL as the user entered it (it may end in ".git") and Git is
	// pointed at exactly that; it only has to normalize, which also refuses non-https, queries and
	// escapes. The credential scope is the normalized form, so the helper matches either spelling.
	if _, err := cloneauth.NormalizeRepository(task.Repository); err != nil || strings.TrimSpace(task.Repository) != task.Repository {
		r := failed(CodeRepositoryURLInvalid, "repository URL is not a usable https URL")
		return cloneauth.Authorization{}, &r
	}
	a, err := cloneauth.Verify(task.Authorization, keys, now)
	switch {
	case errors.Is(err, cloneauth.ErrExpired):
		r := failed(CodeAuthorizationExpired, "authorization has expired")
		return cloneauth.Authorization{}, &r
	case err != nil:
		r := failed(CodeAuthorizationInvalid, "authorization does not verify")
		return cloneauth.Authorization{}, &r
	}
	if field := mismatch(task, &a); field != "" {
		r := failed(CodeAuthorizationMismatch, "task field "+field+" differs from the authorization")
		return cloneauth.Authorization{}, &r
	}
	return a, nil
}

// mismatch returns the JSON name of the first task field that differs from the authorization, or
// "". The comparison is exact: an authorization for one purpose never admits a task of another, so
// a probe grant cannot drive a publish.
func mismatch(t *Task, a *cloneauth.Authorization) string {
	pairs := []struct{ name, task, auth string }{
		{"purpose", string(t.Purpose), string(a.Purpose)},
		{"operationId", t.OperationID, a.OperationID},
		{"verificationId", t.VerificationID, a.VerificationID},
		{"tenantId", t.TenantID, a.TenantID},
		{"projectId", t.ProjectID, a.ProjectID},
		{"bindingId", t.BindingID, a.BindingID},
		{"repository", t.Repository, a.Repository},
		{"branch", t.Branch, a.Branch},
		{"expectedTrackingOid", t.ExpectedTrackingOID, a.ExpectedTrackingOID},
		{"sourceRef", t.SourceRef, a.SourceRef},
		{"sourceCommit", t.SourceCommit, a.SourceCommit},
		{"targetRef", t.TargetRef, a.TargetRef},
		{"expectedOldOid", t.ExpectedOldOID, a.ExpectedOldOID},
		{"service", t.Service, a.Service},
		{"credentialId", t.CredentialID, a.CredentialID},
		{"credentialVersion", strconv.FormatInt(t.CredentialVersion, 10), strconv.FormatInt(a.CredentialVersion, 10)},
	}
	for _, p := range pairs {
		if p.task != p.auth {
			return p.name
		}
	}
	return ""
}

// Limits are the deployment's maximum task budgets per purpose.
type Limits struct {
	Sync, Publish, Probe time.Duration
}

// budget validates the task's own timeout against the deployment's maximum for its purpose.
func (l Limits) budget(t *Task) (time.Duration, bool) {
	var limit time.Duration
	switch t.Purpose {
	case cloneauth.PurposeSync:
		limit = l.Sync
	case cloneauth.PurposePublish:
		limit = l.Publish
	case cloneauth.PurposeProbe:
		limit = l.Probe
	default:
		return 0, false
	}
	d := t.Timeout()
	return d, t.TimeoutSeconds > 0 && d <= limit
}
