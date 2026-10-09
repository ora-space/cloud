package core

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// A user's git commit identity (identity-access 20260928-user-git-commit-identity D1/D2).
//
// The identity is a self-described commit signature: it is never an authentication identity, it
// grants nothing, and only its owner reads or writes it through `/api/v1/me/git-identity`. A stated
// identity lives in `user_git_identities`; without one the default identity applies — the account's
// display name and a per-user noreply address. The email is personal data, so nothing on this path
// logs it.

// gitEmailPattern is D1's `local@domain` shape with no whitespace or angle bracket, the same rule
// the migration enforces.
var gitEmailPattern = regexp.MustCompile(`^[^\s<>@]+@[^\s<>@]+$`)

// validGitName reports whether a name fits D1: 1..200 characters, no line break, no angle bracket —
// the characters that would break or forge the `Name <email>` line Git writes.
func validGitName(name string) bool {
	n := utf8.RuneCountInString(name)
	return utf8.ValidString(name) && n >= 1 && n <= 200 && !strings.ContainsAny(name, "\r\n<>")
}

// validGitEmail reports whether an email fits D1. Only the shape is checked: Cloud never verifies
// ownership because the address authorizes nothing.
func validGitEmail(email string) bool {
	return len(email) <= 254 && gitEmailPattern.MatchString(email)
}

// gitIdentityView renders the identity a user would commit as now. `isDefault` says no identity is
// stated; `version` is 0 then, which is the version a first PUT must carry.
func gitIdentityView(t *transaction, uid string) Object {
	if row := t.one("SELECT name, email, version FROM user_git_identities WHERE user_id=$1", uid); row != nil {
		return Object{"name": row.S("name"), "email": row.S("email"), "isDefault": false, "version": row.N("version")}
	}
	identity, err := defaultGitIdentity(t, uid)
	if err != nil {
		panic(err)
	}
	return Object{"name": identity.S("name"), "email": identity.S("email"), "isDefault": true, "version": int64(0)}
}

// putGitIdentity states the caller's identity. `version` is optimistic concurrency over the stated
// row: 0 creates it, the row's current version replaces it, anything else is a conflict, so two
// tabs cannot silently overwrite each other.
func putGitIdentity(t *transaction, r *PublicRequest, uid string) Object {
	name, email := strings.TrimSpace(r.Body.S("name")), strings.TrimSpace(r.Body.S("email"))
	require(validGitName(name), 400, "invalid_git_name")
	require(validGitEmail(email), 400, "invalid_git_email")
	expected := r.Body.N("version")
	row := t.one("SELECT version FROM user_git_identities WHERE user_id=$1 FOR UPDATE", uid)
	if row == nil {
		require(expected == 0, 409, "version_conflict")
		t.exec("INSERT INTO user_git_identities(user_id,name,email) VALUES($1,$2,$3)", uid, name, email)
	} else {
		require(expected == row.N("version"), 409, "version_conflict")
		t.exec("UPDATE user_git_identities SET name=$2,email=$3,version=version+1,updated_at=now() WHERE user_id=$1", uid, name, email)
	}
	return gitIdentityView(t, uid)
}

// deleteGitIdentity restores the default identity. Restoring an identity that is already the
// default succeeds as a no-op whatever version is sent, which is what makes a retried DELETE safe
// without an idempotency record (`/me` has no tenant to scope one to); a stated identity is removed
// only at its current version, so a stale tab cannot discard an identity set elsewhere.
func deleteGitIdentity(t *transaction, r *PublicRequest, uid string) Object {
	if row := t.one("SELECT version FROM user_git_identities WHERE user_id=$1 FOR UPDATE", uid); row != nil {
		require(r.Body.N("version") == row.N("version"), 409, "version_conflict")
		t.exec("DELETE FROM user_git_identities WHERE user_id=$1", uid)
	}
	return gitIdentityView(t, uid)
}
