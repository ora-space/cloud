package core

import (
	"context"
	"strings"
	"time"
)

// sessionList reads only persisted Cloud Agent runs, without provisioning identities or
// refreshing runtime controls. A cursor is always resolved inside the current tenant.
func (s *Store) sessionList(ctx context.Context, r *PublicRequest) (Object, int, error) {
	out, err := s.transact(ctx, func(t *transaction) Object {
		uid := sessionReadIdentity(t, r.Identity)
		membership(t, r.TenantID, uid, false)
		validateSessionFilters(r)
		query := `SELECT r.id,r.issue_id,i.number AS issue_number,i.title AS issue_title,
			p.id AS project_id,p.name AS project_name,r.input->>'agentPluginId' AS agent_plugin_id,
			r.status,r.thread_state,r.created_at,r.started_at,r.completed_at,
			EXISTS(SELECT 1 FROM revisions v WHERE v.run_id=r.id AND v.tenant_id=r.tenant_id
				AND (v.expires_at IS NULL OR v.expires_at>clock_timestamp())) AS history_available,
			CASE WHEN b.id IS NULL THEN NULL ELSE jsonb_build_object('connectionName',b.connection_name,
				'modelId',b.model->>'id','modelName',b.model->>'name') END AS model
			FROM issue_runs r JOIN issues i ON i.id=r.issue_id AND i.tenant_id=r.tenant_id
			LEFT JOIN projects p ON p.id=i.project_ref AND p.tenant_id=r.tenant_id AND p.deleted_at IS NULL
			LEFT JOIN run_model_bindings b ON b.run_id=r.id
			WHERE r.tenant_id=$1 AND r.executor_type='agent' AND r.deleted_at IS NULL AND i.deleted_at IS NULL`
		args := []any{r.TenantID}
		add := func(condition, value string) {
			if value != "" {
				args = append(args, value)
				query += " AND " + strings.ReplaceAll(condition, "?", "$"+itoa(len(args)))
			}
		}
		add("p.id=?::uuid", r.SessionProjectID)
		add("r.status=?", r.SessionStatus)
		add("r.input->>'agentPluginId'=?", r.SessionAgent)
		add("r.created_at>=?::timestamptz", r.SessionFrom)
		add("r.created_at<?::timestamptz", r.SessionTo)
		if r.After != "" {
			anchor := t.one(`SELECT r.created_at,r.id FROM issue_runs r JOIN issues i ON i.id=r.issue_id AND i.tenant_id=r.tenant_id
				WHERE r.id=$1 AND r.tenant_id=$2 AND r.executor_type='agent' AND r.deleted_at IS NULL AND i.deleted_at IS NULL`, r.After, r.TenantID)
			require(anchor != nil, 400, "invalid_pagination")
			args = append(args, anchor["createdAt"], r.After)
			query += " AND (r.created_at,r.id)<($" + itoa(len(args)-1) + "::timestamptz,$" + itoa(len(args)) + "::uuid)"
		}
		return window(t, query+" ORDER BY r.created_at DESC,r.id DESC", args, r)
	})
	return out, 200, err
}

func validateSessionFilters(r *PublicRequest) {
	require(r.Limit >= 0 && r.Limit <= 100 && r.Before == "", 400, "invalid_pagination")
	require(r.After == "" || validID(r.After), 400, "invalid_pagination")
	require(r.SessionProjectID == "" || validID(r.SessionProjectID), 400, "invalid_session_filter")
	require(len(r.SessionAgent) <= 200, 400, "invalid_session_filter")
	switch r.SessionStatus {
	case "", "queued", "dispatched", "running", "completed", "failed", "cancelled", "deferred":
	default:
		require(false, 400, "invalid_session_filter")
	}
	from, to := sessionDate(r.SessionFrom), sessionDate(r.SessionTo)
	require(from.IsZero() || to.IsZero() || from.Before(to), 400, "invalid_session_filter")
}

func sessionDate(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	date, err := time.Parse(time.RFC3339Nano, value)
	require(err == nil, 400, "invalid_session_filter")
	return date
}
