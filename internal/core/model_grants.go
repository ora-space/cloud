package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// ModelRuntimeScope is derived from a dedicated verified model-access mTLS certificate, never
// from a JSON body. Generation fences a certificate from a replaced runtime.
type ModelRuntimeScope struct {
	TenantID    string
	WorkspaceID string
	Generation  int64
}

// ModelGrant is the private model-gateway policy result. Ciphertext is opaque to Cloud and never
// appears on a public route. The service decrypts it only after durable authorization succeeds.
type ModelGrant struct {
	ID, BindingID, RunID, ExecutionID, WorkspaceID, NodeID, CredentialID, CredentialKeyID, Protocol, BaseURL, AuthMode string
	RuntimeGeneration, ConnectionVersion                                                                               int64
	Model                                                                                                              ModelDefinition
	Ciphertext                                                                                                         []byte
	ExpiresAt                                                                                                          time.Time
}

func liveModelBinding(t *transaction, scope ModelRuntimeScope, bindingID, executionID string) Object {
	require(validID(bindingID) && validID(scope.TenantID) && validID(scope.WorkspaceID) && scope.Generation > 0, 403, "model_scope_required")
	// The binding's owner discriminates the branch: an IssueRun binding checks the run's Thread
	// lifecycle, a WorkflowRun binding checks the run's execution status — both keep every
	// connection, user, tenant, membership, workspace and runtime gate (mappings decision D4:
	// the grant's binding must belong to the session execution's run).
	b := liveIssueRunModelBinding(t, scope, bindingID, executionID)
	if b == nil {
		b = liveWorkflowModelBinding(t, scope, bindingID, executionID)
	}
	require(b != nil, 403, "model_grant_revoked")
	if b.S("runId") == "" {
		b["runId"] = b.S("workflowRunId")
	}
	requireNoForceStop(t, scope.WorkspaceID)
	n := currentNode(t, scope.WorkspaceID)
	require(n.S("nodeId") == b.S("nodeId") && n.B("modelProxy"), 403, "model_grant_revoked")
	c := runtimeControl(t, scope.WorkspaceID)
	require(c.S("state") == "maintenance" && c.S("maintenanceRunId") == b.S("runId") && c.B("bindingConfirmed") && c.S("boundSandboxId") == n.S("sandboxInstanceId"), 403, "model_grant_revoked")
	work := t.one("SELECT target FROM execution_work WHERE id=$1", b.S("workId"))
	require(work != nil && work.O("target").S("sandboxInstanceId") == n.S("sandboxInstanceId"), 403, "model_grant_revoked")
	// This denial is distinguishable only after the certificate and every current authority gate
	// above passed. It lets Node reconcile the durable EndSession command without reclassifying
	// a wrong scope, disabled account or cleared credential as a legitimate shutdown request.
	if b.S("threadState") == "ending" || b.S("cancelRequestedAt") != "" {
		reject(403, "model_session_ending")
	}
	return b
}

// liveIssueRunModelBinding resolves a binding against a live Agent IssueRun session execution,
// with the full Thread lifecycle gates (issue-run-only vocabulary).
func liveIssueRunModelBinding(t *transaction, scope ModelRuntimeScope, bindingID, executionID string) Object {
	return t.one(`SELECT b.*, e.execution_id,e.node_id,e.work_id,w.id AS workspace_id,w.runtime_generation,
	 r.thread_state,r.cancel_requested_at
	 FROM run_model_bindings b JOIN issue_runs r ON r.id=b.run_id
	 JOIN issues issue ON issue.id=r.issue_id AND issue.deleted_at IS NULL
	 JOIN node_executions e ON e.operation_id=r.id AND e.kind='agent_session'
	 JOIN workspaces w ON w.id=e.workspace_id AND w.issue_run_id=r.id
	 JOIN personal_model_connections c ON c.id=b.connection_id AND c.user_id=b.user_id
	 JOIN users u ON u.id=b.user_id
	 JOIN tenants tenant ON tenant.id=r.tenant_id
	 JOIN tenant_memberships member ON member.tenant_id=r.tenant_id AND member.user_id=b.user_id
	 WHERE b.id=$1 AND e.execution_id=$2 AND r.tenant_id=$3 AND w.id=$4 AND w.runtime_generation=$5
	 AND b.revoked_at IS NULL
	 AND r.deleted_at IS NULL AND r.phase IN ('starting','running')
	 AND r.thread_state IN ('pending','active','idle','ending') AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
	 AND w.deleted_at IS NULL AND w.desired_state='running' AND w.observed_state='ready' AND w.admission_open
	 AND c.deleted_at IS NULL AND c.enabled AND c.credential_id IS NOT NULL
	 AND u.deleted_at IS NULL AND u.status='active' AND tenant.deleted_at IS NULL AND tenant.status='active' AND member.status='active'`, bindingID, executionID, scope.TenantID, scope.WorkspaceID, scope.Generation)
}

// liveWorkflowModelBinding resolves a binding against a live WorkflowRun session execution. A
// workflow run has no Thread: the run's `running` status and the session execution's open result
// are the liveness evidence.
//
// Belonging to the run is not enough here, which is where this differs from its IssueRun
// counterpart. An IssueRun owns exactly one binding (0033's `run_id ... UNIQUE`), so "the binding's
// run is the execution's run" names one row. A WorkflowRun owns one binding per executor slot
// (0039 drops that uniqueness), so the same test admits every slot's binding at once — and the
// binding id is the caller's to state, since a model mTLS certificate is bound to a tenant,
// workspace and runtime generation, never to a slot. Node A holding slot `fast` could therefore
// present node B's `slow` binding id and be handed B's credential and model.
//
// So the binding must be the one this execution was dispatched with. `e.input->>'modelBindingId'`
// is that value: it is written by Cloud when the work item is enqueued, copied into the execution
// row at dispatch, and never read from a request body. A session dispatched with no binding leaves
// it NULL, which matches nothing — correct, since such a session has no binding to redeem.
func liveWorkflowModelBinding(t *transaction, scope ModelRuntimeScope, bindingID, executionID string) Object {
	return t.one(`SELECT b.*, e.execution_id,e.node_id,e.work_id,w.id AS workspace_id,w.runtime_generation
	 FROM run_model_bindings b JOIN workflow_runs r ON r.id=b.workflow_run_id AND r.deleted_at IS NULL
	 JOIN node_executions e ON e.operation_id=r.id AND e.kind='agent_session'
	 JOIN workspaces w ON w.id=e.workspace_id AND w.workflow_run_id=r.id
	 JOIN personal_model_connections c ON c.id=b.connection_id AND c.user_id=b.user_id
	 JOIN users u ON u.id=b.user_id
	 JOIN tenants tenant ON tenant.id=r.tenant_id
	 JOIN tenant_memberships member ON member.tenant_id=r.tenant_id AND member.user_id=b.user_id
	 WHERE b.id=$1 AND e.execution_id=$2 AND r.tenant_id=$3 AND w.id=$4 AND w.runtime_generation=$5
	 AND e.input->>'modelBindingId' = b.id::text
	 AND b.revoked_at IS NULL
	 AND r.status='running'
	 AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
	 AND w.deleted_at IS NULL AND w.desired_state='running' AND w.observed_state='ready' AND w.admission_open
	 AND c.deleted_at IS NULL AND c.enabled AND c.credential_id IS NOT NULL
	 AND u.deleted_at IS NULL AND u.status='active' AND tenant.deleted_at IS NULL AND tenant.status='active' AND member.status='active'`, bindingID, executionID, scope.TenantID, scope.WorkspaceID, scope.Generation)
}

func modelGrantResult(t *transaction, g, b Object) ModelGrant {
	secret := t.one("SELECT ciphertext,key_id FROM personal_model_credentials WHERE id=$1 AND user_id=$2", b.S("credentialId"), b.S("userId"))
	require(secret != nil, 403, "model_grant_revoked")
	ciphertext, err := base64.StdEncoding.DecodeString(secret.S("ciphertext"))
	if err != nil {
		panic(databaseFailure{err})
	}
	var model ModelDefinition
	if err = json.Unmarshal([]byte(jsonText(b.O("model"))), &model); err != nil {
		panic(databaseFailure{err})
	}
	expires, err := time.Parse(time.RFC3339Nano, g.S("expiresAt"))
	if err != nil {
		panic(databaseFailure{err})
	}
	return ModelGrant{ID: g.S("id"), BindingID: b.S("id"), RunID: b.S("runId"), ExecutionID: b.S("executionId"), WorkspaceID: b.S("workspaceId"), NodeID: b.S("nodeId"), CredentialID: b.S("credentialId"), CredentialKeyID: secret.S("keyId"), Protocol: b.S("protocol"), BaseURL: b.S("baseUrl"), AuthMode: b.S("authMode"), RuntimeGeneration: b.N("runtimeGeneration"), ConnectionVersion: b.N("connectionVersion"), Model: model, Ciphertext: ciphertext, ExpiresAt: expires}
}

func modelGrantScope(t *transaction, g Object) ModelRuntimeScope {
	r := t.one("SELECT tenant_id FROM workspaces WHERE id=$1", g.S("workspaceId"))
	require(r != nil, 403, "model_grant_revoked")
	return ModelRuntimeScope{TenantID: r.S("tenantId"), WorkspaceID: g.S("workspaceId"), Generation: g.N("runtimeGeneration")}
}

// CreateModelGrant persists only the digest of a temporary bearer token. The caller generates
// the token in memory; expiration is based entirely on PostgreSQL's authoritative clock.
func (s *Store) CreateModelGrant(ctx context.Context, scope ModelRuntimeScope, bindingID, executionID, digest string) (ModelGrant, error) {
	var result ModelGrant
	_, err := s.transact(ctx, func(t *transaction) Object {
		require(len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == "", 400, "invalid_model_grant")
		b := liveModelBinding(t, scope, bindingID, executionID)
		id := newID()
		t.exec("INSERT INTO model_access_grants(id,binding_id,execution_id,workspace_id,runtime_generation,token_digest) VALUES($1,$2,$3,$4,$5,$6)", id, bindingID, executionID, scope.WorkspaceID, scope.Generation, digest)
		result = modelGrantResult(t, t.one("SELECT * FROM model_access_grants WHERE id=$1", id), b)
		return Object{}
	})
	return result, err
}

// RenewModelGrant extends the same grant/digest before it expires. A stale certificate or an
// ended run cannot renew, and a revoked grant can never become live again.
func (s *Store) RenewModelGrant(ctx context.Context, scope ModelRuntimeScope, id, executionID string) (ModelGrant, error) {
	var result ModelGrant
	_, err := s.transact(ctx, func(t *transaction) Object {
		require(validID(id), 404, "not_found")
		g := t.one("SELECT * FROM model_access_grants WHERE id=$1 AND execution_id=$2 AND workspace_id=$3 AND runtime_generation=$4 AND revoked_at IS NULL AND expires_at>now()", id, executionID, scope.WorkspaceID, scope.Generation)
		require(g != nil, 403, "model_grant_revoked")
		b := liveModelBinding(t, scope, g.S("bindingId"), executionID)
		t.exec("UPDATE model_access_grants SET expires_at=now()+interval '15 minutes' WHERE id=$1", id)
		result = modelGrantResult(t, t.one("SELECT * FROM model_access_grants WHERE id=$1", id), b)
		return Object{}
	})
	return result, err
}

// RevokeModelGrant permits its certificate-bound runtime to release a grant after shutdown;
// it does not require the run to remain live. Repeated revocation is harmless.
func (s *Store) RevokeModelGrant(ctx context.Context, scope ModelRuntimeScope, id, executionID string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		require(validID(id), 404, "not_found")
		g := t.one(`SELECT g.id FROM model_access_grants g JOIN workspaces w ON w.id=g.workspace_id WHERE g.id=$1 AND g.execution_id=$2 AND g.workspace_id=$3 AND g.runtime_generation=$4 AND w.tenant_id=$5`, id, executionID, scope.WorkspaceID, scope.Generation, scope.TenantID)
		require(g != nil, 404, "not_found")
		t.exec("UPDATE model_access_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1", id)
		return Object{}
	})
	return err
}

// ResolveModelGrant rechecks account, tenant membership, run, connection, execution and runtime
// state on every request. model-gateway also repeats this check during an active stream to cancel
// promptly when any of those authorizations is withdrawn.
func (s *Store) ResolveModelGrant(ctx context.Context, digest string) (ModelGrant, error) {
	var result ModelGrant
	_, err := s.transact(ctx, func(t *transaction) Object {
		g := t.one("SELECT * FROM model_access_grants WHERE token_digest=$1 AND revoked_at IS NULL AND expires_at>now()", digest)
		require(g != nil, 403, "model_grant_revoked")
		b := liveModelBinding(t, modelGrantScope(t, g), g.S("bindingId"), g.S("executionId"))
		result = modelGrantResult(t, g, b)
		return Object{}
	})
	return result, err
}
