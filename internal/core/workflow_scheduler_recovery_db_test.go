package core

// Crash-takeover DB tests for the workflow scheduler (circus#75 S4; specs
// decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution D1 条件②/④ and D4).
//
// The scheduler keeps no in-memory graph state: every pass recomputes the ready set from the run
// row's committed node_states. These tests start a run at an arbitrary committed midpoint and let a
// *second* Store instance — the same configuration over the same database, the way a restarted
// process sees it — do the advancing. The obligations they pin are the ones a crash can break:
// committed results are never recomputed or re-dispatched, an in-flight session is settled only by
// its own execution's result, an absent result is never read as success, and the result that arrives
// after the restart still drives the run to its terminal state.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// restartStore returns a second Store over the same database and configuration. It stands in for a
// new Cloud process: nothing carries over except what the previous process committed.
func restartStore(store *Store) *Store {
	restarted := *store
	return &restarted
}

// commitWorkflowRunMidpoint rewrites a seeded pending run into the state a crash left behind:
// `running`, with the given committed node states, under the version CAS every later commit reads.
func commitWorkflowRunMidpoint(t *testing.T, store *Store, runID string, states Object) {
	t.Helper()
	raw, err := json.Marshal(states)
	if err != nil {
		t.Fatalf("marshal node states: %v", err)
	}
	if _, err := store.Pool.Exec(`UPDATE workflow_runs
		SET status='running', started_at=now(), node_states=$2::jsonb, version=version+1, updated_at=now()
		WHERE id=$1 AND status='pending'`, runID, string(raw)); err != nil {
		t.Fatalf("commit midpoint: %v", err)
	}
}

// registerWorkflowSessionExecution inserts one agent-session execution for a run's graph node, with
// or without its terminal result. It is the row a Controller registered before the crash; the
// scheduler reads terminal evidence from it by (operation, graph node).
func registerWorkflowSessionExecution(t *testing.T, store *Store, runID, nodeID string, result Object) string {
	t.Helper()
	executionID := "exec-" + nodeID
	var raw any
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal execution result: %v", err)
		}
		raw = string(b)
	}
	if _, err := store.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,node_id,node_operation_id,input,result,dispatched_epoch,workflow_node_id)
		VALUES($1,'agent_session',$2,'node-1','node-op-1','{}'::jsonb,$3::jsonb,1,$4)`,
		executionID, runID, raw, nodeID); err != nil {
		t.Fatalf("register session execution: %v", err)
	}
	return executionID
}

// A mid-run crash leaves some nodes committed and the current wave in flight. A restarted process
// must recompute the ready set from those committed states alone: the start node's recorded output —
// which differs from what re-executing it would produce — is what the condition reads, so the
// branch proves the node was not run again, and the wave downstream of it executes normally.
func TestWorkflowAdvanceResumesFromCommittedStateAfterRestart(t *testing.T) {
	store := dispatcherDB(t)
	start := wfNode("s", "start", "")
	start["data"].(Object)["input"] = "a fresh execution would say this"
	rule := func(id, value string) Object {
		return Object{"id": id, "logic": "and", "conditions": []any{
			Object{"variableSelector": []any{"s", "input"}, "operator": "equals", "value": value},
		}}
	}
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{
			start,
			wfConditionNode("c", []any{rule("committed", "committed value")}),
			wfNode("taken", "output", ""),
			wfNode("other", "output", ""),
		},
		[][3]string{{"s", "c", ""}, {"c", "taken", "committed"}, {"c", "other", workflowBranchElse}},
	))
	// The crash happened after `s` committed. Its output is the committed value, not the authored
	// prompt a re-execution would yield.
	commitWorkflowRunMidpoint(t, store, runID, Object{
		"s": Object{"status": "succeeded", "output": Object{"input": "committed value"}},
	})

	restarted := restartStore(store)
	if err := restarted.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after restart: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, restarted, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	if got := states["s"].(map[string]any)["output"].(map[string]any)["input"]; got != "committed value" {
		t.Fatalf("committed start output became %v; want the committed value — it was re-executed", got)
	}
	if got := wfNodeStatus(t, states, "taken"); got != "succeeded" {
		t.Fatalf("committed branch status = %q; want succeeded — the condition re-read the authored prompt", got)
	}
	if got := wfNodeStatus(t, states, "other"); got != "inactive" {
		t.Fatalf("else branch status = %q; want inactive", got)
	}

	// A second restart over the settled row changes nothing: terminal runs are not rescanned.
	again := restartStore(store)
	if err := again.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("second advance after restart: %v", err)
	}
	if got, _, _, after := readWorkflowRun(t, again, runID); got != "succeeded" || after != version {
		t.Fatalf("second pass moved the run: %q version %d; want succeeded version %d", got, after, version)
	}
}

// A crash while an agent session is in flight leaves the node committed `running` and the session
// execution registered without a result. The restarted process must not invent one: the run stays
// running, the node keeps its running state, nothing downstream executes, and the unregistered work
// item the crashed process left behind is still claimable. When the Node finally reports, the *next*
// pass settles the node from that result and finishes the run — progress committed before the crash
// is preserved and the eventual evidence is not lost.
func TestWorkflowAdvanceResumesAnInFlightSessionFromItsOwnResult(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("a", "agent", "worker"), wfNode("o", "output", "")},
		[][3]string{{"s", "a", ""}, {"a", "o", ""}},
	))
	commitWorkflowRunMidpoint(t, store, runID, Object{
		"s": Object{"status": "succeeded", "output": Object{"input": ""}},
		"a": Object{"status": "running", "startedAt": nowRFC3339()},
	})
	executionID := registerWorkflowSessionExecution(t, store, runID, "a", nil)
	workID := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO execution_work(id,workflow_run_id,kind,input,target,workflow_node_id)
		VALUES($1,$2,'agent_session','{}'::jsonb,'{}'::jsonb,'a')`, workID, runID); err != nil {
		t.Fatalf("seed unregistered work: %v", err)
	}

	restarted := restartStore(store)
	if err := restarted.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after restart: %v", err)
	}
	status, _, states, version := readWorkflowRun(t, restarted, runID)
	if status != "running" {
		t.Fatalf("run status after a restart with no result = %q; want running", status)
	}
	if got := wfNodeStatus(t, states, "a"); got != "running" {
		t.Fatalf("in-flight node status = %q; want running — an absent result was read as terminal", got)
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("node downstream of the in-flight session committed: %v", states)
	}
	var executions int
	if err := restarted.Pool.QueryRow(`SELECT count(*) FROM node_executions WHERE operation_id=$1 AND workflow_node_id='a'`, runID).Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("agent node has %d executions; want the one registered before the crash, no re-dispatch", executions)
	}
	// This execution_work row is what the crashed process left unregistered — the node_executions row
	// above belongs to the already-running session, not to this item. Recovery responsibility means
	// the restarted process leaves the item claimable, not that it drops it.
	if _, err := restarted.transact(context.Background(), func(tx *transaction) Object {
		claimed := claimWorkItem(tx)
		work, ok := asWorkflowObject(claimed["work"])
		if !ok {
			t.Fatalf("claim after restart returned no work: %v", claimed)
		}
		if work.S("id") != workID || work.S("workflowNodeId") != "a" {
			t.Fatalf("claim returned work %s node %q; want %s node a", work.S("id"), work.S("workflowNodeId"), workID)
		}
		return Object{}
	}); err != nil {
		t.Fatalf("claim after restart: %v", err)
	}

	// The Node reconnects and reports its session's terminal result. The next pass reads that exact
	// execution — not "the newest session of the run" — and continues the wave.
	if _, err := restarted.Pool.Exec(`UPDATE node_executions SET result='{"reason":"idle_timeout"}'::jsonb WHERE execution_id=$1`, executionID); err != nil {
		t.Fatalf("record session result: %v", err)
	}
	if err := restarted.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the result arrived: %v", err)
	}
	status, runErr, states, after := readWorkflowRun(t, restarted, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	if after <= version {
		t.Fatalf("run version did not advance: %d -> %d", version, after)
	}
	if got := wfNodeStatus(t, states, "a"); got != "succeeded" {
		t.Fatalf("settled agent node status = %q; want succeeded from its own result", got)
	}
	if got := wfNodeStatus(t, states, "o"); got != "succeeded" {
		t.Fatalf("downstream node status = %q; want succeeded after the session settled", got)
	}
}

// The production crash midpoint has a settled Workspace and a live Node, not the minimal rows the
// test above seeds. startWorkflowAgentSession creates the Workspace before it commits the node
// running, and the session's Node keeps heartbeating; a restart therefore holds the in-flight node
// because the Node can still report and the turn window has not expired. The no-Workspace shape
// above exercises the shared judgement's fail-closed side, but only this shape proves the hold a
// real deployment sees after a restart. The result that arrives after the restart settles the node
// from its own execution and finishes the run.
func TestWorkflowAdvanceHoldsAProductionSessionAcrossRestart(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to provision the workspace: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)

	restarted := restartStore(store)
	if err := restarted.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after restart: %v", err)
	}
	status, _, states, _ := readWorkflowRun(t, restarted, runID)
	if status != "running" {
		t.Fatalf("run status after a restart with a live Node and no result = %q; want running", status)
	}
	if got := wfNodeStatus(t, states, "a"); got != "running" {
		t.Fatalf("in-flight node status = %q; want running within the turn window", got)
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("node downstream of the in-flight session committed: %v", states)
	}
	var executions int
	if err := restarted.Pool.QueryRow(`SELECT count(*) FROM node_executions WHERE operation_id=$1 AND workflow_node_id='a'`, runID).Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("agent node has %d executions; want the one dispatched before the crash, no re-dispatch", executions)
	}
	var heldWorkspace string
	if err := restarted.Pool.QueryRow(`SELECT id FROM workspaces WHERE workflow_run_id=$1`, runID).Scan(&heldWorkspace); err != nil {
		t.Fatalf("read the run workspace: %v", err)
	}
	if heldWorkspace != wsID {
		t.Fatalf("run workspace = %s; want %s still bound", heldWorkspace, wsID)
	}

	reportSessionEnd(t, restarted, runID, execution, nodeService, "idle_timeout")
	status, runErr, states, _ := readWorkflowRun(t, restarted, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded from the session's own result", status, runErr)
	}
	if got := wfNodeStatus(t, states, "a"); got != "succeeded" {
		t.Fatalf("settled agent node status = %q; want succeeded", got)
	}
	if got := wfNodeStatus(t, states, "o"); got != "succeeded" {
		t.Fatalf("downstream node status = %q; want succeeded after the session settled", got)
	}
}
