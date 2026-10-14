package gateway

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCredentialProxyUsesDedicatedTrustAndRejectsUnsafeOrigins(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"resource":{"credentialConfigured":true}}`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := newCredentialProxy(upstream, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "https://app.example.invalid/api/v1/me/model-connections/not-a-uuid/credential", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r, func(err error) { t.Error(err) }, func() {})
	if w.Code != 200 {
		t.Fatal("dedicated trusted credential upstream refused")
	}
	for _, origin := range []string{"http://example.invalid", "https://user:password@example.invalid", "https://example.invalid/path", "https://example.invalid?select=other"} {
		u, parseErr := url.Parse(origin)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if _, err = newCredentialProxy(u, path, time.Second); err == nil {
			t.Fatal("unsafe credential origin accepted")
		}
	}
}
