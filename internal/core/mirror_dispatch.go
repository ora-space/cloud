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

// MirrorPreparationClient carries one preparation to the credential-executing component.
//
// It is an interface rather than the concrete `*cloneexec.Client`, unlike the field `CloneDispatcher`
// holds, and the difference is deliberate. The clone path has a real-chain acceptance test that builds
// the genuine mutual-TLS client; what this dispatcher has to get right is the *authorization it signs*,
// and that is checked against the real `cloneauth` verification rather than against a real socket.
// Making the transport replaceable is what lets a test assert the signed bytes without standing up a
// second PKI, while the real route keeps being exercised end to end by `internal/cloneexec`'s own
// acceptance test.
type MirrorPreparationClient interface {
	PrepareMirror(ctx context.Context, job cloneexec.MirrorJob) (cloneexec.MirrorResult, error)
}

// MirrorDispatcher is the production implementation of the mirror boundary: it signs one
// `mirror_prepare` authorization per claimed attempt and hands the fetch to the component that performs
// it.
//
// It is the counterpart of CloneDispatcher and it shares that dispatcher's two secrets — the executor's
// mutual authentication and Cloud's signing key — but it mints a different authorization, and every
// difference is one the clone path could not express:
//
//   - the operation is `mirror_prepare`, so a credential presented on the wrong route is refused by the
//     signature rather than by the route it arrived on;
//   - the subject is a Project Operation and an attempt, not an execution on a Node, so the payload
//     carries no execution id and forbids the whole Node runtime triple;
//   - the ref is signed rather than carried beside the signature, because the published mirror is keyed
//     by the commit that ref resolved to and a reader compares it against Cloud's rows;
//   - the party permitted to spend it is named by SPIFFE identity rather than by a runtime the caller
//     would have to self-declare, because the executor runs on no Node and has no runtime to state.
//
// Nothing about the credential travels back this way either: the token goes from the Credential Service
// to the executor, and Cloud learns only the commit and the mirror generation.
type MirrorDispatcher struct {
	// Client is the executor's endpoint, reached over mutual TLS. It is the same deployment the clone
	// path dials — one credential-executing component serves both routes — which is why configuration
	// builds one connection rather than two.
	Client MirrorPreparationClient
	// SigningKey is Cloud's Ed25519 authorization key. It is the ability to authorize, and it is held
	// nowhere else: the Credential Service and the executor hold only the public half.
	SigningKey ed25519.PrivateKey
	// ExecutorIdentity is the SPIFFE identity of the credential-executing component as the Credential
	// Service sees it, meaning the URI in the certificate that component presents when it redeems.
	//
	// It is a deployment value rather than a constant here because the URI is minted by the deployment's
	// own setup, and it is load-bearing in a way a wrong value is not silent about: the Store compares it
	// against the identity resolved from the *verified client certificate*, so an authorization naming an
	// identity the deployment never issued a certificate to is refused at redemption — the fetch fails
	// and no credential is delivered. The two spellings have to agree, and this field is the only place
	// Cloud states its half of that agreement.
	ExecutorIdentity string
	// Lifetime is how long one authorization lives. It must be positive and must not exceed
	// MirrorPreparationLease: an authorization outliving its attempt's lease would permit a fetch whose
	// result the generation fence will refuse.
	Lifetime time.Duration
	// Now supplies the clock, so issuance is deterministic in tests.
	Now func() time.Time
}

// Prepare issues one authorization for a claimed attempt and dispatches it.
//
// The failure it returns is either the executor's own coded refusal or a transport failure, and the two
// are deliberately not merged: a refusal is an outcome of this attempt, while an unreachable executor
// leaves the attempt's outcome unknown. Both are handed back as a `*Fault` carrying the reason code, so
// the worker classifies them the way it classifies every other refusal rather than seeing a generic
// internal error.
//
//nolint:gocritic // By value on purpose: the claimed attempt is a snapshot of what the claim transaction decided, and signing a copy is what keeps that decision and the signed bytes the same one.
func (d *MirrorDispatcher) Prepare(ctx context.Context, p MirrorPreparation) (MirrorPreparationOutcome, error) {
	if d.Client == nil || len(d.SigningKey) != ed25519.PrivateKeySize {
		return MirrorPreparationOutcome{}, errors.New("mirror dispatch is not configured")
	}
	// The identity is checked here as well as where the deployment is read, because this is the call that
	// would otherwise sign a binding with an empty identity: `Binding.validate` refuses that, but it
	// refuses it as an incomplete binding, which reads like a bug in the claim rather than like the
	// unconfigured deployment it is.
	if strings.TrimSpace(d.ExecutorIdentity) == "" {
		return MirrorPreparationOutcome{}, errors.New("mirror dispatch names no executor identity")
	}
	// Stated here rather than only at configuration load, so the invariant holds for every caller of this
	// type and not just for the one that reads a deployment file.
	if d.Lifetime <= 0 || d.Lifetime > MirrorPreparationLease {
		return MirrorPreparationOutcome{}, fmt.Errorf("mirror dispatch authorization lifetime must be positive and at most %s", MirrorPreparationLease)
	}
	// The canonical identity is derived here rather than taken from the row, exactly as the clone path
	// derives it: the claim read the Project's stored URL, and the value the authorization binds has to be
	// the value the executor re-derives from the job it is handed. A URL with no canonical form is not a
	// repository this deployment can prepare.
	repository, err := cloneauth.CanonicalRepository(p.Repository)
	if err != nil {
		return MirrorPreparationOutcome{}, err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	issued := now().UTC()
	authorization, err := cloneauth.Issue(d.SigningKey, cloneauth.Binding{
		AuthorizationID: p.AuthorizationID,
		Operation:       cloneauth.OperationMirrorPrepare,
		TenantID:        p.TenantID,
		OwnerUserID:     p.OwnerUserID,
		ProjectID:       p.ProjectID,
		WorkspaceID:     p.WorkspaceID,
		// The attempt's identity is the Operation and the generation, never an execution: this payload
		// names no execution and no Node at all, and `Binding.validateMirrorPrepare` refuses one that does.
		OperationID:           p.OperationID,
		PreparationGeneration: p.Generation,
		ExecutorIdentity:      d.ExecutorIdentity,
		CredentialID:          p.CredentialID,
		CredentialVersion:     p.CredentialVersion,
		Repository:            repository,
		Ref:                   p.Ref,
		Capabilities:          p.Capabilities,
		RuntimeGeneration:     p.RuntimeGeneration,
		ControlEpoch:          p.ControlEpoch,
		IssuedAt:              issued,
		ExpiresAt:             issued.Add(d.Lifetime),
	})
	if err != nil {
		// A binding this package will not sign is a programming error rather than a preparation outcome:
		// every value comes from Cloud's own claim, not from a caller.
		return MirrorPreparationOutcome{}, err
	}
	result, err := d.Client.PrepareMirror(ctx, cloneexec.MirrorJob{
		OperationID:           p.OperationID,
		PreparationGeneration: p.Generation,
		ProjectID:             p.ProjectID,
		Repository:            repository,
		Ref:                   p.Ref,
		CredentialID:          p.CredentialID,
		CredentialVersion:     p.CredentialVersion,
		RuntimeGeneration:     p.RuntimeGeneration,
		ControlEpoch:          p.ControlEpoch,
		Authorization:         authorization,
	})
	if err != nil {
		return MirrorPreparationOutcome{}, mirrorPreparationFault(err)
	}
	// The commit is the evidence a `ready` preparation must carry: the schema constrains a ready row to
	// have one, and a value written from an empty reply would satisfy the constraint while naming nothing
	// a later clone could be pinned to. A transport that answered without one published nothing this
	// Cloud can reference, so it is reported as a failure of this attempt rather than accepted.
	if strings.TrimSpace(result.Commit) == "" {
		return MirrorPreparationOutcome{}, &Fault{
			Status: 409, Code: cloneexec.ReasonCodePublishFailed,
			Params: Object{"reason": "the executor reported no commit"},
		}
	}
	return MirrorPreparationOutcome{Commit: result.Commit, MirrorGeneration: result.Generation}, nil
}

// mirrorPreparationFault names a dispatch failure in the form the worker classifies.
//
// The two halves are the ones every dispatch boundary in this package distinguishes: a refusal the
// executor decided arrives with its own stable code, while a transport failure has no code of its own
// and is reported as the executor being unavailable rather than as a refusal. Reporting the second as
// the first would let an outage settle an Operation, and reporting it as an internal error would hide
// which component is down.
//
// A refusal that carries no code is classified as unavailable rather than passed through as an empty
// code. Both shipped clients substitute the unavailable code for an empty one before returning, so
// this guards the interface rather than them: an empty code is not a code, it would match no entry in
// `terminalCredentialRefusals`, and a fault with no code is the one shape the worker cannot classify.
func mirrorPreparationFault(err error) error {
	var refusal interface{ ReasonCode() string }
	if errors.As(err, &refusal) {
		if code := refusal.ReasonCode(); code != "" {
			return &Fault{Status: 409, Code: code}
		}
	}
	return &Fault{Status: 409, Code: cloneexec.ReasonCodeUnavailable}
}
