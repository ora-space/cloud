package core

// DB tests for the Cloud-side workflow scheduler's advance pass (workflow_scheduler.go): a pending
// run is claimed and settled from the frozen snapshot's graph, an agent node a run cannot execute
// fails it honestly, and the pass is idempotent against already-advanced runs. Each test is a real
// isolated schema (dispatcherDB), and the graph reaches the pass the way production delivers it —
// jsonb round-tripped, so every document is plain map[string]any.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// seedWorkflowRunScene loads one tenant with a workflow, a frozen snapshot and a pending run whose
// graph is the given document, returning the run id the assertions read back. The scene is minimal
// on purpose: the advance pass reads workflow_runs joined to workflow_snapshots and nothing else.
func seedWorkflowRunScene(t *testing.T, store *Store, graph Object) string {
	t.Helper()
	return seedWorkflowRunSceneWithInput(t, store, graph, Object{})
}

// seedWorkflowRunSceneWithInput is seedWorkflowRunScene with a run input, written as the jsonb the
// creation transaction stores so the pass reads exactly the document a real run carries.
func seedWorkflowRunSceneWithInput(t *testing.T, store *Store, graph, input Object) string {
	t.Helper()
	wid, sid, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tid, uid := uuid.NewString(), uuid.NewString()
	g, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	in, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal run input: %v", err)
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
	exec("run", `INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input) VALUES($1,$2,$3,$4,'disp','pending',$5::jsonb)`, runID, tid, wid, sid, in)
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
	start := wfNode("s", "start", "")
	start["data"].(Object)["input"] = "review the diff"
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{start, wfNode("o", "output", "done")},
		[][3]string{{"s", "o", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	if version != 4 { // claim +1, settle +1, release fact +1 (not_required: the graph has no agent node)
		t.Fatalf("version = %d; want 4", version)
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
	// No kickoff was submitted, so the authored Start instruction is the run's kickoff — the same
	// fallback every row created before the kickoff was persisted depends on.
	if got := states["s"].(map[string]any)["output"].(map[string]any)["input"]; got != "review the diff" {
		t.Fatalf("start node output input = %v; want the authored prompt", got)
	}
}

// The kickoff stored on the run row is what [start, input] resolves, both as a selector and through
// a {{#start.input#}} expected-value template. The authored instruction is present too and
// different, so a pass that still read the document would select the other branches.
func TestWorkflowAdvanceStartInputUsesTheStoredKickoff(t *testing.T) {
	store := dispatcherDB(t)
	start := wfNode("s", "start", "")
	start["data"].(Object)["input"] = "authored prompt"
	rule := func(id string, selector []any, value any) Object {
		return Object{"id": id, "logic": "and", "conditions": []any{
			Object{"variableSelector": selector, "operator": "equals", "value": value},
		}}
	}
	graph := wfGraph(
		[]Object{
			start,
			wfConditionNode("c", []any{rule("literal", []any{"s", "input"}, "run override")}),
			wfConditionNode("t", []any{rule("templated", []any{"s", "marker"}, "{{#s.input#}}")}),
			wfNode("e", "output", ""),
			wfNode("f", "output", ""),
			wfNode("g", "output", ""),
			wfNode("h", "output", ""),
		},
		[][3]string{
			{"s", "c", ""},
			{"c", "e", "literal"},
			{"c", "f", workflowBranchElse},
			{"s", "t", ""},
			{"t", "g", "templated"},
			{"t", "h", workflowBranchElse},
		},
	)
	runID := seedWorkflowRunSceneWithInput(t, store, graph,
		Object{"input": "run override", "marker": "run override"})

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	_, _, states, _ := readWorkflowRun(t, store, runID)
	branchOf := func(id string) string {
		state, ok := asWorkflowObject(states[id])
		if !ok {
			t.Fatalf("condition %s has no committed state", id)
		}
		return state.O("output").S("selectedBranch")
	}
	if got := branchOf("c"); got != "literal" {
		t.Fatalf("condition selected %q; want literal — [start, input] did not read the stored kickoff", got)
	}
	if got := branchOf("t"); got != "templated" {
		t.Fatalf("condition selected %q; want templated — {{#start.input#}} did not read the stored kickoff", got)
	}
	if got := states["s"].(map[string]any)["output"].(map[string]any)["input"]; got != "run override" {
		t.Fatalf("start node output input = %v; want the stored kickoff", got)
	}
}

// A run row that already carries an `input` key predates the kickoff convention: before it, the key
// was just another variable value in the submitted form. Reading a string key as this run's kickoff
// is a deliberate reinterpretation of durable state, so both directions are pinned here — a string
// key is the kickoff, and a key of any other type falls back to the authored instruction instead of
// reaching the pool. The row is seeded directly because a pre-existing row is exactly what the
// creation gate never saw; the advance pass must still read it deterministically.
func TestWorkflowAdvanceReinterpretsTheHistoricalStringInputKey(t *testing.T) {
	store := dispatcherDB(t)
	for _, tc := range []struct {
		name  string
		input any
		want  string
	}{
		{name: "string key is the kickoff", input: "historical kickoff", want: "historical kickoff"},
		{name: "non-string key falls back to the authored prompt", input: 7, want: "authored prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := wfNode("s", "start", "")
			start["data"].(Object)["input"] = "authored prompt"
			runID := seedWorkflowRunSceneWithInput(t, store, wfGraph(
				[]Object{start, wfNode("o", "output", "done")},
				[][3]string{{"s", "o", ""}},
			), Object{"input": tc.input})

			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatalf("advance: %v", err)
			}
			status, runErr, states, _ := readWorkflowRun(t, store, runID)
			if status != "succeeded" || runErr != "" {
				t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
			}
			if got := states["s"].(map[string]any)["output"].(map[string]any)["input"]; got != tc.want {
				t.Fatalf("start node output input = %v; want %q", got, tc.want)
			}
		})
	}
}

// With neither a stored kickoff nor an authored instruction {start}.input is unset, as it is in
// desktop's pool: `not_exists` matches and `exists` would not. It is not a present empty string,
// which would answer both existence operators the other way.
func TestWorkflowAdvanceConditionOnUnsetKickoffReadsAbsent(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{
			wfNode("s", "start", ""),
			wfOneRuleNode("c", "absent", []any{"s", "input"}, "not_exists"),
			wfNode("e", "output", ""),
			wfNode("f", "output", ""),
		},
		[][3]string{{"s", "c", ""}, {"c", "e", "absent"}, {"c", "f", workflowBranchElse}},
	))

	if branch := wfSelectedBranch(t, store, runID); branch != "absent" {
		t.Fatalf("condition selected %q; want absent — an unset kickoff read as present", branch)
	}
	var activities, linked int
	if err := store.Pool.QueryRow(`SELECT (SELECT count(*) FROM issue_activities),
		(SELECT count(*) FROM issue_runs WHERE workflow_invocation_ref=$1)`, runID).Scan(&activities, &linked); err != nil || activities != 0 || linked != 0 {
		t.Fatalf("direct run created collaboration records: activities=%d linked=%d err=%v", activities, linked, err)
	}
}

func TestWorkflowAdvanceProjectsConcurrentPassesOnce(t *testing.T) {
	store := dispatcherDB(t)
	s := seedWorkflowProjection(t, store)
	graph := wfGraph([]Object{wfNode("s", "start", ""), wfNode("o", "output", "")}, [][3]string{{"s", "o", ""}})
	if _, err := store.Pool.Exec(`UPDATE workflow_snapshots SET graph=$2::jsonb
		WHERE id=(SELECT snapshot_id FROM workflow_runs WHERE id=$1)`, s.workflowRun, jsonText(graph)); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = store.AdvanceWorkflowRunsOnce(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	var version int
	if err := store.Pool.QueryRow("SELECT status,version FROM issue_runs WHERE id=$1", s.issueRun).Scan(&status, &version); err != nil || status != "completed" || version != 3 {
		t.Fatalf("linked run: status=%s version=%d err=%v", status, version, err)
	}
	activities := workflowProjectionActivities(t, store, s)
	actions := make([]string, len(activities))
	for i, activity := range activities {
		actions[i] = activity.S("action")
	}
	if !reflect.DeepEqual(actions, []string{"run.enqueued", "run.started", "run.completed"}) {
		t.Fatalf("concurrent/repeated passes emitted %v", actions)
	}
}

func TestWorkflowSettleLostCASDoesNotProject(t *testing.T) {
	store := dispatcherDB(t)
	for _, withStates := range []bool{false, true} {
		name := "without-states"
		if withStates {
			name = "with-states"
		}
		t.Run(name, func(t *testing.T) {
			s := seedWorkflowProjection(t, store)
			// A concurrent writer won, but its IssueRun is deliberately still queued: calling
			// projection after our lost CAS would now emit run.started from that winner's row.
			if _, err := store.Pool.Exec("UPDATE workflow_runs SET status='running',started_at=now(),version=version+1 WHERE id=$1", s.workflowRun); err != nil {
				t.Fatal(err)
			}
			var states map[string]workflowNodeState
			if withStates {
				states = map[string]workflowNodeState{"s": {Status: "succeeded"}}
			}
			_, err := store.transact(context.Background(), func(tx *transaction) Object {
				if settleWorkflowRunStates(tx, s.workflowRun, 1, "failed", "stale failure", states) {
					t.Fatal("stale version unexpectedly applied")
				}
				return Object{}
			})
			if err != nil {
				t.Fatal(err)
			}
			status, runErr, committed, version := readWorkflowRun(t, store, s.workflowRun)
			if status != "running" || runErr != "" || len(committed) != 0 || version != 2 {
				t.Fatalf("lost CAS changed winner: %s %q %v version=%d", status, runErr, committed, version)
			}
			var issueStatus string
			if err := store.Pool.QueryRow("SELECT status FROM issue_runs WHERE id=$1", s.issueRun).Scan(&issueStatus); err != nil || issueStatus != "queued" {
				t.Fatalf("lost CAS projected: status=%s err=%v", issueStatus, err)
			}
			if activities := workflowProjectionActivities(t, store, s); len(activities) != 1 {
				t.Fatalf("lost CAS emitted activities: %v", activities)
			}
		})
	}
}

func TestWorkflowSettleTruncatesIssueSummary(t *testing.T) {
	store := dispatcherDB(t)
	for _, source := range []string{"error", "output"} {
		t.Run(source, func(t *testing.T) {
			s := seedWorkflowProjection(t, store)
			text := strings.Repeat("界", 7000)
			status, runErr := "failed", text
			var states map[string]workflowNodeState
			if source == "output" {
				graph := wfGraph([]Object{wfNode("s", "start", ""), wfNode("o", "output", "")}, [][3]string{{"s", "o", ""}})
				if _, err := store.Pool.Exec(`UPDATE workflow_snapshots SET graph=$2::jsonb
					WHERE id=(SELECT snapshot_id FROM workflow_runs WHERE id=$1)`, s.workflowRun, jsonText(graph)); err != nil {
					t.Fatal(err)
				}
				status, runErr = "succeeded", ""
				states = map[string]workflowNodeState{"o": {Status: "succeeded", Output: Object{"text": text}}}
			}
			_, err := store.transact(context.Background(), func(tx *transaction) Object {
				if !settleWorkflowRunStates(tx, s.workflowRun, 1, status, runErr, states) {
					t.Fatal("settle did not apply")
				}
				return Object{}
			})
			if err != nil {
				t.Fatalf("oversized summary rolled back settlement: %v", err)
			}
			activities := workflowProjectionActivities(t, store, s)
			if len(activities) != 3 || activities[1].S("action") != "run.message" {
				t.Fatalf("terminal summary activities: %v", activities)
			}
			message := activities[1].O("details").S("message")
			if len(message) < 19997 || len(message) > 20000 || !utf8.ValidString(message) {
				t.Fatalf("summary bytes=%d valid UTF-8=%v", len(message), utf8.ValidString(message))
			}
			actual, actualErr, _, version := readWorkflowRun(t, store, s.workflowRun)
			if actual != status || actualErr != runErr || version != 2 {
				t.Fatalf("settle did not preserve execution result: status=%s error bytes=%d version=%d", actual, len(actualErr), version)
			}
			var issueStatus string
			wantIssueStatus := "failed"
			if status == "succeeded" {
				wantIssueStatus = "completed"
			}
			if err := store.Pool.QueryRow("SELECT status FROM issue_runs WHERE id=$1", s.issueRun).Scan(&issueStatus); err != nil || issueStatus != wantIssueStatus {
				t.Fatalf("terminal IssueRun: status=%s err=%v", issueStatus, err)
			}
		})
	}
}

func TestWorkflowAdvanceProjectionFailureRollsBackClaim(t *testing.T) {
	store := dispatcherDB(t)
	s := seedWorkflowProjection(t, store)
	if _, err := store.Pool.Exec("ALTER TABLE issue_activities ADD CONSTRAINT reject_scheduler_start CHECK(action <> 'run.started')"); err != nil {
		t.Fatal(err)
	}
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		advanceWorkflowRun(tx, s.workflowRun, store.DeliveryUnreachableAfter)
		return Object{}
	})
	if err == nil {
		t.Fatal("failed projection must abort the scheduler transaction")
	}
	status, _, _, version := readWorkflowRun(t, store, s.workflowRun)
	if status != "pending" || version != 1 {
		t.Fatalf("projection failure left claimed workflow: %s version=%d", status, version)
	}
	var issueStatus string
	if err := store.Pool.QueryRow("SELECT status FROM issue_runs WHERE id=$1", s.issueRun).Scan(&issueStatus); err != nil || issueStatus != "queued" {
		t.Fatalf("projection failure changed IssueRun: status=%s err=%v", issueStatus, err)
	}
	if activities := workflowProjectionActivities(t, store, s); len(activities) != 1 {
		t.Fatalf("rolled-back claim emitted activities: %v", activities)
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
	if agent.S("sessionEndReason") != "workflow_run_project_required" {
		t.Fatalf("agent node reason = %q; want workflow_run_project_required", agent.S("sessionEndReason"))
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
		[]Object{wfNode("s", "start", ""), wfNode("l", "subflow", "subflow"), wfNode("o", "output", "")},
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
	if version != 4 { // claim +1, settle +1, release fact +1, and no node ever committed
		t.Fatalf("version = %d; want 4", version)
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
	if version != 4 {
		t.Fatalf("version = %d after three passes; want 4 (claim + settle + the release fact, with later passes adding nothing)", version)
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
// seedWorkflowRunScene — and the Start node's authored prompt is empty, so {start}.input is present
// and empty: the case the empty-value tests below turn on.
func wfConditionScene(t *testing.T, store *Store, caseID string, selector []any, operator string) string {
	t.Helper()
	start := wfNode("s", "start", "")
	start["data"].(Object)["input"] = ""
	return seedWorkflowRunScene(t, store, wfGraph(
		[]Object{
			start,
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

// An authored Start instruction that is empty is present and empty, not unset: [start, input] is
// the empty string, so `empty` matches. The distinction matters because an unset root is a
// different state — `not_exists` matches it and `exists` does not — which the unset-kickoff test
// below pins.
func TestWorkflowAdvanceConditionOnEmptyAuthoredPrompt(t *testing.T) {
	store := dispatcherDB(t)
	runID := wfConditionScene(t, store, "was_empty", []any{"s", "input"}, "empty")

	if branch := wfSelectedBranch(t, store, runID); branch != "was_empty" {
		t.Fatalf("condition selected %q; want was_empty — an empty authored prompt read as not-empty", branch)
	}
	_, _, states, _ := readWorkflowRun(t, store, runID)
	if got := wfNodeStatus(t, states, "e"); got != "succeeded" {
		t.Fatalf("taken branch status = %q; want succeeded", got)
	}
	// The untaken branch is committed as inactive rather than left absent: the run says which branch
	// it did not take, which is what makes a wrong decision visible after the fact.
	if got := wfNodeStatus(t, states, "f"); got != "inactive" {
		t.Fatalf("else branch status = %q; want inactive — the else branch ran on an empty prompt", got)
	}
}

// not_empty is empty's negation on the same document: the same start -> condition graph, the same
// empty authored prompt, and the case must not match.
func TestWorkflowAdvanceConditionNotEmptyOnEmptyAuthoredPrompt(t *testing.T) {
	store := dispatcherDB(t)
	runID := wfConditionScene(t, store, "was_full", []any{"s", "input"}, "not_empty")

	if branch := wfSelectedBranch(t, store, runID); branch != workflowBranchElse {
		t.Fatalf("condition selected %q; want else — an empty authored prompt read as not-empty", branch)
	}
}

// A failed node decides the run in the pass that produced it. Here the condition fails and an agent
// sibling is ready in the same wave; the pass must stop before dispatching it. Dispatching would
// provision a Workspace and start a session for a run that is already terminal, and it would give the
// agent a committed state — the row would then claim work that could only end in a failure the
// condition had already decided.
func TestWorkflowAdvanceDoesNotDispatchSiblingAfterFailure(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{
			wfNode("s", "start", ""),
			wfOneRuleNode("c", "matched", []any{"s", "never_written"}, "equals"),
			wfNode("a", "agent", "worker"),
		},
		[][3]string{{"s", "c", ""}, {"s", "a", ""}},
	))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" {
		t.Fatalf("run status = %q; want failed", status)
	}
	if !strings.Contains(runErr, "s.never_written") {
		t.Fatalf("run error = %q; want the condition's evaluation error", runErr)
	}
	if got := wfNodeStatus(t, states, "c"); got != "failed" {
		t.Fatalf("condition status = %q; want failed", got)
	}
	if state, ran := states["a"]; ran {
		t.Fatalf("sibling agent committed %v; want no state — it must not be dispatched for a failed run", state)
	}
}

// A comparator on an absent variable fails the node and the run, as desktop's ConditionRuntime does.
// not_contains is the case that used to be special-cased: #67 made it answer false so its negated
// form would agree with not_equals, but desktop raises UnsetVariable for both. Parity means the run
// says which selector was never assigned instead of quietly taking else.
func TestWorkflowAdvanceConditionNotContainsOnAbsentVariable(t *testing.T) {
	store := dispatcherDB(t)
	runID := wfConditionScene(t, store, "absent", []any{"s", "never_written"}, "not_contains")

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" {
		t.Fatalf("run status = %q; want failed", status)
	}
	if !strings.Contains(runErr, "s.never_written") || !strings.Contains(runErr, "has not been assigned") {
		t.Fatalf("run error = %q; want the unset selector named", runErr)
	}
	if got := wfNodeStatus(t, states, "c"); got != "failed" {
		t.Fatalf("condition status = %q; want failed", got)
	}
}

// TestWorkflowAdvanceReadsDesktopGlobals drives a condition against a desktop-exported graph that
// declares globals, through the same advance pass a real run takes. The graph is the envelope
// desktop's serializeWorkflowGraph emits — nodes carry `type: "workflow"`, edges carry `kind: "data"`,
// and each global is `{name, valueType, value}`. The three selectors the run's condition references —
// the runtime-owned `sys.workflow_id` / `sys.timestamp` and a frozen user global — must resolve to
// the values the run was created with, and the ambient pseudo-nodes must not leak into node_states.
func TestWorkflowAdvanceReadsDesktopGlobals(t *testing.T) {
	store := dispatcherDB(t)
	wid, sid, runID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	tid, uid := uuid.NewString(), uuid.NewString()
	created := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	createdMs := created.UnixMilli()

	graph := Object{
		"nodes": []any{
			Object{"id": "start-1", "type": "workflow", "data": Object{"kind": "start", "title": "开始", "description": ""}},
			Object{"id": "cond-1", "type": "workflow", "data": Object{"kind": "condition", "title": "分支", "description": "", "cases": []any{
				Object{"id": "matched", "logic": "and", "conditions": []any{
					Object{"variableSelector": []any{"sys", "workflow_id"}, "operator": "equals", "value": wid},
					Object{"variableSelector": []any{"sys", "timestamp"}, "operator": "equals", "value": float64(createdMs)},
					Object{"variableSelector": []any{"g", "region"}, "operator": "equals", "value": "eu"},
				}},
			}}},
			Object{"id": "out-1", "type": "workflow", "data": Object{"kind": "output", "title": "出口", "description": ""}},
			Object{"id": "out-2", "type": "workflow", "data": Object{"kind": "output", "title": "旁路", "description": ""}},
		},
		"edges": []any{
			Object{"id": "e1", "kind": "data", "source": "start-1", "target": "cond-1"},
			Object{"id": "e2", "kind": "data", "source": "cond-1", "target": "out-1", "sourceHandle": "matched"},
			Object{"id": "e3", "kind": "data", "source": "cond-1", "target": "out-2"},
		},
		"viewport":        Object{"x": 0, "y": 0, "zoom": 1},
		"annotations":     []any{},
		"globalVariables": []any{Object{"name": "g.region", "valueType": "string", "value": "eu"}},
	}

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
	exec("run", `INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,created_at) VALUES($1,$2,$3,$4,'disp','pending','{}'::jsonb,$5)`, runID, tid, wid, sid, created)
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	cond, ok := asWorkflowObject(states["cond-1"])
	if !ok || cond.O("output").S("selectedBranch") != "matched" {
		t.Fatalf("condition state = %v; want selectedBranch matched", states["cond-1"])
	}
	for _, ambient := range []string{"sys", "g"} {
		if _, leaked := states[ambient]; leaked {
			t.Fatalf("ambient root %q leaked into node_states", ambient)
		}
	}
}
