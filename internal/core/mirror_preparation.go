package core

// mirrorNotReady is the stable code Cloud answers with when a credentialed Workspace is about to enter
// `clone` and its Project's mirror is not published, or is published but no longer agrees with live
// state. It is deliberately retryable: a preparation that is merely late becomes ready, and the
// Controller's next round should find the gate open rather than the Operation settled.
const mirrorNotReady = "mirror_not_ready"

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
// Past that guard, readiness is re-established rather than trusted. The preparation's own `ready` state
// is not evidence that the bindings still hold: a Project can be rebound to another credential, a
// credential can be rotated or revoked, a Workspace can be re-requested against another ref, and the
// runtime generation can move, all while a preparation sits ready. Each of those is compared against
// live state here, in this transaction, so the transition to `clone` happens against facts rather than
// against a record that was true once.
//
// What this function deliberately does not do is prepare anything. Admitting a preparation and
// dispatching it to the executor take minutes and belong to a caller outside this transaction; the
// execution contract forbids holding the control plane's lock across that work. This gate only decides
// whether the transition may happen now.
func requireMirrorPrepared(t *transaction, o Object, wid string) {
	pid := o.S("projectId")
	p := t.one("SELECT * FROM projects WHERE id=$1", pid)
	require(p != nil, 404, "not_found")
	if p.S("credentialRefId") == "" {
		return // Anonymous public Git: the existing real execution path, unchanged.
	}
	requireRepositoryAccess(t, pid)

	// The preparation is identified by the Operation, not by the Project: one Operation carries one
	// preparation, which is what makes a late result from a previous attempt identifiable.
	m := t.one("SELECT * FROM mirror_preparations WHERE operation_id=$1", o.S("id"))
	require(m != nil && m.S("state") == "ready", 409, mirrorNotReady)

	w := t.one("SELECT * FROM workspaces WHERE id=$1", wid)
	require(w != nil, 404, "not_found")

	// Identity: the preparation must belong to this tenant, Project and Workspace. A row that matches on
	// operation yet names another Workspace would let one Workspace's published content authorize
	// another's clone.
	require(m.S("tenantId") == p.S("tenantId") && m.S("projectId") == pid && m.S("workspaceId") == wid, 409, mirrorNotReady)

	// Upstream identity is the Project's real repository, never the internal mirror address. A Project
	// rebound to another repository since admission must re-prepare rather than clone the old content.
	require(m.S("repositoryUrl") == p.S("repositoryUrl"), 409, mirrorNotReady)

	// The pinned ref is the qualification of the Workspace's requested branch, in the one spelling the
	// authorization signs.
	require(m.S("pinnedRef") == "refs/heads/"+w.S("requestedRef"), 409, mirrorNotReady)

	// Fencing: a preparation admitted under an older runtime generation must not settle a Workspace that
	// has moved on.
	require(m.N("runtimeGeneration") == w.N("runtimeGeneration"), 409, mirrorNotReady)

	// Credential identity and version, compared against the version the guard above just validated
	// rather than against the one the preparation recorded. The preparation captured a version at
	// admission; a rotation between then and now means the mirror was built from a fetch authorized by a
	// credential that no longer applies, so the transition must not happen.
	require(t.admittedCredential != nil &&
		m.S("credentialRefId") == p.S("credentialRefId") &&
		m.N("credentialRefVersion") == t.admittedCredential.CredentialVersion, 409, mirrorNotReady)

	// Evidence that a published artifact exists at all. `ready` without these is already refused by the
	// schema (0034, mirror_preparation_ready_has_evidence); checking here keeps the gate from depending
	// on a constraint it cannot see, and gives the transition its own reason.
	require(m.S("commitId") != "" && m.N("mirrorGeneration") > 0, 409, mirrorNotReady)
}
