package core

import "strings"

// issueFormContext resolves the issue context a form descriptor may be tailored with: the repository
// and default branch of the project the issue belongs to, and the references the issue can name for
// itself (Issue collaboration D8).
//
// An empty issue id means the caller has no issue in hand — that is the plain
// `GET /collaboration/forms/{formRef}` — and yields the zero value, so no platform field is injected. A
// non-empty id is resolved through the tenant-scoped issue lookup, so an unknown or foreign issue is the
// same 404 it would be anywhere else rather than a silently dropped context.
func issueFormContext(t *transaction, tid, issueID string) IssueFormContext {
	if issueID == "" {
		return IssueFormContext{}
	}
	return formContextForIssue(t, tid, issue(t, tid, issueID))
}

// formContextForIssue is issueFormContext for a caller that already holds the issue row, so assist and
// confirm do not re-read what they just loaded.
func formContextForIssue(t *transaction, tid string, i Object) IssueFormContext {
	out := IssueFormContext{IssueID: i.S("id"), ParentIssueID: i.S("parentIssueId")}
	ref := i.S("projectRef")
	if !validID(ref) {
		return out
	}
	// `issues.project_ref` carries no foreign key — migration 0010 adds the column shape-only — so a
	// project that has since been deleted is a missing default, never an error. Reading the branch
	// alongside the repository costs nothing: it is the same row, and `default_branch` is NOT NULL, so
	// a project that exists always yields one.
	if project := t.one("SELECT repository_url, default_branch FROM "+projectView+" p WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", ref, tid); project != nil {
		out.ProjectID = ref
		out.RepositoryURL = project.S("repositoryUrl")
		out.DefaultBranch = project.S("defaultBranch")
	}
	return out
}

// interaction loads one interaction scoped to its tenant and issue; foreign/missing are 404.
func interaction(t *transaction, tid, iid, ixid string) Object {
	require(validID(ixid), 404, "not_found")
	o := t.one("SELECT * FROM issue_interactions WHERE id=$1 AND tenant_id=$2 AND issue_id=$3", ixid, tid, iid)
	require(o != nil, 404, "not_found")
	return o
}

// interactionList returns the minimal interaction spine for an issue: one row per selected @ target
// on each comment. Comments are 0..N interactions; an interaction is 0..1 initial run (runId).
func interactionList(t *transaction, r *PublicRequest) Object {
	issue(t, r.TenantID, r.IssueID)
	items := t.list("SELECT * FROM issue_interactions WHERE issue_id=$1 AND tenant_id=$2 ORDER BY created_at, id", r.IssueID, r.TenantID)
	return Object{"items": items, "nextCursor": ""}
}

// applyCommentTargets persists the typed @ targets attached to a newly created comment. Human Mention
// stores the typed target with no run; Task Mode (agent/team) enqueues the initial issue-owned run plus
// its context snapshot; Form Mode (workflow) records a *configured-but-unconfirmed* interaction with no
// run — Workflow execution happens only on an explicit Confirm (Issue collaboration D9). Each selected target
// becomes one issue_interactions row. Task runs are appended to *dispatches so the caller dispatches
// them after the comment transaction commits (the global advisory lock must not span external/observer
// writes).
func applyCommentTargets(t *transaction, i Object, r *PublicRequest, uid, commentID string, dispatches *[]dispatchTarget) {
	raw, ok := r.Body["targets"].([]any)
	if !ok {
		return // a plain comment with no targets
	}
	require(len(raw) <= 100, 400, "invalid_input")
	for _, item := range raw {
		m, ok := item.(map[string]any)
		require(ok, 400, "invalid_input")
		targetType, _ := m["type"].(string)
		targetID, _ := m["id"].(string)
		task, _ := m["task"].(string)
		require(validTargetType(targetType) && validID(targetID), 400, "invalid_target")
		mode, _ := modeForType(targetType)
		resolveCollaborationTarget(t, r.TenantID, targetType, targetID)

		var runID any
		if mode == "task" {
			require(strings.TrimSpace(task) != "" && len(task) <= 20000, 400, "task_required")
			var input Object
			if targetType == "agent" {
				if agent, ok := agentRunEvidence(t, r.TenantID, targetID); ok {
					// Real Space Agent (IssueRun D1): enforce the D2 project gate, and snapshot the
					// plugin identity/version before enqueueing. The gate panics 409 and rolls back the
					// whole comment transaction (no comment, no activity, no run) when it fails.
					agentRunGate(t, i, agent)
					input = buildRunContext(t, i, task, targetType, targetID, Object{}, nil)
					input = snapshotAgentRunInput(input, agent)
				} else {
					// Non-space_agents agent (dev/compat fixture): unchanged legacy behavior.
					input = buildRunContext(t, i, task, targetType, targetID, Object{}, nil)
				}
			} else {
				input = buildRunContext(t, i, task, targetType, targetID, Object{}, nil)
			}
			runObj := enqueueRun(t, r.TenantID, i.S("id"), targetType, targetID, input, "comment", commentID, "user", uid)
			runID = runObj.S("id")
			*dispatches = append(*dispatches, dispatchTarget{tenantID: r.TenantID, runID: runObj.S("id")})
		}
		// mode == "form": the interaction is recorded with no run and empty input. Selecting a Workflow is
		// not executing it; the frontend loads the descriptor from `formRef`, and only confirmInteraction
		// may create the execution intent.
		id := newID()
		t.exec("INSERT INTO issue_interactions(id,tenant_id,issue_id,comment_id,target_type,target_id,mode,task,run_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)", id, r.TenantID, i.S("id"), commentID, targetType, targetID, mode, task, runID)
	}
}

// confirmInteraction is the ONLY transition that turns a Workflow interaction into an execution intent
// (Issue collaboration D9). It re-resolves the target server-side, re-validates the submitted values, persists them as
// the interaction's Issue-owned input, creates the WorkflowRun and linked IssueRun, and claims the
// interaction with a compare-and-set on `run_id IS NULL`. WorkflowRun owns execution; this path never
// hands the linked IssueRun to the legacy dispatcher or performs an external effect (Issue collaboration D9).
//
// The values are validated against the schema of the *version being confirmed* alone, never against
// the live document, and against the same admission gate the other two entries use. Selecting a
// version has to be a real way to run it: a draft that moved on after the last publish must not turn
// an already-published version into an unconfirmable one, or the selection would be refused exactly
// when it matters. This mirrors the manual run entry, which also validates against the version it
// runs. The version is the form's own `snapshotId` field, whose option values are snapshot ids;
// leaving it unset means the newest published version, and a version that is not this workflow's is a
// 404 rather than a silent fallback. A workflow with nothing published cannot be confirmed at all:
// there is no frozen document to execute, and running the live draft is what the draft/published
// split forbids. The resolved version is frozen onto the run's input, so the run still says what it
// executed after the workflow has been edited or published again.
//
// `snapshotId` is always the platform's key: the issue form does not render an author declaration under
// it (see withoutPlatformOwnedDeclarations), the submission's value under it is the version selection,
// and a resolved version that declares the key itself is refused rather than executed. That refusal
// answers before the executor-slot gate: the version can never run, so naming a slot the initiator
// would have to configure first would send them to fix something that cannot make this confirm work.
// A submission
// still carrying the pre-rename `version` key is read under it, so the version the user picked is the
// one that runs, and the key is folded away before the values are validated — see
// foldLegacyVersionAnswer.
func confirmInteraction(t *transaction, r *PublicRequest, uid string) Object {
	it := interaction(t, r.TenantID, r.IssueID, r.InteractionID)
	require(it.S("mode") == "form", 409, "interaction_not_confirmable")
	require(it["runId"] == nil, 409, "interaction_already_confirmed")
	i := issue(t, r.TenantID, r.IssueID)
	targetType, targetID := it.S("targetType"), it.S("targetId")
	require(targetType == "workflow", 409, "interaction_not_confirmable")
	values := interactionValues(r.Body)
	w := workflow(t, r.TenantID, targetID)
	cursor := workflowVersionCursor(w.O("graph"), values)
	foldLegacyVersionAnswer(w.O("graph"), values)
	s := workflowRunSnapshot(t, r.TenantID, targetID, cursor)
	// `snapshotId` belongs to the platform in every `@` form, so the submitted value was read as a
	// version cursor — including when the live document still declares a variable of that name, which
	// it can no longer publish. A resolved version whose own graph declares that key would give the one
	// submission two meanings — the version the platform resolves and a value the workflow receives —
	// which is the ambiguity 20261021-snapshot-id-is-a-reserved-field.md D3 refuses to execute. Such
	// versions are stock: the publish gate no longer creates them (D1), and the selector no longer
	// offers them (D2), so this is the backstop for a cursor that reaches one anyway, or for a confirm
	// that submitted no cursor and resolved to the newest one. Substituting a selectable version instead
	// would execute something the user did not choose.
	if workflowDeclaresReservedField(s.O("graph")) {
		rejectWith(409, "workflow_version_reserved_field", Object{"snapshotId": s.S("id"), "field": fieldKeySnapshotID})
	}
	// Only a version that may actually run is worth asking the initiator to configure slots for. This is
	// the same gate the other entries use; it just runs after the reserved-key refusal above.
	requireWorkflowExecutorSlots(t, s.O("graph"), uid)
	issueContext := formContextForIssue(t, r.TenantID, i)
	// The chosen version's own schema decides what the submission may contain: the form offers the
	// selected version's fields, so a live draft that has moved on neither adds nor removes a question.
	validateFormValues(snapshotFormDescriptor(t, w, s, &issueContext), values, true)

	input := buildRunContext(t, i, "", targetType, targetID, values, appliedContextRefs(r.Body))
	runObj := enqueueWorkflowInvocation(t, r.TenantID, i.S("id"), w, s, values, input, formKickoff(s.O("graph"), values), "interaction", it.S("id"), uid)
	claimed := t.execRows("UPDATE issue_interactions SET input=$3, run_id=$4 WHERE id=$1 AND tenant_id=$2 AND run_id IS NULL", it.S("id"), r.TenantID, jsonText(values), runObj.S("id"))
	require(claimed == 1, 409, "interaction_already_confirmed")
	return Object{"resource": runObj}
}

// assistWorkflow serves AI Assist for a Workflow target *before* any interaction exists. The form is a
// draft until the user confirms it — selecting a workflow must leave no trace in the Timeline — so
// assist is deliberately stateless here: it resolves the target and its current descriptor, reads the
// issue context, and returns suggestions. It writes nothing: no comment, no interaction, no run, no
// context ref, no issue mutation (Issue collaboration D5/D9).
//
// Assist validates the same submission the confirm will, against the same descriptor, so it has to
// accept the same keys: a form rendered before the rename answers under the old `version` key, and the
// live descriptor no longer declares it. The fold is the confirm's own, taken from the one place the
// rule is defined (see foldLegacyVersionAnswer), rather than a second compatibility rule that could
// drift from it.
//
// The descriptor is the version's, not the live document's, whenever the submission names a published
// version or the workflow has one: the client renders the selected version's form even on its default,
// so assist has to answer for the fields that form shows. "The default" is the selector's default — the
// newest *selectable* version — not the newest published row: a stock version that claims the reserved
// `snapshotId` is withheld from the selector (20261021-snapshot-id-is-a-reserved-field.md D2), and
// judging the submission against it would refuse fields the visible form offers. A workflow with no
// selectable version has no selector and the live document is the form, exactly as it is when nothing
// is published.
func assistWorkflow(t *transaction, r *PublicRequest) Object {
	i := issue(t, r.TenantID, r.IssueID)
	targetID := r.Body.S("targetId")
	require(validID(targetID), 400, "invalid_target")
	issueContext := formContextForIssue(t, r.TenantID, i)
	w := workflow(t, r.TenantID, targetID)
	descriptor := workflowFormDescriptor(t, r.TenantID, "workflow", targetID, &issueContext)
	// Assist is requested while the form is still being filled, so shape is validated but requiredness
	// is not — an incomplete form is exactly when a suggestion is useful.
	current := interactionValues(r.Body)
	cursor := workflowVersionCursor(w.O("graph"), current)
	foldLegacyVersionAnswer(w.O("graph"), current)
	s := workflowLatestSelectableSnapshot(t, r.TenantID, targetID)
	if cursor != "" {
		s = workflowSnapshot(t, r.TenantID, targetID, cursor)
	}
	if s != nil {
		descriptor = snapshotFormDescriptor(t, w, s, &issueContext)
	}
	validateFormValues(descriptor, current, false)

	in := AssistInput{
		TargetType:       "workflow",
		TargetID:         targetID,
		Descriptor:       descriptor,
		CurrentValues:    current,
		IssueTitle:       i.S("title"),
		IssueDescription: i.S("description"),
		ContextRefs:      t.list("SELECT ref_type, ref_id FROM issue_context_refs WHERE issue_id=$1 AND tenant_id=$2 ORDER BY ref_type, created_at, id", i.S("id"), r.TenantID),
	}
	for _, c := range t.list("SELECT body FROM issue_comments WHERE issue_id=$1 AND tenant_id=$2 AND deleted_at IS NULL ORDER BY seq DESC LIMIT $3", i.S("id"), r.TenantID, maxRecentComments) {
		in.RecentComments = append(in.RecentComments, c.S("body"))
	}
	require(t.assist != nil, 503, "assist_unavailable")
	suggestion, err := t.assist.Suggest(t.ctx, in)
	if err != nil {
		panic(databaseFailure{err})
	}
	return assistSuggestionObject(descriptor, suggestion)
}
