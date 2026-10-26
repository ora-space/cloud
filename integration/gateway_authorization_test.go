package integration

import (
	"net/http"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gateway"
)

// Evidence for specs/test-cases/cloud/identity-access/proxy-and-credentials.md
// #cloud-authorization-decisions-pass-through-unchanged. External authentication establishes an
// identity; every tenant request still consults Cloud's current membership, on every replica.
func TestGatewayTenantAuthorizationUsesCurrentMembership(t *testing.T) {
	f := setupGateway(t)
	first := f.newGateway("", 100)
	second := f.newGateway("", 100)
	owner := first.browser()
	first.login(owner, "/")
	if resp, me := first.do(owner, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusOK || me.S("id") == "" {
		t.Fatalf("tenant creator must have a resolved Cloud identity: %d %v", resp.StatusCode, me)
	}
	createHeaders := first.origin()
	createHeaders["Idempotency-Key"] = "gateway-tenant-create"
	resp, created := first.do(owner, http.MethodPost, "/api/v1/tenants", core.Object{"name": "Gateway team", "slug": "gateway-team"}, createHeaders)
	tenant := created.O("tenant")
	if resp.StatusCode != http.StatusCreated || tenant.S("id") == "" || tenant.S("role") != "admin" || tenant.S("status") != "active" {
		t.Fatalf("tenant creator must become its active administrator: %d %v", resp.StatusCode, created)
	}
	base := "/api/v1/tenants/" + tenant.S("id")
	if resp, spaces := first.do(owner, http.MethodGet, base+"/spaces", nil, nil); resp.StatusCode != http.StatusOK || len(spaces["items"].([]any)) != 1 {
		t.Fatalf("creator must access the tenant's first space: %d %v", resp.StatusCode, spaces)
	}

	// A separate verified external identity gets a session but no tenant membership.
	f.provider.mu.Lock()
	f.provider.userID, f.provider.userName, f.provider.userLogin = 71996634, "Gateway Member", "gateway-member"
	f.provider.mu.Unlock()
	member := first.browser()
	first.login(member, "/")
	resp, me := first.do(member, http.MethodGet, "/api/v1/me", nil, nil)
	if resp.StatusCode != http.StatusOK || me.S("id") == "" {
		t.Fatalf("external login must resolve the prospective member: %d %v", resp.StatusCode, me)
	}
	memberID := me.S("id")
	for _, replica := range []*gatewayInstance{first, second} {
		if resp, out := replica.do(member, http.MethodGet, base+"/spaces", nil, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "membership_required" {
			t.Fatalf("external login must not grant tenant access on %s: %d %v", replica.server.URL, resp.StatusCode, out)
		}
	}
	if count := f.count("SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", tenant.S("id"), memberID); count != 0 {
		t.Fatalf("external authentication created %d memberships", count)
	}

	// Membership is granted through Cloud's invitation flow, using the existing browser session.
	inviteToken, err := gateway.NewSecret()
	must(t, err)
	inviteHeaders := first.origin()
	inviteHeaders["Idempotency-Key"] = "gateway-invitation-create"
	if resp, out := first.do(owner, http.MethodPost, base+"/invitations", core.Object{"token": inviteToken}, inviteHeaders); resp.StatusCode != http.StatusCreated {
		t.Fatalf("creator must be allowed to invite a member: %d %v", resp.StatusCode, out)
	}
	redeemHeaders := first.origin()
	redeemHeaders["Idempotency-Key"] = "gateway-invitation-redeem"
	resp, joined := first.do(member, http.MethodPost, "/api/v1/join/invitations/redeem", core.Object{"token": inviteToken}, redeemHeaders)
	if resp.StatusCode != http.StatusOK || joined.S("role") != "member" || joined.S("status") != "active" {
		t.Fatalf("invitation must grant only an active ordinary membership: %d %v", resp.StatusCode, joined)
	}
	for _, replica := range []*gatewayInstance{first, second} {
		if resp, out := replica.do(member, http.MethodGet, base+"/spaces", nil, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("current membership must allow access on %s: %d %v", replica.server.URL, resp.StatusCode, out)
		}
	}
	if resp, out := first.do(member, http.MethodPut, base+"/members/"+memberID, core.Object{"role": "admin", "status": "active", "version": joined.N("version")}, first.origin()); resp.StatusCode != http.StatusForbidden || out.S("code") != "admin_required" {
		t.Fatalf("external identity must not promote itself: %d %v", resp.StatusCode, out)
	}
	if resp, out := first.do(owner, http.MethodPut, base+"/members/"+memberID, core.Object{"role": "member", "status": "disabled", "version": joined.N("version")}, first.origin()); resp.StatusCode != http.StatusOK || out.S("status") != "disabled" {
		t.Fatalf("administrator must be able to disable the membership: %d %v", resp.StatusCode, out)
	}

	// Revoking membership takes effect immediately without revoking or refreshing the session.
	for _, replica := range []*gatewayInstance{first, second} {
		if resp, out := replica.do(member, http.MethodGet, "/api/v1/me", nil, nil); resp.StatusCode != http.StatusOK || out.S("id") != memberID {
			t.Fatalf("disabled membership must preserve the authenticated identity on %s: %d %v", replica.server.URL, resp.StatusCode, out)
		}
		if resp, out := replica.do(member, http.MethodGet, base+"/spaces", nil, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "membership_required" {
			t.Fatalf("disabled membership must immediately deny access on %s: %d %v", replica.server.URL, resp.StatusCode, out)
		}
		for _, path := range []string{"/api/v1/me/tenants", "/api/v1/me/spaces"} {
			if resp, out := replica.do(member, http.MethodGet, path, nil, nil); resp.StatusCode != http.StatusOK || len(out["items"].([]any)) != 0 {
				t.Fatalf("disabled membership must disappear from %s on %s: %d %v", path, replica.server.URL, resp.StatusCode, out)
			}
		}
	}
	if count := f.count("SELECT count(*) FROM gateway_sessions WHERE revoked_at IS NULL AND expires_at>clock_timestamp()"); count != 2 {
		t.Fatalf("membership changes must preserve both live browser sessions, got %d", count)
	}
	relogin := first.browser()
	first.login(relogin, "/")
	if resp, out := first.do(relogin, http.MethodGet, base+"/spaces", nil, nil); resp.StatusCode != http.StatusForbidden || out.S("code") != "membership_required" {
		t.Fatalf("another external login must not reactivate disabled membership: %d %v", resp.StatusCode, out)
	}
}
