package core

import "strings"

// workflow loads a live workflow scoped to the tenant.
func workflow(t *transaction, tid, wid string) Object {
	require(validID(wid), 404, "not_found")
	w := t.one("SELECT * FROM workflows WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", wid, tid)
	require(w != nil, 404, "not_found")
	return w
}

func workflowList(t *transaction, r *PublicRequest) Object {
	return page(t, "SELECT * FROM workflows WHERE tenant_id=$1 AND deleted_at IS NULL", []any{r.TenantID}, "id", r)
}

// graphText renders the graph body field. A workflow created without one stores the empty document
// rather than SQL NULL: the column is a NOT NULL object, and the editor opens an empty document as a
// blank canvas. The body field is already proven to be a JSON object by the router's field check.
func graphText(r *PublicRequest) string {
	if g, ok := r.Body["graph"]; ok {
		return jsonText(g)
	}
	return "{}"
}

func createWorkflow(t *transaction, r *PublicRequest) Object {
	name := validText(r.Body.S("name"), 200)
	require(t.one("SELECT id FROM workflows WHERE tenant_id=$1 AND name=$2 AND deleted_at IS NULL", r.TenantID, name) == nil, 409, "workflow_conflict")
	return insertWorkflow(t, r.TenantID, name, r.Body.S("description"), graphText(r))
}

// insertWorkflow is the one workflow creation write. Callers have already applied the tenant-level
// name rules; the asset importer uses it so an imported copy is created exactly like an editor draft.
func insertWorkflow(t *transaction, tenantID, name, description, graph string) Object {
	id := newID()
	t.exec("INSERT INTO workflows(id,tenant_id,name,description,graph) VALUES($1,$2,$3,$4,$5)", id, tenantID, name, description, graph)
	return workflow(t, tenantID, id)
}

func updateWorkflow(t *transaction, r *PublicRequest) Object {
	w := workflow(t, r.TenantID, r.WorkflowID)
	version(w, r.Body.N("version"))
	cols := []string{}
	vals := []any{}
	add := func(col string, val any) {
		cols = append(cols, col+"=$"+itoa(len(vals)+1))
		vals = append(vals, val)
	}
	if _, ok := r.Body["name"]; ok {
		name := validText(r.Body.S("name"), 200)
		require(t.one("SELECT id FROM workflows WHERE tenant_id=$1 AND name=$2 AND deleted_at IS NULL AND id<>$3", r.TenantID, name, w.S("id")) == nil, 409, "workflow_conflict")
		add("name", name)
	}
	if _, ok := r.Body["description"]; ok {
		add("description", r.Body.S("description"))
	}
	if _, ok := r.Body["graph"]; ok {
		add("graph", graphText(r))
	}
	require(len(cols) > 0, 400, "invalid_input")
	vals = append(vals, w.S("id"), r.TenantID)
	idPh, tidPh := itoa(len(vals)-1), itoa(len(vals))
	t.exec("UPDATE workflows SET "+strings.Join(cols, ",")+",version=version+1,updated_at=now() WHERE id=$"+idPh+" AND tenant_id=$"+tidPh, vals...)
	return workflow(t, r.TenantID, w.S("id"))
}

func deleteWorkflow(t *transaction, r *PublicRequest) Object {
	w := workflow(t, r.TenantID, r.WorkflowID)
	version(w, r.Body.N("version"))
	t.exec("UPDATE workflows SET deleted_at=now(),version=version+1,updated_at=now() WHERE id=$1", w.S("id"))
	return t.one("SELECT * FROM workflows WHERE id=$1 AND tenant_id=$2", w.S("id"), r.TenantID)
}

// workflowSnapshot loads one snapshot row scoped to a live workflow: archiving the
// workflow hides its whole history, and a cursor to any other workflow is 404.
func workflowSnapshot(t *transaction, tid, wid, sid string) Object {
	require(validID(sid), 404, "not_found")
	workflow(t, tid, wid)
	s := t.one("SELECT * FROM workflow_snapshots WHERE id=$1 AND tenant_id=$2 AND workflow_id=$3", sid, tid, wid)
	require(s != nil, 404, "not_found")
	return s
}

func workflowSnapshotList(t *transaction, r *PublicRequest) Object {
	workflow(t, r.TenantID, r.WorkflowID)
	return page(t, "SELECT * FROM workflow_snapshots WHERE tenant_id=$1 AND workflow_id=$2", []any{r.TenantID, r.WorkflowID}, "id", r)
}

// publishWorkflow freezes the live graph into the next snapshot version. The workflow
// row is locked for the transaction, so two publishes cannot both pick the same next
// version. The snapshot name defaults to the workflow name; the body may override it.
//
// This is the one place a graph is authoritatively judged. A snapshot is what runs execute, so
// publishing is the last moment Cloud can refuse a document it could not run: a graph that reaches
// here and is frozen is a graph Cloud has agreed to execute, and the problems come back with the node
// or slot at fault so the editor can mark it. Saving is deliberately not gated this way — an
// unfinished draft is a legitimate document, and a check moved into the save path would stop the
// author mid-edit. Nothing about the *runtime* is judged here either: whether an executor is
// installed or configured is the runner's answer, not the author's, and asking it at publish would
// refuse a graph for a reason the publisher cannot fix.
func publishWorkflow(t *transaction, r *PublicRequest) Object {
	w := t.one("SELECT * FROM workflows WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL FOR UPDATE", r.WorkflowID, r.TenantID)
	require(w != nil, 404, "not_found")
	graph := w.O("graph")
	requireValidWorkflowGraph(graph)
	w["graph"] = normalizeWorkflowGlobals(graph)
	name := w.S("name")
	if r.Body.S("name") != "" {
		name = validText(r.Body.S("name"), 200)
	}
	return insertWorkflowSnapshot(t, w, name)
}

// insertWorkflowSnapshot freezes the workflow's live graph under the next per-workflow version.
// The caller holds the workflow row lock, so two publishes cannot pick the same version.
func insertWorkflowSnapshot(t *transaction, w Object, name string) Object {
	return insertSnapshotGraph(t, w, name, jsonText(w.O("graph")))
}

// insertSnapshotGraph is the one snapshot write. The graph text is passed separately so a caller can
// freeze a graph it did not build from the live document: the importer passes the package document
// verbatim, re-encoding it only when normalizeWorkflowGlobals rewrote a legacy file reference.
func insertSnapshotGraph(t *transaction, w Object, name, graph string) Object {
	next := t.one("SELECT COALESCE(MAX(version),0)+1 AS version FROM workflow_snapshots WHERE tenant_id=$1 AND workflow_id=$2", w.S("tenantId"), w.S("id")).N("version")
	id := newID()
	t.exec("INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,$4,$5,$6)", id, w.S("tenantId"), w.S("id"), next, name, graph)
	return t.one("SELECT * FROM workflow_snapshots WHERE id=$1 AND tenant_id=$2", id, w.S("tenantId"))
}

// restoreWorkflowSnapshot replaces the live graph with a chosen snapshot's, advancing
// the workflow document version like any editing write would.
func restoreWorkflowSnapshot(t *transaction, r *PublicRequest) Object {
	w := workflow(t, r.TenantID, r.WorkflowID)
	version(w, r.Body.N("version"))
	s := workflowSnapshot(t, r.TenantID, r.WorkflowID, r.SnapshotID)
	t.exec("UPDATE workflows SET graph=$1,version=version+1,updated_at=now() WHERE id=$2 AND tenant_id=$3", jsonText(s.O("graph")), w.S("id"), r.TenantID)
	return workflow(t, r.TenantID, w.S("id"))
}
