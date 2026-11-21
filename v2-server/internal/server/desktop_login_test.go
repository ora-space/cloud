package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// desktopLoginContract is the candidate contract the desktop login routes implement.
var desktopLoginContract = sync.OnceValues(func() (routers.Router, error) {
	doc, e := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "desktop-login.openapi.json"))
	if e != nil {
		return nil, e
	}
	if e := doc.Validate(context.Background()); e != nil {
		return nil, e
	}
	return legacy.NewRouter(doc)
})

const loopback = "http://127.0.0.1:53682/callback"

type desktopFixture struct {
	t     *testing.T
	srv   *httptest.Server
	store *store.Store
	sql   *pgx.Conn
}

func newDesktopFixture(t *testing.T) *desktopFixture {
	t.Helper()
	s, sql := openStoreSQL(t)
	srv := httptest.NewServer(server.New(s, server.Options{SessionTTL: time.Hour}))
	t.Cleanup(srv.Close)
	return &desktopFixture{t: t, srv: srv, store: s, sql: sql}
}

// account makes an account and its web session cookie.
func (f *desktopFixture) account(name string) (string, *http.Cookie) {
	f.t.Helper()
	var id string
	if e := f.sql.QueryRow(context.Background(), `INSERT INTO users (source, subject, display_name) VALUES ('test', $1, $1) RETURNING id`, name).Scan(&id); e != nil {
		f.t.Fatal(e)
	}
	token, e := f.store.StartSession(context.Background(), id, time.Hour)
	if e != nil {
		f.t.Fatal(e)
	}
	return id, &http.Cookie{Name: server.SessionCookie, Value: token}
}

type creds struct {
	cookie *http.Cookie
	bearer string
	origin string
}

// call sends one request and checks the response against desktop-login.openapi.json when the
// route is one it describes.
func (f *desktopFixture) call(method, path string, c creds, body any) (int, map[string]any) {
	f.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, f.srv.URL+path, reader)
	if e != nil {
		f.t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	routes, e := desktopLoginContract()
	if e != nil {
		f.t.Fatal(e)
	}
	if route, params, e := routes.FindRoute(req); e == nil {
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
			Status:                 res.StatusCode, Header: res.Header,
			Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		in.SetBodyBytes(raw)
		if e := openapi3filter.ValidateResponse(context.Background(), in); e != nil {
			f.t.Fatalf("%s %s does not match desktop-login.openapi.json: %v", method, path, e)
		}
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

type pkce struct{ verifier, challenge string }

func newPKCE(seed string) pkce {
	verifier := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(seed, 32)[:32]))
	sum := sha256.Sum256([]byte(verifier))
	return pkce{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}
}

func approval(redirect, challenge string) map[string]string {
	return map[string]string{"redirectUri": redirect, "state": "st-1", "codeChallenge": challenge, "codeChallengeMethod": "S256"}
}

// approve approves as the cookie's account and returns the code the loopback receives.
func (f *desktopFixture) approve(c *http.Cookie, p pkce) string {
	f.t.Helper()
	status, out := f.call("POST", "/auth/desktop/approve", creds{cookie: c, origin: f.srv.URL}, approval(loopback, p.challenge))
	redirect, _ := out["redirectUrl"].(string)
	u, e := url.Parse(redirect)
	if status != 200 || e != nil || u.Scheme+"://"+u.Host+u.Path != loopback || u.Query().Get("state") != "st-1" || u.Query().Get("code") == "" {
		f.t.Fatalf("approve = %d %v", status, out)
	}
	return u.Query().Get("code")
}

func (f *desktopFixture) exchange(code, verifier, redirect string) (int, map[string]any) {
	f.t.Helper()
	return f.call("POST", "/auth/desktop/token", creds{}, map[string]string{"code": code, "codeVerifier": verifier, "redirectUri": redirect})
}

// login runs the whole approval and exchange and returns the desktop token.
func (f *desktopFixture) login(c *http.Cookie) string {
	f.t.Helper()
	p := newPKCE("a")
	status, out := f.exchange(f.approve(c, p), p.verifier, loopback)
	token, _ := out["token"].(string)
	if status != 200 || token == "" {
		f.t.Fatalf("exchange = %d %v", status, out)
	}
	return token
}

func (f *desktopFixture) whoami(c creds) (int, string) {
	f.t.Helper()
	status, out := f.call("GET", "/api/v1/me", c, nil)
	id, _ := out["id"].(string)
	return status, id
}

// A6 D6: the token the desktop receives belongs to the same account as the web session that
// approved it, and authorizes the same routes.
func TestDesktopLoginIssuesATokenForTheApprovingAccount(t *testing.T) {
	f := newDesktopFixture(t)
	alice, cookie := f.account("alice")
	p := newPKCE("a")
	status, out := f.exchange(f.approve(cookie, p), p.verifier, loopback)
	user, _ := out["user"].(map[string]any)
	token, _ := out["token"].(string)
	if status != 200 || user["id"] != alice || user["displayName"] != "alice" || !strings.HasPrefix(token, store.DesktopTokenPrefix) {
		t.Fatalf("exchange = %d %v", status, out)
	}
	if status, id := f.whoami(creds{bearer: token}); status != 200 || id != alice {
		t.Fatalf("me with token = %d %s", status, id)
	}
	if status, out := f.call("GET", "/api/v1/me/tenants", creds{bearer: token}, nil); status != 200 || out["items"] == nil {
		t.Fatalf("tenants with token = %d %v", status, out)
	}
	// IPv6 loopback is accepted like IPv4.
	if status, _ := f.call("POST", "/auth/desktop/approve", creds{cookie: cookie, origin: f.srv.URL}, approval("http://[::1]:8080/cb", p.challenge)); status != 200 {
		t.Fatalf("approve [::1] = %d", status)
	}
}

// A6 invariant 9: a code is spent by its first exchange and yields a token only with its own
// verifier and redirect URI.
func TestDesktopCodeIsSingleUseAndBound(t *testing.T) {
	f := newDesktopFixture(t)
	_, cookie := f.account("alice")
	p := newPKCE("a")

	code := f.approve(cookie, p)
	if status, _ := f.exchange(code, p.verifier, loopback); status != 200 {
		t.Fatalf("first exchange = %d", status)
	}
	if status, out := f.exchange(code, p.verifier, loopback); status != 400 || out["code"] != "invalid_grant" {
		t.Fatalf("replayed code = %d %v", status, out)
	}

	for name, bad := range map[string][2]string{
		"wrong verifier": {newPKCE("b").verifier, loopback},
		"wrong redirect": {p.verifier, "http://127.0.0.1:53683/callback"},
		"short verifier": {"short", loopback},
	} {
		code := f.approve(cookie, p)
		if status, out := f.exchange(code, bad[0], bad[1]); status != 400 || out["code"] != "invalid_grant" {
			t.Fatalf("%s = %d %v", name, status, out)
		}
		// The failed attempt spent the code; the right verifier is too late now.
		if status, _ := f.exchange(code, p.verifier, loopback); status != 400 {
			t.Fatalf("%s: code survived a failed exchange (%d)", name, status)
		}
	}
	if status, out := f.exchange("never-issued", p.verifier, loopback); status != 400 || out["code"] != "invalid_grant" {
		t.Fatalf("unknown code = %d %v", status, out)
	}
}

func TestDesktopApprovalAcceptsOnlyLoopbackAndS256(t *testing.T) {
	f := newDesktopFixture(t)
	_, cookie := f.account("alice")
	p := newPKCE("a")
	approve := func(body map[string]string) (int, string) {
		status, out := f.call("POST", "/auth/desktop/approve", creds{cookie: cookie, origin: f.srv.URL}, body)
		code, _ := out["code"].(string)
		return status, code
	}
	for _, redirect := range []string{
		"http://localhost:53682/callback",     // resolves through DNS and hosts
		"https://127.0.0.1:53682/callback",    // not plain loopback http
		"http://127.0.0.1/callback",           // no port
		"http://127.0.0.1:0/callback",         // not a port a listener has
		"http://127.0.0.1:53682",              // no path
		"http://127.0.0.1:53682/cb?next=x",    // would mix with code and state
		"http://127.0.0.1:53682/cb#frag",      // fragment
		"http://127.0.0.1:53682/cb#",          // empty fragment still moves code and state
		"http://user@127.0.0.1:53682/cb",      // userinfo
		"http://10.0.0.1:53682/callback",      // another machine
		"ora://callback",                      // custom scheme
		"http://127.0.0.1.evil.test:53682/cb", // DNS name dressed as loopback
	} {
		if status, code := approve(approval(redirect, p.challenge)); status != 400 || code != "invalid_redirect_uri" {
			t.Errorf("%s = %d %s", redirect, status, code)
		}
	}
	plain := approval(loopback, p.verifier)
	plain["codeChallengeMethod"] = "plain"
	if status, code := approve(plain); status != 400 || code != "invalid_code_challenge" {
		t.Errorf("plain method = %d %s", status, code)
	}
	if status, code := approve(approval(loopback, "short")); status != 400 || code != "invalid_code_challenge" {
		t.Errorf("short challenge = %d %s", status, code)
	}
	noState := approval(loopback, p.challenge)
	noState["state"] = ""
	if status, code := approve(noState); status != 400 || code != "invalid_state" {
		t.Errorf("empty state = %d %s", status, code)
	}
}

// Only a person in the browser can approve: a page on another origin cannot use the cookie, and
// a desktop token cannot mint further tokens.
func TestDesktopApprovalNeedsTheWebSession(t *testing.T) {
	f := newDesktopFixture(t)
	_, cookie := f.account("alice")
	p := newPKCE("a")
	token := f.login(cookie)
	body := approval(loopback, p.challenge)
	if status, out := f.call("POST", "/auth/desktop/approve", creds{}, body); status != 401 || out["code"] != "unauthenticated" {
		t.Fatalf("no session = %d %v", status, out)
	}
	if status, out := f.call("POST", "/auth/desktop/approve", creds{cookie: cookie, origin: "https://evil.example"}, body); status != 403 || out["code"] != "origin_mismatch" {
		t.Fatalf("foreign origin = %d %v", status, out)
	}
	if status, out := f.call("POST", "/auth/desktop/approve", creds{bearer: token}, body); status != 403 || out["code"] != "web_session_required" {
		t.Fatalf("bearer approval = %d %v", status, out)
	}
	if status, out := f.call("POST", "/auth/desktop/approve", creds{cookie: cookie, bearer: token, origin: f.srv.URL}, body); status != 403 || out["code"] != "web_session_required" {
		t.Fatalf("bearer with cookie = %d %v", status, out)
	}
}

// A6 D6 step 5: with an Authorization header, only the header decides who the caller is.
func TestBearerHeaderAloneDecidesTheCaller(t *testing.T) {
	f := newDesktopFixture(t)
	alice, aliceCookie := f.account("alice")
	bob, bobCookie := f.account("bob")
	bobToken := f.login(bobCookie)

	if status, id := f.whoami(creds{cookie: aliceCookie, bearer: bobToken}); status != 200 || id != bob {
		t.Fatalf("alice's cookie with bob's token = %d %s, want bob %s", status, id, bob)
	}
	for name, c := range map[string]creds{
		"unknown token beside a cookie": {cookie: aliceCookie, bearer: "ora_dt_unknown"},
		"web session as bearer":         {bearer: aliceCookie.Value},
		"desktop token as cookie":       {cookie: &http.Cookie{Name: server.SessionCookie, Value: bobToken}},
	} {
		if status, _ := f.whoami(c); status != 401 {
			t.Errorf("%s = %d, want 401", name, status)
		}
	}
	// A malformed header is refused rather than skipped in favour of the cookie.
	req, _ := http.NewRequest("GET", f.srv.URL+"/api/v1/me", nil)
	req.AddCookie(aliceCookie)
	req.Header.Set("Authorization", "Basic "+bobToken)
	if res, e := http.DefaultClient.Do(req); e != nil || res.StatusCode != 401 {
		t.Fatalf("basic scheme = %v %v", res, e)
	} else {
		res.Body.Close()
	}
	// Bearer writes need no Origin: a browser never attaches the token by itself. The invitation
	// route answers 404 for an unknown link, not 403.
	if status, out := f.call("POST", "/api/v1/invites/preview", creds{bearer: bobToken, origin: "https://elsewhere.example"}, map[string]string{"token": "x"}); status != 404 {
		t.Fatalf("bearer write with foreign Origin = %d %v", status, out)
	}
	if status, id := f.whoami(creds{cookie: aliceCookie}); status != 200 || id != alice {
		t.Fatalf("cookie alone = %d %s", status, id)
	}
}

// A6 D6: desktop logout revokes only its own token; web logout leaves desktop tokens alone; a
// disabled account loses every credential at the next request.
func TestDesktopLogoutAndAccountState(t *testing.T) {
	f := newDesktopFixture(t)
	alice, cookie := f.account("alice")
	laptop := f.login(cookie)
	desk := f.login(cookie)

	if status, _ := f.call("POST", "/auth/desktop/logout", creds{}, nil); status != 401 {
		t.Fatalf("logout without token = %d", status)
	}
	if status, _ := f.call("POST", "/auth/desktop/logout", creds{bearer: laptop}, nil); status != 204 {
		t.Fatalf("logout = %d", status)
	}
	if status, _ := f.call("POST", "/auth/desktop/logout", creds{bearer: laptop}, nil); status != 204 {
		t.Fatalf("repeated logout = %d", status)
	}
	if status, _ := f.whoami(creds{bearer: laptop}); status != 401 {
		t.Fatalf("revoked token = %d", status)
	}
	if status, id := f.whoami(creds{bearer: desk}); status != 200 || id != alice {
		t.Fatalf("other device after logout = %d", status)
	}
	if status, id := f.whoami(creds{cookie: cookie}); status != 200 || id != alice {
		t.Fatalf("web session after desktop logout = %d", status)
	}
	if status, _ := f.call("POST", "/auth/logout", creds{cookie: cookie, origin: f.srv.URL}, nil); status != 204 {
		t.Fatalf("web logout = %d", status)
	}
	if status, _ := f.whoami(creds{bearer: desk}); status != 200 {
		t.Fatalf("desktop token after web logout = %d", status)
	}

	// Approved, then disabled before the desktop exchanges: no token.
	_, cookie2 := f.account("carol")
	p := newPKCE("c")
	code := f.approve(cookie2, p)
	if _, e := f.sql.Exec(context.Background(), `UPDATE users SET status = 'disabled' WHERE display_name IN ('alice', 'carol')`); e != nil {
		t.Fatal(e)
	}
	if status, _ := f.whoami(creds{bearer: desk}); status != 401 {
		t.Fatalf("token of a disabled account = %d", status)
	}
	if status, out := f.exchange(code, p.verifier, loopback); status != 400 || out["code"] != "invalid_grant" {
		t.Fatalf("exchange for a disabled account = %d %v", status, out)
	}
}
