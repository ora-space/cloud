package gateway

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
)

func queryTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_POSTGRES") == "1" {
			t.Fatal("TEST_DATABASE_URL is required for Gateway query acceptance")
		}
		t.Skip("real PostgreSQL: set TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = admin.Close() })
	schema := "gateway_query_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	config.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = pool.Close() })
	store := &core.Store{Pool: pool}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pool
}

// The fixture exercises the production Cookie -> Resolve -> signed proxy -> Cloud router chain.
// Only a synthetic login session and tenant membership are staged in an isolated migrated schema.
func queryTestGateway(t *testing.T) (*httptest.Server, string, string, <-chan string) {
	t.Helper()
	pool := queryTestDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	identity := VerifiedIdentity{Source: "dev", Subject: "query-reader", DisplayName: "Query reader"}
	attempt, err := store.CreateAttempt(t.Context(), "synthetic-attempt", "synthetic-state", "dev", "/", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.ConsumeAttempt(t.Context(), attempt, identity, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tenant, user := uuid.NewString(), uuid.NewString()
	tx, err := pool.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users(id,display_name,status) VALUES($1,'Query reader','active')`, []any{user}},
		{`INSERT INTO user_identities(user_id,source,subject) VALUES($1,'dev','query-reader')`, []any{user}},
		{`INSERT INTO tenants(id,name,status) VALUES($1,'Query test','active')`, []any{tenant}},
		{`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, []any{tenant, user}},
	} {
		if _, err := tx.ExecContext(t.Context(), row.query, row.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	issuer, verifier := newTestIssuer(t, time.Now)
	cloud := router.NewDevelopment(&core.Store{Pool: pool}, verifier, zap.NewNop())
	observed := make(chan string, 32)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.URL.RawQuery
		cloud.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewHandler(&Options{
		Store: store, Login: &Login{}, Issuer: issuer, Limiter: NewRateLimiter(60, 10, 100, time.Now),
		Upstream: upstreamURL, UpstreamTimeout: 5 * time.Second, PublicOrigin: "http://localhost",
		Cookies: CookiePolicy{CallbackPath: CallbackPath}, Log: zap.NewNop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	return server, token, tenant, observed
}

func TestSessionQueryGatewayRejectsMalformedBeforeProxyRewrite(t *testing.T) {
	server, token, tenant, observed := queryTestGateway(t)
	list := "/api/v1/tenants/" + tenant + "/agent-sessions"
	history := "/api/v1/tenants/" + tenant + "/issues/" + uuid.NewString() + "/runs/" + uuid.NewString() + "/history"
	for _, tc := range []struct {
		name, path, query, upstreamQuery string
		status                           int
		code                             string
		forwarded, unauthenticated       bool
	}{
		{name: "statistics malformed escape", path: strings.TrimSuffix(history, "history") + "statistics", query: "unknown=%zz", status: 400, code: "invalid_session_filter"},
		{name: "calls semicolon", path: strings.TrimSuffix(history, "history") + "model-calls", query: "limit=1;after=x", status: 400, code: "invalid_session_filter"},
		{name: "list semicolon", path: list, query: "status=running;status=failed", status: 400, code: "invalid_session_filter"},
		{name: "list escape", path: list, query: "status=running%zz", status: 400, code: "invalid_session_filter"},
		{name: "mixed valid and invalid", path: list, query: "limit=1&status=running;status=failed", status: 400, code: "invalid_session_filter"},
		{name: "history semicolon", path: history, query: "unknown=a;b", status: 400, code: "invalid_session_filter"},
		{name: "history escape", path: history, query: "unknown=%zz", status: 400, code: "invalid_session_filter"},
		{name: "auth still first", path: list, query: "status=running%zz", status: 401, code: "unauthenticated", unauthenticated: true},
		{name: "valid list", path: list, status: 200, forwarded: true},
		{name: "encoded literals", path: list, query: "agent=example%3Bvalue%25", upstreamQuery: "agent=example%3Bvalue%25", status: 200, forwarded: true},
		{name: "valid timezone", path: list, query: "from=2026-10-01T18%3A00%3A00%2B08%3A00", upstreamQuery: "from=2026-10-01T18%3A00%3A00%2B08%3A00", status: 200, forwarded: true},
		{name: "Cloud rejects duplicate", path: list, query: "status=running&status=failed", upstreamQuery: "status=running&status=failed", status: 400, code: "invalid_session_filter", forwarded: true},
		{name: "Cloud rejects history key", path: history, query: "historyKey=private", upstreamQuery: "historyKey=private", status: 400, code: "invalid_session_filter", forwarded: true},
		{name: "other route unchanged", path: "/api/v1/me", query: "unknown=a;b", status: 200, forwarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+tc.path+"?"+tc.query, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.unauthenticated {
				request.AddCookie(&http.Cookie{Name: "ora_session", Value: token})
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Code, RequestID string
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || body.Code != tc.code || tc.status >= 400 && body.RequestID == "" {
				t.Fatalf("status=%d code=%q requestId=%q, want %d %q", response.StatusCode, body.Code, body.RequestID, tc.status, tc.code)
			}
			select {
			case query := <-observed:
				if !tc.forwarded || query != tc.upstreamQuery {
					t.Fatalf("unexpected upstream query %q, forwarded=%v want query=%q", query, tc.forwarded, tc.upstreamQuery)
				}
			default:
				if tc.forwarded {
					t.Fatal("well-formed request never reached Cloud")
				}
			}
		})
	}
}
