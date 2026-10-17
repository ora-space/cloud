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
func (s *Store) AdvanceWorkflowRunsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanAdvancableWorkflowRuns(ctx, workflowAdvanceBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.transact(ctx, func(t *transaction) Object {
			advanceWorkflowRun(t, id)
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	return s.redeclareWorkflowRunWorkspaceDeletes(ctx)
}

// redeclareWorkflowRunWorkspaceDeletes re-declares the Workspace deletion of every terminal run
// still holding a live Workspace. finishWorkflowRunCleanup declares delete in the pass that
// settles the run; a busy Project (another operation in flight) returns busy, so the declaration
// waits for a later pass — this second scan is the retry. Idempotent: deleteWorkflowRunWorkspace
// treats an already-declared or already-deleted Workspace as done.
func (s *Store) redeclareWorkflowRunWorkspaceDeletes(ctx context.Context) error {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT r.id FROM workflow_runs r
		JOIN workspaces w ON w.workflow_run_id=r.id
		WHERE r.status IN ('failed','cancelled','succeeded') AND r.deleted_at IS NULL
		  AND w.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM operations o WHERE o.workspace_id=w.id AND o.kind='delete_workspace'
		   AND o.state IN ('queued','running','retry_wait','blocked','succeeded'))
		ORDER BY r.created_at, r.id LIMIT $1`, workflowAdvanceBatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.transact(ctx, func(t *transaction) Object {
			finishWorkflowRunCleanup(t, id)
			return Object{}
		}); err != nil {
			_ = err
		}
	}
	return nil
}

// workflowNodeView is one node of the frozen snapshot graph the scheduler executes.
type workflowNodeView struct {
	ID   string
	Kind string
	Data Object
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
		view := workflowNodeView{ID: id, Kind: o.O("data").S("kind"), Data: o.O("data")}
		out.Nodes = append(out.Nodes, view)
		out.ByNode[id] = view
		if view.Kind == "start" && out.Start == "" {
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
	// Error is the node's failure reason, and only a failed node has one: a cancelled node was
	// cancelled, not broken, and an idle or succeeded one has nothing to report. The run viewer
	// renders it in its own Error section, which is why it is a field of the state rather than
	// something the viewer has to dig out of Output — a node's Output is what it produced, and a
	// failed node produced nothing.
	Error  string
	Output Object
}

// object renders the state in the shape the run viewer reads (and the simulator writes). The receiver
// is a pointer because the state is large enough that copying it per render is waste, and its one
// caller already holds it in a range variable.
func (s *workflowNodeState) object() Object {
	out := Object{"status": s.Status, "displayName": s.DisplayName, "startedAt": s.StartedAt, "finishedAt": s.FinishedAt}
	if s.Error != "" {
		out["error"] = s.Error
	}
	if s.Output != nil {
		out["output"] = s.Output
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
			Status:      node.S("status"),
			DisplayName: node.S("displayName"),
			StartedAt:   node.S("startedAt"),
			FinishedAt:  node.S("finishedAt"),
			Error:       node.S("error"),
		}
		if output, ok := asWorkflowObject(node["output"]); ok {
			state.Output = output
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
	for id, state := range committed {
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
	for _, node := range p.graph.Nodes {
		if p.projected[node.ID] == "ready" {
			return node, true
		}
	}
	return workflowNodeView{}, false
}

// hasFailure reports whether any committed node failed.
func (p *workflowProjection) hasFailure() bool {
	for _, state := range p.committed {
		if state.Status == "failed" {
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
		if nodeID == p.graph.Start {
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
// is already terminal is left untouched.
func advanceWorkflowRun(t *transaction, runID string) {
	run := t.one(`
		SELECT r.*, s.graph FROM workflow_runs r
		JOIN workflow_snapshots s ON s.id = r.snapshot_id
		WHERE r.id=$1 AND r.deleted_at IS NULL`, runID)
	if run == nil {
		return
	}
	switch run.S("status") {
	case "pending":
		// The claim is the CAS: zero affected rows means another pass claimed it first.
		if t.execRows(`UPDATE workflow_runs SET status='running', started_at=now(), version=version+1, updated_at=now()
			WHERE id=$1 AND status='pending' AND deleted_at IS NULL`, runID) == 0 {
			return
		}
		run = t.one(`
			SELECT r.*, s.graph FROM workflow_runs r
			JOIN workflow_snapshots s ON s.id = r.snapshot_id
			WHERE r.id=$1`, runID)
	case "running":
		// Recompute from committed state; a crash between ticks resumes exactly here.
	default:
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
	// A committed running agent node settles from its session execution's terminal evidence before
	// anything else runs: sessions serialize per run (one unregistered agent_session work item per
	// run), and the next wave waits for the terminal commit. The pass returns after any outcome —
	// a settled node lets the next tick execute its downstream wave.
	if nodeID, running := workflowRunningAgentNode(states); running {
		if _, cont := workflowAgentSessionOutcome(t, runID, version, nodeID, states); !cont {
			return
		}
		// A settled session commits its terminal state and continues the pass; that commit bumped
		// the row's version, so the wave it unblocks must CAS against the new one — a stale version
		// would make the pass's final settle a silent no-op and leave the run in `running`.
		version = workflowRunVersion(t, runID, version)
	}
	for {
		projection := projectWorkflowNodes(graph, states)
		node, ready := projection.readyNode()
		if !ready {
			break
		}
		if node.Kind == "agent" {
			// The session path: provision the Workspace, wait for a connected Node, build the
			// spec from the frozen per-slot bindings, commit running and release the work item.
			// The pass returns either way — a started session commits running, and the next tick
			// settles it from terminal evidence.
			startWorkflowAgentSession(t, run, node, version, states)
			return
		}
		states[node.ID] = executeWorkflowNode(node, run.O("input"), states)
	}
	// The wave stopped with no executable node left. Persist the projection's inactive nodes —
	// behind a branch that was not taken, they never ran and never will — so the run row alone
	// tells the viewer the full story (a missing state renders as idle, which would understate a
	// branch the run already resolved). Then settle: failed when any committed node failed,
	// succeeded otherwise; the failure check reads committed state, never the pass's own reasoning.
	states = workflowInactiveStates(graph, states)
	if projectWorkflowNodes(graph, states).hasFailure() {
		finishWorkflowRun(t, runID, version, "failed", workflowRunFailureReason(graph, states), states)
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
func settleWorkflowRunStates(t *transaction, runID string, version int64, status, runErr string, states map[string]workflowNodeState) {
	// The statuses a run never leaves. `pending`, `running` and `awaiting_input` are the others the
	// column admits, and all three are states a run is still in.
	terminal := status == "succeeded" || status == "failed" || status == "cancelled"
	if states == nil {
		t.exec(`UPDATE workflow_runs SET status=$2, error=$3,
			finished_at=CASE WHEN $5 THEN COALESCE(finished_at,now()) ELSE finished_at END,
			version=version+1, updated_at=now()
			WHERE id=$1 AND version=$4 AND deleted_at IS NULL`, runID, status, runErr, version, terminal)
		return
	}
	encoded := Object{}
	for id, state := range states {
		encoded[id] = state.object()
	}
	t.exec(`UPDATE workflow_runs SET node_states=$2, status=$3, error=$4,
		finished_at=CASE WHEN $6 THEN COALESCE(finished_at,now()) ELSE finished_at END,
		version=version+1, updated_at=now()
		WHERE id=$1 AND version=$5 AND deleted_at IS NULL`, runID, jsonText(encoded), status, runErr, version, terminal)
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
// what the run was kicked off with; condition evaluates its cases against prior node outputs and
// commits the selected branch (first match wins, else when nothing matched); output is a terminal
// marker whose produce stays in the node state for the run viewer (D7: 成果交付延后).
func executeWorkflowNode(node workflowNodeView, input Object, states map[string]workflowNodeState) workflowNodeState {
	now := nowRFC3339()
	state := workflowNodeState{
		Status:      "succeeded",
		DisplayName: workflowNodeDisplayName(node),
		StartedAt:   now,
		FinishedAt:  now,
	}
	switch node.Kind {
	case "start":
		state.Output = Object{"input": input}
	case "condition":
		state.Output = Object{"selectedBranch": evaluateWorkflowCondition(node, states)}
	case "output":
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
