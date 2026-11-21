package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// Ora desktop signs in through the system browser and a loopback redirect (A6 D6, RFC 8252): the
// desktop opens /desktop/authorize with its loopback redirect URI, a state and a PKCE S256
// challenge; the signed-in user approves on that page, which posts here and is sent back to the
// loopback address with a one-time code; the desktop exchanges the code, its verifier and the same
// redirect URI for a desktop token, and sends it as a bearer token from then on.

const (
	// desktopCodeTTL is how long an approval's code can be exchanged (A6 D6).
	desktopCodeTTL = 2 * time.Minute
	// maxDesktopCodes bounds the table; approving needs a session, so filling it takes an account.
	maxDesktopCodes = 10000
	// maxDesktopState bounds the desktop's opaque state, which only travels back to it.
	maxDesktopState = 512
)

// desktopCode is one approval waiting for its exchange. It lives in memory only, like a login
// attempt: one process, and a restart only makes the person approve again.
type desktopCode struct {
	userID, challenge, redirectURI string
	expires                        time.Time
}

type desktopCodes struct {
	mu    sync.Mutex
	items map[string]desktopCode
}

func newDesktopCodes() *desktopCodes { return &desktopCodes{items: map[string]desktopCode{}} }

// add stores a code under a new random value, dropping expired codes and, when full, the oldest.
func (c *desktopCodes) add(dc desktopCode, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var oldest string
	for k, v := range c.items {
		if !now.Before(v.expires) {
			delete(c.items, k)
		} else if oldest == "" || v.expires.Before(c.items[oldest].expires) {
			oldest = k
		}
	}
	if len(c.items) >= maxDesktopCodes {
		delete(c.items, oldest)
	}
	code := randomToken()
	c.items[code] = dc
	return code
}

// take removes and returns the code, so any exchange, right or wrong, spends it.
func (c *desktopCodes) take(code string, now time.Time) (desktopCode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dc, ok := c.items[code]
	delete(c.items, code)
	return dc, ok && now.Before(dc.expires)
}

// loopbackRedirect accepts only http://127.0.0.1:<port>/<path> and http://[::1]:<port>/<path>
// (RFC 8252 §7.3). localhost is refused because it resolves through DNS and the hosts file; a
// query is refused so the code and state this server appends are the only parameters. Any '#' is
// refused before parsing: an empty fragment parses to "" yet would still push the appended code
// and state into a fragment the listener never receives.
func loopbackRedirect(raw string) bool {
	if strings.Contains(raw, "#") {
		return false
	}
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || !strings.HasPrefix(u.Path, "/") || u.Opaque != "" {
		return false
	}
	host, port, e := net.SplitHostPort(u.Host)
	if e != nil || (host != "127.0.0.1" && host != "::1") {
		return false
	}
	p, e := strconv.Atoi(port)
	return e == nil && p > 0 && p < 65536 && strconv.Itoa(p) == port
}

// pkceValue reports a code verifier or S256 challenge in RFC 7636's alphabet and length.
func pkceValue(v string, minLen, maxLen int) bool {
	if len(v) < minLen || len(v) > maxLen {
		return false
	}
	for _, r := range v {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' || r == '~') {
			return false
		}
	}
	return true
}

// s256 is the PKCE S256 challenge of a verifier.
func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// approveDesktop is the 「允许」 on /desktop/authorize. Only a web session from this origin can
// approve: a bearer token is refused, so a desktop cannot mint more desktop tokens for itself, and a
// page on another origin cannot approve with the visitor's cookie. The answer is the loopback
// address to send the browser to, carrying the code and the desktop's state.
func (h *handler) approveDesktop(w http.ResponseWriter, r *http.Request, u store.User) {
	if r.Header.Get("Authorization") != "" {
		fault(w, http.StatusForbidden, "web_session_required")
		return
	}
	var in struct {
		RedirectURI         string `json:"redirectUri"`
		State               string `json:"state"`
		CodeChallenge       string `json:"codeChallenge"`
		CodeChallengeMethod string `json:"codeChallengeMethod"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if !loopbackRedirect(in.RedirectURI) {
		fault(w, http.StatusBadRequest, "invalid_redirect_uri")
		return
	}
	// S256 of 32 bytes is exactly 43 base64url characters.
	if in.CodeChallengeMethod != "S256" || !pkceValue(in.CodeChallenge, 43, 43) {
		fault(w, http.StatusBadRequest, "invalid_code_challenge")
		return
	}
	if in.State == "" || len(in.State) > maxDesktopState || strings.ContainsFunc(in.State, func(r rune) bool { return r < 0x21 || r == 0x7f }) {
		fault(w, http.StatusBadRequest, "invalid_state")
		return
	}
	code := h.desktopCodes.add(desktopCode{userID: u.ID, challenge: in.CodeChallenge, redirectURI: in.RedirectURI, expires: time.Now().Add(desktopCodeTTL)}, time.Now())
	q := url.Values{"code": {code}, "state": {in.State}}
	writeJSON(w, http.StatusOK, map[string]string{"redirectUrl": in.RedirectURI + "?" + q.Encode()})
}

// desktopToken exchanges a code for a desktop token. The code is spent by the first attempt; it
// yields a token only with the verifier of the approved challenge and the approved redirect URI.
// Every refusal is the same invalid_grant, so a caller learns nothing about which part was wrong.
func (h *handler) desktopToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"codeVerifier"`
		RedirectURI  string `json:"redirectUri"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	dc, ok := h.desktopCodes.take(in.Code, time.Now())
	if !ok || !pkceValue(in.CodeVerifier, 43, 128) ||
		subtle.ConstantTimeCompare([]byte(s256(in.CodeVerifier)), []byte(dc.challenge)) != 1 ||
		in.RedirectURI != dc.redirectURI {
		fault(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	token, u, e := h.store.StartDesktopToken(r.Context(), dc.userID)
	if errors.Is(e, store.ErrNotFound) {
		// Disabled or deleted between approval and exchange.
		fault(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if e != nil {
		internal(w, "desktop token", e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": map[string]string{"id": u.ID, "displayName": u.DisplayName}})
}

// desktopLogout revokes the bearer token it is called with, and nothing else (A6 D6). It answers
// 204 for a token that is already gone, so a desktop retrying a logout is not told otherwise.
func (h *handler) desktopLogout(w http.ResponseWriter, r *http.Request) {
	token, ok := bearerToken(r)
	if !ok {
		fault(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if e := h.store.RevokeDesktopToken(r.Context(), token); e != nil {
		internal(w, "desktop logout", e)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bearerToken is the token of a well-formed "Authorization: Bearer" header.
func bearerToken(r *http.Request) (token string, ok bool) {
	v := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(v, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// bearerUser resolves a request carrying an Authorization header. A desktop token is the only
// bearer credential; anything else, including a web session's value, is unauthenticated.
func (h *handler) bearerUser(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	token, ok := bearerToken(r)
	if !ok {
		fault(w, http.StatusUnauthorized, "unauthenticated")
		return store.User{}, false
	}
	u, e := h.store.DesktopTokenUser(r.Context(), token)
	if errors.Is(e, store.ErrNotFound) {
		fault(w, http.StatusUnauthorized, "unauthenticated")
		return store.User{}, false
	}
	if e != nil {
		slog.Error("desktop token", "error", e)
		fault(w, http.StatusInternalServerError, "internal")
		return store.User{}, false
	}
	return u, true
}
