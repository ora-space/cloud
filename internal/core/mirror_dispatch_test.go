package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
)

// recordingMirrorClient stands in for the executor's mirror route.
//
// It is a stand-in for the transport and nothing else: what the real route does with a job is covered
// by `internal/cloneexec`'s own acceptance test, which runs the genuine mutual-TLS server and a
// genuine Git fetch. What is under test here is the *authorization* — the bytes Cloud signs — and
// those are checked against the real `cloneauth` verification rather than against a fake of it.
type recordingMirrorClient struct {
	calls  int
	job    cloneexec.MirrorJob
	result cloneexec.MirrorResult
	err    error
}

func (c *recordingMirrorClient) PrepareMirror(_ context.Context, job cloneexec.MirrorJob) (cloneexec.MirrorResult, error) {
	c.calls++
	c.job = job
	return c.result, c.err
}

// mirrorDispatchFixture builds a configured dispatcher: a real key pair, a recording transport, and a
// clock the test owns so issuance is exact rather than approximately recent.
func mirrorDispatchFixture(t *testing.T) (*MirrorDispatcher, *recordingMirrorClient, ed25519.PublicKey, time.Time) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the dispatcher key: %v", err)
	}
	client := &recordingMirrorClient{result: cloneexec.MirrorResult{
		Commit:     commitHex(),
		Ref:        "refs/heads/release/2026-10",
		Repository: "https://git.example.invalid/group/private",
		Generation: 7,
	}}
	issued := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	return &MirrorDispatcher{
		Client:           client,
		SigningKey:       private,
		ExecutorIdentity: "spiffe://ora.local/credential-store/broker-resolver",
		Lifetime:         4 * time.Minute,
		Now:              func() time.Time { return issued },
	}, client, public, issued
}

// mirrorClaim is one claimed attempt, as the claim transaction would hand it over.
//
// The repository is deliberately not canonical — an uppercase host, a trailing `.git` and a trailing
// slash — because the claim reads the Project's stored URL, which the product does not require to be
// written in the canonical form, and the authorization must bind the identity the executor will
// re-derive rather than the spelling the row happens to carry.
func mirrorClaim() MirrorPreparation {
	return MirrorPreparation{
		OperationID:       newID(),
		AuthorizationID:   newID(),
		Generation:        3,
		ControlEpoch:      5,
		TenantID:          newID(),
		OwnerUserID:       newID(),
		ProjectID:         newID(),
		WorkspaceID:       newID(),
		Repository:        "HTTPS://Git.Example.Invalid/Group/Private.git/",
		Ref:               "refs/heads/release/2026-10",
		CredentialID:      newID(),
		CredentialVersion: 2,
		Capabilities:      []string{"clone"},
		RuntimeGeneration: 4,
	}
}

// TestMirrorDispatchSignsTheClaimedAttemptAndNothingElse is the binding-continuity test: the
// authorization the executor receives must state exactly what the claim decided, and must state
// nothing about a Node, an execution, or a runtime the executor does not have.
func TestMirrorDispatchSignsTheClaimedAttemptAndNothingElse(t *testing.T) {
	dispatcher, client, public, issued := mirrorDispatchFixture(t)
	claim := mirrorClaim()

	outcome, err := dispatcher.Prepare(context.Background(), claim)
	if err != nil {
		t.Fatalf("a configured dispatcher must sign and dispatch a claimed attempt: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("the dispatcher made %d calls, want 1", client.calls)
	}
	// Verified with the public half and the test's clock, so the signature, the lifetime and every
	// binding field are checked as the executor's verifier would check them.
	binding, err := cloneauth.Verify([]ed25519.PublicKey{public}, client.job.Authorization, issued)
	if err != nil {
		t.Fatalf("the dispatched authorization does not verify: %v", err)
	}
	canonical, err := cloneauth.CanonicalRepository(claim.Repository)
	if err != nil {
		t.Fatalf("the fixture repository has no canonical form: %v", err)
	}
	want := cloneauth.Binding{
		AuthorizationID: claim.AuthorizationID,
		Operation:       cloneauth.OperationMirrorPrepare,
		TenantID:        claim.TenantID,
		OwnerUserID:     claim.OwnerUserID,
		ProjectID:       claim.ProjectID,
		WorkspaceID:     claim.WorkspaceID,
		OperationID:     claim.OperationID,
		// The attempt fence travels in the signature: a result for generation 3 must be refused by the
		// worker once generation 4 has been claimed, and the executor's own copy of the number is what
		// lets it tell the job it received from a job for the current attempt.
		PreparationGeneration: claim.Generation,
		ExecutorIdentity:      dispatcher.ExecutorIdentity,
		CredentialID:          claim.CredentialID,
		CredentialVersion:     claim.CredentialVersion,
		Repository:            canonical,
		Ref:                   claim.Ref,
		Capabilities:          claim.Capabilities,
		RuntimeGeneration:     claim.RuntimeGeneration,
		ControlEpoch:          claim.ControlEpoch,
		IssuedAt:              issued,
		ExpiresAt:             issued.Add(dispatcher.Lifetime),
	}
	if binding.AuthorizationID != want.AuthorizationID || binding.Operation != want.Operation ||
		binding.TenantID != want.TenantID || binding.OwnerUserID != want.OwnerUserID ||
		binding.ProjectID != want.ProjectID || binding.WorkspaceID != want.WorkspaceID ||
		binding.OperationID != want.OperationID || binding.PreparationGeneration != want.PreparationGeneration ||
		binding.ExecutorIdentity != want.ExecutorIdentity || binding.CredentialID != want.CredentialID ||
		binding.CredentialVersion != want.CredentialVersion || binding.Repository != want.Repository ||
		binding.Ref != want.Ref || binding.RuntimeGeneration != want.RuntimeGeneration ||
		binding.ControlEpoch != want.ControlEpoch || !binding.IssuedAt.Equal(want.IssuedAt) ||
		!binding.ExpiresAt.Equal(want.ExpiresAt) || !slices.Equal(binding.Capabilities, want.Capabilities) {
		t.Fatalf("the signed binding is\n%+v\nwant\n%+v", binding, want)
	}
	// The mirror purpose is a different subject from a clone, and the payload says so by carrying no
	// execution and none of the three Node runtime values. `Binding.validateMirrorPrepare` refuses a
	// binding that names any of them, so a clone's authorization cannot be presented here and this one
	// cannot be presented on the clone route.
	if binding.ExecutionID != "" || binding.NodeID != "" || binding.NodeInstanceID != "" || binding.NodeIncarnationID != "" {
		t.Fatalf("a mirror authorization named a Node runtime: %+v", binding)
	}
	// The job and the signature must agree on the values the executor re-derives from the job, because
	// the executor compares the two: a job whose repository differed from the signed one would be
	// refused as a mismatch, and a test that only checked the signature would not notice.
	if client.job.OperationID != binding.OperationID || client.job.PreparationGeneration != binding.PreparationGeneration ||
		client.job.ProjectID != binding.ProjectID || client.job.Repository != binding.Repository ||
		client.job.Ref != binding.Ref || client.job.CredentialID != binding.CredentialID ||
		client.job.CredentialVersion != binding.CredentialVersion || client.job.RuntimeGeneration != binding.RuntimeGeneration ||
		client.job.ControlEpoch != binding.ControlEpoch {
		t.Fatalf("the dispatched job is\n%+v\nbut the authorization binds\n%+v", client.job, binding)
	}
	// And what comes back is the evidence the worker writes, not a description of it.
	if outcome.Commit != client.result.Commit || outcome.MirrorGeneration != client.result.Generation {
		t.Fatalf("the outcome is %+v, want the published commit and generation %+v", outcome, client.result)
	}
}

// TestMirrorDispatchRefusesToSignAnIncompleteDeployment pins that every half of the configuration is
// load-bearing and that a missing one refuses *before* anything is dispatched. A dispatcher that
// signed with an empty identity, or with a lifetime outliving the attempt lease, would produce an
// authorization the deployment cannot honor, and the failure would surface as a fetch that runs and
// is then fenced rather than as the misconfiguration it is.
func TestMirrorDispatchRefusesToSignAnIncompleteDeployment(t *testing.T) {
	cases := []struct {
		name   string
		apply  func(d *MirrorDispatcher)
		usable bool
	}{
		{name: "no transport", apply: func(d *MirrorDispatcher) { d.Client = nil }},
		{name: "no signing key", apply: func(d *MirrorDispatcher) { d.SigningKey = nil }},
		{name: "a truncated signing key", apply: func(d *MirrorDispatcher) { d.SigningKey = d.SigningKey[:16] }},
		{name: "no executor identity", apply: func(d *MirrorDispatcher) { d.ExecutorIdentity = "" }},
		{name: "a blank executor identity", apply: func(d *MirrorDispatcher) { d.ExecutorIdentity = "   " }},
		{name: "no lifetime", apply: func(d *MirrorDispatcher) { d.Lifetime = 0 }},
		{name: "a negative lifetime", apply: func(d *MirrorDispatcher) { d.Lifetime = -time.Second }},
		{
			// The one bound that is specific to this capability: an authorization that outlives its
			// attempt's lease authorizes a fetch whose result the generation fence will refuse.
			name:  "a lifetime beyond the attempt lease",
			apply: func(d *MirrorDispatcher) { d.Lifetime = MirrorPreparationLease + time.Second },
		},
		{
			// The whole point of bounding the lifetime is that it is *at most* the lease, so the value
			// equal to it must be accepted rather than refused by an off-by-one.
			name:   "a lifetime exactly at the attempt lease",
			apply:  func(d *MirrorDispatcher) { d.Lifetime = MirrorPreparationLease },
			usable: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dispatcher, client, _, _ := mirrorDispatchFixture(t)
			c.apply(dispatcher)

			outcome, err := dispatcher.Prepare(context.Background(), mirrorClaim())
			if !c.usable {
				if err == nil {
					t.Fatalf("the dispatcher accepted an unusable deployment and returned %+v", outcome)
				}
				if client.calls != 0 {
					t.Fatalf("the dispatcher dispatched %d jobs before refusing", client.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("the dispatcher refused a usable deployment: %v", err)
			}
			if client.calls != 1 {
				t.Fatalf("the dispatcher made %d calls, want 1", client.calls)
			}
		})
	}
}

// TestMirrorDispatchRefusesARepositoryWithNoCanonicalForm pins that the dispatcher derives the
// repository identity rather than trusting the row, so a Project whose URL this deployment cannot
// canonicalize is refused here instead of being signed as written and then refused by the executor as
// a mismatch.
func TestMirrorDispatchRefusesARepositoryWithNoCanonicalForm(t *testing.T) {
	dispatcher, client, _, _ := mirrorDispatchFixture(t)
	claim := mirrorClaim()
	claim.Repository = "http://git.example.invalid/group/private"

	if _, err := dispatcher.Prepare(context.Background(), claim); err == nil {
		t.Fatal("the dispatcher signed an authorization for a repository with no canonical form")
	}
	if client.calls != 0 {
		t.Fatalf("the dispatcher dispatched %d jobs for an uncanonicalizable repository", client.calls)
	}
}

// TestMirrorDispatchReportsTheExecutorRefusalAsACodedFault is the classification test.
//
// The worker settles an Operation from the code on the error, so the dispatcher has to hand back the
// executor's own code when there is one and the unavailable code when the executor could not be
// reached. Getting this wrong is not cosmetic: an outage reported as a refusal can settle an
// Operation that would have succeeded a minute later, and a refusal reported as an outage can retry a
// refusal that can never succeed.
func TestMirrorDispatchReportsTheExecutorRefusalAsACodedFault(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		blankout bool
		wantCode string
	}{
		{
			name:     "the executor's own refusal",
			err:      &cloneexec.DispatcherRefusal{Code: cloneexec.ReasonCodeFetchFailed, Status: 409},
			wantCode: cloneexec.ReasonCodeFetchFailed,
		},
		{
			// An empty code is not a code: it would match nothing in the worker's terminal set, so it is
			// classified as an unavailable executor — which retries — rather than passed through.
			name:     "a refusal with no code of its own",
			err:      &cloneexec.DispatcherRefusal{Status: 500},
			wantCode: cloneexec.ReasonCodeUnavailable,
		},
		{
			name:     "an unreachable executor",
			err:      errors.New("clone executor is unreachable: dial tcp: connection refused"),
			wantCode: cloneexec.ReasonCodeUnavailable,
		},
		{
			// A transport that answered without a commit published nothing a clone could be pinned to,
			// and a `ready` row is required to carry one. The real client refuses this itself, which is
			// why the check here is defensive — the interface is replaceable, and the row that would be
			// written from an empty commit is the one the gate reads.
			name:     "a result with no commit",
			blankout: true,
			wantCode: cloneexec.ReasonCodePublishFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dispatcher, client, _, _ := mirrorDispatchFixture(t)
			client.err = c.err
			if c.blankout {
				client.result.Commit = ""
			}

			outcome, err := dispatcher.Prepare(context.Background(), mirrorClaim())
			if err == nil {
				t.Fatalf("the dispatcher accepted a failed preparation and returned %+v", outcome)
			}
			fault := ErrorCode(err)
			if fault.Code != c.wantCode {
				t.Fatalf("the failure is reported as %q, want %q (error: %v)", fault.Code, c.wantCode, err)
			}
			if fault.Status != 409 {
				t.Fatalf("the failure carries status %d, want 409", fault.Status)
			}
			if outcome != (MirrorPreparationOutcome{}) {
				t.Fatalf("a failed preparation returned an outcome: %+v", outcome)
			}
		})
	}
}
