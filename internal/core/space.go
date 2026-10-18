package core

import "regexp"

// Collab workspaces are the visible identity of a tenant. They are distinct
// from runtime workspaces, which model execution environments. Tenant
// membership is the single authorization source.

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validSlug rejects uppercase, empty, and malformed workspace slugs.
func validSlug(s string) bool { return slugPattern.MatchString(s) }

// spaceMember verifies the user has an active membership in the space's
// tenant. It returns that membership row for role checks.
func spaceMember(t *transaction, spaceID, uid string) Object {
	require(validID(spaceID) && validID(uid), 404, "not_found")
	m := t.one(`SELECT tm.* FROM collab_workspaces w
JOIN tenant_memberships tm ON tm.tenant_id=w.tenant_id AND tm.user_id=$2
JOIN users u ON u.id=tm.user_id
JOIN tenants tn ON tn.id=w.tenant_id
WHERE w.id=$1
AND w.archived_at IS NULL
AND tm.status='active' AND u.status='active' AND u.deleted_at IS NULL
AND tn.status='active' AND tn.deleted_at IS NULL`, spaceID, uid)
	require(m != nil, 404, "not_found")
	return m
}

// requireSpaceRole verifies a member carries at least one of the given roles.
func requireSpaceRole(m Object, roles ...string) {
	for _, role := range roles {
		if m.S("role") == role {
			return
		}
	}
	reject(403, "space_role_required")
}

// soleSpace returns the collaboration space owned by the tenant.
func soleSpace(t *transaction, tid string) Object {
	w := t.one("SELECT * FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL", tid)
	require(w != nil, 404, "not_found")
	return w
}

// listSpaces returns the tenant's sole space to its active members.
func listSpaces(t *transaction, r *PublicRequest, uid string) Object {
	return page(t, "SELECT w.*, tm.role FROM collab_workspaces w JOIN tenant_memberships tm ON tm.tenant_id=w.tenant_id WHERE w.tenant_id=$1 AND tm.user_id=$2 AND tm.status='active' AND w.archived_at IS NULL", []any{r.TenantID, uid}, "w.id", r)
}

// patchSpace updates tenant and space names together; slug is immutable.
func patchSpace(t *transaction, r *PublicRequest, uid string) Object {
	w := t.one("SELECT * FROM collab_workspaces WHERE id=$1 AND tenant_id=$2", r.SpaceID, r.TenantID)
	require(w != nil, 404, "not_found")
	m := spaceMember(t, r.SpaceID, uid)
	requireSpaceRole(m, "admin")
	version(w, r.Body.N("version"))
	name := validText(r.Body.S("name"), 128)
	description := r.Body.S("description")
	require(len(description) <= 2000, 400, "invalid_input")
	t.exec("UPDATE collab_workspaces SET name=$2,description=$3,version=version+1,updated_at=now() WHERE id=$1", r.SpaceID, name, description)
	t.exec("UPDATE tenants SET name=$2,version=version+1 WHERE id=$1", r.TenantID, name)
	return t.one("SELECT * FROM collab_workspaces WHERE id=$1", r.SpaceID)
}

// projectInSpace loads a live project in the tenant and verifies the caller's
// space membership, returning both rows. Entity routes derive the space from
// the project itself; client-supplied space identifiers are never trusted.
func projectInSpace(t *transaction, tid, uid, pid string) (project, membership Object) {
	require(validID(pid), 404, "not_found")
	p := t.one("SELECT * FROM "+projectView+" p WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", pid, tid)
	require(p != nil, 404, "not_found")
	return p, spaceMember(t, p.S("spaceId"), uid)
}
