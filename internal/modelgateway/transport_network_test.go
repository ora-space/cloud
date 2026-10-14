package modelgateway

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Optional live deployment probe: no credential is read or sent. Unit tests remain deterministic.
func TestLiveTrustedDNSAndVerifiedUpstreamTLS(t *testing.T) {
	endpoint := os.Getenv("MODEL_GATEWAY_NETWORK_TEST_URL")
	if endpoint == "" {
		t.Skip("set MODEL_GATEWAY_NETWORK_TEST_URL for the credential-free live network probe")
	}
	client, err := NewUpstreamClient(UpstreamConfig{DNS: DNSConfig{HTTPSURL: "https://cloudflare-dns.com/dns-query", BootstrapIPs: []string{"1.1.1.1", "1.0.0.1"}}})
	if err != nil {
		t.Fatal("trusted resolver configuration rejected")
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal("invalid probe endpoint")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("credential-free HTTPS probe failed: %s", upstreamFailureCode(err))
	}
	defer response.Body.Close()
	if response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
		t.Fatal("upstream TLS identity was not verified")
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 4096)); err != nil {
		t.Fatal("probe body could not be consumed")
	}
	t.Logf("trusted DNS, public-address policy and normal TLS verification passed; unauthenticated HTTP status=%d", response.StatusCode)
}
