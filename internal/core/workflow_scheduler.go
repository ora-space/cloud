package core

import (
	"context"
	"time"
)

// The Cloud-side workflow scheduler: the stateless advance pass that moves a WorkflowRun from
// `pending` into real execution and settles it (specs decisions/cloud/workflow
// 0-cloud-side-scheduler-owns-workflow-execution.md, D1 条件②/④, landing order step 3).
//
// Shape. Every tick recomputes the ready set from committed state only — the node states already
// persisted on the run row plus the frozen snapshot graph — so a replica that crashes mid-pass loses
// nothing and a repeated pass is a no-op. Node results commit as a whole-row compare-and-swap on
// `version` (invariant 3: at most one serial executor per run; late or duplicate advancement is a
// no-op), and a node without a committed result is never recorded as succeeded (invariant 5).
//
// Phase-1 boundary. The executable kinds are start, agent, condition and output. start, condition
// and output commit their results inline: they are pure functions of committed state. An agent node
// needs a run Workspace and a real Node session through the control plane, which lands in the next
// step of the same decision (运行身份与绑定, then the execution wiring); until that step is in place a
// ready agent node settles the run failed with an explicit reason. That is the honest terminal state:
// production runs with agent nodes fail fast with a message that names the gap, and never hang in
// `running`, and never report success for work that did not happen. Dev fixtures keep the simulator,
// which fills the trace at create time, so development behavior is unchanged.

// workflowAdvanceBatchSize bounds the retry-loop scan so one tick advances at most this many runs.
// Ordering is deterministic (created_at, id) so every tick drains the oldest first; runs beyond the
// bound advance on the next tick.
const workflowAdvanceBatchSize = 100

// workflowBranchElse is the branch id a Condition node falls back to when no case matched, and the
// meaning of an outgoing edge with no sourceHandle (desktop branch_projection: an unlabeled edge
// defaults to else so graphs that predate source handles still route somewhere safe).
const workflowBranchElse = "else"

// asWorkflowObject accepts a JSON document as either the package's Object or a plain
// map[string]any. Object is a named type, so an Object boxed in `any` does not assert to
// map[string]any — and the scheduler reads documents from two worlds: jsonb-decoded rows produce
// plain maps, while hand-built documents (tests, callers passing literals) carry Object. Asserting
// one shape silently no-matches the other, so every document-level assertion goes through here.
func asWorkflowObject(v any) (Object, bool) {
	switch o := v.(type) {
	case Object:
		return o, true
	case map[string]any:
		return Object(o), true
	default:
		return nil, false
	}
}

// scanAdvancableWorkflowRuns returns the bounded, deterministically ordered ids of runs the pass can
// advance: `pending` (to claim) and `running` (to progress). Terminal and awaiting runs are never
// rescanned. The scan is read-only and runs outside the advisory lock; claim correctness lives in
// each advance transaction's status CAS.
func (s *Store) scanAdvancableWorkflowRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT id FROM workflow_runs
		WHERE status IN ('pending','running') AND deleted_at IS NULL
		ORDER BY created_at, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// AdvanceWorkflowRunsOnce is one bounded retry-loop pass: advance each claimable or in-flight run in
// its own short transaction, with the sleep between ticks owned by the caller. Per-run failures are
// deliberately not surfaced here — the transaction already rolls back and the run is retried on the
// next tick — so the loop must not flood logs because one run is transiently stuck. Only a scan-level
// failure cancels the pass.
//
// The pass carries `DeliveryUnreachableAfter` into each run's advance: it is the deployment's one
// tolerance for a silent Node, and the settlement of an in-flight session whose Node is gone judges
// that same window through the same shared check (runWorkspaceNodeUnknown) the IssueRun release
// paths use. An unconfigured window (zero or negative) fails closed there — a session is held rather
// than failed — so a deployment that never set the limit loses nothing it was promised.
//
// The pass also closes each terminal run's release: the bounded give-up whose Node is gone, and the
// reconciliation that heals or re-declares the Workspace delete. Together with the delete-completion
// hook they leave every terminal run with a durable release fact and never report `released` for a
// Workspace still holding resources.
func (s *Store) AdvanceWorkflowRunsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanAdvancableWorkflowRuns(ctx, workflowAdvanceBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		// The run's exclusive transaction right is the serial executor: a sibling replica that
		// already holds it makes this tick a no-op for this run, and the next tick retries.
		if _, _, err := s.transactWorkflowRun(ctx, id, func(t *transaction) Object {
			advanceWorkflowRun(t, id, s.DeliveryUnreachableAfter)
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	// The release half of the terminal lifecycle: runs the scheduler no longer scans still owe a
	// Workspace release fact. The give-up pass runs first so a run whose Node is gone is abandoned in
	// this tick instead of having one more delete declared for it.
	if err := s.advanceWorkflowDeliveries(ctx); err != nil {
		return err
	}
	if err := s.giveUpWorkflowRunWorkspaceReleases(ctx); err != nil {
		return err
	}
	return s.reconcileWorkflowRunWorkspaceReleases(ctx)
}

// workflowNodeView is one node of the frozen snapshot graph the scheduler executes.
type workflowNodeView struct {
	ID   string
	Kind string
	Data Object
	// Container is the normalized region membership (data.containerId, else parentId). Decode
	// computes it once so every scheduler helper reads the same value.
	Container string
}

// workflowGraphView is the frozen graph reduced to the shapes the scheduler reads.
type workflowGraphView struct {
	Nodes  []workflowNodeView
	Edges  []Object
	ByNode map[string]workflowNodeView
	Start  string // the one start node's id, or "" when the graph has none
}

// readWorkflowGraph reduces a snapshot graph document into its nodes and edges. The document is
// data the scheduler interprets for the first time here, so malformed entries are dropped rather
// than exploding: a graph that reaches a run through a published snapshot was judged at publish,
// and snapshots predating that gate still get an honest run rather than a panic.
func readWorkflowGraph(graph Object) workflowGraphView {
	out := workflowGraphView{ByNode: map[string]workflowNodeView{}}
	// The comma-ok form keeps the promise above: a document whose nodes are not a list at all is an
	// empty graph, not a panic.
	nodes, _ := graph["nodes"].([]any)
	for _, raw := range nodes {
		o, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		id := o.S("id")
		if id == "" {
			continue
		}
		if _, seen := out.ByNode[id]; seen {
			continue
		}
		view := workflowNodeView{ID: id, Kind: o.O("data").S("kind"), Data: o.O("data"), Container: workflowContainerID(o)}
		out.Nodes = append(out.Nodes, view)
		out.ByNode[id] = view
		// A loop body carries its own start. The run's entry point is the outer start only; picking a
		// body start here would make the projection root inside the container and never run the loop.
		if view.Kind == "start" && view.Container == "" && out.Start == "" {
			out.Start = id
		}
	}
	edges, _ := graph["edges"].([]any)
	for _, raw := range edges {
		e, ok := asWorkflowObject(raw)
		if !ok {
			continue
		}
		if e.S("source") == "" || e.S("target") == "" {
			continue
		}
		if _, sourceKnown := out.ByNode[e.S("source")]; !sourceKnown {
			continue
		}
		if _, targetKnown := out.ByNode[e.S("target")]; !targetKnown {
			continue
		}
		out.Edges = append(out.Edges, e)
	}
	return out
}

// workflowGraphCycle reports whether the graph contains a cycle, naming the source of the back edge the
// walk found — a node that is on a cycle by construction — or "" when the graph is a DAG.
//
// It is a depth-first walk with three colors: a node still on the current path that is reached again
// closes a cycle. The walk is deterministic — it roots at nodes in document order and follows edges in
// document order — so the node it names is too. Two cheaper-looking tests are wrong here and are worth
// naming so they are not reintroduced: peeling nodes with no incoming or no outgoing edge leaves the
// remainder including nodes that are merely *between* two cycles, and unwinding the stack at a back
// edge misses nodes on a cycle reached by a shortcut edge.
//
// Both the publish gate and the run's own advance read this one walk, so a graph Cloud refuses to
// publish and a graph Cloud refuses to run are the same graph.
func workflowGraphCycle(graph workflowGraphView) string {
	const (
		unvisited = 0
		onPath    = 1
		settled   = 2
	)
	state := map[string]int{}
	cycleSource := ""
	var walk func(id string)
	walk = func(id string) {
		state[id] = onPath
		for _, edge := range graph.Edges {
			if edge.S("source") != id {
				continue
			}
			next := edge.S("target")
			switch state[next] {
			case onPath:
				// A back edge: `next` is an ancestor on the current path, so the path from `next`
				// back to `id` plus this edge is a cycle. The first one found wins.
				if cycleSource == "" {
					cycleSource = id
				}
			case unvisited:
				walk(next)
			}
		}
		state[id] = settled
	}
	for _, node := range graph.Nodes {
		if state[node.ID] == unvisited {
			walk(node.ID)
		}
	}
	return cycleSource
}

// workflowNodeState is one node's committed execution state on the run row. The shape is the one the
// run viewer already renders (the simulator writes the same keys): status is the shared run/node
// vocabulary, output carries what the node produced, and an inactive state marks a node behind a
// branch that was never taken — it never ran and never will.
type workflowNodeState struct {
	Status      string
	DisplayName string
	StartedAt   string
	FinishedAt  string
	// Attempt is the 1-based attempt this state describes (retry decision D4). It is committed on
	// every agent-node state — running, retrying and terminal — because the scheduler reads the
	// budget and the evidence scope back from it after a restart, a replica change or a replay; the
	// attempt counter is not kept anywhere else. Non-agent nodes have no attempts and leave it zero.
	Attempt int64
	// RetryAt is the earliest instant at which the next attempt may start (retry decision D3). Only
	// a `retrying` node carries it, and it is the database clock's instant, so every pass judges the
	// same wake-up. A terminal or running state has none.
	RetryAt string
	// Error is the node's failure reason, and only a failed node has one: a cancelled node was
	// cancelled, not broken, and an idle or succeeded one has nothing to report. The run viewer
	// renders it in its own Error section, which is why it is a field of the state rather than
	// something the viewer has to dig out of Output — a node's Output is what it produced, and a
	// failed node produced nothing. A `retrying` node keeps the failed attempt's reason: the retry
	// replaces a failure, it does not erase it.
	Error  string
	Output Object
	// SessionEndReason is execution evidence, never a declared variable root.
	SessionEndReason string
	// Loop is the round progress of a loop node and nil for every other kind. The active round's
	// member states stay in the run's flat node_states map (so the session, retry and viewer paths see
	// one shape); completed rounds are archived here for audit and recovery.
	Loop *workflowLoopRunState
	// Iteration is the round progress and ledger of an iteration node, nil for every other kind.
	Iteration *workflowIterationRunState
}

// attemptNumber is the state's 1-based attempt, reading an absent attempt as the first one. Only
// agent nodes carry attempts; a state written before attempts existed, or by a non-agent node,
// belongs to attempt 1.
func (s *workflowNodeState) attemptNumber() int64 {
	if s.Attempt < 1 {
		return 1
	}
	return s.Attempt
}

// object renders the state in the shape the run viewer reads (and the simulator writes). The receiver
// is a pointer because the state is large enough that copying it per render is waste, and its one
// caller already holds it in a range variable.
func (s *workflowNodeState) object() Object {
	out := Object{"status": s.Status, "displayName": s.DisplayName, "startedAt": s.StartedAt, "finishedAt": s.FinishedAt}
	if s.Attempt >= 1 {
		out["attempt"] = s.Attempt
	}
	if s.RetryAt != "" {
		out["retryAt"] = s.RetryAt
	}
	if s.Error != "" {
		out["error"] = s.Error
	}
	if s.Output != nil {
		out["output"] = s.Output
	}
	if s.SessionEndReason != "" {
		out["sessionEndReason"] = s.SessionEndReason
	}
	if s.Loop != nil {
		out["loop"] = s.Loop.object()
	}
	if s.Iteration != nil {
		out["iteration"] = s.Iteration.object()
	}
	return out
}

// workflowStatesFromJSON decodes the committed node_states column. A row the column cannot interpret
// contributes nothing rather than failing the pass: the states are re-derived from whatever is
// readable, and a truncated state only costs a re-execution of the affected node, never a wrong
// success — a node without a committed result is never recorded as succeeded.
func workflowStatesFromJSON(raw Object) map[string]workflowNodeState {
	out := map[string]workflowNodeState{}
	for id, value := range raw {
		node, ok := asWorkflowObject(value)
		if !ok {
			continue
		}
		state := workflowNodeState{
			Status:           node.S("status"),
			DisplayName:      node.S("displayName"),
			StartedAt:        node.S("startedAt"),
			FinishedAt:       node.S("finishedAt"),
			Attempt:          node.N("attempt"),
			RetryAt:          node.S("retryAt"),
			Error:            node.S("error"),
			SessionEndReason: node.S("sessionEndReason"),
		}
		if output, ok := asWorkflowObject(node["output"]); ok {
			state.Output = output
		}
		if loop, ok := asWorkflowObject(node["loop"]); ok {
			state.Loop = workflowLoopRunStateFromJSON(loop)
		}
		if iteration, ok := asWorkflowObject(node["iteration"]); ok {
			state.Iteration = workflowIterationRunStateFromJSON(iteration)
		}
		out[id] = state
	}
	return out
}

// workflowProjection is one advance pass's view of the run: the frozen graph, the node states
// committed before the pass, and the Ready/Inactive resolution derived from them. It is the Go
// counterpart of desktop's BranchProjection (branch_projection.rs), restricted to the phase-1
// node kinds — no regions, no iterations, no loops: a run's graph is a DAG.
type workflowProjection struct {
	graph     workflowGraphView
	committed map[string]workflowNodeState
	projected map[string]string
}

// projectWorkflowNodes resolves the projection to fixed point over the committed states:
// Ready/Inactive propagate downstream through each pass until nothing changes, exactly like
// desktop's resolve_projected_nodes.
func projectWorkflowNodes(graph workflowGraphView, committed map[string]workflowNodeState) *workflowProjection {
	p := &workflowProjection{
		graph:     graph,
		committed: committed,
		projected: map[string]string{},
	}
	for id := range committed {
		state := committed[id]
		p.projected[id] = state.Status
	}
	for {
		changed := false
		for _, node := range graph.Nodes {
			if _, known := p.projected[node.ID]; known {
				continue
			}
			computed, resolved := p.nodeState(node.ID)
			if !resolved {
				continue
			}
			p.projected[node.ID] = computed
			changed = true
		}
		if !changed {
			break
		}
	}
	return p
}

// readyNode returns the next ready node in document order, or ok=false when the projection has no
// executable node left. This is the scheduler's ready set: recomputed from committed state only
// (D1 条件④), never from anything the pass remembers across transactions.
func (p *workflowProjection) readyNode() (workflowNodeView, bool) {
	// An exit owns the next serial slot before any sibling can start an external effect.
	for _, node := range p.graph.Nodes {
		if node.Kind == "loopExit" && p.projected[node.ID] == "ready" {
			return node, true
		}
	}
	for _, node := range p.graph.Nodes {
		if p.projected[node.ID] == "ready" {
			return node, true
		}
	}
	return workflowNodeView{}, false
}

// hasPending reports whether the projection left a node unresolved because an upstream edge is still
// pending. A scope whose nodes are all resolved and none ready has drained: for a loop body that is
// the moment the round is judged complete.
func (p *workflowProjection) hasPending() bool {
	for _, node := range p.graph.Nodes {
		if _, ok := p.projected[node.ID]; !ok {
			return true
		}
	}
	return false
}

// hasFailure reports whether any committed node failed.
func (p *workflowProjection) hasFailure() bool {
	// Only nodes of this projection can decide it. A body member's failure belongs to its container:
	// the container engine either absorbs it (iteration `continue`) or fails itself, and only the
	// container's own state may stop the outer wave. Reading the whole committed map here let an
	// absorbed failure escape its region and fail the run.
	for _, node := range p.graph.Nodes {
		if state, ok := p.committed[node.ID]; ok && state.Status == "failed" {
			return true
		}
	}
	return false
}

// nodeState derives one uncommitted node's projected state from its incoming edges:
// no incoming edges is ready only for the start node (a rootless non-start node never runs); any
// pending edge leaves the node unreached (not resolved); any failed edge fails it; any active edge
// readies it; all-inactive marks it inactive (behind a branch that was not taken).
func (p *workflowProjection) nodeState(nodeID string) (string, bool) {
	incoming := p.incomingEdges(nodeID)
	if len(incoming) == 0 {
		// A graph without a declared start begins at every entry node. Iteration regions have no
		// start member: their entries are the members with no predecessor inside the region, and
		// this rule is what lets a round begin there. An outer graph always declares exactly one
		// start (the gate requires it), so its stray roots stay inactive.
		if nodeID == p.graph.Start || p.graph.Start == "" {
			return "ready", true
		}
		return "inactive", true
	}
	anyActive, anyFailed, anyPending := false, false, false
	for _, edge := range incoming {
		switch p.edgeState(edge) {
		case "pending":
			anyPending = true
		case "failed":
			anyFailed = true
		case "active":
			anyActive = true
		}
	}
	if anyPending {
		return "", false // an upstream node is still unresolved: wait
	}
	if anyFailed {
		return "failed", true
	}
	if anyActive {
		return "ready", true
	}
	return "inactive", true
}

// edgeState resolves one edge from its source node's projected state and, for a Condition, the
// branch it committed. A succeeded non-condition node activates all of its outgoing edges; a
// succeeded Condition activates only the edge whose sourceHandle is the committed branch (an edge
// with no handle is the else edge); a failed source fails the edge; an inactive source stays
// inactive.
func (p *workflowProjection) edgeState(edge Object) string {
	switch p.projected[edge.S("source")] {
	case "succeeded":
		source := p.graph.ByNode[edge.S("source")]
		if source.Kind != "condition" {
			return "active"
		}
		selected := p.selectedBranch(source)
		handle := edge.S("sourceHandle")
		if handle == "" {
			handle = workflowBranchElse
		}
		if handle == selected {
			return "active"
		}
		return "inactive"
	case "failed":
		return "failed"
	case "inactive", "cancelled":
		return "inactive"
	default:
		return "pending"
	}
}

// selectedBranch reads the branch a committed Condition selected from its committed state. A
// condition whose state carries no selection is read as else: the safe default routes downstream
// somewhere rather than dead-ending the branch, and a succeeded condition always committed one.
func (p *workflowProjection) selectedBranch(source workflowNodeView) string {
	if state, ok := p.committed[source.ID]; ok {
		if branch := state.Output.S("selectedBranch"); branch != "" {
			return branch
		}
	}
	return workflowBranchElse
}

// incomingEdges returns the edges whose target is nodeID, in document order.
func (p *workflowProjection) incomingEdges(nodeID string) []Object {
	out := []Object{}
	for _, edge := range p.graph.Edges {
		if edge.S("target") == nodeID {
			out = append(out, edge)
		}
	}
	return out
}

// advanceWorkflowRun claims one pending run and advances it as far as committed state allows, in the
// caller's transaction. It is idempotent: a run that lost a claim race, was advanced concurrently, or
// is already terminal is left untouched. unreachableAfter is the deployment's Node-unreachability
// window, carried through to the session settlement (see AdvanceWorkflowRunsOnce).
func advanceWorkflowRun(t *transaction, runID string, unreachableAfter time.Duration) {
	run := t.one(`
		SELECT r.*, s.graph, (EXTRACT(EPOCH FROM r.created_at)*1000)::bigint AS created_at_ms
		FROM workflow_runs r
		JOIN workflow_snapshots s ON s.id = r.snapshot_id
		WHERE r.id=$1 AND r.deleted_at IS NULL`, runID)
	if run == nil {
		return
	}
	switch run.S("status") {
	case "pending":
		// A request recorded before the run is claimed settles it here: cancel-before-start is the
		// whole cancellation, so the run never stamps started_at, never validates its graph and never
		// provisions a Workspace. The claim below would otherwise turn a cancellation into a start
		// followed by a stop.
		if run["cancelRequestedAt"] != nil {
			finishWorkflowRun(t, runID, run.N("version"), "cancelled", "", nil)
			return
		}
		// The claim is the CAS: zero affected rows means another pass claimed it first.
		if t.execRows(`UPDATE workflow_runs SET status='running', started_at=now(), version=version+1, updated_at=now()
			WHERE id=$1 AND status='pending' AND deleted_at IS NULL`, runID) == 0 {
			return
		}
		projectWorkflowRunToIssueRun(t, run.S("tenantId"), runID, "")
		run = t.one(`
			SELECT r.*, s.graph, (EXTRACT(EPOCH FROM r.created_at)*1000)::bigint AS created_at_ms
			FROM workflow_runs r
			JOIN workflow_snapshots s ON s.id = r.snapshot_id
			WHERE r.id=$1`, runID)
	case "running":
		// Recompute from committed state; a crash between ticks resumes exactly here.
	default:
		return
	}
	// D4: once a request is recorded while the run is non-terminal, the run's terminal status is
	// `cancelled`, and no further node is dispatched. The reaction is read from committed state like
	// every other scheduler decision, so replay, restart and replica passes settle the same way.
	if run["cancelRequestedAt"] != nil {
		advanceCancelledWorkflowRun(t, run, run.N("version"), workflowStatesFromJSON(run.O("nodeStates")), unreachableAfter)
		return
	}
	version := run.N("version")
	graph := readWorkflowGraph(run.O("graph"))
	if graph.Start == "" {
		// A snapshot predating the publish gate can lack a start node; there is no defined
		// place to begin, so the run settles failed instead of hanging in running.
		finishWorkflowRun(t, runID, version, "failed", "graph has no start node", nil)
		return
	}
	// A cyclic graph is not executable: the projection is a topological read of committed state, so a
	// node on a cycle either waits forever for an upstream that waits for it, or — when the cycle is
	// reachable from the start — goes round once and stops with the back edge silently dropped, which
	// would let the run report success for a document whose author asked for a loop. The graph gate
	// refuses to publish one; a snapshot frozen before that gate existed still reaches here, and
	// neither outcome is honest for it. Nothing has run yet at this point, so the run settles failed
	// with no node states — no node is recorded as having executed, which is the truth.
	if workflowGraphCycle(graph) != "" {
		finishWorkflowRun(t, runID, version, "failed", "graph has a cycle", nil)
		return
	}
	states := workflowStatesFromJSON(run.O("nodeStates"))
	for _, node := range graph.Nodes {
		if node.Kind == "loop" && states[node.ID].Status == "running" {
			config, err := parseWorkflowLoopConfig(node.Data)
			if err != nil || !validWorkflowLoopProgress(states[node.ID].Loop, &config, graph, node.ID) {
				failWorkflowLoop(states, node, "loop_invalid_round", "invalid persisted loop progress")
				finishWorkflowRun(t, runID, version, "failed", "loop_invalid_round", states)
				return
			}
		}
		if node.Kind != "iteration" || states[node.ID].Status != "running" {
			continue
		}
		config, err := parseWorkflowIterationConfig(node.Data)
		if err != nil || !validWorkflowIterationProgress(states[node.ID].Iteration, &config, graph, node.ID) {
			failWorkflowIteration(states, node, "iteration_invalid_round", "invalid persisted iteration progress")
			finishWorkflowRun(t, runID, version, "failed", "iteration_invalid_round", states)
			return
		}
	}
	// The ambient variables a condition reads but no node commits: the runtime-owned sys.* pair and
	// the user globals frozen in the snapshot. They are derived from the run row on every pass, so a
	// retry, recovery or re-dispatch reads the same frozen values the run was created with.
	ambient := workflowAmbientStates(run, run.O("graph"))
	// A committed `retrying` node owns the run's serial agent slot (D3): until its committed due
	// instant the pass writes nothing, and once due it starts the next attempt through the same path
	// a fresh node takes. The branch has to sit here — after the cancel branch and before the
	// running-session settlement — and it always returns: `retrying` is neither failed nor ready, so
	// falling through would let the wave loop's "no failure left" rule settle the run succeeded while
	// the node is still waiting (invariant 3's converse).
	if nodeID, retrying := workflowRetryingAgentNode(states); retrying {
		node, known := graph.ByNode[nodeID]
		if !known {
			// The state names a node the frozen graph does not contain. No session can be started for
			// it and no wake-up can ever do anything, so the run fails with Cloud's own explanation
			// rather than waiting forever — the treatment a snapshot with no start node gets.
			state := states[nodeID]
			state.Status = "failed"
			state.RetryAt = ""
			state.FinishedAt = nowRFC3339()
			state.Error = "graph node missing"
			states[nodeID] = state
			finishWorkflowRun(t, runID, version, "failed", "graph has no retrying node", states)
			return
		}
		if !workflowRetryDue(t, runID, nodeID) {
			return
		}
		readStates := states
		if node.Container != "" {
			owner := states[node.Container]
			if owner.Iteration != nil {
				iteration := owner.Iteration
				readStates = workflowIterationReadStates(graph, node.Container, states, ambient, iteration.Items[iteration.Round], iteration.Round)
			} else {
				readStates = workflowLoopReadStates(graph, node.Container, states, ambient)
			}
		}
		startWorkflowAgentSession(t, run, node, version, states, workflowConditionStates(workflowPredecessorStates(graph, node, readStates), ambient))
		return
	}
	// A committed running agent node settles from its session execution's terminal evidence before
	// anything else runs: sessions serialize per run (one unregistered agent_session work item per
	// run), and the next wave waits for the terminal commit. The pass returns after any outcome —
	// a settled node lets the next tick execute its downstream wave.
	if nodeID, running := workflowRunningAgentNode(states); running {
		node, known := graph.ByNode[nodeID]
		if !known {
			// Same corruption as the retrying branch: the node's session was built from this graph, so
			// a missing entry means the snapshot cannot explain its own state. Fail honestly instead of
			// looking for evidence that can never settle.
			state := states[nodeID]
			state.Status = "failed"
			state.FinishedAt = nowRFC3339()
			state.Error = "graph node missing"
			states[nodeID] = state
			finishWorkflowRun(t, runID, version, "failed", "graph has no running node", states)
			return
		}
		if _, cont := workflowAgentSessionOutcome(t, runID, version, node, states, unreachableAfter); !cont {
			return
		}
		// A settled session commits its terminal state and continues the pass; that commit bumped
		// the row's version, so the wave it unblocks must CAS against the new one — a stale version
		// would make the pass's final settle a silent no-op and leave the run in `running`.
		version = workflowRunVersion(t, runID, version)
	}
	outer := workflowOuterGraph(graph)
	for {
		if over := workflowTransitionBudget(t, runID, version, graph, states, "running"); over != nil {
			if !rejectWorkflowStateOverflow(t, runID, version, states, over) {
				return
			}
			finishWorkflowRun(t, runID, version, "failed", over.reason, states)
			return
		}
		projection := projectWorkflowNodes(outer, states)
		// A failed node decides the run. Dispatching another ready node in the same wave would do
		// real work for a run that is already terminal — for an agent that means provisioning a
		// Workspace and waiting on a session — only to settle failed after the session ends. Stopping
		// here lets the run settle in this pass and leaves the unrun siblings without a committed
		// state, which is what "never executed" should look like.
		if projection.hasFailure() {
			break
		}
		if loopNode, running := runningWorkflowLoop(outer, states); running {
			// A running loop is not "ready" in the projection (its state is committed), so the wave
			// drives it explicitly. A started body session ends the pass; other progress continues.
			if !advanceWorkflowLoop(t, run, loopNode, version, states, ambient, graph) {
				return
			}
			continue
		}
		if iterationNode, running := runningWorkflowIteration(outer, states); running {
			// The iteration drives one region step per pass: a started body session ends the pass,
			// otherwise the round advances and the outer wave continues.
			if !advanceWorkflowIteration(t, run, iterationNode, version, states, ambient, graph) {
				return
			}
			continue
		}
		node, ready := projection.readyNode()
		if !ready {
			break
		}
		if node.Kind == "agent" {
			// The session path: provision the Workspace, wait for a connected Node, build the
			// spec from the frozen per-slot bindings, commit running and release the work item.
			// The pass returns either way — a started session commits running, and the next tick
			// settles it from terminal evidence.
			startWorkflowAgentSession(t, run, node, version, states, workflowConditionStates(workflowPredecessorStates(graph, node, states), ambient))
			return
		}
		if node.Kind == "loop" {
			// Drive the container one step. A loop starts its body agent through the same session
			// path, so a started session ends the pass; otherwise the loop's state is updated in
			// this pass and the outer wave continues.
			if !advanceWorkflowLoop(t, run, node, version, states, ambient, graph) {
				return
			}
			continue
		}
		if node.Kind == "iteration" {
			// Same handshake as the loop: one region step per pass, and a started body session
			// ends it so the next tick settles the session.
			if !advanceWorkflowIteration(t, run, node, version, states, ambient, graph) {
				return
			}
			continue
		}
		states[node.ID] = executeWorkflowNode(node, run.O("input"), states, ambient)
	}
	// The wave stopped with no executable node left. Persist the projection's inactive nodes —
	// behind a branch that was not taken, they never ran and never will — so the run row alone
	// tells the viewer the full story (a missing state renders as idle, which would understate a
	// branch the run already resolved). Then settle: failed when any committed node failed,
	// succeeded otherwise; the failure check reads committed state, never the pass's own reasoning.
	states = workflowInactiveStates(outer, states)
	if projectWorkflowNodes(outer, states).hasFailure() {
		finishWorkflowRun(t, runID, version, "failed", workflowRunFailureReason(outer, states), states)
		return
	}
	if workflowAnyCancelled(states) {
		finishWorkflowRun(t, runID, version, "cancelled", "", states)
		return
	}
	finishWorkflowRun(t, runID, version, "succeeded", "", states)
}

// workflowInactiveStates adds a committed inactive state for every node the projection resolved as
// inactive but that carries no committed state. Behind a branch that was not taken, such a node
// never ran and never will; persisting it keeps the run row alone telling the viewer the full story.
func workflowInactiveStates(graph workflowGraphView, states map[string]workflowNodeState) map[string]workflowNodeState {
	p := projectWorkflowNodes(graph, states)
	out := states
	for _, node := range graph.Nodes {
		if p.projected[node.ID] == "inactive" {
			if _, committed := states[node.ID]; !committed {
				out[node.ID] = workflowNodeState{Status: "inactive", DisplayName: workflowNodeDisplayName(node)}
			}
		}
	}
	return out
}

// settleWorkflowRunStates persists the pass's results behind the run's version CAS — the
// compare-and-swap that makes a late or duplicate pass a no-op (invariant 3). Zero affected rows
// (a concurrent pass committed first) discards this pass's local view; the next tick recomputes
// from the committed row.
//
// finished_at is the run's terminal timestamp, so only a terminal status writes it. This function is
// also how a pass reports progress — committing a node `running`, or settling one and letting the
// next wave start — and those passes used to stamp finished_at too, so a run that was still
// executing carried the moment it "finished". The projection is what a client reads: `finishedAt`
// non-empty is how it renders a run as over, so the field has to mean what it says. COALESCE keeps
// the first terminal write, so a later pass re-declaring a terminal status cannot move the moment
// the run actually stopped. started_at is not this function's business: it is set once, where the
// run first leaves `pending`.
func settleWorkflowRunStates(t *transaction, runID string, version int64, status, runErr string, states map[string]workflowNodeState) bool {
	budgetFailed := false
	if states != nil {
		snapshot := t.one(`SELECT s.graph FROM workflow_runs r JOIN workflow_snapshots s ON s.id=r.snapshot_id WHERE r.id=$1`, runID)
		if over := workflowTransitionBudget(t, runID, version, readWorkflowGraph(snapshot.O("graph")), states, status); over != nil {
			if !rejectWorkflowStateOverflow(t, runID, version, states, over) {
				return false
			}
			status, runErr, budgetFailed = "failed", over.reason, true
		}
	}
	// The statuses a run never leaves. `pending`, `running` and `awaiting_input` are the others the
	// column admits, and all three are states a run is still in.
	terminal := status == "succeeded" || status == "failed" || status == "cancelled"
	var applied int64
	encoded := Object{}
	if states == nil {
		applied = t.execRows(`UPDATE workflow_runs SET status=$2, error=$3,
			finished_at=CASE WHEN $5 THEN COALESCE(finished_at,now()) ELSE finished_at END,
			version=version+1, updated_at=now()
			WHERE id=$1 AND version=$4 AND deleted_at IS NULL`, runID, status, runErr, version, terminal)
	} else {
		for id := range states {
			state := states[id]
			encoded[id] = state.object()
		}
		applied = t.execRows(`UPDATE workflow_runs SET node_states=$2, status=$3, error=$4,
			finished_at=CASE WHEN $6 THEN COALESCE(finished_at,now()) ELSE finished_at END,
			version=version+1, updated_at=now()
			WHERE id=$1 AND version=$5 AND deleted_at IS NULL`, runID, jsonText(encoded), status, runErr, version, terminal)
	}
	if applied == 0 {
		return false
	}
	run := t.one(`SELECT r.tenant_id,s.graph FROM workflow_runs r
		JOIN workflow_snapshots s ON s.id=r.snapshot_id WHERE r.id=$1`, runID)
	projectWorkflowRunToIssueRun(t, run.S("tenantId"), runID, workflowIssueOutputSummary(run.O("graph"), encoded, runErr))
	if budgetFailed {
		finishWorkflowRunCleanup(t, runID)
		return false
	}
	return true
}

// workflowRunVersion re-reads the row's version after a mid-pass settle bumped it: one pass commits
// more than once (a settled session, then the wave it unblocks), and every later commit CASes
// against the version the earlier one left behind. A row that vanished keeps the caller's version,
// so the following settle fails its CAS and the pass writes nothing.
func workflowRunVersion(t *transaction, runID string, fallback int64) int64 {
	if row := t.one("SELECT version FROM workflow_runs WHERE id=$1", runID); row != nil {
		return row.N("version")
	}
	return fallback
}

// executeWorkflowNode commits one ready non-agent node's result from committed state. start reports
// the variable roots desktop's Start node exposes; condition evaluates its cases against prior node
// outputs overlaid with the run's ambient variables (sys.* and frozen user globals) and commits the
// selected branch (first match wins, else when nothing matched, and a failure when a rule cannot be
// evaluated); output resolves explicit named bindings into its committed result. No bindings retain
// an empty result; an unassigned binding fails instead of silently emitting a partial object.
func executeWorkflowNode(node workflowNodeView, input Object, states, ambient map[string]workflowNodeState) workflowNodeState {
	now := nowRFC3339()
	state := workflowNodeState{
		Status:      "succeeded",
		DisplayName: workflowNodeDisplayName(node),
		StartedAt:   now,
		FinishedAt:  now,
	}
	switch node.Kind {
	case "start":
		state.Output = workflowStartOutput(node, input)
	case "condition":
		branch, err := evaluateWorkflowCondition(node, workflowConditionStates(states, ambient))
		if err != nil {
			// A condition that cannot evaluate the document it was given fails the node and the run,
			// exactly as desktop's ConditionRuntime does. Falling through to else would hide a
			// malformed or unassigned-variable reference behind a branch the author never chose.
			state.Status = "failed"
			state.Error = err.Error()
			state.Output = Object{"reason": err.Error()}
			return state
		}
		state.Output = Object{"selectedBranch": branch}
	case "output":
		output, failure := evaluateWorkflowOutput(node, workflowConditionStates(states, ambient))
		if failure != nil {
			state.Status = "failed"
			state.Error = failure.Error()
			state.Output = Object{"reason": failure.Reason}
			return state
		}
		state.Output = output
	case "aggregator":
		value, assigned, err := evaluateWorkflowAggregator(node, workflowConditionStates(states, ambient))
		if err != nil {
			state.Status = "failed"
			state.Error = err.Error()
			state.Output = Object{"reason": "aggregator_invalid_config"}
			return state
		}
		if !assigned {
			// Every candidate was declared but none was produced: the stable desktop failure, not an
			// empty output. Publishing nothing would make a branch merge silently disappear.
			state.Status = "failed"
			state.Error = "variable aggregator found no assigned candidate"
			state.Output = Object{"reason": "aggregator_no_match"}
			return state
		}
		state.Output = Object{"output": value}
	case "loopExit":
		// The early-exit marker carries no configuration and produces nothing; the container engine
		// reads its success to skip feedback and publish the round's outputs.
		state.Output = Object{}
	default:
		// Unreachable: the ready pass only yields executable kinds, and the agent case returned
		// earlier. A future kind that reaches here must not be recorded as success.
		state.Status = "failed"
		state.Error = "unsupported_node_kind"
		state.Output = Object{"reason": "unsupported_node_kind"}
	}
	return state
}

// workflowAmbientStates builds the pseudo-node states a condition reads but no graph node commits:
// the runtime-owned sys.* pair and the user globals frozen in the snapshot. The keys are writers (a
// global's first name segment, or "sys"), so a selector addresses them exactly as it addresses a node.
// sys.workflow_id is the run's workflow id and sys.timestamp is the run's creation time in epoch
// milliseconds, both read from the frozen run row so every pass — retry, recovery, re-dispatch —
// agrees. User-authored sys.* declarations are skipped rather than merged: the runtime owns those two
// names, which is desktop's retain-and-repush behavior.
func workflowAmbientStates(run, graph Object) map[string]workflowNodeState {
	ambient := map[string]workflowNodeState{}
	for _, variable := range workflowGlobalVariables(graph) {
		name := variable.S("name")
		if name == "sys.workflow_id" || name == "sys.timestamp" {
			continue
		}
		writer, root, ok := workflowGlobalSelector(name)
		if !ok {
			continue
		}
		value, present := variable["value"]
		if !present || value == nil {
			continue
		}
		setWorkflowAmbientRoot(ambient, writer, root, value)
	}
	ambient["sys"] = workflowNodeState{
		Status: "succeeded",
		Output: Object{
			"workflow_id": run.S("workflowId"),
			"timestamp":   float64(run.N("createdAtMs")),
		},
	}
	return ambient
}

// setWorkflowAmbientRoot records one global's value under its writer's pseudo-node output, creating
// the pseudo-node on first use. Two globals in one namespace share the writer and union their roots.
func setWorkflowAmbientRoot(ambient map[string]workflowNodeState, writer, root string, value any) {
	state, ok := ambient[writer]
	if !ok {
		state = workflowNodeState{Status: "succeeded", Output: Object{}}
	}
	state.Output[root] = value
	ambient[writer] = state
}

// workflowConditionStates returns the states a condition evaluates against: the committed node states
// overlaid with the run's ambient variables. The overlay is per-root rather than per-node because a
// global's writer is a namespace, not a node — a graph may declare a global `start.foo` beside a real
// start node, and desktop's flat pool keeps `start.foo` (global) and `start.input` (node) as distinct
// keys. Committed roots win on a collision, the same way desktop's node pass overwrites a same-named
// global value. The result is a fresh map so the ambient pseudo-nodes are never persisted into
// node_states.
func workflowConditionStates(states, ambient map[string]workflowNodeState) map[string]workflowNodeState {
	if len(ambient) == 0 {
		return states
	}
	out := make(map[string]workflowNodeState, len(states)+len(ambient))
	for id := range ambient {
		out[id] = ambient[id]
	}
	for id := range states {
		state := states[id]
		if existing, ok := out[id]; ok {
			existing.Output = mergeWorkflowOutput(existing.Output, state.Output)
			out[id] = existing
			continue
		}
		out[id] = state
	}
	return out
}

// mergeWorkflowOutput merges an ambient pseudo-node's output with a committed node's, letting the
// committed roots win. The ambient output is nil when no global shares the node's id.
func mergeWorkflowOutput(ambient, committed Object) Object {
	merged := make(Object, len(ambient)+len(committed))
	for key, value := range ambient {
		merged[key] = value
	}
	for key, value := range committed {
		merged[key] = value
	}
	return merged
}

// workflowStartOutput is the Start node's variable pool in the shape desktop declares it: `input` is
// the kickoff prompt (the reserved free-text selector), and every declared input variable is a root
// named by the variable itself. Before circus#210 Cloud nested the whole run input under `input`, so
// `[start, name]` resolved to nothing and `[start, input]` answered for the entire form — the same
// document meant different things on the two sides.
//
// The run form's values are keyed by variable name, so flattening them is exact. The `input` key is
// not flattened with the rest: it is the reserved kickoff selector, and letting a user variable
// shadow it would make the same selector mean a run value on Cloud and a prompt on desktop. Its value
// comes from WorkflowStartKickoff instead.
func workflowStartOutput(node workflowNodeView, input Object) Object {
	output := make(Object, len(input)+1)
	for key, value := range input {
		if key == "input" {
			continue
		}
		output[key] = value
	}
	if kickoff, ok := WorkflowStartKickoff(node.Data, input); ok {
		output["input"] = kickoff
	}
	return output
}

// WorkflowStartKickoff resolves the Start node's kickoff text and reports whether it exists. The
// run's own submitted `input` wins; the authored Start instruction is the fallback.
//
// The two sources exist because runs outlive the code that created them. A run created before Cloud
// persisted the per-run kickoff has no reserved `input` key in its own input object — the prompt it
// executed then lived in the frozen snapshot's Start node, not the run row — so falling back to that
// node's `data.input` keeps historical runs evaluating exactly as they did (circus#287). The fallback
// reads the snapshot's node, never the live workflow document. Cloud's canonical document stores the
// prompt under `data.input` only; the legacy `data.instruction` desktop also reads is migrated to
// `input` by the frontend when a document is read, so the server never has to fall back to it.
//
// The text is used verbatim, whitespace included: desktop's start.input is the kickoff prompt as the
// author or the run submitted it, and trimming here would make the same selector compare differently
// on the two sides. When neither source has a value the root is left out of the pool rather than set
// to "": desktop leaves {start}.input unset when the run supplied no kickoff and the graph has no
// instruction, and exists/not_exists must answer the same way on both sides.
//
// It is exported for the development simulator, which writes the Start output outside this package;
// sharing the resolver keeps the simulated trace from drifting from what the scheduler commits.
func WorkflowStartKickoff(data, input Object) (string, bool) {
	if v, ok := input["input"]; ok && v != nil {
		if text, ok := v.(string); ok {
			return text, true
		}
	}
	if v, ok := data["input"]; ok && v != nil {
		if text, ok := v.(string); ok {
			return text, true
		}
	}
	return "", false
}

// workflowNodeDisplayName is the label the run viewer shows: the author's title, or the node kind
// when the title is empty — the same projection the simulator writes.
func workflowNodeDisplayName(node workflowNodeView) string {
	if title := node.Data.S("title"); title != "" {
		return title
	}
	return node.Kind
}

// workflowRunFailureReason names the first committed failure in node order, so the run's error
// points at the node that actually failed rather than at a downstream casualty. The order has to
// come from the graph: the states are a map, Go randomizes map iteration, and reading the reason
// out of it would let the same trace record a different reason on every replay. Every committed
// state belongs to a graph node — the scheduler writes them from graph nodes, and the graph is
// frozen at creation — so walking the graph reaches them all.
func workflowRunFailureReason(graph workflowGraphView, states map[string]workflowNodeState) string {
	for _, node := range graph.Nodes {
		state := states[node.ID]
		if state.Status != "failed" {
			continue
		}
		if node.Kind == "agent" && state.Error != "" {
			return state.Error
		}
		if reason := state.Output.S("reason"); reason != "" {
			return reason
		}
		return "node failed"
	}
	return "run failed"
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
