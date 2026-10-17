package cloneauth

import (
	"crypto/ed25519"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSubmissionRoundTrip(t *testing.T) {
	public, private := newKey(t)
	for name, change := range map[string]func(*Submission){
		"personal":         func(*Submission) {},
		"team":             func(s *Submission) { s.Attribution = AttributionTeam; s.ExpectedStoreVersion = 7 },
		"maximum lifetime": func(s *Submission) { s.ExpiresAt = s.IssuedAt.Add(SubmissionMaxLifetime) },
	} {
		t.Run(name, func(t *testing.T) {
			want := testSubmission()
			change(&want)
			token, err := SignSubmission(private, want)
			if err != nil {
				t.Fatalf("SignSubmission: %v", err)
			}
			got, err := VerifySubmission(token, []ed25519.PublicKey{public}, testIssuedAt)
			if err != nil {
				t.Fatalf("VerifySubmission: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed the submission:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestSubmissionRefusesInvalid(t *testing.T) {
	public, private := newKey(t)
	cases := map[string]func(*Submission){
		"execution purpose":      func(s *Submission) { s.Purpose = string(PurposeSync) },
		"empty purpose":          func(s *Submission) { s.Purpose = "" },
		"missing submissionId":   func(s *Submission) { s.SubmissionID = "" },
		"missing tenantId":       func(s *Submission) { s.TenantID = "" },
		"missing credentialId":   func(s *Submission) { s.CredentialID = "" },
		"missing ownerUserId":    func(s *Submission) { s.OwnerUserID = "" },
		"uppercase credentialId": func(s *Submission) { s.CredentialID = strings.ToUpper(testCredentialID) },
		"unknown attribution":    func(s *Submission) { s.Attribution = "org" },
		"unnormalized scope":     func(s *Submission) { s.RepositoryScope = "https://GitHub.com/ora-space/example.git" },
		"missing scope":          func(s *Submission) { s.RepositoryScope = "" },
		"zero store version":     func(s *Submission) { s.ExpectedStoreVersion = 0 },
		"lifetime over maximum":  func(s *Submission) { s.ExpiresAt = s.IssuedAt.Add(SubmissionMaxLifetime + time.Nanosecond) },
		"zero lifetime":          func(s *Submission) { s.ExpiresAt = s.IssuedAt },
		"missing issuedAt":       func(s *Submission) { s.IssuedAt = time.Time{} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := testSubmission()
			change(&s)
			if _, err := SignSubmission(private, s); !errors.Is(err, ErrInvalid) {
				t.Fatalf("SignSubmission = %v, want ErrInvalid", err)
			}
			raw, err := sign(private, s)
			if err != nil {
				t.Fatalf("sign raw: %v", err)
			}
			if _, err := VerifySubmission(raw, []ed25519.PublicKey{public}, testIssuedAt); !errors.Is(err, ErrInvalid) {
				t.Fatalf("VerifySubmission = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestSubmissionClock(t *testing.T) {
	public, private := newKey(t)
	token, err := SignSubmission(private, testSubmission())
	if err != nil {
		t.Fatalf("SignSubmission: %v", err)
	}
	keys := []ed25519.PublicKey{public}
	if _, err := VerifySubmission(token, keys, testIssuedAt.Add(time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired submission: %v, want ErrExpired", err)
	}
	if _, err := VerifySubmission(token, keys, testIssuedAt.Add(-2*MaxClockSkew)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("future submission: %v, want ErrInvalid", err)
	}
	other, _ := newKey(t)
	if _, err := VerifySubmission(token, []ed25519.PublicKey{other}, testIssuedAt); !errors.Is(err, ErrSignature) {
		t.Fatalf("wrong key: %v, want ErrSignature", err)
	}
}

// TestGrantsDoNotCrossVerify signs both kinds with the same key on purpose: even a deployment that
// reused a key pair must not let an execution token act as a submission or the reverse.
func TestGrantsDoNotCrossVerify(t *testing.T) {
	public, private := newKey(t)
	keys := []ed25519.PublicKey{public}
	submission, err := SignSubmission(private, testSubmission())
	if err != nil {
		t.Fatalf("SignSubmission: %v", err)
	}
	if _, err := Verify(submission, keys, testIssuedAt); err == nil {
		t.Fatal("a submission token verified as an execution authorization")
	}
	for name, build := range map[string]func() Authorization{
		"sync": syncAuthorization, "publish": publishAuthorization, "probe": probeAuthorization,
	} {
		execution, err := Sign(private, build())
		if err != nil {
			t.Fatalf("%s: Sign: %v", name, err)
		}
		if _, err := VerifySubmission(execution, keys, testIssuedAt); err == nil {
			t.Fatalf("a %s authorization verified as a submission", name)
		}
	}
	// A payload shaped exactly like a submission but naming an execution purpose is still refused.
	forged := strings.Replace(decodePayload(t, submission), `"purpose":"submit"`, `"purpose":"sync"`, 1)
	if _, err := VerifySubmission(signRaw(private, forged), keys, testIssuedAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a submission payload with an execution purpose: %v, want ErrInvalid", err)
	}
}
