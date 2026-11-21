package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// LoginProviderEmail signs in by address alone: only an address the startup preset lists or an
// owner already invited may sign in. It has no password, so it is for development and demos.
const LoginProviderEmail = "email"

// LoginProviderHuaweiIDaaS signs in with Huawei IDaaS (see the idaas package).
const LoginProviderHuaweiIDaaS = "huawei-idaas"

// LoginProviders are the login methods this build implements; V2_LOGIN picks among them.
var LoginProviders = []string{LoginProviderEmail, LoginProviderHuaweiIDaaS}

// SessionCookie carries the session over plain http, the Gateway's development cookie name.
const SessionCookie = "ora_session"

// SecureSessionCookie carries the session when the public address is https. The __Host- prefix
// makes browsers refuse it unless it is Secure, host-only and on path /, so a sibling subdomain
// sharing a parent domain cannot plant a session of its own choosing.
const SecureSessionCookie = "__Host-ora_session"

// loginProviders are the offered methods, defaulting to email like V2_LOGIN.
func (h *handler) loginProviders() []string {
	if len(h.opts.LoginProviders) == 0 {
		return []string{LoginProviderEmail}
	}
	return h.opts.LoginProviders
}

// secure reports whether browsers reach this server over https.
func (h *handler) secure() bool { return h.opts.PublicURL != nil && h.opts.PublicURL.Scheme == "https" }

func (h *handler) sessionCookie() string {
	if h.secure() {
		return SecureSessionCookie
	}
	return SessionCookie
}

func (h *handler) setSession(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: h.sessionCookie(), Value: value, Path: "/", HttpOnly: true, Secure: h.secure(),
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	})
}

// sameOrigin is the cross-site request check for cookie-authenticated writes. Browsers send Origin
// on every non-GET request, so a write whose Origin names another site is refused. A request
// without Origin did not come from a page and carries no ambient cookie risk. Without a public URL
// only the host is compared, since behind a TLS proxy the server cannot see the page's scheme.
func (h *handler) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if p := h.opts.PublicURL; p != nil {
		return origin == p.Scheme+"://"+p.Host
	}
	u, e := url.Parse(origin)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host && u.Path == ""
}

// safeMethod reports a method that must not change state, so it needs no Origin check.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// providers lists the offered login methods in display order.
func (h *handler) providers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": h.loginProviders()})
}

// login is the Gateway's POST /auth/login. The email login signs in at once and sends the browser
// straight back to returnTo; an unknown address is rejected, never created. A redirect method
// answers with its sign-in address and finishes in callback. A method V2_LOGIN does
// not offer is unknown_provider, so turning email off leaves no way to it.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		fault(w, http.StatusForbidden, "origin_mismatch")
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Email    string `json:"email"`
		ReturnTo string `json:"returnTo"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if in.Provider == "" {
		in.Provider = LoginProviderEmail
	}
	if p, ok := h.redirectProvider(in.Provider); ok {
		h.startRedirectLogin(w, in.Provider, p, in.ReturnTo)
		return
	}
	if in.Provider != LoginProviderEmail || !slices.Contains(h.loginProviders(), in.Provider) {
		fault(w, http.StatusBadRequest, "unknown_provider")
		return
	}
	email, e := store.NormalizeEmail(in.Email)
	if e != nil {
		fault(w, http.StatusBadRequest, "invalid_email")
		return
	}
	u, e := h.store.UserByEmail(r.Context(), email)
	if errors.Is(e, store.ErrNotFound) {
		fault(w, http.StatusForbidden, "email_not_invited")
		return
	}
	if e != nil {
		internal(w, "find login account", e)
		return
	}
	token, e := h.store.StartSession(r.Context(), u.ID, h.opts.SessionTTL)
	if e != nil {
		internal(w, "login", e)
		return
	}
	h.setSession(w, token, int(h.opts.SessionTTL/time.Second))
	writeJSON(w, http.StatusOK, map[string]string{"authorizationUrl": localPath(in.ReturnTo)})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		fault(w, http.StatusForbidden, "origin_mismatch")
		return
	}
	if c, e := r.Cookie(h.sessionCookie()); e == nil {
		if e := h.store.Logout(r.Context(), c.Value); e != nil {
			internal(w, "logout", e)
			return
		}
	}
	h.setSession(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// authed resolves the caller to a user, answering 401 like Cloud when there is none. A request
// with an Authorization header is judged by that header alone (A6 D6): a desktop token or 401,
// never a fallback to the cookie, so the cookie of whoever is signed in to the browser cannot stand
// in for a missing or revoked token. Without the header, the session cookie decides, and a write
// from another origin is refused before the session is read, whatever the route; a bearer request
// needs no such check, because a browser attaches no bearer token on its own.
func (h *handler) authed(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			if u, ok := h.bearerUser(w, r); ok {
				next(w, r, u)
			}
			return
		}
		if !safeMethod(r.Method) && !h.sameOrigin(r) {
			fault(w, http.StatusForbidden, "origin_mismatch")
			return
		}
		c, e := r.Cookie(h.sessionCookie())
		if e != nil {
			fault(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		u, e := h.store.SessionUser(r.Context(), c.Value)
		if errors.Is(e, store.ErrNotFound) {
			fault(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if e != nil {
			internal(w, "session", e)
			return
		}
		next(w, r, u)
	}
}
