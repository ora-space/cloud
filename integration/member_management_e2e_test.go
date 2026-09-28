package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

// staticDirectory is a corporate-deployment Tianzhou double: the keyword
// selects which person is found. The Gone person is reported as unemployed so
// tests can cover the employment recheck, mirroring the router.Directory
// contract.
type staticDirectory struct{}

func (staticDirectory) Search(_ context.Context, keyword string) ([]core.DirectoryPerson, error) {
	if len(strings.TrimSpace(keyword)) < 2 {
		return nil, &core.Fault{Code: "invalid_keyword", Status: 400, Params: core.Object{}}
	}
	switch strings.TrimSpace(keyword) {
	case "Other":
		return []core.DirectoryPerson{{GlobalUserID: "1001", Name: "Other Employee", EmployeeNumber: "00934888", DepartmentName: "R&D", Employed: true}}, nil
	case "Gone":
		return []core.DirectoryPerson{{GlobalUserID: "1002", Name: "Former Employee", EmployeeNumber: "00934889", DepartmentName: "R&D", Employed: false}}, nil
	default:
		return []core.DirectoryPerson{{GlobalUserID: "205045249610656", Name: "Employee", EmployeeNumber: "00934887", DepartmentName: "R&D", Employed: true}}, nil
	}
}

// callAs runs one HTTP call as an arbitrary user through the fixture router.
func callAs(t *testing.T, f *fixture, user core.Claims, method, path, key string, body core.Object, want int) core.Object {
	t.Helper()
	out, status, err := f.client.Call(context.Background(), method, path, "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &user, key, body)
	must(t, err)
	if status != want {
		t.Fatalf("%s %s as %s: want %d got %d %v", method, path, user.Subject, want, status, out)
	}
	return out
}

// codeOf extracts the stable Fault code from an error response body.
func codeOf(t *testing.T, out core.Object) string {
	t.Helper()
	if out.S("code") == "" {
		t.Fatalf("response carries no fault code: %v", out)
	}
	return out.S("code")
}

// TestMemberPutValidationAndAdminGates covers the tenant member PUT matrix:
// who may write, which inputs are accepted, optimistic versioning, and the
// last-admin invariant for self-demotion and self-disable.
func TestMemberPutValidationAndAdminGates(t *testing.T) {
	f := setup(t)
	bob, bobID := f.addUser(t, "bob", "Bob")
	carol := joinUser("carol")

	// An ordinary active member reads the roster with display names; a
	// non-member and a caller with no membership are refused.
	roster := callAs(t, f, bob, "GET", f.path("/members"), "", nil, 200)
	items := roster["items"].([]any)
	found := false
	for _, item := range items {
		row := core.Object(item.(map[string]any))
		if row.S("userId") == bobID {
			found = row.S("displayName") == "Bob" && row.S("role") == "member" && row.S("status") == "active"
		}
	}
	if !found {
		t.Fatalf("member roster must include Bob's projection: %v", roster)
	}
	callAs(t, f, carol, "GET", f.path("/members"), "", nil, 403)
	f.call("GET", f.path("/members?limit=101"), nil, "", 400)
	f.call("GET", f.path("/members?after=not-a-uuid"), nil, "", 400)

	// Only administrators write membership state.
	if code := codeOf(t, callAs(t, f, bob, "PUT", f.path("/members/"+bobID), "", core.Object{"role": "member", "status": "active", "version": 1}, 403)); code != "admin_required" {
		t.Fatalf("member PUT gate: want admin_required got %s", code)
	}

	// Validation matrix as administrator: role/status vocabulary, unknown
	// users, missing membership rows, and version preconditions.
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "owner", "status": "active", "version": 1}, "", 400)
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "member", "status": "paused", "version": 1}, "", 400)
	f.call("PUT", f.path("/members/not-a-uuid"), core.Object{"role": "member", "status": "active", "version": 1}, "", 400)
	f.call("PUT", f.path("/members/"+uuid.NewString()), core.Object{"role": "member", "status": "active", "version": 1}, "", 404)
	dave := callAs(t, f, joinUser("dave"), "GET", "/api/v1/me", "", nil, 200)
	f.call("PUT", f.path("/members/"+dave.S("id")), core.Object{"role": "member", "status": "active", "version": 1}, "", 404)
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active"}, "", 428)
	if code := codeOf(t, f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 99}, "", 409)); code != "version_conflict" {
		t.Fatalf("stale member version: want version_conflict got %s", code)
	}

	// Promotion bumps the version; with two admins the second one can be
	// demoted, but the survivor can neither demote nor disable itself.
	promoted := f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	if promoted.S("role") != "admin" || promoted.N("version") != 2 {
		t.Fatalf("promotion must record role and bump version: %v", promoted)
	}
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "member", "status": "active", "version": 2}, "", 200)
	if code := codeOf(t, f.call("PUT", f.path("/members/"+f.uid), core.Object{"role": "member", "status": "active", "version": 1}, "", 409)); code != "last_admin" {
		t.Fatalf("last admin demotion: want last_admin got %s", code)
	}
	if code := codeOf(t, f.call("PUT", f.path("/members/"+f.uid), core.Object{"role": "admin", "status": "disabled", "version": 1}, "", 409)); code != "last_admin" {
		t.Fatalf("last admin disable: want last_admin got %s", code)
	}

	// A user whose account row is disabled is no longer an addressable member.
	_, err := f.store.Pool.Exec("UPDATE users SET status='disabled' WHERE id=$1", bobID)
	must(t, err)
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 3}, "", 404)
	// The disabled account is also refused at the identity boundary.
	callAs(t, f, bob, "GET", f.path("/members"), "", nil, 403)
}

// TestInvitationTokenValidationAndRevocationSemantics covers join-token
// validation, digest-only persistence, revocation versioning, admin-only
// management, idempotent creation and replay, and pagination.
func TestInvitationTokenValidationAndRevocationSemantics(t *testing.T) {
	f := setup(t)
	bob, _ := f.addUser(t, "bob", "Bob")

	// Malformed tokens are rejected at creation and redemption without any row.
	badTokens := []string{"", "short", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("A", 42) + "B", strings.Repeat("A", 42) + "+"}
	for i, bad := range badTokens {
		key := strconv.Itoa(i)
		out := f.call("POST", f.path("/invitations"), core.Object{"token": bad}, "bad-invite-"+key, 400)
		if codeOf(t, out) != "invalid_join_token" {
			t.Fatalf("bad invitation token %q: want invalid_join_token got %s", bad, out.S("code"))
		}
		f.call("POST", f.path("/join-links"), core.Object{"token": bad}, "bad-link-"+key, 400)
		joinCall(t, f, joinUser("mallory"), "POST", "/api/v1/join/invitations/redeem", "bad-redeem-"+key, core.Object{"token": bad}, 400)
	}
	f.call("POST", f.path("/invitations"), core.Object{"token": joinToken('a'), "extra": 1}, "unknown-field", 400)
	if f.scalar("SELECT count(*) FROM tenant_invitations") != 0 || f.scalar("SELECT count(*) FROM tenant_join_links") != 0 {
		t.Fatal("rejected token writes persisted rows")
	}

	// Unknown but well-formed tokens never reveal whether a link exists.
	out := joinCall(t, f, joinUser("mallory"), "POST", "/api/v1/join/invitations/redeem", "unknown-token", core.Object{"token": joinToken('m')}, 404)
	if codeOf(t, out) != "join_link_unavailable" {
		t.Fatalf("unknown token: want join_link_unavailable got %s", out.S("code"))
	}

	// Creation persists only the SHA-256 digest and returns token-free metadata.
	token := joinToken('b')
	first := f.call("POST", f.path("/invitations"), core.Object{"token": token}, "invite-create", 201)
	if first.S("id") == "" || first["token"] != nil {
		t.Fatalf("invitation response must be token-free metadata: %v", first)
	}
	if digest := sha256Hex(t, token); f.scalar("SELECT count(*) FROM tenant_invitations WHERE id=$1 AND token_hash=decode($2,'hex')", first.S("id"), digest) != 1 {
		t.Fatal("invitation token was not stored as its SHA-256 digest")
	}
	// An idempotent replay returns the same row without a second invitation.
	replay := f.call("POST", f.path("/invitations"), core.Object{"token": token}, "invite-create", 201)
	if replay.S("id") != first.S("id") || f.scalar("SELECT count(*) FROM tenant_invitations") != 1 {
		t.Fatal("invitation replay duplicated the link")
	}
	// A failed attempt never poisons the idempotency key.
	f.call("POST", f.path("/invitations"), core.Object{"token": "broken"}, "retry-after-failure", 400)
	after := f.call("POST", f.path("/invitations"), core.Object{"token": joinToken('c')}, "retry-after-failure", 201)
	if after.S("id") == "" || f.scalar("SELECT count(*) FROM tenant_invitations") != 2 {
		t.Fatal("failed creation poisoned the idempotency key")
	}

	// Revocation is versioned, and re-revocation with the fresh version stays
	// idempotent instead of erroring.
	f.call("DELETE", f.path("/invitations/"+first.S("id")), core.Object{}, "revoke-no-version", 428)
	if code := codeOf(t, f.call("DELETE", f.path("/invitations/"+first.S("id")), core.Object{"version": 99}, "revoke-stale", 409)); code != "version_conflict" {
		t.Fatalf("stale revocation: want version_conflict got %s", code)
	}
	f.call("DELETE", f.path("/invitations/"+uuid.NewString()), core.Object{"version": 1}, "revoke-unknown", 404)
	revoked := f.call("DELETE", f.path("/invitations/"+first.S("id")), core.Object{"version": first.N("version")}, "revoke-1", 200)
	if revoked["revokedAt"] == nil || revoked["consumedAt"] != nil {
		t.Fatalf("revocation must set revokedAt only: %v", revoked)
	}
	again := f.call("DELETE", f.path("/invitations/"+first.S("id")), core.Object{"version": revoked.N("version")}, "revoke-2", 200)
	if again.N("version") != revoked.N("version") {
		t.Fatalf("re-revocation must not bump the version again: %v", again)
	}
	// An idempotent replay of the revocation returns the recorded response.
	replayed := f.call("DELETE", f.path("/invitations/"+first.S("id")), core.Object{"version": first.N("version")}, "revoke-1", 200)
	if replayed.S("id") != first.S("id") || replayed.N("version") != revoked.N("version") {
		t.Fatalf("revocation replay must return the recorded response: %v", replayed)
	}

	// Join links follow the same revocation contract.
	linkTokenValue := joinToken('d')
	link := f.call("POST", f.path("/join-links"), core.Object{"token": linkTokenValue}, "link-create", 201)
	f.call("DELETE", f.path("/join-links/"+link.S("id")), core.Object{"version": link.N("version")}, "link-revoke", 200)
	joinCall(t, f, joinUser("erin"), "POST", "/api/v1/join/requests", "apply-revoked", core.Object{"token": linkTokenValue}, 404)

	// Management is admin-only: ordinary members see neither list nor writes.
	for _, method := range []string{"GET", "POST"} {
		callAs(t, f, bob, method, f.path("/invitations"), "member-"+method, core.Object{"token": joinToken('e')}, 403)
		callAs(t, f, bob, method, f.path("/join-links"), "member-link-"+method, core.Object{"token": joinToken('f')}, 403)
	}
	callAs(t, f, bob, "GET", f.path("/join-requests"), "", nil, 403)

	// Deliberate token reuse is a client conflict, never an internal error,
	// and the digest stays reserved even after revocation.
	spent := joinToken('p')
	f.call("POST", f.path("/invitations"), core.Object{"token": spent}, "invite-reuse-1", 201)
	if code := codeOf(t, f.call("POST", f.path("/invitations"), core.Object{"token": spent}, "invite-reuse-2", 409)); code != "join_token_conflict" {
		t.Fatalf("duplicate invitation token: want join_token_conflict got %s", code)
	}
	linkReuse := joinToken('q')
	f.call("POST", f.path("/join-links"), core.Object{"token": linkReuse}, "link-reuse-1", 201)
	if code := codeOf(t, f.call("POST", f.path("/join-links"), core.Object{"token": linkReuse}, "link-reuse-2", 409)); code != "join_token_conflict" {
		t.Fatalf("duplicate join-link token: want join_token_conflict got %s", code)
	}
	if f.scalar("SELECT count(*) FROM tenant_invitations WHERE token_hash=decode($1,'hex')", sha256Hex(t, spent)) != 1 {
		t.Fatal("duplicate token created a second invitation")
	}

	// Lists page in UUID order and expose only metadata.
	total := f.scalar("SELECT count(*) FROM tenant_invitations")
	var listed []any
	cursor := ""
	// Bound the walk so a server regression that never drains the cursor fails
	// fast instead of spinning until the global test timeout.
	for range total + 1 {
		pagePath := f.path("/invitations?limit=2")
		if cursor != "" {
			pagePath += "&after=" + cursor
		}
		pageList := f.call("GET", pagePath, nil, "", 200)
		listed = append(listed, pageList["items"].([]any)...)
		cursor = pageList.S("nextCursor")
		if cursor == "" {
			break
		}
	}
	if cursor != "" {
		t.Fatalf("pagination cursor never drained within %d pages", total+1)
	}
	if len(listed) != total {
		t.Fatalf("pagination must walk every invitation exactly once: listed=%d total=%d", len(listed), total)
	}
	// Walking every row exactly once also pins the keyset contract: ids must
	// advance in UUID order without revisits, and no page leaks token material.
	var ids []string
	seen := map[string]bool{}
	for _, item := range listed {
		row := core.Object(item.(map[string]any))
		if seen[row.S("id")] {
			t.Fatalf("pagination revisited invitation %s", row.S("id"))
		}
		seen[row.S("id")] = true
		ids = append(ids, row.S("id"))
		if row["token"] != nil || row["tokenHash"] != nil {
			t.Fatalf("invitation list leaked token material: %v", row)
		}
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("invitation pages must advance in UUID order: %v", ids)
	}
}

// TestJoinRequestLifecycleRules covers the reusable application link: duplicate
// pending applications, idempotent replay, the member's own list, approval
// versioning and one-shot decisions, already-member rejection, rejoin of a
// disabled member through approval, and link expiry.
func TestJoinRequestLifecycleRules(t *testing.T) {
	f := setup(t)
	bob := joinUser("bob")
	token := joinToken('g')
	link := f.call("POST", f.path("/join-links"), core.Object{"token": token}, "lifecycle-link", 201)

	// A second application under a new key returns the same pending row.
	first := joinCall(t, f, bob, "POST", "/api/v1/join/requests", "apply-1", core.Object{"token": token}, 201)
	duplicate := joinCall(t, f, bob, "POST", "/api/v1/join/requests", "apply-2", core.Object{"token": token}, 201)
	if first.S("id") != duplicate.S("id") || first.S("status") != "pending" {
		t.Fatalf("duplicate application must return the pending row: %v vs %v", first, duplicate)
	}
	if f.scalar("SELECT count(*) FROM tenant_join_requests WHERE status='pending'") != 1 {
		t.Fatal("duplicate application created a second row")
	}
	// Idempotent replay returns the recorded response; a changed body conflicts.
	replay := joinCall(t, f, bob, "POST", "/api/v1/join/requests", "apply-1", core.Object{"token": token}, 201)
	if replay.S("id") != first.S("id") {
		t.Fatal("replay returned a different request")
	}
	if code := codeOf(t, joinCall(t, f, bob, "POST", "/api/v1/join/requests", "apply-1", core.Object{"token": joinToken('z')}, 409)); code != "idempotency_conflict" {
		t.Fatalf("changed replay body: want idempotency_conflict got %s", code)
	}
	joinCall(t, f, bob, "POST", "/api/v1/join/requests", strings.Repeat("k", 201), core.Object{"token": token}, 400)

	// The member sees their own applications with the tenant name.
	mine := joinCall(t, f, bob, "GET", "/api/v1/me/join-requests", "", nil, 200)
	mineItems := mine["items"].([]any)
	if len(mineItems) != 1 || core.Object(mineItems[0].(map[string]any)).S("name") == "" {
		t.Fatalf("my join requests must carry the tenant name: %v", mine)
	}

	// Decisions are versioned and one-shot.
	f.call("POST", f.path("/join-requests/"+first.S("id")+"/approve"), core.Object{}, "approve-no-version", 428)
	if code := codeOf(t, f.call("POST", f.path("/join-requests/"+first.S("id")+"/approve"), core.Object{"version": 99}, "approve-stale", 409)); code != "version_conflict" {
		t.Fatalf("stale approval: want version_conflict got %s", code)
	}
	approved := f.call("POST", f.path("/join-requests/"+first.S("id")+"/approve"), core.Object{"version": first.N("version")}, "approve-1", 200)
	if approved.S("status") != "approved" || approved["decidedAt"] == nil || approved.S("decidedBy") != f.uid {
		t.Fatalf("approval must record the decision: %v", approved)
	}
	joinCall(t, f, bob, "GET", f.path("/spaces"), "", nil, 200)
	if code := codeOf(t, f.call("POST", f.path("/join-requests/"+first.S("id")+"/approve"), core.Object{"version": approved.N("version")}, "approve-2", 409)); code != "request_already_decided" {
		t.Fatalf("re-approval: want request_already_decided got %s", code)
	}
	if code := codeOf(t, f.call("POST", f.path("/join-requests/"+first.S("id")+"/reject"), core.Object{"version": approved.N("version")}, "reject-decided", 409)); code != "request_already_decided" {
		t.Fatalf("reject after decision: want request_already_decided got %s", code)
	}

	// An active member cannot apply again through any link.
	bobID := userIDOf(t, f, bob)
	if code := codeOf(t, joinCall(t, f, bob, "POST", "/api/v1/join/requests", "apply-3", core.Object{"token": token}, 409)); code != "already_member" {
		t.Fatalf("active member application: want already_member got %s", code)
	}

	// A rejected applicant stays outside the tenant and keeps the decision.
	carol := joinUser("carol")
	carolReq := joinCall(t, f, carol, "POST", "/api/v1/join/requests", "apply-carol", core.Object{"token": token}, 201)
	f.call("POST", f.path("/join-requests/"+carolReq.S("id")+"/reject"), core.Object{"version": carolReq.N("version")}, "reject-carol", 200)
	joinCall(t, f, carol, "GET", f.path("/spaces"), "", nil, 403)
	rejected := joinCall(t, f, carol, "GET", "/api/v1/me/join-requests", "", nil, 200)
	if len(rejected["items"].([]any)) != 1 || core.Object(rejected["items"].([]any)[0].(map[string]any)).S("status") != "rejected" {
		t.Fatalf("rejected application must stay visible to its owner: %v", rejected)
	}
	// A rejected applicant may apply again; the new application is pending.
	second := joinCall(t, f, carol, "POST", "/api/v1/join/requests", "apply-carol-2", core.Object{"token": token}, 201)
	if second.S("status") != "pending" || second.S("id") == carolReq.S("id") {
		t.Fatalf("re-application after rejection: %v", second)
	}
	carolID := userIDOf(t, f, carol)

	// A disabled member rejoins through a fresh approved application.
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "member", "status": "disabled", "version": 1}, "", 200)
	joinCall(t, f, bob, "GET", f.path("/spaces"), "", nil, 403)
	rejoin := joinCall(t, f, bob, "POST", "/api/v1/join/requests", "apply-rejoin", core.Object{"token": token}, 201)
	if rejoin.S("status") != "pending" {
		t.Fatalf("disabled member must be able to re-apply: %v", rejoin)
	}
	f.call("POST", f.path("/join-requests/"+rejoin.S("id")+"/approve"), core.Object{"version": rejoin.N("version")}, "approve-rejoin", 200)
	joinCall(t, f, bob, "GET", f.path("/spaces"), "", nil, 200)

	// Cross-tenant administrators hold no authority here: a foreign admin
	// cannot list or decide this tenant's applications.
	foreign := joinUser("foreign")
	foreignTenant := callAs(t, f, foreign, "POST", "/api/v1/tenants", "foreign-tenant", core.Object{"name": "Foreign", "slug": "foreign-tenant"}, 201)
	if foreignTenant.O("tenant").S("id") == "" {
		t.Fatal("foreign tenant provisioning failed")
	}
	if code := codeOf(t, callAs(t, f, foreign, "GET", f.path("/join-requests"), "", nil, 403)); code != "membership_required" {
		t.Fatalf("foreign admin listing: want membership_required got %s", code)
	}
	callAs(t, f, foreign, "POST", f.path("/join-requests/"+second.S("id")+"/approve"), "foreign-approve", core.Object{"version": second.N("version")}, 403)

	// The administrator's list projects display names for decisions.
	adminList := f.call("GET", f.path("/join-requests"), nil, "", 200)
	named := false
	for _, item := range adminList["items"].([]any) {
		row := core.Object(item.(map[string]any))
		if row.S("userId") == carolID && row.S("displayName") == "carol" {
			named = true
		}
	}
	if !named {
		t.Fatalf("admin join-request list must project display names: %v", adminList)
	}

	// The member's own list pages across their applications.
	page := joinCall(t, f, carol, "GET", "/api/v1/me/join-requests?limit=1", "", nil, 200)
	if len(page["items"].([]any)) != 1 || page.S("nextCursor") == "" {
		t.Fatalf("my join requests must paginate: %v", page)
	}
	rest := joinCall(t, f, carol, "GET", "/api/v1/me/join-requests?limit=1&after="+page.S("nextCursor"), "", nil, 200)
	if len(rest["items"].([]any)) != 1 || rest.S("nextCursor") != "" {
		t.Fatalf("my join requests second page: %v", rest)
	}

	// An expired link accepts no further applications. The CHECK constraint
	// keeps expiry within thirty days of creation, so both columns are backdated.
	_, err := f.store.Pool.Exec("UPDATE tenant_join_links SET created_at=now()-interval '31 days', expires_at=now()-interval '30 days' WHERE id=$1", link.S("id"))
	must(t, err)
	joinCall(t, f, joinUser("erin"), "POST", "/api/v1/join/requests", "apply-expired", core.Object{"token": token}, 404)

	// Join idempotency is scoped per user: two users may reuse the same key
	// for their own independent redemptions.
	firstInvite := joinToken('r')
	secondInvite := joinToken('s')
	f.call("POST", f.path("/invitations"), core.Object{"token": firstInvite}, "scope-invite-1", 201)
	f.call("POST", f.path("/invitations"), core.Object{"token": secondInvite}, "scope-invite-2", 201)
	joinCall(t, f, joinUser("gina"), "POST", "/api/v1/join/invitations/redeem", "shared-key", core.Object{"token": firstInvite}, 200)
	joinCall(t, f, joinUser("hank"), "POST", "/api/v1/join/invitations/redeem", "shared-key", core.Object{"token": secondInvite}, 200)
	if f.scalar("SELECT count(*) FROM tenant_memberships m JOIN users u ON u.id=m.user_id JOIN user_identities i ON i.user_id=u.id WHERE i.source='github.com' AND i.subject IN ('gina','hank') AND m.status='active'") != 2 {
		t.Fatal("per-user idempotency scope lost a redemption")
	}
}

// TestJoinDeploymentModeGating verifies that public deployments expose the
// private join routes but not the corporate directory, and that corporate
// deployments hide every private join route while keeping directory search
// and directory member adds administrator-only.
func TestJoinDeploymentModeGating(t *testing.T) {
	f := setup(t)
	bob, _ := f.addUser(t, "bob", "Bob")

	// Public deployment (the fixture router has no directory): the corporate
	// directory surface is unavailable, not silently empty.
	if code := codeOf(t, f.call("GET", f.path("/people")+"?keyword=Employee", nil, "", 503)); code != "directory_unavailable" {
		t.Fatalf("public people search: want directory_unavailable got %s", code)
	}
	if code := codeOf(t, f.call("POST", f.path("/members/huawei"), core.Object{"keyword": "Employee", "globalUserId": "205045249610656", "role": "member"}, "public-hw", 503)); code != "directory_unavailable" {
		t.Fatalf("public directory add: want directory_unavailable got %s", code)
	}

	// Corporate deployment: every private join route is hidden behind 404,
	// because corporate admission must always recheck employment.
	auth, err := core.NewAuthenticator("ora-cloud", f.client.Credentials.Trust)
	must(t, err)
	server := httptest.NewServer(router.New(f.store, auth, zap.NewNop(), staticDirectory{}))
	t.Cleanup(server.Close)
	client := &simulator.Client{URL: server.URL, Credentials: f.client.Credentials, HTTP: f.client.HTTP}
	corp := func(user core.Claims, method, path, key string, body core.Object, want int) core.Object {
		t.Helper()
		out, status, e := client.Call(context.Background(), method, path, "gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}, &user, key, body)
		must(t, e)
		if status != want {
			t.Fatalf("corporate %s %s: want %d got %d %v", method, path, want, status, out)
		}
		return out
	}
	corpToken := joinToken('h')
	// An exposed route would answer join_link_unavailable for this unknown
	// token; only a hidden route answers not_found, so the fault code is what
	// actually pins the gate.
	if code := codeOf(t, corp(f.user, "POST", "/api/v1/join/invitations/redeem", "corp-redeem", core.Object{"token": corpToken}, 404)); code != "not_found" {
		t.Fatalf("corporate redeem route must be hidden, got %s", code)
	}
	if code := codeOf(t, corp(f.user, "POST", "/api/v1/join/requests", "corp-request", core.Object{"token": corpToken}, 404)); code != "not_found" {
		t.Fatalf("corporate application route must be hidden, got %s", code)
	}
	for _, method := range []string{"GET", "POST"} {
		corp(f.user, method, f.path("/invitations"), "corp-invite-"+method, core.Object{"token": corpToken}, 404)
		corp(f.user, method, f.path("/join-links"), "corp-link-"+method, core.Object{"token": corpToken}, 404)
		corp(f.user, method, f.path("/join-requests"), "corp-req-"+method, nil, 404)
	}
	// Fresh UUIDs can never match a row, so even an exposed route would answer
	// 404 for them. Seed real rows through the public fixture router (the
	// corporate server shares its store) and point the corporate calls at
	// them: an exposed route would now answer 200, not 404.
	seedInvite := f.call("POST", f.path("/invitations"), core.Object{"token": joinToken('n')}, "gate-invite", 201)
	seedLink := f.call("POST", f.path("/join-links"), core.Object{"token": joinToken('o')}, "gate-link", 201)
	seedReq := joinCall(t, f, joinUser("iris"), "POST", "/api/v1/join/requests", "gate-apply", core.Object{"token": joinToken('o')}, 201)
	corp(f.user, "DELETE", f.path("/invitations/"+seedInvite.S("id")), "corp-del", core.Object{"version": seedInvite.N("version")}, 404)
	corp(f.user, "DELETE", f.path("/join-links/"+seedLink.S("id")), "corp-del-link", core.Object{"version": seedLink.N("version")}, 404)
	corp(f.user, "POST", f.path("/join-requests/"+seedReq.S("id")+"/approve"), "corp-approve", core.Object{"version": seedReq.N("version")}, 404)
	corp(f.user, "POST", f.path("/join-requests/"+seedReq.S("id")+"/reject"), "corp-reject", core.Object{"version": seedReq.N("version")}, 404)

	// Directory search stays admin-only, then serves the employed person.
	if code := codeOf(t, corp(bob, "GET", f.path("/people")+"?keyword=Employee", "", nil, 403)); code != "admin_required" {
		t.Fatalf("corporate people search by member: want admin_required got %s", code)
	}
	if code := codeOf(t, corp(f.user, "GET", f.path("/people")+"?keyword=x", "", nil, 400)); code != "invalid_keyword" {
		t.Fatalf("short keyword: want invalid_keyword got %s", code)
	}
	found := corp(f.user, "GET", f.path("/people")+"?keyword=Employee", "", nil, 200)
	people := found["items"].([]any)
	if len(people) != 1 || core.Object(people[0].(map[string]any)).S("globalUserId") != "205045249610656" {
		t.Fatalf("corporate directory search: %v", found)
	}

	// The directory add path rechecks employment and can grant admin directly.
	added := corp(f.user, "POST", f.path("/members/huawei"), "corp-add", core.Object{"keyword": "Employee", "globalUserId": "205045249610656", "role": "admin"}, 200)
	if added.S("role") != "admin" || added.S("status") != "active" {
		t.Fatalf("directory add must honor the requested role: %v", added)
	}
	// A globalUserId absent from the verified search results never becomes a
	// member, and neither does a person the directory reports as unemployed:
	// the add path requires an employed match.
	missing := corp(f.user, "POST", f.path("/members/huawei"), "corp-missing", core.Object{"keyword": "Employee", "globalUserId": "0000000000000001", "role": "member"}, 404)
	if codeOf(t, missing) != "person_not_found" {
		t.Fatalf("unverified person: want person_not_found got %s", missing.S("code"))
	}
	gone := corp(f.user, "POST", f.path("/members/huawei"), "corp-gone", core.Object{"keyword": "Gone", "globalUserId": "1002", "role": "member"}, 404)
	if codeOf(t, gone) != "person_not_found" {
		t.Fatalf("unemployed person: want person_not_found got %s", gone.S("code"))
	}
	// Directory adds are admin-only in corporate deployments too. Re-adding an
	// already-active administrator is idempotent and never rewrites the role,
	// while a fresh person without an explicit role defaults to member.
	corp(bob, "POST", f.path("/members/huawei"), "corp-add-member", core.Object{"keyword": "Employee", "globalUserId": "205045249610656"}, 403)
	readd := corp(f.user, "POST", f.path("/members/huawei"), "corp-readd", core.Object{"keyword": "Employee", "globalUserId": "205045249610656", "role": "member"}, 200)
	if readd.S("role") != "admin" {
		t.Fatalf("directory re-add must not rewrite an active role: %v", readd)
	}
	defaulted := corp(f.user, "POST", f.path("/members/huawei"), "corp-add-default", core.Object{"keyword": "Other", "globalUserId": "1001"}, 200)
	if defaulted.S("role") != "member" || defaulted.S("displayName") != "Other Employee" {
		t.Fatalf("directory add without role must default to member: %v", defaulted)
	}
}

// TestMembershipVisibilityAcrossTenants covers the member's cross-tenant space
// list: multiple tenants, disabled memberships and archived spaces dropping
// out, soft-deleted tenants refusing redemption, and disabled accounts losing
// the self-provisioning path.
func TestMembershipVisibilityAcrossTenants(t *testing.T) {
	f := setup(t)
	dave := joinUser("dave")
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	provision := func(name, slug, key string) core.Object {
		t.Helper()
		out, status, err := f.client.Call(context.Background(), "POST", "/api/v1/tenants", "gateway", gw, &dave, key, core.Object{"name": name, "slug": slug})
		must(t, err)
		if status != 201 {
			t.Fatalf("provision %s: want 201 got %d %v", slug, status, out)
		}
		return out
	}
	first := provision("One", "visibility-one", "vis-1")
	second := provision("Two", "visibility-two", "vis-2")
	if first.O("tenant").S("id") == second.O("tenant").S("id") {
		t.Fatal("provisioning returned the same tenant twice")
	}
	// A failed provisioning never poisons the idempotency key.
	failure, status, err := f.client.Call(context.Background(), "POST", "/api/v1/tenants", "gateway", gw, &dave, "vis-3", core.Object{"name": "Bad", "slug": "Bad Slug"})
	must(t, err)
	if status != 400 {
		t.Fatalf("invalid provisioning: want 400 got %d %v", status, failure)
	}
	third := provision("Three", "visibility-three", "vis-3")
	if third.O("tenant").S("id") == "" {
		t.Fatal("key reuse after a rejected body must still provision")
	}

	// Dave joins the fixture tenant through an invitation and sees all spaces.
	token := joinToken('i')
	f.call("POST", f.path("/invitations"), core.Object{"token": token}, "vis-invite", 201)
	joined := joinCall(t, f, dave, "POST", "/api/v1/join/invitations/redeem", "vis-redeem", core.Object{"token": token}, 200)
	if joined.S("role") != "member" || joined.S("name") == "" {
		t.Fatalf("redemption must return the ordinary membership with tenant name: %v", joined)
	}
	daveID := userIDOf(t, f, dave)
	spaces := joinCall(t, f, dave, "GET", "/api/v1/me/spaces", "", nil, 200)
	if len(spaces["items"].([]any)) != 4 {
		t.Fatalf("dave must see all four joined spaces: %v", spaces)
	}

	// Disabling the fixture membership removes only that space and tenant.
	f.call("PUT", f.path("/members/"+daveID), core.Object{"role": "member", "status": "disabled", "version": 1}, "", 200)
	spaces = joinCall(t, f, dave, "GET", "/api/v1/me/spaces", "", nil, 200)
	tenants := joinCall(t, f, dave, "GET", "/api/v1/me/tenants", "", nil, 200)
	if len(spaces["items"].([]any)) != 3 || len(tenants["items"].([]any)) != 3 {
		t.Fatalf("disabled membership must leave both lists: spaces=%v tenants=%v", spaces, tenants)
	}

	// An archived space drops out of the visible list.
	_, err = f.store.Pool.Exec("UPDATE collab_workspaces SET archived_at=now() WHERE tenant_id=$1", second.O("tenant").S("id"))
	must(t, err)
	spaces = joinCall(t, f, dave, "GET", "/api/v1/me/spaces", "", nil, 200)
	if len(spaces["items"].([]any)) != 2 {
		t.Fatalf("archived space must leave the list: %v", spaces)
	}

	// A soft-deleted tenant drops out too, and its invitations no longer
	// redeem; the invitation is created by that tenant's own administrator
	// before the deletion.
	deadToken := joinToken('j')
	deadPath := "/api/v1/tenants/" + third.O("tenant").S("id") + "/invitations"
	callAs(t, f, dave, "POST", deadPath, "vis-dead-invite", core.Object{"token": deadToken}, 201)
	_, err = f.store.Pool.Exec("UPDATE tenants SET deleted_at=now() WHERE id=$1", third.O("tenant").S("id"))
	must(t, err)
	spaces = joinCall(t, f, dave, "GET", "/api/v1/me/spaces", "", nil, 200)
	items := spaces["items"].([]any)
	if len(items) != 1 || core.Object(items[0].(map[string]any)).S("slug") != "visibility-one" {
		t.Fatalf("soft-deleted tenant must leave only the live space: %v", spaces)
	}
	joinCall(t, f, joinUser("erin"), "POST", "/api/v1/join/invitations/redeem", "vis-dead-redeem", core.Object{"token": deadToken}, 404)

	// A disabled account cannot provision a tenant for itself. The account
	// owns no tenants, so the database's last-administrator guard stays green.
	frank := joinUser("frank")
	frankID := userIDOf(t, f, frank)
	_, err = f.store.Pool.Exec("UPDATE users SET status='disabled' WHERE id=$1", frankID)
	must(t, err)
	if code := codeOf(t, joinCall(t, f, frank, "POST", "/api/v1/tenants", "vis-4", core.Object{"name": "Four", "slug": "visibility-four"}, 403)); code != "user_disabled" {
		t.Fatalf("disabled provisioning: want user_disabled got %s", code)
	}
	// The same disabled account cannot redeem invitations either.
	liveToken := joinToken('m')
	f.call("POST", f.path("/invitations"), core.Object{"token": liveToken}, "vis-live-invite", 201)
	if code := codeOf(t, joinCall(t, f, frank, "POST", "/api/v1/join/invitations/redeem", "vis-frank-redeem", core.Object{"token": liveToken}, 403)); code != "user_disabled" {
		t.Fatalf("disabled redemption: want user_disabled got %s", code)
	}
}

// TestActiveMemberRedeemsAndConsumesInvitation documents that redemption by an
// already-active member neither changes roles nor frees the single-use link,
// and that membership roles survive invitation redemption untouched.
func TestActiveMemberRedeemsAndConsumesInvitation(t *testing.T) {
	f := setup(t)
	bob, bobID := f.addUser(t, "bob", "Bob")
	// Promote Bob so the assertion covers the strongest case: an invitation
	// can never downgrade (or upgrade) an existing role.
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	token := joinToken('k')
	f.call("POST", f.path("/invitations"), core.Object{"token": token}, "consume-invite", 201)

	redeemed := joinCall(t, f, bob, "POST", "/api/v1/join/invitations/redeem", "consume-redeem", core.Object{"token": token}, 200)
	if redeemed.S("role") != "admin" || redeemed.S("status") != "active" {
		t.Fatalf("redemption must not rewrite an existing role: %v", redeemed)
	}
	// The link was consumed exactly once, by the redeemer.
	if f.scalar("SELECT count(*) FROM tenant_invitations WHERE consumed_by=$1 AND consumed_at IS NOT NULL", bobID) != 1 {
		t.Fatal("redemption did not record the consumer")
	}
	// A second redemption under a fresh key is refused with the single code.
	out := joinCall(t, f, bob, "POST", "/api/v1/join/invitations/redeem", "consume-redeem-2", core.Object{"token": token}, 404)
	if codeOf(t, out) != "join_link_unavailable" {
		t.Fatalf("second redemption: want join_link_unavailable got %s", out.S("code"))
	}
	// And nobody else can use the burned link.
	joinCall(t, f, joinUser("carol"), "POST", "/api/v1/join/invitations/redeem", "consume-carol", core.Object{"token": token}, 404)
}

// TestMemberUpdateNotifiesOnlineMembers covers the space.member_updated
// broadcast for membership role and status changes committed through PUT.
func TestMemberUpdateNotifiesOnlineMembers(t *testing.T) {
	f := setup(t)
	stream := f.subscribe(t, f.user, f.fixtureSpace().S("id"), 200)
	t.Cleanup(func() { _ = stream.Body.Close() })

	_, bobID := f.addUser(t, "bob", "Bob")
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	nextEvent(t, stream, "space.member_updated")
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "member", "status": "disabled", "version": 2}, "", 200)
	nextEvent(t, stream, "space.member_updated")
}

// TestConcurrentJoinDecisionAndApplicationRaces covers two races: opposing
// decisions on one application resolve to exactly one committed outcome, and
// the same user applying concurrently never yields two pending rows.
func TestConcurrentJoinDecisionAndApplicationRaces(t *testing.T) {
	f := setup(t)
	bob, bobID := f.addUser(t, "bob", "Bob")
	f.call("PUT", f.path("/members/"+bobID), core.Object{"role": "admin", "status": "active", "version": 1}, "", 200)
	token := joinToken('l')
	f.call("POST", f.path("/join-links"), core.Object{"token": token}, "race-link", 201)

	// Two applications from the same user under different keys race.
	carol := joinUser("carol")
	gw := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}}
	ids := make(chan string, 2)
	statuses := make(chan int, 2)
	for _, key := range []string{"race-apply-1", "race-apply-2"} {
		go func(key string) {
			out, status, err := f.client.Call(context.Background(), "POST", "/api/v1/join/requests", "gateway", gw, &carol, key, core.Object{"token": token})
			if err != nil {
				statuses <- 0
				return
			}
			ids <- out.S("id")
			statuses <- status
		}(key)
	}
	for range 2 {
		if status := <-statuses; status != 201 {
			t.Fatalf("concurrent self-application: want 201 got %d", status)
		}
	}
	firstID := <-ids
	if secondID := <-ids; firstID == "" || secondID != firstID {
		t.Fatalf("concurrent self-application created divergent rows: %q vs %q", firstID, secondID)
	}
	if f.scalar("SELECT count(*) FROM tenant_join_requests WHERE status='pending'") != 1 {
		t.Fatal("concurrent self-application created two pending rows")
	}

	// Opposing decisions from two administrators: exactly one wins.
	subjects := []core.Claims{f.user, bob}
	decisions := []string{"approve", "reject"}
	results := make(chan int, 2)
	for i := range subjects {
		go func(i int) {
			_, status, callErr := f.client.Call(context.Background(), "POST", f.path("/join-requests/"+firstID+"/"+decisions[i]), "gateway", gw, &subjects[i], "race-decision-"+decisions[i], core.Object{"version": 1})
			if callErr != nil {
				results <- 0
				return
			}
			results <- status
		}(i)
	}
	outcomes := map[int]int{}
	for range 2 {
		outcomes[<-results]++
	}
	if outcomes[200] != 1 || outcomes[409] != 1 {
		t.Fatalf("opposing decisions must yield one 200 and one 409: %v", outcomes)
	}
	var final string
	must(t, f.store.Pool.QueryRow("SELECT status FROM tenant_join_requests WHERE id=$1", firstID).Scan(&final))
	if final != "approved" && final != "rejected" {
		t.Fatalf("decision race left the request undecided: %s", final)
	}
	if final == "approved" {
		joinCall(t, f, carol, "GET", f.path("/spaces"), "", nil, 200)
	} else {
		joinCall(t, f, carol, "GET", f.path("/spaces"), "", nil, 403)
	}
}

// sha256Hex mirrors the server's token digest so tests can assert exact storage.
func sha256Hex(t *testing.T, token string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(token)
	must(t, err)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// userIDOf resolves the durable user id behind a verified identity.
func userIDOf(t *testing.T, f *fixture, user core.Claims) string {
	t.Helper()
	out := callAs(t, f, user, "GET", "/api/v1/me", "", nil, 200)
	if out.S("id") == "" {
		t.Fatalf("identity %s has no user id", user.Subject)
	}
	return out.S("id")
}
