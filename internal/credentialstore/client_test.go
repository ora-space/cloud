package credentialstore

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// Client is Cloud's ingress-writer: the only code in Cloud that hands a raw user token to another
// process. These tests drive the real client against the real server over mutual TLS, because the two
// halves only agree if the reference travels back, the refusal vocabulary lines up on both sides, and
// an unreachable store is never mistaken for a refusal (D1, D3, D4, D6, D7).
//
// The assertion below keeps the capability set honest: write, rotate and revoke are exactly what D3
// grants the ingress-writer, and there is deliberately no resolve method to add here.
var _ interface {
	Write(ctx context.Context, credentialID, token string) (string, error)
	Rotate(ctx context.Context, credentialID, token string) error
	Revoke(ctx context.Context, credentialID string) error
} = (*Client)(nil)

// clientTLS builds the client-side configuration the ingress runs with: the deployment-issued
// certificate as the service identity and the store's authority as the only trust root.
func clientTLS(t *testing.T, pki *testPKI, identity string, serial int64) *tls.Config {
	t.Helper()
	certificate := pki.issue(t, serial, identity, false)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      pki.roots,
		ServerName:   testServerName,
	}
}

// newIngressClient wires one real client to a running store.
func newIngressClient(t *testing.T, server *httptest.Server, pki *testPKI, identity string, serial int64) *Client {
	t.Helper()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, TLS: clientTLS(t, pki, identity, serial)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// TestClientRoundTripsTheCredentialLifecycleOverMutualTLS proves the whole ingress handoff works
// against the real service: the token Cloud sends is the token the store sealed, the reference the
// store generated is the reference Cloud receives, and the three operations keep D4's ordering
// properties end to end.
func TestClientRoundTripsTheCredentialLifecycleOverMutualTLS(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	client := newIngressClient(t, server, pki, testIngressIdentity, 40)
	ctx := context.Background()
	id := testCredentialID(40)

	first, err := client.Write(ctx, id, "ghp_first")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	// D1 places generation in the store, so what comes back is the store's own opaque reference.
	if !strings.HasPrefix(first, secretRefPrefix) {
		t.Fatalf("Write returned %q, want the store's own reference", first)
	}
	stored := readRecord(t, fixture.store, id)
	if stored.SecretRef != first {
		t.Fatalf("Client reported %q but the record holds %q", first, stored.SecretRef)
	}
	if openTokenOn(t, fixture.store, id, &stored.Current) != "ghp_first" {
		t.Fatal("the sealed material is not the token that crossed the boundary")
	}
	// A retried create returns the original reference instead of sealing a second copy (D4).
	repeated, err := client.Write(ctx, id, "ghp_first")
	if err != nil || repeated != first {
		t.Fatalf("a replayed Write returned %q (%v), want the original %q", repeated, err, first)
	}

	if err := client.Rotate(ctx, id, "ghp_second"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	rotated := readRecord(t, fixture.store, id)
	if rotated.Version != 2 || openTokenOn(t, fixture.store, id, &rotated.Current) != "ghp_second" {
		t.Fatalf("Rotate left version %d holding the wrong material", rotated.Version)
	}
	if rotated.SecretRef != first {
		t.Fatal("rotation changed the reference Cloud persists")
	}
	// The superseded version is retained rather than deleted, so an execution already admitted under it
	// can still settle after Cloud advanced its pointer.
	if len(rotated.Previous) != 1 || openTokenOn(t, fixture.store, id, &rotated.Previous[0].Material) != "ghp_first" {
		t.Fatal("the superseded version was not retained for the grace period")
	}
	// Re-sending the token already in place is a no-op: treating it as a rotation would advance a
	// version nothing depends on while burning the window above.
	if err := client.Rotate(ctx, id, "ghp_second"); err != nil {
		t.Fatalf("Rotate of the current token: %v", err)
	}
	if version := readRecord(t, fixture.store, id).Version; version != 2 {
		t.Fatalf("a repeated Rotate advanced the version to %d", version)
	}

	if err := client.Revoke(ctx, id); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked := readRecord(t, fixture.store, id)
	if !revoked.revoked() || revoked.Current.Ciphertext != "" || len(revoked.Previous) != 0 {
		t.Fatal("Revoke left readable material behind")
	}
	// The tombstone is what stops a replay from resurrecting a credential Cloud has already reported as
	// gone, and the store's refusal is the answer the client must surface.
	if _, err := client.Write(ctx, id, "ghp_first"); err != nil {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Code != ReasonCodeRevoked || failure.Status != 409 {
			t.Fatalf("a write after revocation returned %v, want a %s refusal", err, ReasonCodeRevoked)
		}
	} else {
		t.Fatal("a replayed create resurrected a revoked credential")
	}
}

// TestClientRefusalsAreStableCodesWithoutStoreText proves the client maps each refusal to the code
// Cloud's own reason-code mapping consumes, and that nothing but the code crosses back — the store's
// text and anything Cloud sent stay on their own sides of the boundary (D6, D7).
func TestClientRefusalsAreStableCodesWithoutStoreText(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	client := newIngressClient(t, server, pki, testIngressIdentity, 41)
	ctx := context.Background()
	const token = "ghp_refusal_must_not_echo_this"
	id := testCredentialID(41)
	if _, err := client.Write(ctx, id, token); err != nil {
		t.Fatalf("Write: %v", err)
	}

	cases := []struct {
		name   string
		call   func() error
		code   string
		status int
	}{
		{"a credential the store never held", func() error { return client.Rotate(ctx, testCredentialID(97), "ghp_next") }, ReasonCodeNotFound, 404},
		{"a token the boundary refuses", func() error { return client.Rotate(ctx, id, "") }, ReasonCodeInvalidRequest, 400},
		{"material that disagrees with the record", func() error {
			_, err := client.Write(ctx, id, "ghp_a_different_secret")
			return err
		}, ReasonCodeConflict, 409},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatalf("got %v, want the store's own refusal", err)
			}
			if failure.Code != test.code || failure.Status != test.status {
				t.Fatalf("refusal = (%s, %d), want (%s, %d)", failure.Code, failure.Status, test.code, test.status)
			}
			if failure.ReasonCode() != test.code {
				t.Fatalf("ReasonCode() = %q, want %q", failure.ReasonCode(), test.code)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("a refusal echoed the token Cloud handed over")
			}
		})
	}
}

// TestClientHoldingTheWrongIdentityIsRefused proves capability separation holds for a caller that
// authenticates successfully: the resolver identity is a real, issued identity, so the refusal is
// about what it may do rather than whether it is known (D3).
func TestClientHoldingTheWrongIdentityIsRefused(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	resolver := newIngressClient(t, server, pki, testResolverIdentity, 42)

	err := resolver.Rotate(context.Background(), testCredentialID(42), "ghp_next")
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "capability_denied" || failure.Status != 403 {
		t.Fatalf("the resolver identity reached a write: %v", err)
	}
	if _, found, readErr := fixture.store.files.read(testCredentialID(42)); readErr != nil || found {
		t.Fatal("a denied caller wrote material")
	}
}

// TestClientWithoutAReachableStoreFailsRatherThanFallingBack pins D6 at the client: a store that
// cannot be reached is an explicit failure, never an anonymous or static-credential substitute, and
// the token it was carrying never appears in the error the failure produces.
func TestClientWithoutAReachableStoreFailsRatherThanFallingBack(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	client := newIngressClient(t, server, pki, testIngressIdentity, 43)
	const token = "ghp_must_not_be_written"
	// Closing the listener is the "the store went away" case, which is the one an ingress meets in
	// production; httptest's Close is idempotent, so the fixture's own cleanup still applies.
	server.Close()

	_, err := client.Write(context.Background(), testCredentialID(43), token)
	if err == nil {
		t.Fatal("a write succeeded with no reachable store")
	}
	var failure *Failure
	if errors.As(err, &failure) {
		t.Fatalf("an unreachable store was reported as the store's own refusal (%s)", failure.Code)
	}
	// The token only ever travels in a request body, so even an error that names the URL cannot carry it.
	if strings.Contains(err.Error(), token) {
		t.Fatal("a transport error carried the token")
	}
	if _, found, readErr := fixture.store.files.read(testCredentialID(43)); readErr != nil || found {
		t.Fatal("an unstored credential left a record behind")
	}
}

// TestNewClientRefusesADeploymentThatCannotAuthenticate proves an enabled ingress either gets a
// complete mutual-TLS configuration or an error at startup. D6 requires the refusal to happen here:
// a client that could not authenticate but started anyway would turn every credential write into a
// per-request failure, and one that silently accepted plaintext would carry the token in the clear.
func TestNewClientRefusesADeploymentThatCannotAuthenticate(t *testing.T) {
	pki := newTestPKI(t)
	cases := []struct {
		name string
		cfg  ClientConfig
	}{
		{"no endpoint", ClientConfig{TLS: clientTLS(t, pki, testIngressIdentity, 44)}},
		{"a plaintext endpoint", ClientConfig{Endpoint: "http://store.invalid:8443", TLS: clientTLS(t, pki, testIngressIdentity, 44)}},
		{"a relative endpoint", ClientConfig{Endpoint: "/v1/credentials", TLS: clientTLS(t, pki, testIngressIdentity, 44)}},
		{"an endpoint carrying a query", ClientConfig{Endpoint: "https://store.invalid:8443?token=x", TLS: clientTLS(t, pki, testIngressIdentity, 44)}},
		{"an endpoint carrying a fragment", ClientConfig{Endpoint: "https://store.invalid:8443#token=x", TLS: clientTLS(t, pki, testIngressIdentity, 44)}},
		{"no transport configuration", ClientConfig{Endpoint: "https://store.invalid:8443"}},
		{"no service certificate", ClientConfig{Endpoint: "https://store.invalid:8443", TLS: &tls.Config{RootCAs: pki.roots}}},
		{"no trusted authority", ClientConfig{Endpoint: "https://store.invalid:8443", TLS: &tls.Config{Certificates: clientTLS(t, pki, testIngressIdentity, 44).Certificates}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewClient(test.cfg)
			if err == nil {
				t.Fatalf("NewClient(%s) returned a usable client", test.name)
			}
			if client != nil {
				t.Fatalf("NewClient(%s) returned a client alongside an error", test.name)
			}
		})
	}

	// The accepted case: a trailing slash is trimmed, so the store's own paths are appended to the base
	// rather than nested under a path the deployment did not mean to configure, and an unset timeout
	// takes the approved default instead of waiting forever (D6).
	client, err := NewClient(ClientConfig{Endpoint: "  https://store.invalid:8443/  ", TLS: clientTLS(t, pki, testIngressIdentity, 45)})
	if err != nil {
		t.Fatalf("NewClient of a complete configuration: %v", err)
	}
	if client.endpoint != "https://store.invalid:8443" {
		t.Fatalf("endpoint = %q, want the trimmed base URL", client.endpoint)
	}
	if client.http.Timeout != DefaultIngressTimeout {
		t.Fatalf("timeout = %v, want the default %v", client.http.Timeout, DefaultIngressTimeout)
	}
}
