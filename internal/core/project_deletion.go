package core

// Deleting a Project is the one lifecycle action that has to work whatever state the Project is in.
// Everything here exists because the alternative — refusing until the Project reaches a state only a
// successful provisioning produces — makes the Projects that failed to provision permanently
// undeletable, which is exactly the state a member most wants to leave behind.
//
// Evidence for specs test-cases/cloud/project/space-scoping.md
// #deleting-a-project-supersedes-what-is-still-running-on-it.

// settleForDeletion makes this Project's next Operation able to be its last one, by ending every
// Operation of the Project that is still live. `queued` work no Controller has taken yet and work a
// Controller is holding are both ended here, in one statement, so that neither can be claimed nor
// dispatched again once the Project is gone: the Project would otherwise be provisioned *after* it
// was deleted.
//
// They end as `failed` with a reason code of this action's own, which is how the administrative
// force stop already records a cancellation a member did not ask for operation by operation. Nothing
// is deleted and nothing is rewritten: the request, the actor, the execution records and the attempt
// history stay exactly as they were, and the reason code is what tells the two cancellations apart
// in the audit.
//
// One live Operation is deliberately not settled, and it is refused instead: a deletion already in
// flight. Superseding it would leave the Project with two orchestrations competing for the same
// sandboxes, effects and rows, and the Project is already going away, so `operation_in_progress` is
// the accurate answer to a second request.
func settleForDeletion(t *transaction, pid string) {
	require(t.one("SELECT id FROM operations WHERE project_id=$1 AND kind='delete_project' AND state IN ('queued','running','retry_wait','blocked')", pid) == nil, 409, "operation_in_progress")
	// A Workspace being created for an Agent run carries that run in its lifecycle, and the run is
	// settled with it — after the Operation, as the force stop does — for the same reason: a
	// superseded creation must not leave a run the Store still believes is starting.
	runs := t.list(`SELECT DISTINCT w.issue_run_id FROM operations o JOIN workspaces w ON w.id=o.workspace_id
		WHERE o.project_id=$1 AND o.kind='create_workspace' AND o.state IN ('queued','running','retry_wait','blocked') AND w.issue_run_id IS NOT NULL`, pid)
	t.exec(`UPDATE operations SET state='failed',error_code='superseded_by_project_deletion',retry_at=NULL,version=version+1,updated_at=now()
		WHERE project_id=$1 AND state IN ('queued','running','retry_wait','blocked')`, pid)
	for _, r := range runs {
		runWorkspaceSettled(t, r.S("issueRunId"), "failed")
	}
}
