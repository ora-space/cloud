package modelgateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func grantCertificates(t *testing.T) (*tls.Config, *x509.CertPool, func(string, x509.ExtKeyUsage) tls.Certificate) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, root, root, public, private)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	serial := int64(1)
	issue := func(identity string, usage x509.ExtKeyUsage) tls.Certificate {
		t.Helper()
		serial++
		pub, key, genErr := ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			t.Fatal(genErr)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, NotBefore: root.NotBefore, NotAfter: root.NotAfter}
		if identity != "" {
			uri, parseErr := url.Parse(identity)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			leaf.URIs = []*url.URL{uri}
		}
		certificate, signErr := x509.CreateCertificate(rand.Reader, leaf, root, pub, private)
		if signErr != nil {
			t.Fatal(signErr)
		}
		return tls.Certificate{Certificate: [][]byte{certificate}, PrivateKey: key}
	}
	server := issue("", x509.ExtKeyUsageServerAuth)
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{server}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, pool, issue
}

func TestGrantBoundaryRequiresDedicatedPurposeAndNeverPersistsRawToken(t *testing.T) {
	c := testCipher(t)
	policy := &fakePolicy{active: true, grant: core.ModelGrant{ID: uuid.NewString(), ExpiresAt: time.Now().Add(15 * time.Minute), Protocol: "openai-completions", Model: core.ModelDefinition{ID: "vendor/model"}}}
	s := testService(t, policy, c, http.DefaultClient)
	configuration, pool, issue := grantCertificates(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(s.GrantHandler))
	server.TLS = configuration
	server.StartTLS()
	defer server.Close()
	tenant, workspace := uuid.NewString(), uuid.NewString()
	identities := []struct {
		name, uri string
		usage     x509.ExtKeyUsage
		want      int
	}{{"model", "spiffe://ora.local/model-access/tenant/" + tenant + "/runtime/" + workspace + "/generation/1", x509.ExtKeyUsageClientAuth, 201}, {"controller", "spiffe://ora.local/controller/local-controller", x509.ExtKeyUsageClientAuth, 401}, {"old-wss", "spiffe://ora.local/tenant/" + tenant + "/runtime/" + workspace + "/generation/1", x509.ExtKeyUsageServerAuth, 0}}
	for _, test := range identities {
		t.Run(test.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{issue(test.uri, test.usage)}}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			r, err := http.NewRequest("POST", server.URL+"/internal/v1/model-grants", strings.NewReader(`{"bindingId":"`+uuid.NewString()+`","executionId":"`+uuid.NewString()+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/json")
			response, err := client.Do(r)
			if test.want == 0 {
				if err == nil {
					response.Body.Close()
					t.Fatal("server-only WSS identity reached client endpoint")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("status %d expected %d", response.StatusCode, test.want)
			}
			if test.want == 201 {
				var body struct{ Token, GrantID, ProxyBaseURL string }
				if err = json.NewDecoder(response.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if len(body.Token) < 32 || body.Token == policy.issuedDigest || policy.issuedDigest != tokenDigest(body.Token) {
					t.Fatal("raw token or invalid digest persisted")
				}
				if body.ProxyBaseURL != "https://models.example.invalid:8443/runtime/openai/v1" {
					t.Fatal("caller selected proxy URL")
				}
			}
		})
	}
}
