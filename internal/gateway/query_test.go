package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSessionQueryGuardPreservesRouteAndValueContracts(t *testing.T) {
	const list = "/api/v1/tenants/t/agent-sessions"
	const history = "/api/v1/tenants/t/issues/i/runs/r/history"
	for _, tc := range []struct {
		name, method, target string
		want                 bool
	}{
		{"list semicolon", "GET", list + "?status=running;status=failed", true},
		{"list malformed escape", "GET", list + "?status=running%zz", true},
		{"history semicolon", "GET", history + "?unknown=a;b", true},
		{"history malformed escape", "GET", history + "?unknown=%zz", true},
		{"statistics malformed escape", "GET", "/api/v1/tenants/t/issues/i/runs/r/statistics?unknown=%zz", true},
		{"calls semicolon", "GET", "/api/v1/tenants/t/issues/i/runs/r/model-calls?limit=1;after=x", true},
		{"empty query", "GET", list, false},
		{"escaped literals", "GET", list + "?agent=example%3Bvalue%25", false},
		{"timezone", "GET", list + "?from=2026-10-01T18%3A00%3A00%2B08%3A00", false},
		{"duplicate stays Cloud owned", "GET", list + "?status=running&status=failed", false},
		{"unknown stays Cloud owned", "GET", history + "?unknown=value", false},
		{"other API", "GET", "/api/v1/me?unknown=%zz", false},
		{"other tenant API", "GET", "/api/v1/tenants/t/issues?unknown=%zz", false},
		{"other run API", "GET", "/api/v1/tenants/t/issues/i/runs/r/thread?unknown=%zz", false},
		{"other method", "POST", list + "?unknown=%zz", false},
		{"missing tenant", "GET", "/api/v1/tenants//agent-sessions?unknown=%zz", false},
		{"missing issue", "GET", "/api/v1/tenants/t/issues//runs/r/history?unknown=%zz", false},
		{"suffix", "GET", history + "/extra?unknown=%zz", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.target, http.NoBody)
			if got := malformedSessionQuery(request); got != tc.want {
				t.Fatalf("malformedSessionQuery=%v, want %v", got, tc.want)
			}
		})
	}
}
