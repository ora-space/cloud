package core

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
)

// PushDispatchLifetime is how long one push authorization lives.
//
// It is the bound on the window the execution authority's D6 describes as irreducible. Cloud re-checks
// the approval's authority immediately before dispatching, and the Credential Service decides on its own
// whether the authorization may be spent — but neither can see the other's state at the instant in
// between, so a revocation that lands inside that window takes effect at the executor's re-check, at the
// Store's redemption, or at this expiry, whichever comes first. A shorter lifetime shrinks the window;
// it cannot close it, and nothing here should be read as having closed it.
//
// It is well under `cloneauth.MaxLifetime` because a push has no long slow phase to cover: the fetch and
// one small update are what the authorization has to outlive, and the executor's own timeout is what
// bounds them. A lifetime near the maximum would widen the window for no gain.
const PushDispatchLifetime = 2 * time.Minute

// PushClient carries one approved write to the credential-executing component.
//
// It is an interface rather than the concrete `*cloneexec.Client`, for the same reason the mirror
// boundary is one: what this dispatcher has to get right is the *authorization it signs and the verdict
// it reads back*, and both are checked against the real `cloneauth` verification and a scripted executor
// rather than against a socket. The transport itself is exercised end to end by `internal/cloneexec`'s
// own tests, which the production client's shape is fixed by.
type PushClient interface {
	Push(ctx context.Context, job cloneexec.PushJob) (cloneexec.PushResult, error)
}

// PushDispatcher is the production implementation of the push boundary: it signs one `push`
// authorization per approval and hands the write to the component that performs it.
//
// It is the counterpart of `MirrorDispatcher`, and the two share the secrets a dispatcher holds — the
// executor's mutual authentication and Cloud's signing key — while minting different authorizations.
// Every difference from the mirror authorization is one a push needs:
//
//   - the operation is `push`, so the Credential Service applies the write purpose's checks rather than
//     a preparation's, and an authorization minted for anything else is refused by the signature;
//   - the payload carries the commit pair. It is the only operation whose binding does: a write is the
//     only one where "which value, from which value" is the whole of what was authorized, and it is what
//     the compare-and-swap is verified against;
//   - the party permitted to spend it is named by SPIFFE identity, as the mirror's is, because a push
//     also runs on no Node and has no runtime for the caller to state;
//   - the subject is the approval itself — its own id is the operation id — because a push belongs to no
//     Operation (the execution authority's D2).
//
// Nothing about the credential travels back this way either: the token goes from the Credential Service
// to the executor, and Cloud learns only what the remote holds.
type PushDispatcher struct {
	// Client is the executor's endpoint, reached over mutual TLS. It is the same deployment the clone and
	// mirror routes dial — one credential-executing component serves all three — which is why
	// configuration builds one connection rather than three.
	Client PushClient
	// SigningKey is Cloud's Ed25519 authorization key. It is the ability to authorize, and it is held
	// nowhere else: the Credential Service and the executor hold only the public half.
	SigningKey ed25519.PrivateKey
	// ExecutorIdentity is the SPIFFE identity of the credential-executing component as the Credential
	// Service sees it: the URI in the certificate that component presents when it redeems.
	//
	// It is a deployment value rather than a constant because the deployment mints the URI, and it is
	// load-bearing in a way a wrong value is not silent about — the Store compares it against the identity
	// resolved from the *verified client certificate*, so an authorization naming an identity the
	// deployment never issued a certificate to is refused at redemption and no credential is delivered.
	ExecutorIdentity string
	// Lifetime is how long one authorization lives. It must be positive and at most `cloneauth.MaxLifetime`,
	// which the issuer enforces in any case; it is checked here so the invariant holds for every caller of
	// this type rather than only for the one that reads a deployment file.
	Lifetime time.Duration
	// Now supplies the clock, so issuance is deterministic in tests.
	Now func() time.Time
}

// Execute signs one authorization for an approved write and dispatches it.
//
// The two answers it can return are the two the caller's settlement distinguishes, and they are kept
// apart by whether the error carries a reason code. A coded refusal from the executor — a bad job, a
// refused authorization, a fetch that failed, a write that is not expressible — is a concluded answer
// about *this* attempt, and it comes back as a `*Fault` carrying the executor's own code.
//
// Anything else is an executor that could not be reached or could not be understood. It comes back as an
// error with **no** code, which is what makes the caller settle the approval `uncertain` rather than
// `failed`: the write may have landed, so recording a refusal would be Cloud asserting an outcome it was
// not told. The distinction is carried by the code rather than by a verdict type for exactly this reason
// — `pushSettlement` classifies by code, and an error that has one is a concluded failure to it.
//
// It is an error rather than the `PushUnknown` verdict on purpose, and the two are different facts even
// though both settle uncertain: `PushUnknown` is a verdict Cloud reached about the *remote*, while this
// is Cloud admitting it could not read its *own executor*. Keeping them apart is what lets the settlement
// record which component could not answer.
//
//nolint:gocritic // By value on purpose: the execution is a snapshot the approval transaction decided, and signing a copy is what keeps that decision and the signed bytes the same one.
func (d *PushDispatcher) Execute(ctx context.Context, push PushExecution) (PushOutcome, error) {
	if d.Client == nil || len(d.SigningKey) != ed25519.PrivateKeySize {
		return PushOutcome{}, errors.New("push dispatch is not configured")
	}
	// Checked here as well as where the deployment is read, because this is the call that would otherwise
	// sign a binding with an empty identity: `Binding.validate` refuses that, but it refuses it as an
	// incomplete binding, which reads like a bug in the approval rather than like the unconfigured
	// deployment it is.
	if strings.TrimSpace(d.ExecutorIdentity) == "" {
		return PushOutcome{}, errors.New("push dispatch names no executor identity")
	}
	if d.Lifetime <= 0 || d.Lifetime > cloneauth.MaxLifetime {
		return PushOutcome{}, fmt.Errorf("push dispatch authorization lifetime must be positive and at most %s", cloneauth.MaxLifetime)
	}
	// The identity is derived here rather than taken from the row, so that the value the authorization
	// binds is provably the same one the executor re-derives from the job it is handed. A stored URL with
	// no canonical form is not a repository this deployment can write to, and it is refused before a
	// signature exists rather than after a credential has been spent.
	repository, err := cloneauth.CanonicalRepository(push.Repository)
	if err != nil {
		return PushOutcome{}, err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	issued := now().UTC()
	authorization, err := cloneauth.Issue(d.SigningKey, cloneauth.Binding{
		AuthorizationID: push.AuthorizationID,
		Operation:       cloneauth.OperationPush,
		TenantID:        push.TenantID,
		OwnerUserID:     push.OwnerUserID,
		ProjectID:       push.ProjectID,
		WorkspaceID:     push.WorkspaceID,
		// The approval's own id is the operation id: a push belongs to no Operation, and the binding's
		// subject is therefore the decision that authorized it. The remaining identity fields are left at
		// their zero values deliberately — `Binding.validatePush` refuses an execution id and the whole
		// Node runtime set, so a push that carried one would not be signable at all.
		OperationID:       push.OperationID,
		ExecutorIdentity:  d.ExecutorIdentity,
		CredentialID:      push.CredentialID,
		CredentialVersion: push.CredentialVersion,
		Repository:        repository,
		Ref:               push.Ref,
		OldCommit:         push.OldCommit,
		NewCommit:         push.NewCommit,
		Capabilities:      push.Capabilities,
		IssuedAt:          issued,
		ExpiresAt:         issued.Add(d.Lifetime),
	})
	if err != nil {
		// A binding this package will not sign is a programming error rather than a push outcome: every
		// value came from Cloud's own approval row, not from a caller.
		return PushOutcome{}, err
	}
	result, err := d.Client.Push(ctx, cloneexec.PushJob{
		OperationID:       push.OperationID,
		ProjectID:         push.ProjectID,
		WorkspaceID:       push.WorkspaceID,
		Repository:        repository,
		Ref:               push.Ref,
		OldCommit:         push.OldCommit,
		NewCommit:         push.NewCommit,
		CredentialID:      push.CredentialID,
		CredentialVersion: push.CredentialVersion,
		Authorization:     authorization,
	})
	if err != nil {
		return PushOutcome{}, pushDispatchFault(err)
	}
	verdict, ok := pushVerdictOf(result.State)
	if !ok {
		// The executor answered with a state this build does not define. It is reported as an error, not
		// as `PushUnknown`, because the two are different facts and the caller records them differently:
		// `PushUnknown` is a verdict this Cloud reached about the remote, while this is Cloud admitting it
		// cannot read its own executor. Both settle uncertain, and keeping them apart is what lets the
		// reason code say which component is at fault.
		return PushOutcome{}, fmt.Errorf("the push executor reported an unknown state %q", result.State)
	}
	// The cleanup failure rides beside the verdict rather than replacing it, because the two are answers to
	// different questions: the verdict is what this attempt established about the remote, and the cleanup
	// failure is whether the executor could prove its own copy of the credential gone. An executor that
	// found the write refused *and* could not remove its material reports both, and the refusal is the one
	// that reaches this line as an error — the executor's own log carries the cleanup failure with the
	// attempt's identifiers, which is the only place it can appear once the attempt is a refusal.
	return PushOutcome{
		Verdict:        verdict,
		ObservedCommit: strings.TrimSpace(result.RemoteCommit),
		CleanupFailure: strings.TrimSpace(result.CleanupFailure),
	}, nil
}

// pushVerdictOf translates the executor's wire state into this package's verdict.
//
// It is total over the executor's vocabulary and reports whether it recognized the value, rather than
// defaulting. A default would have to choose between `Unknown` — which would mean Cloud decided the
// outcome was unestablished when it actually failed to read the answer — and a silent success, and there
// is no third choice that is honest. The caller handles the false case as what it is.
func pushVerdictOf(state cloneexec.PushState) (PushVerdict, bool) {
	switch state {
	case cloneexec.PushConfirmed:
		return PushConfirmed, true
	case cloneexec.PushTargetSatisfied:
		return PushTargetSatisfied, true
	case cloneexec.PushNotApplied:
		return PushNotApplied, true
	case cloneexec.PushTargetConflict:
		return PushTargetConflict, true
	case cloneexec.PushUnknown:
		return PushUnknown, true
	default:
		return "", false
	}
}

// pushDispatchFault names a dispatch failure in the form the caller classifies.
//
// The two halves are the ones every dispatch boundary in this package distinguishes, and preserving the
// difference between them is the whole of what this function may do. A refusal the executor decided
// arrives with its own stable code and is handed back as a coded fault; a transport failure, a lost
// reply, or an answer that could not be read carries no code and is handed back **as it is**.
//
// The second half is deliberately given no code of its own, and the reason is the caller's rule rather
// than a shortage of names. A code is what `pushSettlement` settles a *concluded* failure with, and an
// executor that could not be reached may still have written: inventing a code here would record a
// possible write as a refusal, which is the one direction a write must never be settled in. Left
// uncoded, the error reaches `pushSettlement` as the unknown case it is and settles `uncertain` under
// `push_result_unknown`. This is not a hypothetical path: it is what an interrupted dispatch — the
// executor killed after it accepted the write — actually produces, and the container-level run measures
// the settlement it must end in.
//
// A refusal that carries no code is treated the same way, and for the same reason rather than as a
// fallback: an empty code is not evidence that the executor decided anything. The shipped client
// substitutes a code for an empty one only when the executor itself answered with a body this build
// could read, so a codeless error arriving here is one no component vouched for, and the conservative
// reading of "no component said anything" is the unknown outcome, not the concluded one.
func pushDispatchFault(err error) error {
	var refusal interface{ ReasonCode() string }
	if errors.As(err, &refusal) {
		if code := refusal.ReasonCode(); code != "" {
			return &Fault{Status: 409, Code: code}
		}
	}
	return err
}
