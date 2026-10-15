package core

import (
	"context"
	"errors"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// The stable codes one push approval can end with.
//
// They are the vocabulary the audit's `resultCode` is drawn from, and each names one answer the write
// can have. The list is deliberately not closed: an attempt refused before anything is sent ends under
// the guard's own code (`repository_credential_frozen`, `repository_credential_binding_conflict`,
// `push_credential_required`, `repository_credential_unavailable`, and the guard's other answers),
// because an operator acts on those directly and collapsing them into one "refused" would hide which
// repair is needed. What the codes below have in common is that none is produced by a check that could
// have been satisfied a different way: the deployment's capability is installed once at startup, and
// every other one is either what the executor established about the remote or the honest statement that
// it established nothing.
const (
	// pushExecutorUnavailable is the deployment-level answer: this Cloud has no push executor, so the
	// approval Cloud just recorded cannot be carried out. It is the same answer the credentialed-clone
	// guard gives a Project whose credential the deployment cannot serve, and it is the only outcome a
	// deployment without C can produce.
	//
	// It is a *settled* answer rather than a refusal of the request, and the difference is the point.
	// A push approval is a person's explicit decision, and the record of it is the thing the audit
	// exists to keep: refusing the request would leave no trace that anyone asked. So the approval is
	// recorded and then settled with this code, which still carries no authorization to anyone — Cloud
	// mints the authorization identifier at the same moment and never signs a bearer for it, so there
	// is nothing to spend and no way for a later deployment change to turn this row into a write.
	pushExecutorUnavailable = "push_executor_unavailable"
	// pushApplied is the only success: the remote holds the authorized commit at the authorized ref,
	// and the value is this write's doing.
	pushApplied = "push_applied"
	// pushNotApplied is a completed attempt whose authorized change is not on the remote. It is a
	// failure rather than an unknown because the executor reports it from the remote's own answer, not
	// from its own exit status.
	pushNotApplied = "push_not_applied"
	// pushAttributionUnknown is the case the execution authority's D7 names: the ref does hold the
	// authorized commit, but nothing proves this write is what put it there. It is recorded as
	// uncertain rather than as success, because "the remote ended up correct" and "this approval is
	// what made it correct" are different claims and only the first one can be supported here.
	pushAttributionUnknown = "push_attribution_unknown"
	// pushResultUnknown is a write whose outcome Cloud cannot establish at all: the executor could not
	// be reached, so the request may or may not have arrived. It is never inferred to be a failure —
	// a failed claim would be as unsupported as a successful one — and reconciling it is the
	// executor's, against the remote.
	pushResultUnknown = "push_result_unknown"
	// pushTargetConflict is the third value the reconciliation can find: the authorized ref holds neither
	// the commit the approval observed nor the commit it authorized. Another writer moved it, or this
	// write landed and was then superseded — one observation cannot tell those apart, which is why the
	// outcome is recorded as a conflict rather than as a success or a failure.
	pushTargetConflict = "push_target_conflict"
)

// The states one push approval moves through. The first is Cloud's own: an approval whose dispatch has
// not reported yet. The other three are the execution authority's D7 terminal states, and there is no
// fourth "unknown later" state — an unsettled approval is `dispatching`, which already means exactly
// that its outcome is not known here.
const (
	pushDispatching = "dispatching"
	pushSucceeded   = "succeeded"
	pushFailed      = "failed"
	pushUncertain   = "uncertain"
)

// PushExecution is the whole input of the push executor boundary: what one approval authorized, and
// nothing about how to carry it out.
//
// It carries no credential material and no signature. The signing key and the executor's own identity
// belong to whoever dispatches (the `PushExecutor` implementation), because "who executes a push" is a
// deployment fact: C runs on no Node, so there is no Node identity, no runtime generation and no
// control epoch to bind — the fields the execution authority had to withdraw for pushes.
type PushExecution struct {
	// AuthorizationID is the identifier the Credential Service records consumption under. It is minted
	// once per *attempt*: the first attempt carries the identifier the approval was recorded under, and a
	// re-attempt — the reconciliation of a dispatch that never came back — is issued a fresh one for the
	// same binding, because a spent authorization must never be replayed (D7). One identifier therefore
	// still names exactly one write attempt, which is what makes "one authorization, one write" checkable
	// across a retry.
	AuthorizationID string
	TenantID        string
	// OwnerUserID is the owner of the credential the approval binds, not the person who approved it.
	// The two are different subjects and both are bound: the binding answers "whose credential", while
	// the approval record answers "who decided".
	OwnerUserID string
	ProjectID   string
	WorkspaceID string
	// OperationID is the approval's own identity — the push request's id. A push is not part of an
	// Operation (the execution authority's D2), so the subject the binding names is the approval
	// itself, and the same identifier is what the audit and the authorization both point at.
	OperationID string
	// CredentialID and CredentialVersion pin the exact material the approval was made against.
	CredentialID      string
	CredentialVersion int64
	// Repository is the canonical upstream, Ref the qualified ref, and OldCommit/NewCommit the pair
	// the update moves between. All four are the values the approval is bound to.
	Repository string
	Ref        string
	OldCommit  string
	NewCommit  string
	// Capabilities is the capability set this operation requires, in canonical order. It is the
	// requirement rather than the reference's whole declared set, because an authorization may carry
	// exactly what its operation permits.
	Capabilities []string
}

// PushVerdict is what one push attempt established about the remote: the execution authority's D7
// terminal outcomes, as this package names them.
//
// It is this package's own vocabulary rather than the executor's wire spelling, and the difference is
// the point. The executor reports what it saw; what a seeing means for an approval — which state the
// row settles into and which code the audit carries — is Cloud's policy, and it is decided in one place
// (`settlePushFor`) so that a change to either vocabulary cannot silently change the other. The five
// members are mutually exclusive and `PushUnknown` is one of them: a push whose outcome could not be
// established must be recordable as exactly that, never as an empty value a reader could take for
// "nothing happened".
type PushVerdict string

const (
	// PushConfirmed is the only success: the remote holds the authorized commit at the authorized ref,
	// and the compare-and-swap proves this attempt is what put it there.
	PushConfirmed PushVerdict = "confirmed"
	// PushTargetSatisfied is D7's "写入者不可归因": the ref holds the authorized commit and nothing
	// attributes it to this approval. It is uncertain, not success, because the two claims differ and
	// only the weaker one is supported.
	PushTargetSatisfied PushVerdict = "target_satisfied"
	// PushNotApplied is a confirmed failure: the update was refused and the ref does not hold the
	// authorized commit.
	PushNotApplied PushVerdict = "not_applied"
	// PushTargetConflict is the third observed value: neither the old nor the new commit.
	PushTargetConflict PushVerdict = "target_conflict"
	// PushUnknown is an attempt whose outcome could not be established: a transport failure, a timeout,
	// a killed process, or a reconciliation that could not be read.
	PushUnknown PushVerdict = "unknown"
)

// PushOutcome is what a completed push reports.
//
// It carries the verdict and the one piece of evidence that belongs in the audit alongside it: what the
// authorized ref was observed to hold. The observation is recorded for the conflict case above all —
// "somebody else's commit is there now" is only actionable with the value — and it never changes the
// verdict, because a read after the fact cannot distinguish "my write never arrived" from "my write
// arrived and a later writer moved it".
type PushOutcome struct {
	Verdict PushVerdict
	// ObservedCommit is what the authorized ref held when the executor read it back, empty when it made
	// no observation. It is recorded as evidence and is never a second chance to call the write a
	// success.
	ObservedCommit string
	// CleanupFailure is the executor's own reason code for credential material it could not prove
	// destroyed after the attempt, and it is empty unless that happened. It is carried beside the verdict
	// because it is a fact about the executor rather than about the remote: the verdict still says what
	// the write did, and this says that the executor's copy of the credential may outlive it. See
	// `pushSettlement` for what Cloud records when both are present.
	CleanupFailure string
}

// PushExecutor performs one approved remote write.
//
// It is an interface rather than a concrete client so admission, settlement and the state machine can
// be tested without a network, and so the executor stays replaceable: what Cloud depends on is "the
// remote was updated to exactly this, or it was not", not on how C is built.
//
// PushDispatcher is the production implementation. Until one is installed the field stays nil, every
// approval is settled `failed` with `push_executor_unavailable`, and no authorization is ever signed —
// which is the approved fail-closed state for a deployment whose execution chain is incomplete, and the
// state every deployment is in unless it declares the push capability itself.
type PushExecutor interface {
	Execute(ctx context.Context, push PushExecution) (PushOutcome, error)
}

// SetPushExecutor installs the push executor boundary, or removes it when e is nil.
//
// Installing it is what turns an approval from "recorded and settled as unexecutable" into "dispatched",
// so the call is a deployment decision made once, at startup, from configuration. It is not exported as
// a Store field for the same reason the credentialed-clone boundary is not: which executor exists is a
// deployment fact, and a per-request one would be a per-request grant of write authority.
func (s *Store) SetPushExecutor(e PushExecutor) { s.pushExecutor = e }

// pushApproval loads the Workspace one push is made against and authorizes the caller to approve it.
//
// Two rules compose here, and the composition is narrower than either.
//
// The first is the Workspace's own: `workspace` applies `runtimeUsable`, which is the same gate every
// other workspace-scoped write meets, so a caller who may not use the Workspace may not publish its
// content either. It is applied here rather than inherited from the dispatcher's ordering so that the
// read path and the write path answer the same question.
//
// The second is the push decision's D2: the person who approves a write to a credential's repository
// must be the owner of the Project that bound it, or an administrator of the tenant. It is deliberately
// a Project rule rather than a Workspace one, because what is being published is the Project's
// repository, not this Workspace's runtime.
func pushApproval(t *transaction, r *PublicRequest, uid string) (w, p Object) {
	w = workspace(t, r.TenantID, uid, r.WorkspaceID, false)
	var m Object
	p, m = projectInSpace(t, r.TenantID, uid, w.S("projectId"))
	require(p.S("ownerUserId") == uid || m.S("role") == "admin", 403, "push_approval_forbidden")
	return w, p
}

// requirePushAccess authorizes one push against the Project's bound credential and returns the
// credential identity, its version, and the canonical upstream the approval will bind.
//
// The guard reads one Project and answers one question — may this credential publish to this
// repository — and it is a refusal, not a filter: every one of its answers is either a credential
// usable for a push or a stable refusal code. It never returns a credential it did not fully check.
//
// The deployment's own capability is deliberately absent from this list. It cannot be a refusal here,
// because a push approval that is refused leaves no record of the person who asked for it, and the
// record is the whole audit (D9). A deployment without an executor records the approval and settles it
// with `push_executor_unavailable`, which mints no authorization and produces no write; see
// `pushRequestPublic`.
//
// The guard is total, and that is what lets the dispatch worker treat its refusal as a decision rather
// than as an error: every answer is either a usable credential or a stable code, so a refusal that
// reaches `claimPushDispatch` is one Cloud made and can record.
func requirePushAccess(t *transaction, p Object) (credentialID string, version int64, upstream string) {
	// The two binding columns must agree about which credential this Project uses, including agreeing
	// that there is none. The push guard is the same total guard the clone path uses, so it reads the
	// same column and refuses the same disagreement: silently preferring either value would authorize a
	// write against a credential nobody chose.
	id, legacy := p.S("credentialRefId"), p.S("repositoryCredentialRefId")
	require(legacy == "" || legacy == id, 409, "repository_credential_binding_conflict")
	// A push publishes content through a credential, so a Project that names none has nothing to push
	// with. This is the one place the push guard is stricter than the clone guard by design: an
	// anonymous clone is a real execution path (public Git), while an anonymous push has no meaning —
	// there is no unauthenticated write this product performs or intends to perform.
	require(id != "", 409, "push_credential_required")
	c := t.one("SELECT * FROM credential_refs WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", id, p.S("tenantId"))
	require(c != nil, 409, "repository_credential_unavailable")
	require(c.S("availability") == "available", 409, "repository_credential_frozen")
	// A personally attributed credential belongs to a person, and it stops being usable when that
	// person leaves. The check is the same one the clone guard makes, for the same reason: a frozen
	// member's credential must not keep publishing on their behalf. Team attribution has no single
	// owner to lose.
	if c.S("attribution") != "team" {
		require(t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", c.S("tenantId"), c.S("ownerUserId")) != nil, 409, "repository_credential_frozen")
	}
	// D3: the credential's scope and the canonical upstream must be equal byte for byte. The scope is
	// written by the credential path in exactly this shape (`normalizeRepositoryScope`, which produces
	// the same string `CanonicalRepository` does for a URL both accept), so the comparison is a byte
	// comparison and not a second, looser reading of one URL.
	//
	// Unlike the clone guard, an empty scope is not exempt. The clone guard exempts it for a reference
	// written before scope was part of the binding, by the deployment-only path; a push has no such
	// history, and an unscoped reference cannot be equal to any upstream, so it is refused. That is a
	// refusal in the safe direction and it is the reading D3 states outright.
	upstream, err := cloneauth.CanonicalRepository(p.S("repositoryUrl"))
	require(err == nil, 409, "repository_credential_scope_mismatch")
	require(c.S("repositoryScope") != "" && c.S("repositoryScope") == upstream, 409, "repository_credential_scope_mismatch")
	// The reference has to permit the work, not merely exist. `clone` is not a substitute: the
	// capability set is the reference's whole authority and it is compared as a set, so a reference
	// that may only read is refused here even though it is perfectly usable for a clone. The set is
	// canonicalized rather than only searched, because an unclassified reference (an empty set, and
	// the column's default is the single capability `clone`) permits nothing and reading it as "no
	// restriction" would turn it into an unrestricted one.
	declared, err := cloneauth.CanonicalCapabilities(credentialCapabilities(c))
	require(err == nil && cloneauth.HasCapability(declared, cloneauth.CapabilityPush), 409, repositoryCredentialCapabilityUnsupported)
	return id, c.N("version"), upstream
}

// pushRequestPublic serves the accepting route: it records one person's approval of one exact write.
//
// The order is the one the push decision's D8 fixes: the plan — tenant, Project, Workspace, the
// credential and its version, the upstream, the ref and both commits — and the idempotency identity
// are persisted before anything is dispatched. Nothing here contacts a remote, and the record is
// complete before the caller returns.
//
// What is returned is the plan as it stands at acceptance, which is why an accepted push is answered
// `202` and not `200`: with an executor installed the write has not run yet and the approval reads
// `dispatching`, and its outcome is read back from the audit rather than returned by the accept. A
// deployment with no executor settles the approval in this same transaction, so there the response is
// already final — the difference is whether anything was left to run, not a difference in the record.
//
// Nothing is dispatched from here, and that is the correction this function carries: the write is
// carried out by `Store.DispatchPushesOnce` on the process's own context. A dispatch started in this
// transaction's caller would run in the request's lifetime, so a client that hung up — or a settlement
// whose transaction failed — would leave the approval `dispatching` with nothing left to read it.
func pushRequestPublic(t *transaction, r *PublicRequest, uid string) Object {
	w, p := pushApproval(t, r, uid)
	requestID := r.Body.S("requestId")
	require(requestID != "" && len(requestID) <= 200, 400, "invalid_push_request")
	ref, oldCommit, newCommit := r.Body.S("ref"), r.Body.S("oldCommit"), r.Body.S("newCommit")
	// The three fields a caller supplies are validated as the exact values the authorization will
	// carry, not as hints. `ValidateRef` requires the qualified `refs/heads/` spelling and rejects the
	// wildcard, force-prefix, whitespace and traversal spellings Git allows in a refname pattern; it
	// also rejects a multi-ref request, because a ref list cannot be one string that passes it.
	// `ValidateCommit` requires a full lowercase id and refuses the null object id, which is how a
	// delete and a create-from-nothing are spelled.
	require(cloneauth.ValidateRef(ref) == nil, 400, "invalid_push_ref")
	require(cloneauth.ValidateCommit(oldCommit) == nil && cloneauth.ValidateCommit(newCommit) == nil, 400, "invalid_push_commit")
	// A push that moves a ref to the commit it already holds is refused rather than approved. Nothing
	// about it is harmful, but an approval is a single-use authorization for one change, and this one
	// authorizes no change: its only use would be spent on nothing, and the audit would record a write
	// that could not happen.
	require(oldCommit != newCommit, 400, "invalid_push_commit")
	// The caller's own request identity, checked before the guard so a retry after a lost reply reads
	// back the decision that was made rather than being re-decided against state that has since moved.
	// The scope is (Workspace, approver, request id): one person's repeat of their own request is a
	// replay, and no other person can reach this row.
	//
	// A replay returns the recorded approval, which is what makes "one request must not produce a second
	// write authorization" hold across a retry: the dispatcher claims rows by state and lease, not by
	// this response, so a replay is not a second dispatch either.
	if existing := t.one("SELECT * FROM push_requests WHERE workspace_id=$1 AND actor_user_id=$2 AND request_id=$3", w.S("id"), uid, requestID); existing != nil {
		// The recorded plan holds the canonical upstream rather than the Project's own spelling of its
		// URL (`https://host/path` without the `.git` suffix the creator may have written), so the
		// submission is canonicalized before it is matched. Comparing the raw URL would refuse every
		// retry as a conflict, because the two spellings are deliberately never equal.
		submitted, err := cloneauth.CanonicalRepository(p.S("repositoryUrl"))
		require(err == nil && existing.S("repositoryUrl") == submitted && existing.S("pinnedRef") == ref &&
			existing.S("oldCommit") == oldCommit && existing.S("newCommit") == newCommit, 409, "idempotency_conflict")
		return pushView(existing)
	}
	credentialID, credentialVersion, upstream := requirePushAccess(t, p)
	// The authorization identifier is minted exactly once, here, and written with the approval. A
	// deployment with no executor never signs a bearer for it, so the identifier names an
	// authorization that was never issued — which is the accurate record of a write that was approved
	// and could not be carried out. A dispatch that runs later writes the authorization *it* minted
	// over it, which is the identifier the Store records consumption under for that attempt.
	id, authorizationID := newID(), newID()
	t.exec("INSERT INTO push_requests(id,tenant_id,project_id,workspace_id,actor_user_id,request_id,repository_url,pinned_ref,old_commit,new_commit,credential_ref_id,credential_ref_version,authorization_id,state)"+
		" VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)",
		id, r.TenantID, p.S("id"), w.S("id"), uid, requestID, upstream, ref, oldCommit, newCommit, credentialID, credentialVersion, authorizationID, pushDispatching)
	if t.store.pushExecutor == nil {
		// The deployment-level settlement, in the same transaction as the approval: a deployment with
		// no executor must never have a `dispatching` row, because such a row would claim a dispatch
		// that does not exist. `push_executor_unavailable` is settled here rather than answered as a
		// refusal for the reason the code's own comment gives — the approval record is the audit, and a
		// refusal would leave none. No attempt has claimed the row, so the settlement is fenced at
		// `pushNoAttempt` rather than at a generation nothing ever wrote.
		settlePushRequest(t, id, pushNoAttempt, pushFailed, pushExecutorUnavailable)
	}
	return pushView(t.one("SELECT * FROM push_requests WHERE id=$1", id))
}

// pushRead serves the read routes: one approval, or the Workspace's approvals in the ascending-by-id
// order the shared page cursor walks.
//
// It requires the same authorization the write does, because the record is the Project's audit of who
// approved what rather than a personal document: a member who could not approve a push may not read
// the approvals either.
func pushRead(t *transaction, r *PublicRequest, uid string) Object {
	w, _ := pushApproval(t, r, uid)
	if r.PushID != "" {
		require(validID(r.PushID), 404, "not_found")
		row := t.one("SELECT * FROM push_requests WHERE id=$1 AND workspace_id=$2 AND tenant_id=$3", r.PushID, w.S("id"), r.TenantID)
		require(row != nil, 404, "not_found")
		return pushView(row)
	}
	listing := page(t, "SELECT * FROM push_requests WHERE workspace_id=$1 AND tenant_id=$2", []any{w.S("id"), r.TenantID}, "id", r)
	rows, _ := listing["items"].([]Object)
	items := make([]Object, 0, len(rows))
	for _, row := range rows {
		items = append(items, pushView(row))
	}
	listing["items"] = items
	return listing
}

// pushView is the public shape of one approval: the audit D9 requires, and nothing else.
//
// Every field is an identifier, an instant or a ref, and the one field that could have been a secret
// is not: `authorizationId` is the name the Credential Service records consumption under, and the
// bearer that identifier belongs to is never persisted, never returned and — in a deployment with no
// executor — never signed at all. Holding the identifier grants nothing, which is what makes it safe
// to show and what makes the audit useful: it is the value that ties this decision to the redemption
// the Store would record.
//
// `observedCommit` is the executor's reading of the remote, carried through unchanged and absent when
// there was none. It is what makes the uncertain outcomes checkable rather than merely declared: a
// reader of `push_attribution_unknown` sees the commit that was already there, and a reader of
// `push_target_conflict` sees the one nobody authorized.
func pushView(row Object) Object {
	return Object{
		"id": row["id"], "tenantId": row["tenantId"], "projectId": row["projectId"], "workspaceId": row["workspaceId"],
		"actorUserId": row["actorUserId"], "requestId": row["requestId"],
		"repository": row["repositoryUrl"], "ref": row["pinnedRef"],
		"oldCommit": row["oldCommit"], "newCommit": row["newCommit"],
		"credentialRefId": row["credentialRefId"], "credentialRefVersion": row["credentialRefVersion"],
		"authorizationId": row["authorizationId"],
		"state":           row["state"], "resultCode": row["resultCode"], "observedCommit": row["observedCommit"],
		"createdAt": row["createdAt"], "settledAt": row["settledAt"], "updatedAt": row["updatedAt"],
	}
}

// settlePushRequest records one approval's outcome inside the caller's transaction.
//
// Two predicates fence the write, and they answer different questions. `state='dispatching'` is the
// approval's: only an approval whose outcome is not yet known settles, and it settles once, so a first
// recorded outcome stands — a later failure cannot talk a success into failing, and a repeated report
// writes nothing. `dispatch_generation` is the *attempt's*: the settlement belongs to the attempt that
// was claimed under that generation, so an answer that arrives after the attempt was replaced — a slow
// executor whose lease lapsed, a reply replayed from a lost connection — writes nothing at all. Without
// the second predicate the first would accept that answer as the current attempt's and record an
// outcome nobody established.
//
// The lease is released by the same statement, because a settled approval has no attempt left to wait
// for; the table's CHECK states the pair as an invariant, so a settlement that forgot would fail here
// rather than leave a settled row reading as one still being dispatched.
//
// It reports whether it wrote, which is the fence's answer: false means this attempt's answer was not
// the current one, so the caller has recorded nothing rather than an outcome that arrived too late.
func settlePushRequest(t *transaction, id string, generation int64, state, code string) bool {
	require(code != "" && state != pushDispatching, 500, "invalid_settlement")
	return t.execRows("UPDATE push_requests SET state=$2,result_code=$3,settled_at=now(),dispatch_lease_expires_at=NULL,updated_at=now()"+
		" WHERE id=$1 AND state='"+pushDispatching+"' AND dispatch_generation=$4", id, state, code, generation) > 0
}

// settlePushObservedRequest records one approval's outcome together with the commit the reconciliation
// saw on the remote.
//
// The observed commit is stored beside the outcome rather than folded into it, because the two answer
// different questions and D7 requires both to survive: `state`/`result_code` say what Cloud concluded,
// and `observedCommit` is the raw observation that conclusion was drawn from. A reader can therefore
// re-derive the verdict from the evidence instead of trusting the label — which is the difference
// between an audit and a summary.
//
// It takes the executor's report rather than Cloud's own reading, since Cloud performs no Git work and
// holds no credential: the only party that ever sees the remote's value is the executor, and this column
// is where its observation becomes durable. The value is unset for outcomes where nothing was observed
// (a refusal before any write, or a reconciliation that could not read), and the migration's CHECK
// accepts NULL for exactly that reason.
//
// The fences are `settlePushRequest`'s, including the generation one: this is the settlement a late
// attempt's answer arrives at, so it is the one the fence has to hold for. It reports whether it wrote,
// for the same reason.
func settlePushObservedRequest(t *transaction, id string, generation int64, state, code, observed string) bool {
	require(code != "" && state != pushDispatching, 500, "invalid_settlement")
	return t.execRows("UPDATE push_requests SET state=$2,result_code=$3,observed_commit=$4,settled_at=now(),dispatch_lease_expires_at=NULL,updated_at=now()"+
		" WHERE id=$1 AND state='"+pushDispatching+"' AND dispatch_generation=$5", id, state, code, nullable(observed), generation) > 0
}

// settlePushFor turns one verdict into the state and code the audit records.
//
// It is the only place the two vocabularies meet, which is what keeps them independently changeable and
// what makes the mapping testable on its own. The three uncertain rows are not a hedge: `target_satisfied`
// says the ref holds the right commit and this approval is not credited with it, `target_conflict` says a
// value nobody authorized is there, and `unknown` says nothing was established — three different facts
// that a two-valued "did it work" would have to record as one.
//
// A verdict this build does not define settles as unknown. That direction is the only safe one: an
// unrecognized verdict is not evidence that nothing was written, and recording it as a failure would be
// Cloud asserting an outcome it was not told.
func settlePushFor(verdict PushVerdict, state, code *string) {
	switch verdict {
	case PushConfirmed:
		*state, *code = pushSucceeded, pushApplied
	case PushTargetSatisfied:
		*state, *code = pushUncertain, pushAttributionUnknown
	case PushNotApplied:
		*state, *code = pushFailed, pushNotApplied
	case PushTargetConflict:
		*state, *code = pushUncertain, pushTargetConflict
	default:
		*state, *code = pushUncertain, pushResultUnknown
	}
}

// pushRefusalCode reads the stable reason code an error carries, and reports whether it carried one.
//
// Two shapes reach here and both are refusals: a `*Fault`, which is what every guard in this package
// raises and which the transaction boundary recovers and hands back, and `codedRefusal`, which is what a
// dispatch boundary wraps its executor's error in. They are separate interfaces — a `*Fault` has a `Code`
// field rather than a method — so a caller reading only one of them would silently classify the other as
// uncoded, and the distinction a reason code exists to draw would be lost at exactly the point the audit
// needs it.
//
// An error carrying an empty code is reported as uncoded. That direction is the safe one: a settlement
// requires a non-empty code, so passing an empty one through would record nothing at all where an
// uncoded failure deliberately leaves the approval `dispatching` for the next pass to reconcile.
//
// The two callers ask the same question of it — may this error be recorded as a concluded failure — and
// both answer no without a code, because a code is the only thing that proves the executor decided the
// write did not land rather than failed to say.
func pushRefusalCode(err error) (string, bool) {
	var fault *Fault
	if errors.As(err, &fault) {
		return fault.Code, fault.Code != ""
	}
	var refusal codedRefusal
	if errors.As(err, &refusal) {
		return refusal.ReasonCode(), refusal.ReasonCode() != ""
	}
	return "", false
}
