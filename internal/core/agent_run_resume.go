package core

// Resuming a new IssueRun from its Issue's latest Revision (issue-run 20261010 resume decision).
//
// Selection runs once when the ready-based startup budget is frozen, and its result is copied into the
// AgentSession spec; the Node restores exactly what the spec names and never queries Cloud for it.
// Runs on one Issue are assumed serial: two overlapping runs may select the same prior Revision.

// Session-end details a Node reports with AGENT_FAILED when it could not restore the prior Revision
// (contract decision D3). Only the base-unavailable one, which now means the prior's delivery ref
// could not be fetched from the Git Server, is permanent, so only it refuses the Revision for later
// selection (resume decision D3).
const (
	priorRevisionUnavailable     = "prior_revision_unavailable"
	priorRevisionBaseUnavailable = "prior_revision_base_unavailable"
)

// resumeChoice is the outcome of selecting a prior Revision for one session start.
type resumeChoice struct {
	// prior is the session spec's `priorRevision{revisionId, finalCommit, baseCommit,
	// gitRepositoryUrl, deliveryRef}`; nil starts fresh.
	prior Object
	// context is the initial turn's `resume` value; nil when the Issue has nothing to resume, which
	// keeps the first prompt of every run without history byte-identical to before.
	context Object
}

// selectPriorRevision picks the Revision the run resumes (resume decision D1 as changed by Revision
// D7): the latest Revision of another run of the same Issue that has its own verified delivery ref
// and moved off its base commit. A Revision that only holds its base commit would turn the new
// checkout back to older code, and one registered before Git delivery has no ref to fetch; neither
// is a candidate. The candidate is skipped, and the run starts fresh, when it belongs to a different
// project or repository than this run's Workspace, when the Git Server repository it would be
// fetched from is not the one this Workspace cloned from (contract D8), or when an earlier restore
// refused it. There is deliberately no fallback to an older Revision: older ones usually depend on
// the same rewritten history, and falling back would silently drop the latest work.
func selectPriorRevision(t *transaction, o Object) resumeChoice {
	candidate := t.one(`
		SELECT r.id, r.run_id, r.tenant_id, r.project_id, r.repository_url, r.base_commit, r.final_commit, r.delivery_ref,
		       r.resume_refused_at IS NOT NULL AS refused
		FROM revisions r
		JOIN issue_runs ir ON ir.id = r.run_id AND ir.tenant_id = r.tenant_id
		WHERE ir.issue_id = $1 AND ir.tenant_id = $2 AND r.run_id <> $3
		  AND r.delivery_ref IS NOT NULL AND r.final_commit <> r.base_commit
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT 1`, o.S("issueId"), o.S("tenantId"), o.S("id"))
	if candidate == nil {
		return resumeChoice{}
	}
	skipped := func(reason string) resumeChoice {
		return resumeChoice{context: Object{"skippedRevisionId": candidate.S("id"), "reason": reason}}
	}
	w := t.one(`
		SELECT w.project_id, b.repository_url, w.base_commit_id FROM workspaces w JOIN repository_bindings b ON b.project_id = w.project_id
		WHERE w.id = $1 AND w.issue_run_id = $2 AND w.deleted_at IS NULL`, o.S("workspaceId"), o.S("id"))
	if w == nil || w.S("projectId") != candidate.S("projectId") || w.S("repositoryUrl") != candidate.S("repositoryUrl") {
		return skipped("project_changed")
	}
	if candidate.B("refused") {
		return skipped("resume_refused")
	}
	// The session restores the prior from the same Git Server repository the checkout was cloned
	// from; both addresses come from one configuration and one Project, so a difference means the
	// deployment moved its Git Server and the prior cannot be fetched where the checkout lives.
	clone := t.one(`SELECT input FROM clone_executions WHERE workspace_id=$1 AND result->>'outcome'='clone_ready' AND result->>'commit'=$2
		ORDER BY created_at DESC, execution_id DESC LIMIT 1`, o.S("workspaceId"), w.S("baseCommitId"))
	var repository string
	if g := t.store.GitServer; g != nil {
		repository = g.gitRepositoryURL(candidate.S("tenantId"), candidate.S("projectId"))
	}
	if clone == nil || repository == "" || clone.O("input").S("repositoryUrl") != repository {
		return skipped("repository_changed")
	}
	return resumeChoice{
		prior: Object{
			"revisionId":       candidate.S("id"),
			"finalCommit":      candidate.S("finalCommit"),
			"baseCommit":       candidate.S("baseCommit"),
			"gitRepositoryUrl": repository,
			"deliveryRef":      candidate.S("deliveryRef"),
		},
		context: Object{
			"revisionId":  candidate.S("id"),
			"runId":       candidate.S("runId"),
			"finalCommit": candidate.S("finalCommit"),
			"baseCommit":  candidate.S("baseCommit"),
		},
	}
}

// deliveryPriorRevision is the delivery spec's `priorRevision{revisionId, finalCommit}`: the same
// Revision the session spec carried, without where to fetch it, so the Node can tell a resumed run
// that added nothing (contract decision D4). Nil when the session started fresh.
func deliveryPriorRevision(t *transaction, sessionExecutionID string) Object {
	e := t.one("SELECT input FROM node_executions WHERE execution_id=$1 AND kind='agent_session'", sessionExecutionID)
	if e == nil {
		return nil
	}
	prior := e.O("input").O("priorRevision")
	if prior.S("revisionId") == "" {
		return nil
	}
	return Object{"revisionId": prior.S("revisionId"), "finalCommit": prior.S("finalCommit")}
}

// refuseResumedRevision marks the session's prior Revision as never to be selected again, in the
// session-end settlement transaction, when the Node could not fetch its delivery ref from the Git
// Server (resume decision D3 as changed by Revision D7). A run that already asked to cancel is
// exempt: the Git Server refuses a cancelled run's reads with the same answer it gives for a
// missing ref, so the failure proves nothing about the Revision. Other restore failures may be
// transient and leave it selectable.
func refuseResumedRevision(t *transaction, sessionExecutionID string, ended Object) {
	if ended.S("reason") != "agent_failed" || ended.S("detail") != priorRevisionBaseUnavailable {
		return
	}
	e := t.one("SELECT r.cancel_requested_at IS NOT NULL AS cancelled FROM node_executions e JOIN issue_runs r ON r.id=e.operation_id WHERE e.execution_id=$1", sessionExecutionID)
	if e == nil || e.B("cancelled") {
		return
	}
	prior := deliveryPriorRevision(t, sessionExecutionID)
	if prior == nil {
		return
	}
	t.exec(`UPDATE revisions SET resume_refused_at=now(), resume_refused_reason=$2
		WHERE id=$1 AND resume_refused_at IS NULL`, prior.S("revisionId"), priorRevisionBaseUnavailable)
}
