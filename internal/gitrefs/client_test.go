package gitrefs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

const (
	testTenant  = "11111111-1111-4111-8111-111111111111"
	testProject = "22222222-2222-4222-8222-222222222222"
	testRef     = "refs/ora/upstream/33333333-3333-4333-8333-333333333333/main"
	testCommit  = "1111111111111111111111111111111111111111"
)

// tlsFiles writes a self-signed CA and a client pair, which is all New needs to build its transport.
func tlsFiles(t *testing.T) (caFile, certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "git-refs-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling a key: %v", err)
	}
	caFile = filepath.Join(dir, "ca.pem")
	certFile = filepath.Join(dir, "client.pem")
	keyFile = filepath.Join(dir, "client.key")
	write := func(path string, block *pem.Block) {
		t.Helper()
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	write(caFile, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	write(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	write(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caFile, certFile, keyFile
}

func TestNewBuildsAMutuallyAuthenticatedTLS13Client(t *testing.T) {
	caFile, certFile, keyFile := tlsFiles(t)
	c, err := New(Config{BaseURL: "https://git-server:8446/", CAFile: caFile, CertFile: certFile, KeyFile: keyFile})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if c.base != "https://git-server:8446" {
		t.Fatalf("base = %q, want the trailing slash trimmed", c.base)
	}
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want an HTTP transport", c.http.Transport)
	}
	if transport.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("minimum TLS version = %d, want 1.3", transport.TLSClientConfig.MinVersion)
	}
	if len(transport.TLSClientConfig.Certificates) != 1 {
		t.Fatalf("client identities = %d, want the pinned leaf", len(transport.TLSClientConfig.Certificates))
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("the client trusts no CA")
	}
	if c.http.Timeout != 10*time.Second {
		t.Fatalf("timeout = %v, want the default", c.http.Timeout)
	}
	// A redirect must never move a ref query to another origin.
	if err := c.http.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy = %v, want ErrUseLastResponse", err)
	}
}

func TestNewRejectsUnusableConfiguration(t *testing.T) {
	caFile, certFile, keyFile := tlsFiles(t)
	missing := filepath.Join(t.TempDir(), "absent.pem")
	garbage := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("writing the garbage file: %v", err)
	}
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"a plaintext origin", Config{BaseURL: "http://git-server:8446", CAFile: caFile, CertFile: certFile, KeyFile: keyFile}},
		{"no scheme", Config{BaseURL: "git-server:8446", CAFile: caFile, CertFile: certFile, KeyFile: keyFile}},
		{"no host", Config{BaseURL: "https://", CAFile: caFile, CertFile: certFile, KeyFile: keyFile}},
		{"a path", Config{BaseURL: "https://git-server:8446/internal", CAFile: caFile, CertFile: certFile, KeyFile: keyFile}},
		{"an unparseable origin", Config{BaseURL: "https://git-server:8446/%zz", CAFile: caFile, CertFile: certFile, KeyFile: keyFile}},
		{"a missing CA", Config{BaseURL: "https://git-server:8446", CAFile: missing, CertFile: certFile, KeyFile: keyFile}},
		{"a CA that is not a certificate", Config{BaseURL: "https://git-server:8446", CAFile: garbage, CertFile: certFile, KeyFile: keyFile}},
		{"a missing client identity", Config{BaseURL: "https://git-server:8446", CAFile: caFile, CertFile: missing, KeyFile: keyFile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("New accepted a configuration it cannot query with")
			}
		})
	}
}

func TestNewWithHTTPRequiresBothArguments(t *testing.T) {
	if _, err := NewWithHTTP("", &http.Client{}); err == nil {
		t.Fatal("an empty base URL was accepted")
	}
	if _, err := NewWithHTTP("https://git-server:8446", nil); err == nil {
		t.Fatal("a nil HTTP client was accepted")
	}
	c, err := NewWithHTTP("https://git-server:8446/", &http.Client{})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if c.base != "https://git-server:8446" {
		t.Fatalf("base = %q, want the trailing slash trimmed", c.base)
	}
}

// queryServer answers one ref query with status and body, checking the request the client sent.
func queryServer(t *testing.T, status int, body string, check func(*http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/internal/v1/refs" {
			t.Errorf("path = %s, want /internal/v1/refs", r.URL.Path)
		}
		if check != nil {
			check(r)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveGitRefAsksForOneRefAndReturnsItsCommit(t *testing.T) {
	srv := queryServer(t, http.StatusOK, `{"commit":"`+testCommit+`"}`, func(r *http.Request) {
		q := r.URL.Query()
		if q.Get("tenantId") != testTenant || q.Get("projectId") != testProject || q.Get("ref") != testRef {
			t.Errorf("query = %v, want the tenant, project and ref", q)
		}
	})
	c, err := NewWithHTTP(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	commit, found, err := c.ResolveGitRef(context.Background(), testTenant, testProject, testRef)
	if err != nil || !found || commit != testCommit {
		t.Fatalf("ResolveGitRef = (%q, %v, %v), want (%q, true, nil)", commit, found, err, testCommit)
	}
}

func TestResolveGitRefOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantFound bool
		wantErr   bool
	}{
		{name: "a missing ref is not an error", status: http.StatusNotFound, body: `{"code":"ref_not_found"}`},
		{name: "another 404 is an error", status: http.StatusNotFound, body: `{"code":"project_not_found"}`, wantErr: true},
		{name: "a 404 without a fault is an error", status: http.StatusNotFound, body: `<html>`, wantErr: true},
		{name: "an abbreviated commit is refused", status: http.StatusOK, body: `{"commit":"1111111"}`, wantErr: true},
		{name: "a missing commit is refused", status: http.StatusOK, body: `{}`, wantErr: true},
		{name: "an uppercase commit is refused", status: http.StatusOK, body: `{"commit":"111111111111111111111111111111111111111A"}`, wantErr: true},
		{name: "invalid JSON is an error", status: http.StatusOK, body: `{"commit":`, wantErr: true},
		{name: "a server error is an error", status: http.StatusInternalServerError, body: `{"code":"internal"}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := queryServer(t, tc.status, tc.body, nil)
			c, err := NewWithHTTP(srv.URL, srv.Client())
			if err != nil {
				t.Fatalf("building the client: %v", err)
			}
			commit, found, err := c.ResolveGitRef(context.Background(), testTenant, testProject, testRef)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error = %v", err, tc.wantErr)
			}
			if found != tc.wantFound || commit != "" {
				t.Fatalf("ResolveGitRef = (%q, %v), want (\"\", %v)", commit, found, tc.wantFound)
			}
		})
	}
}

func TestResolveGitRefRefusesAnOversizedAnswer(t *testing.T) {
	srv := queryServer(t, http.StatusOK, strings.Repeat("x", maxResponse+1), nil)
	c, err := NewWithHTTP(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if _, _, err := c.ResolveGitRef(context.Background(), testTenant, testProject, testRef); err == nil {
		t.Fatal("an oversized answer was accepted")
	}
}

func TestQueriesHonourTheCallerContext(t *testing.T) {
	srv := queryServer(t, http.StatusOK, `{"commit":"`+testCommit+`"}`, nil)
	c, err := NewWithHTTP(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.ResolveGitRef(ctx, testTenant, testProject, testRef); err == nil {
		t.Fatal("a cancelled resolve reported success")
	}
	if _, err := c.ListGitRefs(ctx, testTenant, testProject, "refs/ora/upstream/"); err == nil {
		t.Fatal("a cancelled listing reported success")
	}
}

func TestListGitRefsReturnsTheRefsUnderOnePrefix(t *testing.T) {
	body := `{"refs":[{"ref":"` + testRef + `","commit":"` + testCommit + `"},` +
		`{"ref":"refs/ora/upstream/33333333-3333-4333-8333-333333333333/release","commit":"2222222222222222222222222222222222222222"}]}`
	srv := queryServer(t, http.StatusOK, body, func(r *http.Request) {
		if got := r.URL.Query().Get("prefix"); got != "refs/ora/upstream/" {
			t.Errorf("prefix = %q, want refs/ora/upstream/", got)
		}
	})
	c, err := NewWithHTTP(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	refs, err := c.ListGitRefs(context.Background(), testTenant, testProject, "refs/ora/upstream/")
	if err != nil {
		t.Fatalf("ListGitRefs: %v", err)
	}
	want := []core.GitRefCommit{
		{Ref: testRef, Commit: testCommit},
		{Ref: "refs/ora/upstream/33333333-3333-4333-8333-333333333333/release", Commit: "2222222222222222222222222222222222222222"},
	}
	if len(refs) != len(want) {
		t.Fatalf("listed %d refs, want %d", len(refs), len(want))
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("ref %d = %+v, want %+v", i, refs[i], want[i])
		}
	}
}

func TestListGitRefsRejectsAnAnswerOutsideThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "a ref outside the prefix", status: http.StatusOK, body: `{"refs":[{"ref":"refs/heads/main","commit":"` + testCommit + `"}]}`},
		{name: "an invalid commit", status: http.StatusOK, body: `{"refs":[{"ref":"` + testRef + `","commit":"nope"}]}`},
		{name: "a missing ref", status: http.StatusOK, body: `{"refs":[{"commit":"` + testCommit + `"}]}`},
		{name: "a not-found answer", status: http.StatusNotFound, body: `{"code":"ref_not_found"}`},
		{name: "invalid JSON", status: http.StatusOK, body: `{"refs":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := queryServer(t, tc.status, tc.body, nil)
			c, err := NewWithHTTP(srv.URL, srv.Client())
			if err != nil {
				t.Fatalf("building the client: %v", err)
			}
			if _, err := c.ListGitRefs(context.Background(), testTenant, testProject, "refs/ora/upstream/"); err == nil {
				t.Fatal("an unusable listing was accepted")
			}
		})
	}
}

func TestListGitRefsOnAnEmptyNamespace(t *testing.T) {
	srv := queryServer(t, http.StatusOK, `{"refs":[]}`, nil)
	c, err := NewWithHTTP(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	refs, err := c.ListGitRefs(context.Background(), testTenant, testProject, "refs/ora/upstream/")
	if err != nil {
		t.Fatalf("ListGitRefs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("listed %d refs, want none", len(refs))
	}
}

func TestAnUnreachableServerIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	c, err := NewWithHTTP(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if _, _, err := c.ResolveGitRef(context.Background(), testTenant, testProject, testRef); err == nil {
		t.Fatal("an unreachable server was reported as a missing ref")
	}
}
