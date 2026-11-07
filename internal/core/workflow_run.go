package core

import "strings"

// workflowRun loads a live run scoped to its workflow and tenant; a foreign, deleted
// or missing run is 404, and the workflow gate keeps archived workflows (and with them
// their whole run history) out of every read.
func workflowRun(t *transaction, tid, wid, rid string) Object {
	require(validID(rid), 404, "not_found")
	workflow(t, tid, wid)
	o := t.one("SELECT * FROM workflow_runs WHERE id=$1 AND tenant_id=$2 AND workflow_id=$3 AND deleted_at IS NULL", rid, tid, wid)
	require(o != nil, 404, "not_found")
	return composeWorkflowRun(t, o)
}

// workflowRunList reads a workflow's run history newest first.
//
// The walk is ordered by (created_at, id) rather than by id: run ids are random UUIDs, so an
// id-ordered page hands the reader an arbitrary window of the history — the newest run need not
// be in it at all, and the frontend cannot repair that by sorting the page it received. created_at
// is not unique (two runs can share an instant, and a batch of runs created in one test or one
// import frequently does), so id breaks the tie and keeps the order total. The public cursor stays
// the opaque row UUID the page envelope reports; the anchor row's created_at is what resolves the
// position, the same shape the member tenant list uses (pageByCreation).
func workflowRunList(t *transaction, r *PublicRequest) Object {
	w := workflow(t, r.TenantID, r.WorkflowID)
	pageOut := pageNewestRunFirst(t, runHistoryScope{tenantID: r.TenantID, workflowID: w.S("id")}, r)
	// window builds its items in-process as []Object (not []any), so assert that exact type.
	raw, _ := pageOut["items"].([]Object)
	items := make([]any, 0, len(raw))
	for _, row := range raw {
		s := workflowRunSummary(row)
		// The route is already per-workflow, so the label is the same on every row; stamping it
		// keeps list rows schema-identical to a detail read without a per-row join.
		s["workflowName"] = w.S("name")
		items = append(items, s)
	}
	return Object{"items": items, "nextCursor": pageOut.S("nextCursor")}
}

// runHistoryScope is the one history a cursor may position: the walk of a single workflow inside
// the tenant that owns it. The cursor is an opaque run UUID, so the page boundary has to be
// resolved from the run it names; resolving that run without this scope would let an id from
// another workflow, another tenant or a purged row move the boundary of the history being read,
// which is a creation-time side channel out of the caller's own history.
type runHistoryScope struct {
	tenantID   string
	workflowID string
}

// pageNewestRunFirst reads one page of a workflow's run history, newest first, and reports the row
// that continues it.
//
// The walk is ordered by (created_at, id) rather than by id: run ids are random UUIDs, so an
// id-ordered page is an arbitrary window of the history. created_at is not unique (a batch of runs
// created in one import or one test frequently shares an instant), so id breaks the tie and keeps
// the order total.
//
// The public cursor stays the opaque row UUID; the anchor row's created_at is what resolves its
// position. The tuple comparison is strict, so a page boundary that falls inside one created_at
// instant neither repeats the anchor row nor skips the rows after it: the tie is decided by id on
// both sides. The anchor is resolved only inside the scope being paged, so an id that names no live
// run of this workflow compares as NULL and answers an empty page — the same answer as an unknown
// cursor — rather than anchoring the walk outside the history it belongs to.
func pageNewestRunFirst(t *transaction, scope runHistoryScope, r *PublicRequest) Object {
	args := []any{scope.tenantID, scope.workflowID}
	q := "SELECT r.* FROM workflow_runs r WHERE r.tenant_id=$1 AND r.workflow_id=$2 AND r.deleted_at IS NULL"
	if r.After != "" {
		require(validID(r.After), 400, "invalid_cursor")
		args = append(args, r.After)
		after := itoa(len(args))
		q += " AND (r.created_at, r.id) < ((SELECT a.created_at FROM workflow_runs a" +
			" WHERE a.id=$" + after + "::uuid AND a.tenant_id=$1 AND a.workflow_id=$2 AND a.deleted_at IS NULL)," +
			" $" + after + "::uuid)"
	}
	return window(t, q+" ORDER BY r.created_at DESC, r.id DESC", args, r)
}

// latestWorkflowRuns labels a page of workflows with their newest run in one query, so a list of
// workflows costs one extra read rather than one per row. Each lateral lookup takes one row from
// workflow_run_list in the same (created_at, id) order the run history walks, without sorting or
// materializing every run of the page's workflows (including their large JSON fields).
func latestWorkflowRuns(t *transaction, tid string, wids []string) map[string]any {
	out := make(map[string]any, len(wids))
	if len(wids) == 0 {
		return out
	}
	args := []any{tid}
	values := make([]string, len(wids))
	for i, wid := range wids {
		args = append(args, wid)
		values[i] = "($" + itoa(len(args)) + "::uuid)"
	}
	q := "SELECT w.id AS workflow_id, r.id, r.name, r.status, r.snapshot_id," +
		" r.created_at, r.started_at, r.finished_at FROM (VALUES " + strings.Join(values, ",") + ") AS w(id)" +
		" JOIN LATERAL (SELECT id, name, status, snapshot_id, created_at, started_at, finished_at" +
		" FROM workflow_runs WHERE workflow_id=w.id AND tenant_id=$1 AND deleted_at IS NULL" +
		" ORDER BY created_at DESC, id DESC LIMIT 1) AS r ON true"
	for _, row := range t.list(q, args...) {
		out[row.S("workflowId")] = workflowLatestRunSummary(row)
	}
	return out
}

// workflowLatestRunSummary is the compact run projection a workflow row carries: enough to label
// when the workflow last ran, how that run ended and which version it executed, and to link to it.
// The history list answers a different question and keeps the node states and inputs per row; a
// workflow row must not pay for them.
func workflowLatestRunSummary(row Object) Object {
	// startedAt and finishedAt are nullable columns and the label shows "not yet" for them, so the
	// raw value passes through rather than becoming an empty string that is not a date-time.
	return Object{
		"id":         row.S("id"),
		"name":       row.S("name"),
		"status":     row.S("status"),
		"snapshotId": row.S("snapshotId"),
		"createdAt":  row.S("createdAt"),
		"startedAt":  row["startedAt"],
		"finishedAt": row["finishedAt"],
	}
}

// workflowRunSummary is the row projection shared by list and detail: the run's own fields,
// kept identical so a history row and a detail page never disagree about what a run is.
func workflowRunSummary(run Object) Object {
	rounds, _ := run["rounds"].([]any)
	// The OpenAPI schema declares both run timestamps nullable, and the page relies on a nil
	// value to mean "not yet reached". S() would turn a NULL column into "", which is not a
	// date-time and would render as an empty timestamp; pass the raw value through instead.
	return Object{
		"id":         run.S("id"),
		"tenantId":   run.S("tenantId"),
		"workflowId": run.S("workflowId"),
		"snapshotId": run.S("snapshotId"),
		"name":       run.S("name"),
		"status":     run.S("status"),
		"input":      run.O("input"),
		"nodeStates": run.O("nodeStates"),
		"rounds":     rounds,
		"error":      run.S("error"),
		"startedAt":  run["startedAt"],
		"finishedAt": run["finishedAt"],
		"createdAt":  run.S("createdAt"),
		"updatedAt":  run.S("updatedAt"),
		// The user's own D6 request, made through POST …/runs/{rid}/cancel: the run page has to be
		// able to show that a cancellation is pending, and a repeat of the request reads it back.
		"cancelRequestedAt": run["cancelRequestedAt"],
		// The plugin version frozen at creation (D3) is part of the run's identity, so readers see
		// exactly what the run executes with, not what the space declares today. Raw pass-through:
		// an unfrozen run (a row from before 0042, or one whose space declared no plugin) has NULL,
		// and the API reports that absence as JSON null rather than an empty version string.
		"pluginVersion":    run["pluginVersion"],
		"delivery":         Object{"state": run.S("deliveryState"), "error": run.S("deliveryError"), "revisionId": run["deliveryRevisionId"], "startedAt": run["deliveryStartedAt"], "settledAt": run["deliverySettledAt"]},
		"workspaceRelease": Object{"state": run.S("workspaceReleaseState"), "error": run.S("workspaceReleaseError"), "releasedAt": run["workspaceReleasedAt"]},
	}
}

// composeWorkflowRun joins the workflow label and the frozen snapshot graph onto the run row.
// Snapshots are immutable and never soft-deleted, so the graph a run rendered cannot drift or
// vanish even after the workflow's live document has moved on.
func composeWorkflowRun(t *transaction, run Object) Object {
	summary := workflowRunSummary(run)
	graph := Object{}
	if s := t.one("SELECT graph FROM workflow_snapshots WHERE id=$1", run.S("snapshotId")); s != nil {
		graph = s.O("graph")
	}
	// The run row stores only the workflow id; the name makes detail pages legible without a
	// client-side join. Snapshots freeze their own name too, but the workflow's live name is
	// the stable label for a run that belongs to that workflow.
	workflowName := ""
	if w := t.one("SELECT name FROM workflows WHERE id=$1 AND tenant_id=$2", run.S("workflowId"), run.S("tenantId")); w != nil {
		workflowName = w.S("name")
	}
	summary["workflowName"] = workflowName
	summary["definitionSnapshot"] = graph
	// The frozen per-slot bindings (mappings decision D3: the run uses these forever). Detail
	// read only — the history list stays a plain projection of the run row, like definitionSnapshot.
	summary["modelBindings"] = workflowRunBindings(t, run.S("id"))
	return summary
}

// workflowRunBindings projects a run's frozen per-slot model bindings, ordered by slot name so
// the detail page reads deterministically. `revoked` is the only mutable part: the binding row
// itself (connection name, model, protocol) is the frozen truth the session will run against.
func workflowRunBindings(t *transaction, runID string) []any {
	rows := t.list("SELECT slot_name, connection_name, model, revoked_at FROM run_model_bindings WHERE workflow_run_id=$1 ORDER BY slot_name", runID)
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, Object{
			"slot":           r.S("slotName"),
			"connectionName": r.S("connectionName"),
			"modelId":        r.O("model").S("id"),
			"modelName":      r.O("model").S("name"),
			"revoked":        r["revokedAt"] != nil,
		})
	}
	return out
}

// createWorkflowRun records one execution against one frozen snapshot. Cloud has no
// engine, so the row starts `pending`; a wired Store.WorkflowRunSimulator (dev fixtures
// only) fills the trace and advances the status inside the same transaction — a run is
// never half-persisted. Drafts are disallowed: running the live document would let an
// edit change what an already-created run "executed", so a workflow with no published
// snapshot is a hard error, exactly like desktop's workflow_no_published_snapshot.
//
// The snapshot, the input and the executor slots all go through the shared admission gate, so this
// entry accepts exactly what a manual workflow IssueRun and a confirmed @ interaction accept. The
// initiator matters here: the executor slots are resolved against *their* personal configuration,
// which is why the uid is part of the request rather than inferred later.
//
// The run's Project comes from the caller (D3): see resolveWorkflowRunProject for why it is stated
// rather than inferred, and why a request that names none is refused rather than accepted. The plugin
// version and its release package are read here rather than at session start for the same reason —
// see resolveWorkflowRunPlugin — so that upgrading the space's plugin while the run is in flight
// cannot change what it executes.
func createWorkflowRun(t *transaction, r *PublicRequest, uid string) Object {
	w := workflow(t, r.TenantID, r.WorkflowID)
	name := w.S("name")
	if n := r.Body.S("name"); n != "" {
		name = validText(n, 200)
	}
	s := admitWorkflowRun(t, r.TenantID, r.WorkflowID, r.Body.S("snapshotId"), uid)
	input := r.Body.O("input")
	// The invocation identity freezes here (D3): created_by is the aggregate's actor for the whole
	// run — the run Workspace lifecycle reads it instead of re-resolving who pressed Run. The
	// authenticated Identity, not a /users path parameter, is who that is. The project freezes in the
	// same statement, so the two halves of the identity can never disagree about who ran what where.
	require(r.Identity != nil, 401, "user_credential_required")
	createdBy := identityWithAlias(t, r.Identity).S("id")
	created := insertWorkflowRun(t, r.TenantID, r.WorkflowID, s, input, name, createdBy, r.Body.S("projectId"))
	id := created.S("id")
	if t.simulator != nil {
		nodeStates, rounds, status := t.simulator.SimulateWorkflowRun(s.O("graph"), input)
		if t.execRows("UPDATE workflow_runs SET node_states=$1,rounds=$2,status=$3,started_at=now(),finished_at=now(),version=version+1,updated_at=now() WHERE id=$4", jsonText(nodeStates), jsonText(rounds), status, id) > 0 {
			projectWorkflowRunToIssueRun(t, r.TenantID, id, workflowIssueOutputSummary(s.O("graph"), nodeStates, ""))
		}
	} else {
		// The run-creation gate (mappings decision D2): every slot the snapshot references must
		// resolve against the invoker's personal configuration before the run exists, and each
		// resolution freezes one binding. Same transaction as the run INSERT — any gap rolls back
		// the whole creation, so no run ever survives with an unresolved slot. A simulated run
		// (dev fixtures) filled its whole trace above and never executes, so it freezes nothing.
		freezeWorkflowRunExecutors(t, id, createdBy, s.O("graph"))
	}
	return composeWorkflowRun(t, t.one("SELECT * FROM workflow_runs WHERE id=$1 AND tenant_id=$2", id, r.TenantID))
}

// insertWorkflowRun is the shared creation seam after admission. The caller owns the transaction
// and the admitted snapshot. Every entry freezes the verified creator, authorized Project and that
// Project's plugin version here. Callers freeze executor slots before committing; only the direct
// development simulator may instead fill a completed trace without bindings.
func insertWorkflowRun(t *transaction, tid, wid string, snapshot, input Object, name, uid, projectID string) Object {
	require(snapshot.S("tenantId") == tid && snapshot.S("workflowId") == wid, 404, "not_found")
	requireWorkflowRunInput(snapshot.O("graph"), input)
	projectID = resolveWorkflowRunProject(t, tid, uid, projectID)
	pluginVersion, pluginRelease := resolveWorkflowRunPlugin(t, tid, projectID)
	var frozenRelease any
	if pluginRelease != nil {
		frozenRelease = jsonText(pluginRelease)
	}
	id := newID()
	t.exec("INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,created_by,project_id,plugin_version,plugin_release) VALUES($1,$2,$3,$4,$5,'pending',$6,$7,$8,$9,$10)", id, tid, wid, snapshot.S("id"), name, jsonText(input), uid, projectID, pluginVersion, frozenRelease)
	return t.one("SELECT * FROM workflow_runs WHERE id=$1 AND tenant_id=$2", id, tid)
}

// resolveWorkflowRunProject authorizes and returns the Project a run freezes (D3).
// The Project is the one thing that says where the run's Workspace may provision —
// createWorkflowRunWorkspace refuses a run without one — and it is stated by the caller rather than
// inferred: a direct run names it explicitly, while an Issue invocation uses its Issue's project_ref.
// A workflow document belongs to a tenant, not to a Project, so it supplies no default of its own.
//
// The value is authorized by the same helper every project-scoped route uses, so an unknown, foreign,
// archived or foreign-space project is the same 404 and a run can never be created against a Project
// its creator could not open. The frozen value is the row's own id, never the string the caller sent.
//
// A Project already being deleted is refused outright (the `resource_unavailable` rule the rest of the
// Project API applies). Freezing one would be worse than refusing it: the run would commit, and its
// Workspace — which can never be created, because createWorkflowRunWorkspace reads a deleting Project
// as busy and a deleted one as gone — would leave the run `running` for good, one failed advance per
// tick and no terminal state. A Project still provisioning is accepted: that state is transient, and
// the scheduler waits for it the same way it waits for any other admission.
//
// Naming one is mandatory, not optional. A run with no frozen Project is not a run that has not
// needed one yet — it is an identity-incomplete aggregate that can never provision the Workspace its
// agent nodes require, so accepting it would persist a row that is born unable to finish and would
// only ever report the gap later, from inside an execution that cannot proceed. This is the same
// reason created_by is required: project_id is the "where" half of the frozen identity,
// and freezing half an identity is not freezing one. A graph that genuinely needs no Workspace still
// names the Project it would provision in, which costs its caller one choice and keeps every run's
// identity whole.
//
// The gate lives on this resolver rather than on the one caller so that every entry which creates a
// run — the editor, @ confirm and manual run — makes one shared decision in the creation transaction
// instead of each writing its own
// "no Project" rule. Refusal reuses the code createWorkflowRunWorkspace already raises for the same
// condition, so a missing Project reads the same whether it is caught here or there.
func resolveWorkflowRunProject(t *transaction, tid, uid, pid string) string {
	require(pid != "", 409, "workflow_run_project_required")
	p := project(t, tid, uid, pid)
	require(p.S("lifecycle") != "deleting", 409, "resource_unavailable")
	return p.S("id")
}

// resolveWorkflowRunPlugin reads the plugin identity the run will execute with, so the creation
// transaction freezes it alongside the rest of the run's identity (D3). The plugin lives in the
// Project's space, which is why this takes the Project the same transaction just froze: the Project is
// what names the space to read the version from, and creation no longer admits a run without one.
//
// It is the same read the session start used to make, moved earlier rather than changed. The version
// is whatever the space's desired state says at creation; nothing here consults the observed state, so
// an install still converging freezes the version it is converging to, and a removal freezes the
// version it is removing from — both are what the run's author asked for at the moment they asked.
//
// The release package is frozen only when the space's selection is that same version: it is what the
// run Workspace's plugin step installs, and a release for any other version would execute something
// the run was not created against. A selection that names another version — a selection converging,
// or a catalog entry replaced under the same version — freezes no package, and the run falls back to
// the plugin step's space-wide snapshot plus the start gate's `agent_plugin_unavailable`.
//
// A missing plugin freezes nothing rather than refusing the creation. The run's Workspace provisions
// and its first agent node then fails honestly with `agent_plugin_version_unfrozen`, which is where
// every other permanent gap in a run's identity is reported. Refusing at creation is a defensible
// alternative — the executor-slot gate beside this does refuse — but it would turn a request the API
// accepts today into a 400, so it stays an owner decision rather than a change made here.
func resolveWorkflowRunPlugin(t *transaction, tenantID string, projectID any) (any, Object) {
	row := t.one(`SELECT p.desired_version,p.selected_release FROM projects pr JOIN space_plugins p ON p.space_id=pr.space_id
		AND p.source_namespace=split_part($3,'/',1) AND p.identifier=split_part($3,'/',2)
		WHERE pr.id=$1 AND pr.tenant_id=$2 AND pr.deleted_at IS NULL`, projectID, tenantID, opencodeAgentCLI)
	if row == nil || row.S("desiredVersion") == "" {
		return nil, nil
	}
	frozen := row.S("desiredVersion")
	selected := row.O("selectedRelease")
	if selected.S("version") != frozen {
		return frozen, nil
	}
	return frozen, selected
}
