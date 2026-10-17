package core

import (
	"context"
	"strings"
	"time"
)

// Explicit publish (cloud/project, explicit fast-forward publish). The only way Ora writes an
// external repository is a PublishOperation a user created and the credential's approver
// approved; its fields are immutable, the approval binds them and the credential version, and the
// executor only fast-forwards with expectedOldOid as the compare value.

// approvalLifetime bounds how long an approval may wait for its execution (publish ADR D2).
const approvalLifetime = 24 * time.Hour

// GitRefReader is the Git Server's read-only ref query as Cloud consumes it (contract D6). Cloud
// reads refs outside transactions and records what it read inside them.
type GitRefReader interface {
	ResolveGitRef(ctx context.Context, tenantID, projectID, ref string) (commit string, found bool, err error)
	ListGitRefs(ctx context.Context, tenantID, projectID, prefix string) ([]GitRefCommit, error)
}

// GitRefCommit is one ref and the commit it names.
type GitRefCommit struct{ Ref, Commit string }

// publishRequest is a validated publish body, resolved to its source and credential.
type publishRequest struct {
	tenantID, projectID, bindingID          string
	sourceRef, sourceCommit, revisionID     string
	targetBranch, expectedOld, credentialID string
	approverKind                            string
	selfApproved                            bool
	storeVersion                            int64
}

// preparePublish validates a publish body for the caller against current state. It runs once
// before the ref query and again in the creating transaction, so nothing the query could not see
// changes the decision.
func preparePublish(t *transaction, r *PublicRequest, uid string) publishRequest {
	p, _ := projectInSpace(t, r.TenantID, uid, r.ProjectID)
	require(p.S("lifecycle") == "active", 409, "resource_unavailable")
	b := bindingOf(t, p.S("id"))
	q := publishRequest{tenantID: r.TenantID, projectID: p.S("id"), bindingID: b.S("id")}
	if id := r.Body.S("revisionId"); id != "" {
		require(r.Body.S("sourceRef") == "" && r.Body.S("sourceCommit") == "", 400, "source_conflict")
		require(validID(id), 400, "invalid_revision")
		rev := t.one("SELECT * FROM revisions WHERE id=$1 AND project_id=$2 AND tenant_id=$3", id, p.S("id"), r.TenantID)
		require(rev != nil, 404, "revision_not_found")
		// The Revision's delivery ref and final commit are values Cloud already verified.
		name, ok := ParseGitRef(rev.S("revisionRef"))
		require(ok && name.Namespace == GitDelivery, 409, "revision_not_publishable")
		q.revisionID, q.sourceRef, q.sourceCommit = id, rev.S("revisionRef"), rev.S("finalCommit")
	} else {
		q.sourceRef, q.sourceCommit = r.Body.S("sourceRef"), r.Body.S("sourceCommit")
		name, ok := ParseGitRef(q.sourceRef)
		require(ok && (name.Namespace == GitWorkspaceBranch || name.Namespace == GitDelivery), 400, "invalid_source_ref")
		require(GitObjectID(q.sourceCommit) && !gitZeroID(q.sourceCommit), 400, "invalid_source_commit")
	}
	q.targetBranch = r.Body.S("targetBranch")
	require(validBranchName(q.targetBranch) && q.targetBranch != "HEAD", 400, "invalid_target_branch")
	q.expectedOld = r.Body.S("expectedOldOid")
	require(GitObjectID(q.expectedOld), 400, "invalid_expected_old")
	require(q.expectedOld != q.sourceCommit, 400, "nothing_to_publish")
	q.credentialID = r.Body.S("credentialRefId")
	if q.credentialID == "" {
		q.credentialID = b.S("credentialRefId")
	}
	require(validID(q.credentialID), 409, "credential_publish_unverified")
	c := t.one("SELECT * FROM credential_refs WHERE id=$1 AND tenant_id=$2", q.credentialID, r.TenantID)
	require(c != nil, 404, "not_found")
	code := credentialProblem(t, r.TenantID, q.credentialID, b.S("repositoryUrl"), CapabilityPublish)
	require(code == "", 409, code)
	q.storeVersion = c.N("storeVersion")
	m := membership(t, r.TenantID, uid, false)
	publisher := m.S("role") == "admin"
	switch c.S("attribution") {
	case "personal":
		q.approverKind = "credential_owner"
		require(publisher || c.S("ownerUserId") == uid, 403, "publish_permission_required")
		q.selfApproved = c.S("ownerUserId") == uid
	case "team":
		q.approverKind = "publisher"
		require(publisher, 403, "publish_permission_required")
		q.selfApproved = true
	default:
		// No approver can be determined for an unknown attribution (publish ADR D2).
		reject(409, "credential_attribution_unknown")
	}
	return q
}

// CreatePublish creates a PublishOperation. Cloud confirms through the ref query that the source
// ref exists in the Project repository; whether the source commit is reachable from it is the
// executor's check, because Cloud reads no objects (publish ADR D1).
func (s *Store) CreatePublish(ctx context.Context, r *PublicRequest) (Object, int, error) {
	hash := requestHash(r.Method, r.Path, r.Body)
	var q publishRequest
	var replay Object
	status := 0
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		require(r.Key != "" && len(r.Key) <= 200, 400, "idempotency_key_required")
		if old := t.one("SELECT * FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3", r.TenantID, uid, r.Key); old != nil {
			require(old.S("requestHash") == hash, 409, "idempotency_conflict")
			replay, status = old.O("response"), int(old.N("status"))
			return nil
		}
		q = preparePublish(t, r, uid)
		return nil
	})
	if err != nil || replay != nil {
		return replay, status, err
	}
	if s.GitRefs == nil {
		return nil, 0, &Fault{Code: "git_server_unconfigured", Status: 503, Params: Object{}}
	}
	if _, found, err := s.GitRefs.ResolveGitRef(ctx, q.tenantID, q.projectID, q.sourceRef); err != nil {
		return nil, 0, &Fault{Code: "git_server_unavailable", Status: 503, Params: Object{}}
	} else if !found {
		return nil, 0, &Fault{Code: "source_ref_not_found", Status: 409, Params: Object{}}
	}
	out, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		if old := t.one("SELECT * FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3", r.TenantID, uid, r.Key); old != nil {
			require(old.S("requestHash") == hash, 409, "idempotency_conflict")
			status = int(old.N("status"))
			return old.O("response")
		}
		again := preparePublish(t, r, uid)
		require(again == q, 409, "publish_conflict")
		o := insertPublish(t, &again, uid)
		resp := Object{"resource": repositoryOperationView(o)}
		status = 201
		t.exec("INSERT INTO idempotency_records(tenant_id,user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5,$6)", r.TenantID, uid, r.Key, hash, jsonText(resp), status)
		return resp
	})
	return out, status, err
}

func insertPublish(t *transaction, q *publishRequest, uid string) Object {
	id := newID()
	state := "awaiting_approval"
	var approvedBy, approvedVersion any
	if q.selfApproved {
		state, approvedBy, approvedVersion = "pending", uid, q.storeVersion
	}
	var revision any
	if q.revisionID != "" {
		revision = q.revisionID
	}
	t.exec(`INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger,requested_by,credential_ref_id,credential_version,
	 source_ref,source_commit,target_branch,expected_old_oid,revision_id,approver_kind,approval_expires_at,approved_by,approved_at,approved_store_version)
	 VALUES($1,'repository_publish',$2,$3,$4,$5,'manual',$6,$7,$8,$9,$10,$11,$12,$13,$14,clock_timestamp()+make_interval(secs=>$15),$16,CASE WHEN $16::uuid IS NULL THEN NULL ELSE clock_timestamp() END,$17)`,
		id, q.tenantID, q.projectID, q.bindingID, state, uid, q.credentialID, q.storeVersion, q.sourceRef, q.sourceCommit, q.targetBranch, q.expectedOld, revision, q.approverKind, seconds(approvalLifetime), approvedBy, approvedVersion)
	auditRepositoryOperation(t, id, "created", uid, Object{"sourceRef": q.sourceRef, "sourceCommit": q.sourceCommit, "targetBranch": q.targetBranch, "expectedOldOid": q.expectedOld, "credentialRefId": q.credentialID, "storeVersion": q.storeVersion, "approverKind": q.approverKind})
	if q.selfApproved {
		auditRepositoryOperation(t, id, "approved", uid, Object{"storeVersion": q.storeVersion})
	}
	invalidateRepository(t, id)
	return t.one("SELECT * FROM repository_operations WHERE id=$1", id)
}

// mayApprove reports whether uid is the approver the publish's credential determines.
func mayApprove(t *transaction, o Object, uid string) bool {
	c := t.one("SELECT * FROM credential_refs WHERE id=$1", o.S("credentialRefId"))
	if c == nil {
		return false
	}
	m := t.one("SELECT m.role FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", o.S("tenantId"), uid)
	if m == nil {
		return false
	}
	switch o.S("approverKind") {
	case "credential_owner":
		// Never an administrator on the owner's behalf (identity-access D5).
		return c.S("attribution") == "personal" && c.S("ownerUserId") == uid
	case "publisher":
		return m.S("role") == "admin"
	}
	return false
}

// publishAction applies a member's approve, reject, cancel or check-now to one publish.
func publishAction(t *transaction, r *PublicRequest, p Object, uid string) Object {
	o := projectRepositoryOperation(t, p, r.RepositoryOperationID)
	require(o.S("type") == RepositoryPublish, 404, "not_found")
	switch {
	case strings.HasSuffix(r.Path, "/approve"):
		require(o.S("state") == "awaiting_approval", 409, "operation_not_awaiting_approval")
		require(mayApprove(t, o, uid), 403, "approver_required")
		c := t.one("SELECT store_version FROM credential_refs WHERE id=$1", o.S("credentialRefId"))
		if c.N("storeVersion") != o.N("credentialVersion") {
			finishRepositoryOperation(t, o.S("id"), "cancelled", "credential_changed", Object{})
			break
		}
		t.exec(`UPDATE repository_operations SET state='pending',approved_by=$2,approved_at=clock_timestamp(),approved_store_version=$3,approval_expires_at=clock_timestamp()+make_interval(secs=>$4)
		 WHERE id=$1 AND state='awaiting_approval'`, o.S("id"), uid, c.N("storeVersion"), seconds(approvalLifetime))
		auditRepositoryOperation(t, o.S("id"), "approved", uid, Object{"storeVersion": c.N("storeVersion")})
		invalidateRepository(t, o.S("id"))
	case strings.HasSuffix(r.Path, "/reject"):
		require(o.S("state") == "awaiting_approval", 409, "operation_not_awaiting_approval")
		require(mayApprove(t, o, uid), 403, "approver_required")
		auditRepositoryOperation(t, o.S("id"), "rejected_by_approver", uid, Object{})
		finishRepositoryOperation(t, o.S("id"), "cancelled", "rejected_by_approver", Object{})
	case strings.HasSuffix(r.Path, "/cancel"):
		require(o.S("requestedBy") == uid, 403, "requester_required")
		require(o.S("state") == "awaiting_approval" || o.S("state") == "pending", 409, "operation_not_cancellable")
		auditRepositoryOperation(t, o.S("id"), "cancelled_by_requester", uid, Object{})
		finishRepositoryOperation(t, o.S("id"), "cancelled", "cancelled_by_requester", Object{})
	case strings.HasSuffix(r.Path, "/check"):
		require(o.S("state") == "uncertain", 409, "operation_not_uncertain")
		require(o.S("requestedBy") == uid || mayApprove(t, o, uid), 403, "requester_required")
		t.exec("UPDATE repository_operations SET next_check_at=GREATEST(clock_timestamp(),task_deadline) WHERE id=$1", o.S("id"))
		invalidateRepository(t, o.S("id"))
	default:
		reject(404, "not_found")
	}
	return Object{"resource": repositoryOperationView(t.one("SELECT * FROM repository_operations WHERE id=$1", o.S("id")))}
}

// publishStartRefusal reports why an approved publish may no longer start: its approval lapsed,
// or the credential got a new version after the approval, which approved the old token.
func publishStartRefusal(t *transaction, o Object) string {
	if t.one("SELECT 1 AS x FROM repository_operations WHERE id=$1 AND approval_expires_at<=clock_timestamp()", o.S("id")) != nil {
		return "approval_expired"
	}
	c := t.one("SELECT store_version FROM credential_refs WHERE id=$1", o.S("credentialRefId"))
	if c == nil || c.N("storeVersion") != o.N("approvedStoreVersion") {
		return "credential_changed"
	}
	return ""
}

// ExpirePublishApprovalsOnce cancels publishes whose approval window passed before they ran.
func (s *Store) ExpirePublishApprovalsOnce(ctx context.Context) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		for _, o := range t.list("SELECT id FROM repository_operations WHERE type='repository_publish' AND state IN ('awaiting_approval','pending') AND approval_expires_at<=clock_timestamp() ORDER BY approval_expires_at LIMIT 50") {
			finishRepositoryOperation(t, o.S("id"), "cancelled", "approval_expired", Object{})
		}
		return nil
	})
	return err
}

// ListWorkspaceBranches lists one Workspace's collaboration branches of the Project repository
// through the ref query, for choosing a publish source (publish ADR D8). The answer is the query's
// moment only; Cloud records nothing from it.
func (s *Store) ListWorkspaceBranches(ctx context.Context, r *PublicRequest) (Object, error) {
	_, err := s.transact(ctx, func(t *transaction) Object {
		uid := identityWithAlias(t, r.Identity).S("id")
		membership(t, r.TenantID, uid, false)
		p, _ := projectInSpace(t, r.TenantID, uid, r.ProjectID)
		require(validID(r.WorkspaceID) && t.one("SELECT id FROM workspaces WHERE id=$1 AND project_id=$2", r.WorkspaceID, p.S("id")) != nil, 404, "not_found")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.GitRefs == nil {
		return nil, &Fault{Code: "git_server_unconfigured", Status: 503, Params: Object{}}
	}
	refs, err := s.GitRefs.ListGitRefs(ctx, r.TenantID, r.ProjectID, GitWorkspaceRefPrefix(r.WorkspaceID))
	if err != nil {
		return nil, &Fault{Code: "git_server_unavailable", Status: 503, Params: Object{}}
	}
	items := []Object{}
	for _, ref := range refs {
		name, ok := ParseGitRef(ref.Ref)
		if !ok || name.Namespace != GitWorkspaceBranch || name.Owner != r.WorkspaceID {
			continue
		}
		items = append(items, Object{"ref": ref.Ref, "branch": name.Name, "commit": ref.Commit})
	}
	return Object{"items": items}, nil
}
