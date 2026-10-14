package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// The credential ingress is the one place a raw secret crosses Cloud, so its boundary is tested as a
// boundary rather than as a feature: who may name which credential, which bytes may appear where, and
// what a refusal is allowed to say. `cloud/AGENTS.md` states the five conditions the exception holds
// under, and every test below is one of them made executable.

// TestGitCredentialIngressIsolatesOwnersAndTenants pins condition 1: a caller may only reach a
// credential it owns, in a tenant it is an active member of. Another user's credential in the same
// tenant is reported as absent rather than forbidden, because a distinguishable answer would let a
// member enumerate the deployment's credentials.
func TestGitCredentialIngressIsolatesOwnersAndTenants(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets
	bob, _ := f.addUser(t, "git-credential-bob", "Bob")
	aliceCredential := f.createCredential(f.user, "alice", credentialScope)
	bobCredential := f.createCredential(bob, "bob", credentialScope)

	// Another user of the same tenant, on both mutation routes.
	f.failAs(bob, "PUT", f.path("/git-credentials/"+aliceCredential.S("id")), core.Object{"token": credentialToken + "-stolen", "version": 1}, "", 404, "not_found")
	f.failAs(bob, "DELETE", f.path("/git-credentials/"+aliceCredential.S("id")), core.Object{"version": 1}, uuid.NewString(), 404, "not_found")
	// Owning nothing under that id is what makes the answer 404: bob's own credential is reachable, so
	// the refusal is about attribution rather than about the route.
	if f.callAs(bob, "PUT", f.path("/git-credentials/"+bobCredential.S("id")), core.Object{"token": credentialToken + "-bob", "version": 1}, "", 200).N("version") != 2 {
		t.Fatal("bob could not rotate his own credential")
	}
	var aliceRef, aliceVersion string
	must(t, f.store.Pool.QueryRow("SELECT secret_ref,version::text FROM credential_refs WHERE id=$1", aliceCredential.S("id")).Scan(&aliceRef, &aliceVersion))
	if aliceRef != secrets.ref(aliceCredential.S("id")) || aliceVersion != "1" || secrets.stored(aliceCredential.S("id")) != credentialToken {
		t.Fatal("another user's refused mutation disturbed the credential it named")
	}

	// Another tenant. Alice is not a member there, so the tenant itself is out of reach — and the
	// credential of that tenant is not listed, rotated or revoked under this one.
	other, err := f.store.Bootstrap(context.Background(), "Other tenant", "corp", "carol", "Carol")
	must(t, err)
	carol := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "carol"}, Source: "corp", DisplayName: "Carol"}
	otherPath := "/api/v1/tenants/" + other.S("tenantId") + "/git-credentials"
	foreign := f.callAs(carol, "POST", otherPath, core.Object{"name": "carol", "repositoryScope": credentialScope, "token": credentialToken}, uuid.NewString(), 201)
	f.failAs(f.user, "GET", otherPath, nil, "", 403, "membership_required")
	f.failAs(f.user, "PUT", otherPath+"/"+foreign.S("id"), core.Object{"token": credentialToken + "-stolen", "version": 1}, "", 403, "membership_required")
	f.failAs(f.user, "DELETE", otherPath+"/"+foreign.S("id"), core.Object{"version": 1}, uuid.NewString(), 403, "membership_required")
	if f.scalar("SELECT count(*) FROM credential_refs WHERE tenant_id=$1 AND version<>1", other.S("tenantId")) != 0 {
		t.Fatal("a member of another tenant reached a credential outside their tenant")
	}
	// Carol's own tenant still sees exactly her credential, so the refusals above are about tenancy
	// rather than about the credential being unreachable to everyone.
	if held := items(f.callAs(carol, "GET", otherPath, nil, "", 200)); len(held) != 1 || held[0].S("id") != foreign.S("id") {
		t.Fatalf("the owning tenant must still see its credential: %v", held)
	}
}

// TestGitCredentialIngressReachesOnlyPersonalCredentials pins the attribution half of condition 1
// together with the "no scope expansion" clause of condition 5. The exception covers one kind of
// secret — an HTTPS Git token the caller stated as their own — so a reference the deployment manages
// and a team-owned one must be as unreachable through these routes as a stranger's credential is, even
// when the caller is an active member of the tenant and really does own the row. Ownership is
// necessary and not sufficient here, and the routes that carry a token are exactly the routes where
// that has to hold, which is why this is asserted on the ingress rather than only on the
// administrative paths that write those rows.
func TestGitCredentialIngressReachesOnlyPersonalCredentials(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets

	// Two references the caller owns that the ingress must not reach. The first is written through the
	// administrative path and keeps the `unknown` attribution that path produces: the shape a
	// deployment-managed or service-account reference has, which is what the exception's "no scope
	// expansion" clause names. The second is the same row with its attribution moved to `team`, the
	// shape a shared credential has.
	managed, err := f.store.ConfigureCredential(context.Background(), f.tid, f.uid, "deployment-reference/managed")
	must(t, err)
	shared, err := f.store.ConfigureCredential(context.Background(), f.tid, f.uid, "deployment-reference/shared")
	must(t, err)
	_, err = f.store.Pool.Exec("UPDATE credential_refs SET attribution='team',attribution_basis='controlled deployment fixture' WHERE id=$1", shared.S("id"))
	must(t, err)
	unreachable := []struct{ what, id string }{
		{"a deployment-managed reference", managed.S("id")},
		{"a team-owned reference", shared.S("id")},
	}
	for _, row := range unreachable {
		// The row is the caller's own and sits in the caller's own tenant, so nothing below can be
		// explained by ownership or tenancy. What separates it from a reachable credential is the
		// attribution alone.
		if f.scalar("SELECT count(*) FROM credential_refs WHERE id=$1 AND owner_user_id=$2 AND tenant_id=$3", row.id, f.uid, f.tid) != 1 {
			t.Fatalf("%s must be one the caller owns before its refusal is evidence of anything", row.what)
		}
	}

	refuse := func(what, id string) {
		t.Helper()
		state := func() (string, int64, string) {
			t.Helper()
			var availability, reference string
			var version int64
			must(t, f.store.Pool.QueryRow("SELECT availability,version,secret_ref FROM credential_refs WHERE id=$1", id).
				Scan(&availability, &version, &reference))
			return availability, version, reference
		}
		availability, version, reference := state()
		records := f.scalar("SELECT count(*) FROM idempotency_records")

		// Both mutation routes, because the exception is a property of the credential rather than of one
		// verb: a rotate would put material under a reference nothing authorized, and a revoke would
		// withdraw authority the owner of that reference never delegated here.
		f.failAs(f.user, "PUT", f.path("/git-credentials/"+id), core.Object{"token": credentialToken + "-stolen", "version": version}, "", 404, "not_found")
		f.failAs(f.user, "DELETE", f.path("/git-credentials/"+id), core.Object{"version": version}, uuid.NewString(), 404, "not_found")

		afterAvailability, afterVersion, afterReference := state()
		if afterAvailability != availability || afterVersion != version || afterReference != reference {
			t.Fatalf("%s: a refused mutation disturbed the row: %s/%d/%s, want %s/%d/%s",
				what, afterAvailability, afterVersion, afterReference, availability, version, reference)
		}
		if f.scalar("SELECT count(*) FROM idempotency_records") != records {
			t.Fatalf("%s: a refused mutation recorded a result", what)
		}
		// The refusal is decided before the handoff, so no material was asked for and none was
		// destroyed. A row that answered 404 after replacing the secret would be the worst of both.
		if writes, rotates, revokes := secrets.handoffs(); writes != 0 || rotates != 0 || revokes != 0 {
			t.Fatalf("%s: a refused mutation reached the credential service: %d writes, %d rotates, %d revokes",
				what, writes, rotates, revokes)
		}
	}
	for _, row := range unreachable {
		refuse(row.what, row.id)
	}

	// The control. The same two routes, on a credential the caller stated as its own, must still work —
	// without it every assertion above would also pass if these routes refused everyone. The service
	// counters are read last and are exactly one of each, so the arithmetic covers the whole test: the
	// only traffic the service ever saw is the control's, and none of it can be attributed to a refused
	// request above.
	personal := f.createCredential(f.user, "personal", credentialScope)
	if rotated := f.call("PUT", f.path("/git-credentials/"+personal.S("id")), core.Object{"token": credentialToken + "-rotated", "version": 1}, "", 200); rotated.S("attribution") != "personal" || rotated.N("version") != 2 {
		t.Fatalf("the caller's own personal credential must remain rotatable: %v", rotated)
	}
	if revoked := f.call("DELETE", f.path("/git-credentials/"+personal.S("id")), core.Object{"version": 2}, uuid.NewString(), 200); revoked.S("availability") != "invalid" || revoked.N("version") != 3 {
		t.Fatalf("the caller's own personal credential must remain revocable: %v", revoked)
	}
	if writes, rotates, revokes := secrets.handoffs(); writes != 1 || rotates != 1 || revokes != 1 {
		t.Fatalf("the control must be all the traffic the service saw: %d writes, %d rotates, %d revokes", writes, rotates, revokes)
	}
}

// TestGitCredentialCreateIsIdempotentOnItsKey pins the whole reason create takes an idempotency key:
// the key names the credential id, so a retry after an ambiguous handoff reproduces the same id and
// the service recognizes the write as a replay instead of sealing a second copy of the secret.
func TestGitCredentialCreateIsIdempotentOnItsKey(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets
	key := uuid.NewString()
	body := core.Object{"name": "work laptop", "repositoryScope": credentialScope, "token": credentialToken}

	first := f.call("POST", f.path("/git-credentials"), body, key, 201)
	second := f.call("POST", f.path("/git-credentials"), body, key, 201)
	if first.S("id") == "" || second.S("id") != first.S("id") {
		t.Fatalf("a retry under one key must answer with the same credential: %s then %s", first.S("id"), second.S("id"))
	}
	if writes, _, _ := secrets.handoffs(); writes != 1 {
		t.Fatalf("a replayed create reached the service: %d writes", writes)
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE owner_user_id=$1 AND attribution='personal'", f.uid) != 1 {
		t.Fatal("a replayed create wrote a second credential")
	}
	// The same key with a different request is a conflict rather than either answer: the key already
	// names one credential, and honoring the new body would silently return a document that does not
	// describe what was asked for.
	f.failAs(f.user, "POST", f.path("/git-credentials"), core.Object{"name": "other", "repositoryScope": credentialScope, "token": credentialToken}, key, 409, "idempotency_conflict")
	// A create names the id the handoff will use, so it cannot proceed without one.
	f.failAs(f.user, "POST", f.path("/git-credentials"), body, "", 400, "idempotency_key_required")
	if writes, _, _ := secrets.handoffs(); writes != 1 {
		t.Fatalf("a refused create reached the service: %d writes", writes)
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE owner_user_id=$1 AND attribution='personal'", f.uid) != 1 {
		t.Fatal("a refused create left a credential behind")
	}

	// A revoke is idempotent on the same terms, and here the key is required for the same reason: the
	// recorded response is what a retry answers from, so there has to be something to record it under.
	f.failAs(f.user, "DELETE", f.path("/git-credentials/"+first.S("id")), core.Object{"version": 1}, "", 400, "idempotency_key_required")
	revokeKey := uuid.NewString()
	f.call("DELETE", f.path("/git-credentials/"+first.S("id")), core.Object{"version": 1}, revokeKey, 200)
	// The same key with a body naming another version is a different request, and the recorded one is
	// the only honest answer to the original.
	f.failAs(f.user, "DELETE", f.path("/git-credentials/"+first.S("id")), core.Object{"version": 9}, revokeKey, 409, "idempotency_conflict")
}

// TestGitCredentialIngressAcceptsTheTokenInNoOtherPosition pins condition 2. The token travels in one
// field of one request body: not under another name, not in a query, and not for an identity the
// caller does not own.
func TestGitCredentialIngressAcceptsTheTokenInNoOtherPosition(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets
	for _, field := range []string{"secret", "token", "accessToken", "password", "credential", "pat"} {
		body := core.Object{"name": "work", "repositoryScope": credentialScope, "token": credentialToken, field: credentialToken}
		if field == "token" {
			continue // the one accepted spelling is covered by every other test in this file
		}
		params := f.failAs(f.user, "POST", f.path("/git-credentials"), body, uuid.NewString(), 400, "unknown_field")
		if params.S("field") != field {
			t.Fatalf("the refusal must name the offending field, got %v", params)
		}
	}
	// Server-owned identity and scope are not fields a caller may supply, so they are rejected as
	// unknown rather than read: condition 1 derives ownership from claims and current state alone.
	// `capabilities` was one of these until the push capability gave the product a provisioning path,
	// and it is now a create-only field rather than a server-owned one — what bounds it is asserted
	// where a credential exists to be widened (`TestAPushCapableCredentialIsCreatedDeliberatelyAnd…`),
	// because the claim that matters there is that no later request may state it.
	for _, field := range []string{"tenantId", "ownerUserId", "userId", "id", "availability", "secretRef"} {
		body := core.Object{"name": "work", "repositoryScope": credentialScope, "token": credentialToken, field: uuid.NewString()}
		params := f.failAs(f.user, "POST", f.path("/git-credentials"), body, uuid.NewString(), 400, "unknown_field")
		if params.S("field") != field {
			t.Fatalf("%s: the refusal must name the offending field, got %v", field, params)
		}
	}
	if writes, _, _ := secrets.handoffs(); writes != 0 {
		t.Fatalf("a request that carried a token in a refused shape still reached the service: %d writes", writes)
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE attribution='personal'") != 0 {
		t.Fatal("a refused request wrote a credential")
	}
	// Malformed JSON is refused before anything is read out of it, and the token it may have contained
	// is not carried into the error path.
	status, raw := f.raw("POST", f.path("/git-credentials"), uuid.NewString(), `{"name":"work","repositoryScope":"`+credentialScope+`","token":"`+credentialToken+`"}{"name":"second"}`)
	if status != 400 || !strings.Contains(raw, "invalid_json") {
		t.Fatalf("a second JSON value must be refused: %d %s", status, raw)
	}
	if strings.Contains(raw, credentialToken) || f.scalar("SELECT count(*) FROM idempotency_records WHERE response::text LIKE '%'||$1||'%'", credentialToken) != 0 {
		t.Fatal("a malformed request echoed its token back")
	}
}

// TestGitCredentialIngressValidatesNameScopeAndToken pins what the boundary accepts on the way in. A
// refused request must never reach the service, because a token handed over for a credential Cloud
// then declines to record is material nothing points at.
func TestGitCredentialIngressValidatesNameScopeAndToken(t *testing.T) {
	cases := []struct {
		name   string
		body   core.Object
		code   string
		reason string
	}{
		{"an empty name", core.Object{"name": "", "repositoryScope": credentialScope, "token": credentialToken}, "invalid_credential_name", "a credential with no label cannot be listed"},
		{"a name past the bound", core.Object{"name": strings.Repeat("a", core.GitCredentialNameMax+1), "repositoryScope": credentialScope, "token": credentialToken}, "invalid_credential_name", "the bound is the column's own"},
		{"a name counting bytes rather than characters", core.Object{"name": strings.Repeat("凭", core.GitCredentialNameMax+1), "repositoryScope": credentialScope, "token": credentialToken}, "invalid_credential_name", "a multi-byte name is measured in characters"},
		{"a name carrying a newline", core.Object{"name": "work\nlaptop", "repositoryScope": credentialScope, "token": credentialToken}, "invalid_credential_name", "a newline would forge a second list entry"},
		{"an http scope", core.Object{"name": "work", "repositoryScope": "http://example.invalid/repo", "token": credentialToken}, "invalid_repository_scope", "V1 is HTTPS only"},
		{"an ssh scope", core.Object{"name": "work", "repositoryScope": "git@example.invalid:group/repo.git", "token": credentialToken}, "invalid_repository_scope", "an ssh remote carries a key this credential can never match"},
		{"a bare host scope", core.Object{"name": "work", "repositoryScope": "https://example.invalid", "token": credentialToken}, "invalid_repository_scope", "a host-wide scope is not one repository"},
		{"a scope with embedded credentials", core.Object{"name": "work", "repositoryScope": "https://user:token@example.invalid/repo", "token": credentialToken}, "invalid_repository_scope", "a scope must not be a second secret"},
		{"a scope with a query", core.Object{"name": "work", "repositoryScope": "https://example.invalid/repo?ref=main", "token": credentialToken}, "invalid_repository_scope", "parameters are not part of a repository's identity"},
		{"a scope past the bound", core.Object{"name": "work", "repositoryScope": "https://example.invalid/" + strings.Repeat("a", core.GitCredentialRepositoryScopeMax), "token": credentialToken}, "invalid_repository_scope", "the bound is the one a repository URL already has"},
		{"an empty token", core.Object{"name": "work", "repositoryScope": credentialScope, "token": ""}, "invalid_credential_token", "an empty secret is not a credential"},
		{"a token with a trailing newline", core.Object{"name": "work", "repositoryScope": credentialScope, "token": credentialToken + "\n"}, "invalid_credential_token", "a pasted newline is refused rather than trimmed"},
		{"a token with leading whitespace", core.Object{"name": "work", "repositoryScope": credentialScope, "token": " " + credentialToken}, "invalid_credential_token", "leading whitespace is part of the secret, not padding"},
		{"an over-long token", core.Object{"name": "work", "repositoryScope": credentialScope, "token": strings.Repeat("t", core.GitCredentialTokenMax+1)}, "invalid_credential_token", "the service's own bound is applied here so the refusal is a client error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			secrets := &recordingSecrets{}
			f.store.Secrets = secrets
			f.failAs(f.user, "POST", f.path("/git-credentials"), c.body, uuid.NewString(), 400, c.code)
			if writes, _, _ := secrets.handoffs(); writes != 0 {
				t.Fatalf("%s: the service was asked to store material Cloud then declined", c.reason)
			}
			if f.scalar("SELECT count(*) FROM credential_refs WHERE attribution='personal'") != 0 {
				t.Fatalf("%s: a refused create wrote a credential", c.reason)
			}
		})
	}
	// The accepted edge is inclusive, so a name and a token exactly at their bounds are usable. Without
	// this the table above would also pass if the boundary refused everything.
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}
	accepted := f.call("POST", f.path("/git-credentials"), core.Object{"name": strings.Repeat("a", core.GitCredentialNameMax), "repositoryScope": "https://example.invalid/" + strings.Repeat("a", 100), "token": strings.Repeat("t", core.GitCredentialTokenMax)}, uuid.NewString(), 201)
	if accepted.S("name") != strings.Repeat("a", core.GitCredentialNameMax) {
		t.Fatalf("the name bound must be inclusive: %v", accepted.S("name"))
	}
}

// TestGitCredentialListPagesWithinTheSharedWindow pins that the credential list is an ordinary page of
// the shared contract: the caller walks it with the documented cursor and the bound is enforced here
// rather than left to the client.
func TestGitCredentialListPagesWithinTheSharedWindow(t *testing.T) {
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}
	first := f.createCredential(f.user, "first", "https://example.invalid/first.git")
	second := f.createCredential(f.user, "second", "https://example.invalid/second.git")
	ordered := []string{first.S("id"), second.S("id")}
	if ordered[1] < ordered[0] {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}

	head := f.call("GET", f.path("/git-credentials")+"?limit=1", nil, "", 200)
	if listed := items(head); len(listed) != 1 || listed[0].S("id") != ordered[0] || head.S("nextCursor") != ordered[0] {
		t.Fatalf("the first page must be one row with that row as its cursor: %v", head)
	}
	tail := f.call("GET", f.path("/git-credentials")+"?limit=1&after="+head.S("nextCursor"), nil, "", 200)
	if listed := items(tail); len(listed) != 1 || listed[0].S("id") != ordered[1] || tail.S("nextCursor") != "" {
		t.Fatalf("the second page must be the other row and end the walk: %v", tail)
	}
	f.failAs(f.user, "GET", f.path("/git-credentials")+"?limit=101", nil, "", 400, "invalid_pagination")
	f.failAs(f.user, "GET", f.path("/git-credentials")+"?after=not-a-uuid", nil, "", 400, "invalid_cursor")
	// The whole list is two rows regardless of how it was walked, so no page repeated or dropped one.
	if len(items(f.call("GET", f.path("/git-credentials"), nil, "", 200))) != 2 {
		t.Fatal("walking the list changed its contents")
	}
}
