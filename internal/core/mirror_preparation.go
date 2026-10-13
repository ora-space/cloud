package core

import (
	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// mirrorNotReady is the stable code Cloud answers with when a credentialed Workspace is about to enter
// `clone` and its Project's mirror is not published, or is published but no longer agrees with live
// state. It is deliberately retryable: a preparation that is merely late becomes ready, and the
// Controller's next round should find the gate open rather than the Operation settled.
const mirrorNotReady = "mirror_not_ready"

// cloneInput is what one Operation's Workspace may be cloned from.
//
// It is one value with one derivation, because the three places that need it must not be able to
// disagree: the gate on the `node` -> `clone` transition, the snapshot the Controller builds the
// clone from, and the dispatch that registers what the Controller actually chose. A second
// derivation anywhere in that chain would be a way for a Controller to clone something the gate never
// admitted, and the invariants below would hold only as long as the copies stayed equal.
type cloneInput struct {
	// repository is the URL the Node is told to clone. For an anonymous Project it is the Project's
	// own upstream; for a credentialed one it is the internal mirror's address, and the upstream
	// appears in no clone input at all (D7).
	repository string
	// branch is the branch the Node is told to check out. It is the mirror's one branch for a mirrored
	// clone, never the Workspace's requested ref: the mirror holds one branch at one commit, and the
	// checkout a Workspace gets is that commit rather than whatever a branch points at today.
	branch string
	// commit is the commit the mirror pins. It is empty for an anonymous Project, whose baseline is
	// whatever the Node's own clone produces.
	commit string
}

// workspaceCloneInput is the Project's upstream and the Workspace's requested ref.
//
// It is the input every Controller derived before the internal Git Server existed, and it is still the
// one a clone Cloud executes itself is registered with: the credentialed-clone executor clones the
// Project's upstream, with the credential, so that is the input its registration must match. It is also
// what an anonymous Project's clone has always been.
//
// The refusal code is empty when the input exists and `not_found` when the rows it reads are gone.
// Nothing about a mirror is wrong in that case, which is why it does not answer with a mirror code.
func (s *Store) workspaceCloneInput(t *transaction, o Object, wid string) (input cloneInput, code string) {
	p := t.one("SELECT * FROM projects WHERE id=$1", o.S("projectId"))
	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	if p == nil || w == nil {
		return cloneInput{}, "not_found"
	}
	return cloneInput{repository: p.S("repositoryUrl"), branch: w.S("requestedRef")}, ""
}

// cloneInputFor derives the clone input the channel serving this Operation's Workspace must be given,
// and reports the refusal to use when it has none to offer.
//
// An anonymous Project keeps the existing real execution path verbatim: the Project's upstream
// repository and the Workspace's requested ref, which is what `dispatch_conflict` has always compared
// a dispatch against. A credentialed Project has exactly one admissible input — a published mirror
// that still agrees with live state, addressed through the deployment's Git Server — and no fallback:
// `mirror_not_ready` is what the caller answers with, and the upstream is deliberately not offered as a
// substitute, because the Node holds no credential for it and the decision makes the internal mirror
// the single content channel.
//
// Readiness is re-established here rather than trusted. The preparation's own `ready` state is not
// evidence that its bindings still hold: a Project can be rebound to another credential or another
// repository, a Workspace can be re-requested against another ref, and the runtime generation can
// move, all while a preparation sits ready. Each of those is compared against live state in this
// transaction.
//
// The credential *version* is deliberately not compared here. The authoritative version is the one
// `requireRepositoryAccess` resolves from the live reference, which the credential-executor guard
// records on the transaction; this function is also called from the snapshot, where that guard must
// not run because it raises. The version therefore stays with the gate that has already run the
// guard, and the dispatch — the one place a clone is actually registered — runs the guard again
// immediately before comparing the input.
func (s *Store) cloneInputFor(t *transaction, o Object, wid string) (input cloneInput, code string) {
	p := t.one("SELECT * FROM projects WHERE id=$1", o.S("projectId"))
	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	if p == nil || w == nil {
		return cloneInput{}, "not_found"
	}
	if p.S("credentialRefId") == "" {
		return cloneInput{repository: p.S("repositoryUrl"), branch: w.S("requestedRef")}, ""
	}
	if s.mirrorEndpoint == nil {
		// The mirror may well be published; nobody told Cloud where a sandbox reads it. That is a
		// deployment that must not clone rather than one that clones from the upstream.
		return cloneInput{}, mirrorNotReady
	}
	// The preparation is identified by the Operation, not by the Project: one Operation carries one
	// preparation, which is what makes a late result from a previous attempt identifiable.
	m := t.one("SELECT * FROM mirror_preparations WHERE operation_id=$1", o.S("id"))
	if m == nil || m.S("state") != "ready" {
		return cloneInput{}, mirrorNotReady
	}
	// Identity: the preparation must belong to this tenant, Project and Workspace. A row that matches on
	// operation yet names another Workspace would let one Workspace's published content authorize
	// another's clone.
	if m.S("tenantId") != p.S("tenantId") || m.S("projectId") != p.S("id") || m.S("workspaceId") != wid {
		return cloneInput{}, mirrorNotReady
	}
	// Upstream identity is the Project's real repository, never the internal mirror address. A Project
	// rebound to another repository since admission must re-prepare rather than clone the old content —
	// and this comparison is also what keeps the address below bound to the repository the mirror is
	// keyed by.
	if m.S("repositoryUrl") != p.S("repositoryUrl") {
		return cloneInput{}, mirrorNotReady
	}
	// The pinned ref is the qualification of the Workspace's requested branch, in the one spelling the
	// authorization signs.
	if m.S("pinnedRef") != "refs/heads/"+w.S("requestedRef") {
		return cloneInput{}, mirrorNotReady
	}
	// Fencing: a preparation admitted under an older runtime generation must not settle a Workspace that
	// has moved on.
	if m.N("runtimeGeneration") != w.N("runtimeGeneration") {
		return cloneInput{}, mirrorNotReady
	}
	if m.S("credentialRefId") != p.S("credentialRefId") {
		return cloneInput{}, mirrorNotReady
	}
	// Evidence that a published artifact exists at all. `ready` without these is already refused by the
	// schema (0034, mirror_preparation_ready_has_evidence); checking here keeps the derivation from
	// depending on a constraint it cannot see.
	if m.S("commitId") == "" || m.N("mirrorGeneration") <= 0 {
		return cloneInput{}, mirrorNotReady
	}
	// The key is the mirror's identity, not a name for its address: the same three facts the Git Server
	// recomputes when it authorizes the read (`git_read.go`). The commit is the preparation's own
	// evidence, and the gate that admits the transition pins it as the Workspace's baseline, so the
	// address Cloud records and the key the server derives from that baseline name one mirror.
	repository, err := cloneauth.CanonicalRepository(p.S("repositoryUrl"))
	if err != nil {
		return cloneInput{}, mirrorNotReady
	}
	key := mirror.Key{ProjectID: p.S("id"), Repository: repository, Commit: m.S("commitId")}
	address, err := s.mirrorEndpoint.CloneAddress(key)
	if err != nil {
		return cloneInput{}, mirrorNotReady
	}
	return cloneInput{repository: address, branch: mirrorpublish.MirrorBranch, commit: m.S("commitId")}, ""
}

// projectMirrorInput redirects one snapshot's clone input at the internal mirror.
//
// It writes the derivation's answer onto the snapshot and changes no row: `p` is the Project the
// Controller will read `repository_url` from, and the entry in `workspaces` whose id is this
// Operation's workspace is the one the Controller will read `requested_ref` from. Those two fields,
// and only those two, are what a Controller turns into a clone (`dispatch_clone`). Everything else in
// the snapshot — the Project's real upstream in the database, the credential binding, the mirror key,
// the read authorization — keeps naming the upstream.
//
// It is a no-op whenever there is no mirror to point at: an anonymous Project, a preparation that is
// not ready, a binding that no longer agrees with live state, or a deployment that named no Git
// Server. A Controller that receives an unredirected snapshot for a credentialed Workspace therefore
// dispatches an input naming the upstream, and `workspaceCloneDispatch` refuses it with
// `mirror_not_ready` — the two ends of the chain agree because they run the same derivation.
func (s *Store) projectMirrorInput(t *transaction, o, p Object, workspaces []Object) {
	wid := o.S("workspaceId")
	if p == nil || wid == "" {
		return
	}
	input, code := s.cloneInputFor(t, o, wid)
	if code != "" || input.commit == "" {
		// Nothing to redirect: either there is no mirror in play, or there is one and it cannot be
		// addressed — which the transition gate and the dispatch both refuse.
		return
	}
	for _, w := range workspaces {
		if w.S("id") == wid {
			w["requestedRef"] = input.branch
		}
	}
	p["repositoryUrl"] = input.repository
}

// requireMirrorPrepared is the mirror gate on the `node` -> `clone` transition. A Workspace whose
// Project names a credential may not begin cloning until that Project's mirror has been published and
// every binding recorded at admission still agrees with live state.
//
// The order inside this function is the point. An anonymous Project returns immediately, so the
// existing public-clone path is untouched. A credentialed Project then goes through the same guard a
// credentialed clone already goes through, *before* anything is said about the mirror: that guard is
// the single place a credentialed clone can be admitted, and it is where a refusal about the
// deployment or the reference keeps its own code rather than being reported as a mirror problem. The
// mirror gate therefore sits behind it and only becomes reachable once an executor is installed and
// the reference actually permits the work.
//
// Which of that guard's refusals are durable and which are retryable is the guard's classification,
// not this gate's: `terminalCredentialRefusals` is the single list, and it is deliberately shorter
// than "everything the reference can go wrong with" — a frozen reference is retryable, because an
// owner can reconnect it. This gate must not turn any of them into `mirror_not_ready`, which would
// name the mirror as the obstacle when the cause is elsewhere.
//
// What this function deliberately does not do is prepare anything. Admitting a preparation and
// dispatching it to the executor take minutes and belong to a caller outside this transaction; the
// execution contract forbids holding the control plane's lock across that work. This gate only decides
// whether the transition may happen now.
//
// It also writes the Workspace's baseline, and that write is part of the same decision rather than an
// extra effect. D7 pins the clone to the commit the mirror holds, and a Workspace has two states that
// must agree about which commit that is: the mirror a sandbox reads is keyed partly by the
// Workspace's `base_commit_id`, so a baseline written only after a successful clone would mean the
// clone itself could not be authorized — every read of the mirror would be refused as a read of a
// mirror that is not the Workspace's. Pinning it here, in the same transaction that admits the
// transition, is what makes the mirror address Cloud records and the mirror the Git Server serves one
// and the same. `advanceClone` then requires the Node's clone to land on exactly this commit.
func (s *Store) requireMirrorPrepared(t *transaction, o Object, wid string) {
	pid := o.S("projectId")
	p := t.one("SELECT * FROM projects WHERE id=$1", pid)
	require(p != nil, 404, "not_found")
	if p.S("credentialRefId") == "" {
		return // Anonymous public Git: the existing real execution path, unchanged.
	}
	requireRepositoryAccess(t, pid)

	// Credential identity and version, compared against the version the guard above just validated
	// rather than against the one the preparation recorded. A rotation between admission and now means
	// the mirror was built from a fetch authorized by a credential that no longer applies, so the
	// transition must not happen. This is the one comparison the derivation below leaves out, because
	// the live version is only available here.
	m := t.one("SELECT * FROM mirror_preparations WHERE operation_id=$1", o.S("id"))
	require(t.admittedCredential != nil &&
		m.S("credentialRefId") == p.S("credentialRefId") &&
		m.N("credentialRefVersion") == t.admittedCredential.CredentialVersion, 409, mirrorNotReady)

	input, code := s.cloneInputFor(t, o, wid)
	require(code == "", 409, code)

	// The baseline the read authorization and every work dispatch are derived from. It is written
	// under the same fence the transition is: the generation the Workspace runs was just compared to
	// the preparation's, so an Operation that settles a Workspace that has moved on cannot land here.
	t.exec("UPDATE workspaces SET base_commit_id=$2,version=version+1 WHERE id=$1", wid, input.commit)
}
