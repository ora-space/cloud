package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// ErrUnknownProvider reports a start request naming an adapter that is not registered.
var ErrUnknownProvider = errors.New("unknown authentication provider")

// ErrLoginFailed is the single failure the callback presents to browsers. Wrong state, wrong or
// missing attempt cookie, expired or consumed attempts, provider rejection, and a lost session
// transaction all collapse into it so callers cannot probe which attempts exist.
var ErrLoginFailed = errors.New("login failed")

// Login orchestrates external authentication: it owns attempts, state, PKCE, return_to, failure
// mapping, and session creation. Adapters only translate provider protocols.
type Login struct {
	store        *Store
	providers    map[string]Authenticator
	pkceKey      []byte
	callbackBase string
	attemptTTL   time.Duration
	sessionTTL   time.Duration
}

// Started is the result of a successful start: where to send the browser and the attempt secret it
// must hold in the attempt cookie.
type Started struct {
	AuthorizationURL string
	AttemptSecret    string
}

// Completed is the result of a successful callback: the raw session token (returned exactly once)
// and the trusted redirect target read back from the attempt row.
type Completed struct {
	SessionToken string
	ReturnTo     string
}

// NewLogin validates the orchestration configuration. callbackBase is the public callback URL
// prefix; each provider's callback is callbackBase + "/" + provider. The PKCE key must hold at
// least 256 bits so derived verifiers keep the entropy of the attempt secret.
func NewLogin(store *Store, providers map[string]Authenticator, pkceKey []byte, callbackBase string, attemptTTL, sessionTTL time.Duration) (*Login, error) {
	if store == nil || len(providers) == 0 || callbackBase == "" {
		return nil, fmt.Errorf("store, at least one provider and callback base URL are required")
	}
	if len(pkceKey) < 32 {
		return nil, fmt.Errorf("PKCE derivation key must hold at least 32 bytes")
	}
	if attemptTTL < time.Minute || attemptTTL > time.Hour {
		return nil, fmt.Errorf("login attempt TTL must be between 1 minute and 1 hour")
	}
	if sessionTTL <= 0 || sessionTTL > MaxSessionLifetime {
		return nil, fmt.Errorf("session TTL must be positive and at most %s", MaxSessionLifetime)
	}
	for name := range providers {
		if name == "" || len(name) > 64 {
			return nil, fmt.Errorf("provider names must be 1-64 characters")
		}
	}
	return &Login{store: store, providers: providers, pkceKey: pkceKey, callbackBase: callbackBase, attemptTTL: attemptTTL, sessionTTL: sessionTTL}, nil
}

// CallbackURL is the fixed, configuration-derived redirect URI for one provider.
func (l *Login) CallbackURL(provider string) string { return l.callbackBase + "/" + provider }

// Start creates a login attempt and the provider redirect. Nothing external is called.
func (l *Login) Start(ctx context.Context, provider, returnTo string) (Started, error) {
	if provider == "" && len(l.providers) == 1 {
		for configured := range l.providers {
			provider = configured
		}
	}
	adapter, ok := l.providers[provider]
	if !ok {
		return Started{}, ErrUnknownProvider
	}
	path, ok := NormalizeReturnTo(returnTo)
	if !ok {
		return Started{}, fmt.Errorf("%w: invalid return_to", ErrLoginFailed)
	}
	secret, e := NewSecret()
	if e != nil {
		return Started{}, e
	}
	state, e := NewSecret()
	if e != nil {
		return Started{}, e
	}
	if _, e = l.store.CreateAttempt(ctx, secret, state, provider, path, l.attemptTTL); e != nil {
		return Started{}, e
	}
	target, e := adapter.AuthorizationURL(AuthorizationRequest{State: state, CodeChallenge: codeChallenge(l.codeVerifier(secret)), CallbackURL: l.CallbackURL(provider)})
	if e != nil {
		return Started{}, fmt.Errorf("build authorization URL: %w", e)
	}
	return Started{AuthorizationURL: target, AttemptSecret: secret}, nil
}

// Callback completes a login. The attempt is checked before the provider exchange, the exchange
// runs with no database lock held, and the attempt is consumed together with session creation in
// one transaction. A provider code that was redeemed but whose transaction failed yields
// ErrLoginFailed; no session is inferred.
func (l *Login) Callback(ctx context.Context, provider, attemptSecret, state, code string) (Completed, error) {
	adapter, ok := l.providers[provider]
	if !ok || attemptSecret == "" || state == "" || code == "" || len(attemptSecret) > 128 || len(state) > 128 || len(code) > 512 {
		return Completed{}, ErrLoginFailed
	}
	attempt, e := l.store.LookupAttempt(ctx, attemptSecret, state, provider)
	if errors.Is(e, ErrNotFound) {
		return Completed{}, ErrLoginFailed
	}
	if e != nil {
		return Completed{}, e
	}
	identity, e := adapter.Exchange(ctx, code, l.codeVerifier(attemptSecret), l.CallbackURL(provider))
	if errors.Is(e, ErrProviderRejected) {
		return Completed{}, fmt.Errorf("%w: %w", ErrLoginFailed, e)
	}
	if e != nil {
		return Completed{}, fmt.Errorf("provider exchange: %w", e)
	}
	identity, e = Normalize(identity)
	if e != nil {
		return Completed{}, fmt.Errorf("%w: %w", ErrLoginFailed, e)
	}
	token, e := l.store.ConsumeAttempt(ctx, attempt.ID, identity, l.sessionTTL)
	if errors.Is(e, ErrNotFound) {
		return Completed{}, ErrLoginFailed
	}
	if e != nil {
		return Completed{}, e
	}
	return Completed{SessionToken: token, ReturnTo: attempt.ReturnTo}, nil
}

// codeVerifier derives the PKCE verifier from the browser-held attempt secret with a domain-separated
// HMAC. The database never stores the verifier; only a caller holding both the secret and the
// Gateway key can reproduce it. The output is 43 URL-safe characters as RFC 7636 requires.
func (l *Login) codeVerifier(attemptSecret string) string {
	mac := hmac.New(sha256.New, l.pkceKey)
	mac.Write([]byte("ora-gateway-pkce-verifier\x00"))
	mac.Write([]byte(attemptSecret))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// codeChallenge is the S256 transform of a verifier.
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
