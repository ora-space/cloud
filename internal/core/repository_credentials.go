package core

import (
	"context"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// repositoryCredentialExecutorUnavailable is the stable code Cloud answers a credentialed clone with
// when this deployment installed no credentialed-clone executor (D6). It is a statement about the
// deployment's capability rather than about the request: the executor boundary is installed once at
// startup, so no retry, no later attempt and no other Controller can change the answer.
const repositoryCredentialExecutorUnavailable = "repository_credential_executor_unavailable"

// repositoryCredentialCapabilityUnsupported is the stable code Cloud answers with when the reference
// a Project names does not permit the operation the clone needs.
//
// The continuity decision's D4 names this refusal next to the other two the execution plan can fail
// on — "找不到引用、范围不符或能力不支持时明确拒绝" — so it is a reason of its own rather than a case of
// one of them: the reference exists, its scope matches, and it still cannot be used for this work.
const repositoryCredentialCapabilityUnsupported = "repository_credential_capability_unsupported"

// terminalCredentialRefusals are the refusal codes that can never change for as long as the
// deployment and the reference stay as they are, and that therefore have to settle an Operation
// rather than leave it retrying.
//
// Both are facts about durable state rather than about this attempt: the executor boundary is
// installed once at startup, and a reference's capability set is written when the reference is
// created. A Controller that reads either as a retryable conflict would dispatch again forever
// against an answer that cannot move.
var terminalCredentialRefusals = map[string]bool{
	repositoryCredentialExecutorUnavailable:   true,
	repositoryCredentialCapabilityUnsupported: true,
}

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
		reject(409, repositoryCredentialExecutorUnavailable)
	}
	// The reference has to permit the work, not merely exist. D4 requires the execution plan to fix
	// "引用身份、版本、仓库与所需能力" and requires an unsupported capability to be refused explicitly, so
	// the capability the clone needs is compared against the one the reference declares, here, before
	// any authorization can name it.
	//
	// This check sits after the deployment-level one on purpose. A deployment with no executor refuses
	// every credentialed clone whatever the reference says, and that refusal is the one the fail-closed
	// decision fixes and settles; reporting a capability problem there would describe the reference as
	// the obstacle when the deployment is.
	//
	// The set is canonicalized rather than only searched, because it is about to become part of a
	// signed authorization, and `Issue` refuses a set this package could not have issued. Rejecting it
	// here keeps an unissuable plan from reaching the point where signing it would fail as a server
	// error. `CanonicalCapabilities` also refuses the empty set, which is what a reference written
	// before the column existed carries: it permits nothing, and reading it as "no restriction" would
	// turn an unclassified reference into an unrestricted one.
	capabilities, err := cloneauth.CanonicalCapabilities(credentialCapabilities(c))
	require(err == nil && cloneauth.HasCapability(capabilities, cloneauth.CapabilityClone), 409, repositoryCredentialCapabilityUnsupported)
	// The admission is recorded rather than returned, because the caller that acts on it runs outside
	// this transaction: a clone takes minutes and the execution contract forbids holding the control
	// plane's lock across it.
	t.admittedCredential = &admittedCredential{
		CredentialID:      id,
		CredentialVersion: c.N("version"),
		OwnerUserID:       c.S("ownerUserId"),
		Repository:        c.S("repositoryScope"),
		Capabilities:      capabilities,
	}
}

// credentialCapabilities reads a credential row's capability set.
//
// The column is `jsonb`, so it arrives through the row's JSON projection as a list of untyped values
// rather than as a `[]string`. Anything that is not a list of strings reads as no capabilities at
// all: a malformed set must not be narrowed into a usable one by skipping the member that did not
// parse, and the caller refuses an empty set outright.
func credentialCapabilities(c Object) []string {
	raw, ok := c["capabilities"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, member := range raw {
		capability, ok := member.(string)
		if !ok {
			return nil
		}
		out = append(out, capability)
	}
	return out
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
	// Capabilities is the canonical capability set admission required and found, so the dispatch binds
	// the same set the guard approved rather than reading the column a second time.
	Capabilities []string
}

// settleRefusedCredentialedClone records the terminal fact of a clone admission this deployment
// refused for one of the reasons that can never change.
//
// A refused dispatch is not by itself the end of an Operation. The Controller reads the 409 as a
// retryable conflict and leaves the Operation `running`, so it dispatches again — and is refused
// again, forever, because the guard is total. That is not a harmless loop: `one_project_operation`
// covers `running`, so the Project's single operation slot stays occupied for good, the Project stays
// `provisioning`, and nothing the user can do clears it. Recording `failed` is what makes the refusal
// terminal, and it is the settlement the fail-closed decision describes: the Operation is Cloud's
// durable fact about the work, and this refusal is that work's outcome. The stable `errorCode` is
// carried on the Operation the public contract already exposes, so the reason reaches the user
// instead of a Project that looks forever like it is still being created.
//
// Every code settled here is one of `terminalCredentialRefusals`, and the settlement covers all of
// them rather than the deployment-level one alone: each is a fact about durable state, so each would
// otherwise wedge the Project exactly the same way.
//
// It runs in its own transaction because the refusal has already unwound the dispatch's transaction:
// `reject` panics and `transact` rolls back, so a write made on the way to the refusal would not
// survive it. Nothing here can weaken the refusal — the caller's error is returned unchanged, and
// this only records a decision Cloud has already made and would make again.
//
// The returned error is the settlement's own, never the refusal's, and it is returned only when the
// refusal was settled here and the settlement could not be written. A caller that receives it retries;
// the refusal is repeated and the settlement is attempted again, so a transient failure heals itself.
func (s *Store) settleRefusedCredentialedClone(ctx context.Context, r *ControlRequest, cause error) error {
	code := ErrorCode(cause).Code
	if !terminalCredentialRefusals[code] {
		return nil
	}
	// Only a clone dispatch can be refused this way, and only for a Workspace Operation: on the
	// dispatch path the guard this code comes from is reached from `workspaceCloneDispatch` alone.
	// The checks below repeat that, because the settlement must be exactly as narrow as the refusal
	// and a later change to the dispatch path must not silently widen it.
	if r.Action != "clone_dispatch" {
		return nil
	}
	operation := r.Body.S("operationId")
	if !validID(operation) {
		return nil
	}
	_, err := s.transact(ctx, func(t *transaction) Object {
		// The WHERE clause is the whole safety argument, so every condition in it is load-bearing.
		//
		//   state='running' AND step='clone' — the dispatch that was refused, still where it was. A
		//     terminal Operation no longer matches, which is what makes a repeated settlement a no-op
		//     rather than a second, conflicting fact.
		//   controller_epoch — the Controller whose dispatch was refused still owns the Operation.
		//     `workspaceCloneDispatch` checked this epoch before it reached the guard, and it would
		//     refuse under any other epoch too, so a moved epoch means a newer Controller is already
		//     driving the Operation: that one is refused and settles in its own turn, and this stale
		//     one must not write over the newer state.
		//   credential_ref_id IS NOT NULL — the refusal is about this Project's credential binding
		//     (D7's authoritative column). Without it, a Project that names no credential could be
		//     settled by a code that cannot apply to it.
		//
		// `retry_at` is cleared rather than left as it was: `claim` moves a due retry to `running`
		// without clearing the deadline it fired on, so a settled Operation could otherwise carry a
		// retry time that no longer means anything and would be read back as a pending retry.
		t.exec("UPDATE operations SET state='failed',error_code=$2,retry_at=NULL,version=version+1,updated_at=now()"+
			" WHERE id=$1 AND workspace_id IS NOT NULL AND state='running' AND step='clone' AND controller_epoch=$3"+
			" AND EXISTS (SELECT 1 FROM projects p WHERE p.id=operations.project_id AND p.credential_ref_id IS NOT NULL)",
			operation, code, r.Body.N("epoch"))
		return nil
	})
	return err
}

// freezeInactiveRepositoryCredentials also catches deployment-side state changes before admission.
// The database triggers make membership deactivation and freezing atomic; this is a recovery scan.
func freezeInactiveRepositoryCredentials(t *transaction) {
	t.exec("UPDATE credential_refs c SET availability='frozen',unavailable_reason='associated_member_departed',version=version+1 WHERE c.attribution IN ('personal','unknown') AND c.availability='available' AND NOT EXISTS(SELECT 1 FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=c.tenant_id AND m.user_id=c.owner_user_id AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL)")
}
