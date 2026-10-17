package cloneauth

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"
)

// Purpose is the single kind of task an Authorization permits. The purposes are mutually exclusive:
// each forbids the fields of the others, so a payload can never be read as two kinds of work.
type Purpose string

// The execution purposes. The earlier clone, mirror_prepare and push purposes are not defined here, so
// a payload naming one is refused as an unknown purpose.
const (
	// PurposeSync fetches one external branch and pushes a snapshot to the Git Server.
	PurposeSync Purpose = "sync"
	// PurposePublish fast-forwards one external branch to a commit taken from the Git Server.
	PurposePublish Purpose = "publish"
	// PurposeProbe only reads a ref advertisement. It exists so capability verification and the
	// check after an uncertain publish never hold an authorization that could push.
	PurposeProbe Purpose = "probe"
)

// ProbeService values: which advertisement a probe reads.
const (
	ServiceUploadPack  = "upload-pack"
	ServiceReceivePack = "receive-pack"
)

// ExecutorIdentity is the only SPIFFE identity an Authorization may name. The Credential Store
// compares it with the verified peer certificate, so the party that may redeem is the one the
// deployment issued that certificate to, never one a request body names.
const ExecutorIdentity = "spiffe://ora.local/repo-executor"

// MaxLifetime bounds any Authorization. It only bounds "start the task and redeem the credential";
// how long the task then runs is governed by the Operation lease, not by this lifetime.
const MaxLifetime = 15 * time.Minute

// Authorization is everything one execution grant is bound to. The executor compares every field with
// the task it was asked to run and refuses any difference before redeeming anything.
//
// Which fields are required and which must be empty depends on Purpose; see Sign.
type Authorization struct {
	// AuthorizationID is the canonical UUID the Credential Store records the single redemption under.
	AuthorizationID string `json:"authorizationId"`
	// Purpose is the single kind of task permitted.
	Purpose Purpose `json:"purpose"`
	// ExecutorIdentity must equal the package constant ExecutorIdentity.
	ExecutorIdentity string `json:"executorIdentity"`
	// IssuedAt and ExpiresAt bound the lifetime, in UTC, set by the issuer's clock.
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// OperationID names the Cloud Operation (required for sync and publish; a probe carries exactly
	// one of OperationID and VerificationID).
	OperationID string `json:"operationId,omitempty"`
	// VerificationID names a credential capability verification (probe only).
	VerificationID string `json:"verificationId,omitempty"`
	// TenantID, ProjectID and BindingID scope the task. All three are required for sync and publish; a
	// probe requires TenantID and may carry the other two.
	TenantID  string `json:"tenantId"`
	ProjectID string `json:"projectId,omitempty"`
	BindingID string `json:"bindingId,omitempty"`
	// Repository is the external repository URL exactly as the Binding records it, so the executor
	// fetches what the user named (some hosts do not answer the ".git"-less spelling). It must be a
	// URL NormalizeRepository accepts; scope comparisons always go through NormalizeRepository.
	Repository string `json:"repository"`
	// Branch and ExpectedTrackingOID are the sync's upstream branch short name and the tracking ref
	// value the Git Server must still hold (all zeros: the tracking ref must not exist). Sync only.
	Branch              string `json:"branch,omitempty"`
	ExpectedTrackingOID string `json:"expectedTrackingOid,omitempty"`
	// SourceRef and SourceCommit name what a publish pushes: a commit reachable from that Git Server
	// ref. TargetRef is the external "refs/heads/<branch>" and ExpectedOldOID the value it must still
	// hold (all zeros: create). Publish only, except that a probe may carry TargetRef.
	SourceRef      string `json:"sourceRef,omitempty"`
	SourceCommit   string `json:"sourceCommit,omitempty"`
	TargetRef      string `json:"targetRef,omitempty"`
	ExpectedOldOID string `json:"expectedOldOid,omitempty"`
	// Service is the advertisement a probe reads: ServiceUploadPack or ServiceReceivePack. Probe only.
	Service string `json:"service,omitempty"`
	// CredentialID and CredentialVersion pin the exact credential version to redeem. Only a sync may
	// be anonymous (both empty/zero); CredentialVersion is positive exactly when CredentialID is set.
	CredentialID      string `json:"credentialId,omitempty"`
	CredentialVersion int64  `json:"credentialVersion,omitempty"`
}

// Sign validates a and returns its token.
//
// It applies the same field rules as Verify (everything except the clock): it refuses an unknown
// purpose, a missing required field, a field the purpose forbids, a malformed ID, OID, ref or
// repository, and a lifetime outside (0, MaxLifetime]. The instants are converted to UTC first, so
// callers may pass times from any location.
//
//nolint:gocritic // Value parameter on purpose: the snapshot validated is the snapshot signed.
func Sign(key ed25519.PrivateKey, a Authorization) (string, error) {
	a.IssuedAt = a.IssuedAt.UTC()
	a.ExpiresAt = a.ExpiresAt.UTC()
	if err := a.validate(); err != nil {
		return "", err
	}
	return sign(key, a)
}

// Verify authenticates token under any of keys and returns the Authorization it carries.
//
// keys is a set so a deployment can rotate the signing key with an overlap window. now is the
// verifier's own clock: the grant must not be expired (now < ExpiresAt) and IssuedAt may lie at most
// MaxClockSkew in the future. A submission token is refused here (ErrMalformed), as are payloads with
// unknown fields.
func Verify(token string, keys []ed25519.PublicKey, now time.Time) (Authorization, error) {
	var a Authorization
	if err := open(token, keys, &a); err != nil {
		return Authorization{}, err
	}
	if err := a.validate(); err != nil {
		return Authorization{}, err
	}
	if err := checkWindow(a.IssuedAt, a.ExpiresAt, now); err != nil {
		return Authorization{}, err
	}
	return a, nil
}

// field is one named value in a presence rule.
type field struct {
	name    string
	present bool
}

// validate applies the clock-independent rules shared by Sign and Verify.
//
//nolint:gocritic // Value receiver on purpose: validation must not be able to modify the snapshot.
func (a Authorization) validate() error {
	switch a.Purpose {
	case PurposeSync, PurposePublish, PurposeProbe:
	default:
		return fmt.Errorf("%w: purpose %q is not defined", ErrInvalid, a.Purpose)
	}
	if !validUUID(a.AuthorizationID) {
		return fmt.Errorf("%w: authorizationId is not a canonical UUID", ErrInvalid)
	}
	if a.ExecutorIdentity != ExecutorIdentity {
		return fmt.Errorf("%w: executorIdentity is not %s", ErrInvalid, ExecutorIdentity)
	}
	if err := checkLifetime(a.IssuedAt, a.ExpiresAt, MaxLifetime); err != nil {
		return err
	}
	if _, err := NormalizeRepository(a.Repository); err != nil || strings.TrimSpace(a.Repository) != a.Repository {
		return fmt.Errorf("%w: repository is not a valid https repository URL", ErrInvalid)
	}
	if err := a.validateCredential(); err != nil {
		return err
	}
	switch a.Purpose {
	case PurposeSync:
		return a.validateSync()
	case PurposePublish:
		return a.validatePublish()
	default:
		return a.validateProbe()
	}
}

// validateCredential checks the credential pair: both set (a canonical UUID and a positive version)
// or both empty, and empty only for a sync of a public repository.
//
//nolint:gocritic // Value receiver, see validate.
func (a Authorization) validateCredential() error {
	if a.CredentialID == "" {
		if a.CredentialVersion != 0 {
			return fmt.Errorf("%w: credentialVersion is set without credentialId", ErrInvalid)
		}
		if a.Purpose != PurposeSync {
			return fmt.Errorf("%w: purpose %q requires a credential", ErrInvalid, a.Purpose)
		}
		return nil
	}
	if !validUUID(a.CredentialID) {
		return fmt.Errorf("%w: credentialId is not a canonical UUID", ErrInvalid)
	}
	if a.CredentialVersion <= 0 {
		return fmt.Errorf("%w: credentialVersion must be positive", ErrInvalid)
	}
	return nil
}

//nolint:gocritic // Value receiver, see validate.
func (a Authorization) validateSync() error {
	if err := forbid(a.Purpose,
		field{"verificationId", a.VerificationID != ""},
		field{"sourceRef", a.SourceRef != ""},
		field{"sourceCommit", a.SourceCommit != ""},
		field{"targetRef", a.TargetRef != ""},
		field{"expectedOldOid", a.ExpectedOldOID != ""},
		field{"service", a.Service != ""},
	); err != nil {
		return err
	}
	if err := a.requireScope(); err != nil {
		return err
	}
	if !validBranch(a.Branch) {
		return fmt.Errorf("%w: branch is not a valid branch name", ErrInvalid)
	}
	if !validOID(a.ExpectedTrackingOID) {
		return fmt.Errorf("%w: expectedTrackingOid is not a full object ID", ErrInvalid)
	}
	return nil
}

//nolint:gocritic // Value receiver, see validate.
func (a Authorization) validatePublish() error {
	if err := forbid(a.Purpose,
		field{"verificationId", a.VerificationID != ""},
		field{"branch", a.Branch != ""},
		field{"expectedTrackingOid", a.ExpectedTrackingOID != ""},
		field{"service", a.Service != ""},
	); err != nil {
		return err
	}
	if err := a.requireScope(); err != nil {
		return err
	}
	if !validRefName(a.SourceRef) {
		return fmt.Errorf("%w: sourceRef is not a full ref name", ErrInvalid)
	}
	if !validOID(a.SourceCommit) || isZeroOID(a.SourceCommit) {
		return fmt.Errorf("%w: sourceCommit is not a commit object ID", ErrInvalid)
	}
	if !validBranchRef(a.TargetRef) {
		return fmt.Errorf("%w: targetRef is not refs/heads/<branch>", ErrInvalid)
	}
	if !validOID(a.ExpectedOldOID) {
		return fmt.Errorf("%w: expectedOldOid is not a full object ID", ErrInvalid)
	}
	return nil
}

//nolint:gocritic // Value receiver, see validate.
func (a Authorization) validateProbe() error {
	if err := forbid(a.Purpose,
		field{"branch", a.Branch != ""},
		field{"expectedTrackingOid", a.ExpectedTrackingOID != ""},
		field{"sourceRef", a.SourceRef != ""},
		field{"sourceCommit", a.SourceCommit != ""},
		field{"expectedOldOid", a.ExpectedOldOID != ""},
	); err != nil {
		return err
	}
	if (a.OperationID == "") == (a.VerificationID == "") {
		return fmt.Errorf("%w: a probe carries exactly one of operationId and verificationId", ErrInvalid)
	}
	for _, id := range []struct{ name, value string }{
		{"operationId", a.OperationID},
		{"verificationId", a.VerificationID},
		{"projectId", a.ProjectID},
		{"bindingId", a.BindingID},
	} {
		if id.value != "" && !validUUID(id.value) {
			return fmt.Errorf("%w: %s is not a canonical UUID", ErrInvalid, id.name)
		}
	}
	if !validUUID(a.TenantID) {
		return fmt.Errorf("%w: tenantId is not a canonical UUID", ErrInvalid)
	}
	if a.TargetRef != "" && !validBranchRef(a.TargetRef) {
		return fmt.Errorf("%w: targetRef is not refs/heads/<branch>", ErrInvalid)
	}
	if a.Service != ServiceUploadPack && a.Service != ServiceReceivePack {
		return fmt.Errorf("%w: service is not %s or %s", ErrInvalid, ServiceUploadPack, ServiceReceivePack)
	}
	return nil
}

// requireScope checks the identifiers sync and publish both require.
//
//nolint:gocritic // Value receiver, see validate.
func (a Authorization) requireScope() error {
	for _, id := range []struct{ name, value string }{
		{"operationId", a.OperationID},
		{"tenantId", a.TenantID},
		{"projectId", a.ProjectID},
		{"bindingId", a.BindingID},
	} {
		if !validUUID(id.value) {
			return fmt.Errorf("%w: %s is not a canonical UUID", ErrInvalid, id.name)
		}
	}
	return nil
}

// forbid refuses any present field: a purpose must not carry another purpose's fields, or the payload
// could be read as either.
func forbid(purpose Purpose, fields ...field) error {
	for _, f := range fields {
		if f.present {
			return fmt.Errorf("%w: purpose %q does not carry %s", ErrInvalid, purpose, f.name)
		}
	}
	return nil
}
