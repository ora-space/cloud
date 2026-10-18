package core

import "strings"

// targetSummaryObject converts a typed CollaborationTargetSummary into the wire Object shape. The
// interface returns plain strings, so the fields survive Object.S(); the descriptor is collapsed to
// {mode, requiresTask} as the frozen wire contract (Issue collaboration D3).
func targetSummaryObject(s *CollaborationTargetSummary) Object {
	return Object{
		"type":        s.Type,
		"id":          s.ID,
		"displayName": s.DisplayName,
		"description": s.Description,
		"interactionDescriptor": Object{
			"mode":         s.InteractionDescriptor.Mode,
			"requiresTask": s.InteractionDescriptor.RequiresTask,
			"formRef":      formRefOrNil(s.InteractionDescriptor.FormRef),
		},
	}
}

// formRefOrNil keeps the closed OpenAPI shape: the key is always present, null when the mode has no form.
func formRefOrNil(ref string) any {
	if ref == "" {
		return nil
	}
	return ref
}

// collaborationTargetList is the read-only Issues-facing target projection (Issue collaboration D3). Humans come from
// active tenant members (reused, not re-invented); agent/team/workflow come from the directory when
// wired. It is NOT an Agent/Team/Workflow CRUD surface.
func collaborationTargetList(t *transaction, r *PublicRequest) Object {
	term := strings.TrimSpace(r.Query)
	if term != "" {
		require(len(term) <= 200, 400, "invalid_query")
	}
	items := []Object{}
	q := "SELECT DISTINCT m.user_id AS id, u.display_name AS display_name FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL"
	args := []any{r.TenantID}
	if term != "" {
		args = append(args, likePattern(term))
		q += " AND u.display_name ILIKE $2"
	}
	q += " ORDER BY u.display_name, m.user_id"
	for _, u := range t.list(q, args...) {
		items = append(items, targetSummaryObject(&CollaborationTargetSummary{
			Type:                  "user",
			ID:                    u.S("id"),
			DisplayName:           u.S("displayName"),
			InteractionDescriptor: InteractionDescriptor{Mode: "mention", RequiresTask: false},
		}))
	}
	if t.directory != nil {
		list, err := t.directory.ListTargets(t.ctx, r.TenantID, term)
		if err != nil {
			panic(databaseFailure{err})
		}
		for i := range list {
			items = append(items, targetSummaryObject(&list[i]))
		}
	}
	return Object{"items": items, "nextCursor": ""}
}

// resolveCollaborationTarget confirms that a selected target exists and is usable from this tenant.
// Humans must be active members; agent/team/workflow must resolve through the directory (Unavailable
// directory => 404 target_not_found, never cross-tenant leakage). Agents are authoritative and
// against the space_agents roster first (IssueRun D1), falling through to the directory only for ids
// that are not a real Space Agent (dev/compat fixtures). Rejects via panic like the rest of the core
// helpers.
func resolveCollaborationTarget(t *transaction, tid, targetType, targetID string) {
	require(validTargetType(targetType) && validID(targetID), 400, "invalid_target")
	if targetType == "user" {
		require(t.one("SELECT m.user_id FROM tenant_memberships m JOIN users u ON u.id=m.user_id WHERE m.tenant_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL", tid, targetID) != nil, 404, "target_not_found")
		return
	}
	if targetType == "agent" {
		// Only an ACTIVE space_agents row resolves; resolve in-transaction so the run's atomic read of
		// the same row cannot be raced by a retire. Unknown/foreign/retired ids fall through below.
		if _, ok := agentRunEvidence(t, tid, targetID); ok {
			return
		}
	}
	if t.directory == nil {
		reject(404, "target_not_found")
	}
	_, ok, err := t.directory.ResolveTarget(t.ctx, tid, targetType, targetID)
	if err != nil {
		panic(databaseFailure{err})
	}
	require(ok, 404, "target_not_found")
}
