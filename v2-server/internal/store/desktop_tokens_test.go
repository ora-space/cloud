package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestDesktopTokenIsItsOwnKindOfCredential(t *testing.T) {
	f := newSquadFixture(t)
	alice := f.outsider(t, "alice")

	token, u, e := f.store.StartDesktopToken(f.ctx, alice)
	if e != nil || u.ID != alice || !strings.HasPrefix(token, store.DesktopTokenPrefix) {
		t.Fatalf("start = %q %+v %v", token, u, e)
	}
	if got, e := f.store.DesktopTokenUser(f.ctx, token); e != nil || got.ID != alice {
		t.Fatalf("desktop token user = %+v %v", got, e)
	}
	// A desktop token is no web session and a web session no desktop token.
	if _, e := f.store.SessionUser(f.ctx, token); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("desktop token as web session = %v", e)
	}
	web, e := f.store.StartSession(f.ctx, alice, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DesktopTokenUser(f.ctx, web); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("web session as desktop token = %v", e)
	}
	// Web logout and desktop logout each end only their own kind.
	if e := f.store.Logout(f.ctx, token); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DesktopTokenUser(f.ctx, token); e != nil {
		t.Fatalf("web logout ended the desktop token: %v", e)
	}
	if e := f.store.RevokeDesktopToken(f.ctx, web); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SessionUser(f.ctx, web); e != nil {
		t.Fatalf("desktop logout ended the web session: %v", e)
	}
	if e := f.store.RevokeDesktopToken(f.ctx, token); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DesktopTokenUser(f.ctx, token); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("revoked token = %v", e)
	}
}

func TestDesktopTokenExpiresAfterThirtyIdleDays(t *testing.T) {
	f := newSquadFixture(t)
	alice := f.outsider(t, "alice")
	token, _, e := f.store.StartDesktopToken(f.ctx, alice)
	if e != nil {
		t.Fatal(e)
	}
	expiry := func() (lastUsed, expires time.Time) {
		t.Helper()
		if e := f.db.QueryRow(f.ctx, `SELECT last_used_at, expires_at FROM sessions WHERE kind = 'desktop' AND user_id = $1`, alice).Scan(&lastUsed, &expires); e != nil {
			t.Fatal(e)
		}
		return
	}
	if lastUsed, expires := expiry(); expires.Sub(lastUsed) != store.DesktopTokenIdle {
		t.Fatalf("issued with expiry %v after last use", expires.Sub(lastUsed))
	}

	// Used within the hour: nothing is written.
	if _, e := f.store.DesktopTokenUser(f.ctx, token); e != nil {
		t.Fatal(e)
	}
	// Last used 29 days ago: still valid, and this use moves both times forward.
	if _, e := f.db.Exec(f.ctx, `UPDATE sessions SET last_used_at = now() - interval '29 days', expires_at = now() + interval '1 day' WHERE user_id = $1`, alice); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DesktopTokenUser(f.ctx, token); e != nil {
		t.Fatalf("token used within 30 days = %v", e)
	}
	if lastUsed, expires := expiry(); time.Since(lastUsed) > time.Minute || time.Until(expires) < store.DesktopTokenIdle-time.Minute {
		t.Fatalf("use did not refresh: last %v expires %v", lastUsed, expires)
	}
	// Past its expiry: unknown, and a use does not revive it.
	if _, e := f.db.Exec(f.ctx, `UPDATE sessions SET last_used_at = now() - interval '31 days', expires_at = now() - interval '1 day' WHERE user_id = $1`, alice); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DesktopTokenUser(f.ctx, token); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("expired token = %v", e)
	}
}

func TestDesktopTokenFollowsTheAccount(t *testing.T) {
	f := newSquadFixture(t)
	alice := f.outsider(t, "alice")
	token, _, e := f.store.StartDesktopToken(f.ctx, alice)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, alice); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.DesktopTokenUser(f.ctx, token); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("token of a disabled account = %v", e)
	}
	// An approval given before the account was disabled yields no new token.
	if _, _, e := f.store.StartDesktopToken(f.ctx, alice); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("token issued to a disabled account: %v", e)
	}
}
