package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gateway"
	"github.com/wanglongan587/cloud/internal/gateway/github"
	"github.com/wanglongan587/cloud/internal/gateway/idaas"
)

// fakeProvider is a GitHub-shaped OAuth server that really verifies PKCE: the authorize step records
// the S256 challenge per issued code, and the token step refuses a mismatching verifier or a reused
// code (unless reuse is enabled to expose the Gateway's own consume race).
type fakeProvider struct {
	mu        sync.Mutex
	codes     map[string]string // code -> challenge
	reusable  bool
	userID    int64
	userName  string
	userLogin string
	server    *httptest.Server
}

type fakeIDaaS struct {
	mu        sync.Mutex
	codes     map[string]struct{}
	reusable  bool
	server    *httptest.Server
	userUUID  string
	userName  string
	lastToken map[string]string
	lastUser  map[string]string
}

func newFakeIDaaS(t *testing.T) *fakeIDaaS {
	t.Helper()
	p := &fakeIDaaS{codes: map[string]struct{}{}, userUUID: " w00576782 ", userName: "Wang Longan"}
	mux := http.NewServeMux()
	mux.HandleFunc("/saaslogin1/oauth2/v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "idaas-client" || q.Get("response_type") != "code" || q.Get("scope") != idaas.Scope || q.Get("state") == "" || q.Get("redirect_uri") == "" {
			http.Error(w, "missing IDaaS authorization parameters", http.StatusBadRequest)
			return
		}
		for key := range q {
			switch key {
			case "client_id", "response_type", "redirect_uri", "scope", "state":
			default:
				http.Error(w, "undocumented IDaaS authorization parameter", http.StatusBadRequest)
				return
			}
		}
		code := "idaas-code-" + strings.ReplaceAll(q.Get("state")[:8], "/", "_")
		p.mu.Lock()
		p.codes[code] = struct{}{}
		p.mu.Unlock()
		target, e := url.Parse(q.Get("redirect_uri"))
		if e != nil {
			http.Error(w, "bad redirect", http.StatusBadRequest)
			return
		}
		params := target.Query()
		params.Set("code", code)
		params.Set("state", q.Get("state"))
		target.RawQuery = params.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	})
	mux.HandleFunc("/saaslogin1/oauth2/v1/token", func(w http.ResponseWriter, r *http.Request) {
		body := readPostedForm(t, r)
		p.mu.Lock()
		_, ok := p.codes[body["code"]]
		if ok && !p.reusable {
			delete(p.codes, body["code"])
		}
		p.lastToken = maps.Clone(body)
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok || len(body) != 4 || body["client_id"] != "idaas-client" || body["client_secret"] != "idaas-secret" || body["grant_type"] != "authorization_code" || body["code"] == "" {
			_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"code Parameter error"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"idaas-access-token","token_type":"Bearer","refresh_token":"idaas-refresh-token","expires_in":"1800"}`))
	})
	mux.HandleFunc("/saaslogin1/oauth2/v1/userinfo", func(w http.ResponseWriter, r *http.Request) {
		body := readPostedForm(t, r)
		p.mu.Lock()
		p.lastUser = maps.Clone(body)
		uuid, name := p.userUUID, p.userName
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if len(body) != 1 || body["access_token"] != "idaas-access-token" {
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		must(t, json.NewEncoder(w).Encode(map[string]any{"uuid": uuid, "userName": name, "globalUserID": "174022309561388", "tenantId": "111", "employeeNumber": "30000000", "email": "private@example.com"}))
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

func readPostedForm(t *testing.T, r *http.Request) map[string]string {
	t.Helper()
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
		t.Errorf("client_secret_post request was %s %q Authorization=%q query=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.URL.RawQuery)
	}
	must(t, r.ParseForm())
	out := map[string]string{}
	for key, values := range r.PostForm {
		if len(values) > 0 {
			out[key] = values[0]
		}
	}
	return out
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{codes: map[string]string{}, userID: 71996633, userName: "Ray Zhang", userLogin: "obsismc"}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
			http.Error(w, "missing PKCE or state", http.StatusBadRequest)
			return
		}
		code := "code-" + strings.ReplaceAll(q.Get("state")[:8], "/", "_")
		p.mu.Lock()
		p.codes[code] = q.Get("code_challenge")
		p.mu.Unlock()
		target, e := url.Parse(q.Get("redirect_uri"))
		if e != nil {
			http.Error(w, "bad redirect", http.StatusBadRequest)
			return
		}
		rq := target.Query()
		rq.Set("code", code)
		rq.Set("state", q.Get("state"))
		target.RawQuery = rq.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		must(t, r.ParseForm())
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		p.mu.Lock()
		challenge, ok := p.codes[r.PostForm.Get("code")]
		if ok && !p.reusable {
			delete(p.codes, r.PostForm.Get("code"))
		}
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok || challenge != base64.RawURLEncoding.EncodeToString(sum[:]) || r.PostForm.Get("client_secret") != "provider-secret" {
			_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"gho_provider_token","token_type":"bearer"}`))
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_provider_token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		must(t, json.NewEncoder(w).Encode(map[string]any{"id": p.userID, "login": p.userLogin, "name": p.userName, "email": "never-used@example.invalid"}))
	})
	p.server = httptest.NewServer(mux)
	t.Cleanup(p.server.Close)
	return p
}

type gatewayFixture struct {
	t        *testing.T
	pool     *sql.DB
	store    *gateway.Store
	cloud    *httptest.Server
	provider *fakeProvider
	replicas []replicaKeys
	pkceKey  []byte
	log      *zap.Logger
}

// replicaKeys is one Gateway replica's identity as trusted by Cloud: a service subject and two
// purpose-separated signing keys.
type replicaKeys struct {
	subject       string
	service, user ed25519.PrivateKey
}

func setupGateway(t *testing.T) *gatewayFixture {
	t.Helper()
	pool, _ := testSchema(t, "test_gateway_")
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, e)
	cloudStore, e := core.NewStore(db)
	must(t, e)
	must(t, cloudStore.Migrate(context.Background()))
	must(t, cloudStore.CheckSchema(context.Background()))
	f := &gatewayFixture{t: t, pool: pool, provider: newFakeProvider(t)}
	f.store, e = gateway.NewStore(pool)
	must(t, e)
	f.pkceKey = make([]byte, 32)
	_, e = rand.Read(f.pkceKey)
	must(t, e)
	// Cloud trusts a fixed set of replica identities up front; tests take replicas in order.
	var trust []core.TrustedKey
	for _, subject := range []string{"gateway-a", "gateway-b", "gateway-c", "gateway-d", "gateway-e", "gateway-f"} {
		servicePub, serviceKey, e := ed25519.GenerateKey(rand.Reader)
		must(t, e)
		userPub, userKey, e := ed25519.GenerateKey(rand.Reader)
		must(t, e)
		trust = append(trust,
			core.TrustedKey{ID: subject + "-service", Issuer: "ora-internal-issuer", Kind: "service", Role: "gateway", Key: servicePub},
			core.TrustedKey{ID: subject + "-user", Issuer: "ora-internal-issuer", Kind: "user", Key: userPub},
		)
		f.replicas = append(f.replicas, replicaKeys{subject: subject, service: serviceKey, user: userKey})
	}
	auth, e := core.NewAuthenticator("ora-cloud", trust)
	must(t, e)
	gin.SetMode(gin.TestMode)
	f.log, _ = zap.NewDevelopment()
	f.cloud = httptest.NewServer(router.New(cloudStore, auth, f.log))
	t.Cleanup(f.cloud.Close)
	return f
}

type gatewayInstance struct {
	f      *gatewayFixture
	server *httptest.Server
}

// newGateway starts the next Gateway replica sharing the fixture's PostgreSQL schema and Cloud.
// Each replica has its own service subject and keys, so multi-replica behavior is exercised through
// real HTTP. An empty upstream targets the fixture's Cloud.
func (f *gatewayFixture) newGateway(upstream string, burst int) *gatewayInstance {
	f.t.Helper()
	provider, e := github.New(&github.Options{ClientID: "client-id", ClientSecret: "provider-secret", AuthorizeURL: f.provider.server.URL + "/login/oauth/authorize", TokenURL: f.provider.server.URL + "/login/oauth/access_token", UserURL: f.provider.server.URL + "/user", HTTP: github.NewHTTPClient(5 * time.Second)})
	must(f.t, e)
	return f.newGatewayWithProvider(upstream, burst, gateway.ProviderGitHub, provider, 48*time.Hour)
}

func (f *gatewayFixture) newIDaaSGateway(provider *fakeIDaaS) *gatewayInstance {
	f.t.Helper()
	adapter, e := idaas.New(&idaas.Options{BaseURL: provider.server.URL, ClientID: "idaas-client", ClientSecret: "idaas-secret", DisplayNameField: "userName", HTTP: idaas.NewHTTPClient(5 * time.Second)})
	must(f.t, e)
	return f.newGatewayWithProvider("", 100, gateway.ProviderHuaweiIDaaS, adapter, gateway.DefaultIDaaSSessionLifetime)
}

func (f *gatewayFixture) newGatewayWithProvider(upstream string, burst int, providerName string, provider gateway.Authenticator, sessionTTL time.Duration) *gatewayInstance {
	f.t.Helper()
	if len(f.replicas) == 0 {
		f.t.Fatal("no replica identities left; extend the trusted set in setupGateway")
	}
	keys := f.replicas[0]
	f.replicas = f.replicas[1:]
	log := f.log
	if upstream == "" {
		upstream = f.cloud.URL
	}
	issuer, e := gateway.NewIssuer("ora-internal-issuer", "ora-cloud", keys.subject, gateway.SigningKey{ID: keys.subject + "-service", Key: keys.service}, gateway.SigningKey{ID: keys.subject + "-user", Key: keys.user}, time.Minute, time.Now)
	must(f.t, e)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	must(f.t, e)
	origin := "http://" + listener.Addr().String()
	login, e := gateway.NewLogin(f.store, map[string]gateway.Authenticator{providerName: provider}, f.pkceKey, origin+gateway.CallbackPath, 5*time.Minute, sessionTTL)
	must(f.t, e)
	upstreamURL, e := url.Parse(upstream)
	must(f.t, e)
	handler, e := gateway.NewHandler(&gateway.Options{
		Store: f.store, Login: login, Issuer: issuer, Limiter: gateway.NewRateLimiter(600, burst, 1000, time.Now),
		Upstream: upstreamURL, UpstreamTimeout: 2 * time.Second, PublicOrigin: origin,
		Cookies: gateway.CookiePolicy{Secure: false, CallbackPath: gateway.CallbackPath}, Log: log, Now: time.Now,
	})
	must(f.t, e)
	server := httptest.NewUnstartedServer(handler)
	must(f.t, server.Listener.Close())
	server.Listener = listener
	server.Start()
	f.t.Cleanup(server.Close)
	return &gatewayInstance{f: f, server: server}
}

// browser is a cookie-holding client that never follows redirects, so every hop is asserted.
func (g *gatewayInstance) browser() *http.Client {
	jar, e := cookiejar.New(nil)
	must(g.f.t, e)
	return &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// reply is a fully consumed HTTP response: status, headers, cookies, and decoded JSON body.
type reply struct {
	StatusCode int
	Header     http.Header
	cookies    []*http.Cookie
	body       core.Object
}

func (r reply) Cookies() []*http.Cookie { return r.cookies }

func consume(t *testing.T, resp *http.Response) reply {
	t.Helper()
	defer resp.Body.Close()
	out := core.Object{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, cookies: resp.Cookies(), body: out}
}

func (g *gatewayInstance) do(client *http.Client, method, path string, body any, headers map[string]string) (reply, core.Object) {
	g.f.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, e := json.Marshal(body)
		must(g.f.t, e)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, e := http.NewRequestWithContext(context.Background(), method, g.server.URL+path, reader)
	must(g.f.t, e)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := client.Do(req)
	must(g.f.t, e)
	r := consume(g.f.t, resp)
	return r, r.body
}

func (g *gatewayInstance) origin() map[string]string {
	return map[string]string{"Origin": g.server.URL}
}

// startLogin runs the start step and the provider hop, returning the callback URL the provider
// redirects the browser to.
func (g *gatewayInstance) startLogin(client *http.Client, returnTo string) string {
	g.f.t.Helper()
	resp, out := g.do(client, http.MethodPost, gateway.LoginPath, map[string]string{"returnTo": returnTo}, g.origin())
	if resp.StatusCode != http.StatusOK || out.S("authorizationUrl") == "" {
		g.f.t.Fatalf("login start: %d %v", resp.StatusCode, out)
	}
	for _, c := range resp.Cookies() {
		if c.Name == "ora_login" && (c.Path != gateway.CallbackPath || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 300) {
			g.f.t.Fatalf("attempt cookie attributes: %+v", c)
		}
	}
	hopReq, e := http.NewRequestWithContext(context.Background(), http.MethodGet, out.S("authorizationUrl"), http.NoBody)
	must(g.f.t, e)
	hopResp, e := client.Do(hopReq)
	must(g.f.t, e)
	hop := consume(g.f.t, hopResp)
	if hop.StatusCode != http.StatusFound || hop.Header.Get("Location") == "" {
		g.f.t.Fatalf("provider authorize hop: %d", hop.StatusCode)
	}
	return hop.Header.Get("Location")
}

func (g *gatewayInstance) callback(client *http.Client, location string) reply {
	g.f.t.Helper()
	req, e := http.NewRequestWithContext(context.Background(), http.MethodGet, location, http.NoBody)
	must(g.f.t, e)
	resp, e := client.Do(req)
	must(g.f.t, e)
	return consume(g.f.t, resp)
}

func callbackRace(t *testing.T, client *http.Client, location string) []int {
	t.Helper()
	callbackURL, e := url.Parse(location)
	must(t, e)
	cookies := client.Jar.Cookies(callbackURL)
	requests := make([]*http.Request, 2)
	for i := range requests {
		requests[i], e = http.NewRequestWithContext(context.Background(), http.MethodGet, location, http.NoBody)
		must(t, e)
		for _, cookie := range cookies {
			requests[i].AddCookie(cookie)
		}
	}
	raceClient := *client
	raceClient.Jar = nil
	var wg sync.WaitGroup
	results := make([]int, 2)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, e := raceClient.Do(requests[i])
			if e != nil {
				results[i] = -1
				return
			}
			results[i] = consume(t, resp).StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	slices.Sort(results)
	return results
}

func (g *gatewayInstance) login(client *http.Client, returnTo string) reply {
	g.f.t.Helper()
	resp := g.callback(client, g.startLogin(client, returnTo))
	if resp.StatusCode != http.StatusSeeOther {
		g.f.t.Fatalf("callback must redirect after login: %d", resp.StatusCode)
	}
	return resp
}

func (f *gatewayFixture) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	must(f.t, f.pool.QueryRow(q, args...).Scan(&n))
	return n
}

// Evidence for specs/test-cases/cloud/identity-access/proxy-and-credentials.md
// (#cloud-only-receives-gateway-issued-short-lived-credentials,
// #internal-routes-and-cross-site-mutations-never-reach-cloud,
// #cloud-authorization-decisions-pass-through-unchanged) and
// browser-session.md#revocation-is-immediate-on-every-replica-and-idempotent.
func TestGatewayLoginProxyLogoutAndCredentialBoundaries(t *testing.T) {
	f := setupGateway(t)
	gw := f.newGateway("", 100)
	browser := gw.browser()

	resp := gw.login(browser, "/projects?tab=ops")
	if resp.Header.Get("Location") != "/projects?tab=ops" {
		t.Fatalf("callback must redirect to the stored return_to, got %q", resp.Header.Get("Location"))
	}
	var sessionCookie *http.Cookie
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "ora_session":
			sessionCookie = c
		case "ora_login":
			if c.MaxAge != -1 {
				t.Fatalf("attempt cookie must be cleared by the callback: %+v", c)
			}
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Path != "/" || sessionCookie.MaxAge > 48*60*60 || sessionCookie.MaxAge < 48*60*60-60 {
		t.Fatalf("session cookie attributes or lifetime wrong: %+v", sessionCookie)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE token_hash=$1", gateway.Digest(sessionCookie.Value)); n != 1 {
		t.Fatalf("database must hold exactly the digest of the browser token, found %d rows", n)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE encode(token_hash,'escape') LIKE '%' || $1 || '%'", sessionCookie.Value[:16]); n != 0 {
		t.Fatal("raw session token must never be stored")
	}
	if n := f.count("SELECT count(*) FROM gateway_login_attempts WHERE consumed_at IS NULL"); n != 0 {
		t.Fatalf("attempt must be consumed, %d open", n)
	}

	// Proxy: Cloud sees Gateway-issued credentials, creates the user JIT, and answers as the user.
	resp, me := gw.do(browser, http.MethodGet, "/api/v1/me", nil, nil)
	if resp.StatusCode != http.StatusOK || me.S("displayName") != "Ray Zhang" {
		t.Fatalf("/api/v1/me through gateway: %d %v", resp.StatusCode, me)
	}
	if n := f.count("SELECT count(*) FROM user_identities WHERE source='github.com' AND subject='71996633'"); n != 1 {
		t.Fatalf("Cloud identity mapping must use github.com + numeric id, found %d", n)
	}
	// Forged internal headers, caller, upstream selection: all dropped or ignored.
	forged := map[string]string{"Authorization": "Bearer forged", "X-Ora-User-Token": "forged", "X-Forwarded-Host": "evil.example", "X-Forwarded-For": "10.0.0.1"}
	req, e := http.NewRequestWithContext(context.Background(), http.MethodGet, gw.server.URL+"/api/v1/me", http.NoBody)
	must(t, e)
	for k, v := range forged {
		req.Header.Set(k, v)
	}
	req.AddCookie(sessionCookie)
	forgedResp, e := (&http.Client{}).Do(req)
	must(t, e)
	if forged := consume(t, forgedResp); forged.StatusCode != http.StatusOK {
		t.Fatalf("forged headers must be stripped, not relayed: %d", forged.StatusCode)
	}
	if resp, out := gw.do(&http.Client{}, http.MethodGet, "/api/v1/me", nil, map[string]string{"Authorization": "Bearer forged"}); resp.StatusCode != http.StatusUnauthorized || out.S("code") != "unauthenticated" {
		t.Fatalf("no session cookie must be unauthenticated regardless of headers: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, "/internal/v1/access", core.Object{}, gw.origin()); resp.StatusCode != http.StatusNotFound || out.S("code") != "not_found" {
		t.Fatalf("internal API must never be proxied: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, "/api/v1/tenants/x/projects", core.Object{}, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "origin_forbidden" {
		t.Fatalf("cross-site mutation must be rejected before reaching Cloud: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, "/api/v1/tenants/x/projects", core.Object{}, map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != http.StatusForbidden || out.S("code") != "origin_forbidden" {
		t.Fatalf("foreign origin must be rejected: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, "/api/v1/tenants/x/projects", core.Object{}, map[string]string{"Sec-Fetch-Site": "same-origin"}); resp.StatusCode == http.StatusForbidden && out.S("code") == "origin_forbidden" {
		t.Fatalf("same-origin fetch metadata must be accepted when Origin is absent: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodGet, "/api/v1/me/tenants", nil, nil); resp.StatusCode != http.StatusOK || out["items"] == nil {
		t.Fatalf("GET relays without origin proof: %d %v", resp.StatusCode, out)
	}

	// Disabled in Cloud: the session stays, Cloud's current state decides.
	_, e = f.pool.Exec("UPDATE users SET status='disabled' WHERE id=(SELECT user_id FROM user_identities WHERE source='github.com' AND subject='71996633')")
	must(t, e)
	if resp, out := gw.do(browser, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "user_disabled" {
		t.Fatalf("Cloud denial must pass through unchanged: %d %v", resp.StatusCode, out)
	}
	_, e = f.pool.Exec("UPDATE users SET status='active'")
	must(t, e)

	// Logout: same-origin, idempotent, immediate.
	if resp, out := gw.do(browser, http.MethodPost, gateway.LogoutPath, nil, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "origin_forbidden" {
		t.Fatalf("logout without origin proof: %d %v", resp.StatusCode, out)
	}
	if resp, _ := gw.do(browser, http.MethodPost, gateway.LogoutPath, nil, gw.origin()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, out := gw.do(browser, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusUnauthorized || out.S("code") != "unauthenticated" {
		t.Fatalf("revoked session must be unauthenticated: %d %v", resp.StatusCode, out)
	}
	if resp, _ := gw.do(browser, http.MethodPost, gateway.LogoutPath, nil, gw.origin()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second logout must be idempotent: %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE revoked_reason='logout' AND revoked_at IS NOT NULL"); n != 1 {
		t.Fatalf("logout must persist a bounded reason, found %d", n)
	}
}

// Evidence for specs/test-cases/cloud/identity-access/login-attempt.md
// (#huawei-idaas-identity-uses-corporate-uuid-and-discards-extra-profile-data,
// #provider-rejection-never-creates-a-session) and
// browser-session.md#revocation-is-immediate-on-every-replica-and-idempotent.
func TestGatewayIDaaSLoginIdentityAndSessionLifetime(t *testing.T) {
	f := setupGateway(t)
	provider := newFakeIDaaS(t)
	gw := f.newIDaaSGateway(provider)
	browser := gw.browser()

	resp := gw.login(browser, "/projects?tab=internal")
	if resp.Header.Get("Location") != "/projects?tab=internal" {
		t.Fatalf("IDaaS callback must restore the target, got %q", resp.Header.Get("Location"))
	}
	resp, me := gw.do(browser, http.MethodGet, "/api/v1/me", nil, nil)
	if resp.StatusCode != http.StatusOK || me.S("displayName") != "Wang Longan" {
		t.Fatalf("IDaaS /me: %d %v", resp.StatusCode, me)
	}
	if n := f.count("SELECT count(*) FROM user_identities WHERE source='huawei-corp' AND subject='w00576782'"); n != 1 {
		t.Fatalf("IDaaS identity must use huawei-corp + uuid, found %d", n)
	}
	if n := f.count("SELECT count(*) FROM tenant_memberships"); n != 0 {
		t.Fatalf("external login must not grant membership, found %d", n)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE source='huawei-corp' AND subject='w00576782' AND display_name='Wang Longan'"); n != 1 {
		t.Fatalf("session must retain only minimized identity fields, found %d", n)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE token_hash IN ($1,$2)", gateway.Digest("idaas-access-token"), gateway.Digest("idaas-refresh-token")); n != 0 {
		t.Fatal("provider tokens must not become browser session tokens")
	}
	var lifetime int
	must(t, f.pool.QueryRow("SELECT EXTRACT(EPOCH FROM (expires_at-created_at))::int FROM gateway_sessions WHERE source='huawei-corp'").Scan(&lifetime))
	if lifetime != int(gateway.DefaultIDaaSSessionLifetime/time.Second) {
		t.Fatalf("IDaaS session lifetime = %ds", lifetime)
	}
	provider.mu.Lock()
	if len(provider.lastToken) != 4 || provider.lastToken["grant_type"] != "authorization_code" || provider.lastToken["client_id"] != "idaas-client" || provider.lastToken["client_secret"] != "idaas-secret" || provider.lastToken["code"] == "" || provider.lastToken["code_verifier"] != "" || provider.lastToken["redirect_uri"] != "" || len(provider.lastUser) != 1 || provider.lastUser["access_token"] != "idaas-access-token" {
		t.Fatal("IDaaS exchange must bind the documented client_secret_post fields and userinfo access_token")
	}
	provider.mu.Unlock()

	provider.mu.Lock()
	provider.userName = "Changed Provider Name"
	provider.mu.Unlock()
	secondBrowser := gw.browser()
	if second := gw.login(secondBrowser, "/"); second.StatusCode != http.StatusSeeOther {
		t.Fatalf("repeat IDaaS login: %d", second.StatusCode)
	}
	if second, repeatedMe := gw.do(secondBrowser, http.MethodGet, "/api/v1/me", nil, nil); second.StatusCode != http.StatusOK || repeatedMe.S("displayName") != "Wang Longan" {
		t.Fatalf("repeat login must preserve the JIT display-name snapshot: %d %v", second.StatusCode, repeatedMe)
	}
	if n := f.count("SELECT count(*) FROM user_identities WHERE source='huawei-corp' AND subject='w00576782'"); n != 1 {
		t.Fatalf("repeat IDaaS login must reuse one identity, found %d", n)
	}
	sessionsBefore := f.count("SELECT count(*) FROM gateway_sessions")

	withoutCookie := gw.browser()
	withoutCookieLocation := gw.startLogin(withoutCookie, "/")
	if failed := gw.callback(gw.browser(), withoutCookieLocation); failed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("IDaaS callback without attempt cookie must fail: %d", failed.StatusCode)
	}
	wrongStateBrowser := gw.browser()
	wrongStateLocation := gw.startLogin(wrongStateBrowser, "/")
	tampered, e := url.Parse(wrongStateLocation)
	must(t, e)
	query := tampered.Query()
	query.Set("state", "wrong-state")
	tampered.RawQuery = query.Encode()
	if failed := gw.callback(wrongStateBrowser, tampered.String()); failed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("IDaaS callback with wrong state must fail: %d", failed.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions"); n != sessionsBefore {
		t.Fatalf("failed IDaaS callbacks created %d sessions", n-sessionsBefore)
	}

	replayBrowser := gw.browser()
	replayLocation := gw.startLogin(replayBrowser, "/")
	if first := gw.callback(replayBrowser, replayLocation); first.StatusCode != http.StatusSeeOther {
		t.Fatalf("first IDaaS callback failed: %d", first.StatusCode)
	}
	if replayed := gw.callback(replayBrowser, replayLocation); replayed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed IDaaS callback must fail: %d", replayed.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions"); n != sessionsBefore+1 {
		t.Fatalf("IDaaS replay created an unexpected session count: %d", n)
	}

	provider.mu.Lock()
	provider.reusable = true
	provider.mu.Unlock()
	racer := gw.browser()
	raceLocation := gw.startLogin(racer, "/")
	if results := callbackRace(t, racer, raceLocation); results[0] != http.StatusSeeOther || results[1] != http.StatusUnauthorized {
		t.Fatalf("exactly one concurrent IDaaS callback may win: %v", results)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions"); n != sessionsBefore+2 {
		t.Fatalf("concurrent IDaaS callback created an unexpected session count: %d", n)
	}
}

// Evidence for specs/test-cases/cloud/identity-access/login-attempt.md
// (#login-start-requires-same-origin-proof-and-writes-one-bound-attempt,
// #callback-consumes-an-attempt-at-most-once).
func TestGatewayLoginAttemptReplayStateAndConcurrentConsume(t *testing.T) {
	f := setupGateway(t)
	gw := f.newGateway("", 100)

	browser := gw.browser()
	location := gw.startLogin(browser, "/")
	gatewayURL, e := url.Parse(gw.server.URL + gateway.CallbackPath)
	must(t, e)
	for _, c := range browser.Jar.Cookies(gatewayURL) {
		if c.Name == "ora_login" && f.count("SELECT count(*) FROM gateway_login_attempts WHERE encode(secret_hash,'escape') LIKE '%' || $1 || '%'", c.Value[:16]) != 0 {
			t.Fatal("raw attempt secret must never be stored")
		}
	}
	if n := f.count("SELECT count(*) FROM gateway_login_attempts WHERE octet_length(secret_hash)<>32 OR octet_length(state_hash)<>32"); n != 0 {
		t.Fatalf("attempt digests must be fixed-size SHA-256, %d rows are not", n)
	}
	if resp := gw.callback(gw.browser(), location); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback without the attempt cookie must fail: %d", resp.StatusCode)
	}
	tampered, e := url.Parse(location)
	must(t, e)
	q := tampered.Query()
	q.Set("state", "wrong-state")
	tampered.RawQuery = q.Encode()
	if resp := gw.callback(browser, tampered.String()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong state must fail: %d", resp.StatusCode)
	}
	// A failed callback clears the attempt cookie, so the original callback can no longer complete
	// either: an injected callback ends the pending attempt instead of leaving it open.
	if resp := gw.callback(browser, location); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("attempt must be unusable after a failed callback cleared its cookie: %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions"); n != 0 {
		t.Fatalf("no session may exist yet, found %d", n)
	}
	location = gw.startLogin(browser, "/")
	if resp := gw.callback(browser, location); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid callback: %d", resp.StatusCode)
	}
	if resp := gw.callback(browser, location); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed callback must fail: %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions"); n != 1 {
		t.Fatalf("replay must not create a second session, found %d", n)
	}
	if resp, out := gw.do(browser, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "github", "returnTo": "https://evil.example/"}, gw.origin()); resp.StatusCode != http.StatusBadRequest || out.S("code") != "invalid_return_to" {
		t.Fatalf("absolute return_to must fail before any attempt exists: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "github", "returnTo": "//evil.example/"}, gw.origin()); resp.StatusCode != http.StatusBadRequest || out.S("code") != "invalid_return_to" {
		t.Fatalf("protocol-relative return_to must fail: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "github", "returnTo": "/\\evil.example"}, gw.origin()); resp.StatusCode != http.StatusBadRequest || out.S("code") != "invalid_return_to" {
		t.Fatalf("backslash return_to must fail: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "github"}, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "origin_forbidden" {
		t.Fatalf("start without origin proof: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodGet, gateway.LoginPath, nil, gw.origin()); resp.StatusCode != http.StatusMethodNotAllowed || out.S("code") != "method_not_allowed" {
		t.Fatalf("GET start must not exist: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "ldap"}, gw.origin()); resp.StatusCode != http.StatusBadRequest || out.S("code") != "unknown_provider" {
		t.Fatalf("unknown provider: %d %v", resp.StatusCode, out)
	}
	if resp, out := gw.do(browser, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "github", "extra": "x"}, gw.origin()); resp.StatusCode != http.StatusBadRequest || out.S("code") != "invalid_json" {
		t.Fatalf("unknown field: %d %v", resp.StatusCode, out)
	}

	// Two callbacks race on one attempt with a code the provider lets both redeem: the database
	// consume step must still admit exactly one session.
	f.provider.mu.Lock()
	f.provider.reusable = true
	f.provider.mu.Unlock()
	racer := gw.browser()
	location = gw.startLogin(racer, "/")
	results := callbackRace(t, racer, location)
	if results[0] != http.StatusSeeOther || results[1] != http.StatusUnauthorized {
		t.Fatalf("exactly one concurrent callback may win: %v", results)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE revoked_at IS NULL"); n != 2 {
		t.Fatalf("expected the earlier session plus one from the race, found %d live sessions", n)
	}
}

// Evidence for specs/test-cases/cloud/identity-access/browser-session.md
// (#session-validity-is-decided-only-by-postgresql-state-and-time,
// #revocation-is-immediate-on-every-replica-and-idempotent,
// #cleanup-is-bounded-and-preserves-live-rows).
func TestGatewaySessionsAcrossReplicasExpiryRevocationAndCleanup(t *testing.T) {
	f := setupGateway(t)
	replicaA := f.newGateway("", 100)
	replicaB := f.newGateway("", 100)
	phone := replicaA.browser()
	laptop := replicaA.browser()
	replicaA.login(phone, "/")
	replicaA.login(laptop, "/")
	if n := f.count("SELECT count(*) FROM gateway_sessions WHERE source='github.com' AND subject='71996633' AND revoked_at IS NULL"); n != 2 {
		t.Fatalf("one identity may hold two device sessions, found %d", n)
	}
	// A session created by replica A resolves on replica B: PostgreSQL is the only authority.
	phoneB := &http.Client{Jar: phone.Jar, CheckRedirect: phone.CheckRedirect}
	if resp, out := replicaB.do(phoneB, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("replica B must resolve replica A's session: %d %v", resp.StatusCode, out)
	}
	// Device logout is independent.
	if resp, _ := replicaA.do(phone, http.MethodPost, gateway.LogoutPath, nil, replicaA.origin()); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout phone: %d", resp.StatusCode)
	}
	if resp, _ := replicaA.do(laptop, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("laptop must survive phone logout: %d", resp.StatusCode)
	}
	if resp, _ := replicaB.do(phoneB, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revocation must apply on every replica: %d", resp.StatusCode)
	}
	// Absolute expiry decided by database time; access never extends it.
	var expiryBefore, expiryAfter time.Time
	must(t, f.pool.QueryRow("SELECT expires_at FROM gateway_sessions WHERE revoked_at IS NULL").Scan(&expiryBefore))
	for range 3 {
		if resp, _ := replicaA.do(laptop, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("session before expiry: %d", resp.StatusCode)
		}
	}
	must(t, f.pool.QueryRow("SELECT expires_at FROM gateway_sessions WHERE revoked_at IS NULL").Scan(&expiryAfter))
	if !expiryAfter.Equal(expiryBefore) {
		t.Fatalf("reads must not slide the absolute expiry: %s -> %s", expiryBefore, expiryAfter)
	}
	_, e := f.pool.Exec("UPDATE gateway_sessions SET expires_at=created_at+interval '1 second' WHERE revoked_at IS NULL")
	must(t, e)
	_, e = f.pool.Exec("UPDATE gateway_sessions SET expires_at=clock_timestamp()-interval '1 millisecond' WHERE revoked_at IS NULL")
	must(t, e)
	if resp, _ := replicaA.do(laptop, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session must be unauthenticated: %d", resp.StatusCode)
	}
	if resp, _ := replicaB.do(&http.Client{Jar: laptop.Jar, CheckRedirect: laptop.CheckRedirect}, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expiry must apply on every replica: %d", resp.StatusCode)
	}
	// RevokeIdentity ends every live session of the identity, idempotently.
	tablet := replicaB.browser()
	replicaB.login(tablet, "/")
	replicaB.login(replicaB.browser(), "/")
	n, e := f.store.RevokeIdentity(context.Background(), "github.com", "71996633", gateway.RevokeIdentity)
	must(t, e)
	if n != 2 {
		t.Fatalf("expected 2 sessions revoked by identity, got %d", n)
	}
	if n, e = f.store.RevokeIdentity(context.Background(), "github.com", "71996633", gateway.RevokeIdentity); e != nil || n != 0 {
		t.Fatalf("identity revoke must be idempotent: %d %v", n, e)
	}
	if resp, _ := replicaB.do(tablet, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("identity revocation must deny new entry: %d", resp.StatusCode)
	}
	// Cleanup is bounded and never touches live rows.
	replicaA.login(replicaA.browser(), "/")
	before := f.count("SELECT count(*) FROM gateway_sessions")
	deleted, e := f.store.Cleanup(context.Background(), 24*time.Hour, 100)
	must(t, e)
	if deleted != 0 || f.count("SELECT count(*) FROM gateway_sessions") != before {
		t.Fatalf("rows inside the retention window must stay: deleted %d", deleted)
	}
	deleted, e = f.store.Cleanup(context.Background(), 0, 2)
	must(t, e)
	if deleted > 4 {
		t.Fatalf("cleanup must respect the batch bound per table, deleted %d", deleted)
	}
	for {
		more, e := f.store.Cleanup(context.Background(), 0, 2)
		must(t, e)
		if more == 0 {
			break
		}
	}
	if live := f.count("SELECT count(*) FROM gateway_sessions"); live != 1 || f.count("SELECT count(*) FROM gateway_sessions WHERE revoked_at IS NULL AND expires_at>clock_timestamp()") != 1 {
		t.Fatalf("only the live session may remain after cleanup, found %d", live)
	}
	if open := f.count("SELECT count(*) FROM gateway_login_attempts WHERE expires_at>clock_timestamp()"); open == 0 {
		t.Fatal("unexpired attempts must survive cleanup")
	}
}

// Evidence for specs/test-cases/cloud/identity-access/login-attempt.md
// (#provider-rejection-never-creates-a-session,
// #github-identity-is-keyed-by-numeric-id-within-cloud-field-limits) and
// proxy-and-credentials.md#cloud-outage-is-a-stable-failure-that-keeps-sessions.
func TestGatewayProviderAndUpstreamFailuresKeepFactsStraight(t *testing.T) {
	f := setupGateway(t)
	gw := f.newGateway("", 100)
	// A name longer than Cloud accepts is truncated at the Gateway; login still succeeds.
	f.provider.mu.Lock()
	f.provider.userName = strings.Repeat("名", 120)
	f.provider.mu.Unlock()
	browser := gw.browser()
	gw.login(browser, "/")
	resp, me := gw.do(browser, http.MethodGet, "/api/v1/me", nil, nil)
	if resp.StatusCode != http.StatusOK || len(me.S("displayName")) > gateway.MaxDisplayNameLength || me.S("displayName") != strings.Repeat("名", 66) {
		t.Fatalf("long provider name must be truncated on a rune boundary before Cloud: %d %q", resp.StatusCode, me.S("displayName"))
	}
	// A renamed provider account still maps to the same Cloud user.
	f.provider.mu.Lock()
	f.provider.userName, f.provider.userLogin = "Renamed", "renamed"
	f.provider.mu.Unlock()
	gw.login(gw.browser(), "/")
	if n := f.count("SELECT count(*) FROM users"); n != 1 {
		t.Fatalf("renaming must not split the identity, found %d users", n)
	}
	// PKCE is enforced by the provider: a replica holding a different derivation key reproduces a
	// different verifier, so the exchange is rejected and no session appears even though the attempt
	// row and cookie are valid.
	sessionsBefore := f.count("SELECT count(*) FROM gateway_sessions")
	original := f.pkceKey
	f.pkceKey = bytes.Repeat([]byte{7}, 32)
	misconfigured := f.newGateway("", 100)
	f.pkceKey = original
	stranger := gw.browser()
	location, e := url.Parse(gw.startLogin(stranger, "/"))
	must(t, e)
	elsewhere, e := url.Parse(misconfigured.server.URL)
	must(t, e)
	location.Host = elsewhere.Host
	if resp := misconfigured.callback(stranger, location.String()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("verifier mismatch must be login_failed: %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_sessions"); n != sessionsBefore {
		t.Fatalf("PKCE rejection must not create a session, found %d new", n-sessionsBefore)
	}
	// Force the provider to reject: a callback code the provider never issued.
	tampered, e := url.Parse(gw.startLogin(browser, "/"))
	must(t, e)
	q := tampered.Query()
	q.Set("code", "never-issued")
	tampered.RawQuery = q.Encode()
	if resp := gw.callback(browser, tampered.String()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("provider rejection must be login_failed: %d", resp.StatusCode)
	}
	if n := f.count("SELECT count(*) FROM gateway_login_attempts WHERE consumed_at IS NULL AND expires_at>clock_timestamp()"); n != 2 {
		t.Fatalf("rejected exchanges must leave their attempts unconsumed, found %d open", n)
	}
	// Cloud failure: stable 502, session untouched.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	broken := f.newGateway(deadURL, 100)
	victim := broken.browser()
	broken.login(victim, "/")
	if resp, out := broken.do(victim, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusBadGateway || out.S("code") != "upstream_unavailable" {
		t.Fatalf("Cloud outage must be a stable upstream failure: %d %v", resp.StatusCode, out)
	}
	healthy := f.newGateway("", 100)
	if resp, _ := healthy.do(&http.Client{Jar: victim.Jar, CheckRedirect: victim.CheckRedirect}, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("Cloud outage must not revoke the session: %d", resp.StatusCode)
	}
	// Rate limit rejects before writing an attempt.
	limited := f.newGateway("", 2)
	attempts := f.count("SELECT count(*) FROM gateway_login_attempts")
	client := limited.browser()
	codes := []int{}
	for range 3 {
		resp, _ := limited.do(client, http.MethodPost, gateway.LoginPath, map[string]string{"provider": "github"}, limited.origin())
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("burst of 2 then 429: %v", codes)
	}
	if f.count("SELECT count(*) FROM gateway_login_attempts") != attempts+2 {
		t.Fatal("a rate-limited start must not write an attempt")
	}
}

// Evidence for specs/test-cases/cloud/identity-access/browser-session.md
// #schema-constraints-reject-invalid-session-and-attempt-rows.
func TestGatewaySchemaConstraints(t *testing.T) {
	f := setupGateway(t)
	hash := gateway.Digest("x")
	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{"session beyond 90 days", "INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at) VALUES(gen_random_uuid(),$1,'github.com','1',now()+interval '91 days')", []any{hash}},
		{"session expiring before creation", "INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at) VALUES(gen_random_uuid(),$1,'github.com','1',now()-interval '1 second')", []any{hash}},
		{"short token digest", "INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at) VALUES(gen_random_uuid(),$1,'github.com','1',now()+interval '1 day')", []any{[]byte("short")}},
		{"revoked without reason", "INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at,revoked_at) VALUES(gen_random_uuid(),$1,'github.com','1',now()+interval '1 day',now())", []any{hash}},
		{"unknown revoke reason", "INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at,revoked_at,revoked_reason) VALUES(gen_random_uuid(),$1,'github.com','1',now()+interval '1 day',now(),'because')", []any{hash}},
		{"source over 128", "INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at) VALUES(gen_random_uuid(),$1,$2,'1',now()+interval '1 day')", []any{hash, strings.Repeat("s", 129)}},
		{"display name over 200", "INSERT INTO gateway_sessions(id,token_hash,source,subject,display_name,expires_at) VALUES(gen_random_uuid(),$1,'github.com','1',$2,now()+interval '1 day')", []any{hash, strings.Repeat("n", 201)}},
		{"attempt beyond one hour", "INSERT INTO gateway_login_attempts(id,secret_hash,state_hash,provider,return_to,expires_at) VALUES(gen_random_uuid(),$1,$2,'github','/',now()+interval '2 hours')", []any{hash, gateway.Digest("y")}},
		{"attempt absolute return_to", "INSERT INTO gateway_login_attempts(id,secret_hash,state_hash,provider,return_to,expires_at) VALUES(gen_random_uuid(),$1,$2,'github','https://evil.example',now()+interval '5 minutes')", []any{hash, gateway.Digest("y")}},
		{"attempt protocol-relative return_to", "INSERT INTO gateway_login_attempts(id,secret_hash,state_hash,provider,return_to,expires_at) VALUES(gen_random_uuid(),$1,$2,'github','//evil.example',now()+interval '5 minutes')", []any{hash, gateway.Digest("y")}},
		{"attempt backslash return_to", "INSERT INTO gateway_login_attempts(id,secret_hash,state_hash,provider,return_to,expires_at) VALUES(gen_random_uuid(),$1,$2,'github',E'/\\\\evil.example',now()+interval '5 minutes')", []any{hash, gateway.Digest("y")}},
	}
	for _, tc := range cases {
		if _, e := f.pool.Exec(tc.query, tc.args...); e == nil {
			t.Errorf("%s: constraint must reject the row", tc.name)
		}
	}
	if _, e := f.pool.Exec("INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at) VALUES(gen_random_uuid(),$1,'github.com','1',now()+interval '90 days')", hash); e != nil {
		t.Fatalf("90 days exactly must be accepted: %v", e)
	}
	if _, e := f.pool.Exec("INSERT INTO gateway_sessions(id,token_hash,source,subject,expires_at) VALUES(gen_random_uuid(),$1,'github.com','2',now()+interval '1 day')", hash); e == nil {
		t.Fatal("token digest must be unique")
	}
}
