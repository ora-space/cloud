package core

// requireRepositoryAccess authorizes only new remote Git work. Finishing accepted executions,
// querying results, local code access, ordinary restart and cleanup do not call this guard.
// An available reference alone cannot enable a missing scoped credential provider.
//
// The guard is total: a Project that names a credential is refused on every current execution path,
// so this function is the single place a credentialed clone could ever be admitted, and it must stay
// a refusal until a Node-side authentication channel exists (D6).
func requireRepositoryAccess(t *transaction, pid string) {
	p := t.one("SELECT * FROM projects WHERE id=$1", pid)
	require(p != nil, 404, "not_found")
	// D7: `projects.credential_ref_id` is the one authoritative binding. The 0023 column
	// `repository_credential_ref_id` has no writer on any current path and is never read to *select* a
	// credential here: preferring it, or falling back to it, would let a stale row authorize work with
	// a credential the product never bound.
	//
	// It is still inspected, because a non-empty value is evidence that the row was written to mean
	// "this project needs a credential". Treating such a project as anonymous would send an
	// uncredentialed clone of a repository the operator bound a credential to, which is the same
	// failure the fallback caused, only quieter. So the two columns must agree about which credential
	// this project uses — including agreeing that there is none — and any disagreement is refused
	// rather than resolved (D10). Silently picking either value would execute a clone against a
	// credential nobody chose.
	id, legacy := p.S("credentialRefId"), p.S("repositoryCredentialRefId")
	if legacy != "" && legacy != id {
		reject(409, "repository_credential_binding_conflict")
	}
	if id == "" {
		return
	} // Anonymous public Git is the existing real execution path.
	c := t.one("SELECT * FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", id, p.S("tenantId"))
	require(c != nil, 409, "repository_credential_unavailable")
	require(c.S("availability") == "available", 409, "repository_credential_frozen")
	if c.S("attribution") != "team" {
		require(t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", c.S("tenantId"), c.S("ownerUserId")) != nil, 409, "repository_credential_frozen")
	}
	// The scope is compared in the normalized form a product credential stores it in, so the two
	// spellings of one repository that differ only by case, a trailing `.git` or a trailing slash match
	// while two different repositories never do. `projects.repository_url` keeps whatever the caller
	// submitted — it is durable state this change must not reinterpret — so the comparison normalizes
	// it here, per request. A URL that has no normalized form at all, such as an ssh remote, cannot
	// match any scope and is refused as a mismatch rather than waved through.
	//
	// An empty scope is a reference written before this binding existed, by the deployment-only
	// `ConfigureCredential` path. It is left unscoped for compatibility; the product path always writes
	// a scope, so no user-managed credential can be in that state.
	if scope := c.S("repositoryScope"); scope != "" {
		normalized, ok := normalizeRepositoryScope(p.S("repositoryUrl"))
		require(ok && normalized == scope, 409, "repository_credential_scope_mismatch")
	}
	// A credentialed clone is executed by Cloud itself, through the credentialed-clone executor, and
	// only by a deployment that installed one. Without an executor the refusal stands unchanged:
	// never substitute a global Git/SSH credential, and never issue an anonymous clone for a
	// credentialed project.
	if t.store.credentialedCloner == nil {
		reject(409, "repository_credential_executor_unavailable")
	}
	// The admission is recorded rather than returned, because the caller that acts on it runs outside
	// this transaction: a clone takes minutes and the execution contract forbids holding the control
	// plane's lock across it.
	t.admittedCredential = &admittedCredential{
		CredentialID:      id,
		CredentialVersion: c.N("version"),
		OwnerUserID:       c.S("ownerUserId"),
		Repository:        c.S("repositoryScope"),
	}
}

// admittedCredential is the credential a credentialed clone was admitted with.
//
// It is deliberately not the credential itself: nothing here can be used to obtain a token, and the
// scope is the one admission already compared against the Project's repository. It exists so the
// dispatch can name what was authorized without re-deriving it from a second read that could disagree.
type admittedCredential struct {
	CredentialID      string
	CredentialVersion int64
	OwnerUserID       string
	Repository        string
}

// freezeInactiveRepositoryCredentials also catches deployment-side state changes before admission.
// The database triggers make membership deactivation and freezing atomic; this is a recovery scan.
func freezeInactiveRepositoryCredentials(t *transaction) {
	t.exec("UPDATE credential_refs c SET availability='frozen',unavailable_reason='associated_member_departed',version=version+1 WHERE c.attribution IN ('personal','unknown') AND c.availability='available' AND NOT EXISTS(SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=c.tenant_id AND m.user_id=c.owner_user_id AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL)")
}
