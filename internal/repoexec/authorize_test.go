package repoexec

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// Every bound field is compared: changing any one of them refuses the task before redemption and
// before any external request.
func TestAuthorizationMismatchOfAnyField(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, _ := f.ext.repository(f, "bound")
	other := strings.Repeat("e", 40)
	publish := func() *cloneauth.Authorization {
		return credential(&cloneauth.Authorization{
			Purpose: cloneauth.PurposePublish, OperationID: f.readOperation(), TenantID: f.tenant, ProjectID: f.project, BindingID: f.binding,
			Repository: repo, SourceRef: "refs/heads/workspaces/" + uuid.NewString() + "/main", SourceCommit: strings.Repeat("a", 40), TargetRef: "refs/heads/main", ExpectedOldOID: cloneauth.ZeroOID40,
		})
	}
	sync := func() *cloneauth.Authorization {
		return credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40))
	}
	probe := func() *cloneauth.Authorization { return f.probeAuthorization(repo, cloneauth.ServiceUploadPack, "") }
	for _, c := range []struct {
		name   string
		auth   func() *cloneauth.Authorization
		mutate func(*Task)
	}{
		{"purpose", sync, func(t *Task) { t.Purpose = cloneauth.PurposePublish }},
		{"operationId", sync, func(t *Task) { t.OperationID = uuid.NewString() }},
		{"verificationId", probe, func(t *Task) { t.VerificationID = uuid.NewString() }},
		{"tenantId", sync, func(t *Task) { t.TenantID = uuid.NewString() }},
		{"projectId", sync, func(t *Task) { t.ProjectID = uuid.NewString() }},
		{"bindingId", sync, func(t *Task) { t.BindingID = uuid.NewString() }},
		{"repository", sync, func(t *Task) { t.Repository += "-fork" }},
		{"branch", sync, func(t *Task) { t.Branch = "release" }},
		{"expectedTrackingOid", sync, func(t *Task) { t.ExpectedTrackingOID = other }},
		{"sourceRef", publish, func(t *Task) { t.SourceRef = "refs/heads/workspaces/" + uuid.NewString() + "/main" }},
		{"sourceCommit", publish, func(t *Task) { t.SourceCommit = other }},
		{"targetRef", publish, func(t *Task) { t.TargetRef = "refs/heads/release" }},
		{"expectedOldOid", publish, func(t *Task) { t.ExpectedOldOID = other }},
		{"service", probe, func(t *Task) { t.Service = cloneauth.ServiceReceivePack }},
		{"credentialId", sync, func(t *Task) { t.CredentialID = uuid.NewString() }},
		{"credentialVersion", sync, func(t *Task) { t.CredentialVersion++ }},
		{"credential dropped", sync, func(t *Task) { t.CredentialID, t.CredentialVersion = "", 0 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			task := f.task(c.auth(), time.Minute)
			c.mutate(task)
			result := f.run(e, task)
			if result.Classification != Failed || result.ErrorCode != CodeAuthorizationMismatch {
				t.Fatalf("result = %+v, want authorization_mismatch", result)
			}
		})
	}
	if f.redeemer.count() != 0 {
		t.Fatalf("a mismatching task redeemed %d times", f.redeemer.count())
	}
	if requests, _ := f.ext.counts(); requests != 0 {
		t.Fatalf("a mismatching task reached the external repository %d times", requests)
	}
}

// A read-only probe grant can never drive a publish, even with every other field copied over.
func TestProbeAuthorizationCannotPublish(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, _ := f.ext.repository(f, "bound")
	probeTask := f.task(f.probeAuthorization(repo, cloneauth.ServiceReceivePack, "refs/heads/main"), time.Minute)
	probeTask.Purpose = cloneauth.PurposePublish
	probeTask.OperationID, probeTask.ProjectID, probeTask.BindingID = f.readOperation(), f.project, f.binding
	probeTask.SourceRef, probeTask.SourceCommit, probeTask.ExpectedOldOID = "refs/heads/workspaces/"+uuid.NewString()+"/main", strings.Repeat("a", 40), cloneauth.ZeroOID40
	result := f.run(e, probeTask)
	if result.Classification != Failed || result.ErrorCode != CodeAuthorizationMismatch {
		t.Fatalf("result = %+v, want authorization_mismatch", result)
	}
	if f.redeemer.count() != 0 {
		t.Fatal("the probe grant was redeemed for a publish")
	}
}

func TestAuthorizationRefusals(t *testing.T) {
	f := newFixture(t)
	repo, _ := f.ext.repository(f, "bound")
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		build  func() *Task
		mutate func(*Config)
		want   string
	}{
		{
			"expired", func() *Task {
				return f.task(credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)), time.Minute)
			},
			func(c *Config) { c.Now = func() time.Time { return time.Now().Add(10 * time.Minute) } }, CodeAuthorizationExpired,
		},
		{"foreign signature", func() *Task {
			task := f.task(credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)), time.Minute)
			a, _ := cloneauth.Verify(task.Authorization, []ed25519.PublicKey{f.public}, time.Now())
			task.Authorization, _ = cloneauth.Sign(otherKey, a)
			return task
		}, nil, CodeAuthorizationInvalid},
		{"garbage token", func() *Task {
			task := f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute)
			task.Authorization = "not-a-token"
			return task
		}, nil, CodeAuthorizationInvalid},
		{"userinfo in the URL", func() *Task {
			task := f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute)
			task.Repository = strings.Replace(task.Repository, "https://", "https://user:"+f.token+"@", 1)
			return task
		}, nil, CodeRepositoryURLInvalid},
		{"timeout above the limit", func() *Task { return f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Hour) }, nil, CodeInvalidTask},
		{"zero timeout", func() *Task { return f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), 0) }, nil, CodeInvalidTask},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mutate []func(*Config)
			if c.mutate != nil {
				mutate = append(mutate, c.mutate)
			}
			result := f.run(f.executor(mutate...), c.build())
			if result.Classification != Failed || result.ErrorCode != c.want {
				t.Fatalf("result = %+v, want %s", result, c.want)
			}
		})
	}
	if f.redeemer.count() != 0 {
		t.Fatalf("a refused task redeemed %d times", f.redeemer.count())
	}
}

// A redemption failure is reported as such and leaves nothing behind.
func TestRedemptionFailure(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	repo, _ := f.ext.repository(f, "bound")
	f.redeemer.fail = true
	result := f.run(e, f.task(credential(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40)), time.Minute))
	if result.Classification != Failed || result.ErrorCode != CodeRedemptionFailed {
		t.Fatalf("result = %+v", result)
	}
}
