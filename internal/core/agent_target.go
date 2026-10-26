package core

import (
	"context"
	"database/sql"
	"strings"
)

// Agent target resolution + project gate (IssueRun D1/D2, plugin-marketplace "node executes plugin
// installs" D3).
//
// A Task Mode @ target with type=agent is the *Space Agent* projection: it must resolve to an ACTIVE
// space_agents row in the caller's tenant (IssueRun D1). Agents are the one collaboration target with
// real production backing today, so agent resolution is authoritative and in-transaction: it reads the
// exact space_agents row the enqueueRun INSERT will reference, inside the same transaction, never a
// side channel. A concurrent retire can therefore never desynchronize resolution from the run's own
// write. Teams and workflows still resolve through the (possibly nil) CollaborationDirectory.
//
// Project gate (IssueRun D2): creating an agent run requires the issue to carry a valid project_ref —
// non-empty, an existing project in the same tenant and the same collaboration space as the agent.
// Without a project there is no repository, so the run could never begin; the whole comment target is
// rejected with `409 issue_project_required` and the enclosing comment transaction rolls back.
//
// Snapshot (IssueRun D1, D6): the run input snapshot fixes the agent plugin identity and its pinned
// version so a later plugin upgrade never changes what a historical run executed. The keys mirror the
// D6 AgentSession `agent_plugin_id` / `agent_plugin_version` fields.

// agentRunEvidence resolves a Task Mode agent target to its authoritative space_agents row, joining
// the plugin's pinned desired version. ok=false means the tenant has no ACTIVE space_agents row with
// this id — either an unknown/foreign/retired id (404) or a dev/compat fixture agent — and the caller
// falls through to the directory for that. Reuses the grab-one-row transaction helper and the
// canonical plugin_id (namespace/identifier) split to reach space_plugins (D3), never matching on
// title or guessing.
func agentRunEvidence(t *transaction, tid, agentID string) (Object, bool) {
	row := t.one(`SELECT sa.plugin_id, sa.space_id, sa.display_name, p.desired_version AS desired_version
	       FROM space_agents sa
	       LEFT JOIN space_plugins p
	         ON p.space_id = sa.space_id
	        AND p.source_namespace = split_part(sa.plugin_id, '/', 1)
	        AND p.identifier = split_part(sa.plugin_id, '/', 2)
	       WHERE sa.id=$1 AND sa.tenant_id=$2 AND sa.status='active'`, agentID, tid)
	return row, row != nil
}

// agentRunGate enforces the IssueRun D2 project gate for a real space_agents-backed agent run. Any
// failure rejects the whole comment target by panicking (409), rolling back the enclosing comment
// transaction so no comment, activity, or run survives.
func agentRunGate(t *transaction, i, agent Object) {
	require(agentRunGateOK(t, i, agent), 409, "issue_project_required")
}

// agentRunGateOK reports the same IssueRun D2 project gate without panicking, so a scheduled-task
// admission maps the failure to its D5 failure reason and records the occurrence instead of rolling
// the whole evaluation transaction back.
func agentRunGateOK(t *transaction, i, agent Object) bool {
	ref, _ := i["projectRef"].(string)
	if ref == "" {
		return false
	}
	p := t.one("SELECT id, space_id FROM projects WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", ref, i.S("tenantId"))
	return p != nil && p.S("spaceId") == agent.S("spaceId")
}

// snapshotAgentRunInput merges the D1 plugin identity/version snapshot into a run input so a later
// install never changes what a historical run executed. It copies the builder's bundle and adds the
// two AgentSession keys; already-present keys win (the snapshot is written once at create).
func snapshotAgentRunInput(input, agent Object) Object {
	out := make(Object, len(input)+2)
	for k, v := range input {
		out[k] = v
	}
	out["agentPluginId"] = agent.S("pluginId")
	out["agentPluginVersion"] = agent.S("desiredVersion")
	return out
}

// SpaceAgentDirectory is the production CollaborationDirectory implementation backed by the
// space_agents roster (plugin-marketplace D3). It is deliberately scoped to Agent — the one target
// type with real production data — and never fabricates Team/Workflow targets. It serves the read-only
// target discovery projection; the authoritative run-create resolution still runs in-transaction via
// agentRunEvidence so it stays atomic with the IssueRun INSERT.
type SpaceAgentDirectory struct {
	pool *sql.DB
}

// NewSpaceAgentDirectory constructs the production roster-backed directory from the store's pool.
func NewSpaceAgentDirectory(pool *sql.DB) *SpaceAgentDirectory {
	return &SpaceAgentDirectory{pool: pool}
}

// ListTargets returns the tenant's active Space Agents as selectable @ targets, ordered stably and
// optionally filtered by a display-name substring. It applies the same `q` contract as the member
// and workflow projections of this endpoint: an empty query lists every active agent, a non-empty one
// keeps the active agents whose display_name contains it case-insensitively. The term is escaped
// through likePattern so `%` and `_` stay literal — a caller-supplied wildcard must never widen the
// result set past the agent the client asked for.
func (d *SpaceAgentDirectory) ListTargets(ctx context.Context, tenantID, query string) ([]CollaborationTargetSummary, error) {
	q := `SELECT id::text, display_name FROM space_agents WHERE tenant_id=$1 AND status='active'`
	args := []any{tenantID}
	if term := strings.TrimSpace(query); term != "" {
		args = append(args, likePattern(term))
		q += " AND display_name ILIKE $2"
	}
	q += " ORDER BY display_name, id"
	rows, err := d.pool.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CollaborationTargetSummary
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out = append(out, CollaborationTargetSummary{
			Type:                  "agent",
			ID:                    id,
			DisplayName:           name,
			InteractionDescriptor: InteractionDescriptor{Mode: "task", RequiresTask: true},
		})
	}
	return out, rows.Err()
}

// ResolveTarget confirms an agent id is an ACTIVE space_agents row in the tenant. Non-agent types
// and unknown/retired/foreign agent ids return ok=false (404 downstream); a retired agent never
// resolves (IssueRun D1 active-only). It is a short read on its own connection; run-create correctness
// is re-validated in-transaction by agentRunEvidence.
func (d *SpaceAgentDirectory) ResolveTarget(ctx context.Context, tenantID, targetType, targetID string) (CollaborationTargetSummary, bool, error) {
	if targetType != "agent" {
		return CollaborationTargetSummary{}, false, nil
	}
	var id, name string
	err := d.pool.QueryRowContext(ctx, `SELECT id::text, display_name FROM space_agents WHERE id=$1 AND tenant_id=$2 AND status='active'`, targetID, tenantID).Scan(&id, &name)
	if err == sql.ErrNoRows {
		return CollaborationTargetSummary{}, false, nil
	}
	if err != nil {
		return CollaborationTargetSummary{}, false, err
	}
	return CollaborationTargetSummary{
		Type:                  "agent",
		ID:                    id,
		DisplayName:           name,
		InteractionDescriptor: InteractionDescriptor{Mode: "task", RequiresTask: true},
	}, true, nil
}
