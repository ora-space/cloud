package core

import (
	"context"
	"crypto/ed25519"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// Git credential references (identity-access, Git credentials in an independent Credential Store).
// Cloud keeps only metadata: attribution, scope, availability, the Store's current version and
// which capabilities that version has proven. The token itself goes from the browser straight to
// the Credential Store under a one-time submission grant, so no Cloud path ever sees it.

// Capability names of the credential vocabulary (identity-access D4).
const (
	CapabilityRead    = "read"
	CapabilityPublish = "publish"
)

// submissionLifetime bounds a submission grant (identity-access D3: at most five minutes).
const submissionLifetime = 5 * time.Minute

// submissionSettleMargin is how long after a grant expires Cloud waits before deciding an
// unconfirmed submission was abandoned. The Store judges expiry on its own clock, so the margin
// keeps a write the Store accepted just before expiry from racing the decision.
const submissionSettleMargin = 2 * time.Minute

// GitCredentialStore is the Credential Store's management interface as Cloud consumes it. It can
// read version metadata and revoke; it can neither write nor read plaintext.
type GitCredentialStore interface {
	CredentialVersion(ctx context.Context, credentialID string) (StoredCredential, error)
	RevokeCredential(ctx context.Context, credentialID string) error
}

// StoredCredential is what the Credential Store reports about one credential: its current
// version, the submission that wrote it, and whether it was revoked.
type StoredCredential struct {
	CurrentVersion int64
	SubmissionID   string
	Revoked        bool
}

// GitCredentialSubmissions signs browser submission grants. The key is distinct from the
// execution-authorization key, so neither kind of token can stand in for the other.
type GitCredentialSubmissions struct {
	Key ed25519.PrivateKey
	// IngressURL is the Credential Store's browser origin; the grant is presented there.
	IngressURL string
}

// gitCredentialView is the public document of a reference: an allowlist, so a new column can
// never leak through it.
func gitCredentialView(c Object) Object {
	out := Object{}
	for _, k := range []string{"id", "tenantId", "name", "attribution", "ownerUserId", "repositoryScope", "secretState", "availability", "unavailableReason", "storeVersion", "capabilities", "capabilityEvidence", "requestedCapabilities", "version", "createdAt", "revokedAt"} {
		out[k] = c[k]
	}
	if c.S("pendingSubmissionId") != "" {
		out["pendingSubmission"] = Object{"submissionId": c["pendingSubmissionId"], "storeVersion": c["pendingStoreVersion"], "expiresAt": c["pendingSubmissionExpiresAt"]}
	} else {
		out["pendingSubmission"] = nil
	}
	out["lastVerification"] = c["lastVerification"]
	return out
}

const gitCredentialSelect = `SELECT c.*,(SELECT row_to_json(v) FROM (SELECT id,store_version AS "storeVersion",state,result,created_at AS "createdAt",finished_at AS "finishedAt" FROM credential_verifications
 WHERE credential_ref_id=c.id ORDER BY created_at DESC LIMIT 1) v) AS last_verification FROM credential_refs c`

func loadGitCredential(t *transaction, tid, id string) Object {
	require(validID(id), 404, "not_found")
	c := t.one(gitCredentialSelect+" WHERE c.id=$1 AND c.tenant_id=$2 AND c.deleted_at IS NULL AND c.name IS NOT NULL", id, tid)
	require(c != nil, 404, "not_found")
	return c
}

// requireCredentialManager enforces who may rotate or bind a reference: a personal one only its
// owner, a team one only a tenant admin (identity-access D5). An admin never acts with somebody
// else's personal credential.
func requireCredentialManager(t *transaction, c Object, uid string) {
	m := membership(t, c.S("tenantId"), uid, false)
	switch c.S("attribution") {
	case "personal":
		require(c.S("ownerUserId") == uid, 403, "credential_owner_required")
	case "team":
		require(m.S("role") == "admin", 403, "admin_required")
	default:
		reject(403, "credential_attribution_unknown")
	}
}

func gitCredentialsPublic(t *transaction, r *PublicRequest, uid string) (Object, int) {
	if r.Method == "GET" {
		if r.GitCredentialID != "" {
			return gitCredentialView(loadGitCredential(t, r.TenantID, r.GitCredentialID)), 200
		}
		page := window(t, gitCredentialSelect+" WHERE c.tenant_id=$1 AND c.deleted_at IS NULL AND c.name IS NOT NULL ORDER BY c.created_at DESC, c.id", []any{r.TenantID}, r)
		items := []Object{}
		for _, c := range page["items"].([]Object) {
			items = append(items, gitCredentialView(c))
		}
		page["items"] = items
		return page, 200
	}
	switch {
	case r.GitCredentialID == "" && r.Method == "POST":
		return createGitCredential(t, r, uid), 201
	case strings.HasSuffix(r.Path, "/rotate"):
		return rotateGitCredential(t, r, uid), 200
	case strings.HasSuffix(r.Path, "/verifications"):
		return requestPublishVerification(t, r, uid), 202
	case r.Method == "DELETE":
		return revokeGitCredential(t, r, uid), 200
	}
	reject(404, "not_found")
	return nil, 0
}

func createGitCredential(t *transaction, r *PublicRequest, uid string) Object {
	m := membership(t, r.TenantID, uid, false)
	name := validText(r.Body.S("name"), 200)
	attribution := r.Body.S("attribution")
	switch attribution {
	case "personal":
	case "team":
		// Team credentials are created only by an administrator; the basis records who and when.
		require(m.S("role") == "admin", 403, "admin_required")
	default:
		reject(400, "invalid_attribution")
	}
	scope, err := cloneauth.NormalizeRepository(r.Body.S("repositoryScope"))
	require(err == nil, 400, "invalid_repository_scope")
	requested := requestedCapabilities(r.Body["requestedCapabilities"])
	id := newID()
	basis := ""
	if attribution == "team" {
		basis = "created by tenant admin " + uid
	}
	t.exec(`INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,name,attribution,attribution_basis,repository_scope,secret_state,requested_capabilities,revoked_at)
	 VALUES($1,$2,$3,'git',$4,$5,NULLIF($6,''),$7,'awaiting_secret',$8,NULL)`, id, r.TenantID, uid, name, attribution, basis, scope, jsonText(requested))
	submission := issueSubmission(t, id)
	return Object{"resource": gitCredentialView(loadGitCredential(t, r.TenantID, id)), "submission": submission}
}

func requestedCapabilities(v any) []string {
	out := []string{}
	if v == nil {
		return out
	}
	list, ok := v.([]any)
	require(ok, 400, "invalid_capabilities")
	seen := map[string]bool{}
	for _, item := range list {
		name, _ := item.(string)
		require(name == CapabilityRead || name == CapabilityPublish, 400, "invalid_capabilities")
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

func rotateGitCredential(t *transaction, r *PublicRequest, uid string) Object {
	c := loadGitCredential(t, r.TenantID, r.GitCredentialID)
	requireCredentialManager(t, c, uid)
	version(c, r.Body.N("version"))
	require(c.S("secretState") != "revoked", 409, "credential_revoked")
	submission := issueSubmission(t, c.S("id"))
	return Object{"resource": gitCredentialView(loadGitCredential(t, r.TenantID, c.S("id"))), "submission": submission}
}

// issueSubmission records the pending submission for the next Store version and signs its grant.
// A still-valid pending submission is re-signed rather than replaced: one version has one grant,
// so two grants can never race to write the same version.
func issueSubmission(t *transaction, credentialID string) Object {
	keys := t.store.GitSubmissions
	require(keys != nil && len(keys.Key) == ed25519.PrivateKeySize && keys.IngressURL != "", 503, "credential_store_unconfigured")
	now := t.one("SELECT clock_timestamp() AS now")
	at, _ := time.Parse(time.RFC3339Nano, now.S("now"))
	c := t.one("SELECT * FROM credential_refs WHERE id=$1", credentialID)
	expires, _ := time.Parse(time.RFC3339Nano, c.S("pendingSubmissionExpiresAt"))
	if c.S("pendingSubmissionId") == "" || !at.Before(expires) {
		expires = at.Add(submissionLifetime)
		t.exec("UPDATE credential_refs SET pending_submission_id=$2,pending_store_version=store_version+1,pending_submission_expires_at=$3,version=version+1 WHERE id=$1", credentialID, newID(), expires)
		c = t.one("SELECT * FROM credential_refs WHERE id=$1", credentialID)
		expires, _ = time.Parse(time.RFC3339Nano, c.S("pendingSubmissionExpiresAt"))
	}
	grant := cloneauth.Submission{
		SubmissionID: c.S("pendingSubmissionId"), Purpose: cloneauth.SubmissionPurpose, TenantID: c.S("tenantId"), CredentialID: credentialID,
		OwnerUserID: c.S("ownerUserId"), Attribution: c.S("attribution"), RepositoryScope: c.S("repositoryScope"),
		ExpectedStoreVersion: c.N("pendingStoreVersion"), IssuedAt: expires.Add(-submissionLifetime).UTC(), ExpiresAt: expires.UTC(),
	}
	token, err := cloneauth.SignSubmission(keys.Key, grant)
	if err != nil {
		panic(databaseFailure{err})
	}
	ingress := strings.TrimRight(keys.IngressURL, "/") + "/v1/credentials/" + credentialID + "/versions"
	return Object{"submissionId": grant.SubmissionID, "grant": token, "ingressUrl": ingress, "storeVersion": grant.ExpectedStoreVersion, "expiresAt": grant.ExpiresAt.Format(time.RFC3339Nano)}
}

func revokeGitCredential(t *transaction, r *PublicRequest, uid string) Object {
	c := loadGitCredential(t, r.TenantID, r.GitCredentialID)
	m := membership(t, r.TenantID, uid, false)
	// Revocation tightens access, so an administrator may revoke even another member's personal
	// credential; the owner may always revoke their own.
	require(m.S("role") == "admin" || c.S("attribution") == "personal" && c.S("ownerUserId") == uid, 403, "credential_owner_required")
	version(c, r.Body.N("version"))
	if c.S("secretState") != "revoked" {
		// Cloud stops issuing authorizations first; the Store revocation follows and is retried
		// until it succeeds, so the window in which an issued authorization can still be redeemed
		// is bounded by that authorization's lifetime.
		t.exec(`UPDATE credential_refs SET secret_state='revoked',revoked_at=clock_timestamp(),store_revocation_pending=true,pending_submission_id=NULL,pending_store_version=NULL,pending_submission_expires_at=NULL,
		 capabilities='[]'::jsonb,version=version+1 WHERE id=$1`, c.S("id"))
	}
	return Object{"resource": gitCredentialView(loadGitCredential(t, r.TenantID, c.S("id")))}
}

func requestPublishVerification(t *transaction, r *PublicRequest, uid string) Object {
	c := loadGitCredential(t, r.TenantID, r.GitCredentialID)
	requireCredentialManager(t, c, uid)
	require(c.S("secretState") == "stored", 409, "credential_not_in_store")
	return Object{"resource": credentialVerificationView(ensurePublishVerification(t, c, uid))}
}

// ensurePublishVerification returns the unfinished push-capability check of the reference's
// current version, creating one when there is none.
func ensurePublishVerification(t *transaction, c Object, uid string) Object {
	if v := t.one("SELECT * FROM credential_verifications WHERE credential_ref_id=$1 AND store_version=$2 AND state IN ('pending','leased','running')", c.S("id"), c.N("storeVersion")); v != nil {
		return v
	}
	id := newID()
	var actor any
	if uid != "" {
		actor = uid
	}
	t.exec("INSERT INTO credential_verifications(id,tenant_id,credential_ref_id,store_version,repository,requested_by,state) VALUES($1,$2,$3,$4,$5,$6,'pending')", id, c.S("tenantId"), c.S("id"), c.N("storeVersion"), c.S("repositoryScope"), actor)
	return t.one("SELECT * FROM credential_verifications WHERE id=$1", id)
}

// credentialProblem reports why a reference cannot back new remote work against repositoryURL
// with the given capability ("" for a sync, which proves read rather than requiring it), or ""
// when it can. It is evaluated when the work is created and again when its authorization is
// issued (sync ADR D7, publish ADR D1).
func credentialProblem(t *transaction, tenantID, credentialID, repositoryURL, capability string) string {
	c := t.one("SELECT * FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", credentialID, tenantID)
	switch {
	case c == nil || c.S("secretState") == "revoked":
		return "repository_credential_unavailable"
	case c.S("secretState") != "stored":
		return "credential_not_in_store"
	case c.S("availability") == "frozen":
		return "repository_credential_frozen"
	case c.S("availability") == "invalid":
		return "repository_credential_invalid"
	}
	if c.S("attribution") != "team" && t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", tenantID, c.S("ownerUserId")) == nil {
		return "repository_credential_frozen"
	}
	scope, err := cloneauth.NormalizeRepository(repositoryURL)
	if err != nil || scope != c.S("repositoryScope") {
		return "repository_credential_scope_mismatch"
	}
	if capability != "" && !hasCapability(c, capability) {
		return "credential_publish_unverified"
	}
	return ""
}

func hasCapability(c Object, capability string) bool {
	list, _ := c["capabilities"].([]any)
	for _, v := range list {
		if v == capability {
			return true
		}
	}
	return false
}

// recordCapability marks capability proven for the reference's version, in the transaction that
// settles the evidence. Evidence for an older version is ignored: a new token has to prove itself.
func recordCapability(t *transaction, credentialID string, storeVersion int64, capability, source string) {
	t.exec(`UPDATE credential_refs SET capabilities=CASE WHEN capabilities ? $3 THEN capabilities ELSE capabilities||jsonb_build_array($3::text) END,
	 capability_evidence=capability_evidence||jsonb_build_object($3::text,jsonb_build_object('at',clock_timestamp(),'storeVersion',$2::bigint,'source',$4::text)),version=version+1
	 WHERE id=$1 AND store_version=$2 AND secret_state='stored'`, credentialID, storeVersion, capability, source)
}

// dropCapability removes a capability the external platform has just disproven for the version.
func dropCapability(t *transaction, credentialID string, storeVersion int64, capability string) {
	t.exec(`UPDATE credential_refs SET capabilities=capabilities-$3::text,capability_evidence=capability_evidence-$3::text,version=version+1
	 WHERE id=$1 AND store_version=$2 AND capabilities ? $3`, credentialID, storeVersion, capability)
}

// markCredentialInvalid records that the external repository rejected the version's token. A
// new submission makes the reference available again; a frozen one stays frozen.
func markCredentialInvalid(t *transaction, credentialID string, storeVersion int64) {
	t.exec(`UPDATE credential_refs SET availability='invalid',unavailable_reason='credential_rejected',capabilities='[]'::jsonb,version=version+1
	 WHERE id=$1 AND store_version=$2 AND availability='available'`, credentialID, storeVersion)
}

func credentialVerificationView(v Object) Object {
	out := Object{}
	for _, k := range []string{"id", "storeVersion", "state", "result", "createdAt", "finishedAt"} {
		out[k] = v[k]
	}
	return out
}
