package cloneauth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testIssued is the fixed instant every test builds bindings from, so lifetime assertions depend on
// the values under test rather than on how long the test took to run.
func testIssued() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }

// testBinding returns one complete, valid binding. Tests mutate a single field at a time so each
// field's contribution is visible in the failure it causes.
func testBinding() Binding {
	return Binding{
		AuthorizationID:   "11111111-1111-4111-8111-111111111111",
		Operation:         OperationClone,
		TenantID:          "tenant-1",
		OwnerUserID:       "user-1",
		ProjectID:         "project-1",
		WorkspaceID:       "workspace-1",
		ExecutionID:       "execution-1",
		CredentialID:      "22222222-2222-4222-8222-222222222222",
		CredentialVersion: 1,
		Repository:        "https://github.com/ora-space/private",
		Capabilities:      []string{string(CapabilityClone)},
		NodeID:            "node-1",
		NodeInstanceID:    "node-instance-1",
		NodeIncarnationID: "incarnation-1",
		RuntimeGeneration: 3,
		ControlEpoch:      7,
		IssuedAt:          testIssued(),
		ExpiresAt:         testIssued().Add(time.Minute),
	}
}

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	return public, private
}

// verifyOne verifies against a single-key set, which is the deployment shape most of these tests are
// about. The multi-key cases call Verify directly.
func verifyOne(public ed25519.PublicKey, token string, now time.Time) (Binding, error) {
	return Verify([]ed25519.PublicKey{public}, token, now)
}

// TestIssueAndVerifyRoundTrip proves a signed binding survives the wire unchanged, which is what lets
// the Store check the binding rather than only the signature.
func TestIssueAndVerifyRoundTrip(t *testing.T) {
	public, private := testKeys(t)
	want := testBinding()
	token, err := Issue(private, want)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if strings.Count(token, ".") != 1 {
		t.Fatalf("token has %d separators, want exactly 1", strings.Count(token, "."))
	}
	got, err := verifyOne(public, token, testIssued())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Verify returned\n%+v\nwant\n%+v", got, want)
	}
}

// TestIssueRejectsIncompleteBindings proves every field the decisions require is load-bearing: a
// caller cannot mint an authorization that leaves one of them unset, so the Store never has to decide
// what an empty tenant or a zero generation means.
func TestIssueRejectsIncompleteBindings(t *testing.T) {
	_, private := testKeys(t)
	cases := []struct {
		name   string
		mutate func(*Binding)
		want   error
	}{
		{"no authorization id", func(b *Binding) { b.AuthorizationID = "" }, ErrIncomplete},
		{"no operation", func(b *Binding) { b.Operation = "" }, ErrIncomplete},
		{"wrong operation", func(b *Binding) { b.Operation = "push" }, ErrIncomplete},
		{"a clone carrying a ref", func(b *Binding) { b.Ref = "refs/heads/main" }, ErrIncomplete},
		{"a mirror preparation without a ref", func(b *Binding) { b.Operation = OperationMirrorPrepare }, ErrIncomplete},
		{"a mirror preparation with an unqualified ref", func(b *Binding) {
			b.Operation = OperationMirrorPrepare
			b.Ref = "main"
		}, ErrIncomplete},
		{"a mirror preparation naming a tag", func(b *Binding) {
			b.Operation = OperationMirrorPrepare
			b.Ref = "refs/tags/v1.0.0"
		}, ErrIncomplete},
		{"no tenant", func(b *Binding) { b.TenantID = "" }, ErrIncomplete},
		{"blank owner", func(b *Binding) { b.OwnerUserID = "  " }, ErrIncomplete},
		{"no project", func(b *Binding) { b.ProjectID = "" }, ErrIncomplete},
		{"no workspace", func(b *Binding) { b.WorkspaceID = "" }, ErrIncomplete},
		{"no execution", func(b *Binding) { b.ExecutionID = "" }, ErrIncomplete},
		{"no credential", func(b *Binding) { b.CredentialID = "" }, ErrIncomplete},
		{"no repository", func(b *Binding) { b.Repository = "" }, ErrIncomplete},
		{"no capabilities", func(b *Binding) { b.Capabilities = nil }, ErrIncomplete},
		{"empty capability set", func(b *Binding) { b.Capabilities = []string{} }, ErrIncomplete},
		{"unknown capability", func(b *Binding) { b.Capabilities = []string{"fetch"} }, ErrIncomplete},
		{"repeated capability", func(b *Binding) { b.Capabilities = []string{"clone", "clone"} }, ErrIncomplete},
		{"a capability outside the V1 vocabulary", func(b *Binding) { b.Capabilities = []string{"clone", "push"} }, ErrIncomplete},
		{"no node", func(b *Binding) { b.NodeID = "" }, ErrIncomplete},
		{"no node instance", func(b *Binding) { b.NodeInstanceID = "" }, ErrIncomplete},
		{"no node incarnation", func(b *Binding) { b.NodeIncarnationID = "" }, ErrIncomplete},
		{"zero version", func(b *Binding) { b.CredentialVersion = 0 }, ErrIncomplete},
		{"zero generation", func(b *Binding) { b.RuntimeGeneration = 0 }, ErrIncomplete},
		{"negative epoch", func(b *Binding) { b.ControlEpoch = -1 }, ErrIncomplete},
		{"no issued at", func(b *Binding) { b.IssuedAt = time.Time{} }, ErrIncomplete},
		{"no expiry", func(b *Binding) { b.ExpiresAt = time.Time{} }, ErrIncomplete},
		{"expiry not after issuance", func(b *Binding) { b.ExpiresAt = b.IssuedAt }, ErrIncomplete},
		{"lifetime above the maximum", func(b *Binding) { b.ExpiresAt = b.IssuedAt.Add(MaxLifetime + time.Second) }, ErrLifetime},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			binding := testBinding()
			test.mutate(&binding)
			if _, err := Issue(private, binding); !errors.Is(err, test.want) {
				t.Fatalf("Issue error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestIssueAcceptsAZeroControlEpoch pins the one fencing value that is legitimately absent at
// issuance. A Workspace clone is dispatched as soon as its Node is ready, which is before any holder
// has claimed the runtime, so the epoch Cloud can honestly bind is the zero its control row carries.
// Refusing it would make every ordinary credentialed clone unissuable; accepting a negative one would
// make the field meaningless, which the table above covers.
func TestIssueAcceptsAZeroControlEpoch(t *testing.T) {
	public, private := testKeys(t)
	binding := testBinding()
	binding.ControlEpoch = 0
	token, err := Issue(private, binding)
	if err != nil {
		t.Fatalf("Issue with a zero control epoch: %v", err)
	}
	verified, err := verifyOne(public, token, binding.IssuedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("Verify of a zero-epoch authorization: %v", err)
	}
	if verified.ControlEpoch != 0 {
		t.Fatalf("verified control epoch = %d, want 0", verified.ControlEpoch)
	}
}

// TestIssueAcceptsTheMaximumLifetime pins the boundary itself, so the ceiling cannot silently tighten
// or disappear.
func TestIssueAcceptsTheMaximumLifetime(t *testing.T) {
	_, private := testKeys(t)
	binding := testBinding()
	binding.ExpiresAt = binding.IssuedAt.Add(MaxLifetime)
	if _, err := Issue(private, binding); err != nil {
		t.Fatalf("Issue at the maximum lifetime: %v", err)
	}
}

// TestVerifyRejectsMalformedTokens proves the token shape is checked before anything else, so no
// later check has to reason about a partially decoded input.
func TestVerifyRejectsMalformedTokens(t *testing.T) {
	public, _ := testKeys(t)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"operation":"clone"}`))
	signature := base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	cases := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"no separator", "abcdef"},
		{"two separators", payload + "." + signature + ".extra"},
		{"empty payload", "." + signature},
		{"empty signature", payload + "."},
		{"payload is not base64", "!!!." + signature},
		{"signature is not base64", payload + ".!!!"},
		{"signature is too short", payload + "." + base64.RawURLEncoding.EncodeToString([]byte("short"))},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := verifyOne(public, test.token, testIssued()); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Verify error = %v, want %v", err, ErrMalformed)
			}
		})
	}
}

// TestVerifyRejectsATamperedPayload proves the signature covers the bytes that are actually read. A
// verifier that re-encoded the payload before checking would accept a token whose transmitted form
// differs from the form that was signed.
func TestVerifyRejectsATamperedPayload(t *testing.T) {
	public, private := testKeys(t)
	token, err := Issue(private, testBinding())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	encodedPayload, encodedSignature, _ := strings.Cut(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	tampered := strings.Replace(string(payload), `"tenantId":"tenant-1"`, `"tenantId":"tenant-2"`, 1)
	if tampered == string(payload) {
		t.Fatalf("test did not change the payload: %s", payload)
	}
	forged := base64.RawURLEncoding.EncodeToString([]byte(tampered)) + "." + encodedSignature
	if _, err := verifyOne(public, forged, testIssued()); !errors.Is(err, ErrSignature) {
		t.Fatalf("Verify(tampered) error = %v, want %v", err, ErrSignature)
	}
}

// TestVerifyRejectsAnotherIssuersKey proves verification is bound to the deployment's key: a token
// signed by anyone else is refused, which is what stops a holder of the resolve certificate from
// minting its own authorizations.
func TestVerifyRejectsAnotherIssuersKey(t *testing.T) {
	public, _ := testKeys(t)
	_, otherPrivate := testKeys(t)
	token, err := Issue(otherPrivate, testBinding())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := verifyOne(public, token, testIssued()); !errors.Is(err, ErrSignature) {
		t.Fatalf("Verify error = %v, want %v", err, ErrSignature)
	}
}

// TestVerifyUsesTheCallersClock proves expiry is decided by the verifier's own clock rather than the
// payload's self-reported instants. The Store passes its own clock, so clock skew resolves in the
// Store's favor.
func TestVerifyUsesTheCallersClock(t *testing.T) {
	public, private := testKeys(t)
	binding := testBinding()
	binding.ExpiresAt = binding.IssuedAt.Add(time.Minute)
	token, err := Issue(private, binding)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := verifyOne(public, token, binding.ExpiresAt.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("Verify just before expiry: %v", err)
	}
	// The expiry instant itself is already expired: an authorization is valid strictly before it.
	for _, at := range []time.Time{binding.ExpiresAt, binding.ExpiresAt.Add(time.Hour)} {
		if _, err := verifyOne(public, token, at); !errors.Is(err, ErrExpired) {
			t.Fatalf("Verify at %s error = %v, want %v", at, err, ErrExpired)
		}
	}
}

// TestVerifyRejectsUnusableKeys proves a caller that forgot to configure a key cannot accidentally
// verify against a zero-length one, and that an empty key set is a refusal rather than a vacuous
// success.
func TestVerifyRejectsUnusableKeys(t *testing.T) {
	_, private := testKeys(t)
	token, err := Issue(private, testBinding())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for _, keys := range [][]ed25519.PublicKey{nil, {}, {nil}, {make(ed25519.PublicKey, 8)}} {
		if _, err := Verify(keys, token, testIssued()); !errors.Is(err, ErrKey) {
			t.Fatalf("Verify(%d keys) error = %v, want %v", len(keys), err, ErrKey)
		}
	}
	if _, err := Issue(nil, testBinding()); !errors.Is(err, ErrKey) {
		t.Fatalf("Issue(nil key) error = %v, want %v", err, ErrKey)
	}
}

// TestVerifyAcceptsEveryKeyInTheSet proves the overlap window a key rotation depends on: while both
// the outgoing and the incoming key are mounted, an authorization signed by either one is accepted.
func TestVerifyAcceptsEveryKeyInTheSet(t *testing.T) {
	outgoingPublic, outgoingPrivate := testKeys(t)
	incomingPublic, incomingPrivate := testKeys(t)
	keys := []ed25519.PublicKey{outgoingPublic, incomingPublic}
	for name, private := range map[string]ed25519.PrivateKey{"outgoing": outgoingPrivate, "incoming": incomingPrivate} {
		t.Run(name, func(t *testing.T) {
			token, err := Issue(private, testBinding())
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			if _, err := Verify(keys, token, testIssued()); err != nil {
				t.Fatalf("Verify during the overlap: %v", err)
			}
		})
	}
}

// TestVerifyRefusesAKeyWithdrawnFromTheSet proves the other half of rotation and the whole of
// revocation: once the outgoing key is removed from the file, everything it signed is refused, and
// the surviving key still works. MaxLifetime is what makes this sufficient — no authorization signed
// by the withdrawn key can still be live by the time a deployment removes it.
func TestVerifyRefusesAKeyWithdrawnFromTheSet(t *testing.T) {
	_, outgoingPrivate := testKeys(t)
	incomingPublic, incomingPrivate := testKeys(t)
	withdrawn, err := Issue(outgoingPrivate, testBinding())
	if err != nil {
		t.Fatalf("Issue with the outgoing key: %v", err)
	}
	kept, err := Issue(incomingPrivate, testBinding())
	if err != nil {
		t.Fatalf("Issue with the incoming key: %v", err)
	}
	remaining := []ed25519.PublicKey{incomingPublic}
	if _, err := Verify(remaining, withdrawn, testIssued()); !errors.Is(err, ErrSignature) {
		t.Fatalf("Verify(withdrawn key) error = %v, want %v", err, ErrSignature)
	}
	if _, err := Verify(remaining, kept, testIssued()); err != nil {
		t.Fatalf("Verify(kept key): %v", err)
	}
}

// TestLoadVerificationKeysReadsEveryMountedKey proves the file, not the code, is what decides how
// many keys are in force, so an operator rotates by editing a mount rather than by changing a
// deployment's configuration shape.
func TestLoadVerificationKeysReadsEveryMountedKey(t *testing.T) {
	first, _ := testKeys(t)
	second, _ := testKeys(t)
	path := filepath.Join(t.TempDir(), "verification.pem")
	var file []byte
	for _, key := range []ed25519.PublicKey{first, second} {
		der, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		file = append(file, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...)
	}
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatalf("write verification file: %v", err)
	}
	loaded, err := LoadVerificationKeys(path)
	if err != nil {
		t.Fatalf("LoadVerificationKeys: %v", err)
	}
	if len(loaded) != 2 || !loaded[0].Equal(first) || !loaded[1].Equal(second) {
		t.Fatalf("LoadVerificationKeys read %d keys, want the two that were written", len(loaded))
	}
}

// TestLoadVerificationKeysRefusesAnUnusableBlockInTheSet proves the set is validated as a whole. A
// file that silently dropped the block an operator had just added would keep verifying against the
// outgoing key until the issuer switched, and then refuse everything — the failure would appear long
// after its cause, so it is a startup refusal instead.
func TestLoadVerificationKeysRefusesAnUnusableBlockInTheSet(t *testing.T) {
	public, _ := testKeys(t)
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	good := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	junk := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("not a public key")})
	for name, file := range map[string][]byte{
		"unusable block after a good one":  append(append([]byte{}, good...), junk...),
		"unusable block before a good one": append(append([]byte{}, junk...), good...),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verification.pem")
			if err := os.WriteFile(path, file, 0o600); err != nil {
				t.Fatalf("write verification file: %v", err)
			}
			if _, err := LoadVerificationKeys(path); !errors.Is(err, ErrKey) {
				t.Fatalf("LoadVerificationKeys error = %v, want %v", err, ErrKey)
			}
		})
	}
}

// TestLoadSigningKeyRefusesMoreThanOneBlock proves the signing key has no set semantics: two private
// keys in one file would make "which key signs" a question of file order, so it is refused rather
// than resolved by position.
func TestLoadSigningKeyRefusesMoreThanOneBlock(t *testing.T) {
	_, first := testKeys(t)
	_, second := testKeys(t)
	path := filepath.Join(t.TempDir(), "signing.pem")
	var file []byte
	for _, key := range []ed25519.PrivateKey{first, second} {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal private key: %v", err)
		}
		file = append(file, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})...)
	}
	if err := os.WriteFile(path, file, 0o600); err != nil {
		t.Fatalf("write signing file: %v", err)
	}
	if _, err := LoadSigningKey(path); !errors.Is(err, ErrKey) {
		t.Fatalf("LoadSigningKey error = %v, want %v", err, ErrKey)
	}
}

// writePEM writes one PEM block to a temporary path, so generated key material never reaches a
// durable fixture.
func writePEM(t *testing.T, name, blockType string, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	encoded := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestKeyFilesRoundTrip proves the deployment can generate these keys with the tooling it already
// uses: PKCS#8 for the private half and PKIX for the public half, both PEM-wrapped.
func TestKeyFilesRoundTrip(t *testing.T) {
	public, private := testKeys(t)
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	loadedPrivate, err := LoadSigningKey(writePEM(t, "signing.pem", "PRIVATE KEY", privateDER))
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	loadedPublic, err := LoadVerificationKeys(writePEM(t, "verification.pem", "PUBLIC KEY", publicDER))
	if err != nil {
		t.Fatalf("LoadVerificationKeys: %v", err)
	}
	if len(loadedPublic) != 1 {
		t.Fatalf("LoadVerificationKeys read %d keys, want 1", len(loadedPublic))
	}
	token, err := Issue(loadedPrivate, testBinding())
	if err != nil {
		t.Fatalf("Issue with the loaded key: %v", err)
	}
	if _, err := Verify(loadedPublic, token, testIssued()); err != nil {
		t.Fatalf("Verify with the loaded key: %v", err)
	}
}

// TestLoadKeysRejectUnusableFiles proves a misconfigured deployment fails at startup rather than
// coming up with a key that verifies nothing or, worse, accepts anything.
func TestLoadKeysRejectUnusableFiles(t *testing.T) {
	_, private := testKeys(t)
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(private.Public())
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	// A second, non-Ed25519 key proves the loader checks the algorithm rather than only the encoding.
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	ecdsaPrivateDER, err := x509.MarshalPKCS8PrivateKey(other)
	if err != nil {
		t.Fatalf("marshal ECDSA private key: %v", err)
	}
	ecdsaPublicDER, err := x509.MarshalPKIXPublicKey(&other.PublicKey)
	if err != nil {
		t.Fatalf("marshal ECDSA public key: %v", err)
	}
	notPEM := filepath.Join(t.TempDir(), "plain.pem")
	if err := os.WriteFile(notPEM, []byte("not a pem file"), 0o600); err != nil {
		t.Fatalf("write plain file: %v", err)
	}
	cases := []struct {
		name    string
		load    func(string) error
		path    string
		wantErr bool
	}{
		{"signing key", func(p string) error { _, err := LoadSigningKey(p); return err }, writePEM(t, "signing.pem", "PRIVATE KEY", privateDER), false},
		{"verification key", func(p string) error { _, err := LoadVerificationKeys(p); return err }, writePEM(t, "verification.pem", "PUBLIC KEY", publicDER), false},
		{"signing key given the public half", func(p string) error { _, err := LoadSigningKey(p); return err }, writePEM(t, "public.pem", "PUBLIC KEY", publicDER), true},
		{"verification key given the private half", func(p string) error { _, err := LoadVerificationKeys(p); return err }, writePEM(t, "private.pem", "PRIVATE KEY", privateDER), true},
		{"signing key of another algorithm", func(p string) error { _, err := LoadSigningKey(p); return err }, writePEM(t, "ecdsa-private.pem", "PRIVATE KEY", ecdsaPrivateDER), true},
		{"verification key of another algorithm", func(p string) error { _, err := LoadVerificationKeys(p); return err }, writePEM(t, "ecdsa-public.pem", "PUBLIC KEY", ecdsaPublicDER), true},
		{"not PEM", func(p string) error { _, err := LoadSigningKey(p); return err }, notPEM, true},
		{"absent", func(p string) error { _, err := LoadVerificationKeys(p); return err }, filepath.Join(t.TempDir(), "absent.pem"), true},
		{"unconfigured", func(p string) error { _, err := LoadVerificationKeys(p); return err }, "", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.load(test.path)
			if test.wantErr {
				if !errors.Is(err, ErrKey) {
					t.Fatalf("error = %v, want %v", err, ErrKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
