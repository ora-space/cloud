package core

// freezeInactiveRepositoryCredentials also catches deployment-side state changes before admission.
// The database triggers make membership deactivation and freezing atomic; this is a recovery scan.
func freezeInactiveRepositoryCredentials(t *transaction) {
	t.exec("UPDATE credential_refs c SET availability='frozen',unavailable_reason='associated_member_departed',version=version+1 WHERE c.attribution IN ('personal','unknown') AND c.availability='available' AND NOT EXISTS(SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=c.tenant_id AND m.user_id=c.owner_user_id AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL)")
}
