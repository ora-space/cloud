package core

import "strings"

// runtimeUsable requires a current membership and the verified creator or current administrator.
// Durable project ownership and a historical operation actor never confer runtime content access.
func runtimeUsable(t *transaction, w Object, uid string) bool {
	m := membership(t, w.S("tenantId"), uid, false)
	return m.S("role") == "admin" || w.S("creatorUserId") == uid
}

// runtimeOverview is deliberately an allowlist: adding a database column cannot leak content.
func runtimeOverview(w Object, usable bool) Object {
	out := Object{"canUse": usable}
	for _, key := range []string{"id", "tenantId", "projectId", "ownerUserId", "kind", "creatorUserId", "creatorEvidence", "desiredState", "observedState", "runtimeGeneration", "version", "createdAt", "deletedAt"} {
		out[key] = w[key]
	}
	return out
}

// runtimeListing keeps shared safe summaries while exposing clone/task content only to its users.
func runtimeListing(t *transaction, r *PublicRequest, uid, pid string) Object {
	out := page(t, "SELECT w.*,wt.branch_name,task.title FROM workspaces w LEFT JOIN workspace_worktrees wt ON wt.workspace_id=w.id LEFT JOIN tasks task ON task.workspace_id=w.id WHERE w.project_id=$1 AND w.tenant_id=$2 AND w.deleted_at IS NULL AND w.issue_run_id IS NULL AND w.workflow_run_id IS NULL", []any{pid, r.TenantID}, "w.id", r)
	items, _ := out["items"].([]Object)
	for i, w := range items {
		usable := runtimeUsable(t, w, uid)
		if usable {
			w["canUse"] = true
		} else {
			items[i] = runtimeOverview(w, false)
		}
	}
	return out
}

// authorizeRuntimeReplay rechecks content authorization before returning a durable old response.
// Replayed success is historical evidence, never permission after an administrator is demoted.
func authorizeRuntimeReplay(t *transaction, r *PublicRequest, uid string) {
	if r.WorkspaceID != "" {
		workspace(t, r.TenantID, uid, r.WorkspaceID, false)
	}
	if r.OperationID != "" {
		ownedOperation(t, r, uid)
	}
	if r.SpaceID != "" && (r.Method == "POST" || r.Method == "DELETE") && strings.HasSuffix(r.Path, "/plugins") {
		requireSpaceRole(spaceMember(t, r.SpaceID, uid), "admin")
	}
}
