package core

import (
	"context"
	"errors"
)

// AuthorizeGitOperation decides from current PostgreSQL state what a Repo Executor request made
// for one repository Operation may do in the Project repository. The executor's service
// certificate carries no permission: everything comes from the Operation, so canceling or ending
// it ends Git access on the next request, including a pre-receive already in flight.
//
// The Operation must be running in the named tenant and Project, on that Project's Binding, and
// the Project must not be deleted. Unlike a sandbox execution it does not require an active
// Project: the first sync runs while the Project is provisioning, and a sync already running
// when the Project enters deleting must still be able to settle. The external line adds the
// lease to this predicate and the tracking-ref rule with its recorded expected old value; neither
// may widen the grant beyond the executor's namespaces.
func (s *Store) AuthorizeGitOperation(ctx context.Context, operationID, tenantID, projectID string) (GitGrant, error) {
	var grant GitGrant
	err := s.view(ctx, func(t *transaction) {
		grant = gitOperationAccess(t, operationID, tenantID, projectID)
	})
	var fault *Fault
	if errors.As(err, &fault) {
		return GitGrant{}, &Fault{Code: "git_access_denied", Status: 403, Params: Object{}}
	}
	return grant, err
}

func gitOperationAccess(t *transaction, operationID, tenantID, projectID string) GitGrant {
	require(canonicalUUID(operationID) && validID(tenantID) && validID(projectID), 403, "git_access_denied")
	o := t.one(`SELECT o.type,o.binding_id FROM repository_operations o
	 JOIN repository_bindings b ON b.id=o.binding_id AND b.project_id=o.project_id AND b.tenant_id=o.tenant_id
	 JOIN projects p ON p.id=o.project_id AND p.tenant_id=o.tenant_id
	 WHERE o.id=$1 AND o.tenant_id=$2 AND o.project_id=$3 AND o.state='running'
	 AND p.deleted_at IS NULL AND p.lifecycle IN ('provisioning','active','deleting')`, operationID, tenantID, projectID)
	require(o != nil, 403, "git_access_denied")
	grant := GitGrant{TenantID: tenantID, ProjectID: projectID}
	switch o.S("type") {
	case RepositorySync:
		// Cloud cannot know which commit the external repository will yield, so the snapshot
		// grant is the Binding's prefix; create-only and "name equals target" bound it.
		grant.Writable = []GitWritableRef{{Prefix: GitSnapshotRefPrefix(o.S("bindingId")), Rule: GitCreateOnly}}
	case RepositoryPublish:
		// A publish only reads its source from the Project repository.
	default:
		reject(403, "git_access_denied")
	}
	return grant
}
