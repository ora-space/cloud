package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WorkspaceLocation is a checkout on this host and the commit it was cloned at.
type WorkspaceLocation struct {
	Path       string
	BaseCommit string
}

// RecordWorkspace remembers where a workspace's checkout is. A later checkout for the same
// workspace (a retry after a failed clone) replaces the earlier one.
func (s *Store) RecordWorkspace(ctx context.Context, id string, at WorkspaceLocation) error {
	_, e := s.pool.Exec(ctx, `
		INSERT INTO v2_workspaces (id, path, base_commit) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET path = EXCLUDED.path, base_commit = EXCLUDED.base_commit`,
		id, at.Path, at.BaseCommit)
	if e != nil {
		return fmt.Errorf("record workspace: %w", e)
	}
	return nil
}

// ChatWorkspace returns the checkout of the person's conversation. ErrNotFound covers both a
// conversation that is not theirs and one whose checkout is not ready yet; callers that must tell
// the two apart look the conversation up first.
func (s *Store) ChatWorkspace(ctx context.Context, tenant, user, conversationID string) (WorkspaceLocation, error) {
	return s.workspace(ctx, `
		SELECT w.path, w.base_commit
		FROM v2_chat_conversations c
		JOIN v2_chat_sessions s ON s.conversation_id = c.id
		JOIN v2_execution_sessions e ON e.id = s.session_id
		JOIN v2_workspaces w ON w.id = e.data->>'WorkspaceID'
		WHERE c.tenant_id = $1 AND c.user_id = $2 AND c.id = $3`, tenant, user, conversationID)
}

// WorkflowRunWorkspace returns the checkout a workflow run's Agent nodes share, from its most
// recently created node that ran an Agent. Runs are readable by every tenant member, so this is
// scoped by tenant only, like the run itself.
func (s *Store) WorkflowRunWorkspace(ctx context.Context, tenant, runID string) (WorkspaceLocation, error) {
	return s.workspace(ctx, `
		SELECT w.path, w.base_commit
		FROM v2_workflow_node_runs n
		JOIN v2_execution_sessions e ON e.id = n.session_id AND e.tenant_id = n.tenant_id
		JOIN v2_workspaces w ON w.id = e.data->>'WorkspaceID'
		WHERE n.tenant_id = $1 AND n.run_id = $2
		ORDER BY n.created_at DESC, n.id DESC
		LIMIT 1`, tenant, runID)
}

// TaskRunWorkspace returns the checkout of one run of a task; each run has its own. Runs are
// readable by every tenant member, so this is scoped by tenant only, like the run itself.
func (s *Store) TaskRunWorkspace(ctx context.Context, tenant, taskID, runID string) (WorkspaceLocation, error) {
	return s.workspace(ctx, `
		SELECT w.path, w.base_commit
		FROM v2_task_runs r
		JOIN v2_execution_sessions e ON e.id = r.session_id AND e.tenant_id = r.tenant_id
		JOIN v2_workspaces w ON w.id = e.data->>'WorkspaceID'
		WHERE r.tenant_id = $1 AND r.task_id = $2 AND r.id = $3`, tenant, taskID, runID)
}

func (s *Store) workspace(ctx context.Context, query string, args ...any) (WorkspaceLocation, error) {
	var at WorkspaceLocation
	e := s.pool.QueryRow(ctx, query, args...).Scan(&at.Path, &at.BaseCommit)
	if errors.Is(e, pgx.ErrNoRows) {
		return at, ErrNotFound
	}
	if e != nil {
		return at, fmt.Errorf("read workspace: %w", e)
	}
	return at, nil
}
