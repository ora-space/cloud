package gitrefquery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

// A ref query's negative evidence is only a named 404; transport and malformed responses prove
// nothing. The production client must preserve that distinction before Revision registration.
func TestResolveGitRefDistinguishesMissingFromUnavailable(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, body string
		status     int
		missing    bool
		valid      bool
	}{
		{"present", `{"commit":"` + commit + `"}`, 200, false, true},
		{"missing", `{"code":"ref_not_found"}`, 404, true, false},
		{"unavailable", `{"code":"unavailable"}`, 503, false, false},
		{"unknown_not_found", `{"code":"not_found"}`, 404, false, false},
		{"malformed_commit", `{"commit":"not-a-commit"}`, 200, false, false},
		{"malformed_body", `not-json`, 200, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/refs" || r.URL.Query().Get("tenantId") != "tenant" || r.URL.Query().Get("projectId") != "project" || r.URL.Query().Get("ref") != "refs/ora/deliveries/work" {
					t.Error("query changed the exact Project/ref identity")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(server.URL, server.Client(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.ResolveGitRef(t.Context(), "tenant", "project", "refs/ora/deliveries/work")
			if tc.valid {
				if got != commit || err != nil {
					t.Fatalf("resolve = %q, %v", got, err)
				}
			} else if got != "" || err == nil || errors.Is(err, core.ErrGitRefNotFound) != tc.missing {
				t.Fatalf("resolve = %q, %v; missing=%v", got, err, tc.missing)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := client.ResolveGitRef(ctx, "tenant", "project", "refs/ora/deliveries/work"); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled query = %v", err)
			}
		})
	}
}
