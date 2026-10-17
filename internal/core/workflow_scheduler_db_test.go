package core

// DB tests for the Cloud-side workflow scheduler's advance pass (workflow_scheduler.go): a pending
// run is claimed and settled from the frozen snapshot's graph, an agent node a run cannot execute
// fails it honestly, and the pass is idempotent against already-advanced runs. Each test is a real
// isolated schema (dispatcherDB), and the graph reaches the pass the way production delivers it —
// jsonb round-tripped, so every document is plain map[string]any.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// seedWorkflowRunScene loads one tenant with a workflow, a frozen snapshot and a pending run whose
// graph is the given document, returning the run id the assertions read back. The scene is minimal
// on purpose: the advance pass reads workflow_runs joined to workflow_snapshots and nothing else.
func seedWorkflowRunScene(t *testing.T, store *Store, graph Object) string {
	t.Helper()
	wid, sid, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tid, uid := uuid.NewString(), uuid.NewString()
	g, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	tx, err := store.Pool.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	exec := func(name, q string, args ...any) {
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	exec("user", `INSERT INTO users(id,display_name,status) VALUES($1,'wf','active')`, uid)
	exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'wf','active')`, tid)
	exec("membership", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tid, uid)
	exec("workflow", `INSERT INTO workflows(id,tenant_id,name,graph) VALUES($1,$2,'wf',$3::jsonb)`, wid, tid, g)
	exec("snapshot", `INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,1,'v1',$4::jsonb)`, sid, tid, wid, g)
	exec("run", `INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input) VALUES($1,$2,$3,$4,'disp','pending','{}'::jsonb)`, runID, tid, wid, sid)
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return runID
}

// readWorkflowRun reads the run row back at the storage boundary: the settled status and error, the
// committed node states and the version the CAS bumped.
func readWorkflowRun(t *testing.T, store *Store, runID string) (status, runErr string, states Object, version int64) {
	t.Helper()
	var raw []byte
	if err := store.Pool.QueryRow(`SELECT status, error, node_states, version FROM workflow_runs WHERE id=$1`, runID).
		Scan(&status, &runErr, &raw, &version); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if err := json.Unmarshal(raw, &states); err != nil {
		t.Fatalf("decode node_states: %v", err)
	}
	return status, runErr, states, version
}

// One pass claims the pending run, executes start then output from the frozen graph, and settles the
// run succeeded with every node's committed state on the row.
func TestWorkflowAdvanceSucceedsLinearRun(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "done")},
		[][3]string{{"s", "o", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	if version != 3 { // claim +1, settle +1
		t.Fatalf("version = %d; want 3", version)
	}
	for id, want := range map[string]string{"s": "succeeded", "o": "succeeded"} {
		state, ok := asWorkflowObject(states[id])
		if !ok {
			t.Fatalf("node %s has no committed state", id)
		}
		if state.S("status") != want {
			t.Fatalf("node %s status = %q; want %q", id, state.S("status"), want)
		}
	}
	if start := states["s"].(map[string]any)["output"].(map[string]any); start["input"] == nil {
		t.Fatal("start node output does not report the run input")
	}
}

// A ready agent node on a run with no project settles the run failed with an explicit reason: the
// session has nowhere to run, so the node is never recorded as succeeded (invariant 5) and nothing
// downstream of it committed a state.
func TestWorkflowAdvanceFailsRunOnProjectlessAgentNode(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("a", "agent", "worker"), wfNode("o", "output", "")},
		[][3]string{{"s", "a", ""}, {"a", "o", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" {
		t.Fatalf("run settled %q; want failed", status)
	}
	if runErr != "workflow_run_project_required" {
		t.Fatalf("run error %q; want workflow_run_project_required", runErr)
	}
	agent, ok := asWorkflowObject(states["a"])
	if !ok {
		t.Fatal("agent node has no committed state")
	}
	if agent.S("status") != "failed" {
		t.Fatalf("agent node status = %q; want failed", agent.S("status"))
	}
	if agent.O("output").S("reason") != "workflow_run_project_required" {
		t.Fatalf("agent node reason = %q; want workflow_run_project_required", agent.O("output").S("reason"))
	}
	if _, ran := states["o"]; ran {
		t.Fatal("output node downstream of the failed agent committed a state")
	}
	if _, ran := states["s"]; !ran {
		t.Fatal("start node before the agent did not commit its state")
	}
}

// Publishing a new version — and editing the live document — while a run is in flight cannot change
// what that run executes: every pass joins the run's own snapshot_id, so the graph it runs is the one
// frozen at creation (invariant 1, "编辑、发布或插件升级都不改变在途运行执行的内容"). The run here is
// still `pending` when version 2 lands, which is the window the invariant is about.
func TestWorkflowAdvanceIgnoresARepublishedVersion(t *testing.T) {
	store := dispatcherDB(t)
	frozen := wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o1", "output", "")},
		[][3]string{{"s", "o1", ""}},
	)
	runID := seedWorkflowRunScene(t, store, frozen)
	var snapshotID string
	if err := store.Pool.QueryRow(`SELECT snapshot_id FROM workflow_runs WHERE id=$1`, runID).Scan(&snapshotID); err != nil {
		t.Fatal(err)
	}
	// The author edits the live document and publishes version 2 after the run exists.
	republished := wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o2", "output", "")},
		[][3]string{{"s", "o2", ""}},
	)
	raw, err := json.Marshal(republished)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE workflows SET graph=$2::jsonb, version=version+1 WHERE id=(SELECT workflow_id FROM workflow_runs WHERE id=$1)`, runID, string(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph)
		SELECT $2,tenant_id,workflow_id,2,'v2',$3::jsonb FROM workflow_runs WHERE id=$1`, runID, uuid.NewString(), string(raw)); err != nil {
		t.Fatal(err)
	}

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	if _, ran := states["o1"]; !ran {
		t.Fatalf("the frozen version's node did not commit: %v", states)
	}
	if _, ran := states["o2"]; ran {
		t.Fatalf("the republished version's node committed: %v", states)
	}
	var after string
	if err := store.Pool.QueryRow(`SELECT snapshot_id FROM workflow_runs WHERE id=$1`, runID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != snapshotID {
		t.Fatalf("run snapshot moved to %s; want the frozen %s", after, snapshotID)
	}
}

// A snapshot that predates the publish gate can carry a node kind the first phase does not execute.
// Such a node must fail honestly with its own reason: the run settles failed and the unsupported
// node is never recorded as succeeded (#69 leaves this judgement to the execution side, D2 refuses
// these kinds at publish for everything published after the gate).
func TestWorkflowAdvanceFailsAnUnsupportedNodeKind(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("l", "loop", "iterate"), wfNode("o", "output", "")},
		[][3]string{{"s", "l", ""}, {"l", "o", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "unsupported_node_kind" {
		t.Fatalf("run settled %q error %q; want failed/unsupported_node_kind", status, runErr)
	}
	node, ok := asWorkflowObject(states["l"])
	if !ok {
		t.Fatalf("the unsupported node has no committed state: %v", states)
	}
	if node.S("status") != "failed" || node.O("output").S("reason") != "unsupported_node_kind" {
		t.Fatalf("unsupported node state = %v; want failed with its reason", states["l"])
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("the node downstream of the unsupported one committed a state: %v", states)
	}
}

// A DAG that reconverges is legal and drains: two paths lead into `c`, and the wave visits `c` once
// after both of them committed. The cycle refusal must not read a join as one — refusing a legal
// document is worse than missing an illegal one, and a join is the shape the two are easiest to
// confuse.
func TestWorkflowAdvanceSucceedsAReconvergentGraph(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("a", "output", ""), wfNode("b", "output", ""), wfNode("c", "output", ""), wfNode("o", "output", "")},
		[][3]string{{"s", "a", ""}, {"s", "b", ""}, {"a", "c", ""}, {"b", "c", ""}, {"c", "o", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	for _, id := range []string{"s", "a", "b", "c", "o"} {
		state, ok := asWorkflowObject(states[id])
		if !ok {
			t.Fatalf("node %s has no committed state: %v", id, states)
		}
		if state.S("status") != "succeeded" {
			t.Fatalf("node %s status = %q; want succeeded", id, state.S("status"))
		}
	}
}

// A snapshot frozen before the publish gate can carry a cycle, and such a run must not settle
// succeeded: a cycle reachable from the start goes round once and stops with its back edge silently
// dropped, so the pass used to report success for a document whose author drew a loop — the node on
// the back edge never produced a result for the edge that pointed back at it. The run settles failed
// instead, and because nothing executed, no node is recorded as having executed: a state written here
// would claim work that never happened.
func TestWorkflowAdvanceFailsACyclicSnapshot(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("a", "output", ""), wfNode("b", "output", "")},
		[][3]string{{"s", "a", ""}, {"a", "b", ""}, {"b", "a", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "graph has a cycle" {
		t.Fatalf("run settled %q error %q; want failed/graph has a cycle", status, runErr)
	}
	if len(states) != 0 {
		t.Fatalf("a refused cyclic run committed node states: %v", states)
	}
	if version != 3 { // claim +1, settle +1, and no node ever committed
		t.Fatalf("version = %d; want 3", version)
	}
	// The refusal is terminal: the retry loop never rescans the run, so it cannot drift to succeeded.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if status, _, _, after := readWorkflowRun(t, store, runID); status != "failed" || after != version {
		t.Fatalf("second pass moved the run: %q version %d; want failed version %d", status, after, version)
	}
}

// A cycle nothing reaches is refused the same way. The projection would leave these nodes unresolved
// — each waits for an upstream that waits for it — and the run used to settle succeeded with them
// carrying no state at all, which is the plainest form of "succeeded with nodes that never ran".
func TestWorkflowAdvanceFailsAHistoricalCycleBehindTheStart(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", ""), wfNode("a", "output", ""), wfNode("b", "output", "")},
		[][3]string{{"s", "o", ""}, {"a", "b", ""}, {"b", "a", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "graph has a cycle" {
		t.Fatalf("run settled %q error %q; want failed/graph has a cycle", status, runErr)
	}
	for _, id := range []string{"s", "o", "a", "b"} {
		if _, ran := states[id]; ran {
			t.Fatalf("node %s committed a state on a refused run: %v", id, states)
		}
	}
}

// The pass is idempotent: a second tick over a settled run rescans nothing and leaves the committed
// row untouched — no second claim, no second settle, no version drift.
func TestWorkflowAdvanceIsIdempotent(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "")},
		[][3]string{{"s", "o", ""}},
	))

	for i := 0; i < 3; i++ {
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
	}
	status, runErr, _, version := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	if version != 3 {
		t.Fatalf("version = %d after three passes; want 3 (claim + settle only)", version)
	}
}

// wfOneRuleNode is a Condition node with exactly one case, matching when `operator` holds for the
// variable at `selector`. It exists so a DB test can name the operator under test without spelling
// out the whole case document.
func wfOneRuleNode(id, caseID string, selector []any, operator string) Object {
	return wfConditionNode(id, []any{
		Object{"id": caseID, "logic": "and", "conditions": []any{
			Object{"variableSelector": selector, "operator": operator, "value": nil},
		}},
	})
}

// wfConditionScene seeds a start -> condition -> {taken, else} graph whose single case is the given
// operator over the given selector, and returns the run id. The run's input is {} — see
// seedWorkflowRunScene — which is the case the empty-input tests below turn on.
func wfConditionScene(t *testing.T, store *Store, caseID string, selector []any, operator string) string {
	t.Helper()
	return seedWorkflowRunScene(t, store, wfGraph(
		[]Object{
			wfNode("s", "start", ""),
			wfOneRuleNode("c", caseID, selector, operator),
			wfNode("e", "output", ""),
			wfNode("f", "output", ""),
		},
		[][3]string{{"s", "c", ""}, {"c", "e", caseID}, {"c", "f", workflowBranchElse}},
	))
}

// wfSelectedBranch advances one pass and returns the branch the condition node committed.
func wfSelectedBranch(t *testing.T, store *Store, runID string) string {
	t.Helper()
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	_, _, states, _ := readWorkflowRun(t, store, runID)
	cond, ok := asWorkflowObject(states["c"])
	if !ok {
		t.Fatal("condition node has no committed state")
	}
	return cond.O("output").S("selectedBranch")
}

// wfNodeStatus reads one committed node's status, failing the test if the node has no state at all.
func wfNodeStatus(t *testing.T, states Object, nodeID string) string {
	t.Helper()
	state, ok := asWorkflowObject(states[nodeID])
	if !ok {
		t.Fatalf("node %s has no committed state", nodeID)
	}
	return state.S("status")
}

// A condition reading the run's own input sees an empty input as empty. This is the end-to-end shape
// of the named-type trap: the start node nests the run input as the package's Object, so the value a
// selector walks out is an Object too, and a type switch that only names map[string]any falls
// through it. Before asWorkflowObject was used there, `empty` answered false on `{}` and the run took
// the branch desktop's is_empty would not have taken — a legal start -> condition graph, decided
// wrong, with nothing in the run to say so.
func TestWorkflowAdvanceConditionOnEmptyRunInput(t *testing.T) {
	store := dispatcherDB(t)
	runID := wfConditionScene(t, store, "was_empty", []any{"s", "input"}, "empty")

	if branch := wfSelectedBranch(t, store, runID); branch != "was_empty" {
		t.Fatalf("condition selected %q; want was_empty — an empty run input read as not-empty", branch)
	}
	_, _, states, _ := readWorkflowRun(t, store, runID)
	if got := wfNodeStatus(t, states, "e"); got != "succeeded" {
		t.Fatalf("taken branch status = %q; want succeeded", got)
	}
	// The untaken branch is committed as inactive rather than left absent: the run says which branch
	// it did not take, which is what makes a wrong decision visible after the fact.
	if got := wfNodeStatus(t, states, "f"); got != "inactive" {
		t.Fatalf("else branch status = %q; want inactive — the else branch ran on an empty input", got)
	}
}

// not_empty is empty's negation on the same document: the same start -> condition graph, the same
// empty input, and the case must not match.
func TestWorkflowAdvanceConditionNotEmptyOnEmptyRunInput(t *testing.T) {
	store := dispatcherDB(t)
	runID := wfConditionScene(t, store, "was_full", []any{"s", "input"}, "not_empty")

	if branch := wfSelectedBranch(t, store, runID); branch != workflowBranchElse {
		t.Fatalf("condition selected %q; want else — an empty run input read as not-empty", branch)
	}
}

// not_contains must not match an absent variable, and the run has to prove it end to end: the
// selector names a path the start node never produced, and the case is written with not_contains, the
// one negated comparator whose absence handling is not simply the inner verdict flipped.
func TestWorkflowAdvanceConditionNotContainsOnAbsentVariable(t *testing.T) {
	store := dispatcherDB(t)
	runID := wfConditionScene(t, store, "absent", []any{"s", "never_written"}, "not_contains")

	if branch := wfSelectedBranch(t, store, runID); branch != workflowBranchElse {
		t.Fatalf("condition selected %q; want else — not_contains matched an absent variable", branch)
	}
}
