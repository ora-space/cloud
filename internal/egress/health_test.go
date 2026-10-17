package egress

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthExposesOnlyPolicyIdentity(t *testing.T) {
	p, err := Parse([]byte(enterpriseFixture))
	if err != nil {
		t.Fatal(err)
	}
	digest := p.Digest(nil)
	h := HealthMetadata(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }), digest)
	for _, path := range []string{"/healthz", "/api/v1/me"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if path == "/healthz" {
			if w.Header().Get("X-Ora-Egress-Schema") != "1" || w.Header().Get("X-Ora-Egress-Digest") != digest || len(digest) != 64 {
				t.Fatal("missing policy identity")
			}
		} else if w.Header().Get("X-Ora-Egress-Digest") != "" {
			t.Fatal("metadata escaped health endpoint")
		}
		if strings.Contains(w.Header().Get("X-Ora-Egress-Digest"), "example") {
			t.Fatal("config exposed")
		}
	}
}
