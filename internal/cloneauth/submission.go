package cloneauth

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// SubmissionPurpose is the only purpose a Submission carries. It differs from every execution
// Purpose, so neither kind of grant can be presented as the other.
const SubmissionPurpose = "submit"

// SubmissionMaxLifetime bounds a Submission. A browser uses it right after Cloud issues it, so the
// window is shorter than an execution grant's.
const SubmissionMaxLifetime = 5 * time.Minute

// Submission attributions: whose credential the browser is writing.
const (
	AttributionPersonal = "personal"
	AttributionTeam     = "team"
)

// Submission lets a browser write exactly one new version of one credential into the Credential
// Store. Cloud never sees the plaintext: it signs this grant, the browser sends grant and secret to
// the Store directly, and the Store claims SubmissionID exclusively before writing.
//
// Submissions are signed with a key pair distinct from execution Authorizations.
type Submission struct {
	// SubmissionID is the canonical UUID the Store claims once, so a grant cannot write twice.
	SubmissionID string `json:"submissionId"`
	// Purpose must equal SubmissionPurpose.
	Purpose string `json:"purpose"`
	// TenantID, CredentialID and OwnerUserID are canonical UUIDs scoping the write.
	TenantID     string `json:"tenantId"`
	CredentialID string `json:"credentialId"`
	OwnerUserID  string `json:"ownerUserId"`
	// Attribution is AttributionPersonal or AttributionTeam.
	Attribution string `json:"attribution"`
	// RepositoryScope is the repository the credential is for, in NormalizeRepository form.
	RepositoryScope string `json:"repositoryScope"`
	// ExpectedStoreVersion is the version this submission writes; the Store refuses it unless it is
	// exactly the current version plus one, which serializes concurrent submissions.
	ExpectedStoreVersion int64 `json:"expectedStoreVersion"`
	// IssuedAt and ExpiresAt bound the lifetime, in UTC.
	IssuedAt  time.Time `json:"issuedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// SignSubmission validates s and returns its token, refusing the same things VerifySubmission would
// (except the clock). The instants are converted to UTC first.
//
//nolint:gocritic // Value parameter on purpose: the snapshot validated is the snapshot signed.
func SignSubmission(key ed25519.PrivateKey, s Submission) (string, error) {
	s.IssuedAt = s.IssuedAt.UTC()
	s.ExpiresAt = s.ExpiresAt.UTC()
	if err := s.validate(); err != nil {
		return "", err
	}
	return sign(key, s)
}

// VerifySubmission authenticates token under any of keys and returns the Submission it carries, with
// the same clock rules as Verify. An execution token is refused here (ErrMalformed).
func VerifySubmission(token string, keys []ed25519.PublicKey, now time.Time) (Submission, error) {
	var s Submission
	if err := open(token, keys, &s); err != nil {
		return Submission{}, err
	}
	if err := s.validate(); err != nil {
		return Submission{}, err
	}
	if err := checkWindow(s.IssuedAt, s.ExpiresAt, now); err != nil {
		return Submission{}, err
	}
	return s, nil
}

//nolint:gocritic // Value receiver on purpose: validation must not be able to modify the snapshot.
func (s Submission) validate() error {
	if s.Purpose != SubmissionPurpose {
		return fmt.Errorf("%w: purpose is not %q", ErrInvalid, SubmissionPurpose)
	}
	for _, id := range []struct{ name, value string }{
		{"submissionId", s.SubmissionID},
		{"tenantId", s.TenantID},
		{"credentialId", s.CredentialID},
		{"ownerUserId", s.OwnerUserID},
	} {
		if !validUUID(id.value) {
			return fmt.Errorf("%w: %s is not a canonical UUID", ErrInvalid, id.name)
		}
	}
	if s.Attribution != AttributionPersonal && s.Attribution != AttributionTeam {
		return fmt.Errorf("%w: attribution is not %s or %s", ErrInvalid, AttributionPersonal, AttributionTeam)
	}
	if normalized, err := NormalizeRepository(s.RepositoryScope); err != nil || normalized != s.RepositoryScope {
		return fmt.Errorf("%w: repositoryScope is not a normalized https URL", ErrInvalid)
	}
	if s.ExpectedStoreVersion < 1 {
		return fmt.Errorf("%w: expectedStoreVersion must be at least 1", ErrInvalid)
	}
	return checkLifetime(s.IssuedAt, s.ExpiresAt, SubmissionMaxLifetime)
}
