package core

// projectView reads a Project together with its Binding's repository under the Project's
// published field names, so the Project resource, the Controller snapshot and the clone step keep
// their shape. Phase one has exactly one Binding per Project, created in the same transaction.
const projectView = `(SELECT p.*,b.repository_url,b.branch AS default_branch,b.credential_ref_id
 FROM projects p JOIN repository_bindings b ON b.project_id=p.id)`

// Repository Operation types (contract D5 item 3).
const (
	RepositorySync    = "repository_sync"
	RepositoryPublish = "repository_publish"
)

// unfinishedRepositoryStates are the states of both Operation types that have not ended. An
// `uncertain` publish is unfinished but has a bounded number of checks, so every Operation ends.
const unfinishedRepositoryStates = `('awaiting_approval','pending','leased','running','uncertain')`

// createBinding records a new Project's external repository and the sync that must complete before
// any Workspace can clone from a snapshot. Both are in the creating transaction, so every Project
// has the shape both development lines expect from the start.
func createBinding(t *transaction, tenantID, projectID, repositoryURL, branch string, credential any) {
	bid := newID()
	t.exec("INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch,credential_ref_id) VALUES($1,$2,$3,$4,$5,$6)", bid, tenantID, projectID, repositoryURL, branch, credential)
	t.exec("INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger) VALUES($1,$2,$3,$4,$5,'pending','project_created')", newID(), RepositorySync, tenantID, projectID, bid)
}

// repositoryOperationsUnfinished reports whether the Project has any sync or publish that has not
// ended. delete_project waits for none, and the two development lines share this one definition.
func repositoryOperationsUnfinished(t *transaction, projectID string) bool {
	return t.one("SELECT id FROM repository_operations WHERE project_id=$1 AND state IN "+unfinishedRepositoryStates+" LIMIT 1", projectID) != nil
}

// cancelRepositoryOperationsForDeletion runs in the transaction that moves a Project to deleting.
// Operations that never reached running are cancelled, so no new Git Server write can start;
// running and uncertain ones settle normally and delete_project waits for them. The resync request
// is dropped: a deleting Project creates no Operation. It reports whether a sync was owed — a
// queued sync was cancelled or a resync was requested — so an aborted deletion can restore it.
func cancelRepositoryOperationsForDeletion(t *transaction, projectID string) (syncOwed bool) {
	syncOwed = t.one("SELECT id FROM repository_operations WHERE project_id=$1 AND type='repository_sync' AND state IN ('pending','leased') LIMIT 1", projectID) != nil ||
		t.one("SELECT id FROM repository_bindings WHERE project_id=$1 AND resync_requested LIMIT 1", projectID) != nil
	t.exec("UPDATE repository_operations SET state='cancelled',error_code='project_deleting',finished_at=clock_timestamp() WHERE project_id=$1 AND state IN ('pending','leased','awaiting_approval')", projectID)
	t.exec("UPDATE repository_bindings SET resync_requested=false WHERE project_id=$1", projectID)
	return syncOwed
}

// restoreRepositorySync runs in the transaction that returns a Project from deleting to active
// when its deletion owed a sync. Each Binding gets that sync back: a pending `deletion_aborted`
// sync, or, while a sync that was already running still holds the one-unfinished-sync slot, the
// resync request it will pick up. Cancelled publishes stay cancelled; the user re-creates them.
func restoreRepositorySync(t *transaction, projectID string) {
	for _, b := range t.list("SELECT id,tenant_id FROM repository_bindings WHERE project_id=$1 ORDER BY id", projectID) {
		if t.one("SELECT id FROM repository_operations WHERE binding_id=$1 AND type='repository_sync' AND state IN ('pending','leased','running')", b.S("id")) != nil {
			t.exec("UPDATE repository_bindings SET resync_requested=true WHERE id=$1", b.S("id"))
			continue
		}
		t.exec("INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger) VALUES($1,$2,$3,$4,$5,'pending','deletion_aborted')", newID(), RepositorySync, b.S("tenantId"), projectID, b.S("id"))
	}
}

// settleFailedProjectCreation makes a Project whose create_project failed usable: whatever the
// cause, the Project never stays in provisioning. Its Workspace keeps its own failure state.
func settleFailedProjectCreation(t *transaction, projectID string) {
	t.exec("UPDATE projects SET lifecycle='active',version=version+1 WHERE id=$1 AND lifecycle='provisioning'", projectID)
}
