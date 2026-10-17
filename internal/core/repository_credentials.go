package core

// requireRepositoryAccess authorizes only new remote Git work. Finishing accepted executions,
// querying results, local code access, ordinary restart and cleanup do not call this guard.
// An available reference alone cannot enable a missing scoped credential provider.
func requireRepositoryAccess(t *transaction, pid string) {
	p := t.one("SELECT * FROM "+projectView+" p WHERE id=$1", pid)
	require(p != nil, 404, "not_found")
	id := p.S("credentialRefId")
	if id == "" {
		return
	} // Anonymous public Git is the existing real execution path.
	c := t.one("SELECT * FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", id, p.S("tenantId"))
	require(c != nil, 409, "repository_credential_unavailable")
	require(c.S("availability") == "available", 409, "repository_credential_frozen")
	if c.S("attribution") != "team" {
		require(t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", c.S("tenantId"), c.S("ownerUserId")) != nil, 409, "repository_credential_frozen")
	}
	require(c.S("repositoryScope") == "" || c.S("repositoryScope") == p.S("repositoryUrl"), 409, "repository_credential_scope_mismatch")
	// No production Controller/Node resolves project references yet. Refuse explicitly; never
	// substitute a global Git/SSH credential or issue an anonymous clone for a credentialed project.
	reject(409, "repository_credential_executor_unavailable")
}

// freezeInactiveRepositoryCredentials also catches deployment-side state changes before admission.
// The database triggers make membership deactivation and freezing atomic; this is a recovery scan.
func freezeInactiveRepositoryCredentials(t *transaction) {
	t.exec("UPDATE credential_refs c SET availability='frozen',unavailable_reason='associated_member_departed',version=version+1 WHERE c.attribution IN ('personal','unknown') AND c.availability='available' AND NOT EXISTS(SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=c.tenant_id AND m.user_id=c.owner_user_id AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL)")
}
