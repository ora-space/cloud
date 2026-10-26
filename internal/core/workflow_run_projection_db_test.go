package core

import (
	"context"
	"testing"
)

// DB tests for the WorkflowRun detail projection of the frozen plugin version (circus#369).
//
// workflow_runs.plugin_version is written once by the creation transaction (D3, migration 0042) and
// is the identity the run's agent sessions execute with. Readers must see that frozen value — not
// the space's current desired version — and must see null, not an empty string, when the run was
// created without one: an empty version reads like a real version to a client and hides the
// `agent_plugin_version_unfrozen` gap the first agent node reports.

// workflowRunDetailRead drives the public detail read the way the router does.
func workflowRunDetailRead(t *testing.T, store *Store, tid, wid, runID string, caller *Claims) Object {
	t.Helper()
	out, status, err := store.Public(context.Background(), &PublicRequest{
		Method:   "GET",
		Path:     "/api/v1/tenants/" + tid + "/workflows/" + wid + "/runs/" + runID,
		TenantID: tid, WorkflowID: wid, RunID: runID, Identity: caller,
	})
	if err != nil || status != 200 {
		t.Fatalf("GET workflow run detail: status=%d err=%v", status, err)
	}
	return out
}

// The seeded scene freezes 1.0.0 at creation. Both a pending run and a settled one project that
// same value: settling never rewrites the frozen identity, and the page reads the run, not the
// current space plugin.
func TestWorkflowRunDetailProjectsTheFrozenPluginVersion(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	caller := cancelCaller(t, store, tid)

	pending := workflowRunDetailRead(t, store, tid, wid, runID, caller)
	if got := pending.S("status"); got != "pending" {
		t.Fatalf("seeded run status = %q; want pending", got)
	}
	if got := pending.S("pluginVersion"); got != "1.0.0" {
		t.Fatalf("pending run pluginVersion = %q; want the frozen 1.0.0", got)
	}

	// A terminal run keeps the identity it started with; the projection reads the stored column.
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET status='succeeded', started_at=now(), finished_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("settle the run: %v", err)
	}
	succeeded := workflowRunDetailRead(t, store, tid, wid, runID, caller)
	if got := succeeded.S("status"); got != "succeeded" {
		t.Fatalf("settled run status = %q; want succeeded", got)
	}
	if got := succeeded.S("pluginVersion"); got != "1.0.0" {
		t.Fatalf("succeeded run pluginVersion = %q; want the frozen 1.0.0", got)
	}
}

// 0042 added the column without a backfill and creation still accepts a space with no plugin, so
// NULL is a real state. The detail read reports it as JSON null, and the key is present either way
// so a client can tell "unfrozen" from "field missing".
func TestWorkflowRunDetailProjectsAnUnfrozenPluginVersionAsNull(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET plugin_version=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("clear the frozen version: %v", err)
	}

	detail := workflowRunDetailRead(t, store, tid, wid, runID, cancelCaller(t, store, tid))
	got, ok := detail["pluginVersion"]
	if !ok {
		t.Fatal("detail read omitted pluginVersion; an unfrozen run must report null")
	}
	if got != nil {
		t.Fatalf("unfrozen pluginVersion = %#v; want JSON null", got)
	}
}
