package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// credentialToken is fixed, obviously synthetic material. Nothing in this file is a real credential:
// the ingress contract is exercised against a constant that could not authenticate anywhere, so no
// test can hold a live secret and no failure message can print one.
const credentialToken = "ghp_synthetic-0000000000000000000000"

// credentialScope is the repository the fixtures' Project is created against, in the spelling a caller
// would plausibly paste — trailing `.git` and all — so the normalization is exercised on the way in
// rather than assumed.
const credentialScope = "https://example.invalid/repo.git"

// normalizedCredentialScope is what that spelling is stored as (D2's single normalized form).
const normalizedCredentialScope = "https://example.invalid/repo"

// recordingSecrets is the independent Credential Service's ingress as a test double. It stands in for
// the process on the far side of the handoff: it counts what Cloud asked of it, keeps the material
// only in this process's memory, and answers a repeated write for the same credential id with the
// reference it already holds — the idempotency store D4 requires of it, and the property Cloud's
// retry path depends on.
type recordingSecrets struct {
	mu       sync.Mutex
	refs     map[string]string
	material map[string]string
	versions map[string]int64
	writes   int
	rotates  int
	revokes  int
	// fail, when set, is what the next handoff of any kind returns, so the refusal mapping can be
	// exercised without a network.
	fail error
}

func (s *recordingSecrets) Write(_ context.Context, credentialID, token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return "", s.fail
	}
	if ref, ok := s.refs[credentialID]; ok {
		if s.material[credentialID] != token {
			// Two different secrets under one id. Cloud may not resolve that by overwriting.
			return "", refusal{code: "credential_conflict"}
		}
		return ref, nil
	}
	if s.refs == nil {
		s.refs, s.material = map[string]string{}, map[string]string{}
	}
	ref := "cs_" + credentialID
	s.refs[credentialID], s.material[credentialID] = ref, token
	s.writes++
	return ref, nil
}

// Rotate replaces the material and answers with the store's own version counter, which moves only when
// the material actually changes — the real Store's `Rotate`. Cloud records that version, so a double
// answering a constant would hide the value every later authorization names.
func (s *recordingSecrets) Rotate(_ context.Context, credentialID, token string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	if _, ok := s.refs[credentialID]; !ok {
		return 0, refusal{code: "credential_not_found"}
	}
	if s.versions == nil {
		s.versions = map[string]int64{}
	}
	current := s.versions[credentialID]
	if current == 0 {
		current = 1
	}
	if s.material[credentialID] != token {
		current++
	}
	s.material[credentialID], s.versions[credentialID] = token, current
	s.rotates++
	return current, nil
}

func (s *recordingSecrets) Revoke(_ context.Context, credentialID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	delete(s.material, credentialID)
	s.revokes++
	return nil
}

// ref reports the reference the service generated for one credential.
func (s *recordingSecrets) ref(credentialID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[credentialID]
}

// stored reports the material the service holds for one credential. It is the only place a test reads
// the token back from, which is the point: the secret lives on the far side of the boundary.
func (s *recordingSecrets) stored(credentialID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.material[credentialID]
}

func (s *recordingSecrets) handoffs() (writes, rotates, revokes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes, s.rotates, s.revokes
}

// refusal is a Credential Service refusal that names its own reason code — the only shape
// core.credentialStoreFault reads. Every other failure, including this one carrying an unlisted code,
// must degrade to the same unavailable answer.
type refusal struct{ code string }

func (r refusal) Error() string      { return "refused" }
func (r refusal) ReasonCode() string { return r.code }

// items returns a list response's elements. Every public list carries them under `items`; keeping the
// assertion here means a malformed list fails the same way in every test.
func items(o core.Object) []core.Object {
	raw, _ := o["items"].([]any)
	out := make([]core.Object, 0, len(raw))
	for _, element := range raw {
		if row, ok := element.(map[string]any); ok {
			out = append(out, core.Object(row))
		}
	}
	return out
}

// callAs sends one public request as a specific end user. The fixture's own page is alice's, and the
// credential rules are per-user, so the tests that prove isolation need a second identity on the wire.
func (f *fixture) callAs(user core.Claims, method, path string, body core.Object, key string, want int) core.Object {
	f.t.Helper()
	o, status, e := f.client.Call(context.Background(), method, path, "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &user, key, body)
	must(f.t, e)
	if status != want {
		f.t.Fatalf("%s %s as %s: want %d got %d %v", method, path, user.Subject, want, status, o)
	}
	return o
}

// failAs asserts one public request is refused with a specific reason code, and returns the fault's
// parameters. Asserting the code rather than "not 200" is what keeps a security boundary from passing
// its test for the wrong reason: an internal error from a broken query would satisfy the weaker check.
func (f *fixture) failAs(user core.Claims, method, path string, body core.Object, key string, want int, code string) core.Object {
	f.t.Helper()
	o, status, e := f.client.Call(context.Background(), method, path, "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &user, key, body)
	must(f.t, e)
	if status != want || o.S("code") != code {
		f.t.Fatalf("%s %s (%s): want %d %s got %d %v", method, path, user.Subject, want, code, status, o)
	}
	return o.O("params")
}

// rawAs sends one request without the simulator client's decoding and returns the status and the
// response bytes exactly as they arrived. A decoded assertion can only find a leak in a field it
// named; searching the bytes themselves is what makes "the response never carries this" a claim about
// the whole response rather than a list of remembered keys.
func (f *fixture) rawAs(user core.Claims, method, path, key, body string) (int, string) {
	f.t.Helper()
	return f.rawAt(f.cloud.URL, user, method, path, key, body)
}

func (f *fixture) raw(method, path, key, body string) (int, string) {
	f.t.Helper()
	return f.rawAs(f.user, method, path, key, body)
}

func decodeObject(t *testing.T, raw string) core.Object {
	t.Helper()
	out := core.Object{}
	if e := json.Unmarshal([]byte(raw), &out); e != nil {
		t.Fatalf("decode %q: %v", raw, e)
	}
	return out
}

// credentialBody is the create body for one credential, spelled as JSON so a test can put anything at
// all on the wire.
func credentialBody(name, scope, token string) string {
	return fmt.Sprintf(`{"name":%q,"repositoryScope":%q,"token":%q}`, name, scope, token)
}

// createCredential creates one credential for a user and returns its document.
func (f *fixture) createCredential(user core.Claims, name, scope string) core.Object {
	f.t.Helper()
	return f.callAs(user, "POST", f.path("/git-credentials"), core.Object{"name": name, "repositoryScope": scope, "token": credentialToken}, uuid.NewString(), 201)
}

// assertTokenAbsentFromCloudState proves the raw token reached no durable Cloud state: not the
// metadata row — including in place of the reference — not the idempotency record that keeps this
// response, and not the operation journal. Each query searches its whole recorded body as text, so a
// field nobody thought to assert is covered too.
func (f *fixture) assertTokenAbsentFromCloudState(token string) {
	f.t.Helper()
	for what, query := range map[string]string{
		"credential metadata": "SELECT count(*) FROM credential_refs WHERE secret_ref=$1 OR name=$1 OR repository_scope=$1",
		"idempotency record":  "SELECT count(*) FROM idempotency_records WHERE request_hash=$1 OR key=$1 OR response::text LIKE '%'||$1||'%'",
		"operation journal":   "SELECT count(*) FROM operations WHERE request::text LIKE '%'||$1||'%'",
	} {
		if f.scalar(query, token) != 0 {
			f.t.Fatalf("the token reached Cloud's %s", what)
		}
	}
}

// TestGitCredentialCreateHandsTheTokenOverAndKeepsOnlyMetadata walks the whole ingress boundary once:
// what the caller sends, what the service is asked for, what Cloud durably keeps, and what comes back.
func TestGitCredentialCreateHandsTheTokenOverAndKeepsOnlyMetadata(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets
	status, raw := f.raw("POST", f.path("/git-credentials"), uuid.NewString(), credentialBody("work laptop", credentialScope, credentialToken))
	if status != 201 {
		t.Fatalf("create: want 201 got %d %s", status, raw)
	}
	created := decodeObject(t, raw)
	id := created.S("id")
	if id == "" || created.S("name") != "work laptop" || created.S("ownerUserId") != f.uid || created.S("tenantId") != f.tid {
		t.Fatalf("credential document: %s", raw)
	}
	if created.S("attribution") != "personal" || created.S("availability") != "available" || created.N("version") != 1 {
		t.Fatalf("a new credential is personal, available and at version 1: %s", raw)
	}
	if created.S("repositoryScope") != normalizedCredentialScope {
		t.Fatalf("the scope is stored in its one normalized spelling: %q", created.S("repositoryScope"))
	}
	if got := fmt.Sprint(created["capabilities"]); got != "[clone]" {
		t.Fatalf("V1 grants exactly the clone capability, got %s", got)
	}
	// D9: the reference that locates the material is not a public field, so it is absent rather than
	// null and no client can come to depend on its presence.
	if _, ok := created["secretRef"]; ok {
		t.Fatal("the credential document carries the service's reference")
	}
	// The bytes are searched, not the decoded document: the token and the reference must not appear in
	// any field, any nested object, or any error text.
	if strings.Contains(raw, credentialToken) {
		t.Fatal("the create response echoed the token")
	}
	if strings.Contains(raw, secrets.ref(id)) {
		t.Fatal("the create response carried the service's reference")
	}
	var ref, scope, attribution, basis, availability, owner, purpose string
	var version int64
	must(t, f.store.Pool.QueryRow("SELECT secret_ref,repository_scope,attribution,attribution_basis,availability,owner_user_id::text,purpose,version FROM credential_refs WHERE id=$1", id).Scan(&ref, &scope, &attribution, &basis, &availability, &owner, &purpose, &version))
	if ref != secrets.ref(id) || owner != f.uid || purpose != "git" || scope != normalizedCredentialScope || attribution != "personal" || basis != "user_stated" || availability != "available" || version != 1 {
		t.Fatalf("row: ref=%q owner=%s purpose=%s scope=%q attribution=%s basis=%s availability=%s version=%d", ref, owner, purpose, scope, attribution, basis, availability, version)
	}
	if writes, rotates, revokes := secrets.handoffs(); writes != 1 || rotates != 0 || revokes != 0 {
		t.Fatalf("create must be one write and nothing else: %d writes, %d rotates, %d revokes", writes, rotates, revokes)
	}
	if secrets.stored(id) != credentialToken {
		t.Fatal("the service did not receive the token")
	}
	f.assertTokenAbsentFromCloudState(credentialToken)
	// The metadata row and the response that created it are the only durable records, and the response
	// is recorded under the credential's own id so a retry can replay it.
	if f.scalar("SELECT count(*) FROM idempotency_records WHERE response::text LIKE '%'||$1||'%'", id) != 1 {
		t.Fatal("the created credential was not recorded for idempotent replay")
	}
}

// TestGitCredentialListIsScopedToTheOwningUser pins D1's attribution root: the list is the caller's own
// personal credentials in this tenant, and nothing else can appear in it.
func TestGitCredentialListIsScopedToTheOwningUser(t *testing.T) {
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}
	dana, _ := f.addUser(t, "git-credential-dana", "Dana")
	alice := f.createCredential(f.user, "alice work", credentialScope)
	danaCredential := f.createCredential(dana, "dana work", "https://example.invalid/other.git")
	// A deployment-managed reference is the `unknown` attribution `ConfigureCredential` writes. It is
	// owned by alice and lives in this tenant, and D2 makes it invisible to the product list all the
	// same: it is not a credential a user manages.
	reference, err := f.store.ConfigureCredential(context.Background(), f.tid, f.uid, "deployment/managed-reference")
	must(t, err)

	listed := items(f.call("GET", f.path("/git-credentials"), nil, "", 200))
	if len(listed) != 1 || listed[0].S("id") != alice.S("id") {
		t.Fatalf("alice sees exactly her own product credential: %v", listed)
	}
	if _, ok := listed[0]["secretRef"]; ok {
		t.Fatal("the list carries the service's reference")
	}
	theirs := items(f.callAs(dana, "GET", f.path("/git-credentials"), nil, "", 200))
	if len(theirs) != 1 || theirs[0].S("id") != danaCredential.S("id") {
		t.Fatalf("dana sees exactly her own credential: %v", theirs)
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE id=$1 AND attribution='unknown'", reference.S("id")) != 1 {
		t.Fatal("the deployment reference is not recorded as unknown attribution")
	}
	// Re-attributing the product credential as a team credential removes it from the personal list,
	// which is the filter working rather than the query happening to return one row.
	_, err = f.store.Pool.Exec("UPDATE credential_refs SET attribution='team' WHERE id=$1", alice.S("id"))
	must(t, err)
	if listed = items(f.call("GET", f.path("/git-credentials"), nil, "", 200)); len(listed) != 0 {
		t.Fatalf("a team credential appeared in a personal list: %v", listed)
	}
}

// TestGitCredentialRotateReplacesMaterialBeforeAdvancingTheVersion covers the ordering store D4
// requires and every precondition a rotation has, each on its own credential so one case's state
// change cannot mask another's.
func TestGitCredentialRotateReplacesMaterialBeforeAdvancingTheVersion(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets
	frozen := f.createCredential(f.user, "frozen", credentialScope)
	_, err := f.store.Pool.Exec("UPDATE credential_refs SET availability='frozen',unavailable_reason='associated_member_departed' WHERE id=$1", frozen.S("id"))
	must(t, err)
	live := f.createCredential(f.user, "live", credentialScope)
	other, _ := f.addUser(t, "git-credential-other", "Other")
	foreign := f.createCredential(other, "foreign", credentialScope)

	rotated := f.call("PUT", f.path("/git-credentials/"+live.S("id")), core.Object{"token": credentialToken + "-rotated", "version": 1}, "", 200)
	if rotated.N("version") != 2 || rotated.S("id") != live.S("id") {
		t.Fatalf("rotate advances the metadata version in place: %v", rotated)
	}
	if rotated.S("repositoryScope") != normalizedCredentialScope || rotated.S("availability") != "available" || rotated.S("name") != "live" {
		t.Fatalf("rotate replaces the material and nothing else about the credential: %v", rotated)
	}
	if _, ok := rotated["secretRef"]; ok {
		t.Fatal("the rotate response carries the service's reference")
	}
	if _, rotates, _ := secrets.handoffs(); rotates != 1 {
		t.Fatalf("rotate must replace the material exactly once, got %d", rotates)
	}
	if secrets.stored(live.S("id")) != credentialToken+"-rotated" {
		t.Fatal("the service did not receive the replacement material")
	}
	// The reference locates the credential, not one version of it, so the service keeps it across a
	// rotation and Cloud's row still points at the same place.
	if f.scalar("SELECT count(*) FROM credential_refs WHERE id=$1 AND secret_ref=$2", live.S("id"), secrets.ref(live.S("id"))) != 1 {
		t.Fatal("rotation re-pointed the credential at a new material location")
	}

	// Every precondition is decided before the service is asked to replace anything, so none of these
	// may leave new material behind. The checked count afterwards is the assertion that matters.
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+live.S("id")), core.Object{"token": credentialToken + "-third", "version": 0}, "", 428, "version_required")
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+live.S("id")), core.Object{"token": credentialToken + "-third", "version": 1}, "", 409, "version_conflict")
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+frozen.S("id")), core.Object{"token": credentialToken + "-third", "version": 2}, "", 409, "credential_not_available")
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+foreign.S("id")), core.Object{"token": credentialToken + "-third", "version": 1}, "", 404, "not_found")
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+live.S("id")), core.Object{"token": "trailing newline\n", "version": 2}, "", 400, "invalid_credential_token")
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+uuid.NewString()), core.Object{"token": credentialToken, "version": 1}, "", 404, "not_found")
	if _, rotates, _ := secrets.handoffs(); rotates != 1 {
		t.Fatalf("a refused rotation reached the service: %d", rotates)
	}
	if secrets.stored(foreign.S("id")) != credentialToken || secrets.stored(frozen.S("id")) != credentialToken {
		t.Fatal("a refused rotation replaced material it had no business touching")
	}
	if secrets.stored(live.S("id")) != credentialToken+"-rotated" {
		t.Fatal("a refused rotation disturbed the credential it named")
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen' AND version=1", frozen.S("id")) != 1 {
		t.Fatal("a refused rotation advanced a frozen credential's version")
	}
}

// TestGitCredentialRevokeIsAuthoritativeAndIdempotent pins D5: Cloud's availability change is the
// revocation, it lands before the material is destroyed, and asking twice is not an error.
func TestGitCredentialRevokeIsAuthoritativeAndIdempotent(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets
	created := f.createCredential(f.user, "to revoke", credentialScope)
	id := created.S("id")
	path := f.path("/git-credentials/" + id)
	key := uuid.NewString()

	revoked := f.call("DELETE", path, core.Object{"version": 1}, key, 200)
	if revoked.S("availability") != "invalid" || revoked.S("unavailableReason") != "revoked_by_owner" || revoked.N("version") != 2 {
		t.Fatalf("revocation is expressed through availability: %v", revoked)
	}
	if revoked["revokedAt"] == nil {
		t.Fatal("revocation records when it happened")
	}
	if _, _, revokes := secrets.handoffs(); revokes != 1 {
		t.Fatalf("revocation must destroy the material exactly once, got %d", revokes)
	}
	if secrets.stored(id) != "" {
		t.Fatal("the service still holds the material")
	}
	var deleted any
	must(t, f.store.Pool.QueryRow("SELECT deleted_at FROM credential_refs WHERE id=$1", id).Scan(&deleted))
	if deleted != nil {
		t.Fatal("revocation wrote the deletion column, whose semantics are deliberately undefined")
	}
	// A revoked credential stays in its owner's list: that entry is the only way "revoked" remains
	// distinguishable from "never existed".
	listed := items(f.call("GET", f.path("/git-credentials"), nil, "", 200))
	if len(listed) != 1 || listed[0].S("availability") != "invalid" {
		t.Fatalf("a revoked credential must remain visible to its owner: %v", listed)
	}

	// The same request replayed under its own idempotency key answers from the recorded response: no
	// authoritative state is decided a second time, and the version therefore cannot move. The destroy
	// attempt does repeat, which is deliberate rather than incidental — the service's revoke is
	// idempotent, "already gone" is success, and a client's retry is exactly where material a first
	// attempt could not destroy should be swept.
	replay := f.call("DELETE", path, core.Object{"version": 1}, key, 200)
	if replay.N("version") != 2 || replay.S("availability") != "invalid" {
		t.Fatalf("a replayed revocation re-decided authoritative state: %v", replay)
	}
	if f.scalar("SELECT count(*) FROM credential_refs WHERE id=$1 AND version=2 AND availability='invalid'", id) != 1 {
		t.Fatal("a replayed revocation changed the stored row")
	}
	// A second revocation under a fresh key is not a conflict either: the caller's desired end state is
	// already true, and it cannot observe the version the first call produced.
	again := f.call("DELETE", path, core.Object{"version": 1}, uuid.NewString(), 200)
	if again.N("version") != 2 || again.S("availability") != "invalid" {
		t.Fatalf("a repeated revocation must be a no-op: %v", again)
	}
	// A service that cannot confirm destruction does not turn the revocation into a failure: Cloud's
	// own state is authoritative and already committed. The residue goes to the operator instead.
	secrets.fail = errors.New("dial tcp 10.0.0.9:8443: connection refused")
	residue := f.call("DELETE", path, core.Object{"version": 1}, uuid.NewString(), 200)
	if residue.S("availability") != "invalid" {
		t.Fatalf("an unreachable service failed a committed revocation: %v", residue)
	}
	secrets.fail = nil
}

// TestGitCredentialIngressFailsClosedWithoutACredentialService covers store D6's gate as an ingress
// rule: a deployment with no credential service cannot hold a token, and it must refuse before writing
// anything it could not honor.
func TestGitCredentialIngressFailsClosedWithoutACredentialService(t *testing.T) {
	f := setup(t)
	if f.store.Secrets != nil {
		t.Fatal("this test needs a deployment with no credential service")
	}
	key := uuid.NewString()
	f.failAs(f.user, "POST", f.path("/git-credentials"), core.Object{"name": "work", "repositoryScope": credentialScope, "token": credentialToken}, key, 503, "credential_store_unavailable")
	if f.scalar("SELECT count(*) FROM credential_refs WHERE purpose='git' AND attribution='personal'") != 0 {
		t.Fatal("an unconfigured deployment wrote credential metadata it could not honor")
	}
	if f.scalar("SELECT count(*) FROM idempotency_records WHERE key=$1", key) != 0 {
		t.Fatal("a refused create was recorded as if it had happened")
	}
	f.assertTokenAbsentFromCloudState(credentialToken)
	// A revoke is the one mutation that does not need the service: it removes material rather than
	// creating it, so a credential that reached this deployment some other way can still be withdrawn.
	orphan := uuid.NewString()
	_, err := f.store.Pool.Exec("INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,attribution_basis,repository_scope,capabilities) VALUES($1,$2,$3,'git','cs_orphan','orphan','personal','user_stated',$4,'[\"clone\"]'::jsonb)", orphan, f.tid, f.uid, normalizedCredentialScope)
	must(t, err)
	revoked := f.call("DELETE", f.path("/git-credentials/"+orphan), core.Object{"version": 1}, uuid.NewString(), 200)
	if revoked.S("availability") != "invalid" || revoked.S("unavailableReason") != "revoked_by_owner" {
		t.Fatalf("revocation must not require a credential service: %v", revoked)
	}
}

// TestGitCredentialServiceRefusalsBecomeReasonCodes pins D6/D7's mapping end to end: the service's own
// vocabulary decides the public code, anything unrecognized fails closed, and no refusal ever leaves a
// metadata row behind.
func TestGitCredentialServiceRefusalsBecomeReasonCodes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"an unreachable service", errors.New("dial tcp 10.0.0.9:8443: connection refused"), 503, "credential_store_unavailable"},
		{"a refusal this build does not know", refusal{code: "something_new"}, 503, "credential_store_unavailable"},
		{"a different secret under a replayed id", refusal{code: "credential_conflict"}, 409, "credential_store_inconsistent"},
		{"a reference with no material", refusal{code: "credential_not_found"}, 409, "credential_store_inconsistent"},
		{"a request the service refuses", refusal{code: "invalid_request"}, 400, "credential_store_rejected_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.store.Secrets = &recordingSecrets{fail: c.err}
			params := f.failAs(f.user, "POST", f.path("/git-credentials"), core.Object{"name": "work", "repositoryScope": credentialScope, "token": credentialToken}, uuid.NewString(), c.status, c.code)
			// The failure's own text is transport detail. D7 keeps it out of the response, so not even
			// the parameters may quote it.
			for _, value := range params {
				if text, ok := value.(string); ok && strings.Contains(text, "dial tcp") {
					t.Fatalf("the fault leaked the transport's text: %v", params)
				}
			}
			if f.scalar("SELECT count(*) FROM credential_refs WHERE purpose='git' AND attribution='personal'") != 0 {
				t.Fatal("a failed handoff left credential metadata behind")
			}
		})
	}
}

// TestAPushCapableCredentialIsCreatedDeliberatelyAndRestoresNoAuthority is the provisioning path the
// push capability needed, and the four things that make it a boundary rather than a switch.
//
// Before this, `POST /git-credentials` wrote `["clone"]` unconditionally, so no reference in any
// deployment could permit a push and every push approval was refused at the capability check. The
// field is the smallest hole that closes: it exists on the create alone, and it is optional, so the
// body every existing client sends still produces the reference it produced before.
//
// The other three assertions are what keep it from being a way to widen authority: the set is refused
// rather than repaired when it is not the vocabulary's own spelling; a rotation rewrites material and
// leaves the set exactly as it was (nothing upgrades a credential in place, and nothing downgrades one
// either); and a reference that permits a push does not permit anything else — it is accepted by the
// push admission and refused by the clone admission, which is the separation the capability set exists
// to express.
func TestAPushCapableCredentialIsCreatedDeliberatelyAndRestoresNoAuthority(t *testing.T) {
	f := setup(t)
	f.store.Secrets = &recordingSecrets{}

	readOnly := f.call("POST", f.path("/git-credentials"), core.Object{"name": "read only", "repositoryScope": credentialScope, "token": credentialToken}, uuid.NewString(), 201)
	if got := fmt.Sprint(readOnly["capabilities"]); got != "[clone]" {
		t.Fatalf("a create that names no capabilities must still produce the clone-only reference: %s", got)
	}
	// The same set is refused when it is not the vocabulary's spelling, and the refusal happens before
	// the handoff, so a rejected request stores no reference and no material.
	f.failAs(f.user, "POST", f.path("/git-credentials"), core.Object{"name": "widened", "repositoryScope": credentialScope, "token": credentialToken, "capabilities": []any{"clone", "clone"}}, uuid.NewString(), 400, "invalid_credential_capabilities")
	f.failAs(f.user, "POST", f.path("/git-credentials"), core.Object{"name": "widened", "repositoryScope": credentialScope, "token": credentialToken, "capabilities": []any{}}, uuid.NewString(), 400, "invalid_credential_capabilities")
	f.failAs(f.user, "POST", f.path("/git-credentials"), core.Object{"name": "widened", "repositoryScope": credentialScope, "token": credentialToken, "capabilities": []any{"admin"}}, uuid.NewString(), 400, "invalid_credential_capabilities")
	if f.scalar("SELECT count(*) FROM credential_refs WHERE name='widened'") != 0 {
		t.Fatal("a refused capability set still created a reference")
	}

	writer := f.call("POST", f.path("/git-credentials"), core.Object{"name": "writer", "repositoryScope": credentialScope, "token": credentialToken, "capabilities": []any{"push"}}, uuid.NewString(), 201)
	if got := fmt.Sprint(writer["capabilities"]); got != "[push]" {
		t.Fatalf("a requested push capability must be stored as asked: %s", got)
	}
	var stored string
	must(t, f.store.Pool.QueryRow("SELECT capabilities::text FROM credential_refs WHERE id=$1", writer.S("id")).Scan(&stored))
	if stored != `["push"]` {
		t.Fatalf("the row does not carry the set the request declared: %s", stored)
	}

	rotated := f.call("PUT", f.path("/git-credentials/"+writer.S("id")), core.Object{"token": credentialToken + "-rotated", "version": 1}, uuid.NewString(), 200)
	if got := fmt.Sprint(rotated["capabilities"]); got != "[push]" {
		t.Fatalf("rotation must not change what a credential permits: %s", got)
	}
	// The field is a create's alone. A rotate that carried it would be an attempt to widen a reference
	// that already exists, so the boundary refuses the name outright instead of reading and ignoring it,
	// and the row is asserted unchanged afterwards.
	f.failAs(f.user, "PUT", f.path("/git-credentials/"+writer.S("id")), core.Object{"token": credentialToken + "-second", "version": 2, "capabilities": []any{"clone", "push"}}, uuid.NewString(), 400, "unknown_field")
	var afterRotate string
	must(t, f.store.Pool.QueryRow("SELECT capabilities::text FROM credential_refs WHERE id=$1", writer.S("id")).Scan(&afterRotate))
	if afterRotate != `["push"]` {
		t.Fatalf("a refused rotate changed what the credential permits: %s", afterRotate)
	}
	// The one thing a deployment must never be able to do by accident is widen an existing reference,
	// so the clone-only credential created above is re-read here rather than assumed unchanged.
	var readOnlyStored string
	must(t, f.store.Pool.QueryRow("SELECT capabilities::text FROM credential_refs WHERE id=$1", readOnly.S("id")).Scan(&readOnlyStored))
	if readOnlyStored != `["clone"]` {
		t.Fatalf("a second credential's creation changed the first one's capabilities: %s", readOnlyStored)
	}

	// Two Projects over the same repository, one per credential, so the two admissions are asked the
	// same question about two references that differ only in their capability set.
	pushProject := f.call("POST", f.path("/projects"), core.Object{"name": "Writer", "repositoryUrl": credentialScope, "defaultBranch": "main", "credentialRefId": writer.S("id")}, "push-capable", 202)
	readProject := f.call("POST", f.path("/projects"), core.Object{"name": "Reader", "repositoryUrl": credentialScope, "defaultBranch": "main", "credentialRefId": readOnly.S("id")}, "read-only", 202)
	approval := core.Object{"requestId": "approve-one", "ref": "refs/heads/main", "oldCommit": strings.Repeat("a", 40), "newCommit": strings.Repeat("b", 40)}

	// The clone-only reference is refused exactly as it was before this change: the write path does not
	// accept a reference that may only read, and `clone` is not a substitute for `push`.
	f.failAs(f.user, "POST", f.path("/workspaces/"+readProject.O("workspace").S("id")+"/pushes"), approval, uuid.NewString(), 409, "repository_credential_capability_unsupported")

	// The push-capable reference passes the capability check and is stopped by the next thing that is
	// true of this deployment: no executor is installed, so the approval is recorded and settled as
	// failed rather than left looking dispatched, and no authorization is ever signed for it.
	accepted := f.call("POST", f.path("/workspaces/"+pushProject.O("workspace").S("id")+"/pushes"), approval, uuid.NewString(), 202)
	if accepted.S("state") != "failed" || accepted.S("resultCode") != "push_executor_unavailable" {
		t.Fatalf("an approved push on an executor-less deployment must settle as failed: %v", accepted)
	}
	if accepted.S("credentialRefId") != writer.S("id") || accepted.N("credentialRefVersion") != 2 {
		t.Fatalf("the approval did not bind the credential version it was approved against: %v", accepted)
	}
	if accepted.S("authorizationId") == "" {
		t.Fatal("the approval record must name the authorization it would have used")
	}
	// A capability is not a write. The deployment performed none, and the reference is unchanged by
	// having been approved against once.
	if f.scalar("SELECT count(*) FROM clone_executions") != 0 {
		t.Fatal("a push approval produced an execution")
	}
}
