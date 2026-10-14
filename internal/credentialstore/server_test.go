package credentialstore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The three identities the deployment fixes; tests reuse exactly this spelling so a certificate and
// the table it must match are built from one source (D3).
const (
	testServerIdentity   = "spiffe://ora/cloud/credential-store"
	testIngressIdentity  = "spiffe://ora/cloud/credential-ingress"
	testResolverIdentity = "spiffe://ora/node/credential-executor"
	testOperatorIdentity = "spiffe://ora/ops/kek-operator"
	testServerName       = "cloud.test"
)

func testIdentityConfig() IdentityConfig {
	return IdentityConfig{
		IngressWriter:  testIngressIdentity,
		BrokerResolver: testResolverIdentity,
		KEKOperator:    testOperatorIdentity,
	}
}

// testPKI is an in-memory certificate authority. Its key and every certificate it issues live only in
// process memory so generated key material never reaches a durable fixture.
type testPKI struct {
	ca    *x509.Certificate
	caDER []byte
	caKey ed25519.PrivateKey
	roots *x509.CertPool
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "credential store test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return &testPKI{ca: parsed, caDER: der, caKey: key, roots: roots}
}

// issue mints one leaf certificate. A server leaf carries the test DNS name; a client leaf carries
// the SPIFFE URI the identity table resolves (D3).
func (p *testPKI) issue(t *testing.T, serial int64, identity string, server bool) tls.Certificate {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	uri, err := url.Parse(identity)
	if err != nil {
		t.Fatalf("parse identity %q: %v", identity, err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{uri},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		leaf.DNSNames = []string{testServerName}
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, p.ca, pub, p.caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der, p.caDER}, PrivateKey: key}
}

// client builds an HTTP client that trusts the test CA and, when cert is non-nil, presents it as the
// service identity.
func (p *testPKI) client(t *testing.T, cert *tls.Certificate) *http.Client {
	t.Helper()
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: p.roots, ServerName: testServerName}
	if cert != nil {
		config.Certificates = []tls.Certificate{*cert}
	}
	transport := &http.Transport{TLSClientConfig: config}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

// newTestServer mounts the Store's handler behind a real mutual-TLS listener and returns it with the
// PKI used to reach it.
func newTestServer(t *testing.T, store *Store, logger *zap.Logger) (*httptest.Server, *testPKI) {
	t.Helper()
	pki := newTestPKI(t)
	serverCert := pki.issue(t, 1, testServerIdentity, true)
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    pki.roots,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	server, err := NewServer(store, ServerConfig{TLS: tlsConfig, Identities: testIdentityConfig(), Log: logger})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	instance := httptest.NewUnstartedServer(server.Handler())
	// The no-certificate case is an expected handshake refusal; its server-side line is not test output.
	instance.Config.ErrorLog = log.New(io.Discard, "", 0)
	instance.TLS = tlsConfig
	instance.StartTLS()
	t.Cleanup(instance.Close)
	return instance, pki
}

// bufferedLogger captures every log line in memory so a test can assert on the raw text.
func bufferedLogger() (*zap.Logger, *bytes.Buffer) {
	buffer := &bytes.Buffer{}
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(buffer), zapcore.DebugLevel)
	return zap.New(core), buffer
}

// doJSON performs one request and returns the status and raw body.
func doJSON(t *testing.T, client *http.Client, method, target, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, target, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response.StatusCode, raw
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return decoded
}

// TestNewIdentitiesValidatesConfiguration proves all three identities must be present, distinct, and
// well-formed, because a Store missing one would accept no caller while looking healthy and reusing
// one identity for two roles would collapse the capability separation (D3).
func TestNewIdentitiesValidatesConfiguration(t *testing.T) {
	valid := testIdentityConfig()
	cases := []struct {
		name    string
		config  IdentityConfig
		wantErr bool
	}{
		{"valid", valid, false},
		{"blank ingress", IdentityConfig{IngressWriter: "  ", BrokerResolver: valid.BrokerResolver, KEKOperator: valid.KEKOperator}, true},
		{"blank resolver", IdentityConfig{IngressWriter: valid.IngressWriter, BrokerResolver: "", KEKOperator: valid.KEKOperator}, true},
		{"blank operator", IdentityConfig{IngressWriter: valid.IngressWriter, BrokerResolver: valid.BrokerResolver, KEKOperator: ""}, true},
		{"duplicate roles", IdentityConfig{IngressWriter: valid.IngressWriter, BrokerResolver: valid.IngressWriter, KEKOperator: valid.KEKOperator}, true},
		{"not spiffe", IdentityConfig{IngressWriter: "https://ora/cloud/credential-ingress", BrokerResolver: valid.BrokerResolver, KEKOperator: valid.KEKOperator}, true},
		{"no trust domain", IdentityConfig{IngressWriter: "spiffe:///cloud/ingress", BrokerResolver: valid.BrokerResolver, KEKOperator: valid.KEKOperator}, true},
		{"too many segments", IdentityConfig{IngressWriter: "spiffe://ora/cloud/ingress/extra", BrokerResolver: valid.BrokerResolver, KEKOperator: valid.KEKOperator}, true},
		{"with query", IdentityConfig{IngressWriter: "spiffe://ora/cloud/ingress?role=writer", BrokerResolver: valid.BrokerResolver, KEKOperator: valid.KEKOperator}, true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewIdentities(test.config)
			if test.wantErr && err == nil {
				t.Fatalf("NewIdentities(%s) accepted an invalid configuration", test.name)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("NewIdentities(%s) rejected a valid configuration: %v", test.name, err)
			}
		})
	}
}

// TestIdentitiesCapabilityMapping proves each identity yields exactly one capability drawn from the
// fixed table, and an identity nobody holds is reported as unknown rather than anonymous (D3).
func TestIdentitiesCapabilityMapping(t *testing.T) {
	identities, err := NewIdentities(testIdentityConfig())
	if err != nil {
		t.Fatalf("NewIdentities: %v", err)
	}
	cases := []struct {
		identity string
		want     Capability
	}{
		{testIngressIdentity, CapabilityWrite},
		{testResolverIdentity, CapabilityResolve},
		{testOperatorIdentity, CapabilityKeys},
	}
	for _, test := range cases {
		got, known := identities.Capability(test.identity)
		if !known {
			t.Fatalf("identity %q is unknown to the table", test.identity)
		}
		if got != test.want {
			t.Fatalf("identity %q capability = %q, want %q", test.identity, got, test.want)
		}
	}
	if got, known := identities.Capability("spiffe://ora/unknown/peer"); known {
		t.Fatalf("unknown identity was given capability %q", got)
	}
}

// TestPresentedIdentityRequiresVerifiedPeer proves a certificate that did not verify, or names an
// identity nobody holds, is rejected rather than treated as anonymous (D3).
func TestPresentedIdentityRequiresVerifiedPeer(t *testing.T) {
	identities, err := NewIdentities(testIdentityConfig())
	if err != nil {
		t.Fatalf("NewIdentities: %v", err)
	}
	known, err := url.Parse(testResolverIdentity)
	if err != nil {
		t.Fatalf("parse known identity: %v", err)
	}
	unknown, err := url.Parse("spiffe://ora/node/unknown")
	if err != nil {
		t.Fatalf("parse unknown identity: %v", err)
	}
	verified := [][]*x509.Certificate{{{}}}
	cases := []struct {
		name    string
		state   *tls.ConnectionState
		want    string
		wantErr bool
	}{
		{"nil state", nil, "", true},
		{"no verified chain", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{known}}}}, "", true},
		{"no peer certificate", &tls.ConnectionState{VerifiedChains: verified}, "", true},
		{"unknown identity", &tls.ConnectionState{VerifiedChains: verified, PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{unknown}}}}, "", true},
		{"known identity", &tls.ConnectionState{VerifiedChains: verified, PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{unknown, known}}}}, testResolverIdentity, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := identities.PresentedIdentity(test.state)
			if test.wantErr {
				if err == nil {
					t.Fatalf("PresentedIdentity(%s) returned %q, want an error", test.name, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PresentedIdentity(%s) = %v", test.name, err)
			}
			if got != test.want {
				t.Fatalf("PresentedIdentity(%s) = %q, want %q", test.name, got, test.want)
			}
		})
	}
}

// TestServerRefusesClientWithoutCertificate proves an anonymous peer never reaches the Store. With the
// mandated RequireAndVerifyClientCert policy the refusal is the TLS handshake, so the request cannot
// succeed; if a future policy returned an HTTP response instead it must still be a 401 (D3, D6).
func TestServerRefusesClientWithoutCertificate(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	anonymous := pki.client(t, nil)
	response, err := anonymous.Post(server.URL+"/v1/credentials", "application/json", strings.NewReader("{}"))
	if response != nil {
		defer response.Body.Close()
	}
	if err == nil && response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a client with no certificate reached the handler with status %d, want a TLS refusal or 401", response.StatusCode)
	}
}

// TestServerRefusesUnknownClientIdentity proves a certificate that verifies but names an identity the
// deployment never issued is refused with 401, not treated as a caller (D3).
func TestServerRefusesUnknownClientIdentity(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	unknown := pki.issue(t, 2, "spiffe://ora/node/not-in-the-table", false)
	status, raw := doJSON(t, pki.client(t, &unknown), http.MethodPost, server.URL+"/v1/credentials", `{"credentialId":"`+testCredentialID(30)+`","token":"t"}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown identity status = %d, want 401", status)
	}
	if code := decodeBody(t, raw)["code"]; code != "service_identity_required" {
		t.Fatalf("unknown identity code = %v, want service_identity_required", code)
	}
}

// TestServerEnforcesCapabilitySeparation proves each identity reaches only the operations its
// capability allows, so one compromised service identity cannot both read and erase every secret
// (D3).
func TestServerEnforcesCapabilitySeparation(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	id := testCredentialID(31)
	writeBody := `{"credentialId":"` + id + `","token":"token"}`
	clients := map[string]*http.Client{}
	for identity, serial := range map[string]int64{testIngressIdentity: 10, testResolverIdentity: 11, testOperatorIdentity: 12} {
		cert := pki.issue(t, serial, identity, false)
		clients[identity] = pki.client(t, &cert)
	}
	cases := []struct {
		name       string
		identity   string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"ingress writes", testIngressIdentity, http.MethodPost, "/v1/credentials", writeBody, http.StatusOK, ""},
		{"ingress resolve denied", testIngressIdentity, http.MethodPost, "/v1/credentials/" + id + "/resolve", "", http.StatusForbidden, "capability_denied"},
		{"ingress resolve-mirror denied", testIngressIdentity, http.MethodPost, "/v1/credentials/" + id + "/resolve-mirror", "", http.StatusForbidden, "capability_denied"},
		{"resolver write denied", testResolverIdentity, http.MethodPost, "/v1/credentials", writeBody, http.StatusForbidden, "capability_denied"},
		{"resolver rotate denied", testResolverIdentity, http.MethodPost, "/v1/credentials/" + id + "/rotate", `{"token":"next"}`, http.StatusForbidden, "capability_denied"},
		{"resolver revoke denied", testResolverIdentity, http.MethodPost, "/v1/credentials/" + id + "/revoke", "{}", http.StatusForbidden, "capability_denied"},
		{"operator write denied", testOperatorIdentity, http.MethodPost, "/v1/credentials", writeBody, http.StatusForbidden, "capability_denied"},
		{"resolver resolve unimplemented", testResolverIdentity, http.MethodPost, "/v1/credentials/" + id + "/resolve", "", http.StatusConflict, "authorization_verification_unavailable"},
		{"resolver resolve-mirror unimplemented", testResolverIdentity, http.MethodPost, "/v1/credentials/" + id + "/resolve-mirror", "", http.StatusConflict, "authorization_verification_unavailable"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, raw := doJSON(t, clients[test.identity], test.method, server.URL+test.path, test.body)
			if status != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", status, test.wantStatus, raw)
			}
			if test.wantCode == "" {
				return
			}
			if code := decodeBody(t, raw)["code"]; code != test.wantCode {
				t.Fatalf("code = %v, want %q", code, test.wantCode)
			}
		})
	}
}

// TestServerWriteIsIdempotentOverTransport proves the idempotency key survives the real transport: a
// repeated write of the same credential returns the original secret_ref instead of a second secret
// (D1, D4).
func TestServerWriteIsIdempotentOverTransport(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	ingress := pki.issue(t, 20, testIngressIdentity, false)
	client := pki.client(t, &ingress)
	id := testCredentialID(32)
	body := `{"credentialId":"` + id + `","token":"token"}`
	status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials", body)
	if status != http.StatusOK {
		t.Fatalf("first write status = %d, want 200 (body %s)", status, raw)
	}
	first, _ := decodeBody(t, raw)["secretRef"].(string)
	if !strings.HasPrefix(first, secretRefPrefix) {
		t.Fatalf("returned secret_ref %q lacks the store prefix", first)
	}
	status, raw = doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials", body)
	if status != http.StatusOK {
		t.Fatalf("repeated write status = %d, want 200 (body %s)", status, raw)
	}
	if again, _ := decodeBody(t, raw)["secretRef"].(string); again != first {
		t.Fatalf("repeated write returned %q, want the original %q", again, first)
	}
}

// TestServerRefusesCallerSuppliedReference proves the write endpoint rejects a caller-supplied
// secret_ref instead of honoring it, since D1 forbids the browser, user, or deployer choosing one
// (D1).
func TestServerRefusesCallerSuppliedReference(t *testing.T) {
	fixture := newStoreFixture(t)
	server, pki := newTestServer(t, fixture.store, nil)
	ingress := pki.issue(t, 21, testIngressIdentity, false)
	body := `{"credentialId":"` + testCredentialID(33) + `","token":"token","secretRef":"cs_caller-chosen"}`
	status, raw := doJSON(t, pki.client(t, &ingress), http.MethodPost, server.URL+"/v1/credentials", body)
	if status != http.StatusBadRequest {
		t.Fatalf("status with a caller-supplied reference = %d, want 400 (body %s)", status, raw)
	}
	if code := decodeBody(t, raw)["code"]; code != "invalid_request" {
		t.Fatalf("code = %v, want invalid_request", code)
	}
}

// TestServerDoesNotRequireTLSToEmitAnAnonymousRefusal proves the handler itself fails closed when no
// TLS layer established an identity, so the 401 branch is not silently dead (D3, D6).
func TestServerDoesNotRequireTLSToEmitAnAnonymousRefusal(t *testing.T) {
	fixture := newStoreFixture(t)
	pki := newTestPKI(t)
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pki.issue(t, 1, testServerIdentity, true)},
		ClientCAs:    pki.roots,
		ClientAuth:   tls.RequireAndVerifyClientCert,
	}
	server, err := NewServer(fixture.store, ServerConfig{TLS: tlsConfig, Identities: testIdentityConfig(), Log: nil})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	plaintext := httptest.NewServer(server.Handler())
	t.Cleanup(plaintext.Close)
	status, raw := doJSON(t, plaintext.Client(), http.MethodPost, plaintext.URL+"/v1/credentials", "{}")
	if status != http.StatusUnauthorized {
		t.Fatalf("plaintext request status = %d, want 401", status)
	}
	if code := decodeBody(t, raw)["code"]; code != "service_identity_required" {
		t.Fatalf("plaintext request code = %v, want service_identity_required", code)
	}
}

// TestServerAuditLinesExcludeSecrets proves the audit lines name the operation, credential id,
// identity and result while carrying no token, ciphertext, wrapped data key, or key material (D7).
func TestServerAuditLinesExcludeSecrets(t *testing.T) {
	fixture := newStoreFixture(t)
	log, buffer := bufferedLogger()
	server, pki := newTestServer(t, fixture.store, log)
	ingress := pki.issue(t, 30, testIngressIdentity, false)
	client := pki.client(t, &ingress)
	id := testCredentialID(34)
	const writeToken, rotateToken = "ghp_AuditMustNotLeakWrite_9f8e7d6c5b4a", "ghp_AuditMustNotLeakRotate_1a2b3c4d5e6f"

	if status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials", `{"credentialId":"`+id+`","token":"`+writeToken+`"}`); status != http.StatusOK {
		t.Fatalf("write status = %d, want 200 (body %s)", status, raw)
	}
	afterWrite := readRecord(t, fixture.store, id)
	if status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/rotate", `{"token":"`+rotateToken+`"}`); status != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200 (body %s)", status, raw)
	}
	afterRotate := readRecord(t, fixture.store, id)
	// A refusal on the store's own decision path, which must still name the operation and result.
	if status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials", `{"credentialId":"`+id+`","token":"different"}`); status != http.StatusConflict {
		t.Fatalf("conflicting write status = %d, want 409 (body %s)", status, raw)
	}
	// A capability refusal, which must name the identity and reason without any secret.
	resolverCert := pki.issue(t, 31, testResolverIdentity, false)
	if status, raw := doJSON(t, pki.client(t, &resolverCert), http.MethodPost, server.URL+"/v1/credentials", `{"credentialId":"`+id+`","token":"t"}`); status != http.StatusForbidden {
		t.Fatalf("resolver write status = %d, want 403 (body %s)", status, raw)
	}
	if status, raw := doJSON(t, client, http.MethodPost, server.URL+"/v1/credentials/"+id+"/revoke", "{}"); status != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200 (body %s)", status, raw)
	}

	text := buffer.String()
	materials := []material{afterWrite.Current, afterRotate.Current}
	for _, entry := range afterRotate.Previous {
		materials = append(materials, entry.Material)
	}
	forbidden := []string{writeToken, rotateToken}
	for _, m := range materials {
		forbidden = append(forbidden, m.Ciphertext, m.WrappedDEK, m.Nonce, m.DEKNonce, m.Fingerprint)
	}
	for _, entry := range fixture.keys {
		forbidden = append(forbidden, base64.StdEncoding.EncodeToString(entry))
	}
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("audit log contains secret material %q", secret)
		}
	}
	for _, required := range []string{`"operation":"write"`, `"operation":"rotate"`, `"operation":"revoke"`, id, testIngressIdentity, `"result":"succeeded"`, "credential_conflict", "capability_denied"} {
		if !strings.Contains(text, required) {
			t.Fatalf("audit log is missing required field %q; log was:\n%s", required, text)
		}
	}
}
