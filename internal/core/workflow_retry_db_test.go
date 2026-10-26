package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

// DB tests for workflow agent-node retry (specs decisions/cloud/workflow
// 20261021-node-retry-and-resume-contract.md D1–D6).
//
// The decision's obligations are proved on the shipped seams, not around them: the session result
// arrives through `executionResult`, the scheduler wakes through `AdvanceWorkflowRunsOnce`, and the
// cancellation request goes through `Store.Public` exactly as the router drives it. Only two
// preconditions are staged directly — a settled run Workspace (the control plane's asynchronous
// create) and, where a case needs the committed due instant already passed, the `retryAt` value
// itself, because the database clock is the only authority the scheduler reads and a test must not
// sleep for it.

// workflowRetryGraph is the linear start -> agent -> output graph with an explicit retry policy on
// the agent node. The policy is written where the published gate reads it, so the scheduler parses
// exactly what an author would have published.
func workflowRetryGraph(enabled bool, maxRetries, initialDelaySeconds int64) Object {
	return wfGraph(
		[]Object{
			wfNode("s", "start", ""),
			wfAgentNode("a", Object{
				"prompt": "Do the thing",
				"retry": Object{
					"enabled": enabled, "maxRetries": maxRetries, "initialDelaySeconds": initialDelaySeconds,
				},
			}),
			wfNode("o", "output", ""),
		},
		[][3]string{{"s", "a", ""}, {"a", "o", ""}},
	)
}

// readWorkflowNodeState reads one node's committed state from the run row.
func readWorkflowNodeState(t *testing.T, store *Store, runID, nodeID string) Object {
	t.Helper()
	_, _, states, _ := readWorkflowRun(t, store, runID)
	state, _ := asWorkflowObject(states[nodeID])
	if state == nil {
		t.Fatalf("run %s carries no state for node %s", runID, nodeID)
	}
	return state
}

// requireRetrying asserts the committed state of a node waiting for its next attempt (D3): the
// attempt that failed, its reason, the kept start instant and no finish instant, plus the committed
// due instant.
func requireRetrying(t *testing.T, state Object, attempt int64) time.Time {
	t.Helper()
	if state.S("status") != "retrying" || state.N("attempt") != attempt {
		t.Fatalf("state = %v; want retrying attempt %d", state, attempt)
	}
	if state.S("retryAt") == "" {
		t.Fatalf("state = %v; a retrying node must carry retryAt", state)
	}
	if state.S("finishedAt") != "" {
		t.Fatalf("state = %v; a retrying node has not finished", state)
	}
	if state.S("startedAt") == "" {
		t.Fatalf("state = %v; the failed attempt's start instant must be kept", state)
	}
	if state.S("error") != "agent_failed" || state.O("output").S("reason") != "agent_failed" {
		t.Fatalf("state = %v; want the failed attempt's reason kept as agent_failed", state)
	}
	at, err := time.Parse(time.RFC3339, state.S("retryAt"))
	if err != nil {
		t.Fatalf("retryAt %q is not RFC 3339: %v", state.S("retryAt"), err)
	}
	return at
}

// makeWorkflowRetryDue moves the committed retry instant into the past. The scheduler judges only
// `retryAt <= clock_timestamp()`, so this is the one boundary a test crosses to advance the database
// clock without sleeping.
func makeWorkflowRetryDue(t *testing.T, store *Store, runID, nodeID string) {
	t.Helper()
	if _, err := store.Pool.Exec(`UPDATE workflow_runs
		SET node_states = jsonb_set(node_states, ARRAY[$2::text,$3::text], to_jsonb(clock_timestamp() - interval '1 second'))
		WHERE id=$1`, runID, nodeID, "retryAt"); err != nil {
		t.Fatalf("move retryAt into the past: %v", err)
	}
}

// dispatchSessionAttempt dispatches an attempt's released session work and passes the scheduler once,
// which holds the committed running node until terminal evidence arrives. The execution id must be
// unique per attempt: the same id with a different input is a conflict, and the re-keyed per-attempt
// uniqueness is exactly what lets a second attempt register at all.
func dispatchSessionAttempt(t *testing.T, store *Store, runID, graphNode, nodeService, execution string, spec Object) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "nodeId": nodeService, "input": spec, "epoch": int64(1)}})
		return Object{}
	}); err != nil {
		t.Fatalf("dispatch node %s attempt %s: %v", graphNode, execution, err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after dispatching %s: %v", execution, err)
	}
	state := readWorkflowNodeState(t, store, runID, graphNode)
	if state.S("status") != "running" {
		t.Fatalf("dispatched session held: %v", state)
	}
}

// sessionWorkAttempt reads the attempt recorded on a node's unregistered work item — Cloud's own
// routing identity, which the dispatch copies onto the execution.
func sessionWorkAttempt(t *testing.T, store *Store, runID, graphNode string) int64 {
	t.Helper()
	var attempt int64
	if err := store.Pool.QueryRow(`SELECT workflow_node_attempt FROM execution_work
		WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND execution_id IS NULL`, runID, graphNode).Scan(&attempt); err != nil {
		t.Fatalf("read session work attempt for %s: %v", graphNode, err)
	}
	return attempt
}

// runWorkspaceReleaseState reads the run's release fact; `pending` is what a non-terminal run
// carries, because no release was declared yet.
func runWorkspaceReleaseState(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var state string
	if err := store.Pool.QueryRow(`SELECT COALESCE(workspace_release_state,'') FROM workflow_runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read workspace release state: %v", err)
	}
	return state
}

// A retryable failure commits `retrying`, not a terminal state: the run stays `running`, the node
// keeps the failed attempt's evidence and start instant, and the committed wait is the database
// clock's instant. Until that instant passes a pass writes nothing. Once it is due, the next attempt
// starts through the ordinary session path with the next attempt number, and the run settles from
// that attempt's own result — a lookup scoped to attempt 2 must not read attempt 1's failure.
func TestWorkflowRetryWaitsThenStartsTheNextAttempt(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 2, 300))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a")
	firstStarted := readWorkflowNodeState(t, store, runID, "a").S("startedAt")
	execution1 := "exec-a-attempt-1-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution1, spec1)
	reportSessionEnd(t, store, runID, execution1, nodeService, "agent_failed")

	status, runErr, _, version := readWorkflowRun(t, store, runID)
	state := readWorkflowNodeState(t, store, runID, "a")
	retryAt := requireRetrying(t, state, 1)
	if status != "running" || runErr != "" {
		t.Fatalf("run = %q error %q; a waiting retry keeps the run running", status, runErr)
	}
	if state.S("startedAt") != firstStarted {
		t.Fatalf("startedAt = %q; want the failed attempt's %q", state.S("startedAt"), firstStarted)
	}
	// The wait is the policy's 300 seconds measured on the database clock.
	if wait := time.Until(retryAt); wait < 295*time.Second || wait > 305*time.Second {
		t.Fatalf("retryAt = %s (in %s); want about 300s from now", state.S("retryAt"), wait)
	}
	if got := runWorkspaceReleaseState(t, store, runID); got != "pending" {
		t.Fatalf("workspace release state = %q; a waiting retry is not terminal", got)
	}
	var unregistered int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatal(err)
	}
	if unregistered != 0 {
		t.Fatalf("%d work items released while the retry waits; none should be", unregistered)
	}

	// Replaying the same terminal receipt — the Controller resending an already recorded result — is
	// idempotent: the waiting state, its instant and the run version stay exactly as committed, and
	// the replay releases no work. This is the late/duplicate callback half of the acceptance; the
	// repeated wake below covers the other half.
	reportSessionEnd(t, store, runID, execution1, nodeService, "agent_failed")
	if replayed := readWorkflowNodeState(t, store, runID, "a"); jsonText(replayed) != jsonText(state) {
		t.Fatalf("state after replaying the receipt = %v; want the unchanged waiting state %v", replayed, state)
	}
	if replayedStatus, replayedErr, _, replayedVersion := readWorkflowRun(t, store, runID); replayedStatus != status || replayedErr != runErr || replayedVersion != version {
		t.Fatalf("run after replaying the receipt = %q error %q version %d; want %q error %q version %d",
			replayedStatus, replayedErr, replayedVersion, status, runErr, version)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatal(err)
	}
	if unregistered != 0 {
		t.Fatalf("%d work items released by the replayed receipt; none should be", unregistered)
	}

	// Before the committed instant the pass is a no-op: no new attempt, no work item.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance before due: %v", err)
	}
	if again := readWorkflowNodeState(t, store, runID, "a"); again.S("status") != "retrying" || again.N("attempt") != 1 || again.S("retryAt") != state.S("retryAt") {
		t.Fatalf("state before due = %v; want the unchanged waiting state", again)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatal(err)
	}
	if unregistered != 0 {
		t.Fatalf("%d work items released before the retry was due", unregistered)
	}

	// Once due, the next attempt starts with the next attempt number and a fresh state.
	makeWorkflowRetryDue(t, store, runID, "a")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance at due: %v", err)
	}
	started := readWorkflowNodeState(t, store, runID, "a")
	if started.S("status") != "running" || started.N("attempt") != 2 {
		t.Fatalf("state after due = %v; want running attempt 2", started)
	}
	if started.S("retryAt") != "" || started.S("finishedAt") != "" || started.S("error") != "" || started["output"] != nil {
		t.Fatalf("state after due = %v; the new attempt starts clean", started)
	}
	if attempt := sessionWorkAttempt(t, store, runID, "a"); attempt != 2 {
		t.Fatalf("released work item names attempt %d; want 2", attempt)
	}

	// The second attempt settles the run from its own result.
	spec2 := sessionWorkSpec(t, store, runID, "a")
	execution2 := "exec-a-attempt-2-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution2, spec2)
	reportSessionEnd(t, store, runID, execution2, nodeService, "idle_timeout")

	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want succeeded from attempt 2", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	outs, _ := asWorkflowObject(states["o"])
	if agent == nil || agent.S("status") != "succeeded" || agent.N("attempt") != 2 || agent.O("output").S("reason") != "idle_timeout" {
		t.Fatalf("agent state = %v; want succeeded attempt 2 with its own result", states["a"])
	}
	if outs == nil || outs.S("status") != "succeeded" {
		t.Fatalf("output state = %v; want succeeded after the retried node", states["o"])
	}

	// Two sessions registered for one node, each against its own attempt: the lifetime uniqueness was
	// re-keyed on the attempt (D4), and the dispatch copied it off the work item.
	rows, err := store.Pool.Query(`SELECT workflow_node_attempt, execution_id FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id='a' AND kind='agent_session' ORDER BY workflow_node_attempt`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type session struct {
		attempt   int64
		execution string
	}
	var sessions []session
	for rows.Next() {
		var s session
		if err := rows.Scan(&s.attempt, &s.execution); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []session{{1, execution1}, {2, execution2}}
	if len(sessions) != len(want) || sessions[0] != want[0] || sessions[1] != want[1] {
		t.Fatalf("registered sessions = %v; want one per attempt %v", sessions, want)
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// Attempt identity scopes the session's end request and its terminal evidence, not only the session
// itself (D4). Once the node has retried, attempt 1's outstanding end request is neither claimable
// for attempt 2's session nor accepted as delivered to it, attempt 2 gets its own request, and
// replaying attempt 1's terminal result cannot settle the in-flight attempt 2.
func TestWorkflowRetryKeepsAttemptOneRequestAndResultOutOfAttemptTwo(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 1, 0))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a")
	execution1 := "exec-attempt-scope-1-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution1, spec1)

	// The initial turn ends, so Cloud releases attempt 1's end request; the session then fails
	// before the request is ever delivered (the Controller may not have claimed it), which leaves
	// the row undelivered while the node waits for its retry.
	takeOverWorkflowEvents(t, store, runID, execution1, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after turn end: %v", err)
	}
	first := claimCommands(t, store)
	if len(first) != 1 || first[0].S("executionId") != execution1 || first[0].S("kind") != "end_session" {
		t.Fatalf("attempt 1 commands = %v; want one end_session addressed to %s", first, execution1)
	}
	reportSessionEndAt(t, store, runID, execution1, nodeService, "agent_failed", 2)
	requireRetrying(t, readWorkflowNodeState(t, store, runID, "a"), 1)

	// The retry starts attempt 2 and registers its own session, which stays in flight.
	makeWorkflowRetryDue(t, store, runID, "a")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance at due: %v", err)
	}
	spec2 := sessionWorkSpec(t, store, runID, "a")
	execution2 := "exec-attempt-scope-2-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution2, spec2)

	// Attempt 1's request names attempt 1: it is not claimable against attempt 2's live session
	// (whose own turn has not ended), and naming that execution on delivery is refused.
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("attempt 2 claimed attempt 1's request: %v", got)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		recordThreadCommandDelivered(tx, &ControlRequest{Body: Object{"commandId": first[0].S("commandId"), "executionId": execution2}})
		return Object{}
	}); err == nil || ErrorCode(err).Code != "dispatch_conflict" {
		t.Fatalf("cross-attempt delivery: err=%v; want dispatch_conflict", err)
	}
	var attempt int64
	var node, deliveredExecution string
	if err := store.Pool.QueryRow(`SELECT workflow_node_attempt, workflow_node_id, COALESCE(delivered_execution_id,'')
		FROM thread_commands WHERE id=$1`, first[0].S("commandId")).Scan(&attempt, &node, &deliveredExecution); err != nil {
		t.Fatalf("read attempt 1's request: %v", err)
	}
	if attempt != 1 || node != "a" || deliveredExecution != "" {
		t.Fatalf("attempt 1's request names node %q attempt %d, delivered to %q; want a/1 delivered to nothing", node, attempt, deliveredExecution)
	}

	// Replaying attempt 1's terminal result is a receipt replay, not evidence for attempt 2: its
	// lookup names the current attempt, so the committed node stays the running attempt 2.
	reportSessionEndAt(t, store, runID, execution1, nodeService, "agent_failed", 2)
	if state := readWorkflowNodeState(t, store, runID, "a"); state.S("status") != "running" || state.N("attempt") != 2 || state.S("retryAt") != "" {
		t.Fatalf("state after replaying attempt 1's result = %v; want running attempt 2", state)
	}
	if status, runErr, _, _ := readWorkflowRun(t, store, runID); status != "running" || runErr != "" {
		t.Fatalf("run after replaying attempt 1's result = %q error %q; want running", status, runErr)
	}

	// Attempt 2's own turn end releases a fresh request addressed to attempt 2 — attempt 1's row
	// neither suppresses it nor gets reused for it — and delivering that request is accepted.
	takeOverWorkflowEvents(t, store, runID, execution2, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after attempt 2 turn end: %v", err)
	}
	second := claimCommands(t, store)
	if len(second) != 1 || second[0].S("executionId") != execution2 || second[0].S("commandId") == first[0].S("commandId") {
		t.Fatalf("attempt 2 commands = %v; want a new request addressed to %s", second, execution2)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		recordThreadCommandDelivered(tx, &ControlRequest{Body: Object{"commandId": second[0].S("commandId"), "executionId": execution2}})
		return Object{}
	}); err != nil {
		t.Fatalf("deliver attempt 2's request: %v", err)
	}
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("commands after delivery: %v; want none", got)
	}
	reportSessionEndAt(t, store, runID, execution2, nodeService, "idle_timeout", 2)
	if status, runErr, _, _ := readWorkflowRun(t, store, runID); status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded from attempt 2", status, runErr)
	}
}

// The wake is serialized by the run's exclusive transaction (D3): two replicas advancing the same
// due retry at the same time release exactly one next attempt. The pass that loses reads the
// winning attempt as running and releases nothing, so the run cannot end up with two session work
// items for the same attempt or skip an attempt number.
func TestWorkflowRetryDueStartsOneAttemptUnderConcurrentPasses(t *testing.T) {
	store := dispatcherDB(t)
	replica := secondReplica(t, store)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 2, 0))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := "exec-concurrent-due-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution, spec)
	reportSessionEnd(t, store, runID, execution, nodeService, "agent_failed")
	requireRetrying(t, readWorkflowNodeState(t, store, runID, "a"), 1)
	makeWorkflowRetryDue(t, store, runID, "a")

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*Store{store, replica} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-start
			errs <- s.AdvanceWorkflowRunsOnce(context.Background())
		}(s)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent pass: %v", err)
		}
	}

	state := readWorkflowNodeState(t, store, runID, "a")
	if state.S("status") != "running" || state.N("attempt") != 2 || state.S("retryAt") != "" {
		t.Fatalf("state after concurrent passes = %v; want running attempt 2 with no retryAt", state)
	}
	rows, err := store.Pool.Query(`SELECT workflow_node_attempt FROM execution_work
		WHERE workflow_run_id=$1 AND workflow_node_id='a' AND execution_id IS NULL ORDER BY workflow_node_attempt`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var released []int64
	for rows.Next() {
		var attempt int64
		if err := rows.Scan(&attempt); err != nil {
			t.Fatal(err)
		}
		released = append(released, attempt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(released) != 1 || released[0] != 2 {
		t.Fatalf("released session work attempts = %v; want exactly [2]", released)
	}
}

// The budget is `maxRetries + 1` attempts (D1/D6). After the last allowed attempt fails, the node
// keeps that attempt's reason and finishes failed, and the run fails and releases its Workspace —
// there is no third attempt and no unregistered work left behind.
func TestWorkflowRetryBudgetExhaustionFailsTheRun(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 1, 0))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a")
	execution1 := "exec-budget-1-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution1, spec1)
	reportSessionEnd(t, store, runID, execution1, nodeService, "agent_failed")
	requireRetrying(t, readWorkflowNodeState(t, store, runID, "a"), 1)

	// maxRetries=1 allows exactly one more attempt.
	makeWorkflowRetryDue(t, store, runID, "a")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to attempt 2: %v", err)
	}
	started := readWorkflowNodeState(t, store, runID, "a")
	if started.S("status") != "running" || started.N("attempt") != 2 {
		t.Fatalf("state after due = %v; want running attempt 2", started)
	}
	spec2 := sessionWorkSpec(t, store, runID, "a")
	execution2 := "exec-budget-2-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution2, spec2)
	reportSessionEnd(t, store, runID, execution2, nodeService, "agent_failed")

	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "agent_failed" {
		t.Fatalf("run settled %q error %q states=%v; want failed with the last attempt's reason", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.N("attempt") != 2 || agent.S("error") != "agent_failed" {
		t.Fatalf("agent state = %v; want failed attempt 2 keeping its reason", states["a"])
	}
	if agent.S("retryAt") != "" || agent.S("finishedAt") == "" {
		t.Fatalf("agent state = %v; a failed node has finished and schedules nothing", agent)
	}
	var unregistered int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatal(err)
	}
	if unregistered != 0 {
		t.Fatalf("%d work items left behind by a failed run", unregistered)
	}
	// A further pass does not invent a third attempt.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after failure: %v", err)
	}
	status, _, states, _ = readWorkflowRun(t, store, runID)
	agent, _ = asWorkflowObject(states["a"])
	if status != "failed" || agent == nil || agent.N("attempt") != 2 {
		t.Fatalf("a terminal run moved: status=%q agent=%v", status, states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// A failure that must not retry settles the node and the run immediately: a disabled policy, a zero
// budget, and a reason outside D2's closed set are all decided before the node status is mapped, so
// none of them leaves a committed wait. `interrupted` is the reason desktop never retries.
func TestWorkflowRetryOnlyRetriesAgentFailedWithinBudget(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		budget  int64
		reason  string
	}{
		{name: "a disabled policy", enabled: false, budget: 2, reason: "agent_failed"},
		{name: "a zero budget", enabled: true, budget: 0, reason: "agent_failed"},
		{name: "an interrupted session", enabled: true, budget: 2, reason: "interrupted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := dispatcherDB(t)
			runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(tc.enabled, tc.budget, 0))
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatalf("advance 1: %v", err)
			}
			wsID := settleSessionWorkspace(t, store, runID)
			nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
			execution := "exec-no-retry-" + runID[:8]
			dispatchSessionAttempt(t, store, runID, "a", nodeService, execution, spec)
			reportSessionEnd(t, store, runID, execution, nodeService, tc.reason)

			status, runErr, states, _ := readWorkflowRun(t, store, runID)
			if status != "failed" || runErr != tc.reason {
				t.Fatalf("run settled %q error %q states=%v; want failed with %q and no retry", status, runErr, states, tc.reason)
			}
			agent, _ := asWorkflowObject(states["a"])
			if agent == nil || agent.S("status") != "failed" || agent.N("attempt") != 1 || agent.S("retryAt") != "" || agent.S("error") != tc.reason {
				t.Fatalf("agent state = %v; want failed attempt 1 with %q and no retryAt", states["a"], tc.reason)
			}
			var unregistered int
			if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
				t.Fatal(err)
			}
			if unregistered != 0 {
				t.Fatalf("%d work items released by a failure that must not retry", unregistered)
			}
			assertRunWorkspaceReleased(t, store, runID)
		})
	}
}

// D2's closed set is keyed on the session's result.reason, not on the public reason derived from
// its detail. The Controller reports a pinned-plugin startup failure as `agent_failed` with
// `detail=agent_plugin_unavailable`; the decision therefore retries it, and the waiting state keeps
// the raw `agent_failed`. Only the terminal state lifts the detail (the mapping issue-runs share),
// so an exhausted budget still reports the precise public cause. Widening the set to exclude the
// detail is an ADR change, not an implementation detail.
func TestWorkflowRetryDecidesOnTheSessionReasonNotItsDetail(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 1, 0))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	reportPluginFailure := func(execution string) {
		t.Helper()
		if _, err := store.transact(context.Background(), func(tx *transaction) Object {
			executionResult(tx, &ControlRequest{Body: Object{
				"operationId": runID, "executionId": execution, "sequence": int64(1), "event": "ended",
				"result": Object{
					"outcome": "agent_session_ended", "reason": "agent_failed",
					"detail": "agent_plugin_unavailable", "node": Object{"nodeId": nodeService},
				},
			}}, true)
			return Object{}
		}); err != nil {
			t.Fatalf("report session end: %v", err)
		}
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatalf("advance after result: %v", err)
		}
	}
	execution1 := "exec-plugin-detail-1-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution1, spec)
	reportPluginFailure(execution1)
	requireRetrying(t, readWorkflowNodeState(t, store, runID, "a"), 1)

	// The single retry runs and fails the same way; the budget is then spent, and the terminal row
	// carries the public reason lifted from the detail.
	makeWorkflowRetryDue(t, store, runID, "a")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance at due: %v", err)
	}
	spec2 := sessionWorkSpec(t, store, runID, "a")
	execution2 := "exec-plugin-detail-2-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution2, spec2)
	reportPluginFailure(execution2)

	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "agent_plugin_unavailable" {
		t.Fatalf("run settled %q error %q states=%v; want failed/agent_plugin_unavailable", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.N("attempt") != 2 || agent.S("retryAt") != "" ||
		agent.S("error") != "agent_plugin_unavailable" || agent.O("output").S("reason") != "agent_plugin_unavailable" {
		t.Fatalf("agent state = %v; want failed attempt 2 with the lifted reason and no retryAt", states["a"])
	}
	var unregistered int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatal(err)
	}
	if unregistered != 0 {
		t.Fatalf("%d work items released after the budget was spent", unregistered)
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// Cancellation wins over a waiting retry (D5): the node settles cancelled without waiting for the
// committed due instant, the run settles cancelled, the Workspace is released — and even a due
// instant that has already passed starts no attempt. The cancelled node keeps the attempt it failed
// on and carries no retryAt, so no later pass mistakes it for scheduled work.
func TestWorkflowRunCancelWhileRetryingStartsNoAttempt(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowRetryGraph(true, 2, 300))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := "exec-cancel-retry-" + runID[:8]
	dispatchSessionAttempt(t, store, runID, "a", nodeService, execution, spec)
	reportSessionEnd(t, store, runID, execution, nodeService, "agent_failed")
	requireRetrying(t, readWorkflowNodeState(t, store, runID, "a"), 1)

	// The due instant passes before the request is recorded: the request still decides the run.
	makeWorkflowRetryDue(t, store, runID, "a")
	wid := workflowRunWorkflowID(t, store, runID)
	mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-while-retrying", cancelCaller(t, store, tid))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the request: %v", err)
	}

	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "cancelled" || agent.N("attempt") != 1 {
		t.Fatalf("agent state = %v; want cancelled keeping attempt 1", states["a"])
	}
	if agent.S("retryAt") != "" || agent.S("finishedAt") == "" || agent.S("error") != "" || agent.O("output").S("reason") != "cancelled" {
		t.Fatalf("agent state = %v; a cancelled wait is finished, unscheduled and carries no error", agent)
	}
	var unregistered, sessions int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM node_executions WHERE operation_id=$1 AND workflow_node_id='a' AND kind='agent_session'`, runID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if unregistered != 0 || sessions != 1 {
		t.Fatalf("cancelled run holds %d work items and %d sessions; want none and the one that failed", unregistered, sessions)
	}
	// A later pass changes nothing.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after cancellation: %v", err)
	}
	status, _, states, _ = readWorkflowRun(t, store, runID)
	agent, _ = asWorkflowObject(states["a"])
	if status != "cancelled" || agent == nil || agent.S("retryAt") != "" {
		t.Fatalf("a terminal run moved: status=%q agent=%v", status, states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
}
