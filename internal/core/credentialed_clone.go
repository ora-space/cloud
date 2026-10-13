package core

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
)

// CredentialedClone names one clone Cloud executes itself, on behalf of a Project that names a
// credential.
//
// It is the whole input of the executor boundary and nothing more: Cloud has already decided that the
// clone is allowed, so what travels is the scope the decision was made in, not the evidence for it.
type CredentialedClone struct {
	// AuthorizationID is the identifier the Credential Service records consumption under. Cloud mints
	// it here because the Store turns it into a file name and D10 makes the consumption record the
	// Store's.
	AuthorizationID string
	TenantID        string
	OwnerUserID     string
	ProjectID       string
	WorkspaceID     string
	ExecutionID     string
	// CredentialID and CredentialVersion pin the exact material. The version is what makes "which
	// credential did this clone use" answerable after a rotation.
	CredentialID      string
	CredentialVersion int64
	// Repository is the canonical repository identity, and Branch the requested ref.
	Repository string
	Branch     string
	// Capabilities is the set of repository operations this clone is authorized for, in canonical
	// order.
	//
	// It travels with the admission rather than being decided at dispatch time, because it is read
	// from the credential the clone resolves to and is therefore part of what admission decided. The
	// continuity decision's D4 requires the execution plan to fix "引用身份、版本、仓库与所需能力"; without
	// it here, the plan would name a credential and a repository and leave what may be done with them
	// to whatever the executor happens to implement.
	Capabilities []string
	// Runtime fences the authorization to the runtime the clone was admitted under.
	Runtime CredentialedCloneRuntime
}

// CredentialedCloneRuntime is the fencing set an authorization is bound to.
//
// RuntimeGeneration and ControlEpoch may be zero, which means the clone was admitted before that
// value existed for this Workspace: a Workspace clone is dispatched as soon as the Node is ready,
// which is before any runtime control has been established. Zero is a real value here rather than a
// missing one, and both sides compare it for equality like any other.
type CredentialedCloneRuntime struct {
	NodeID            string
	NodeInstanceID    string
	NodeIncarnationID string
	RuntimeGeneration int64
	ControlEpoch      int64
}

// CredentialedCloneOutcome is what a completed clone produced.
type CredentialedCloneOutcome struct {
	// Directory is the checkout the workload reads.
	Directory string
	// Commit is the revision that was checked out.
	Commit string
	// CredentialVersion is the material version the clone actually used, which is not necessarily the
	// version Cloud asked for: the Credential Service decides which retained version is available.
	CredentialVersion int64
}

// CredentialedCloner mints one single-use authorization and dispatches the clone it authorizes.
//
// It is an interface rather than a concrete client so that admission and registration can be tested
// without a network, and so that the executor stays replaceable: what Cloud depends on is "a clone
// happened and here is its commit", not on how the credential-executing component is built.
type CredentialedCloner interface {
	Execute(ctx context.Context, clone CredentialedClone) (CredentialedCloneOutcome, error)
}

// credentialedCloneRefusal is the shape a cloner's error takes when it is a refusal with a stable
// reason code rather than an infrastructure failure.
type credentialedCloneRefusal interface {
	ReasonCode() string
}

// CloneDispatcher is the production implementation of the executor boundary: it signs one
// authorization with Cloud's Ed25519 key and hands the job to the credentialed-clone executor.
//
// Cloud is the dispatcher rather than the Node because the authorization is the ability to obtain a
// user's repository credential, and Cloud is the only party that decides a clone is allowed. Nothing
// about the credential travels back this way: the token goes from the Credential Service to the
// executor, and Cloud learns only the commit.
type CloneDispatcher struct {
	// Client is the executor's mutually authenticated endpoint.
	Client *cloneexec.Client
	// SigningKey is Cloud's Ed25519 authorization key. It is the ability to authorize, and it is held
	// nowhere else: the Credential Service and the executor hold only the public half.
	SigningKey ed25519.PrivateKey
	// Lifetime is how long one authorization lives. It is bounded by the decision's ceiling and is
	// also what limits the clone, because an authorization that expires mid-clone fails a clone that
	// was otherwise permitted.
	Lifetime time.Duration
	// Now supplies the clock, so issuance is deterministic in tests.
	Now func() time.Time
}

// Execute issues one authorization for the clone and dispatches it.
//
// The failure it returns is either the executor's own coded refusal or a transport error, and the two
// are deliberately not merged: a refusal is an outcome of this clone, while an unreachable executor
// leaves the clone's outcome unknown and must be reported as such.
//
//nolint:gocritic // By value on purpose: the admitted clone is a snapshot of what admission decided, and signing a copy is what keeps that decision and the signed bytes the same one.
func (d *CloneDispatcher) Execute(ctx context.Context, clone CredentialedClone) (CredentialedCloneOutcome, error) {
	if d.Client == nil || len(d.SigningKey) != ed25519.PrivateKeySize {
		return CredentialedCloneOutcome{}, errors.New("clone dispatch is not configured")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	issued := now().UTC()
	authorization, err := cloneauth.Issue(d.SigningKey, cloneauth.Binding{
		AuthorizationID:   clone.AuthorizationID,
		Operation:         cloneauth.OperationClone,
		TenantID:          clone.TenantID,
		OwnerUserID:       clone.OwnerUserID,
		ProjectID:         clone.ProjectID,
		WorkspaceID:       clone.WorkspaceID,
		ExecutionID:       clone.ExecutionID,
		CredentialID:      clone.CredentialID,
		CredentialVersion: clone.CredentialVersion,
		Repository:        clone.Repository,
		Capabilities:      clone.Capabilities,
		NodeID:            clone.Runtime.NodeID,
		NodeInstanceID:    clone.Runtime.NodeInstanceID,
		NodeIncarnationID: clone.Runtime.NodeIncarnationID,
		RuntimeGeneration: clone.Runtime.RuntimeGeneration,
		ControlEpoch:      clone.Runtime.ControlEpoch,
		IssuedAt:          issued,
		ExpiresAt:         issued.Add(d.Lifetime),
	})
	if err != nil {
		// A binding this package will not sign is a programming error rather than a clone outcome: the
		// values come from Cloud's own admission, not from a caller.
		return CredentialedCloneOutcome{}, err
	}
	result, err := d.Client.Dispatch(ctx, cloneexec.Job{
		ExecutionID:       clone.ExecutionID,
		Repository:        clone.Repository,
		Branch:            clone.Branch,
		Capabilities:      clone.Capabilities,
		CredentialID:      clone.CredentialID,
		CredentialVersion: clone.CredentialVersion,
		Authorization:     authorization,
		Runtime: cloneexec.Runtime{
			NodeID:            clone.Runtime.NodeID,
			NodeInstanceID:    clone.Runtime.NodeInstanceID,
			NodeIncarnationID: clone.Runtime.NodeIncarnationID,
			RuntimeGeneration: clone.Runtime.RuntimeGeneration,
			ControlEpoch:      clone.Runtime.ControlEpoch,
		},
	})
	if err != nil {
		return CredentialedCloneOutcome{}, err
	}
	return CredentialedCloneOutcome{
		Directory:         result.Directory,
		Commit:            result.Commit,
		CredentialVersion: result.CredentialVersion,
	}, nil
}

// SetCredentialedCloner installs the credentialed-clone boundary, or removes it when c is nil.
//
// Installing it is what turns a credentialed clone from a refusal into an executed clone, so the call
// is a deployment decision and is made once, at startup, from configuration. Leaving it unset — the
// default — keeps every credentialed clone refused.
func (s *Store) SetCredentialedCloner(c CredentialedCloner) { s.credentialedCloner = c }

// credentialedCloneResult is the outcome Cloud records on the clone execution it registered.
//
// It is carried on the control request rather than through a Store field because it belongs to one
// call: two concurrent dispatches must not be able to see each other's outcome.
type credentialedCloneResult struct {
	outcome string
	commit  string
}

// cloneReadyResult and cloneFailedResult are the two outcomes the execution registry accepts. They are
// the same two a Node reports, so the Controller's own result handling is unchanged: a credentialed
// clone is a clone the Controller never has to dispatch.
const (
	cloneReadyResult  = "clone_ready"
	cloneFailedResult = "clone_failed"
)

// admitCredentialedClone decides whether one `clone_dispatch` call is a credentialed clone this
// deployment executes itself, and returns everything the dispatch needs.
//
// It runs in its own short transaction, before the clone, for a reason that is about correctness
// rather than tidiness: the execution must be registered with its result already written, in one
// transaction, or the Controller's dispatch loop could observe a registered execution with no result
// and hand it to a Node that holds no credential at all. Registering first and updating later would
// leave exactly that window open for as long as a clone takes.
//
// A nil result with a nil error means "not a credentialed clone Cloud executes" — an anonymous Project,
// a legacy clone request, a deployment that named the internal Git Server and therefore serves the
// content from a published mirror, or a deployment with no executor configured — and the caller then
// takes the ordinary path unchanged.
func (s *Store) admitCredentialedClone(ctx context.Context, r *ControlRequest) (*CredentialedClone, error) {
	var admitted *CredentialedClone
	_, err := s.transact(ctx, func(t *transaction) Object {
		operation, execution, node := r.Body.S("operationId"), r.Body.S("executionId"), r.Body.S("nodeId")
		if !validID(operation) || execution == "" || node == "" {
			return nil
		}
		o := t.one("SELECT * FROM operations WHERE id=$1 AND workspace_id IS NOT NULL", operation)
		if o == nil {
			return nil
		}
		// Admission runs here and again in the registering transaction, which stays authoritative. The
		// duplication buys one thing: an authorization is never minted for a clone Cloud would refuse.
		requireRepositoryAccess(t, o.S("projectId"))
		credential := t.admittedCredential
		if credential == nil {
			return nil
		}
		// D1 of the content-transport decision: the content channel is exactly one. A deployment that
		// named the internal Git Server serves a credentialed Workspace's clone from the published mirror,
		// which the Node clones with the ordinary anonymous HTTPS clone the server authorizes by source
		// container — so this is not a clone Cloud executes, and declining here is what keeps the two
		// channels from both being live for one Workspace.
		//
		// The decline is placed after the guard rather than before it, so every refusal the guard owns —
		// an unavailable or frozen reference, a scope mismatch, a deployment with neither channel — is
		// still given with its own code, and the ordinary dispatch below repeats the guard on its own.
		// Without this, a mirror deployment would clone the upstream itself and never touch the mirror,
		// which would leave the ready gate and the pinned baseline enforcing a channel nothing used.
		if t.store.mirrorEndpoint != nil {
			return nil
		}
		wid := o.S("workspaceId")
		// The repository is the Project's, and the Project is the only place that names it: `workspaces`
		// carries the requested ref, never a URL. Reading one off the Workspace row would silently yield
		// the empty string — the column does not exist — and refuse every credentialed clone. The JOIN is
		// also what keeps the URL in one column instead of a second copy that could drift from it.
		// The projection is explicit because the Workspace and the Project share column names: `SELECT *`
		// across the JOIN would leave `repository_url`, `version`, `tenant_id` and the rest ambiguous to
		// read back. Only the three values this admission uses are selected, and every one of them is
		// named, so a field added to either table cannot silently change what is read here.
		w := t.one("SELECT w.requested_ref,w.runtime_generation,p.repository_url FROM workspaces w JOIN projects p ON p.id=w.project_id WHERE w.id=$1", wid)
		require(w != nil, 404, "not_found")
		// The scope is compared through the same normalizer, over the same column, that
		// `requireRepositoryAccess` compared one step earlier. That makes this comparison provably
		// redundant within one transaction — the operation's `(workspace_id, project_id)` pair is held
		// to a workspaces row by a composite foreign key, so the JOIN reaches the very row the guard
		// read — and it is kept anyway, because this is the site that mints the binding: the repository
		// an authorization will name is tied to the credential's scope here as well as there, so a later
		// change to one of the two cannot leave the other minting a wider authorization. The comparison
		// must not become a second, more permissive reading of one URL; it is the same reading.
		//
		// An empty scope is the deployment-managed reference that guard and `createProject` both exempt
		// (D7). It has no scope to compare, and it is spelled the same way in all three places.
		if credential.Repository != "" {
			scope, ok := normalizeRepositoryScope(w.S("repositoryUrl"))
			require(ok && scope == credential.Repository, 409, "repository_credential_scope_mismatch")
		}
		// The canonical repository identity the authorization binds and the executor re-derives is
		// generated separately, because it is a different value with a different lifecycle: the scope is
		// durable state written when the credential was created, while this is a per-dispatch identity.
		// It is never derived from the scope, which would let one stored string decide both.
		//
		// A URL with no canonical form is refused here rather than admitted: an authorization binding the
		// empty repository would name no repository at all.
		repository, err := cloneauth.CanonicalRepository(w.S("repositoryUrl"))
		require(err == nil, 409, "repository_credential_scope_mismatch")
		n := currentNode(t, wid)
		require(n.S("nodeId") == node, 409, "dispatch_conflict")
		c := t.one("SELECT * FROM runtime_controls WHERE workspace_id=$1", wid)
		require(c != nil, 409, "runtime_control_reconciliation_required")
		admitted = &CredentialedClone{
			AuthorizationID:   newID(),
			TenantID:          o.S("tenantId"),
			OwnerUserID:       credential.OwnerUserID,
			ProjectID:         o.S("projectId"),
			WorkspaceID:       wid,
			ExecutionID:       execution,
			CredentialID:      credential.CredentialID,
			CredentialVersion: credential.CredentialVersion,
			Repository:        repository,
			Branch:            w.S("requestedRef"),
			Capabilities:      credential.Capabilities,
			Runtime: CredentialedCloneRuntime{
				NodeID:            n.S("nodeId"),
				NodeInstanceID:    n.S("id"),
				NodeIncarnationID: n.S("nodeIncarnationId"),
				RuntimeGeneration: w.N("runtimeGeneration"),
				ControlEpoch:      c.N("controlEpoch"),
			},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return admitted, nil
}

// registerCredentialedClone runs the clone and then registers it with its result already written.
//
// The order is fixed by the same argument as admitCredentialedClone: the clone happens with no
// transaction held, and the registration happens with the outcome already in hand, so the Controller
// never observes the execution in a state that would make it dispatch the work to a Node.
func (s *Store) registerCredentialedClone(ctx context.Context, r *ControlRequest, clone *CredentialedClone) (Object, error) {
	outcome, cloneErr := s.credentialedCloner.Execute(ctx, *clone)
	result := &credentialedCloneResult{outcome: cloneReadyResult, commit: outcome.Commit}
	if cloneErr != nil {
		// A clone that did not produce a revision is registered as failed rather than left
		// unregistered: an execution the Controller cannot see would leave the operation waiting
		// forever, and the failure is a real fact about this execution.
		result = &credentialedCloneResult{outcome: cloneFailedResult}
	}
	copyRequest := *r
	copyRequest.credentialedResult = result
	out, err := s.control(ctx, &copyRequest)
	if err != nil {
		return nil, err
	}
	if cloneErr != nil {
		return nil, &Fault{Status: 409, Code: credentialedCloneReasonCode(cloneErr)}
	}
	return out, nil
}

// credentialedCloneReasonCode names a clone failure in a form the Controller can act on, falling back
// to the executor's generic code so a transport failure is never reported as a refusal.
func credentialedCloneReasonCode(err error) string {
	var refusal credentialedCloneRefusal
	if errors.As(err, &refusal) {
		return refusal.ReasonCode()
	}
	return cloneexec.ReasonCodeUnavailable
}

// recordCredentialedCloneResult writes the outcome Cloud's own clone produced onto the execution it
// just registered.
//
// It runs inside the registering transaction, so the row exists and is complete before the Controller
// can see it. The shape is the one a Node reports, including the `node` field the registry checks the
// reporting identity against, because the registry's contract is that a result names the Node it came
// from — here, the Node the clone was admitted for.
func recordCredentialedCloneResult(t *transaction, e Object, result *credentialedCloneResult) Object {
	record := Object{"outcome": result.outcome, "node": Object{"nodeId": e.S("nodeId")}}
	if result.outcome == cloneReadyResult {
		record["commit"] = result.commit
	}
	// The registry's contract is that a result is written once: a repeated, identical fact is an
	// idempotent no-op, and a second, different fact conflicts and leaves the recorded one untouched
	// (cloneResult enforces exactly this for a Node-reported result). A dispatch replayed after a lost
	// reply reaches here with the execution row already carrying the first clone's result, because the
	// `clone_dispatch` registration is itself idempotent on `(operation, execution, input)`. Writing
	// unconditionally in that case would replace the commit Cloud already recorded with a second
	// clone's outcome, so the Workspace's registered baseline could change under a retry. The identity
	// a replay is keyed on is the execution row, so the comparison is against it.
	if previous := e.O("result"); len(previous) > 0 {
		require(jsonText(previous) == jsonText(record), 409, "result_conflict")
		return e
	}
	t.exec("UPDATE clone_executions SET result=$2,updated_at=now() WHERE execution_id=$1", e.S("executionId"), jsonText(record))
	return t.one("SELECT * FROM clone_executions WHERE execution_id=$1", e.S("executionId"))
}
