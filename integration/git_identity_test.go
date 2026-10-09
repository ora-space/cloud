package integration

import (
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestGitIdentityAPI drives /api/v1/me/git-identity through the real router (identity-access git
// identity D1): the default identity until one is stated, optimistic concurrency on PUT and DELETE,
// D1's shape validation, a DELETE that is safe to retry, isolation between users, and no git email
// on `/me`.
func TestGitIdentityAPI(t *testing.T) {
	f := setup(t)
	const path = "/api/v1/me/git-identity"
	me := f.call("GET", "/api/v1/me", nil, "", 200)
	uid := me.S("id")
	def := core.Object{"name": me.S("displayName"), "email": uid + "@users.noreply.ora.invalid", "isDefault": true, "version": float64(0)}
	sameAs := func(got, want core.Object) {
		t.Helper()
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%s = %v, want %v (got %v)", k, got[k], v, got)
			}
		}
	}
	sameAs(f.call("GET", path, nil, "", 200), def)

	stated := f.call("PUT", path, core.Object{"name": "  Ada Lovelace ", "email": "ada@example.invalid", "version": 0}, "", 200)
	sameAs(stated, core.Object{"name": "Ada Lovelace", "email": "ada@example.invalid", "isDefault": false, "version": float64(1)})
	f.call("PUT", path, core.Object{"name": "Stale", "email": "stale@example.invalid", "version": 0}, "", 409)
	sameAs(f.call("PUT", path, core.Object{"name": "Ada", "email": "ada@example.invalid", "version": 1}, "", 200), core.Object{"name": "Ada", "version": float64(2)})

	for _, bad := range []struct {
		body core.Object
		code string
	}{
		{core.Object{"name": "", "email": "ada@example.invalid", "version": 2}, "invalid_git_name"},
		{core.Object{"name": "Ada\nEvil", "email": "ada@example.invalid", "version": 2}, "invalid_git_name"},
		{core.Object{"name": "Ada <x>", "email": "ada@example.invalid", "version": 2}, "invalid_git_name"},
		{core.Object{"name": "Ada", "email": "not-an-email", "version": 2}, "invalid_git_email"},
		{core.Object{"name": "Ada", "email": "a b@example.invalid", "version": 2}, "invalid_git_email"},
		{core.Object{"name": "Ada", "email": strings.Repeat("a", 250) + "@x.io", "version": 2}, "invalid_git_email"},
	} {
		if got := f.call("PUT", path, bad.body, "", 400); got.S("code") != bad.code {
			t.Fatalf("PUT %v: code %q, want %q", bad.body, got.S("code"), bad.code)
		}
	}
	f.call("PUT", path, core.Object{"name": "Ada", "email": "ada@example.invalid", "version": 2, "userId": uid}, "", 400)

	if mine := f.call("GET", "/api/v1/me", nil, "", 200); strings.Contains(mustJSON(t, mine), "ada@example.invalid") {
		t.Fatalf("/me must not carry the git email: %v", mine)
	}

	f.user.Subject = "git-identity-other"
	other := f.call("GET", path, nil, "", 200)
	if other.B("isDefault") != true || strings.Contains(other.S("email"), "ada@") {
		t.Fatalf("another user must see only their own default identity, got %v", other)
	}
	f.user.Subject = "alice"

	f.call("DELETE", path, core.Object{"version": 1}, "", 409)
	sameAs(f.call("DELETE", path, core.Object{"version": 2}, "", 200), def)
	// Already the default: a retried DELETE succeeds whatever version it carries.
	sameAs(f.call("DELETE", path, core.Object{"version": 2}, "", 200), def)
	sameAs(f.call("PUT", path, core.Object{"name": "Again", "email": "again@example.invalid", "version": 0}, "", 200), core.Object{"version": float64(1)})
}
