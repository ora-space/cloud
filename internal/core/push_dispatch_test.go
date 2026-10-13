package core

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
)

// This file covers the dispatcher: the layer that turns one approved write into a signed authorization
// and one call to the executor, and that reads the executor's answer back as a verdict.
//
// Two things are asserted here and nothing else, because they are the two the dispatcher owns. The first
// is the *signed bytes*: every case verifies the authorization with the real `cloneauth.Verify` against
// the public half of a real key, so what is checked is the payload an executor would actually receive
// rather than a restatement of the fields this package believes it set. The second is the classification
// of the answer, because a dispatcher that merged "the executor said no" with "the executor could not be
// reached" would make the caller record an outage as a concluded failure — for a write, the one
// direction that must never be taken.

// pushTestScene is a dispatcher with a real key, a scripted client, and the execution it is handed.
type pushTestScene struct {
	dispatcher *PushDispatcher
	client     *scriptedPushClient
	public     ed25519.PublicKey
	execution  PushExecution
	now        time.Time
}

// newPushTestScene builds the scene. The execution is shaped like the one `pushExecution` reads back —
// including the canonical upstream and the single permitted capability — so a failure names the
// dispatcher rather than a fixture that disagreed with the real caller.
func newPushTestScene(t *testing.T) pushTestScene {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate a signing key: %v", err)
	}
	client := &scriptedPushClient{result: cloneexec.PushResult{State: cloneexec.PushConfirmed, RemoteCommit: pushNewCommit}}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	return pushTestScene{
		dispatcher: &PushDispatcher{
			Client: client, SigningKey: key, ExecutorIdentity: pushExecutorIdentity,
			Lifetime: PushDispatchLifetime, Now: func() time.Time { return now },
		},
		client: client, public: public, now: now,
		execution: PushExecution{
			AuthorizationID:   "3f1d2c3b-4a59-4687-9b01-2c3d4e5f6071",
			TenantID:          "0f1e2d3c-4b5a-6978-8c9d-0e1f2a3b4c5d",
			OwnerUserID:       "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
			ProjectID:         "2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e",
			WorkspaceID:       "3c4d5e6f-7a8b-4c9d-8e1f-2a3b4c5d6e7f",
			OperationID:       "4d5e6f7a-8b9c-4d0e-9f1a-2b3c4d5e6f7a",
			CredentialID:      "5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b",
			CredentialVersion: 3,
			Repository:        admittedScope,
			Ref:               pushRef,
			OldCommit:         pushOldCommit,
			NewCommit:         pushNewCommit,
			Capabilities:      []string{"push"},
		},
	}
}

// pushExecutorIdentity is the SPIFFE URI the deployment gives the credential-executing component. It is
// a value, not a constant of this package, and the tests spell it out so a case that asserts it is
// asserting the deployment's identity rather than echoing whatever the dispatcher happened to hold.
const pushExecutorIdentity = "spiffe://ora/node/credential-executor"

// scriptedPushClient records the job it was handed and answers with the scripted result or error.
type scriptedPushClient struct {
	jobs   []cloneexec.PushJob
	result cloneexec.PushResult
	err    error
}

func (c *scriptedPushClient) Push(_ context.Context, job cloneexec.PushJob) (cloneexec.PushResult, error) {
	c.jobs = append(c.jobs, job)
	return c.result, c.err
}

// pushClientRefusal is a coded refusal from a client, shaped like the real client's.
type pushClientRefusal struct{ code string }

func (e pushClientRefusal) Error() string      { return e.code }
func (e pushClientRefusal) ReasonCode() string { return e.code }

// TestTheDispatcherSignsExactlyTheWriteItWasGiven is the signed-payload case, and it is asserted by
// verifying rather than by inspecting the binding the dispatcher built: the only thing an executor ever
// sees is the token, so the token is what the case has to check.
//
// Every field that carries authority is asserted, and the ones that must be *absent* are asserted too.
// The absent ones are the whole reason a push has its own operation: it runs on no Node, so a payload
// carrying a runtime, an execution id or a preparation attempt would be one the Store's push branch
// could not interpret — and `Binding.validatePush` refuses it rather than signing it, which is what
// makes these assertions about the deployment instead of about a habit of the code.
func TestTheDispatcherSignsExactlyTheWriteItWasGiven(t *testing.T) {
	scene := newPushTestScene(t)

	outcome, err := scene.dispatcher.Execute(context.Background(), scene.execution)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(scene.client.jobs) != 1 {
		t.Fatalf("the client was called %d times, want 1", len(scene.client.jobs))
	}
	token := scene.client.jobs[0].Authorization
	if token == "" {
		t.Fatal("the job carried no authorization")
	}

	binding, err := cloneauth.Verify([]ed25519.PublicKey{scene.public}, token, scene.now)
	if err != nil {
		t.Fatalf("the authorization the dispatcher signed does not verify: %v", err)
	}
	if binding.Operation != cloneauth.OperationPush {
		t.Fatalf("signed operation = %q, want %q", binding.Operation, cloneauth.OperationPush)
	}
	if binding.AuthorizationID != scene.execution.AuthorizationID {
		t.Fatalf("signed authorization id = %q, want %q", binding.AuthorizationID, scene.execution.AuthorizationID)
	}
	// The approval's own id is the operation id: a push belongs to no Operation, so the subject the
	// binding names is the decision that authorized it.
	if binding.OperationID != scene.execution.OperationID {
		t.Fatalf("signed operation id = %q, want the approval's %q", binding.OperationID, scene.execution.OperationID)
	}
	for _, field := range []struct{ name, got, want string }{
		{"tenant", binding.TenantID, scene.execution.TenantID},
		{"owner", binding.OwnerUserID, scene.execution.OwnerUserID},
		{"project", binding.ProjectID, scene.execution.ProjectID},
		{"workspace", binding.WorkspaceID, scene.execution.WorkspaceID},
		{"credential", binding.CredentialID, scene.execution.CredentialID},
		{"repository", binding.Repository, admittedScope},
		{"ref", binding.Ref, pushRef},
		{"old commit", binding.OldCommit, pushOldCommit},
		{"new commit", binding.NewCommit, pushNewCommit},
		{"executor", binding.ExecutorIdentity, pushExecutorIdentity},
	} {
		if field.got != field.want {
			t.Fatalf("signed %s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if binding.CredentialVersion != scene.execution.CredentialVersion {
		t.Fatalf("signed credential version = %d, want %d", binding.CredentialVersion, scene.execution.CredentialVersion)
	}
	if len(binding.Capabilities) != 1 || binding.Capabilities[0] != "push" {
		t.Fatalf("signed capabilities = %v, want exactly [push]", binding.Capabilities)
	}
	// A push is not a Node workload, and the payload says so by carrying nothing rather than by carrying
	// zeroes that a reader would have to interpret.
	if binding.ExecutionID != "" || binding.NodeID != "" || binding.NodeInstanceID != "" ||
		binding.NodeIncarnationID != "" || binding.RuntimeGeneration != 0 || binding.ControlEpoch != 0 ||
		binding.PreparationGeneration != 0 {
		t.Fatalf("a push authorization carried runtime or execution identity: %+v", binding)
	}
	// The lifetime is the dispatcher's, measured from the dispatcher's own clock, and it is strictly
	// inside the issuer's maximum — the bound on the window the two revocation checkpoints cannot close.
	if !binding.IssuedAt.Equal(scene.now) || !binding.ExpiresAt.Equal(scene.now.Add(PushDispatchLifetime)) {
		t.Fatalf("signed window = %s..%s, want %s..%s", binding.IssuedAt, binding.ExpiresAt, scene.now, scene.now.Add(PushDispatchLifetime))
	}
	if PushDispatchLifetime >= cloneauth.MaxLifetime {
		t.Fatalf("the dispatch lifetime %s must stay strictly inside the issuer's maximum %s", PushDispatchLifetime, cloneauth.MaxLifetime)
	}
	if outcome.Verdict != PushConfirmed || outcome.ObservedCommit != pushNewCommit {
		t.Fatalf("outcome = %+v, want a confirmed verdict with the observed commit", outcome)
	}
}

// TestTheJobCarriesTheSameWriteAsTheAuthorization is the other half of "the two agree": the executor
// re-derives the repository identity from the job, so a job that named one repository while the
// authorization bound another would be refused at the far end as a binding mismatch — a failure that
// would look like an attack rather than like the dispatcher bug it is.
func TestTheJobCarriesTheSameWriteAsTheAuthorization(t *testing.T) {
	scene := newPushTestScene(t)
	// The execution arrives with the upstream as the approval stored it, which is already canonical. The
	// case asserts the job does not degrade it — the value the executor dials is the value the Store's
	// binding was compared against.
	if _, err := scene.dispatcher.Execute(context.Background(), scene.execution); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	job := scene.client.jobs[0]
	if job.Repository != admittedScope || job.Ref != pushRef || job.OldCommit != pushOldCommit || job.NewCommit != pushNewCommit {
		t.Fatalf("the job's write disagrees with the authorization's: %+v", job)
	}
	if job.OperationID != scene.execution.OperationID || job.ProjectID != scene.execution.ProjectID ||
		job.WorkspaceID != scene.execution.WorkspaceID || job.CredentialID != scene.execution.CredentialID ||
		job.CredentialVersion != scene.execution.CredentialVersion {
		t.Fatalf("the job's binding disagrees with the approval's: %+v", job)
	}
}

// TestTheDispatcherReportsEveryVerdictTheExecutorCanState pins the translation as total over the
// executor's vocabulary.
//
// A missing row here would not fail loudly. It would fall to the dispatcher's unknown-state branch and
// be recorded as "Cloud could not read its executor", which is a different finding from any of these —
// so the table is the thing that keeps a state from being silently reclassified, and it is written out
// rather than derived so that adding a state to the executor is a deliberate act on this side too.
func TestTheDispatcherReportsEveryVerdictTheExecutorCanState(t *testing.T) {
	cases := []struct {
		state     cloneexec.PushState
		want      PushVerdict
		observed  string
		wantKnown bool
	}{
		{state: cloneexec.PushConfirmed, want: PushConfirmed, observed: pushNewCommit, wantKnown: true},
		{state: cloneexec.PushTargetSatisfied, want: PushTargetSatisfied, observed: pushNewCommit, wantKnown: true},
		{state: cloneexec.PushNotApplied, want: PushNotApplied, observed: pushOldCommit, wantKnown: true},
		{state: cloneexec.PushTargetConflict, want: PushTargetConflict, observed: pushThirdCommit, wantKnown: true},
		{state: cloneexec.PushUnknown, want: PushUnknown, wantKnown: true},
		{state: cloneexec.PushState("something-else"), wantKnown: false},
	}
	for _, testCase := range cases {
		t.Run(string(testCase.state), func(t *testing.T) {
			scene := newPushTestScene(t)
			scene.client.result = cloneexec.PushResult{State: testCase.state, RemoteCommit: testCase.observed}

			outcome, err := scene.dispatcher.Execute(context.Background(), scene.execution)
			if !testCase.wantKnown {
				// An unrecognized state is reported as an error and never as a verdict: Cloud cannot read
				// its own executor, which is not the same claim as "the remote's state is unknown".
				if err == nil {
					t.Fatalf("an unrecognized executor state was accepted as the verdict %q", outcome.Verdict)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if outcome.Verdict != testCase.want {
				t.Fatalf("verdict = %q, want %q", outcome.Verdict, testCase.want)
			}
			if outcome.ObservedCommit != testCase.observed {
				t.Fatalf("observed commit = %q, want %q", outcome.ObservedCommit, testCase.observed)
			}
		})
	}
}

// TestTheDispatcherSeparatesARefusalFromAnUnreachableExecutor is the classification the caller's
// settlement depends on, and the two cases must not be interchangeable.
//
// A coded refusal is a concluded answer about this attempt and is handed back with the executor's own
// code, which is what lets the audit say *why* the write did not happen. Anything else — a dial failure,
// a reset connection, an unreadable answer — is handed back as the executor being unavailable, and the
// caller settles that as an uncertain outcome rather than a failure, because an executor that could not
// be reached may still have written.
func TestTheDispatcherSeparatesARefusalFromAnUnreachableExecutor(t *testing.T) {
	t.Run("a coded refusal keeps its code", func(t *testing.T) {
		scene := newPushTestScene(t)
		scene.client.err = pushClientRefusal{code: cloneexec.ReasonCodePushNotApplicable}

		_, err := scene.dispatcher.Execute(context.Background(), scene.execution)
		code, coded := pushRefusalCode(err)
		if !coded || code != cloneexec.ReasonCodePushNotApplicable {
			t.Fatalf("refusal read back as (%q, %v), want the executor's own code", code, coded)
		}
		var fault *Fault
		if !errors.As(err, &fault) || fault.Status != 409 {
			t.Fatalf("a refusal must reach the caller as a 409 fault, got %v", err)
		}
	})

	t.Run("an unreachable executor is not a refusal", func(t *testing.T) {
		scene := newPushTestScene(t)
		scene.client.err = errors.New("dial tcp: connection refused")

		_, err := scene.dispatcher.Execute(context.Background(), scene.execution)
		code, coded := pushRefusalCode(err)
		if !coded || code != cloneexec.ReasonCodeUnavailable {
			t.Fatalf("an unreachable executor read back as (%q, %v), want the unavailable code", code, coded)
		}
	})

	t.Run("a refusal with no code is an unavailable executor", func(t *testing.T) {
		// A coded refusal whose code is empty is the one shape the caller cannot classify: an empty code
		// is not a code, it would match no entry in any table of terminal refusals, and passing it through
		// would settle the approval with nothing. It is therefore reported as the executor being
		// unavailable, which is true of a component that cannot say why it refused.
		scene := newPushTestScene(t)
		scene.client.err = pushClientRefusal{}

		_, err := scene.dispatcher.Execute(context.Background(), scene.execution)
		code, coded := pushRefusalCode(err)
		if !coded || code != cloneexec.ReasonCodeUnavailable {
			t.Fatalf("a codeless refusal read back as (%q, %v), want the unavailable code", code, coded)
		}
	})
}

// TestTheDispatcherRefusesToSignFromAnIncompleteDeployment is the configuration boundary. Each case
// removes one thing the dispatcher cannot work without, and none of them may reach the network: a
// deployment that cannot sign must refuse before a write is anywhere near, and a partial configuration
// that signed anyway would produce an authorization naming an identity no certificate matches.
func TestTheDispatcherRefusesToSignFromAnIncompleteDeployment(t *testing.T) {
	cases := []struct {
		name    string
		disable func(scene *pushTestScene)
	}{
		{name: "no client", disable: func(s *pushTestScene) { s.dispatcher.Client = nil }},
		{name: "no signing key", disable: func(s *pushTestScene) { s.dispatcher.SigningKey = ed25519.PrivateKey{} }},
		{name: "no executor identity", disable: func(s *pushTestScene) { s.dispatcher.ExecutorIdentity = "  " }},
		{name: "no lifetime", disable: func(s *pushTestScene) { s.dispatcher.Lifetime = 0 }},
		{name: "a lifetime beyond the issuer's maximum", disable: func(s *pushTestScene) { s.dispatcher.Lifetime = cloneauth.MaxLifetime + time.Second }},
		{name: "a repository with no canonical form", disable: func(s *pushTestScene) { s.execution.Repository = "ssh://git@example.invalid/group/private.git" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scene := newPushTestScene(t)
			testCase.disable(&scene)

			if _, err := scene.dispatcher.Execute(context.Background(), scene.execution); err == nil {
				t.Fatal("an incomplete deployment signed an authorization")
			}
			if len(scene.client.jobs) != 0 {
				t.Fatalf("an incomplete deployment reached the executor %d times", len(scene.client.jobs))
			}
		})
	}
}
