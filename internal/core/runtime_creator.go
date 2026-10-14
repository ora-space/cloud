package core

// resolveCreatingOperation names the Operation that created `w`, filling `creatorOperationId` on a
// Workspace that does not carry it durably.
//
// Migration 0018 made the creating evidence immutable, but the creating transaction cannot write it.
// `workspaces.creator_operation_id` references `operations` and `operations.workspace_id` references
// `workspaces`; neither foreign key is deferrable, and the `runtime_creator_immutable` trigger
// refuses every later update of the trio, so the two rows cannot name each other at insert time.
// Cloud therefore creates the Workspace first — the order `createProject` and `createWorkspace`
// use — and every Workspace Cloud creates itself leaves the link empty.
//
// The link is resolved on read instead, with the rule migration 0018's backfill used: exactly one
// Operation exactly scoped to this Workspace, meaning the same workspace, project and tenant, of the
// kind that creates this Workspace's kind. One answer for every row, whether it was created before
// or after that migration, and durable evidence is never rewritten to produce it.
//
// Ambiguity resolves to nothing rather than to a guess. Naming an Operation that did not create the
// Workspace would attribute a failure to the wrong cause, which is worse than the empty link: a
// caller that reads no creating Operation shows what the Project's own state says.
func resolveCreatingOperation(t *transaction, w Object) {
	if w.S("creatorOperationId") != "" {
		return
	}
	created := t.list("SELECT o.id FROM operations o WHERE o.workspace_id=$1 AND o.project_id=$2 AND o.tenant_id=$3 AND o.kind=$4", w.S("id"), w.S("projectId"), w.S("tenantId"), creatingOperationKind(w.S("kind")))
	if len(created) == 1 {
		w["creatorOperationId"] = created[0].S("id")
	}
}

// workspaceDocument is the public Workspace document: the row a member may use, with its creating
// Operation named. Every read that returns a Workspace to a member goes through here, so the same
// Workspace cannot describe its creation differently depending on how it was fetched.
//
// `admin` keeps the administrator's stronger membership requirement of the underlying read.
func workspaceDocument(t *transaction, tid, uid, wid string, admin bool) Object {
	w := workspace(t, tid, uid, wid, admin)
	resolveCreatingOperation(t, w)
	return w
}

// creatingOperationKind is the Operation kind that creates a Workspace of this kind, or the empty
// string for a kind nothing creates — a Workspace whose creating Operation is unknown must not be
// matched against an Operation that happens to share its scope.
func creatingOperationKind(workspaceKind string) string {
	switch workspaceKind {
	case "main":
		return "create_project"
	case "isolated":
		return "create_workspace"
	default:
		return ""
	}
}
