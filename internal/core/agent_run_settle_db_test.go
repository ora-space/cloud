package core

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Phase 2A settlement DB tests. They drive the B-owned settleRunWorkspace core through the control
// plane's own terminal step for a run Workspace's create_workspace operation — the operation is
// written succeeded, the runtime control is handed back, and the A→B hook fires (control.go's
// `advance`) — because that transaction is where the settlement runs and what makes its delete
// declaration possible at all: a Project whose create_workspace operation is still queued is busy, so
// no delete could be declared from anywhere else.
//
// They live in package core for the white-box reason the seam itself imposes: the hook receives the
// unexported *transaction. The scenes come from the shipped code — AgentRunDispatcher.Dispatch creates
// the Workspace through createRunWorkspace — so nothing here installs a control-plane double.

// settleScene is one dispatched run in `provisioning`/`dispatched` that owns a real, bound run
// Workspace and the create_workspace operation whose terminal step settles it.
type settleScene struct {
	seed dispSeed
	ws   string
	op   string
}

// stageSettleScene claims the seeded run through the B-side dispatcher, which is what gives the run a
// run Workspace, a create_workspace operation and its `provisioning`/`dispatched` phase. `cancelAt`
// additionally records a cancel request, the D6 state a settlement during provisioning must honor.
func stageSettleScene(t *testing.T, store *Store, cancelAt bool) settleScene {
	t.Helper()
	seed := seedDispatchScene(t, store.Pool, false)
	if err := store.agentRunDispatcher().Dispatch(context.Background(), seed.run); err != nil {
		t.Fatalf("dispatch run: %v", err)
	}
	var ws, op string
	if err := store.Pool.QueryRow(`SELECT id FROM workspaces WHERE issue_run_id=$1`, seed.run).Scan(&ws); err != nil {
		t.Fatalf("read run workspace: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT id FROM operations WHERE workspace_id=$1 AND kind='create_workspace'`, ws).Scan(&op); err != nil {
		t.Fatalf("read create operation: %v", err)
	}
	status, phase := runClaim(t, store, seed.tenant, seed.run)
	if !phase.Valid || phase.String != "provisioning" || status != "dispatched" {
		t.Fatalf("dispatch must leave the run provisioning/dispatched, got phase=%v status=%q", phase, status)
	}
	if cancelAt {
		stageSettleState(t, store, seed.run, "provisioning", "dispatched", true)
	}
	return settleScene{seed: seed, ws: ws, op: op}
}

// stageSettleState rewrites the seeded run's phase/status/cancel_requested_at to the exact pre-settle
// authoritative state a test needs.
func stageSettleState(t *testing.T, store *Store, runID, phase, status string, cancelAt bool) {
	t.Helper()
	q := `UPDATE issue_runs SET phase=$2, status=$3, cancel_requested_at=now(), version=version+1, updated_at=now() WHERE id=$1`
	if !cancelAt {
		q = `UPDATE issue_runs SET phase=$2, status=$3, cancel_requested_at=NULL, version=version+1, updated_at=now() WHERE id=$1`
	}
	if _, err := store.Pool.Exec(q, runID, phase, status); err != nil {
		t.Fatalf("stage settle state (phase=%s status=%s cancel=%v): %v", phase, status, cancelAt, err)
	}
}

// settleRun drives the create_workspace operation's terminal step and the settlement it fires, in one
// transaction, exactly as the control plane's `advance` does: the operation reaches `succeeded`, the
// runtime control is handed back to the run, and only then does the settlement run. A nil return means
// the transaction committed.
func settleRun(t *testing.T, store *Store, scene settleScene, status string) error {
	t.Helper()
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		settleCreateWorkspaceOperation(t, tx, scene.op)
		runWorkspaceSettled(tx, scene.seed.run, status)
		return Object{}
	})
	return err
}

// settleCreateWorkspaceOperation reproduces the control plane's own terminal write for a
// create_workspace operation (control.go `advance`, `next == "done"`). Without it the operation is
// still queued and its Project is busy, so the settlement could not declare a delete.
func settleCreateWorkspaceOperation(t *testing.T, tx *transaction, opID string) {
	t.Helper()
	if tx.execRows(`UPDATE operations SET step='done', state='succeeded', error_code=NULL, retry_at=NULL, version=version+1, updated_at=now() WHERE id=$1`, opID) != 1 {
		t.Fatalf("the create_workspace operation %s must be writable to its terminal state", opID)
	}
	finishRuntimeMaintenance(tx, opID)
}

// settleRunRaw invokes the settle core for a run this test did not stage a Workspace for, which is the
// shape a settlement whose delete seam refuses takes.
func settleRunRaw(t *testing.T, store *Store, runID, status string) error {
	t.Helper()
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		runWorkspaceSettled(tx, runID, status)
		return Object{}
	})
	return err
}

// runFields reads the durable run columns a settlement test asserts on.
func runFields(t *testing.T, store *Store, runID string) (phase sql.NullString, status, failureReason string, result, workspaceID, cancelAt sql.NullString) {
	t.Helper()
	err := store.Pool.QueryRow(
		`SELECT phase, status, failure_reason, result, workspace_id, cancel_requested_at FROM issue_runs WHERE id=$1`,
		runID,
	).Scan(&phase, &status, &failureReason, &result, &workspaceID, &cancelAt)
	if err != nil {
		t.Fatalf("read run fields: %v", err)
	}
	return phase, status, failureReason, result, workspaceID, cancelAt
}

// countRunActivities reports how many issue_activities of the given action exist for the run's issue.
func countRunActivities(t *testing.T, store *Store, issueID, action string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM issue_activities WHERE issue_id=$1 AND action=$2`, issueID, action).Scan(&n); err != nil {
		t.Fatalf("count activities %s: %v", action, err)
	}
	return n
}

// deleteOperationWorkspace returns the Workspace a run's delete_workspace declaration names, or "" when
// the run has no such declaration. It is the durable form of "the release declared the delete": the
// declaration is a real control-plane operation a Controller can claim, not a recorded intention.
func deleteOperationWorkspace(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var wid sql.NullString
	err := store.Pool.QueryRow(`
		SELECT o.workspace_id FROM operations o JOIN workspaces w ON w.id=o.workspace_id
		WHERE w.issue_run_id=$1 AND o.kind='delete_workspace'
		ORDER BY o.created_at DESC, o.id DESC LIMIT 1`, runID).Scan(&wid)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read delete declaration: %v", err)
	}
	return wid.String
}

// TestPhase2ASettleReady (T2-1): an idle provisioning/dispatched run with no cancel and a `ready`
// operation outcome settles provisioning → starting, status stays dispatched, no delete declared.
func TestPhase2ASettleReady(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)

	if err := settleRun(t, store, scene, "ready"); err != nil {
		t.Fatalf("ready settle: %v", err)
	}
	phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
	if !phase.Valid || phase.String != "starting" {
		t.Fatalf("ready settle must reach starting, got phase=%v", phase)
	}
	if status != "dispatched" {
		t.Fatalf("ready settle must keep status=dispatched, got %q", status)
	}
	if reason != "" {
		t.Fatalf("ready settle must not set a failure_reason, got %q", reason)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 0 {
		t.Fatalf("ready settle must not declare delete, got %d", n)
	}
	if bound := runWorkspaceID(t, store, scene.seed.run); bound != scene.ws {
		t.Fatalf("ready settle must keep the run's own Workspace binding, got %q want %q", bound, scene.ws)
	}
}

// TestPhase2ASettleWorkspaceFailure (T2-2): a `failed` operation outcome with no cancel settles
// provisioning → releasing/failed/workspace_unavailable and declares the delete exactly once.
func TestPhase2ASettleWorkspaceFailure(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)

	if err := settleRun(t, store, scene, "failed"); err != nil {
		t.Fatalf("failure settle: %v", err)
	}
	phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
	if !phase.Valid || phase.String != "releasing" {
		t.Fatalf("failure settle must reach releasing, got phase=%v", phase)
	}
	if status != "failed" {
		t.Fatalf("failure settle must set status=failed, got %q", status)
	}
	if reason != "workspace_unavailable" {
		t.Fatalf("failure settle must set failure_reason=workspace_unavailable, got %q", reason)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 1 {
		t.Fatalf("failure settle must declare delete once, got %d", n)
	}
	if wid := deleteOperationWorkspace(t, store, scene.seed.run); wid != scene.ws {
		t.Fatalf("the delete must be declared for the run's own Workspace %s, got %q", scene.ws, wid)
	}
	if n := countRunActivities(t, store, scene.seed.issue, "run.failed"); n != 1 {
		t.Fatalf("failure settle must append one run.failed activity, got %d", n)
	}
}

// pinSettlePlugin writes the run-create snapshot the settle reads: the pinned Agent plugin identity
// and version live in issue_runs.input, never in the current roster (IssueRun D1/D6).
func pinSettlePlugin(t *testing.T, store *Store, runID, pluginID, version string) {
	t.Helper()
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET input=$2, version=version+1, updated_at=now() WHERE id=$1`,
		runID, jsonText(Object{"agentPluginId": pluginID, "agentPluginVersion": version})); err != nil {
		t.Fatalf("pin agent plugin: %v", err)
	}
}

// seedSettlePluginInstance writes the workspace_plugin_instances row the create_workspace plugin
// step leaves behind, with the observed state/version under test.
func seedSettlePluginInstance(t *testing.T, store *Store, wsID, pluginID, state, version string) {
	t.Helper()
	namespace, identifier, ok := pluginIdentity(pluginID)
	if !ok {
		t.Fatalf("seed plugin instance: invalid plugin id %q", pluginID)
	}
	if _, err := store.Pool.Exec(`INSERT INTO workspace_plugin_instances(workspace_id,tenant_id,owner_user_id,project_id,source_namespace,identifier,observed_state,observed_version)
		SELECT id,tenant_id,owner_user_id,project_id,$2,$3,$4,NULLIF($5,'') FROM workspaces WHERE id=$1`,
		wsID, namespace, identifier, state, version); err != nil {
		t.Fatalf("seed plugin instance: %v", err)
	}
}

// TestPhase2ASettlePinnedPluginUnavailable: a run that pins an Agent plugin is only ready when that
// plugin is installed at the pinned version on its Workspace. The create_workspace plugin step opens
// admission even when an item install failed, so the Workspace's readiness is not evidence; an absent,
// failed or wrong-version instance settles releasing/failed/agent_plugin_unavailable in the same
// transaction as the operation terminal and declares the delete, exactly like any other startup
// failure. The run never reaches starting.
func TestPhase2ASettlePinnedPluginUnavailable(t *testing.T) {
	const pluginID = "official/ora-space.opencode"
	for _, tc := range []struct {
		name    string
		seed    bool
		state   string
		version string
	}{
		{name: "absent"},
		{name: "failed", seed: true, state: "failed"},
		{name: "still installing", seed: true, state: "installing", version: "1.0.0"},
		{name: "wrong version", seed: true, state: "installed", version: "2.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := dispatcherDB(t)
			scene := stageSettleScene(t, store, false)
			pinSettlePlugin(t, store, scene.seed.run, pluginID, "1.0.0")
			if tc.seed {
				seedSettlePluginInstance(t, store, scene.ws, pluginID, tc.state, tc.version)
			}

			if err := settleRun(t, store, scene, "ready"); err != nil {
				t.Fatalf("ready settle: %v", err)
			}
			phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
			if !phase.Valid || phase.String != "releasing" || status != "failed" || reason != "agent_plugin_unavailable" {
				t.Fatalf("settle = %v/%q/%q; want releasing/failed/agent_plugin_unavailable", phase, status, reason)
			}
			if n := countDeleteOperations(t, store, scene.seed.run); n != 1 {
				t.Fatalf("plugin-unavailable settle must declare delete once, got %d", n)
			}
			if n := countRunActivities(t, store, scene.seed.issue, "run.failed"); n != 1 {
				t.Fatalf("plugin-unavailable settle must append one run.failed activity, got %d", n)
			}
		})
	}
}

// TestPhase2ASettlePinnedPluginInstalled: the pinned plugin installed at exactly the pinned version
// keeps the ordinary ready settlement. Together with the cases above this is the D3 line: readiness
// of the Workspace plus the pinned instance, not readiness alone.
func TestPhase2ASettlePinnedPluginInstalled(t *testing.T) {
	const pluginID = "official/ora-space.opencode"
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)
	pinSettlePlugin(t, store, scene.seed.run, pluginID, "1.0.0")
	seedSettlePluginInstance(t, store, scene.ws, pluginID, "installed", "1.0.0")

	if err := settleRun(t, store, scene, "ready"); err != nil {
		t.Fatalf("ready settle: %v", err)
	}
	phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
	if !phase.Valid || phase.String != "starting" || status != "dispatched" || reason != "" {
		t.Fatalf("settle = %v/%q/%q; want starting/dispatched with no failure", phase, status, reason)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 0 {
		t.Fatalf("an installed pinned plugin must not declare delete, got %d", n)
	}
}

// TestPhase2ASettleHookRollback (T2-4): when the delete seam refuses, the whole caller transaction
// rolls back — the run stays provisioning/dispatched and no failure activity or delete intent commits.
//
// The seam's only refusal today is a run with no Workspace row to delete (a busy Project is an
// accepted outcome, not an error — see declareDelete), so the scene is the invariant contradiction it
// describes: a provisioning run that never got a run Workspace. That is exactly the case that must
// abort rather than be tolerated.
func TestPhase2ASettleHookRollback(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	stageSettleState(t, store, seed.run, "provisioning", "dispatched", false)

	if err := settleRunRaw(t, store, seed.run, "failed"); err == nil {
		t.Fatalf("a settlement whose delete seam refuses must surface as an error (rollback)")
	}
	phase, status, reason, _, _, _ := runFields(t, store, seed.run)
	if !phase.Valid || phase.String != "provisioning" {
		t.Fatalf("rollback must keep the run provisioning, got phase=%v", phase)
	}
	if status != "dispatched" {
		t.Fatalf("rollback must keep status=dispatched, got %q", status)
	}
	if reason != "" {
		t.Fatalf("rollback must not persist a failure_reason, got %q", reason)
	}
	if n := countRunActivities(t, store, seed.issue, "run.failed"); n != 0 {
		t.Fatalf("rollback must not persist a run.failed activity, got %d", n)
	}
	if n := countDeleteOperations(t, store, seed.run); n != 0 {
		t.Fatalf("rollback must not leave a delete intent, got %d", n)
	}
}

// TestPhase2ASettleReplayReady (T2-5): settling ready twice claims once — the second call finds the
// run already starting and is a no-op, with no delete and no side effect.
func TestPhase2ASettleReplayReady(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)

	if err := settleRun(t, store, scene, "ready"); err != nil {
		t.Fatalf("first ready settle: %v", err)
	}
	phase, status, version := runPhaseStatus(t, store, scene.seed.run)
	if err := settleRun(t, store, scene, "ready"); err != nil {
		t.Fatalf("replay ready settle must be a clean no-op: %v", err)
	}
	gotPhase, gotStatus, gotVersion := runPhaseStatus(t, store, scene.seed.run)
	if gotPhase != phase || gotStatus != status || gotVersion != version {
		t.Fatalf("a replayed ready settle must write nothing: %s/%s/%d became %s/%s/%d", phase, status, version, gotPhase, gotStatus, gotVersion)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 0 {
		t.Fatalf("replay ready must not declare delete, got %d", n)
	}
}

// TestPhase2ASettleReplayFailure (T2-6): settling failure twice declares delete once — the second call
// finds the run already releasing and is a no-op.
func TestPhase2ASettleReplayFailure(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)

	if err := settleRun(t, store, scene, "failed"); err != nil {
		t.Fatalf("first failure settle: %v", err)
	}
	version := runVersion(t, store, scene.seed.run)
	if err := settleRun(t, store, scene, "failed"); err != nil {
		t.Fatalf("replay failure settle must be a clean no-op: %v", err)
	}
	phase, status, reason, _, _, _ := runFields(t, store, scene.seed.run)
	if !phase.Valid || phase.String != "releasing" || status != "failed" || reason != "workspace_unavailable" {
		t.Fatalf("replay failure must keep releasing/failed/workspace_unavailable, got phase=%v status=%q reason=%q", phase, status, reason)
	}
	if got := runVersion(t, store, scene.seed.run); got != version {
		t.Fatalf("a replayed failure settle must write nothing: version %d became %d", version, got)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 1 {
		t.Fatalf("replay failure must declare delete exactly once, got %d", n)
	}
}

// TestPhase2ASettleStaleCallback (T2-7): a settlement arriving when the run is already starting or
// releasing is a deterministic no-op — the state is unchanged and no delete intent is created.
func TestPhase2ASettleStaleCallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		phase, stat string
		ready       bool
	}{
		{"starting", "starting", "dispatched", true},
		{"releasing", "releasing", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := dispatcherDB(t)
			scene := stageSettleScene(t, store, false)
			stageSettleState(t, store, scene.seed.run, tc.phase, tc.stat, false)
			version := runVersion(t, store, scene.seed.run)
			status := "ready"
			if !tc.ready {
				status = "failed"
			}

			if err := settleRun(t, store, scene, status); err != nil {
				t.Fatalf("stale settle must be a clean no-op: %v", err)
			}
			gotPhase, gotStatus, gotVersion := runPhaseStatus(t, store, scene.seed.run)
			if gotPhase != tc.phase || gotStatus != tc.stat || gotVersion != version {
				t.Fatalf("stale settle must not move the run, got %s/%s/%d (want %s/%s/%d)", gotPhase, gotStatus, gotVersion, tc.phase, tc.stat, version)
			}
			if n := countDeleteOperations(t, store, scene.seed.run); n != 0 {
				t.Fatalf("stale settle must not declare delete, got %d", n)
			}
		})
	}
}

// TestPhase2ASettleCancelBeforeSettle (T2-8): a provisioning run with cancel_requested_at set settles
// to releasing/cancelled with result.deliveryState=skipped and declares the delete even when the
// operation outcome is `ready` — cancel wins over ready (IssueRun D6).
func TestPhase2ASettleCancelBeforeSettle(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, true)

	if err := settleRun(t, store, scene, "ready"); err != nil {
		t.Fatalf("cancel settle with a ready outcome must not error: %v", err)
	}
	phase, status, reason, result, _, _ := runFields(t, store, scene.seed.run)
	if !phase.Valid || phase.String != "releasing" {
		t.Fatalf("cancel-before-settle must reach releasing (not starting), got phase=%v", phase)
	}
	if status != "cancelled" {
		t.Fatalf("cancel-before-settle must set status=cancelled, got %q", status)
	}
	if reason != "" {
		t.Fatalf("cancel-before-settle must not set a failure_reason, got %q", reason)
	}
	if !result.Valid || !strings.Contains(result.String, "deliveryState") || !strings.Contains(result.String, "skipped") {
		t.Fatalf("cancel-before-settle must set result.deliveryState=skipped, got %v", result)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 1 {
		t.Fatalf("cancel-before-settle must declare delete once, got %d", n)
	}
	if n := countRunActivities(t, store, scene.seed.issue, "run.cancelled"); n != 1 {
		t.Fatalf("cancel-before-settle must append one run.cancelled activity, got %d", n)
	}
}

// TestPhase2ASettleWorkspaceBinding (T2-9): the settlement never regenerates or overwrites the
// authoritative workspace binding, and the delete it declares names that same Workspace. The binding is
// re-read from issue_runs inside the transaction rather than taken from the caller, and the control
// plane's own delete declaration resolves the Workspace by the run's `issue_run_id` — so there is no
// caller-supplied value that could redirect either.
func TestPhase2ASettleWorkspaceBinding(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)
	// A second Workspace in the same Project must never be the one the release deletes. An isolated
	// Workspace owns exactly one task identity (schema invariant), so it is seeded the way
	// insertWorkspace creates one.
	stray := newID()
	tx, err := store.Pool.Begin()
	if err != nil {
		t.Fatalf("seed stray workspace: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
		SELECT $1,tenant_id,owner_user_id,creator_user_id,creator_evidence,$2,'isolated','running','ready','HEAD' FROM workspaces WHERE id=$3`,
		stray, scene.seed.project, scene.ws); err != nil {
		t.Fatalf("seed stray workspace: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'stray task')`, newID(), stray); err != nil {
		t.Fatalf("seed stray task: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed stray workspace commit: %v", err)
	}

	if err := settleRun(t, store, scene, "failed"); err != nil {
		t.Fatalf("failure settle: %v", err)
	}
	_, _, _, _, bound, _ := runFields(t, store, scene.seed.run)
	if !bound.Valid || bound.String != scene.ws {
		t.Fatalf("settlement must not overwrite the authoritative workspace binding, got %v (want %s)", bound, scene.ws)
	}
	if wid := deleteOperationWorkspace(t, store, scene.seed.run); wid != scene.ws {
		t.Fatalf("the delete declaration must carry the authoritative workspace, got %q want %q", wid, scene.ws)
	}
	if wid := deleteOperationWorkspace(t, store, scene.seed.run); wid == stray {
		t.Fatalf("the delete declaration must not name an unrelated Workspace of the same Project")
	}
}

// TestPhase2ASettleAtomicity (T2-10): within a single transaction, an A-side terminal mutation and the
// B transition commit or roll back together. The settlement's delete seam refuses (a provisioning run
// with no run Workspace to delete), so both the A write and the B transition must be undone.
func TestPhase2ASettleAtomicity(t *testing.T) {
	store := dispatcherDB(t)
	seed := seedDispatchScene(t, store.Pool, false)
	stageSettleState(t, store, seed.run, "provisioning", "dispatched", false)

	// One transaction: a simulated A terminal write (dispatched_at is A-owned and NULL on a seeded
	// provisioning run) followed by the B settlement whose delete seam refuses.
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`UPDATE issue_runs SET dispatched_at=now(), version=version+1, updated_at=now() WHERE id=$1`, seed.run)
		runWorkspaceSettled(tx, seed.run, "failed")
		return Object{}
	})
	if err == nil {
		t.Fatalf("a refused delete seam must abort the shared A/B transaction")
	}
	var dispatchedAt sql.NullString
	phase, status, reason, _, _, _ := runFields(t, store, seed.run)
	if err := store.Pool.QueryRow(`SELECT dispatched_at FROM issue_runs WHERE id=$1`, seed.run).Scan(&dispatchedAt); err != nil {
		t.Fatalf("read dispatched_at: %v", err)
	}
	if !phase.Valid || phase.String != "provisioning" || status != "dispatched" || reason != "" {
		t.Fatalf("A/B rollback must leave run provisioning/dispatched with no reason, got phase=%v status=%q reason=%q", phase, status, reason)
	}
	if dispatchedAt.Valid {
		t.Fatalf("A/B rollback must undo the simulated A terminal write, dispatched_at still set: %v", dispatchedAt.String)
	}
	if n := countRunActivities(t, store, seed.issue, "run.failed"); n != 0 {
		t.Fatalf("A/B rollback must not persist a run.failed activity, got %d", n)
	}
}

// TestPhase2ASettleInvariantErrors: genuine invariant violations abort the transaction — a settlement
// for a nonexistent run, for a non-agent run, and for an operation outcome word this matrix has no row
// for each surface as errors that roll back the caller-owned transaction.
func TestPhase2ASettleInvariantErrors(t *testing.T) {
	store := dispatcherDB(t)
	scene := stageSettleScene(t, store, false)

	// Nonexistent run id (still a well-formed uuid) → invariant error.
	if err := settleRunRaw(t, store, newID(), "ready"); err == nil {
		t.Fatalf("settle for a nonexistent run must return an invariant error")
	}
	// A team run settling as if it were an Agent run → invariant error.
	teamRun := newID()
	if _, err := store.Pool.Exec(
		`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status) VALUES($1,$2,$3,'team',$4,'queued')`,
		teamRun, scene.seed.tenant, scene.seed.issue, newID(),
	); err != nil {
		t.Fatalf("seed team run: %v", err)
	}
	if err := settleRunRaw(t, store, teamRun, "ready"); err == nil {
		t.Fatalf("settle for a non-agent run must return an invariant error")
	}
	// An operation outcome that is neither `ready` nor `failed` is an internal contradiction, not a
	// third row of D3's matrix.
	if err := settleRun(t, store, scene, "bogus"); err == nil {
		t.Fatalf("settle with an unknown operation outcome must return an invariant error")
	}
	// The rejected settlements must not touch the valid run.
	phase, status, _, _, _, _ := runFields(t, store, scene.seed.run)
	if !phase.Valid || phase.String != "provisioning" || status != "dispatched" {
		t.Fatalf("invariant-error settlements must not touch the valid run, got phase=%v status=%q", phase, status)
	}
}

// TestSettleHookIsBoundAndFailsClosed covers the two halves of the seam contract that replaced the
// pre-merge AgentRunHooks double: the Cloud lifecycle really is bound onto the control plane's
// callbacks (so a deployment that calls BindBusinessHooks gets the transitions above), and a hook
// reached outside the control-plane transaction that published itself aborts instead of running on a
// second, uncommitted view of the handle.
func TestSettleHookIsBoundAndFailsClosed(t *testing.T) {
	store := dispatcherDB(t)
	if store.OnRunWorkspaceSettled == nil || store.OnRunWorkspaceDeleted == nil || store.OnThreadEvents == nil ||
		store.OnSessionEnded == nil || store.OnDeliverySettled == nil {
		t.Fatalf("BindBusinessHooks must bind all five A→B callbacks")
	}

	// A hook invoked on a handle the enclosing transaction did not publish is an invariant violation:
	// its writes would land in a different commit than the evidence it reacts to.
	foreign, err := store.Pool.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("open foreign transaction: %v", err)
	}
	defer func() { _ = foreign.Rollback() }()
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("a hook reached on a foreign transaction handle must abort")
			}
		}()
		_ = store.OnRunWorkspaceSettled(context.Background(), foreign, newID(), "ready")
	}()
}
