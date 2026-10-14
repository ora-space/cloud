package core

// Resuming a new IssueRun from its Issue's latest Revision (issue-run 20261010 resume decision).
//
// Selection runs once, in the session-start transaction, and its result is frozen into the
// AgentSession spec; the Node restores exactly what the spec names and never queries Cloud for it.
// Runs on one Issue are assumed serial: two overlapping runs may select the same prior Revision.

// Session-end details a Node reports with AGENT_FAILED when it could not restore the prior Revision
// (contract decision D3). Only the base-unavailable one is permanent, so only it refuses the Revision
// for later selection (resume decision D3).
const (
	priorRevisionUnavailable     = "prior_revision_unavailable"
	priorRevisionBaseUnavailable = "prior_revision_base_unavailable"
)

// resumeChoice is the outcome of selecting a prior Revision for one session start.
type resumeChoice struct {
	// prior is the spec's `priorRevision{revisionId, finalCommit, bundle}`; nil starts fresh.
	prior Object
	// context is the initial turn's `resume` value; nil when the Issue has nothing to resume, which
	// keeps the first prompt of every run without history byte-identical to before.
	context Object
}

// selectPriorRevision picks the Revision the run resumes (resume decision D1): the latest Revision
// of another run of the same Issue that carries a bundle, its own or reused. It is skipped, and the
// run starts fresh, when it belongs to a different project or repository than this run's Workspace,
// or when an earlier restore refused it. There is deliberately no fallback to an older Revision:
// older ones usually depend on the same rewritten history, and falling back would silently drop the
// latest work.
func selectPriorRevision(t *transaction, o Object) resumeChoice {
	candidate := t.one(`
		SELECT r.id, r.run_id, r.project_id, r.repository_url, r.base_commit, r.final_commit,
		       r.resume_refused_at IS NOT NULL AS refused,
		       COALESCE(h.bundle_key, r.bundle_key) AS bundle_key,
		       COALESCE(h.bundle_size, r.bundle_size) AS bundle_size,
		       COALESCE(h.bundle_sha256, r.bundle_sha256) AS bundle_sha256
		FROM revisions r
		JOIN issue_runs ir ON ir.id = r.run_id AND ir.tenant_id = r.tenant_id
		LEFT JOIN revisions h ON h.id = r.bundle_revision_id
		WHERE ir.issue_id = $1 AND ir.tenant_id = $2 AND r.run_id <> $3
		  AND (r.bundle_key IS NOT NULL OR r.bundle_revision_id IS NOT NULL)
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT 1`, o.S("issueId"), o.S("tenantId"), o.S("id"))
	if candidate == nil {
		return resumeChoice{}
	}
	skipped := func(reason string) resumeChoice {
		return resumeChoice{context: Object{"skippedRevisionId": candidate.S("id"), "reason": reason}}
	}
	w := t.one(`
		SELECT w.project_id, p.repository_url FROM workspaces w JOIN projects p ON p.id = w.project_id
		WHERE w.id = $1 AND w.issue_run_id = $2 AND w.deleted_at IS NULL`, o.S("workspaceId"), o.S("id"))
	if w == nil || w.S("projectId") != candidate.S("projectId") || w.S("repositoryUrl") != candidate.S("repositoryUrl") {
		return skipped("project_changed")
	}
	if candidate.B("refused") {
		return skipped("resume_refused")
	}
	return resumeChoice{
		prior: Object{
			"revisionId":  candidate.S("id"),
			"finalCommit": candidate.S("finalCommit"),
			"bundle":      Object{"key": candidate.S("bundleKey"), "size": candidate.N("bundleSize"), "sha256": candidate.S("bundleSha256")},
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
// Revision the session spec carried, without the bundle, so the Node can tell a resumed run that
// added nothing (contract decision D4). Nil when the session started fresh.
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
// session-end settlement transaction, when the Node found its bundle's base commit gone from the
// remote (resume decision D3). Other restore failures may be transient and leave it selectable.
func refuseResumedRevision(t *transaction, sessionExecutionID string, ended Object) {
	if ended.S("reason") != "agent_failed" || ended.S("detail") != priorRevisionBaseUnavailable {
		return
	}
	prior := deliveryPriorRevision(t, sessionExecutionID)
	if prior == nil {
		return
	}
	t.exec(`UPDATE revisions SET resume_refused_at=now(), resume_refused_reason=$2
		WHERE id=$1 AND resume_refused_at IS NULL`, prior.S("revisionId"), priorRevisionBaseUnavailable)
}

// resumedBundleHolder returns the Revision that holds the bundle a resumed run reuses when it added
// nothing (resume decision D5): the prior itself when it stored one, else the holder it reused.
func resumedBundleHolder(t *transaction, priorRevisionID string) string {
	prior := t.one("SELECT id, bundle_key, bundle_revision_id FROM revisions WHERE id=$1", priorRevisionID)
	require(prior != nil, 409, "result_conflict")
	if prior.S("bundleKey") != "" {
		return prior.S("id")
	}
	require(prior.S("bundleRevisionId") != "", 409, "result_conflict")
	return prior.S("bundleRevisionId")
}
