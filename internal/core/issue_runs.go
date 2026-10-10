package core

// agentRunSkeletonFields are the B-skeleton-only columns added to issue_runs and
// workspaces by migration 0018. The schema and the D6 business hooks land ahead of
// the feature that owns them (dispatcher, OpenAPI contract, Thread API); until then
// they are deliberately not part of the public resource shape, so they are removed
// before an issue_runs or workspaces resource reaches the API. The AgentRunDispatcher
// that reads and writes them is a later change and reads through the same transaction
// helpers, not through these shaping functions.
var agentRunSkeletonFields = []string{"phase", "workspaceId", "cancelRequestedAt", "threadState", "idleSince", "issueRunId"}

// stripAgentRunSkeleton removes the migration-0018 agent columns from a resource in
// place and returns it, keeping the public contract byte-identical for now.
func stripAgentRunSkeleton(o Object) Object {
	for _, k := range agentRunSkeletonFields {
		delete(o, k)
	}
	return o
}

// run loads a live run scoped to its issue and tenant; foreign/deleted/missing are all 404.
func run(t *transaction, tid, iid, rid string) Object {
	require(validID(rid), 404, "not_found")
	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL", rid, tid, iid)
	require(o != nil, 404, "not_found")
	return withRevision(t, stripAgentRunSkeleton(o))
}

func runList(t *transaction, r *PublicRequest) Object {
	issue(t, r.TenantID, r.IssueID)
	items := t.list("SELECT * FROM issue_runs WHERE issue_id=$1 AND tenant_id=$2 AND deleted_at IS NULL ORDER BY created_at, id", r.IssueID, r.TenantID)
	for _, o := range items {
		withRevision(t, stripAgentRunSkeleton(o))
	}
	return Object{"items": items, "nextCursor": ""}
}

// withRevision projects the run's registered Revision as public metadata (Cloud Revision D5): the
// final and base commits, whether the run changed anything, and the object sizes. Object keys, the
// revision ref and digests stay private — the public API offers no download, and a key would name
// a private storage location. `revision` is null for a run that registered none (not an agent run,
// delivery still pending, skipped or failed); the run's `result.deliveryState` says which.
//
// `changed` says whether this run stored a bundle of its own: a resumed run that added nothing reuses
// its prior Revision's bundle and is unchanged (resume decision D5). `priorRevisionId` names the
// Revision the run resumed; the run's own `resumeRevisionId` shows it from session start on.
func withRevision(t *transaction, o Object) Object {
	o["revision"] = nil
	if o.S("executorType") != "agent" {
		return o
	}
	rev := t.one(`SELECT id, base_commit, final_commit, bundle_key IS NOT NULL AS stored_bundle, bundle_size,
		history_size, prior_revision_id, created_at
		FROM revisions WHERE run_id=$1 AND tenant_id=$2`, o.S("id"), o.S("tenantId"))
	if rev == nil {
		return o
	}
	o["revision"] = Object{
		"id":              rev.S("id"),
		"baseCommit":      rev.S("baseCommit"),
		"finalCommit":     rev.S("finalCommit"),
		"changed":         rev.B("storedBundle"),
		"bundleSize":      rev["bundleSize"],
		"historySize":     rev["historySize"],
		"priorRevisionId": rev["priorRevisionId"],
		"createdAt":       rev["createdAt"],
	}
	return o
}

// createRun enqueues a manual IssueRun record. It never dispatches this wave: a manual run persists
// as `queued` (the least misleading behavior). For executor_type='agent' the executor is hardened to
// the same authoritative resolution and snapshot as the comment/@agent path, instead of accepting any
// UUID shape: a real Space Agent is resolved in-transaction against space_agents and its plugin
// identity/version is snapshotted into the run input as authoritative (IssueRun D1/D6); an id that is
// not a real Space Agent falls through to the directory (dev/compat fixtures) or is rejected 404
// without leaking whether it exists. Team/workflow keep the opaque UUID shape, resolved by ports later.
func createRun(t *transaction, r *PublicRequest, uid string) Object {
	i := issue(t, r.TenantID, r.IssueID)
	executorType := r.Body.S("executorType")
	require(executorType == "agent" || executorType == "team" || executorType == "workflow", 400, "invalid_executor")
	executorID := r.Body.S("executorId")
	require(validID(executorID), 400, "invalid_executor")
	input := r.Body.O("input")
	if executorType == "agent" {
		// Authoritative Space Agent (IssueRun D1): reuses the same evidence resolver as the comment
		// path. On success the snapshot overwrites any caller-supplied plugin identity/version so they
		// can never be spoofed (IssueRun D6 authoritative server fields win). Otherwise fall through to
		// the directory exactly like the comment/@agent path; an unresolvable id (retired/foreign) is a
		// 404 with no existence leak.
		if agent, ok := agentRunEvidence(t, r.TenantID, executorID); ok {
			input = snapshotAgentRunInput(input, agent)
		} else {
			resolveCollaborationTarget(t, r.TenantID, "agent", executorID)
		}
	}
	return enqueueRun(t, r.TenantID, i.S("id"), executorType, executorID, input, "", nil, "user", uid)
}
