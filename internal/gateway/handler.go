package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Options wires the Gateway HTTP boundary. Every dependency is injected; the handler holds no
// authoritative state.
type Options struct {
	Store           *Store
	Login           *Login
	Issuer          *Issuer
	Limiter         *RateLimiter
	Upstream        *url.URL
	UpstreamTimeout time.Duration
	PublicOrigin    string
	Cookies         CookiePolicy
	Log             *zap.Logger
	Now             func() time.Time
	// Web, when set, serves the built frontend for GET/HEAD requests no route matches.
	Web *Web
}

// Route paths of the authentication boundary. Everything under /api/v1 is proxied; /internal/v1
// deliberately has no route and falls through to 404.
const (
	LoginPath     = "/auth/login"
	LogoutPath    = "/auth/logout"
	ProvidersPath = "/auth/providers"
	ProxyPath     = "/api/v1/*path"
	// maxBodyBytes matches Cloud's request body limit so the Gateway never relays more than Cloud accepts.
	maxBodyBytes = 64 << 10
)

type handler struct {
	Options
	proxy *proxy
}

// fault is the stable, bounded error shape shared with Cloud responses.
type fault struct {
	code   string
	status int
}

// NewHandler builds the Gin engine for the Gateway. It validates the options up front so a
// misconfigured process fails at startup.
func NewHandler(o *Options) (*gin.Engine, error) {
	if o == nil || o.Store == nil || o.Login == nil || o.Issuer == nil || o.Limiter == nil || o.Upstream == nil || o.Log == nil {
		return nil, errors.New("store, login, issuer, limiter, upstream and logger are required")
	}
	if o.PublicOrigin == "" || o.Cookies.CallbackPath == "" || o.UpstreamTimeout <= 0 {
		return nil, errors.New("public origin, callback path and upstream timeout are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	h := &handler{Options: *o, proxy: newProxy(o.Upstream, o.UpstreamTimeout)}
	r := gin.New()
	r.Use(h.recovery)
	r.GET("/healthz", h.health)
	r.GET(ProvidersPath, h.providers)
	r.POST(LoginPath, h.login)
	r.GET(o.Cookies.CallbackPath+"/:provider", h.callback)
	r.POST(LogoutPath, h.logout)
	r.Any(ProxyPath, h.relay)
	r.NoRoute(func(c *gin.Context) {
		if h.Web != nil && h.Web.serve(c.Writer, c.Request) {
			return
		}
		h.fail(c, fault{"not_found", http.StatusNotFound})
	})
	r.NoMethod(func(c *gin.Context) { h.fail(c, fault{"method_not_allowed", http.StatusMethodNotAllowed}) })
	r.HandleMethodNotAllowed = true
	return r, nil
}

func (h *handler) recovery(c *gin.Context) {
	id := uuid.NewString()
	c.Set("requestId", id)
	c.Header("X-Request-Id", id)
	defer func() {
		if recovered := recover(); recovered != nil {
			h.Log.Error("request panic", zap.String("requestId", id), zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
			h.fail(c, fault{"internal_error", http.StatusInternalServerError})
		}
	}()
	c.Next()
}

func (h *handler) fail(c *gin.Context, f fault) {
	c.AbortWithStatusJSON(f.status, gin.H{"code": f.code, "params": gin.H{}, "requestId": c.GetString("requestId")})
}

func (h *handler) health(c *gin.Context) {
	if e := h.Store.pool.PingContext(c.Request.Context()); e != nil {
		h.fail(c, fault{"database_unavailable", http.StatusServiceUnavailable})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// clientKey identifies the rate-limit bucket. Only the transport peer address is trusted; forwarded
// headers are attacker-controlled unless a trusted ingress rewrites them, which is deployment policy.
func clientKey(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return r.RemoteAddr
	}
	return host
}

type loginRequest struct {
	Provider string `json:"provider"`
	ReturnTo string `json:"returnTo"`
}

// providers names the logins this deployment offers and which one a start request without a
// provider uses. It reveals nothing beyond what the sign-in screen must show and takes no input,
// so it needs no origin proof.
func (h *handler) providers(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"providers": h.Login.Providers(), "default": h.Login.DefaultProvider()})
}

// login starts an external login. It is a same-origin JSON POST so it cannot be triggered by a
// cross-site navigation, image, or prefetch, and it is rate limited before any attempt is written.
func (h *handler) login(c *gin.Context) {
	if !SameOrigin(c.Request, h.PublicOrigin) {
		h.fail(c, fault{"origin_forbidden", http.StatusForbidden})
		return
	}
	if !h.Limiter.Allow(clientKey(c.Request)) {
		h.fail(c, fault{"rate_limited", http.StatusTooManyRequests})
		return
	}
	var req loginRequest
	if !strings.HasPrefix(c.ContentType(), "application/json") || !decodeStrict(c, &req) {
		h.fail(c, fault{"invalid_json", http.StatusBadRequest})
		return
	}
	started, e := h.Login.Start(c.Request.Context(), req.Provider, req.ReturnTo)
	switch {
	case errors.Is(e, ErrUnknownProvider):
		h.fail(c, fault{"unknown_provider", http.StatusBadRequest})
		return
	case errors.Is(e, ErrLoginFailed):
		h.fail(c, fault{"invalid_return_to", http.StatusBadRequest})
		return
	case e != nil:
		h.Log.Error("login start failed", zap.String("requestId", c.GetString("requestId")), zap.Error(e))
		h.fail(c, fault{"login_unavailable", http.StatusServiceUnavailable})
		return
	}
	http.SetCookie(c.Writer, h.Cookies.AttemptCookie(started.AttemptSecret, h.Login.attemptTTL))
	c.JSON(http.StatusOK, gin.H{"authorizationUrl": started.AuthorizationURL})
}

// callback completes the provider redirect. Every failure clears the attempt cookie and returns the
// same login_failed shape; only unexpected infrastructure errors are logged, never secrets.
func (h *handler) callback(c *gin.Context) {
	http.SetCookie(c.Writer, h.Cookies.ClearAttemptCookie())
	if !h.Limiter.Allow(clientKey(c.Request)) {
		h.fail(c, fault{"rate_limited", http.StatusTooManyRequests})
		return
	}
	secret := ""
	if cookie, e := c.Request.Cookie(h.Cookies.AttemptCookieName()); e == nil {
		secret = cookie.Value
	}
	completed, e := h.Login.Callback(c.Request.Context(), c.Param("provider"), secret, c.Query("state"), c.Query("code"))
	if errors.Is(e, ErrLoginFailed) {
		h.Log.Info("login failed", zap.String("requestId", c.GetString("requestId")), zap.String("provider", c.Param("provider")))
		h.fail(c, fault{"login_failed", http.StatusUnauthorized})
		return
	}
	if e != nil {
		h.Log.Error("login callback failed", zap.String("requestId", c.GetString("requestId")), zap.String("provider", c.Param("provider")), zap.Error(e))
		h.fail(c, fault{"login_unavailable", http.StatusServiceUnavailable})
		return
	}
	session, e := h.Store.Resolve(c.Request.Context(), completed.SessionToken)
	if e != nil {
		h.Log.Error("resolve new session failed", zap.String("requestId", c.GetString("requestId")), zap.Error(e))
		h.fail(c, fault{"login_unavailable", http.StatusServiceUnavailable})
		return
	}
	http.SetCookie(c.Writer, h.Cookies.SessionCookie(completed.SessionToken, session.ExpiresAt, h.Now()))
	c.Redirect(http.StatusSeeOther, completed.ReturnTo)
}

// logout revokes the current session and clears the cookie. It is idempotent and never reveals
// whether the presented token was live.
func (h *handler) logout(c *gin.Context) {
	if !SameOrigin(c.Request, h.PublicOrigin) {
		h.fail(c, fault{"origin_forbidden", http.StatusForbidden})
		return
	}
	if cookie, e := c.Request.Cookie(h.Cookies.SessionCookieName()); e == nil && cookie.Value != "" {
		if e = h.Store.Revoke(c.Request.Context(), cookie.Value, RevokeLogout); e != nil {
			h.Log.Error("logout failed", zap.String("requestId", c.GetString("requestId")), zap.Error(e))
			h.fail(c, fault{"logout_unavailable", http.StatusServiceUnavailable})
			return
		}
	}
	http.SetCookie(c.Writer, h.Cookies.ClearSessionCookie())
	c.Status(http.StatusNoContent)
}

// relay proxies one authenticated public API request. The browser's credential headers are dropped,
// fresh short-lived internal credentials are attached, and only the fixed upstream is contacted.
func (h *handler) relay(c *gin.Context) {
	if mutating(c.Request.Method) && !SameOrigin(c.Request, h.PublicOrigin) {
		h.fail(c, fault{"origin_forbidden", http.StatusForbidden})
		return
	}
	cookie, e := c.Request.Cookie(h.Cookies.SessionCookieName())
	if e != nil || cookie.Value == "" {
		h.fail(c, fault{"unauthenticated", http.StatusUnauthorized})
		return
	}
	session, e := h.Store.Resolve(c.Request.Context(), cookie.Value)
	if errors.Is(e, ErrNotFound) {
		http.SetCookie(c.Writer, h.Cookies.ClearSessionCookie())
		h.fail(c, fault{"unauthenticated", http.StatusUnauthorized})
		return
	}
	if e != nil {
		h.Log.Error("resolve session failed", zap.String("requestId", c.GetString("requestId")), zap.Error(e))
		h.fail(c, fault{"session_unavailable", http.StatusServiceUnavailable})
		return
	}
	credentials, e := h.Issuer.Issue(session.Identity)
	if e != nil {
		h.Log.Error("issue credentials failed", zap.String("requestId", c.GetString("requestId")), zap.String("sessionId", session.ID), zap.Error(e))
		h.fail(c, fault{"credential_unavailable", http.StatusServiceUnavailable})
		return
	}
	if c.Request.ContentLength > maxBodyBytes {
		h.fail(c, fault{"request_too_large", http.StatusRequestEntityTooLarge})
		return
	}
	// The upstream timeout bounds a whole bounded exchange. It is a stoppable timer rather than a
	// context deadline so an authorized event stream can outlive it once Cloud has answered.
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	deadline := time.AfterFunc(h.UpstreamTimeout, cancel)
	defer deadline.Stop()
	out := c.Request.Clone(ctx)
	out.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	for _, name := range []string{"Authorization", "X-Ora-User-Token", "Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip"} {
		out.Header.Del(name)
	}
	out.Header.Set("Authorization", "Bearer "+credentials.Service)
	out.Header.Set("X-Ora-User-Token", credentials.User)
	h.proxy.ServeHTTP(c.Writer, out, func(err error) {
		h.Log.Warn("upstream failure", zap.String("requestId", c.GetString("requestId")), zap.String("sessionId", session.ID), zap.Error(err))
		h.fail(c, fault{"upstream_unavailable", http.StatusBadGateway})
	}, func() {
		// An event stream outlives the upstream and write timeouts by design; they guard against
		// slow bounded exchanges, not against subscriptions. Lifting them is per-connection and
		// happens only after Cloud has authorized the stream.
		deadline.Stop()
		if e := http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{}); e != nil {
			h.Log.Warn("event stream keeps write deadline", zap.String("requestId", c.GetString("requestId")), zap.String("sessionId", session.ID), zap.Error(e))
		}
	})
}

// decodeStrict reads a bounded JSON object and rejects unknown fields or trailing values.
func decodeStrict(c *gin.Context, v any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(v); e != nil {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}
