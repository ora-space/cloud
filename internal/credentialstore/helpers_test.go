package credentialstore

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

const (
	testTenantID     = "3e9cad31-4a5f-4f6e-9d2b-8a7f6e5d4c33"
	testOwnerID      = "1c7a8b1f-2e3d-4d4c-9b0f-6e5d4c3b2a11"
	testProjectID    = "4fadbe42-5b6a-4a7f-8e3c-9b8a7f6e5d44"
	testBindingID    = "5abecf53-6c7b-4b8a-9f4d-ac9b8a7f6e55"
	testCredentialID = "6bcfd064-7d8c-4c9b-8a5e-bdac9b8a7f66"
	testRepository   = "https://github.com/ora-space/example"
	testOrigin       = "https://app.ora.example"
	// canaryToken is the secret every test submits; the log scan looks for it.
	canaryToken = "ghp_CANARY_d41d8cd98f00b204e9800998ecf8427e"
)

var testStart = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

// testClock is the injected clock; tests advance it instead of sleeping.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// keyringJSON builds a keyring document from id → key.
func keyringJSON(t *testing.T, primary string, keys map[string][]byte) []byte {
	t.Helper()
	doc := keyringFile{Primary: primary, Keys: map[string]string{}}
	for id, key := range keys {
		doc.Keys[id] = base64.StdEncoding.EncodeToString(key)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func mustKeyring(t *testing.T, primary string, keys map[string][]byte) *Keyring {
	t.Helper()
	ring, err := ParseKeyring(keyringJSON(t, primary, keys))
	if err != nil {
		t.Fatalf("ParseKeyring: %v", err)
	}
	return ring
}

// testPKI is an in-memory CA issuing the server certificate and SPIFFE client certificates.
type testPKI struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	pool   *x509.CertPool
	server tls.Certificate
}

func newPKI(t *testing.T) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	p := &testPKI{caCert: ca, caKey: key, pool: pool}
	p.server = p.issue(t, func(c *x509.Certificate) {
		c.DNSNames = []string{"localhost"}
		c.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	})
	return p
}

func (p *testPKI) issue(t *testing.T, shape func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	shape(template)
	der, err := x509.CreateCertificate(rand.Reader, template, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// client issues a client certificate carrying the given URI SANs.
func (p *testPKI) client(t *testing.T, uris ...string) tls.Certificate {
	t.Helper()
	return p.issue(t, func(c *x509.Certificate) {
		c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		for _, raw := range uris {
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			c.URIs = append(c.URIs, parsed)
		}
	})
}

// clientTLS trusts the test CA and, when given, presents cert.
func (p *testPKI) clientTLS(certs ...tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: p.pool, Certificates: certs}
}

// harness is one Store with all four listeners served over real TLS.
type harness struct {
	clock          *testClock
	root           string
	store          *Store
	server         *Server
	pki            *testPKI
	logs           *observer.ObservedLogs
	executionKey   ed25519.PrivateKey
	submissionKey  ed25519.PrivateKey
	redemption     *httptest.Server
	management     *httptest.Server
	key            *httptest.Server
	ingress        *httptest.Server
	executorCert   tls.Certificate
	managerCert    tls.Certificate
	kekCert        tls.Certificate
	executionPubs  []ed25519.PublicKey
	submissionPubs []ed25519.PublicKey
}

type harnessOption func(*harnessSetup)

type harnessSetup struct {
	root       string
	keyring    *Keyring
	identities Identities
	rate       int
	burst      int
	execPriv   ed25519.PrivateKey
	subPriv    ed25519.PrivateKey
}

func withRoot(root string) harnessOption { return func(s *harnessSetup) { s.root = root } }

func withKeyring(k *Keyring) harnessOption { return func(s *harnessSetup) { s.keyring = k } }

func withIdentities(i Identities) harnessOption { return func(s *harnessSetup) { s.identities = i } }

func withRate(rate, burst int) harnessOption {
	return func(s *harnessSetup) { s.rate, s.burst = rate, burst }
}

func withSigners(exec, sub ed25519.PrivateKey) harnessOption {
	return func(s *harnessSetup) { s.execPriv, s.subPriv = exec, sub }
}

func newSigner(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return private
}

func newHarness(t *testing.T, options ...harnessOption) *harness {
	t.Helper()
	setup := harnessSetup{
		root:       filepath.Join(t.TempDir(), "store"),
		identities: DefaultIdentities(),
		rate:       600, burst: 100,
	}
	for _, option := range options {
		option(&setup)
	}
	if setup.keyring == nil {
		setup.keyring = mustKeyring(t, "kek-1", map[string][]byte{"kek-1": randomKey(t)})
	}
	if setup.execPriv == nil {
		setup.execPriv, setup.subPriv = newSigner(t), newSigner(t)
	}
	clock := &testClock{now: testStart}
	store, err := OpenStore(StoreConfig{Root: setup.root, Keyring: setup.keyring, Now: clock.Now})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	core, logs := observer.New(zapcore.DebugLevel)
	h := &harness{
		clock: clock, root: setup.root, store: store, pki: newPKI(t), logs: logs,
		executionKey: setup.execPriv, submissionKey: setup.subPriv,
		executionPubs:  []ed25519.PublicKey{setup.execPriv.Public().(ed25519.PublicKey)},
		submissionPubs: []ed25519.PublicKey{setup.subPriv.Public().(ed25519.PublicKey)},
	}
	h.server, err = NewServer(&ServerOptions{
		Store: store, ExecutionKeys: h.executionPubs, SubmissionKeys: h.submissionPubs,
		Identities: setup.identities, AllowedOrigins: []string{testOrigin},
		RatePerMinute: setup.rate, RateBurst: setup.burst, Logger: zap.New(core),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	h.executorCert = h.pki.client(t, setup.identities.Redeemer)
	h.managerCert = h.pki.client(t, setup.identities.Manager)
	h.kekCert = h.pki.client(t, setup.identities.KeyOperator)
	mtls := func(handler http.Handler) *httptest.Server {
		srv := httptest.NewUnstartedServer(handler)
		srv.TLS = &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{h.pki.server},
			ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: h.pki.pool,
		}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv
	}
	h.redemption = mtls(h.server.RedemptionHandler())
	h.management = mtls(h.server.ManagementHandler())
	h.key = mtls(h.server.KeyHandler())
	h.ingress = httptest.NewUnstartedServer(h.server.IngressHandler())
	h.ingress.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{h.pki.server}}
	h.ingress.StartTLS()
	t.Cleanup(h.ingress.Close)
	return h
}

// httpClient returns a client trusting the test CA and presenting certs.
func (h *harness) httpClient(t *testing.T, certs ...tls.Certificate) *http.Client {
	t.Helper()
	transport := &http.Transport{TLSClientConfig: h.pki.clientTLS(certs...)}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}
}

func (h *harness) redemptionClient(t *testing.T, cert tls.Certificate) *RedemptionClient {
	t.Helper()
	c, err := NewRedemptionClient(&ClientConfig{BaseURL: h.redemption.URL, TLS: h.pki.clientTLS(cert)})
	if err != nil {
		t.Fatalf("NewRedemptionClient: %v", err)
	}
	return c
}

func (h *harness) managementClient(t *testing.T, cert tls.Certificate) *ManagementClient {
	t.Helper()
	c, err := NewManagementClient(&ClientConfig{BaseURL: h.management.URL, TLS: h.pki.clientTLS(cert)})
	if err != nil {
		t.Fatalf("NewManagementClient: %v", err)
	}
	return c
}

// submission returns a valid grant for testCredentialID at version 1, issued now.
func (h *harness) submission(mutate ...func(*cloneauth.Submission)) cloneauth.Submission {
	now := h.clock.Now()
	s := cloneauth.Submission{
		SubmissionID: uuid.NewString(), Purpose: cloneauth.SubmissionPurpose, TenantID: testTenantID,
		CredentialID: testCredentialID, OwnerUserID: testOwnerID, Attribution: cloneauth.AttributionPersonal,
		RepositoryScope: testRepository, ExpectedStoreVersion: 1, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute),
	}
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func (h *harness) signSubmission(t *testing.T, s cloneauth.Submission) string {
	t.Helper()
	token, err := cloneauth.SignSubmission(h.submissionKey, s)
	if err != nil {
		t.Fatalf("SignSubmission: %v", err)
	}
	return token
}

// authorization returns a valid publish authorization for testCredentialID version 1, issued now.
func (h *harness) authorization(mutate ...func(*cloneauth.Authorization)) cloneauth.Authorization {
	now := h.clock.Now()
	a := cloneauth.Authorization{
		AuthorizationID: uuid.NewString(), Purpose: cloneauth.PurposePublish, ExecutorIdentity: cloneauth.ExecutorIdentity,
		IssuedAt: now, ExpiresAt: now.Add(2 * time.Minute), OperationID: uuid.NewString(),
		TenantID: testTenantID, ProjectID: testProjectID, BindingID: testBindingID, Repository: testRepository,
		SourceRef: "refs/heads/workspaces/" + testProjectID + "/feature", SourceCommit: "0123456789abcdef0123456789abcdef01234567",
		TargetRef: "refs/heads/main", ExpectedOldOID: "0000000000000000000000000000000000000000",
		CredentialID: testCredentialID, CredentialVersion: 1,
	}
	for _, m := range mutate {
		m(&a)
	}
	return a
}

func (h *harness) signAuthorization(t *testing.T, a cloneauth.Authorization) string {
	t.Helper()
	token, err := cloneauth.Sign(h.executionKey, a)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return token
}

// put sends one ingress request. authorization "" sends no Authorization header; origin "" sends
// no Origin header.
func (h *harness) put(t *testing.T, credentialID, authorization, origin, body string) (int, map[string]string, http.Header) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPut, h.ingress.URL+"/v1/credentials/"+credentialID+"/versions", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", "Ora-Submission "+authorization)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	return do(t, h.httpClient(t), request)
}

func do(t *testing.T, client *http.Client, request *http.Request) (int, map[string]string, http.Header) {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded := map[string]string{}
	if len(raw) > 0 {
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatalf("response is not JSON: %q", raw)
		}
		for k, v := range generic {
			b, _ := json.Marshal(v)
			decoded[k] = string(bytes.Trim(b, `"`))
		}
	}
	return response.StatusCode, decoded, response.Header
}

// storeVersion submits canaryToken at version and asserts success.
func (h *harness) storeVersion(t *testing.T, version int64) cloneauth.Submission {
	t.Helper()
	grant := h.submission(func(s *cloneauth.Submission) { s.ExpectedStoreVersion = version })
	body := `{"token":"` + canaryToken + `"}`
	status, out, _ := h.put(t, testCredentialID, h.signSubmission(t, grant), testOrigin, body)
	if status != http.StatusOK || out["storeVersion"] != itoa(version) {
		t.Fatalf("submit version %d = %d %v", version, status, out)
	}
	return grant
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// post sends a POST with the given client certificate and body.
func (h *harness) post(t *testing.T, target string, cert tls.Certificate, body string) (int, map[string]string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, target, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	status, out, _ := do(t, h.httpClient(t, cert), request)
	return status, out
}

// get sends a GET with the given client certificate.
func (h *harness) get(t *testing.T, target string, cert tls.Certificate) (int, map[string]string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, out, _ := do(t, h.httpClient(t, cert), request)
	return status, out
}
