package core

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Join tokens are generated with 32 random browser bytes, sent only in a
// request body, and stored solely as SHA-256 digests. The creator builds the
// share URL locally, so idempotency records never contain the raw token.
func joinTokenDigest(token string) []byte {
	require(len(token) == 43, 400, "invalid_join_token")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	require(err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == token, 400, "invalid_join_token")
	sum := sha256.Sum256(raw)
	return sum[:]
}

// joinBeforeMembership distinguishes a fresh redemption from an idempotent
// replay so only the former invalidates online members after commit.
func joinBeforeMembership(t *transaction, r *PublicRequest, uid string) (out Object, status int, publishMemberEvent bool) {
	if r.Path == "/api/v1/me/join-requests" {
		return page(t, `SELECT j.id,j.tenant_id,j.user_id,j.link_id,j.status,j.created_at,j.decided_at,j.decided_by,j.version,tn.name
FROM tenant_join_requests j JOIN tenants tn ON tn.id=j.tenant_id WHERE j.user_id=$1`, []any{uid}, "j.id", r), 200, false
	}
	require(r.Key != "" && len(r.Key) <= 200, 400, "idempotency_key_required")
	hash := requestHash(r.Method, r.Path, r.Body)
	if old := t.one("SELECT * FROM join_idempotency_records WHERE user_id=$1 AND key=$2", uid, r.Key); old != nil {
		require(old.S("requestHash") == hash, 409, "idempotency_conflict")
		return old.O("response"), int(old.N("status")), false
	}
	switch r.Path {
	case "/api/v1/join/invitations/redeem":
		out, status = redeemInvitation(t, r.Body.S("token"), uid), 200
		publishMemberEvent = true
	case "/api/v1/join/requests":
		out, status = requestJoin(t, r.Body.S("token"), uid), 201
	default:
		reject(404, "not_found")
	}
	t.exec("INSERT INTO join_idempotency_records(user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5)", uid, r.Key, hash, jsonText(out), status)
	return out, status, publishMemberEvent
}

func redeemInvitation(t *transaction, token, uid string) Object {
	invite := t.one(`SELECT i.id,i.tenant_id FROM tenant_invitations i JOIN tenants tn ON tn.id=i.tenant_id
WHERE i.token_hash=$1 AND i.revoked_at IS NULL AND i.consumed_at IS NULL
AND i.expires_at>clock_timestamp() AND tn.status='active' AND tn.deleted_at IS NULL`, joinTokenDigest(token))
	require(invite != nil, 404, "join_link_unavailable")
	addMembership(t, invite.S("tenantId"), uid, "member")
	t.exec("UPDATE tenant_invitations SET consumed_by=$2,consumed_at=clock_timestamp(),version=version+1 WHERE id=$1", invite.S("id"), uid)
	return t.one("SELECT m.tenant_id,m.user_id,m.role,m.status,m.version,tn.name FROM tenant_memberships m JOIN tenants tn ON tn.id=m.tenant_id WHERE m.tenant_id=$1 AND m.user_id=$2", invite.S("tenantId"), uid)
}

func requestJoin(t *transaction, token, uid string) Object {
	link := t.one(`SELECT l.id,l.tenant_id FROM tenant_join_links l JOIN tenants tn ON tn.id=l.tenant_id
WHERE l.token_hash=$1 AND l.revoked_at IS NULL AND l.expires_at>clock_timestamp()
AND tn.status='active' AND tn.deleted_at IS NULL`, joinTokenDigest(token))
	require(link != nil, 404, "join_link_unavailable")
	require(t.one("SELECT user_id FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND status='active'", link.S("tenantId"), uid) == nil, 409, "already_member")
	if old := t.one("SELECT * FROM tenant_join_requests WHERE tenant_id=$1 AND user_id=$2 AND status='pending'", link.S("tenantId"), uid); old != nil {
		return old
	}
	id := newID()
	t.exec("INSERT INTO tenant_join_requests(id,tenant_id,user_id,link_id,status) VALUES($1,$2,$3,$4,'pending')", id, link.S("tenantId"), uid, link.S("id"))
	return t.one("SELECT * FROM tenant_join_requests WHERE id=$1", id)
}

// addMembership preserves durable references by reactivating an existing row
// instead of deleting or replacing it. Caller authorization is checked above.
func addMembership(t *transaction, tid, uid, role string) Object {
	old := t.one("SELECT * FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", tid, uid)
	if old == nil {
		t.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,$3,'active')", tid, uid, role)
	} else if old.S("status") == "disabled" {
		t.exec("UPDATE tenant_memberships SET role=$3,status='active',version=version+1 WHERE tenant_id=$1 AND user_id=$2", tid, uid, role)
	}
	return t.one("SELECT * FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2", tid, uid)
}

func adminJoinRead(t *transaction, r *PublicRequest) Object {
	switch {
	case strings.HasSuffix(r.Path, "/invitations"):
		return page(t, "SELECT id,tenant_id,created_by,created_at,expires_at,revoked_at,consumed_by,consumed_at,version FROM tenant_invitations WHERE tenant_id=$1", []any{r.TenantID}, "id", r)
	case strings.HasSuffix(r.Path, "/join-links"):
		return page(t, "SELECT id,tenant_id,created_by,created_at,expires_at,revoked_at,version FROM tenant_join_links WHERE tenant_id=$1", []any{r.TenantID}, "id", r)
	default:
		return page(t, "SELECT j.id,j.tenant_id,j.user_id,j.link_id,j.status,j.created_at,j.decided_at,j.decided_by,j.version,u.display_name FROM tenant_join_requests j JOIN users u ON u.id=j.user_id WHERE j.tenant_id=$1", []any{r.TenantID}, "j.id", r)
	}
}

func adminJoinWrite(t *transaction, r *PublicRequest, uid string) (out Object, status int) {
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.Path, "/invitations"):
		digest := joinTokenDigest(r.Body.S("token"))
		// Token digests are globally unique per table. A deliberately reused
		// token is a client conflict, never an internal constraint failure.
		require(t.one("SELECT id FROM tenant_invitations WHERE token_hash=$1", digest) == nil, 409, "join_token_conflict")
		id := newID()
		t.exec("INSERT INTO tenant_invitations(id,tenant_id,token_hash,created_by,expires_at) VALUES($1,$2,$3,$4,now()+interval '7 days')", id, r.TenantID, digest, uid)
		return t.one("SELECT id,tenant_id,created_by,created_at,expires_at,version FROM tenant_invitations WHERE id=$1", id), 201
	case r.Method == "POST" && strings.HasSuffix(r.Path, "/join-links"):
		digest := joinTokenDigest(r.Body.S("token"))
		require(t.one("SELECT id FROM tenant_join_links WHERE token_hash=$1", digest) == nil, 409, "join_token_conflict")
		id := newID()
		t.exec("INSERT INTO tenant_join_links(id,tenant_id,token_hash,created_by,expires_at) VALUES($1,$2,$3,$4,now()+interval '30 days')", id, r.TenantID, digest, uid)
		return t.one("SELECT id,tenant_id,created_by,created_at,expires_at,version FROM tenant_join_links WHERE id=$1", id), 201
	case r.Method == "DELETE" && r.InvitationID != "":
		row := t.one("SELECT * FROM tenant_invitations WHERE id=$1 AND tenant_id=$2", r.InvitationID, r.TenantID)
		require(row != nil, 404, "not_found")
		version(row, r.Body.N("version"))
		t.exec("UPDATE tenant_invitations SET revoked_at=clock_timestamp(),version=version+1 WHERE id=$1 AND revoked_at IS NULL", r.InvitationID)
		return t.one("SELECT id,tenant_id,created_by,created_at,expires_at,revoked_at,consumed_by,consumed_at,version FROM tenant_invitations WHERE id=$1", r.InvitationID), 200
	case r.Method == "DELETE" && r.JoinLinkID != "":
		row := t.one("SELECT * FROM tenant_join_links WHERE id=$1 AND tenant_id=$2", r.JoinLinkID, r.TenantID)
		require(row != nil, 404, "not_found")
		version(row, r.Body.N("version"))
		t.exec("UPDATE tenant_join_links SET revoked_at=clock_timestamp(),version=version+1 WHERE id=$1 AND revoked_at IS NULL", r.JoinLinkID)
		return t.one("SELECT id,tenant_id,created_by,created_at,expires_at,revoked_at,version FROM tenant_join_links WHERE id=$1", r.JoinLinkID), 200
	case r.Method == "POST" && r.JoinRequestID != "":
		row := t.one("SELECT * FROM tenant_join_requests WHERE id=$1 AND tenant_id=$2", r.JoinRequestID, r.TenantID)
		require(row != nil, 404, "not_found")
		version(row, r.Body.N("version"))
		require(row.S("status") == "pending", 409, "request_already_decided")
		decision := "rejected"
		if strings.HasSuffix(r.Path, "/approve") {
			addMembership(t, r.TenantID, row.S("userId"), "member")
			decision = "approved"
		}
		t.exec("UPDATE tenant_join_requests SET status=$2,decided_at=clock_timestamp(),decided_by=$3,version=version+1 WHERE id=$1", r.JoinRequestID, decision, uid)
		return t.one("SELECT * FROM tenant_join_requests WHERE id=$1", r.JoinRequestID), 200
	}
	reject(404, "not_found")
	return nil, 0
}
