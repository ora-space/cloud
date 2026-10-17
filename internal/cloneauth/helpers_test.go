package cloneauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const (
	testAuthorizationID = "0b6f7a0e-1f2d-4c3b-8a9e-5d4c3b2a1f00"
	testOperationID     = "1c7a8b1f-2e3d-4d4c-9b0f-6e5d4c3b2a11"
	testVerificationID  = "2d8b9c20-3f4e-4e5d-8c1a-7f6e5d4c3b22"
	testTenantID        = "3e9cad31-4a5f-4f6e-9d2b-8a7f6e5d4c33"
	testProjectID       = "4fadbe42-5b6a-4a7f-8e3c-9b8a7f6e5d44"
	testBindingID       = "5abecf53-6c7b-4b8a-9f4d-ac9b8a7f6e55"
	testCredentialID    = "6bcfd064-7d8c-4c9b-8a5e-bdac9b8a7f66"
	testRepository      = "https://github.com/ora-space/example"
	testOID             = "0123456789abcdef0123456789abcdef01234567"
	testOID64           = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// testIssuedAt is fixed so every case is deterministic; tests pass their own clock to Verify.
var testIssuedAt = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return public, private
}

func syncAuthorization() Authorization {
	return Authorization{
		AuthorizationID:     testAuthorizationID,
		Purpose:             PurposeSync,
		ExecutorIdentity:    ExecutorIdentity,
		IssuedAt:            testIssuedAt,
		ExpiresAt:           testIssuedAt.Add(2 * time.Minute),
		OperationID:         testOperationID,
		TenantID:            testTenantID,
		ProjectID:           testProjectID,
		BindingID:           testBindingID,
		Repository:          testRepository,
		Branch:              "main",
		ExpectedTrackingOID: ZeroOID40,
		CredentialID:        testCredentialID,
		CredentialVersion:   3,
	}
}

func publishAuthorization() Authorization {
	return Authorization{
		AuthorizationID:   testAuthorizationID,
		Purpose:           PurposePublish,
		ExecutorIdentity:  ExecutorIdentity,
		IssuedAt:          testIssuedAt,
		ExpiresAt:         testIssuedAt.Add(2 * time.Minute),
		OperationID:       testOperationID,
		TenantID:          testTenantID,
		ProjectID:         testProjectID,
		BindingID:         testBindingID,
		Repository:        testRepository,
		SourceRef:         "refs/heads/workspaces/" + testProjectID + "/feature",
		SourceCommit:      testOID,
		TargetRef:         "refs/heads/main",
		ExpectedOldOID:    ZeroOID40,
		CredentialID:      testCredentialID,
		CredentialVersion: 1,
	}
}

func probeAuthorization() Authorization {
	return Authorization{
		AuthorizationID:   testAuthorizationID,
		Purpose:           PurposeProbe,
		ExecutorIdentity:  ExecutorIdentity,
		IssuedAt:          testIssuedAt,
		ExpiresAt:         testIssuedAt.Add(2 * time.Minute),
		VerificationID:    testVerificationID,
		TenantID:          testTenantID,
		Repository:        testRepository,
		Service:           ServiceReceivePack,
		CredentialID:      testCredentialID,
		CredentialVersion: 2,
	}
}

func testSubmission() Submission {
	return Submission{
		SubmissionID:         testAuthorizationID,
		Purpose:              SubmissionPurpose,
		TenantID:             testTenantID,
		CredentialID:         testCredentialID,
		OwnerUserID:          testOperationID,
		Attribution:          AttributionPersonal,
		RepositoryScope:      testRepository,
		ExpectedStoreVersion: 1,
		IssuedAt:             testIssuedAt,
		ExpiresAt:            testIssuedAt.Add(time.Minute),
	}
}

// signRaw signs arbitrary payload bytes, bypassing Sign's validation, so tests can prove Verify
// enforces the rules on its own.
func signRaw(key ed25519.PrivateKey, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(payload)))
}

// decodePayload returns a token's payload JSON.
func decodePayload(t *testing.T, token string) string {
	t.Helper()
	encoded, _, _ := strings.Cut(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return string(payload)
}
