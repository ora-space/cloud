package repoworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

func publishCheck() core.RepositoryCheck {
	return core.RepositoryCheck{
		OperationID: publishOpID, TenantID: tenantID, ProjectID: projectID, BindingID: bindingID,
		RepositoryURL: repositoryURL, TargetRef: "refs/heads/release", SourceCommit: testCommit, ExpectedOldOID: testCommit,
		CredentialID: credentialID, CredentialVersion: 3, Attempt: 2, AuthorizationID: authorizationID,
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}
}

func TestCheckReadsTheTargetOnceWithAProbe(t *testing.T) {
	store := &fakeStore{}
	exec := &fakeExecutor{run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return repoexec.Result{Classification: repoexec.Applied, Advertised: true, Refs: map[string]string{"refs/heads/release": testCommit}}, nil
	}}
	w := newWorker(t, store, exec, &fakeRefs{}, nil)

	check := publishCheck()
	w.runCheck(context.Background(), &check)

	settle, ok := store.settledCheck()
	if !ok {
		t.Fatal("the check was never settled")
	}
	want := checkSettle{id: publishOpID, attempt: 2, observed: testCommit, found: true}
	if settle != want {
		t.Fatalf("settled %+v, want %+v", settle, want)
	}
	calls := exec.calls()
	if len(calls) != 1 {
		t.Fatalf("executor calls = %d, want exactly one probe", len(calls))
	}
	task := calls[0]
	if task.Purpose != cloneauth.PurposeProbe || task.Service != cloneauth.ServiceUploadPack || task.TargetRef != "refs/heads/release" ||
		task.OperationID != publishOpID || task.VerificationID != "" || task.TimeoutSeconds != 60 || task.Authorization == "" {
		t.Fatalf("probe task = %+v, want an upload-pack read of the target ref", task)
	}
}

func TestCheckOutcomes(t *testing.T) {
	applied := func(refs map[string]string) func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return func(context.Context, *repoexec.Task) (repoexec.Result, error) {
			return repoexec.Result{Classification: repoexec.Applied, Refs: refs}, nil
		}
	}
	failed := func(code string) func(context.Context, *repoexec.Task) (repoexec.Result, error) {
		return func(context.Context, *repoexec.Task) (repoexec.Result, error) {
			return repoexec.Result{Classification: repoexec.Failed, ErrorCode: code}, nil
		}
	}
	for _, tc := range []struct {
		name      string
		problem   string
		run       func(context.Context, *repoexec.Task) (repoexec.Result, error)
		want      checkSettle
		execCalls int
	}{
		{
			name:    "core already decided the check cannot run",
			problem: "credential_unavailable",
			run:     applied(nil),
			want:    checkSettle{failure: "credential_unavailable"},
		},
		{
			name:      "the target ref is not advertised",
			run:       applied(map[string]string{"refs/heads/main": testCommit}),
			want:      checkSettle{},
			execCalls: 1,
		},
		{
			name: "the executor cannot be reached",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{}, errors.New("connection refused")
			},
			want:      checkSettle{failure: "executor_unreachable"},
			execCalls: 1,
		},
		{
			name:      "the executor's own code is passed through",
			run:       failed("external_rejected"),
			want:      checkSettle{failure: "external_rejected"},
			execCalls: 1,
		},
		{
			name:      "a blank code becomes verification_failed",
			run:       failed("  "),
			want:      checkSettle{failure: "verification_failed"},
			execCalls: 1,
		},
		{
			name:      "a missing code becomes verification_failed",
			run:       failed(""),
			want:      checkSettle{failure: "verification_failed"},
			execCalls: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			exec := &fakeExecutor{run: tc.run}
			w := newWorker(t, store, exec, &fakeRefs{}, nil)

			check := publishCheck()
			check.Problem = tc.problem
			w.runCheck(context.Background(), &check)

			settle, ok := store.settledCheck()
			if !ok {
				t.Fatal("the check was never settled")
			}
			want := tc.want
			want.id, want.attempt = publishOpID, 2
			if settle != want {
				t.Fatalf("settled %+v, want %+v", settle, want)
			}
			if got := len(exec.calls()); got != tc.execCalls {
				t.Fatalf("executor calls = %d, want %d", got, tc.execCalls)
			}
		})
	}
}

func credentialCheck() core.CredentialCheck {
	return core.CredentialCheck{
		VerificationID: verificationID, TenantID: tenantID, CredentialID: credentialID, Repository: repositoryURL,
		CredentialVersion: 3, LeaseGeneration: 4, AuthorizationID: authorizationID,
		IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}
}

func TestCredentialVerificationOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *repoexec.Task) (repoexec.Result, error)
		want string
	}{
		{
			name: "the advertisement proves push capability",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{Classification: repoexec.Applied, Advertised: true}, nil
			},
			want: "verified",
		},
		{
			name: "a refused push is a denied credential",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{Classification: repoexec.Failed, ErrorCode: repoexec.CodePushDenied}, nil
			},
			want: "push_denied",
		},
		{
			name: "a rejected credential is a denied credential",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{Classification: repoexec.Failed, ErrorCode: repoexec.CodeCredentialRejected}, nil
			},
			want: "push_denied",
		},
		{
			// Applied without an advertisement read is not proof that a push would be accepted.
			name: "an applied probe that read no advertisement is not proof",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{Classification: repoexec.Applied}, nil
			},
			want: "verification_failed",
		},
		{
			name: "any other failure is unverified",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{Classification: repoexec.Failed, ErrorCode: repoexec.CodeFetchFailed}, nil
			},
			want: "verification_failed",
		},
		{
			name: "an unreachable executor leaves the credential unverified",
			run: func(context.Context, *repoexec.Task) (repoexec.Result, error) {
				return repoexec.Result{}, errors.New("connection refused")
			},
			want: "verification_failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			exec := &fakeExecutor{run: tc.run}
			w := newWorker(t, store, exec, &fakeRefs{}, nil)

			check := credentialCheck()
			w.runVerification(context.Background(), &check)

			settle, ok := store.settledProbe()
			if !ok {
				t.Fatal("the verification was never settled")
			}
			if settle.result != tc.want {
				t.Fatalf("settled %q, want %q", settle.result, tc.want)
			}
			if settle.id != verificationID || settle.gen != 4 {
				t.Fatalf("settled (%q, %d), want the verification and its lease generation", settle.id, settle.gen)
			}
			calls := exec.calls()
			if len(calls) != 1 {
				t.Fatalf("executor calls = %d, want exactly one probe", len(calls))
			}
			task := calls[0]
			if task.Purpose != cloneauth.PurposeProbe || task.Service != cloneauth.ServiceReceivePack ||
				task.VerificationID != verificationID || task.OperationID != "" || task.CredentialID != credentialID ||
				task.CredentialVersion != 3 || task.TimeoutSeconds != 60 {
				t.Fatalf("probe task = %+v, want a receive-pack probe of the credential version", task)
			}
		})
	}
}
