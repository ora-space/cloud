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

// GitAccessMode is the repository permission one execution currently holds.
type GitAccessMode int

const (
	// GitRead permits fetching the Project repository: a session restoring a prior Revision.
	GitRead GitAccessMode = iota + 1
	// GitWrite additionally permits creating the delivery execution's single remote ref.
	GitWrite
)

// GitDeliveryRefPrefix names the remote refs delivery attempts create. Each delivery work item
// owns exactly one ref under it, so a retry never collides with an earlier attempt.
const GitDeliveryRefPrefix = "refs/ora/deliveries/"

// GitAccess is the git-server policy result for one request. DeliveryRef is set only for
// GitWrite and is the only ref that request may create.
type GitAccess struct {
	ProjectID   string
	Mode        GitAccessMode
	DeliveryRef string
}

// AuthorizeGitAccess decides from current PostgreSQL state whether the execution a sandbox
// declares may read or write the Project repository. The certificate only names the runtime
// target; every permission comes from this check, so ending the execution, replacing the
// sandbox, canceling the run or disabling the trigger's account takes effect on the next request.
// It runs in a read-only snapshot without the control-plane advisory lock: Git traffic must not
// serialize with control-plane writes, and the decision mutates nothing.
func (s *Store) AuthorizeGitAccess(ctx context.Context, scope GitRuntimeScope, executionID, projectID string) (GitAccess, error) {
	var access GitAccess
	err := s.view(ctx, func(t *transaction) {
		access = gitAccess(t, scope, executionID, projectID)
	})
	// Every denial is the same fault, whichever gate refused: a sandbox must not learn whether a
	// repository, execution or Node exists. Database failures stay distinct so callers fail closed
	// as unavailable instead of reporting a permanent denial.
	var fault *Fault
	if errors.As(err, &fault) {
		return GitAccess{}, &Fault{Code: "git_access_denied", Status: 403, Params: Object{}}
	}
	return access, err
}

func gitAccess(t *transaction, scope GitRuntimeScope, executionID, projectID string) GitAccess {
	require(validID(scope.TenantID) && validID(scope.WorkspaceID) && scope.Generation > 0 && validID(projectID) && executionID != "" && len(executionID) <= 200, 403, "git_access_denied")
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
	require(e != nil, 403, "git_access_denied")
	access := GitAccess{ProjectID: projectID}
	switch e.S("kind") {
	case "deliver_revision":
		require(e.S("phase") == "delivering", 403, "git_access_denied")
		access.Mode, access.DeliveryRef = GitWrite, GitDeliveryRefPrefix+e.S("workId")
	case "agent_session":
		// Only a session restoring a prior Revision reads the repository; other sessions start
		// from the upstream clone and have no reason to reach the Git Server.
		live := e.S("phase") == "starting" || e.S("phase") == "running"
		require(live && e.S("cancelRequestedAt") == "" && e.S("threadState") != "ending" && e.S("threadState") != "ended" && len(e.O("input").O("priorRevision")) > 0, 403, "git_access_denied")
		access.Mode = GitRead
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
