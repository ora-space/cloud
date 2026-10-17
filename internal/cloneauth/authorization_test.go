package cloneauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAuthorizationRoundTrip(t *testing.T) {
	public, private := newKey(t)
	cases := map[string]func() Authorization{
		"sync with credential": syncAuthorization,
		"sync anonymous": func() Authorization {
			a := syncAuthorization()
			a.CredentialID, a.CredentialVersion = "", 0
			a.ExpectedTrackingOID = testOID64
			a.Branch = "release/v1.2"
			return a
		},
		"publish":                            publishAuthorization,
		"publish over existing ref":          func() Authorization { a := publishAuthorization(); a.ExpectedOldOID = testOID; return a },
		"probe receive-pack by verification": probeAuthorization,
		"probe upload-pack by operation with scope": func() Authorization {
			a := probeAuthorization()
			a.VerificationID, a.OperationID = "", testOperationID
			a.ProjectID, a.BindingID = testProjectID, testBindingID
			a.Service = ServiceUploadPack
			a.TargetRef = "refs/heads/main"
			return a
		},
		"maximum lifetime": func() Authorization {
			a := syncAuthorization()
			a.ExpiresAt = a.IssuedAt.Add(MaxLifetime)
			return a
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			want := build()
			token, err := Sign(private, want)
			if err != nil {
				t.Fatalf("Sign refused a valid authorization: %v", err)
			}
			got, err := Verify(token, []ed25519.PublicKey{public}, want.IssuedAt.Add(time.Second))
			if err != nil {
				t.Fatalf("Verify refused a token Sign produced: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed the authorization:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestSignConvertsToUTC(t *testing.T) {
	public, private := newKey(t)
	a := syncAuthorization()
	zone := time.FixedZone("UTC+8", 8*3600)
	a.IssuedAt, a.ExpiresAt = a.IssuedAt.In(zone), a.ExpiresAt.In(zone)
	token, err := Sign(private, a)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := Verify(token, []ed25519.PublicKey{public}, testIssuedAt)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.IssuedAt.Location() != time.UTC || !got.IssuedAt.Equal(testIssuedAt) {
		t.Fatalf("issuedAt = %v, want %v in UTC", got.IssuedAt, testIssuedAt)
	}
}

// invalidAuthorizations is the field-rule matrix: every case must be refused by Sign, and by Verify
// when signed without validation.
func invalidAuthorizations() map[string]Authorization {
	mutate := func(build func() Authorization, change func(*Authorization)) Authorization {
		a := build()
		change(&a)
		return a
	}
	cases := map[string]Authorization{
		// Shared fields.
		"unknown purpose":             mutate(syncAuthorization, func(a *Authorization) { a.Purpose = "fetch" }),
		"old purpose clone":           mutate(syncAuthorization, func(a *Authorization) { a.Purpose = "clone" }),
		"old purpose mirror_prepare":  mutate(syncAuthorization, func(a *Authorization) { a.Purpose = "mirror_prepare" }),
		"old purpose push":            mutate(publishAuthorization, func(a *Authorization) { a.Purpose = "push" }),
		"empty purpose":               mutate(syncAuthorization, func(a *Authorization) { a.Purpose = "" }),
		"missing authorizationId":     mutate(syncAuthorization, func(a *Authorization) { a.AuthorizationID = "" }),
		"uppercase authorizationId":   mutate(syncAuthorization, func(a *Authorization) { a.AuthorizationID = strings.ToUpper(testAuthorizationID) }),
		"braced authorizationId":      mutate(syncAuthorization, func(a *Authorization) { a.AuthorizationID = "{" + testAuthorizationID + "}" }),
		"nil authorizationId":         mutate(syncAuthorization, func(a *Authorization) { a.AuthorizationID = "00000000-0000-0000-0000-000000000000" }),
		"wrong executor identity":     mutate(syncAuthorization, func(a *Authorization) { a.ExecutorIdentity = "spiffe://ora.local/other" }),
		"missing executor identity":   mutate(syncAuthorization, func(a *Authorization) { a.ExecutorIdentity = "" }),
		"missing issuedAt":            mutate(syncAuthorization, func(a *Authorization) { a.IssuedAt = time.Time{} }),
		"missing expiresAt":           mutate(syncAuthorization, func(a *Authorization) { a.ExpiresAt = time.Time{} }),
		"zero lifetime":               mutate(syncAuthorization, func(a *Authorization) { a.ExpiresAt = a.IssuedAt }),
		"negative lifetime":           mutate(syncAuthorization, func(a *Authorization) { a.ExpiresAt = a.IssuedAt.Add(-time.Second) }),
		"lifetime over maximum":       mutate(syncAuthorization, func(a *Authorization) { a.ExpiresAt = a.IssuedAt.Add(MaxLifetime + time.Nanosecond) }),
		"missing repository":          mutate(syncAuthorization, func(a *Authorization) { a.Repository = "" }),
		"invalid repository":          mutate(syncAuthorization, func(a *Authorization) { a.Repository = "http://example.com/ora-space/example" }),
		"padded repository":           mutate(syncAuthorization, func(a *Authorization) { a.Repository = " " + testRepository }),
		"http repository":             mutate(syncAuthorization, func(a *Authorization) { a.Repository = "http://github.com/ora-space/example" }),
		"credential version alone":    mutate(syncAuthorization, func(a *Authorization) { a.CredentialID = "" }),
		"credential id alone":         mutate(syncAuthorization, func(a *Authorization) { a.CredentialVersion = 0 }),
		"negative credential version": mutate(syncAuthorization, func(a *Authorization) { a.CredentialVersion = -1 }),
		"malformed credential id":     mutate(syncAuthorization, func(a *Authorization) { a.CredentialID = "cred-1" }),
		// Sync.
		"sync missing operationId":        mutate(syncAuthorization, func(a *Authorization) { a.OperationID = "" }),
		"sync missing tenantId":           mutate(syncAuthorization, func(a *Authorization) { a.TenantID = "" }),
		"sync missing projectId":          mutate(syncAuthorization, func(a *Authorization) { a.ProjectID = "" }),
		"sync missing bindingId":          mutate(syncAuthorization, func(a *Authorization) { a.BindingID = "" }),
		"sync missing branch":             mutate(syncAuthorization, func(a *Authorization) { a.Branch = "" }),
		"sync branch with parent segment": mutate(syncAuthorization, func(a *Authorization) { a.Branch = "main/../x" }),
		"sync branch HEAD":                mutate(syncAuthorization, func(a *Authorization) { a.Branch = "HEAD" }),
		"sync branch option":              mutate(syncAuthorization, func(a *Authorization) { a.Branch = "-x" }),
		"sync branch with space":          mutate(syncAuthorization, func(a *Authorization) { a.Branch = "a b" }),
		"sync branch with dot-dot":        mutate(syncAuthorization, func(a *Authorization) { a.Branch = "a..b" }),
		"sync branch ending .lock":        mutate(syncAuthorization, func(a *Authorization) { a.Branch = "a.lock" }),
		"sync branch with reflog syntax":  mutate(syncAuthorization, func(a *Authorization) { a.Branch = "a@{1}" }),
		"sync branch with colon":          mutate(syncAuthorization, func(a *Authorization) { a.Branch = "a:b" }),
		"sync missing tracking oid":       mutate(syncAuthorization, func(a *Authorization) { a.ExpectedTrackingOID = "" }),
		"sync short tracking oid":         mutate(syncAuthorization, func(a *Authorization) { a.ExpectedTrackingOID = testOID[:12] }),
		"sync uppercase tracking oid":     mutate(syncAuthorization, func(a *Authorization) { a.ExpectedTrackingOID = strings.ToUpper(testOID) }),
		"sync with verificationId":        mutate(syncAuthorization, func(a *Authorization) { a.VerificationID = testVerificationID }),
		"sync with sourceRef":             mutate(syncAuthorization, func(a *Authorization) { a.SourceRef = "refs/heads/x" }),
		"sync with sourceCommit":          mutate(syncAuthorization, func(a *Authorization) { a.SourceCommit = testOID }),
		"sync with targetRef":             mutate(syncAuthorization, func(a *Authorization) { a.TargetRef = "refs/heads/main" }),
		"sync with expectedOldOid":        mutate(syncAuthorization, func(a *Authorization) { a.ExpectedOldOID = ZeroOID40 }),
		"sync with service":               mutate(syncAuthorization, func(a *Authorization) { a.Service = ServiceUploadPack }),
		// Publish.
		"publish anonymous":                mutate(publishAuthorization, func(a *Authorization) { a.CredentialID, a.CredentialVersion = "", 0 }),
		"publish missing operationId":      mutate(publishAuthorization, func(a *Authorization) { a.OperationID = "" }),
		"publish missing tenantId":         mutate(publishAuthorization, func(a *Authorization) { a.TenantID = "" }),
		"publish missing projectId":        mutate(publishAuthorization, func(a *Authorization) { a.ProjectID = "" }),
		"publish missing bindingId":        mutate(publishAuthorization, func(a *Authorization) { a.BindingID = "" }),
		"publish missing sourceRef":        mutate(publishAuthorization, func(a *Authorization) { a.SourceRef = "" }),
		"publish short sourceRef":          mutate(publishAuthorization, func(a *Authorization) { a.SourceRef = "main" }),
		"publish missing sourceCommit":     mutate(publishAuthorization, func(a *Authorization) { a.SourceCommit = "" }),
		"publish zero sourceCommit":        mutate(publishAuthorization, func(a *Authorization) { a.SourceCommit = ZeroOID40 }),
		"publish missing targetRef":        mutate(publishAuthorization, func(a *Authorization) { a.TargetRef = "" }),
		"publish tag targetRef":            mutate(publishAuthorization, func(a *Authorization) { a.TargetRef = "refs/tags/v1" }),
		"publish short targetRef":          mutate(publishAuthorization, func(a *Authorization) { a.TargetRef = "main" }),
		"publish missing expectedOldOid":   mutate(publishAuthorization, func(a *Authorization) { a.ExpectedOldOID = "" }),
		"publish with verificationId":      mutate(publishAuthorization, func(a *Authorization) { a.VerificationID = testVerificationID }),
		"publish with branch":              mutate(publishAuthorization, func(a *Authorization) { a.Branch = "main" }),
		"publish with expectedTrackingOid": mutate(publishAuthorization, func(a *Authorization) { a.ExpectedTrackingOID = ZeroOID40 }),
		"publish with service":             mutate(publishAuthorization, func(a *Authorization) { a.Service = ServiceReceivePack }),
		// Probe.
		"probe anonymous":                mutate(probeAuthorization, func(a *Authorization) { a.CredentialID, a.CredentialVersion = "", 0 }),
		"probe with both ids":            mutate(probeAuthorization, func(a *Authorization) { a.OperationID = testOperationID }),
		"probe with neither id":          mutate(probeAuthorization, func(a *Authorization) { a.VerificationID = "" }),
		"probe malformed verificationId": mutate(probeAuthorization, func(a *Authorization) { a.VerificationID = "v-1" }),
		"probe malformed operationId":    mutate(probeAuthorization, func(a *Authorization) { a.VerificationID, a.OperationID = "", "op-1" }),
		"probe missing tenantId":         mutate(probeAuthorization, func(a *Authorization) { a.TenantID = "" }),
		"probe malformed projectId":      mutate(probeAuthorization, func(a *Authorization) { a.ProjectID = "p" }),
		"probe malformed bindingId":      mutate(probeAuthorization, func(a *Authorization) { a.BindingID = "b" }),
		"probe missing service":          mutate(probeAuthorization, func(a *Authorization) { a.Service = "" }),
		"probe unknown service":          mutate(probeAuthorization, func(a *Authorization) { a.Service = "git-receive-pack" }),
		"probe tag targetRef":            mutate(probeAuthorization, func(a *Authorization) { a.TargetRef = "refs/tags/v1" }),
		"probe with branch":              mutate(probeAuthorization, func(a *Authorization) { a.Branch = "main" }),
		"probe with expectedTrackingOid": mutate(probeAuthorization, func(a *Authorization) { a.ExpectedTrackingOID = ZeroOID40 }),
		"probe with sourceRef":           mutate(probeAuthorization, func(a *Authorization) { a.SourceRef = "refs/heads/x" }),
		"probe with sourceCommit":        mutate(probeAuthorization, func(a *Authorization) { a.SourceCommit = testOID }),
		"probe with expectedOldOid":      mutate(probeAuthorization, func(a *Authorization) { a.ExpectedOldOID = ZeroOID40 }),
	}
	return cases
}

func TestSignRefusesInvalidAuthorizations(t *testing.T) {
	_, private := newKey(t)
	for name, a := range invalidAuthorizations() {
		t.Run(name, func(t *testing.T) {
			token, err := Sign(private, a)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("Sign = %q, %v; want ErrInvalid", token, err)
			}
		})
	}
}

// TestVerifyRefusesInvalidAuthorizations proves Verify enforces the field rules itself, so a payload
// signed by a buggy or bypassed signer never becomes a verified Authorization.
func TestVerifyRefusesInvalidAuthorizations(t *testing.T) {
	public, private := newKey(t)
	for name, a := range invalidAuthorizations() {
		t.Run(name, func(t *testing.T) {
			token, err := sign(private, a)
			if err != nil {
				t.Fatalf("sign raw: %v", err)
			}
			if _, err := Verify(token, []ed25519.PublicKey{public}, testIssuedAt); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Verify = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestVerifyFailures(t *testing.T) {
	public, private := newKey(t)
	otherPublic, otherPrivate := newKey(t)
	token, err := Sign(private, syncAuthorization())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	payloadPart, signaturePart, _ := strings.Cut(token, ".")
	tamperedPayload := strings.Replace(decodePayload(t, token), `"branch":"main"`, `"branch":"evil"`, 1)
	signature, _ := base64.RawURLEncoding.DecodeString(signaturePart)
	signature[0] ^= 0xff
	expiresAt := testIssuedAt.Add(2 * time.Minute)
	wrongKeyToken, _ := Sign(otherPrivate, syncAuthorization())

	cases := []struct {
		name  string
		token string
		keys  []ed25519.PublicKey
		now   time.Time
		want  error
	}{
		{"wrong key", token, []ed25519.PublicKey{otherPublic}, testIssuedAt, ErrSignature},
		{"signed by a key outside the set", wrongKeyToken, []ed25519.PublicKey{public}, testIssuedAt, ErrSignature},
		{"tampered payload", base64.RawURLEncoding.EncodeToString([]byte(tamperedPayload)) + "." + signaturePart, []ed25519.PublicKey{public}, testIssuedAt, ErrSignature},
		{"tampered signature", payloadPart + "." + base64.RawURLEncoding.EncodeToString(signature), []ed25519.PublicKey{public}, testIssuedAt, ErrSignature},
		{"expired exactly at expiresAt", token, []ed25519.PublicKey{public}, expiresAt, ErrExpired},
		{"expired after expiresAt", token, []ed25519.PublicKey{public}, expiresAt.Add(time.Hour), ErrExpired},
		{"issuedAt beyond skew", token, []ed25519.PublicKey{public}, testIssuedAt.Add(-MaxClockSkew - time.Nanosecond), ErrInvalid},
		{"no keys", token, nil, testIssuedAt, ErrKey},
		{"short key", token, []ed25519.PublicKey{public[:10]}, testIssuedAt, ErrKey},
		{"empty token", "", []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"no separator", payloadPart, []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"two separators", token + ".x", []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"empty payload", "." + signaturePart, []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"payload not base64url", "!!!." + signaturePart, []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"padded base64", payloadPart + "=." + signaturePart, []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"short signature", payloadPart + "." + signaturePart[:20], []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
		{"oversized token", strings.Repeat("a", maxTokenLength) + "." + signaturePart, []ed25519.PublicKey{public}, testIssuedAt, ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Verify(tc.token, tc.keys, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyClockWindow(t *testing.T) {
	public, private := newKey(t)
	token, err := Sign(private, syncAuthorization())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	for name, now := range map[string]time.Time{
		"at issuance":            testIssuedAt,
		"issuedAt within skew":   testIssuedAt.Add(-MaxClockSkew),
		"just before expiration": testIssuedAt.Add(2*time.Minute - time.Nanosecond),
	} {
		if _, err := Verify(token, []ed25519.PublicKey{public}, now); err != nil {
			t.Fatalf("%s: Verify = %v, want success", name, err)
		}
	}
}

func TestVerifyAcceptsAnyKeyInTheSet(t *testing.T) {
	outgoing, _ := newKey(t)
	incoming, private := newKey(t)
	token, err := Sign(private, publishAuthorization())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := Verify(token, []ed25519.PublicKey{outgoing, incoming}, testIssuedAt); err != nil {
		t.Fatalf("a token signed by the second key of the set was refused: %v", err)
	}
}

func TestVerifyDecodesStrictly(t *testing.T) {
	public, private := newKey(t)
	token, err := Sign(private, syncAuthorization())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	valid := decodePayload(t, token)
	cases := map[string]string{
		"unknown field":  strings.Replace(valid, `{`, `{"nodeId":"n",`, 1),
		"trailing value": valid + `{}`,
		"wrong type":     strings.Replace(valid, `"credentialVersion":3`, `"credentialVersion":"3"`, 1),
		"not an object":  `[]`,
		"not json":       `authorization`,
		"empty payload":  ``,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(signRaw(private, payload), []ed25519.PublicKey{public}, testIssuedAt); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Verify = %v, want ErrMalformed", err)
			}
		})
	}
	t.Run("non-UTC instant", func(t *testing.T) {
		payload := strings.Replace(valid, `"issuedAt":"2026-10-17T09:00:00Z"`, `"issuedAt":"2026-10-17T17:00:00+08:00"`, 1)
		if payload == valid {
			t.Fatal("test payload was not rewritten")
		}
		if _, err := Verify(signRaw(private, payload), []ed25519.PublicKey{public}, testIssuedAt); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Verify = %v, want ErrInvalid", err)
		}
	})
}

func TestSignRefusesUnusableKey(t *testing.T) {
	for name, key := range map[string]ed25519.PrivateKey{"nil": nil, "short": make(ed25519.PrivateKey, 10)} {
		if _, err := Sign(key, syncAuthorization()); !errors.Is(err, ErrKey) {
			t.Fatalf("%s key: Sign = %v, want ErrKey", name, err)
		}
		if _, err := SignSubmission(key, testSubmission()); !errors.Is(err, ErrKey) {
			t.Fatalf("%s key: SignSubmission = %v, want ErrKey", name, err)
		}
	}
}
