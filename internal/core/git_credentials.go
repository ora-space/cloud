package core

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// A user's personal HTTPS Git credential (identity-access
// 20261010-user-managed-git-credentials, and the store boundary in
// 20261010-user-managed-git-secret-store).
//
// `credential_refs` is the only business record (credentials D1). A product credential is a row
// attributed by `tenant_id` + `owner_user_id` with `purpose='git'`, `attribution='personal'` written
// explicitly rather than inherited from the column's `unknown` default (D2), exactly one
// `repository_scope`, and the capability set the create validated. The row never holds the token. What
// it holds is an opaque reference the independent Credential Service generated, beside the metadata
// Cloud is authoritative for: attribution, availability, scope, capabilities and a version.
//
// The capability set is the one field here that exists for a write. It is optional on the create — a
// request that names none gets `clone`, which is what every credential carried before the push
// decision existed — and a request that names `push` gets a reference the push admission will accept,
// because nothing else can produce one: the vocabulary's members are refused everywhere they are not
// served, and the product had no path that wrote a second member until this one. Declaring it grants
// no write by itself. A push still needs its own single-use approval, signed by Cloud and bound to this
// credential's id and version, redeemed by the one executor the deployment fixes, and the reference is
// still compared against the upstream's normalized identity at admission. Nothing here upgrades an
// existing credential: only a create writes this column, rotation rewrites material alone, and a
// reference that says `["clone"]` keeps saying it.
//
// Three orderings from the approved decisions are implemented here, and they are the reason create
// and rotate are split into a short transaction, an out-of-transaction handoff, and a second short
// transaction:
//
//   - Create hands the token over first and commits the metadata second (store D4), so a crash in
//     between leaves a store-side orphan the retry reconciles instead of a metadata row pointing at
//     nothing.
//   - Rotate writes the new material first and advances the metadata pointer second (store D4), never
//     delete-then-write, so an execution admitted under the old version can still settle.
//   - Revoke commits Cloud's `availability` first — that is authoritative — and destroys the material
//     afterwards, best effort (store D4). A failure there is residual material to sweep, never a
//     failed revocation.
//
// No transaction here spans the handoff: `cloud/AGENTS.md` forbids holding one across external work,
// and the handoff is a network call into another process with its own data store.

// The three bounds below are exported because the published contract carries them: the request schema
// a client generates from must state the same limit this boundary enforces, or a client can build a
// request the server is guaranteed to refuse.

// GitCredentialNameMax is the longest display name a credential may carry, in characters. It matches
// the column's own bound, so the boundary and the schema agree on the same unit and no accepted name
// can fail the `char_length` check.
const GitCredentialNameMax = 120

// GitCredentialRepositoryScopeMax bounds a scope URL in bytes. It is the bound `createProject`
// already applies to a repository URL, because a scope is only ever compared against those.
const GitCredentialRepositoryScopeMax = 2048

// GitCredentialTokenMax is the longest token the ingress will carry. It is the Credential Service's
// own bound, applied here as well so a token the Service would refuse is answered as a client error
// instead of being discovered as an infrastructure failure one round trip later.
const GitCredentialTokenMax = 4096

// gitCredentialColumns is the exact public projection of a credential.
//
// It is a column list rather than `*` on purpose. A reference is not a secret, but D9 keeps it out of
// every public response, and a query that never reads the column cannot leak it through a field added
// later. It also excludes the freeze bookkeeping the caller has no use for.
const gitCredentialColumns = "id,tenant_id,owner_user_id,name,repository_scope,attribution,availability,unavailable_reason,capabilities,version,created_at,revoked_at"

// gitCredentialNamespace is the fixed UUIDv5 namespace credential ids are derived in. It is a
// package-level value and never reassigned; it exists so every process derives the same id from the
// same inputs, which is what makes the cross-store create idempotent across a restart.
var gitCredentialNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://cloud.ora.space/credentials/git"))

// gitCredentialID derives the id a create will use from the tenant, the owner and the request's
// idempotency key.
//
// D4 makes the two-store create idempotent on `credential_refs.id`, and the id must therefore be
// known before the metadata row exists: the handoff happens first and needs the key it will be
// retried under. Deriving it rather than drawing it is what lets a retry that follows a failed
// metadata commit reproduce the same id, so the Store recognizes the write as a replay and returns
// the original reference instead of sealing a second copy. The inputs are never echoed anywhere, and
// the result is an ordinary opaque UUID — nothing about the owner or the key can be read back out of
// it.
func gitCredentialID(tenantID, ownerID, key string) string {
	return uuid.NewSHA1(gitCredentialNamespace, []byte(tenantID+"\n"+ownerID+"\n"+key)).String()
}

// validGitCredentialName reports whether a name is usable as the label a credential is listed under.
//
// The count is in characters, the unit the column's own `char_length` check uses. The name authorizes
// nothing — the credential's authority comes from its attribution, scope and capabilities — so this
// only bounds a display value and refuses the control characters that would let one name forge a
// second list entry.
func validGitCredentialName(name string) bool {
	characters := utf8.RuneCountInString(name)
	return utf8.ValidString(name) && characters >= 1 && characters <= GitCredentialNameMax &&
		name == strings.TrimSpace(name) && !strings.ContainsAny(name, "\r\n\x00")
}

// validGitCredentialToken reports whether a token may cross the ingress.
//
// A token is refused rather than repaired. Trimming a pasted newline would silently change the secret
// and leave the caller unable to tell which bytes were sent, which is exactly the confusion D7's
// reason codes exist to prevent. The bound and the NUL check are the Credential Service's own.
func validGitCredentialToken(token string) bool {
	return token != "" && len(token) <= GitCredentialTokenMax &&
		token == strings.TrimSpace(token) && !strings.ContainsRune(token, 0)
}

// gitCredentialCapabilities reads the capability set a create asks the new reference to carry.
//
// It is the one place a request may influence a reference's capabilities, and it is a create: the row
// does not exist yet, so the only thing the caller can name is what its own new credential will permit.
// Every later operation reads the set from the row and cannot add to it — `requireRepositoryAccess`
// fixes `clone` for the work it admits and `pushAuthorizedCredential` requires the reference to already
// declare `push` — which is the vocabulary decision's D3.1: 请求方自述不增加能力.
//
// The field is optional, and its absence means the capability every credential had before `push`
// existed. That default is what keeps the create path's published behavior unchanged for a client that
// has never heard of the field, and it is why a push-capable reference has to be asked for explicitly.
//
// A stated set is held to the vocabulary's own rule rather than repaired: unknown members, repeats, the
// empty set and a non-canonical order are all refused. The stored value is compared as a set at every
// enforcement point, so accepting a second spelling of one set would make "what the row declares" and
// "what the caller was told it declared" two values that a later reader could not reconcile. The
// `'[]'` a deployment-managed or pre-column reference carries keeps its own meaning — it permits
// nothing — and nothing here migrates one: only a create writes this field.
func gitCredentialCapabilities(body Object) ([]string, bool) {
	raw, present := body["capabilities"]
	if !present || raw == nil {
		return []string{string(cloneauth.CapabilityClone)}, true
	}
	members, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	requested := make([]string, 0, len(members))
	for _, member := range members {
		capability, ok := member.(string)
		if !ok {
			return nil, false
		}
		requested = append(requested, capability)
	}
	canonical, err := cloneauth.CanonicalCapabilities(requested)
	return canonical, err == nil
}

// normalizeRepositoryScope reduces one HTTPS repository URL to the single spelling a credential scope
// is stored and compared in (D2), and reports whether the URL can carry a scope at all.
//
// Matching is exact on the normalized form, so every rule below keeps two spellings of one repository
// from becoming two scopes — and, in the other direction, keeps two genuinely different repositories
// from collapsing into one:
//
//   - the scheme must be https, because D2 scopes V1 to HTTPS tokens and an ssh URL carries a key no
//     credential of this kind can ever match;
//   - userinfo, query and fragment are refused, because D2 forbids a URL that embeds a credential or
//     parameters, and a scope holding one would be a second secret in a metadata column;
//   - the host is lowercased, because DNS names are case-insensitive, while the port is kept, because
//     a different port is a different endpoint;
//   - the path's case is preserved, because two repositories can differ only by case and folding them
//     would widen one credential's scope to cover both;
//   - percent-encoding is refused rather than decoded, because `%2F` and `/` would otherwise become
//     the same normalized path while meaning different things to the server;
//   - one trailing `/` and one trailing `.git` are dropped, because each names the same repository as
//     its absence; an empty segment, `.` or `..` is refused instead, because dropping one of those
//     would do the same thing for a path that does not obviously mean that;
//   - at least one path segment is required, so a bare host never becomes a host-wide scope. One
//     segment is enough: `https://host/repo` names exactly one repository, and because matching is
//     exact it cannot widen to `https://host/other` either. A deeper path is not safer, only less
//     usable — a deployment whose repositories sit at the host root could then hold no credential.
func normalizeRepositoryScope(raw string) (string, bool) {
	text := strings.TrimSpace(raw)
	if text == "" || len(text) > GitCredentialRepositoryScopeMax {
		return "", false
	}
	parsed, err := url.Parse(text)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return "", false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", false
	}
	if strings.ContainsRune(parsed.Path, '%') {
		return "", false
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	segments[len(segments)-1] = strings.TrimSuffix(segments[len(segments)-1], ".git")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	return "https://" + strings.ToLower(parsed.Host) + "/" + strings.Join(segments, "/"), true
}

// gitCredentialView loads one personal credential the caller owns.
//
// A credential belonging to another user of the same tenant, or to another tenant, is reported as
// absent rather than as forbidden: D1's attribution root is `tenant_id` + `owner_user_id`, and a
// distinguishable answer would let a member enumerate the deployment's credentials. Revoked rows stay
// visible to their owner, which is the only way "revoked" stays distinguishable from "never existed"
// — the row is where the revocation is recorded, because `deleted_at` deliberately has no writer
// (D5).
func gitCredentialView(t *transaction, tid, uid, id string) Object {
	require(validID(id), 404, "not_found")
	c := t.one("SELECT "+gitCredentialColumns+" FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND owner_user_id=$3 AND purpose='git' AND attribution='personal' AND deleted_at IS NULL", id, tid, uid)
	require(c != nil, 404, "not_found")
	return c
}

// listGitCredentials pages the caller's own personal credentials in this tenant.
//
// The list is scoped to the caller by `owner_user_id` and filtered to `attribution='personal'`, so a
// deployment-managed reference (which `ConfigureCredential` writes as `unknown`) and a team credential
// can never appear in it. Tenant membership is checked by the caller of this function, not here: the
// list spans one tenant the caller has already proved membership of.
func listGitCredentials(t *transaction, r *PublicRequest, uid string) Object {
	return page(t, "SELECT "+gitCredentialColumns+" FROM credential_refs WHERE tenant_id=$1 AND owner_user_id=$2 AND purpose='git' AND attribution='personal' AND deleted_at IS NULL", []any{r.TenantID, uid}, "id", r)
}

// CredentialSecretWriter is the Credential Service's ingress capability, declared at the boundary
// that consumes it.
//
// The interface is structural: `credentialstore.Client` satisfies it without this package importing
// the transport, so the three ordering rules below are the only coupling between Cloud's aggregate
// and the store. Its methods are exactly the capabilities store D3 grants the `ingress-writer`
// identity — write, rotate, revoke — and deliberately none of the resolver's: Cloud holds no
// decryption path, and resolve is a capability it does not have.
type CredentialSecretWriter interface {
	// Write stores a token for one credential id and returns the Store-generated reference. Repeating
	// it with the same id and the same material returns the original reference (D4).
	Write(ctx context.Context, credentialID, token string) (string, error)
	// Rotate replaces the material of an existing credential and returns the Store's material version
	// afterwards.
	//
	// The version is returned because it, and not a counter Cloud keeps, is what a later authorization
	// names: the Store resolves an authorization by looking the version up among the generations it can
	// still serve, and refuses one naming a version it does not have. A caller that advanced its own
	// counter instead would record a version the material was never written under, and every
	// authorization built from it would be refused — permanently, since nothing repairs a version the
	// Store never issued. Re-rotating with the current material is a no-op that returns the version
	// already current, which is why the returned value rather than an increment is the only answer that
	// stays correct on a retry.
	Rotate(ctx context.Context, credentialID, token string) (int64, error)
	// Revoke destroys the material of a credential Cloud has already marked unusable. It is idempotent:
	// "already gone" is success, because the caller's desired end state is "no material".
	Revoke(ctx context.Context, credentialID string) error
}

// credentialRefusal is the refusal code a Credential Service client reports, when it reports one.
//
// The code is read through this structural interface so `core` stays free of the transport package:
// only a client that speaks the store's refusal vocabulary can influence a public reason code, and
// anything else is an infrastructure failure by construction.
type credentialRefusal interface{ ReasonCode() string }

// credentialStoreFault maps a handoff failure to the public reason code.
//
// D6 requires an unavailable or unverifiable store to fail closed, and D7 requires reason codes that
// tell the cases apart without revealing anything about the secret. The mapping is therefore total
// and never guesses: an unrecognized failure — including one from a client that reports no code at
// all — is `credential_store_unavailable`, and no branch carries any part of the failure's text.
func credentialStoreFault(err error) *Fault {
	var refusal credentialRefusal
	if errors.As(err, &refusal) {
		switch refusal.ReasonCode() {
		case "credential_revoked":
			// The store holds a tombstone. For a create that means the idempotency key names a
			// credential that was already revoked; D4's tombstone is what stops the retry from
			// resurrecting material Cloud has reported as gone.
			return &Fault{Code: "credential_revoked", Status: 409, Params: Object{}}
		case "credential_conflict", "credential_not_found":
			// Cloud's metadata and the store disagree about a credential both were told about — a
			// different secret under a replayed id, or a row whose material is absent. That is an
			// operator problem, and the client must not be told to retry it blindly.
			return &Fault{Code: "credential_store_inconsistent", Status: 409, Params: Object{}}
		case "invalid_request":
			// The ingress validated the same shape before handing it over, so the store refusing it
			// means the two bounds have diverged.
			return &Fault{Code: "credential_store_rejected_request", Status: 400, Params: Object{}}
		}
	}
	return &Fault{Code: "credential_store_unavailable", Status: 503, Params: Object{}}
}

// GitCredentialResult is one credential-ingress outcome.
type GitCredentialResult struct {
	// Resource is the public credential document, in the same shape every read returns.
	Resource Object
	// Status is the response status.
	Status int
	// Reconciliation names a credential whose stored material Cloud could not confirm as destroyed
	// after the authoritative revocation committed (store D4). It is a server-side signal: it never
	// enters the response, and the caller records it as residue for a later sweep. Empty means there is
	// nothing to reconcile.
	Reconciliation string
}

// WriteGitCredential executes one credential-ingress mutation: create, rotate or revoke.
//
// `token` is the one raw secret the ingress accepts, and it arrives as a parameter rather than as a
// field of `r.Body` so that no part of the aggregate — including the request hash it persists for
// idempotency — can carry it further than the handoff. It is empty for a revoke, which destroys
// material rather than replacing it, and for a replay, which never reaches the store at all.
func (s *Store) WriteGitCredential(ctx context.Context, r *PublicRequest, token string) (GitCredentialResult, error) {
	if r.Method != "DELETE" && s.Secrets == nil {
		// D6: there is no anonymous or local fallback. A deployment with no credential service cannot
		// hold a token, and Cloud's own database is explicitly not a substitute for one (ingress
		// exception 3). Refusing before any transaction also means an unconfigured deployment never
		// writes a metadata row it could not honor.
		return GitCredentialResult{}, &Fault{Code: "credential_store_unavailable", Status: 503, Params: Object{}}
	}
	switch r.Method {
	case "POST":
		return s.createGitCredential(ctx, r, token)
	case "PUT":
		return s.rotateGitCredential(ctx, r, token)
	case "DELETE":
		return s.revokeGitCredential(ctx, r)
	}
	return GitCredentialResult{}, &Fault{Code: "method_not_allowed", Status: 405, Params: Object{}}
}

// gitCredentialPrepare is what the first transaction of a create decides.
type gitCredentialPrepare struct {
	// userID is the resolved owner the credential will be attributed to.
	userID string
	// id is the credential id the handoff will use and the metadata row will carry.
	id string
	// name and scope are the validated, normalized fields the insert will write.
	name, scope string
	// capabilities is the canonical capability set the insert will write. It is fixed here, before the
	// handoff, so the stored set is the one this request was validated for rather than whatever a later
	// read of the request would produce.
	capabilities []string
	// hash is the idempotency hash of this request. It covers the body with the token already removed
	// and is therefore never a digest of anything secret.
	hash string
	// response and status are set only when an earlier attempt under the same key already committed.
	// The handoff is then skipped entirely: the secret is already in the store and its reference is
	// already in the recorded response.
	response Object
	status   int
}

// replay reports whether the request is a repeat of a committed attempt. The prepare value is taken by
// pointer because it carries a decoded response document and callers already hold it in place.
func (p *gitCredentialPrepare) replay() bool { return p.status != 0 }

// prepareGitCredential authorizes the caller, validates the request and decides the credential id
// without writing anything.
//
// It is read-only on purpose: the handoff happens after it and the metadata commit after that, so a
// request that turns out to be a replay never reaches the store, and a request that fails validation
// or authorization never stores a secret Cloud would then have to compensate for.
func (s *Store) prepareGitCredential(ctx context.Context, r *PublicRequest) (gitCredentialPrepare, error) {
	name, scope := r.Body.S("name"), r.Body.S("repositoryScope")
	if !validGitCredentialName(name) {
		return gitCredentialPrepare{}, &Fault{Code: "invalid_credential_name", Status: 400, Params: Object{}}
	}
	normalized, ok := normalizeRepositoryScope(scope)
	if !ok {
		return gitCredentialPrepare{}, &Fault{Code: "invalid_repository_scope", Status: 400, Params: Object{}}
	}
	capabilities, ok := gitCredentialCapabilities(r.Body)
	if !ok {
		return gitCredentialPrepare{}, &Fault{Code: "invalid_credential_capabilities", Status: 400, Params: Object{}}
	}
	var prepared gitCredentialPrepare
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		hash := requestHash(r.Method, r.Path, r.Body)
		if old := t.one("SELECT * FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3", r.TenantID, uid, r.Key); old != nil {
			require(old.S("requestHash") == hash, 409, "idempotency_conflict")
			prepared = gitCredentialPrepare{userID: uid, hash: hash, response: old.O("response"), status: int(old.N("status"))}
			return prepared.response
		}
		prepared = gitCredentialPrepare{userID: uid, id: gitCredentialID(r.TenantID, uid, r.Key), name: name, scope: normalized, capabilities: capabilities, hash: hash}
		return Object{}
	})
	if err != nil {
		return gitCredentialPrepare{}, err
	}
	return prepared, nil
}

// createGitCredential stores a new token and then commits the metadata that locates it.
func (s *Store) createGitCredential(ctx context.Context, r *PublicRequest, token string) (GitCredentialResult, error) {
	if !validGitCredentialToken(token) {
		return GitCredentialResult{}, &Fault{Code: "invalid_credential_token", Status: 400, Params: Object{}}
	}
	// POST carries an idempotency key like every other public create, and here it does more than
	// deduplicate responses: it names the credential id, so it is the whole reason the cross-store
	// create can be retried at all.
	if r.Key == "" || len(r.Key) > 200 {
		return GitCredentialResult{}, &Fault{Code: "idempotency_key_required", Status: 400, Params: Object{}}
	}
	prepared, err := s.prepareGitCredential(ctx, r)
	if err != nil {
		return GitCredentialResult{}, err
	}
	if prepared.replay() {
		return GitCredentialResult{Resource: prepared.response, Status: prepared.status}, nil
	}
	secretRef, err := s.Secrets.Write(ctx, prepared.id, token)
	if err != nil {
		// No compensation is needed here: the metadata row does not exist yet. A write that reached the
		// store but lost its answer leaves an orphan the retry adopts, because the retry derives the
		// same id and the store's own idempotency returns the reference it already holds (D4).
		return GitCredentialResult{}, credentialStoreFault(err)
	}
	var out Object
	_, err = s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		if old := t.one("SELECT * FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3", r.TenantID, uid, r.Key); old != nil {
			// A concurrent request sharing this key committed while the handoff was in flight. Its row
			// and its secret are the ones that count; this attempt's store call was the same idempotent
			// write, so replaying the committed response is the correct answer for both callers.
			require(old.S("requestHash") == prepared.hash, 409, "idempotency_conflict")
			out = old.O("response")
			return out
		}
		// `attribution` is written explicitly rather than left to the `unknown` default: D2 makes that
		// default the marker of a deployment-managed reference, and a product credential that inherited
		// it would be indistinguishable from one the freeze triggers treat as unattributed.
		// `capabilities` is written from the validated request rather than read back from it here, and it
		// is written as a marshaled JSON array because the column is `jsonb`. The default in
		// `gitCredentialCapabilities` is what keeps an older client's create byte-identical in meaning:
		// the set it does not mention is the one it always got.
		t.exec("INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,attribution_basis,repository_scope,capabilities) VALUES($1,$2,$3,'git',$4,$5,'personal','user_stated',$6,$7::jsonb)",
			prepared.id, r.TenantID, uid, secretRef, prepared.name, prepared.scope, jsonText(prepared.capabilities))
		out = t.one("SELECT "+gitCredentialColumns+" FROM credential_refs WHERE id=$1", prepared.id)
		t.exec("INSERT INTO idempotency_records(tenant_id,user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5,201)", r.TenantID, uid, r.Key, prepared.hash, jsonText(out))
		return out
	})
	if err != nil {
		// The store now holds material no metadata row points at. That orphan is deliberate and
		// recoverable: the client's retry under the same key derives the same id, so the store answers
		// with the reference it already has and the commit below completes. Sweeping an orphan nobody
		// retries is the store's own open question in its ADR, not something this path may guess at.
		return GitCredentialResult{}, err
	}
	return GitCredentialResult{Resource: out, Status: 201}, nil
}

// rotateGitCredential replaces the material of a credential and then records the Store's material
// version for it.
func (s *Store) rotateGitCredential(ctx context.Context, r *PublicRequest, token string) (GitCredentialResult, error) {
	if !validGitCredentialToken(token) {
		return GitCredentialResult{}, &Fault{Code: "invalid_credential_token", Status: 400, Params: Object{}}
	}
	if !validID(r.CredentialID) {
		return GitCredentialResult{}, &Fault{Code: "not_found", Status: 404, Params: Object{}}
	}
	// First transaction: authorize the caller and require the metadata the rotation will advance to be
	// exactly where the caller thinks it is. Checking before the handoff keeps an unauthorized or stale
	// request from replacing material at all.
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		rotatableGitCredential(t, r, uid)
		return Object{}
	})
	if err != nil {
		return GitCredentialResult{}, err
	}
	// The handoff happens outside any transaction: a rotation is an HTTP call to another service, and
	// the execution contract forbids holding Cloud's lock across it. The Store's absence is already
	// refused by the ingress entry point, before any transaction runs.
	stored, err := s.Secrets.Rotate(ctx, r.CredentialID, token)
	if err != nil {
		return GitCredentialResult{}, credentialStoreFault(err)
	}
	// Second transaction: record the version the Store just wrote. D4 orders the handoff first, so a
	// failure here leaves material newer than the metadata. The row then lags whatever the Store holds —
	// by this rotation, or by more if a concurrent rotation won the row's own version check and this one
	// was reported to its caller as a conflict — and authorizations naming the lagging version are
	// refused until it is closed. `version < $2` is what closes it: the Store's counter only moves
	// forward, so the next rotation of this credential writes the Store's current version whatever token
	// it carries, and the row catches up. That ordering is what D4 asks for; the reverse order could
	// leave a reference pointing at material that was never written.
	//
	// `version` is the Store's own counter, not Cloud's, because it is the value an authorization
	// carries: `requireRepositoryAccess` stamps this column into every plan, and the Store refuses a
	// plan naming a version it cannot serve. So the row tracks the Store monotonically and never leads
	// it. The predicate is what makes that true under a retry: an identical re-rotation returns the
	// version already current, and advancing the column past it would leave every later authorization
	// naming a generation the Store never wrote, which is a credential that can never be used again.
	var out Object
	_, err = s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		c := rotatableGitCredential(t, r, uid)
		t.exec("UPDATE credential_refs SET version=$2 WHERE id=$1 AND version < $2", c.S("id"), stored)
		out = t.one("SELECT "+gitCredentialColumns+" FROM credential_refs WHERE id=$1", c.S("id"))
		return out
	})
	if err != nil {
		return GitCredentialResult{}, err
	}
	return GitCredentialResult{Resource: out, Status: 200}, nil
}

// rotatableGitCredential loads a credential the caller owns and applies the preconditions a rotation
// has. It runs in both rotate transactions so the second one re-decides on current state rather than
// trusting what the first saw.
//
// A credential that is not `available` cannot be rotated. A frozen reference is one the freeze
// triggers withdrew — its owner left the tenant or was disabled — and D5's semantics are explicit that
// rejoining does not unfreeze it, so letting a rotate write fresh material into it would leave the
// row's availability permanently disagreeing with its contents.
func rotatableGitCredential(t *transaction, r *PublicRequest, uid string) Object {
	c := gitCredentialView(t, r.TenantID, uid, r.CredentialID)
	require(c.S("availability") == "available", 409, "credential_not_available")
	version(c, r.Body.N("version"))
	return c
}

// revokeGitCredential withdraws a credential. Cloud's metadata change is the authoritative one and
// lands first; the material is destroyed afterwards, best effort.
func (s *Store) revokeGitCredential(ctx context.Context, r *PublicRequest) (GitCredentialResult, error) {
	if !validID(r.CredentialID) {
		return GitCredentialResult{}, &Fault{Code: "not_found", Status: 404, Params: Object{}}
	}
	if r.Key == "" || len(r.Key) > 200 {
		return GitCredentialResult{}, &Fault{Code: "idempotency_key_required", Status: 400, Params: Object{}}
	}
	hash := requestHash(r.Method, r.Path, r.Body)
	var out Object
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		if old := t.one("SELECT * FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3", r.TenantID, uid, r.Key); old != nil {
			require(old.S("requestHash") == hash, 409, "idempotency_conflict")
			out = old.O("response")
			return out
		}
		c := gitCredentialView(t, r.TenantID, uid, r.CredentialID)
		if c.S("availability") != "invalid" {
			version(c, r.Body.N("version"))
			// Revocation is expressed through `availability`, never through `deleted_at`: D5 leaves the
			// deletion column without a writer because its semantics are not defined yet, and taking it
			// over here would put a second meaning on a column the whole system already reads as
			// "hidden". `revoked_at` is the audit timestamp beside the state, not a second authority.
			t.exec("UPDATE credential_refs SET availability='invalid',unavailable_reason='revoked_by_owner',revoked_at=now(),version=version+1 WHERE id=$1", c.S("id"))
			c = t.one("SELECT "+gitCredentialColumns+" FROM credential_refs WHERE id=$1", c.S("id"))
		}
		// Already revoked: the owner's desired end state is "unusable" and reaching it twice is not an
		// error, so the version is deliberately not compared. A client that lost its idempotency key
		// between attempts then still sees success rather than a conflict on a state it cannot have
		// observed, and nothing is written that would bump the version a second time.
		out = c
		t.exec("INSERT INTO idempotency_records(tenant_id,user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5,200)", r.TenantID, uid, r.Key, hash, jsonText(out))
		return out
	})
	if err != nil {
		return GitCredentialResult{}, err
	}
	return s.destroyGitCredentialMaterial(ctx, r.CredentialID, out), nil
}

// destroyGitCredentialMaterial asks the store to destroy the material of a credential Cloud has
// already reported as revoked, and reports the residue when it cannot.
//
// Store D4 orders revocation this way on purpose. Cloud's `availability` has committed and is
// authoritative, so a failure here must not be reported as a failed revocation — that would invite a
// retry of a request that already succeeded — and must not be swallowed either, because the residue is
// the only evidence a later sweep has. The identifier is returned rather than an error: the caller owns
// the logger, and an identifier is the only part of this that is safe to record.
func (s *Store) destroyGitCredentialMaterial(ctx context.Context, credentialID string, resource Object) GitCredentialResult {
	result := GitCredentialResult{Resource: resource, Status: 200}
	if s.Secrets == nil {
		// No credential service is configured, so this deployment never stored material for the
		// credential being revoked. There is nothing to destroy and nothing to sweep.
		return result
	}
	// A client that hung up after the response was decided must not cancel the destruction: the
	// revocation has committed and the material is still there. The call stays bounded by the client's
	// own request timeout, so an unreachable store cannot hold the handler open indefinitely.
	if err := s.Secrets.Revoke(context.WithoutCancel(ctx), credentialID); err != nil {
		result.Reconciliation = credentialID
	}
	return result
}
