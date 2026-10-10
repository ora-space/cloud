package core

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestNormalizeRepositoryScopeIsTheOneSpellingOfOneRepository pins the D2 matching strategy: a scope
// is stored and compared in exactly one normalized form, so two spellings of one repository cannot
// become two scopes and two different repositories cannot collapse into one.
func TestNormalizeRepositoryScopeIsTheOneSpellingOfOneRepository(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"root-level repository", "https://git.example.invalid/ora.git", "https://git.example.invalid/ora", true},
		{"trailing slash and .git are one repository", "https://git.example.invalid/group/repo.git/", "https://git.example.invalid/group/repo", true},
		{"host is case-insensitive", "https://Git.Example.Invalid/group/repo", "https://git.example.invalid/group/repo", true},
		{"path case is preserved", "https://git.example.invalid/Group/Repo", "https://git.example.invalid/Group/Repo", true},
		{"the port is part of the endpoint", "https://git.example.invalid:8443/group/repo", "https://git.example.invalid:8443/group/repo", true},
		{"surrounding whitespace is trimmed", "  https://git.example.invalid/group/repo  ", "https://git.example.invalid/group/repo", true},
		{"an empty scope names nothing", "", "", false},
		{"a bare host would be host-wide", "https://git.example.invalid", "", false},
		{"a bare host with a slash is the same", "https://git.example.invalid/", "", false},
		{"ssh carries a key this credential can never match", "ssh://git@git.example.invalid/group/repo", "", false},
		{"plain http is refused", "http://git.example.invalid/group/repo", "", false},
		{"embedded credentials are refused", "https://user:token@git.example.invalid/group/repo", "", false},
		{"a username alone is still userinfo", "https://user@git.example.invalid/group/repo", "", false},
		{"a query is refused", "https://git.example.invalid/group/repo?ref=main", "", false},
		{"a fragment is refused", "https://git.example.invalid/group/repo#readme", "", false},
		{"percent-encoding is refused rather than decoded", "https://git.example.invalid/group%2Frepo", "", false},
		{"an empty segment is refused", "https://git.example.invalid/group//repo", "", false},
		{"a relative segment is refused", "https://git.example.invalid/group/../repo", "", false},
		{"the current-directory segment is refused", "https://git.example.invalid/group/./repo", "", false},
		{"a scope longer than the bound is refused", "https://git.example.invalid/" + strings.Repeat("a", GitCredentialRepositoryScopeMax), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := normalizeRepositoryScope(c.raw)
			if ok != c.ok || got != c.want {
				t.Fatalf("normalizeRepositoryScope(%q) = %q, %v; want %q, %v", c.raw, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestValidGitCredentialNameCountsCharactersNotBytes pins the boundary to the unit the column's own
// `char_length` check measures, so a name the boundary accepts can never fail the insert.
func TestValidGitCredentialNameCountsCharactersNotBytes(t *testing.T) {
	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"a single character is enough", "a", true},
		{"the bound is inclusive", strings.Repeat("a", GitCredentialNameMax), true},
		{"one past the bound is refused", strings.Repeat("a", GitCredentialNameMax+1), false},
		{"the bound counts characters, not bytes", strings.Repeat("凭", GitCredentialNameMax), true},
		{"a multi-byte name past the bound is refused", strings.Repeat("凭", GitCredentialNameMax+1), false},
		{"an empty name is refused", "", false},
		{"leading whitespace is refused rather than trimmed", " work", false},
		{"a newline is refused", "work\nlaptop", false},
		{"a carriage return is refused", "work\rlaptop", false},
		{"a NUL is refused", "work\x00laptop", false},
		{"invalid UTF-8 is refused", string([]byte{0xff, 0xfe}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validGitCredentialName(c.value); got != c.ok {
				t.Fatalf("validGitCredentialName(%q) = %v; want %v", c.value, got, c.ok)
			}
		})
	}
}

// TestValidGitCredentialTokenRefusesRatherThanRepairs pins D7's reason-code contract at the boundary:
// a token is accepted exactly as sent or refused, because trimming a pasted newline would silently
// change the secret and leave the caller unable to tell which bytes were stored.
func TestValidGitCredentialTokenRefusesRatherThanRepairs(t *testing.T) {
	cases := []struct {
		name  string
		value string
		ok    bool
	}{
		{"an ordinary token is accepted", "ghp_abcdefghijklmnop", true},
		{"the service's own bound is inclusive", strings.Repeat("t", GitCredentialTokenMax), true},
		{"one past the bound is refused here, not one round trip later", strings.Repeat("t", GitCredentialTokenMax+1), false},
		{"an empty token is refused", "", false},
		{"a trailing newline is refused, never trimmed", "token\n", false},
		{"leading whitespace is refused, never trimmed", " token", false},
		{"a NUL is refused", "tok\x00en", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validGitCredentialToken(c.value); got != c.ok {
				t.Fatalf("validGitCredentialToken(%d bytes) = %v; want %v", len(c.value), got, c.ok)
			}
		})
	}
}

// TestGitCredentialIDIsDerivedFromTenantOwnerAndKey pins the property the cross-store create rests
// on: a retry under the same idempotency key reproduces the same id, so the Credential Service
// recognizes the handoff as a replay instead of sealing a second copy of the secret (store D4).
func TestGitCredentialIDIsDerivedFromTenantOwnerAndKey(t *testing.T) {
	tid, uid := uuid.NewString(), uuid.NewString()
	first := gitCredentialID(tid, uid, "key-a")
	if first != gitCredentialID(tid, uid, "key-a") {
		t.Fatal("the same tenant, owner and key must derive the same id; the cross-store create would otherwise never reconcile")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("a derived id must be an ordinary opaque UUID, got %q: %v", first, err)
	}
	distinct := map[string]string{
		"another key":    gitCredentialID(tid, uid, "key-b"),
		"another owner":  gitCredentialID(tid, uuid.NewString(), "key-a"),
		"another tenant": gitCredentialID(uuid.NewString(), uid, "key-a"),
	}
	for what, id := range distinct {
		if id == first {
			t.Fatalf("a different %s must derive a different id, or one retry would answer as another credential's replay", what)
		}
	}
}

// unlistedRefusal is a Credential Service failure whose reason code this package does not know.
type unlistedRefusal struct{ code string }

func (r unlistedRefusal) Error() string      { return "refused" }
func (r unlistedRefusal) ReasonCode() string { return r.code }

// TestCredentialStoreFaultIsTotalAndCarriesNoDetail pins D6/D7: every handoff failure becomes one
// stable public reason code, an unknown failure is never guessed into a success, and no branch
// carries any part of the failure's own text into the response.
func TestCredentialStoreFaultIsTotalAndCarriesNoDetail(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"a store that cannot be reached", errors.New("dial tcp 10.0.0.9:8443: connection refused"), "credential_store_unavailable", 503},
		{"a refusal this build does not know", unlistedRefusal{code: "something_new"}, "credential_store_unavailable", 503},
		{"an already revoked credential", unlistedRefusal{code: "credential_revoked"}, "credential_revoked", 409},
		{"a substituted secret under a replayed id", unlistedRefusal{code: "credential_conflict"}, "credential_store_inconsistent", 409},
		{"a reference with no material", unlistedRefusal{code: "credential_not_found"}, "credential_store_inconsistent", 409},
		{"a request the store refuses", unlistedRefusal{code: "invalid_request"}, "credential_store_rejected_request", 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fault := credentialStoreFault(c.err)
			if fault.Code != c.code || fault.Status != c.status {
				t.Fatalf("credentialStoreFault(%v) = %s/%d; want %s/%d", c.err, fault.Code, fault.Status, c.code, c.status)
			}
			for _, param := range fault.Params {
				if text, ok := param.(string); ok && strings.Contains(text, "dial tcp") {
					t.Fatalf("the fault leaked the transport's own text: %v", param)
				}
			}
		})
	}
}
