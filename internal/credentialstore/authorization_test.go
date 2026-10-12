package credentialstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// The runtime the fixture's authorizations are bound to. The values are arbitrary but must be
// positive, because the binding validation refuses a zero generation or epoch.
const (
	testNodeID            = "node-1"
	testNodeInstanceID    = "node-instance-1"
	testNodeIncarnationID = "incarnation-1"
	testRuntimeGeneration = int64(3)
	testControlEpoch      = int64(7)
)

// testAuthorizationID builds a canonical UUID for an authorization, matching what Cloud mints and
// what the Store requires before it will turn one into a file name.
func testAuthorizationID(n int) string {
	return fmt.Sprintf("11111111-1111-4111-8111-%012x", n)
}

// writePublicKeyFile mounts Ed25519 public keys the way the deployment does: a PEM file, never an
// inline configuration value, and never a private half. It takes a set because a deployment rotates
// its signing key by mounting the outgoing and incoming keys together for one overlap window.
func writePublicKeyFile(t *testing.T, path string, keys ...ed25519.PublicKey) string {
	t.Helper()
	var encoded []byte
	for _, key := range keys {
		der, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			t.Fatalf("marshal public key: %v", err)
		}
		encoded = append(encoded, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return path
}

// resolveFixture is a Store with the resolve capability configured, alongside the signing keys the
// tests issue authorizations with.
type resolveFixture struct {
	*storeFixture
	resolver *Resolver
	// signers are the fixture's signing keys, by position. A fixture that mounted one trusted key has
	// one signer; a rotation fixture mounts two trusted keys and can sign with either.
	signers    []ed25519.PrivateKey
	publicFile string
}

// newResolveFixture builds a fixture whose Store has a real volume, a real keyring, an injected clock
// and a mounted verification key.
func newResolveFixture(t *testing.T) *resolveFixture {
	t.Helper()
	return newResolveFixtureWith(t, DefaultRetention)
}

// newResolveFixtureWith builds the same fixture with a chosen retention window, so a test can reach a
// closed retention window without exceeding the authorization's own maximum lifetime.
func newResolveFixtureWith(t *testing.T, retention time.Duration) *resolveFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate authorization key: %v", err)
	}
	return newResolveFixtureWithKeys(t, retention, []ed25519.PrivateKey{private}, []ed25519.PublicKey{public})
}

// newResolveFixtureWithKeys builds the fixture over a chosen verification key set. `signers` is what
// the tests sign with; `trusted` is what the Store mounted. They are separate arguments so a test can
// sign with a key the deployment does not trust, which is the forgery case.
func newResolveFixtureWithKeys(t *testing.T, retention time.Duration, signers []ed25519.PrivateKey, trusted []ed25519.PublicKey) *resolveFixture {
	t.Helper()
	base := t.TempDir()
	volume := filepath.Join(base, "volume")
	primary := "kek-primary"
	keys := map[string][]byte{primary: keyMaterial(t)}
	keyFile := writeKeyringFile(t, filepath.Join(base, "kek.json"), primary, keys)
	publicFile := writePublicKeyFile(t, filepath.Join(base, "cloneauth.pem"), trusted...)
	clock := &testClock{now: testStart()}
	store, err := Open(Config{
		Volume:                     volume,
		KeyFile:                    keyFile,
		Retention:                  retention,
		Now:                        clock.Now,
		AuthorizationPublicKeyFile: publicFile,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	resolver, ok := store.Resolver()
	if !ok {
		t.Fatal("Store.Resolver() reported no capability although a verification key was configured")
	}
	return &resolveFixture{
		storeFixture: &storeFixture{store: store, clock: clock, volume: volume, keyFile: keyFile, keys: keys, primary: primary},
		resolver:     resolver,
		signers:      signers,
		publicFile:   publicFile,
	}
}

// binding returns a valid binding for one credential version at the fixture's current instant.
func (f *resolveFixture) binding(credentialID string, version int64) cloneauth.Binding {
	issued := f.clock.Now()
	return cloneauth.Binding{
		AuthorizationID:   testAuthorizationID(1),
		Operation:         cloneauth.OperationClone,
		TenantID:          "tenant-1",
		OwnerUserID:       "user-1",
		ProjectID:         "project-1",
		WorkspaceID:       "workspace-1",
		ExecutionID:       "execution-1",
		CredentialID:      credentialID,
		CredentialVersion: version,
		Repository:        "https://github.com/ora-space/private",
		Capabilities:      []string{string(cloneauth.CapabilityClone)},
		NodeID:            testNodeID,
		NodeInstanceID:    testNodeInstanceID,
		NodeIncarnationID: testNodeIncarnationID,
		RuntimeGeneration: testRuntimeGeneration,
		ControlEpoch:      testControlEpoch,
		IssuedAt:          issued,
		ExpiresAt:         issued.Add(time.Minute),
	}
}

// request returns the resolve request a correctly behaved workload would present for one token.
func (f *resolveFixture) request(authorization string) ResolveRequest {
	return ResolveRequest{
		Authorization:     authorization,
		NodeID:            testNodeID,
		NodeInstanceID:    testNodeInstanceID,
		NodeIncarnationID: testNodeIncarnationID,
		RuntimeGeneration: testRuntimeGeneration,
		ControlEpoch:      testControlEpoch,
	}
}

// issue signs a binding with the fixture's first signing key.
func (f *resolveFixture) issue(t *testing.T, binding cloneauth.Binding) string {
	t.Helper()
	return f.issueWith(t, 0, binding)
}

// issueWith signs with one of the fixture's signing keys by position, so a rotation test can present
// an authorization from the outgoing key and one from the incoming key.
func (f *resolveFixture) issueWith(t *testing.T, index int, binding cloneauth.Binding) string {
	t.Helper()
	token, err := cloneauth.Issue(f.signers[index], binding)
	if err != nil {
		t.Fatalf("issue authorization: %v", err)
	}
	return token
}

// storeCredential writes one token and returns the version the Store assigned it.
func storeCredential(t *testing.T, store *Store, credentialID, token string) int64 {
	t.Helper()
	_, version, err := store.Write(context.Background(), credentialID, token)
	if err != nil {
		t.Fatalf("write credential %s: %v", credentialID, err)
	}
	return version
}

// readConsumption loads one consumption record through the Store's own path, so a test asserts what
// is on the volume rather than what the resolver happened to keep in memory.
func readConsumption(t *testing.T, fixture *resolveFixture, authorizationID string) (*consumptionRecord, bool) {
	t.Helper()
	path, err := fixture.store.files.authorizationPath(authorizationID)
	if err != nil {
		t.Fatalf("authorization path: %v", err)
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		t.Fatalf("read consumption record: %v", err)
	}
	var record consumptionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("decode consumption record: %v", err)
	}
	return &record, true
}

// TestResolveDeliversTheBoundToken proves the happy path: an authorization Cloud signed for one
// credential version yields exactly that token.
func TestResolveDeliversTheBoundToken(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(1)
	storeCredential(t, fixture.store, id, "ghp_bound_token")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	resolved, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "ghp_bound_token" {
		t.Fatalf("token = %q, want %q", resolved.Token, "ghp_bound_token")
	}
	if resolved.Version != 1 {
		t.Fatalf("version = %d, want 1", resolved.Version)
	}
	record, found := readConsumption(t, fixture, testAuthorizationID(1))
	if !found {
		t.Fatal("no consumption record was written")
	}
	if record.Result != consumptionDelivered {
		t.Fatalf("consumption result = %q, want %q", record.Result, consumptionDelivered)
	}
	if record.CredentialID != id || record.CredentialVersion != 1 {
		t.Fatalf("consumption record names %s v%d, want %s v1", record.CredentialID, record.CredentialVersion, id)
	}
	if record.NodeInstanceID != testNodeInstanceID || record.RuntimeGeneration != testRuntimeGeneration {
		t.Fatalf("consumption record names runtime %s/%d, want %s/%d",
			record.NodeInstanceID, record.RuntimeGeneration, testNodeInstanceID, testRuntimeGeneration)
	}
}

// TestResolveConsumesTheAuthorizationOnce proves the single-use rule: the second redemption of one
// authorization is refused, so a captured token cannot be spent twice (D10).
func TestResolveConsumesTheAuthorizationOnce(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(2)
	storeCredential(t, fixture.store, id, "ghp_once")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	_, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
	if !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("second Resolve error = %v, want %v", err, ErrAuthorizationConsumed)
	}
}

// TestResolveSurvivesARestart proves the consumption record is durable state rather than process
// memory: a Store reopened on the same volume still refuses the spent authorization, which is what
// D10 requires of a restart.
func TestResolveSurvivesARestart(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(3)
	storeCredential(t, fixture.store, id, "ghp_restart")
	authorization := fixture.issue(t, fixture.binding(id, 1))
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	// Reopen the same volume with the same key material, exactly as a restarted process would.
	reopened, err := Open(Config{
		Volume:                     fixture.volume,
		KeyFile:                    fixture.keyFile,
		Now:                        fixture.clock.Now,
		AuthorizationPublicKeyFile: fixture.publicFile,
	})
	if err != nil {
		t.Fatalf("reopen the Store: %v", err)
	}
	resolver, ok := reopened.Resolver()
	if !ok {
		t.Fatal("the reopened Store lost its resolve capability")
	}
	if _, err := resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("Resolve after restart error = %v, want %v", err, ErrAuthorizationConsumed)
	}
}

// TestResolveRejectsAForgedAuthorization proves the Store decides authenticity itself. Without this,
// anything holding the `broker-resolver` certificate could mint an authorization for any tenant's
// credential.
func TestResolveRejectsAForgedAuthorization(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(4)
	storeCredential(t, fixture.store, id, "ghp_forged")
	_, impostor, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate impostor key: %v", err)
	}
	forged, err := cloneauth.Issue(impostor, fixture.binding(id, 1))
	if err != nil {
		t.Fatalf("Issue with the impostor key: %v", err)
	}
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(forged)); !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationInvalid)
	}
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("a forged authorization was recorded as consumed")
	}
}

// TestResolveRejectsATamperedAuthorization proves the binding is covered by the signature, so a
// captured authorization cannot be edited to name another credential, tenant, or runtime.
func TestResolveRejectsATamperedAuthorization(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(5)
	storeCredential(t, fixture.store, id, "ghp_tampered")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	encodedPayload, encodedSignature, found := strings.Cut(authorization, ".")
	if !found {
		t.Fatalf("issued authorization %q has no separator", authorization)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	edited := strings.Replace(string(payload), `"tenantId":"tenant-1"`, `"tenantId":"tenant-2"`, 1)
	if edited == string(payload) {
		t.Fatalf("test did not change the payload: %s", payload)
	}
	tampered := base64.RawURLEncoding.EncodeToString([]byte(edited)) + "." + encodedSignature
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(tampered)); !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationInvalid)
	}
}

// TestResolveRejectsAnExpiredAuthorization proves expiry is judged by the Store's own clock and that
// an expired authorization is refused without being spent.
func TestResolveRejectsAnExpiredAuthorization(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(6)
	storeCredential(t, fixture.store, id, "ghp_expired")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	fixture.clock.Advance(2 * time.Minute)
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationExpired)
	}
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("an expired authorization was recorded as consumed")
	}
}

// TestResolveRejectsBindingMismatches proves each fencing value is load-bearing: an authorization
// issued for one credential or one runtime cannot be spent on another, and a refused attempt leaves
// the authorization unspent.
func TestResolveRejectsBindingMismatches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*cloneauth.Binding, *ResolveRequest)
	}{
		{"another credential", func(b *cloneauth.Binding, _ *ResolveRequest) { b.CredentialID = testCredentialID(99) }},
		{"another node", func(_ *cloneauth.Binding, r *ResolveRequest) { r.NodeID = "node-2" }},
		{"another node instance", func(_ *cloneauth.Binding, r *ResolveRequest) { r.NodeInstanceID = "node-instance-2" }},
		{"another incarnation", func(_ *cloneauth.Binding, r *ResolveRequest) { r.NodeIncarnationID = "incarnation-2" }},
		{"another runtime generation", func(_ *cloneauth.Binding, r *ResolveRequest) { r.RuntimeGeneration = testRuntimeGeneration + 1 }},
		{"another control epoch", func(_ *cloneauth.Binding, r *ResolveRequest) { r.ControlEpoch = testControlEpoch + 1 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			id := testCredentialID(7)
			storeCredential(t, fixture.store, id, "ghp_binding")
			binding := fixture.binding(id, 1)
			request := fixture.request("")
			test.mutate(&binding, &request)
			request.Authorization = fixture.issue(t, binding)

			if _, err := fixture.resolver.Resolve(context.Background(), id, request); !errors.Is(err, ErrAuthorizationBinding) {
				t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationBinding)
			}
			if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
				t.Fatal("a mismatched authorization was recorded as consumed")
			}
		})
	}
}

// TestResolveRejectsAnUnavailableVersion proves the version binding is checked against the Store's own
// state: an authorization for a version this Store cannot serve is refused rather than silently
// answered with the current one, and a rotated-away version stops being available once its retention
// window closes (D4, D7).
func TestResolveRejectsAnUnavailableVersion(t *testing.T) {
	t.Run("version never existed", func(t *testing.T) {
		fixture := newResolveFixture(t)
		id := testCredentialID(8)
		storeCredential(t, fixture.store, id, "ghp_version")
		authorization := fixture.issue(t, fixture.binding(id, 9))
		if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrVersionUnavailable) {
			t.Fatalf("Resolve error = %v, want %v", err, ErrVersionUnavailable)
		}
	})

	t.Run("retention window closed", func(t *testing.T) {
		// The retention window must be shorter than the authorization's lifetime, otherwise the
		// authorization would expire first and this would prove expiry rather than retention.
		fixture := newResolveFixtureWith(t, 30*time.Second)
		id := testCredentialID(9)
		storeCredential(t, fixture.store, id, "ghp_first")
		if _, err := fixture.store.Rotate(context.Background(), id, "ghp_second"); err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		authorization := fixture.issue(t, fixture.binding(id, 1))
		fixture.clock.Advance(31 * time.Second)
		if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrVersionUnavailable) {
			t.Fatalf("Resolve error = %v, want %v", err, ErrVersionUnavailable)
		}
	})
}

// TestResolveServesARetainedVersion proves the other half of D4: an execution admitted under the
// version that was current when it started can still settle after a rotation, inside the window.
func TestResolveServesARetainedVersion(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(10)
	storeCredential(t, fixture.store, id, "ghp_first")
	if _, err := fixture.store.Rotate(context.Background(), id, "ghp_second"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	authorization := fixture.issue(t, fixture.binding(id, 1))
	resolved, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Token != "ghp_first" {
		t.Fatalf("token = %q, want the retained version %q", resolved.Token, "ghp_first")
	}
	if resolved.Version != 1 {
		t.Fatalf("version = %d, want 1", resolved.Version)
	}
}

// TestResolveRejectsARevokedCredential proves revocation is checked before anything is delivered, so a
// revoked credential cannot be revived by an authorization issued earlier.
func TestResolveRejectsARevokedCredential(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(11)
	storeCredential(t, fixture.store, id, "ghp_revoked")
	authorization := fixture.issue(t, fixture.binding(id, 1))
	if err := fixture.store.Revoke(context.Background(), id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrRevoked)
	}
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("an authorization for a revoked credential was recorded as consumed")
	}
}

// TestResolveRejectsUnknownAndMalformedCredentialIds proves the Store's own boundary checks run
// independently of whatever the transport already validated.
func TestResolveRejectsUnknownAndMalformedCredentialIds(t *testing.T) {
	fixture := newResolveFixture(t)
	absent := testCredentialID(12)
	authorization := fixture.issue(t, fixture.binding(absent, 1))
	if _, err := fixture.resolver.Resolve(context.Background(), absent, fixture.request(authorization)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(absent) error = %v, want %v", err, ErrNotFound)
	}
	malformed := fixture.issue(t, fixture.binding("not-a-uuid", 1))
	if _, err := fixture.resolver.Resolve(context.Background(), "not-a-uuid", fixture.request(malformed)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Resolve(malformed) error = %v, want %v", err, ErrInvalidRequest)
	}
}

// TestResolveHonoursCancellation proves a cancelled request never spends an authorization, so a
// caller that went away does not burn the only credential it had.
func TestResolveHonoursCancellation(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(13)
	storeCredential(t, fixture.store, id, "ghp_cancelled")
	authorization := fixture.issue(t, fixture.binding(id, 1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.resolver.Resolve(ctx, id, fixture.request(authorization)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve error = %v, want %v", err, context.Canceled)
	}
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("a cancelled request recorded a consumption")
	}
}

// TestResolveLeavesTheAuthorizationUnspentWhenChecksFail proves the ordering D10 requires: every check
// that can fail without a secret runs before the consumption record is written, so a caller refused
// for a fixable reason can correct its request instead of asking for a new authorization.
func TestResolveLeavesTheAuthorizationUnspentWhenChecksFail(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(14)
	storeCredential(t, fixture.store, id, "ghp_retry")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	wrong := fixture.request(authorization)
	wrong.ControlEpoch = testControlEpoch + 1
	if _, err := fixture.resolver.Resolve(context.Background(), id, wrong); !errors.Is(err, ErrAuthorizationBinding) {
		t.Fatalf("Resolve(mismatched) error = %v, want %v", err, ErrAuthorizationBinding)
	}
	resolved, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
	if err != nil {
		t.Fatalf("Resolve after a refused attempt: %v", err)
	}
	if resolved.Token != "ghp_retry" {
		t.Fatalf("token = %q, want %q", resolved.Token, "ghp_retry")
	}
}

// TestResolveIsUnavailableWithoutAVerificationKey proves the fail-closed default: a deployment that
// configured no key gets no resolve capability at all, rather than one that trusts its callers.
func TestResolveIsUnavailableWithoutAVerificationKey(t *testing.T) {
	fixture := newStoreFixture(t)
	if _, ok := fixture.store.Resolver(); ok {
		t.Fatal("a Store with no verification key offered a resolver")
	}
}

// TestResolveConsumesAtMostOnceUnderConcurrency proves the single-use decision is atomic under
// contention: simultaneous redemptions of one authorization yield exactly one success, never two.
func TestResolveConsumesAtMostOnceUnderConcurrency(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(15)
	storeCredential(t, fixture.store, id, "ghp_concurrent")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	const attempts = 8
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wait sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	succeeded, consumed := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAuthorizationConsumed):
			consumed++
		default:
			t.Fatalf("unexpected Resolve error under contention: %v", err)
		}
	}
	if succeeded != 1 || consumed != attempts-1 {
		t.Fatalf("succeeded = %d, consumed = %d; want 1 success and %d refusals", succeeded, consumed, attempts-1)
	}
}

// rotatingFixture builds a fixture whose Store trusts two signing keys at once, which is the state a
// deployment is in during a key rotation, and returns the two signing keys.
func rotatingFixture(t *testing.T) (*resolveFixture, ed25519.PrivateKey, ed25519.PrivateKey) {
	t.Helper()
	outgoingPublic, outgoingPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate outgoing key: %v", err)
	}
	incomingPublic, incomingPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate incoming key: %v", err)
	}
	fixture := newResolveFixtureWithKeys(t, DefaultRetention,
		[]ed25519.PrivateKey{outgoingPrivate, incomingPrivate},
		[]ed25519.PublicKey{outgoingPublic, incomingPublic},
	)
	return fixture, outgoingPrivate, incomingPrivate
}

// TestResolveAcceptsAnAuthorizationFromAnyTrustedKey proves the Store honors the whole mounted key
// set, which is what lets a deployment rotate its signing key without refusing the clones that were
// already authorized under the outgoing one.
func TestResolveAcceptsAnAuthorizationFromAnyTrustedKey(t *testing.T) {
	fixture, _, _ := rotatingFixture(t)
	id := testCredentialID(30)
	storeCredential(t, fixture.store, id, "ghp_rotating")

	for index, name := range []string{"outgoing key", "incoming key"} {
		t.Run(name, func(t *testing.T) {
			binding := fixture.binding(id, 1)
			binding.AuthorizationID = testAuthorizationID(30 + index)
			resolved, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(fixture.issueWith(t, index, binding)))
			if err != nil {
				t.Fatalf("Resolve with the %s: %v", name, err)
			}
			if resolved.Token != "ghp_rotating" {
				t.Fatalf("token = %q, want %q", resolved.Token, "ghp_rotating")
			}
		})
	}
}

// reopenResolveFixture opens the same volume and keyring again with a different verification key
// file, which is what a deployment does when it edits the mounted keys and restarts the Store.
func reopenResolveFixture(t *testing.T, fixture *resolveFixture, publicFile string) *resolveFixture {
	t.Helper()
	store, err := Open(Config{
		Volume:                     fixture.volume,
		KeyFile:                    fixture.keyFile,
		Now:                        fixture.clock.Now,
		AuthorizationPublicKeyFile: publicFile,
	})
	if err != nil {
		t.Fatalf("reopen the Store: %v", err)
	}
	resolver, ok := store.Resolver()
	if !ok {
		t.Fatal("the reopened Store lost its resolve capability")
	}
	return &resolveFixture{storeFixture: fixture.storeFixture, resolver: resolver, signers: fixture.signers, publicFile: publicFile}
}

// TestResolveRefusesAnAuthorizationFromAnUntrustedKey proves the key set is a whitelist rather than a
// widening: mounting an additional key does not make the Store accept anything else, and a key that
// was withdrawn is refused exactly as an arbitrary forgery is. This is the state a deployment reaches
// once a rotation completes and the outgoing key is removed from the file.
func TestResolveRefusesAnAuthorizationFromAnUntrustedKey(t *testing.T) {
	fixture, _, _ := rotatingFixture(t)
	id := testCredentialID(31)
	storeCredential(t, fixture.store, id, "ghp_withdrawn")

	incoming := fixture.signers[1].Public().(ed25519.PublicKey)
	afterRotation := reopenResolveFixture(t, fixture, writePublicKeyFile(t, filepath.Join(t.TempDir(), "cloneauth.pem"), incoming))

	authorization := fixture.issueWith(t, 0, fixture.binding(id, 1))
	if _, err := afterRotation.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationInvalid)
	}
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("an authorization signed by a withdrawn key was recorded as consumed")
	}
	// The surviving key is still in force, so the refusal above is about the key that was withdrawn
	// rather than about the reopened Store refusing everything.
	surviving := fixture.binding(id, 1)
	surviving.AuthorizationID = testAuthorizationID(31)
	if _, err := afterRotation.resolver.Resolve(context.Background(), id, fixture.request(fixture.issueWith(t, 1, surviving))); err != nil {
		t.Fatalf("Resolve with the surviving key: %v", err)
	}
}

// TestResolveTreatsAnUnparseableConsumptionRecordAsSpent proves the single-use decision is made by
// the existence of the record rather than by its contents. A record torn by a crash mid-write, or
// truncated by a full volume, therefore reads as "spent" — the conservative direction — instead of
// being reported as a decode error a caller might retry, or worse, treated as absent and replayed.
func TestResolveTreatsAnUnparseableConsumptionRecordAsSpent(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(32)
	storeCredential(t, fixture.store, id, "ghp_torn_record")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	path, err := fixture.store.files.authorizationPath(testAuthorizationID(1))
	if err != nil {
		t.Fatalf("authorization path: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"authorizationId":`), 0o600); err != nil {
		t.Fatalf("write a torn consumption record: %v", err)
	}
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationConsumed)
	}
}

// TestResolveRefusesWhenTheConsumptionRecordCannotBeWritten proves the other direction of the same
// ordering: a Store that cannot durably record that an authorization was spent must deliver nothing,
// even though the authorization itself is genuine and every other check passed. The volume is made
// unwritable by replacing the record directory with a file, which fails for any process identity.
func TestResolveRefusesWhenTheConsumptionRecordCannotBeWritten(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(33)
	storeCredential(t, fixture.store, id, "ghp_no_record")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	directory := filepath.Join(fixture.volume, authorizationDirectory)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatalf("remove the consumption record directory: %v", err)
	}
	if err := os.WriteFile(directory, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("replace the consumption record directory: %v", err)
	}
	resolved, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
	if err == nil {
		t.Fatal("Resolve delivered a secret it could not record as spent")
	}
	if resolved.Token != "" {
		t.Fatalf("Resolve returned a token alongside the error %v", err)
	}
}

// TestResolveRecordsConsumedNotDeliveredWhenTheSecretCannotBeOpened proves the durable state a crash
// between spending the authorization and opening the secret leaves behind, and that the state is
// terminal: the record says "consumed, not delivered", and the same authorization can never be
// presented again. Reversing the two steps would instead leave a secret that left the Store with no
// evidence it was spent, which is exactly a replayable authorization (D10).
func TestResolveRecordsConsumedNotDeliveredWhenTheSecretCannotBeOpened(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(34)
	storeCredential(t, fixture.store, id, "ghp_unopenable")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	stored := readRecord(t, fixture.store, id)
	stored.Current.Ciphertext = base64.StdEncoding.EncodeToString([]byte("tampered ciphertext"))
	if err := fixture.store.files.write(stored); err != nil {
		t.Fatalf("corrupt the stored material: %v", err)
	}

	resolved, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization))
	if err == nil {
		t.Fatal("Resolve reported success although the sealed token could not be opened")
	}
	if errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("the first attempt reported %v; it should report the failure to open the secret", ErrAuthorizationConsumed)
	}
	if resolved.Token != "" {
		t.Fatalf("Resolve returned a token alongside the error %v", err)
	}
	record, found := readConsumption(t, fixture, testAuthorizationID(1))
	if !found {
		t.Fatal("no consumption record was written although the authorization was spent")
	}
	if record.Result != consumptionConsumed {
		t.Fatalf("consumption result = %q, want %q", record.Result, consumptionConsumed)
	}
	if _, err := fixture.resolver.Resolve(context.Background(), id, fixture.request(authorization)); !errors.Is(err, ErrAuthorizationConsumed) {
		t.Fatalf("second Resolve error = %v, want %v", err, ErrAuthorizationConsumed)
	}
}

// TestResolveRefusesAnIncompleteRuntimeDeclaration proves the Store does not treat a caller's
// silence as agreement: a request that declares no runtime at all cannot match a binding that names
// one, so it is refused before anything is spent. The runtime fields are the presenting workload's
// own declaration, so a caller that omits them has declared nothing the Store can compare.
func TestResolveRefusesAnIncompleteRuntimeDeclaration(t *testing.T) {
	fixture := newResolveFixture(t)
	id := testCredentialID(35)
	storeCredential(t, fixture.store, id, "ghp_silent")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	bare := ResolveRequest{Authorization: authorization}
	if _, err := fixture.resolver.Resolve(context.Background(), id, bare); !errors.Is(err, ErrAuthorizationBinding) {
		t.Fatalf("Resolve error = %v, want %v", err, ErrAuthorizationBinding)
	}
	if _, found := readConsumption(t, fixture, testAuthorizationID(1)); found {
		t.Fatal("a request that declared no runtime recorded a consumption")
	}
}

// resolveBody builds the transport payload a workload sends, so the HTTP tests exercise the same
// shape the Store's decoder accepts.
func resolveBody(t *testing.T, authorization string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"authorization":     authorization,
		"nodeId":            testNodeID,
		"nodeInstanceId":    testNodeInstanceID,
		"nodeIncarnationId": testNodeIncarnationID,
		"runtimeGeneration": testRuntimeGeneration,
		"controlEpoch":      testControlEpoch,
	})
	if err != nil {
		t.Fatalf("encode resolve body: %v", err)
	}
	return string(raw)
}

// TestServerResolveDeliversThenRefusesReplay proves the whole path over real mutual TLS: the resolver
// identity receives the token once, and the same authorization is then refused as consumed.
func TestServerResolveDeliversThenRefusesReplay(t *testing.T) {
	fixture := newResolveFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolverCert := pki.issue(t, 40, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(20)
	storeCredential(t, fixture.store, id, "ghp_over_transport")
	authorization := fixture.issue(t, fixture.binding(id, 1))
	body := resolveBody(t, authorization)

	status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve", body)
	if status != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 (body %s)", status, raw)
	}
	if token, _ := decodeBody(t, raw)["token"].(string); token != "ghp_over_transport" {
		t.Fatalf("resolved token = %q, want %q", token, "ghp_over_transport")
	}
	status, raw = doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve", body)
	if status != http.StatusConflict {
		t.Fatalf("replay status = %d, want 409 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "authorization_consumed" {
		t.Fatalf("replay code = %v, want authorization_consumed", code)
	}
}

// TestServerResolveRefusalCodesOverTransport proves each refusal is distinguishable at the wire, which
// is what D7 requires and what lets a caller choose between asking for a new authorization and
// investigating.
func TestServerResolveRefusalCodesOverTransport(t *testing.T) {
	cases := []struct {
		name       string
		prepare    func(t *testing.T, fixture *resolveFixture, id string) (string, string)
		wantStatus int
		wantCode   string
	}{
		{
			name: "expired",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) (string, string) {
				authorization := fixture.issue(t, fixture.binding(id, 1))
				fixture.clock.Advance(2 * time.Minute)
				return id, authorization
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_expired",
		},
		{
			name: "forged",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) (string, string) {
				_, impostor, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatalf("generate impostor key: %v", err)
				}
				forged, err := cloneauth.Issue(impostor, fixture.binding(id, 1))
				if err != nil {
					t.Fatalf("Issue with the impostor key: %v", err)
				}
				return id, forged
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_invalid",
		},
		{
			name: "binding mismatch",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) (string, string) {
				return testCredentialID(98), fixture.issue(t, fixture.binding(id, 1))
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "authorization_binding_mismatch",
		},
		{
			name: "revoked credential",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) (string, string) {
				authorization := fixture.issue(t, fixture.binding(id, 1))
				if err := fixture.store.Revoke(context.Background(), id); err != nil {
					t.Fatalf("Revoke: %v", err)
				}
				return id, authorization
			},
			wantStatus: http.StatusConflict,
			wantCode:   "credential_revoked",
		},
		{
			name: "version unavailable",
			prepare: func(t *testing.T, fixture *resolveFixture, id string) (string, string) {
				return id, fixture.issue(t, fixture.binding(id, 9))
			},
			wantStatus: http.StatusConflict,
			wantCode:   "credential_version_unavailable",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			server, pki := newTestServer(t, fixture.store, nil)
			resolverCert := pki.issue(t, 41, testResolverIdentity, false)
			client := pki.client(t, &resolverCert)
			id := testCredentialID(21)
			storeCredential(t, fixture.store, id, "ghp_refusals")
			pathID, authorization := test.prepare(t, fixture, id)

			status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+pathID+"/resolve", resolveBody(t, authorization))
			if status != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, test.wantStatus, raw)
			}
			if code := decodeBody(t, raw)["code"]; code != test.wantCode {
				t.Fatalf("code = %v, want %q", code, test.wantCode)
			}
		})
	}
}

// TestServerResolveKeepsSecretsOutOfTheAuditLog proves the audit line records the operation without
// recording either the authorization or the token. D7 fixes what may appear there, and both of these
// are bearer secrets.
func TestServerResolveKeepsSecretsOutOfTheAuditLog(t *testing.T) {
	fixture := newResolveFixture(t)
	logger, buffer := bufferedLogger()
	server, pki := newTestServer(t, fixture.store, logger)
	resolverCert := pki.issue(t, 42, testResolverIdentity, false)
	client := pki.client(t, &resolverCert)
	id := testCredentialID(22)
	storeCredential(t, fixture.store, id, "ghp_must_not_be_logged")
	authorization := fixture.issue(t, fixture.binding(id, 1))

	status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/resolve", resolveBody(t, authorization))
	if status != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200 (body %s)", status, raw)
	}
	logged := buffer.String()
	if strings.Contains(logged, authorization) {
		t.Fatalf("the audit log contains the authorization: %s", logged)
	}
	if strings.Contains(logged, "ghp_must_not_be_logged") {
		t.Fatalf("the audit log contains the token: %s", logged)
	}
	if !strings.Contains(logged, `"operation":"resolve"`) || !strings.Contains(logged, `"result":"succeeded"`) {
		t.Fatalf("the audit log is missing the resolve record: %s", logged)
	}
}
