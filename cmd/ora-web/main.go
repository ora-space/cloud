// Command ora-web is the local development edge server for the web frontend in `frontend/`.
//
// It mirrors the topology of `cmd/demo-issue-board-web`: the browser stays untrusted, and this
// process — not the browser — signs the two credentials the public API requires. It adds a
// real auth session (login/logout) and serves the built SPA, so the frontend talks to the actual
// cloud HTTP API instead of its MSW mocks.
//
// It is a development convenience, not a production gateway: production deployments sign user
// tokens in the Gateway/infrastructure adapter and hold the service private key out of reach of
// both the browser and this process (see docs/identity/authentication.md).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/collab"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

const (
	gatewaySubject = "ora-web-gateway" // fixed service identity the browser acts under
	identitySource = "dev"             // login namespace; see docs/identity/authentication.md
	sessionCookie  = "ora_subject"     // holds the login subject; user token is re-signed per request
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ora-web failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	dsn := envOr("ORA_DATABASE_URL", "host=127.0.0.1 port=55432 user=ora password=ora-local dbname=ora sslmode=disable")
	config, e := pgx.ParseConfig(dsn)
	if e != nil {
		return e
	}
	admin := stdlib.OpenDB(*config)
	if e = admin.Ping(); e != nil {
		return fmt.Errorf("connect PostgreSQL: %w", e)
	}
	defer admin.Close()

	// A fixed schema keeps the dev frontend away from other data while surviving restarts
	// (unlike demo-issue-board-web, which drops its schema on exit).
	if _, e = admin.Exec("CREATE SCHEMA IF NOT EXISTS ora_web"); e != nil {
		return e
	}
	config.RuntimeParams["search_path"] = "ora_web"
	pool := stdlib.OpenDB(*config)
	defer pool.Close()
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		return e
	}
	store, e := core.NewStore(db)
	if e != nil {
		return e
	}
	// Dev/demo collaboration fixtures (production leaves these nil -> Unavailable). Single wiring
	// point shared with cmd/server's development gate: collab.WireDevelopmentFixtures.
	collab.WireDevelopmentFixtures(store)
	if e = store.Migrate(ctx); e != nil {
		return fmt.Errorf("migrate: %w", e)
	}

	credentials, e := simulator.NewCredentials()
	if e != nil {
		return e
	}
	auth, e := core.NewAuthenticator("ora-cloud", credentials.Trust)
	if e != nil {
		return e
	}
	gin.SetMode(gin.ReleaseMode)
	log, _ := zap.NewProduction()
	rt := router.New(store, auth, log)

	tenant, e := ensureDevTenant(ctx, store)
	if e != nil {
		return e
	}
	tid := tenant.S("tenantId")

	// signUser mints a fresh, short-lived user token for the logged-in subject. The simulator
	// enforces a <=5-minute lifetime, so a token is re-signed per request rather than cached.
	signUser := func(subject string) (string, error) {
		claims := core.Claims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: subject},
			Source:           identitySource,
			DisplayName:      displayName(subject),
			Caller:           gatewaySubject,
		}
		return credentials.Token("user", claims)
	}
	signGateway := func() (string, error) {
		claims := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: gatewaySubject}}
		return credentials.Token("gateway", claims)
	}

	mux := http.NewServeMux()

	// Session endpoints. The browser holds only a session cookie and the user profile; the raw
	// user token never needs to reach JavaScript.
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		subject := normalizeSubject(body.Email)
		u, e := store.EnsureMember(ctx, tid, identitySource, subject, displayName(subject))
		if e != nil {
			http.Error(w, core.ErrorCode(e).Code, http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: subject, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int((24 * time.Hour).Seconds())}) //nolint:gosec // dev-only local auth over plain HTTP; loopback has no TLS
		writeJSON(w, 200, map[string]any{
			"user":       map[string]any{"id": u.S("id"), "displayName": u.S("displayName"), "subject": subject},
			"tenantId":   tid,
			"tenantName": tenant.S("name"),
		})
	})

	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1}) //nolint:gosec // dev-only local auth over plain HTTP; loopback has no TLS
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// Registration creates a new user identity (name + email) and signs them straight
	// into the same session the login flow uses, so the new user enters the
	// current-user flow without a second step. A duplicate email is a 409, not a
	// silent login. It provisions no password, workspace membership, or project
	// ownership — registration is account creation only.
	mux.HandleFunc("POST /auth/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		}
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		subject := normalizeEmail(body.Email)
		u, e := store.RegisterIdentity(ctx, tid, identitySource, subject, body.Name)
		if e != nil {
			fault := core.ErrorCode(e)
			writeJSON(w, fault.Status, map[string]any{"code": fault.Code, "params": fault.Params, "requestId": ""})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: subject, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int((24 * time.Hour).Seconds())}) //nolint:gosec // dev-only local auth over plain HTTP; loopback has no TLS
		writeJSON(w, 200, map[string]any{
			"user":       map[string]any{"id": u.S("id"), "displayName": u.S("displayName"), "subject": subject},
			"tenantId":   tid,
			"tenantName": tenant.S("name"),
		})
	})

	// API proxy: inject the two credentials the router requires and forward in-process.
	proxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw, e := signGateway()
		if e != nil {
			http.Error(w, e.Error(), http.StatusInternalServerError)
			return
		}
		r.Header.Set("Authorization", "Bearer "+gw)
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			user, e := signUser(c.Value)
			if e != nil {
				http.Error(w, e.Error(), http.StatusInternalServerError)
				return
			}
			r.Header.Set("X-Ora-User-Token", user)
		}
		rt.ServeHTTP(w, r)
	})
	mux.Handle("/api/", proxy)
	mux.Handle("/internal/", proxy)
	mux.Handle("/healthz", proxy)

	// SPA: built assets in production mode, Vite in dev mode.
	spa := spaHandler(envOr("ORA_WEB_DEV", "") == "1")
	mux.Handle("/", spa)

	addr := envOr("ORA_WEB_ADDR", "127.0.0.1:8080")
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	fmt.Printf("Ora web edge: http://%s  (tenant %s, schema ora_web)\n", addr, tid)
	fmt.Println("Run the frontend with `cd frontend && npm run dev` (dev) or `npm run build` + restart (production).")

	stopCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	failed := make(chan error, 1)
	go func() { failed <- srv.ListenAndServe() }()
	select {
	case e = <-failed:
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	case <-stopCtx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// ensureDevTenant returns the bootstrap tenant, creating it on first run so restarts reuse the
// same tenant and its data instead of piling up fresh tenants.
func ensureDevTenant(ctx context.Context, store *core.Store) (core.Object, error) {
	const name = "Ora Workspace"
	var tid string
	row := store.Pool.QueryRowContext(ctx, `
		SELECT t.id FROM tenants t
		JOIN tenant_memberships m ON m.tenant_id = t.id
		JOIN users u ON u.id = m.user_id
		JOIN user_identities i ON i.user_id = u.id
		WHERE i.source = $1 AND i.subject = $2 AND m.status = 'active' AND t.status = 'active' AND t.deleted_at IS NULL
		LIMIT 1`, identitySource, "demo")
	switch e := row.Scan(&tid); {
	case e == sql.ErrNoRows:
		b, err := store.Bootstrap(ctx, name, identitySource, "demo", "Demo User")
		if err != nil {
			return nil, err
		}
		return core.Object{"tenantId": b.S("tenantId"), "name": name}, nil
	case e != nil:
		return nil, e
	default:
		return core.Object{"tenantId": tid, "name": name}, nil
	}
}

// normalizeSubject turns the login email into the stable identity subject. An empty input maps to
// the bootstrap "demo" identity so the default login is the tenant administrator.
func normalizeSubject(email string) string {
	s := strings.ToLower(strings.TrimSpace(email))
	if s == "" {
		return "demo"
	}
	return s
}

// normalizeEmail is the registration variant of normalizeSubject: it folds the
// address to lowercase+trimmed (so case variants are one identity) but, unlike
// login, keeps an empty value empty — registration must reject it, not fall back
// to the "demo" bootstrap.
func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func displayName(subject string) string {
	if i := strings.IndexByte(subject, '@'); i > 0 {
		subject = subject[:i]
	}
	if subject == "" {
		return "User"
	}
	return subject
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// spaHandler serves the built SPA from `frontend/dist` with an index.html fallback for
// client-side routes, or proxies the whole frontend to the Vite dev server in dev mode.
func spaHandler(dev bool) http.Handler {
	if dev {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Vite dev server; relative path is preserved so its own HMR keeps working.
			http.Redirect(w, r, "http://localhost:5173"+r.URL.Path, http.StatusFound) //nolint:gosec // dev-only: fixed localhost:5173 origin, not user-controlled
		})
	}
	dist := http.Dir(filepath.Join("frontend", "dist"))
	fs := http.FileServer(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			if _, e := os.Stat(filepath.Join("frontend", "dist", filepath.Clean(strings.TrimPrefix(r.URL.Path, "/")))); e != nil {
				r.URL.Path = "/" // client-side route: serve the shell
			}
		}
		fs.ServeHTTP(w, r)
	})
}
