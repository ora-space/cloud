package modelgateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/egress"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// Bind a real non-loopback interface: production denies loopback even in the whitelist.
// The test's synthetic deployment topology excludes this external fixture interface.
func enterpriseTLS(t *testing.T, handler http.Handler) (*httptest.Server, string, string) {
	t.Helper()
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	var host string
	for _, raw := range interfaces {
		prefix, err := netip.ParsePrefix(raw.String())
		if err == nil {
			a := prefix.Addr().Unmap()
			if a.Is4() && a.IsGlobalUnicast() && !a.IsLoopback() {
				host = a.String()
				break
			}
		}
	}
	if host == "" {
		t.Fatal("enterprise transport test needs a non-loopback IPv4 interface")
	}
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewUnstartedServer(handler)
	_ = s.Listener.Close()
	s.Listener = l
	s.StartTLS()
	t.Cleanup(s.Close)
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return s, host, port
}

func enterpriseClient(t *testing.T, s *httptest.Server, ip, port string) (*http.Client, *egress.Policy, string) {
	t.Helper()
	host := s.Certificate().DNSNames[0]
	raw := fmt.Sprintf(`{"schemaVersion":1,"mode":"enterprise","targets":[{"host":%q,"port":%s,"allowPrivate":true,"staticAddresses":[%q]}],"topology":{"clusterCidrs":["192.0.2.0/24"],"hostAddresses":["192.0.2.1"]}}`, host, port, ip)
	p, err := egress.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewUpstreamClient(UpstreamConfig{Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	trust := x509.NewCertPool()
	trust.AddCert(s.Certificate())
	client.Transport.(*upstreamTransport).TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: trust}
	t.Cleanup(client.CloseIdleConnections)
	return client, p, "https://" + net.JoinHostPort(host, port)
}

func TestEnterpriseModelAndAssetShareTransportPolicy(t *testing.T) {
	var modelCalls, assetCalls atomic.Int32
	secret := randomSecret(t)
	packageBytes := []byte("verified package bytes")
	const query = "signature=a%2Fb%2Bz&key=two"
	s, ip, port := enterpriseTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName == "" {
			t.Error("missing original TLS hostname")
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			modelCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+secret {
				t.Error("model credential missing")
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"shared-egress\",\"tool_calls\":[{\"index\":0,\"id\":\"t1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n")
		case "/package.orax":
			assetCalls.Add(1)
			if r.URL.RawQuery != query || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
				t.Error("asset URL or credential boundary changed")
			}
			_, _ = w.Write(packageBytes)
		case "/redirect":
			http.Redirect(w, r, "https://not-listed.invalid", http.StatusFound)
		default:
			t.Error("unexpected path")
		}
	}))
	client, policy, base := enterpriseClient(t, s, ip, port)
	// An environment proxy must never receive either request.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	cipher := testCipher(t)
	id := uuid.NewString()
	sealed, err := cipher.Seal(id, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakePolicy{active: true, grant: core.ModelGrant{CredentialID: id, CredentialKeyID: "test-v1", Ciphertext: sealed, Protocol: "openai-completions", BaseURL: base + "/v1", AuthMode: "bearer", Model: core.ModelDefinition{ID: "vendor/model"}}}
	service := testService(t, store, cipher, client)
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"vendor/model","stream":true,"messages":[]}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+randomSecret(t))
		w := httptest.NewRecorder()
		service.ModelHandler(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "tool_calls") || strings.Contains(w.Body.String(), secret) {
			t.Fatalf("model response status %d", w.Code)
		}
	}
	digest := sha256.Sum256(packageBytes)
	fetch := workflowasset.NewFetcher(nil, client.Transport)
	for range 2 {
		data, err := fetch.Fetch(t.Context(), base+"/package.orax?"+query, hex.EncodeToString(digest[:]))
		if err != nil || string(data) != string(packageBytes) {
			t.Fatalf("asset fetch %v", err)
		}
	}
	if modelCalls.Load() != 2 || assetCalls.Load() != 2 {
		t.Fatal("missing requests")
	}
	if _, err = fetch.Fetch(t.Context(), "https://unlisted.example.invalid/package", hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("unlisted asset accepted")
	}
	store.mu.Lock()
	store.grant.BaseURL = "https://unlisted.example.invalid/v1"
	store.mu.Unlock()
	r := httptest.NewRequest(http.MethodPost, "/runtime/openai/v1/chat/completions", strings.NewReader(`{"model":"vendor/model"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+randomSecret(t))
	w := httptest.NewRecorder()
	service.ModelHandler(w, r)
	if w.Code == 200 || modelCalls.Load() != 2 {
		t.Fatal("unlisted frozen model reached upstream")
	}
	if err := policy.CheckModelURL(t.Context(), base+"/v1"); err != nil {
		t.Fatal(err)
	}
	if _, err = fetch.Fetch(t.Context(), base+"/redirect", hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("redirect accepted")
	}
	if _, err = fetch.Fetch(t.Context(), base+"/package.orax?"+query, strings.Repeat("0", 64)); err == nil {
		t.Fatal("digest failure accepted")
	}
	if _, err = NewUpstreamClient(UpstreamConfig{Policy: policy, DevelopmentHosts: []string{"example.com"}}); err == nil {
		t.Fatal("enterprise development bypass accepted")
	}
	// Use the same destination policy with no fixture CA: standard certificate verification must fail.
	untrusted, err := NewUpstreamClient(UpstreamConfig{Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.CloseIdleConnections()
	response, err := untrusted.Get(base + "/package.orax?" + query)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestConnectionLifetimeClosesActiveAndIdleSockets(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			left, right := net.Pipe()
			defer right.Close()
			leased := leaseConnection(left, time.Hour)
			defer leased.Close()
			// Fire the actual expiry callback deterministically rather than sleeping for the production bound.
			conn := leased.(*leasedConnection)
			conn.mu.Lock()
			conn.timer.Reset(0)
			conn.mu.Unlock()
			if active {
				_, err := leased.Write([]byte("stream"))
				if err == nil {
					t.Fatal("active connection survived expiry")
				}
			} else {
				_ = right.SetReadDeadline(time.Now().Add(time.Second))
				var b [1]byte
				_, err := right.Read(b[:])
				if err != io.EOF {
					t.Fatalf("expiry did not close socket: %v", err)
				}
			}
		})
	}
}

func TestPolicyPreventsReusedConnectionToUnlistedAuthority(t *testing.T) {
	s, ip, port := enterpriseTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	client, _, base := enterpriseClient(t, s, ip, port)
	resp, err := client.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	r, err := http.NewRequestWithContext(context.Background(), "GET", "https://unlisted.example.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err = client.Do(r); err == nil {
		_ = resp.Body.Close()
		t.Fatal("unlisted authority reused transport")
	}
}
