package integration

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/wanglongan587/cloud/internal/gateway"
)

// An attempt's database expiry is authoritative even while the browser still holds its cookie
// and the external provider's code remains redeemable. Rejection must precede code exchange.
func TestGatewayExpiredLoginAttemptRejectsCallbackBeforeProviderExchange(t *testing.T) {
	f := setupGateway(t)
	gw := f.newGateway("", 100)
	browser := gw.browser()
	location := gw.startLogin(browser, "/projects")
	callbackURL, err := url.Parse(location)
	must(t, err)
	secret := ""
	for _, cookie := range browser.Jar.Cookies(callbackURL) {
		if cookie.Name == "ora_login" {
			secret = cookie.Value
		}
	}
	if secret == "" {
		t.Fatal("the browser must still hold the valid attempt cookie")
	}
	result, err := f.pool.Exec(`UPDATE gateway_login_attempts
SET created_at=clock_timestamp()-interval '10 minutes', expires_at=clock_timestamp()-interval '1 minute'
WHERE secret_hash=$1`, gateway.Digest(secret))
	must(t, err)
	updated, err := result.RowsAffected()
	must(t, err)
	if updated != 1 {
		t.Fatalf("expiry setup must change exactly the pending attempt, got %d", updated)
	}
	resp := gw.callback(browser, location)
	if resp.StatusCode != http.StatusUnauthorized || resp.body.S("code") != "login_failed" {
		t.Fatalf("an expired attempt must reject an otherwise valid callback: %d %v", resp.StatusCode, resp.body)
	}
	cleared := false
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "ora_session" {
			t.Fatal("an expired callback must not issue a session cookie")
		}
		if cookie.Name == "ora_login" && cookie.MaxAge == -1 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("an expired callback must clear the attempt cookie")
	}
	if count := f.count("SELECT count(*) FROM gateway_sessions"); count != 0 {
		t.Fatalf("an expired callback created %d sessions", count)
	}
	if count := f.count("SELECT count(*) FROM gateway_login_attempts WHERE secret_hash=$1 AND consumed_at IS NULL", gateway.Digest(secret)); count != 1 {
		t.Fatal("an expired callback must leave its unconsumed attempt for bounded cleanup")
	}
	f.provider.mu.Lock()
	_, providerCodeAvailable := f.provider.codes[callbackURL.Query().Get("code")]
	f.provider.mu.Unlock()
	if !providerCodeAvailable {
		t.Fatal("an expired attempt must be rejected before exchanging the provider code")
	}
}
