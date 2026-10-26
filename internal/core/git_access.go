package core

import (
	"context"
	"database/sql"
	"errors"
)

// GitRuntimeScope is derived from a verified git-access mTLS certificate, never from a request
// header or path. Generation fences a certificate issued to a replaced sandbox.
type GitRuntimeScope struct {
	TenantID    string
	WorkspaceID string
	Generation  int64
}

// GitWritableRef is one entry of a grant's write rule set: an exact ref, or every ref under a
// prefix, and the rule its updates must satisfy. ExpectedOld is used by GitTrackingCAS only; ""
// means the ref must not exist yet.
type GitWritableRef struct {
	Ref         string
	Prefix      string
	Rule        GitRefRule
	ExpectedOld string
}

// GitGrant is the Git Server policy result for one request: the Project repository it may read in
// full, and the refs it may write. An empty Writable is a read-only grant. The repository is the
// isolation boundary, so a grant never hides refs inside it.
type GitGrant struct {
	TenantID, ProjectID string
	Writable            []GitWritableRef
}

// Writes reports whether the grant admits any ref update at all.
func (g *GitGrant) Writes() bool { return len(g.Writable) > 0 }

// AuthorizeGitAccess decides from current PostgreSQL state whether the execution a sandbox
// declares may read the Project repository and which refs it may write (contract D5). The
// certificate only names the runtime target; every permission comes from this check, so ending the
// execution, advancing its operation, replacing the sandbox, canceling the run or disabling the
// trigger's account takes effect on the next request. It runs in a read-only snapshot without the
// control-plane advisory lock: Git traffic must not serialize with control-plane writes, and the
// decision mutates nothing.
func (s *Store) AuthorizeGitAccess(ctx context.Context, scope GitRuntimeScope, executionID, projectID string) (GitGrant, error) {
	var access GitGrant
	err := s.view(ctx, func(t *transaction) {
		access = gitAccess(t, scope, executionID, projectID)
	})
	// Every denial is the same fault, whichever gate refused: a sandbox must not learn whether a
	// repository, execution or Node exists. Database failures stay distinct so callers fail closed
	// as unavailable instead of reporting a permanent denial.
	var fault *Fault
	if errors.As(err, &fault) {
		return GitGrant{}, &Fault{Code: "git_access_denied", Status: 403, Params: Object{}}
	}
	return access, err
}

// gitAccess maps the declared execution to its grant. Every granted execution reads the whole
// Project repository; what it may write depends on its kind, and never exceeds the sandbox column of
// the ref contract: no upstream ref, no other Workspace's branch, no ref outside the namespaces.
func gitAccess(t *transaction, scope GitRuntimeScope, executionID, projectID string) GitGrant {
	require(validID(scope.TenantID) && validID(scope.WorkspaceID) && scope.Generation > 0 && validID(projectID) && executionID != "" && len(executionID) <= 200, 403, "git_access_denied")
	if t.one("SELECT execution_id FROM clone_executions WHERE execution_id=$1", executionID) != nil {
		return gitCloneAccess(t, scope, executionID, projectID)
	}
	e := t.one(`SELECT e.kind,e.node_id,e.work_id,e.input,e.operation_id AS run_id,w.creator_user_id,w.tenant_id,w.id AS workspace_id,
	 r.phase,r.thread_state,r.cancel_requested_at
	 FROM node_executions e
	 JOIN workspaces w ON w.id=e.workspace_id AND w.issue_run_id=e.operation_id
	 JOIN issue_runs r ON r.id=e.operation_id AND r.tenant_id=w.tenant_id
	 JOIN issues issue ON issue.id=r.issue_id AND issue.deleted_at IS NULL
	 JOIN projects p ON p.id=w.project_id AND p.tenant_id=w.tenant_id
	 WHERE e.execution_id=$1 AND w.tenant_id=$2 AND w.id=$3 AND w.runtime_generation=$4 AND w.project_id=$5
	 AND e.kind IN ('agent_session','deliver_revision') AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
	 AND r.deleted_at IS NULL AND w.deleted_at IS NULL AND w.desired_state='running' AND w.observed_state='ready' AND w.admission_open
	 AND p.deleted_at IS NULL AND p.lifecycle='active'`, executionID, scope.TenantID, scope.WorkspaceID, scope.Generation, projectID)
	// Plugin executions never reach the repository: the kind filter above leaves them unmatched.
	require(e != nil, 403, "git_access_denied")
	access := GitGrant{TenantID: scope.TenantID, ProjectID: projectID}
	switch e.S("kind") {
	case "deliver_revision":
		require(e.S("phase") == "delivering", 403, "git_access_denied")
		// The delivery creates its own ref, named in its fixed input, and may fast-forward the run
		// Workspace's branch to the same final commit (contract D6). Both come from the input Cloud
		// fixed; the branch must belong to the certificate's Workspace whatever the input says.
		input := e.O("input")
		require(input.S("deliveryRef") == GitDeliveryRefPrefix+e.S("workId"), 403, "git_access_denied")
		access.Writable = []GitWritableRef{{Ref: input.S("deliveryRef"), Rule: GitCreateOnly}}
		if ref, ok := ParseGitRef(input.S("workspaceRef")); ok && ref.Namespace == GitWorkspaceBranch && ref.Owner == scope.WorkspaceID {
			access.Writable = append(access.Writable, GitWritableRef{Ref: input.S("workspaceRef"), Rule: GitFastForward})
		}
	case "agent_session":
		// Every live session reads the repository to collaborate and may create or fast-forward
		// branches of its own Workspace. The prefix comes from the certificate, never the request.
		live := e.S("phase") == "starting" || e.S("phase") == "running"
		require(live && e.S("cancelRequestedAt") == "" && e.S("threadState") != "ending" && e.S("threadState") != "ended", 403, "git_access_denied")
		access.Writable = []GitWritableRef{{Prefix: GitWorkspaceRefPrefix(scope.WorkspaceID), Rule: GitFastForward}}
	default:
		reject(403, "git_access_denied")
	}
	// The run's trigger must still be able to use this runtime: an active member of an active
	// tenant whose account is active. Durable project ownership alone confers nothing.
	require(runtimeUsable(t, e, e.S("creatorUserId")), 403, "git_access_denied")
	requireNoForceStop(t, scope.WorkspaceID)
	n := currentNode(t, scope.WorkspaceID)
	require(n.S("nodeId") == e.S("nodeId"), 403, "git_access_denied")
	c := runtimeControl(t, scope.WorkspaceID)
	require(c.S("state") == "maintenance" && c.S("maintenanceRunId") == e.S("runId") && c.B("bindingConfirmed") && c.S("boundSandboxId") == n.S("sandboxInstanceId"), 403, "git_access_denied")
	work := t.one("SELECT target FROM execution_work WHERE id=$1", e.S("workId"))
	require(work != nil && work.O("target").S("sandboxInstanceId") == n.S("sandboxInstanceId"), 403, "git_access_denied")
	return access
}

// gitCloneAccess grants a Workspace clone read-only access for exactly as long as its operation is
// in the clone step (contract D5). Unlike sessions it needs no ready Workspace or open admission:
// cloning is what happens before admission opens, and main Workspaces clone while their Project is
// still provisioning. The operation leaving the step, or the execution getting a result, ends it.
func gitCloneAccess(t *transaction, scope GitRuntimeScope, executionID, projectID string) GitGrant {
	e := t.one(`SELECT e.node_id,e.input,o.actor_user_id,w.id,w.tenant_id,w.creator_user_id
	 FROM clone_executions e
	 JOIN operations o ON o.id=e.operation_id AND o.workspace_id=e.workspace_id
	 JOIN workspaces w ON w.id=e.workspace_id
	 JOIN projects p ON p.id=w.project_id AND p.tenant_id=w.tenant_id
	 WHERE e.execution_id=$1 AND w.tenant_id=$2 AND w.id=$3 AND w.runtime_generation=$4 AND w.project_id=$5
	 AND e.result IS NULL AND e.terminated_by_force_stop_id IS NULL
	 AND o.state='running' AND o.step='clone' AND w.deleted_at IS NULL
	 AND p.deleted_at IS NULL AND p.lifecycle IN ('provisioning','active')`, executionID, scope.TenantID, scope.WorkspaceID, scope.Generation, projectID)
	require(e != nil, 403, "git_access_denied")
	require(gitRepositoryURLNames(e.O("input").S("repositoryUrl"), scope.TenantID, projectID), 403, "git_access_denied")
	// The operation's actor must still be able to use this runtime, as when the clone was dispatched.
	require(runtimeUsable(t, e, e.S("actorUserId")), 403, "git_access_denied")
	requireNoForceStop(t, scope.WorkspaceID)
	require(currentNode(t, scope.WorkspaceID).S("nodeId") == e.S("nodeId"), 403, "git_access_denied")
	return GitGrant{TenantID: scope.TenantID, ProjectID: projectID}
}

// view runs a read-only policy decision in one consistent snapshot. Unlike transact it takes no
// advisory lock and has no side effects, hooks or events: the decision may only read. A Fault
// panic becomes the returned error; any database failure is returned unchanged so callers can
// fail closed as unavailable rather than as denied.
func (s *Store) view(ctx context.Context, fn func(*transaction)) (err error) {
	tx, e := s.Pool.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	defer func() {
		if r := recover(); r != nil {
			switch v := r.(type) {
			case *Fault:
				err = v
			case databaseFailure:
				err = v.err
			default:
				panic(r)
			}
		}
	}()
	fn(&transaction{tx: tx, ctx: ctx, store: s})
	return tx.Commit()
}
