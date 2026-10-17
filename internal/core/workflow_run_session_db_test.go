package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// workflowSessionGraph is a linear start -> agent -> output graph; the agent carries the prompt
// and the implicit default slot.
func workflowSessionGraph() Object {
	g := wfGraph(
		[]Object{wfNode("s", "start", ""), wfAgentNode("a", Object{"prompt": "Do the thing"}), wfNode("o", "output", "")},
		[][3]string{{"s", "a", ""}, {"a", "o", ""}},
	)
	return g
}

// workflowTwoAgentGraph is start -> agent a1 -> agent a2 -> output: one run with two agent nodes,
// which D4 executes serially through the run's single Workspace.
func workflowTwoAgentGraph() Object {
	return wfGraph(
		[]Object{
			wfNode("s", "start", ""),
			wfAgentNode("a1", Object{"prompt": "First"}),
			wfAgentNode("a2", Object{"prompt": "Second"}),
			wfNode("o", "output", ""),
		},
		[][3]string{{"s", "a1", ""}, {"a1", "a2", ""}, {"a2", "o", ""}},
	)
}

// seedWorkflowSessionScene stages a project-backed run one advance away from session start: the
// binding and the plugin version (the same freezes the creation transaction performs), and the
// project space carries the production plugin. Returns the ids the tests read back.
func seedWorkflowSessionScene(t *testing.T, store *Store) (runID, tid, uid, space, project string) {
	t.Helper()
	return seedWorkflowSessionSceneGraph(t, store, workflowSessionGraph())
}

// seedWorkflowSessionSceneGraph is seedWorkflowSessionScene for a caller-supplied graph: the
// snapshot, the live document and the frozen bindings all carry the same document, the way
// publishing freezes a version.
func seedWorkflowSessionSceneGraph(t *testing.T, store *Store, graph Object) (runID, tid, uid, space, project string) {
	t.Helper()
	_, uid = modelUser(t, store, "wf-session")
	tid, wid, sid, rid := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	space, project = uuid.NewString(), uuid.NewString()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		exec := func(name, q string, args ...any) {
			tx.exec(q, args...)
		}
		exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'wfs','active')`, tid)
		exec("memb", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tid, uid)
		exec("space", `INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'wfs','wfs-'||left($4::text,8),$3)`, space, tid, uid, project)
		exec("project", `WITH p AS (INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle) VALUES($1,$2,$3,$4,'wfs','active') RETURNING id,tenant_id) INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch) SELECT gen_random_uuid(),tenant_id,id,'https://example.invalid/wfs.git','main' FROM p`, project, tid, uid, space)
		exec("main", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), tid, uid, project)
		exec("plugin", `INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,observed_state) VALUES($1,$2,'official','ora-space.opencode','installed','1.0.0','installed')`, space, tid)
		graphJSON, _ := json.Marshal(graph)
		exec("workflow", `INSERT INTO workflows(id,tenant_id,name,graph) VALUES($1,$2,'wfs',$3::jsonb)`, wid, tid, string(graphJSON))
		exec("snapshot", `INSERT INTO workflow_snapshots(id,tenant_id,workflow_id,version,name,graph) VALUES($1,$2,$3,1,'v1',$4::jsonb)`, sid, tid, wid, string(graphJSON))
		exec("run", `INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,input,created_by,project_id,plugin_version) VALUES($1,$2,$3,$4,'disp','pending','{}'::jsonb,$5,$6,$7)`, rid, tid, wid, sid, uid, project, resolveWorkflowRunPluginVersion(tx, tid, project))
		cred := uuid.NewString()
		conn := uuid.NewString()
		exec("credential", `INSERT INTO personal_model_credentials(id,user_id,ciphertext,key_id) VALUES($1,$2,repeat('c',64),'test-key')`, cred, uid)
		exec("connection", `INSERT INTO personal_model_connections(id,user_id,credential_id,name,protocol,base_url,auth_mode,models,enabled) VALUES($1,$2,$3,'Personal Provider','openai-completions','https://models.example.invalid/v1','bearer',$4::jsonb,true)`, conn, uid, cred, jsonText([]any{map[string]any{"id": "vendor/model-v1", "name": "Model V1"}}))
		exec("default", `INSERT INTO personal_model_defaults(user_id,connection_id,model_id,version) VALUES($1,$2,'vendor/model-v1',1)`, uid, conn)
		// A named slot too, so a graph that references one freezes a second binding on the same run.
		// 0039 keys a workflow binding by (run, slot) rather than by run alone, which is what makes
		// "belongs to this run" too weak a test for a session to redeem one (liveWorkflowModelBinding).
		exec("slot", `INSERT INTO executor_slot_mappings(user_id,slot_name,agent_cli,connection_id,model_id,version) VALUES($1,'strong','official/ora-space.opencode',$2,'vendor/model-v1',1)`, uid, conn)
		freezeWorkflowRunExecutors(tx, rid, uid, graph)
		return Object{}
	}); err != nil {
		t.Fatalf("seed scene: %v", err)
	}
	return rid, tid, uid, space, project
}

// settleSessionWorkspace completes the run Workspace's create operation the way the control plane
// does — the operation settles, admission opens with a recorded baseline and checkout — and returns
// the Workspace id.
func settleSessionWorkspace(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var wsID, createOp string
	if err := store.Pool.QueryRow(`SELECT w.id, o.id FROM workspaces w JOIN operations o ON o.workspace_id=w.id AND o.kind='create_workspace' WHERE w.workflow_run_id=$1`, runID).Scan(&wsID, &createOp); err != nil {
		t.Fatalf("read provisioned workspace: %v", err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		settleCreateWorkspaceOperation(t, tx, createOp)
		runWorkspaceSettled(tx, runID, "ready")
		tx.exec(`UPDATE workspaces SET observed_state='ready', admission_open=true, base_commit_id=$2, version=version+1 WHERE id=$1`, wsID, baseCommit)
		tx.exec(`INSERT INTO clone_executions(execution_id,operation_id,node_id,node_operation_id,workspace_id,input,result,dispatched_epoch) VALUES($1,$2,'node-checkout','op-checkout',$3,'{"kind":"clone"}',jsonb_build_object('outcome','clone_ready','commit',$4::text),1)`, "exec-checkout-"+wsID[:8], createOp, wsID, baseCommit)
		return Object{}
	}); err != nil {
		t.Fatalf("settle create: %v", err)
	}
	return wsID
}

// startSessionNode connects a Node to the settled Workspace, advances until the agent node is
// committed running, and returns the Node's service id and the released session spec. graphNode
// names which agent node of the graph the pass is expected to start.
func startSessionNode(t *testing.T, store *Store, runID, wsID, graphNode string) (string, Object) {
	t.Helper()
	_, nodeService := bindConnectedNode(t, store, wsID)
	if _, err := store.Pool.Exec(`UPDATE node_instances SET model_proxy=true WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to session start: %v", err)
	}
	status, _, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states[graphNode])
	if status != "running" || agent == nil || agent.S("status") != "running" {
		t.Fatalf("after session start: %q states=%v; want agent %s running", status, states, graphNode)
	}
	spec := sessionWorkSpec(t, store, runID, graphNode)
	if spec.S("agentPluginId") != opencodeAgentCLI || spec.S("agentPluginVersion") != "1.0.0" || spec.S("modelBindingId") == "" {
		t.Fatalf("spec = %v", spec)
	}
	if spec.O("initialTurn").S("turnId") == "" || spec.O("initialTurn")["content"] == nil || spec.S("checkoutExecutionId") != "exec-checkout-"+wsID[:8] {
		t.Fatalf("spec turn/checkout = %v", spec)
	}
	if git := spec.O("gitIdentity"); git.S("name") == "" || git.S("email") == "" {
		t.Fatalf("git identity = %v", git)
	}
	return nodeService, spec
}

// sessionWorkSpec reads back the unregistered session work item a graph node's start released.
func sessionWorkSpec(t *testing.T, store *Store, runID, graphNode string) Object {
	t.Helper()
	var rawSpec []byte
	if err := store.Pool.QueryRow(`SELECT input FROM execution_work
		WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND execution_id IS NULL`, runID, graphNode).Scan(&rawSpec); err != nil {
		t.Fatalf("read session work for node %s: %v", graphNode, err)
	}
	spec := Object{}
	if err := json.Unmarshal(rawSpec, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// dispatchSession dispatches the released session work from the Node and advances: the pass holds
// the committed running node until terminal evidence arrives.
func dispatchSession(t *testing.T, store *Store, runID, graphNode, nodeService string, spec Object) string {
	t.Helper()
	execution := "exec-" + graphNode + "-" + runID[:8]
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "nodeId": nodeService, "input": spec, "epoch": int64(1)}})
		return Object{}
	}); err != nil {
		t.Fatalf("dispatch node %s: %v", graphNode, err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after dispatch: %v", err)
	}
	_, _, held, _ := readWorkflowRun(t, store, runID)
	h, _ := asWorkflowObject(held[graphNode])
	if h == nil || h.S("status") != "running" {
		t.Fatalf("dispatched session held: %v", held)
	}
	return execution
}

// reportSessionEnd reports the session's terminal event from the Node and runs the settling pass.
func reportSessionEnd(t *testing.T, store *Store, runID, execution, nodeService, reason string) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		executionResult(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "result": Object{"outcome": "agent_session_ended", "reason": reason, "node": Object{"nodeId": nodeService}}, "sequence": int64(1), "event": "ended"}}, true)
		return Object{}
	}); err != nil {
		t.Fatalf("report result: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after result: %v", err)
	}
}

// The wired end to end: one advance provisions the run Workspace, the create settles, a
// connected Node releases the session work item, the Node dispatches and reports, and the run
// settles succeeded with its Workspace deletion declared.
func TestWorkflowSessionRunsEndToEnd(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	status, _, states, _ := readWorkflowRun(t, store, runID)
	if status != "running" || len(states) != 0 {
		t.Fatalf("after advance 1: status=%q states=%v; want running with no node states", status, states)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	// No connected Node yet: the pass holds without committing a node state.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if _, _, held, _ := readWorkflowRun(t, store, runID); len(held) != 0 {
		t.Fatalf("advance 2 committed states %v without a connected node", held)
	}
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	// The Node reports a natural end; the run drains to succeeded and cleans up.
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	var runErr string
	status, runErr, states, _ = readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want succeeded", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	outs, _ := asWorkflowObject(states["o"])
	if agent == nil || outs == nil || agent.S("status") != "succeeded" || outs.S("status") != "succeeded" {
		t.Fatalf("node states = %v", states)
	}
	if reason := agent.O("output").S("reason"); reason != "idle_timeout" {
		t.Fatalf("agent output reason = %q; want idle_timeout", reason)
	}
	// Terminal cleanup: no unregistered work, and the Workspace deletion is declared.
	assertRunWorkspaceReleased(t, store, runID)
}

// A run's agent nodes execute serially and settle from their own evidence. The second node starts on
// the pass that settles the first, so it must be committed running and carry no terminal state: a
// run-scoped evidence lookup (the newest agent_session result for the run) hands it the first node's
// result and records it succeeded without it ever executing — and the per-run session uniqueness
// refuses its dispatch outright, so the node can neither run nor be settled honestly.
func TestWorkflowSessionRunsTwoAgentNodesInOrder(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowTwoAgentGraph())
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a1")
	execution1 := dispatchSession(t, store, runID, "a1", nodeService, spec1)
	reportSessionEnd(t, store, runID, execution1, nodeService, "idle_timeout")

	// One pass settled a1 and started a2. The loop keeps ticking while a2's session is in flight —
	// and a2 must still be running: it has no execution of its own, and a run-scoped evidence lookup
	// hands it a1's result and records it succeeded without it ever executing.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance while a2 is in flight: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	a1, _ := asWorkflowObject(states["a1"])
	a2, _ := asWorkflowObject(states["a2"])
	if status != "running" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want running with a2 in flight", status, runErr, states)
	}
	if a1 == nil || a1.S("status") != "succeeded" || a1.O("output").S("reason") != "idle_timeout" {
		t.Fatalf("a1 state = %v; want succeeded/idle_timeout", states["a1"])
	}
	if a2 == nil || a2.S("status") != "running" {
		t.Fatalf("a2 state = %v; want running — a node with no result of its own is never succeeded", states["a2"])
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("the output node ran before a2 settled: %v", states)
	}

	// The second node dispatches on its own work item and settles from its own result.
	spec2 := sessionWorkSpec(t, store, runID, "a2")
	execution2 := dispatchSession(t, store, runID, "a2", nodeService, spec2)
	if execution2 == execution1 {
		t.Fatalf("both nodes dispatched as %s; each node needs its own execution", execution1)
	}
	reportSessionEnd(t, store, runID, execution2, nodeService, "user_ended")

	status, runErr, states, _ = readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want succeeded", status, runErr, states)
	}
	a1, _ = asWorkflowObject(states["a1"])
	a2, _ = asWorkflowObject(states["a2"])
	outs, _ := asWorkflowObject(states["o"])
	if a1 == nil || a1.S("status") != "succeeded" || a2 == nil || a2.S("status") != "succeeded" || outs == nil || outs.S("status") != "succeeded" {
		t.Fatalf("node states = %v; want both agents and the output succeeded", states)
	}
	// Each node carries its own evidence: a2's reason is its own, not a1's.
	if reason := a2.O("output").S("reason"); reason != "user_ended" {
		t.Fatalf("a2 output reason = %q; want user_ended (a1 ended idle_timeout)", reason)
	}

	// Two executions, each registered against the work item of the node it ran for.
	rows, err := store.Pool.Query(`SELECT ew.workflow_node_id, ne.execution_id, ne.result->>'reason'
		FROM node_executions ne JOIN execution_work ew ON ew.id=ne.work_id
		WHERE ew.workflow_run_id=$1 AND ne.kind='agent_session' ORDER BY ew.workflow_node_id`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type session struct{ node, execution, reason string }
	var sessions []session
	for rows.Next() {
		var s session
		if err := rows.Scan(&s.node, &s.execution, &s.reason); err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []session{{"a1", execution1, "idle_timeout"}, {"a2", execution2, "user_ended"}}
	if len(sessions) != len(want) {
		t.Fatalf("registered sessions = %v; want one per agent node", sessions)
	}
	for i, w := range want {
		if sessions[i] != w {
			t.Fatalf("session %d = %v; want %v", i, sessions[i], w)
		}
	}
}

// Dispatch stays bounded now that the per-run agent_session uniqueness applies to IssueRuns alone:
// the work item is consumed by its dispatch, so no second execution can register against a node's
// work, and a replayed dispatch is the same fact rather than a second session.
func TestWorkflowSessionDispatchIsNotRepeatable(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowTwoAgentGraph())
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a1")
	dispatch := func(executionID string, input Object) error {
		_, err := store.transact(context.Background(), func(tx *transaction) Object {
			nodeDispatch(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": executionID, "nodeId": nodeService, "input": input, "epoch": int64(1)}})
			return Object{}
		})
		return err
	}
	execution1 := "exec-a1-" + runID[:8]
	if err := dispatch(execution1, spec1); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// The same execution and input is the same fact replayed, not a second session.
	if err := dispatch(execution1, spec1); err != nil {
		t.Fatalf("replayed dispatch: %v", err)
	}
	// A second execution cannot register against the consumed work item: a run runs one session at
	// a time, and a node's work item names exactly that node's session.
	if err := dispatch("exec-a1-again-"+runID[:8], spec1); ErrorCode(err).Code != "dispatch_conflict" {
		t.Fatalf("second execution for the same node: err = %v; want dispatch_conflict", err)
	}
	var executions int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM node_executions WHERE operation_id=$1 AND kind='agent_session'`, runID).Scan(&executions); err != nil {
		t.Fatal(err)
	}
	if executions != 1 {
		t.Fatalf("%d agent_session executions registered; want 1", executions)
	}

	// Once a1 settles, its dispatch cannot be replayed onto a2's released work item: the input a
	// work item carries is the session it admits, so a settled node's spec is refused rather than
	// run a second time as the next node.
	reportSessionEnd(t, store, runID, execution1, nodeService, "idle_timeout")
	if err := dispatch("exec-a1-late-"+runID[:8], spec1); ErrorCode(err).Code != "dispatch_conflict" {
		t.Fatalf("late dispatch of a settled node: err = %v; want dispatch_conflict", err)
	}
	var stillOne int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM node_executions WHERE operation_id=$1 AND kind='agent_session'`, runID).Scan(&stillOne); err != nil {
		t.Fatal(err)
	}
	if stillOne != 1 {
		t.Fatalf("%d agent_session executions after the late dispatch; want 1", stillOne)
	}
}

// A session that ends `agent_failed` fails its node and the run with that reason — the evidence
// decides, not a policy — and the terminal cleanup still runs.
func TestWorkflowSessionFailureFailsRun(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	reportSessionEnd(t, store, runID, execution, nodeService, "agent_failed")
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "agent_failed" {
		t.Fatalf("run settled %q error %q states=%v; want failed/agent_failed", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.O("output").S("reason") != "agent_failed" {
		t.Fatalf("agent state = %v; want failed with agent_failed", states["a"])
	}
	// A failed node carries its reason as `error`, the field the run viewer's Error section reads.
	// It was described in the contract and read by the client but never written, so a failed node
	// showed an empty Error section while its reason sat unrendered inside output.
	if got := agent.S("error"); got != "agent_failed" {
		t.Fatalf("failed agent error = %q; want agent_failed", got)
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// A session that reports `cancelled` cancels its node and the run. Cancelled is a third terminal
// status with its own settlement path — a run that is cancelled rather than failed is not a run that
// failed, and the two are asserted separately rather than inferred from each other.
func TestWorkflowSessionCancelledReleasesTheRunWorkspace(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	reportSessionEnd(t, store, runID, execution, nodeService, "cancelled")
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "cancelled" || runErr != "cancelled" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled/cancelled", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "cancelled" || agent.O("output").S("reason") != "cancelled" {
		t.Fatalf("agent state = %v; want cancelled carrying the session's own reason", states["a"])
	}
	// A cancelled node was cancelled, not broken, so it carries no error: the viewer's Error section
	// is for nodes that failed.
	if _, present := agent["error"]; present {
		t.Fatalf("a cancelled node carries error %v; want none", agent["error"])
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// readRunFinishedAt reads the run's terminal timestamp; "" means the run has not finished, which is
// what the projection's finishedAt carries.
func readRunFinishedAt(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var finished sql.NullTime
	if err := store.Pool.QueryRow(`SELECT finished_at FROM workflow_runs WHERE id=$1`, runID).Scan(&finished); err != nil {
		t.Fatalf("read run finished_at: %v", err)
	}
	if !finished.Valid {
		return ""
	}
	return finished.Time.UTC().Format(time.RFC3339)
}

// A run that is still executing carries no finished_at. The timestamp is the run's terminal moment,
// and the projection is what a client reads: a non-empty finishedAt is how it renders a run as over.
// The progress passes stamp it too — committing an agent node running, and settling one so the next
// wave starts — so a live run used to report the moment it "finished" while its session was still
// executing. Only a terminal status writes it, and the first terminal write wins.
func TestWorkflowRunningCarriesNoFinishedAt(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	// The pass that committed the node running is a progress settle: the run is `running`.
	if finished := readRunFinishedAt(t, store, runID); finished != "" {
		t.Fatalf("a run whose session just started carries finished_at %q; want none", finished)
	}
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	if finished := readRunFinishedAt(t, store, runID); finished != "" {
		t.Fatalf("a run with a dispatched session carries finished_at %q; want none", finished)
	}
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	finished := readRunFinishedAt(t, store, runID)
	if finished == "" {
		t.Fatal("a succeeded run carries no finished_at; want the terminal moment")
	}
	// A later pass re-declaring the terminal status cannot move the moment the run stopped.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after terminal: %v", err)
	}
	if again := readRunFinishedAt(t, store, runID); again != finished {
		t.Fatalf("finished_at moved from %q to %q on a later pass", finished, again)
	}
}

// assertRunWorkspaceReleased asserts the D7 half of a terminal settle: the run's Workspace has its
// deletion declared, and no unregistered session work survives to be claimed. Every terminal status
// owes this, and the failing and cancelled ones owe it most — a run that stops holding a live
// Workspace is exactly the case where a missing release leaves the clone, the sandbox and the
// Project's operation slot occupied with nothing left to notice. Declaring the delete is a
// declaration rather than a completed deletion: the operation machinery drains it asynchronously,
// and re-declaring on a later pass is a no-op.
func assertRunWorkspaceReleased(t *testing.T, store *Store, runID string) {
	t.Helper()
	var unregistered int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND execution_id IS NULL`, runID).Scan(&unregistered); err != nil {
		t.Fatalf("count unregistered work: %v", err)
	}
	if unregistered != 0 {
		t.Fatalf("%d unregistered work items survived a terminal run", unregistered)
	}
	var deletes int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM operations o JOIN workspaces w ON w.id=o.workspace_id
		WHERE w.workflow_run_id=$1 AND o.kind='delete_workspace'`, runID).Scan(&deletes); err != nil {
		t.Fatalf("count delete operations: %v", err)
	}
	if deletes != 1 {
		t.Fatalf("%d workspace deletions declared for a terminal run; want 1", deletes)
	}
}

// readFrozenPluginVersion reads the plugin version off the run row: the frozen identity is a stored
// fact, so the only way to tell "the API froze it" from "something resolved it later" is to look at
// what the creation transaction left behind.
func readFrozenPluginVersion(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var frozen sql.NullString
	if err := store.Pool.QueryRow(`SELECT plugin_version FROM workflow_runs WHERE id=$1`, runID).Scan(&frozen); err != nil {
		t.Fatalf("read run plugin version: %v", err)
	}
	return frozen.String
}

// pluginVersionOf runs the creation-time resolver in its own transaction and returns what it would
// freeze right now — the same read createWorkflowRun makes, so a run created at this moment freezes
// exactly this.
func pluginVersionOf(t *testing.T, store *Store, tid, project string) string {
	t.Helper()
	frozen := ""
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		frozen, _ = resolveWorkflowRunPluginVersion(tx, tid, project).(string)
		return Object{}
	}); err != nil {
		t.Fatalf("resolve plugin version: %v", err)
	}
	return frozen
}

// An upgrade of the space's plugin while a run is in flight cannot change what that run executes: the
// version is frozen in the creation transaction and the session spec carries the frozen one. Before
// this, the version was read at session start, so a run created against 1.0.0 and dispatched after an
// operator upgraded to 2.0.0 executed 2.0.0 — the run's execution content decided by a change made
// after it was created, which is the one thing the frozen identity exists to prevent.
func TestWorkflowRunFreezesPluginVersionAgainstUpgrade(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, space, project := seedWorkflowSessionScene(t, store)
	if frozen := readFrozenPluginVersion(t, store, runID); frozen != "1.0.0" {
		t.Fatalf("run froze plugin version %q; want the space's 1.0.0", frozen)
	}

	// The upgrade an operator performs without knowing a run is open.
	if _, err := store.Pool.Exec(`UPDATE space_plugins SET desired_version='2.0.0', version=version+1 WHERE space_id=$1`, space); err != nil {
		t.Fatalf("upgrade plugin: %v", err)
	}

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to workspace create: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	_, spec := startSessionNode(t, store, runID, wsID, "a")
	if spec.S("agentPluginVersion") != "1.0.0" {
		t.Fatalf("session plugin version = %q; want the frozen 1.0.0", spec.S("agentPluginVersion"))
	}
	// The freeze reads the live value at creation rather than a constant: a run created after the
	// upgrade freezes 2.0.0, and the run already in flight still carries 1.0.0.
	if after := pluginVersionOf(t, store, tid, project); after != "2.0.0" {
		t.Fatalf("a run created after the upgrade would freeze %q; want 2.0.0", after)
	}
	if frozen := readFrozenPluginVersion(t, store, runID); frozen != "1.0.0" {
		t.Fatalf("the upgrade rewrote the in-flight run's frozen version to %q", frozen)
	}
}

// A run created before the version was frozen carries none, and the session start must not read the
// space to find one: the space still has the plugin installed here, so a live read would succeed and
// would silently execute a version the run was never created against. The node fails with its own
// reason instead, which is what makes such rows countable while the compatibility rule is undecided.
func TestWorkflowSessionRefusesAnUnfrozenPluginVersion(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, space, _ := seedWorkflowSessionScene(t, store)
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET plugin_version=NULL WHERE id=$1`, runID); err != nil {
		t.Fatalf("clear frozen version: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to workspace create: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	bindConnectedNode(t, store, wsID)
	if _, err := store.Pool.Exec(`UPDATE node_instances SET model_proxy=true WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to session start: %v", err)
	}

	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "agent_plugin_version_unfrozen" {
		t.Fatalf("run settled %q error %q; want failed/agent_plugin_version_unfrozen", status, runErr)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.O("output").S("reason") != "agent_plugin_version_unfrozen" {
		t.Fatalf("agent state = %v; want the unfrozen-version failure", states["a"])
	}
	// Nothing was dispatched: the run must not have reached the Node with a version read from the space.
	var work int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1`, runID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if work != 0 {
		t.Fatalf("%d work items were released for a run with no frozen plugin version", work)
	}
	var installed string
	if err := store.Pool.QueryRow(`SELECT desired_version FROM space_plugins WHERE space_id=$1`, space).Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed != "1.0.0" {
		t.Fatalf("the space's plugin moved to %q; the case no longer proves a live read was refused", installed)
	}
}

// A session that never dispatches fails honestly once the startup deadline passes: the node and the
// run carry the named reason instead of hanging in `running` forever.
func TestWorkflowSessionStartDeadlineFailsNode(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	startSessionNode(t, store, runID, wsID, "a")
	// The Node never picks the work item up: the committed start ages past the deadline.
	stale := time.Now().Add(-workflowSessionStartDeadline - time.Minute).UTC().Format(time.RFC3339)
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET node_states=jsonb_set(node_states,'{a,startedAt}',to_jsonb($2::text)) WHERE id=$1`, runID, stale); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance past deadline: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "session start deadline exceeded" {
		t.Fatalf("run settled %q error %q states=%v; want the deadline failure", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.O("output").S("reason") != "session_start_deadline_exceeded" {
		t.Fatalf("agent state = %v; want the deadline failure", states["a"])
	}
	if got := agent.S("error"); got != "session_start_deadline_exceeded" {
		t.Fatalf("failed agent error = %q; want session_start_deadline_exceeded", got)
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// The run-creation gate freezes one binding per slot the graph references, so a node whose slot has
// no frozen binding is a run whose freeze never happened — not a state the next tick can fix. The
// session must not start without a model to run on: the node fails under the gate's own reason, the
// way it does for an unresolvable slot at creation, instead of handing the Node a binding-less
// session that would run on whatever the Node defaults to.
func TestWorkflowSessionWithoutFrozenBindingFailsTheNode(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	bindConnectedNode(t, store, wsID)
	if _, err := store.Pool.Exec(`UPDATE node_instances SET model_proxy=true WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	// Everything the session needs is in place except the freeze itself.
	if _, err := store.Pool.Exec(`DELETE FROM run_model_bindings WHERE workflow_run_id=$1`, runID); err != nil {
		t.Fatalf("drop the frozen binding: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to session start: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "failed" || runErr != "workflow_executor_unconfigured" {
		t.Fatalf("run settled %q error %q; want failed/workflow_executor_unconfigured", status, runErr)
	}
	if agent == nil || agent.S("status") != "failed" || agent.S("error") != "workflow_executor_unconfigured" {
		t.Fatalf("node a = %v; want failed under the same reason", states["a"])
	}
}

// A replayed terminal receipt for a settled run changes nothing: the run's status, its committed
// node states and its version are exactly what the first settle left, and the extra passes write
// nothing. Late or duplicate evidence is a no-op (invariant 3).
func TestWorkflowSessionLateReceiptIsANoOp(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if status != "succeeded" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	// The Node replays the same terminal fact, and the loop keeps ticking.
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after replay: %v", err)
	}
	replayedStatus, replayedErr, replayedStates, replayedVersion := readWorkflowRun(t, store, runID)
	if replayedStatus != status || replayedErr != runErr {
		t.Fatalf("replay moved the run to %q error %q; want %q error %q", replayedStatus, replayedErr, status, runErr)
	}
	if replayedVersion != version {
		t.Fatalf("replay bumped the version to %d; want %d", replayedVersion, version)
	}
	if jsonText(replayedStates) != jsonText(states) {
		t.Fatalf("replay rewrote the node states:\n got %v\nwant %v", replayedStates, states)
	}
}

// The model grant's live binding resolves a WorkflowRun session through the workflow branch: the
// frozen slot binding, the dispatched session execution and the confirmed runtime binding authorize
// it, and an execution that is not this run's session is refused rather than resolved to some other
// run.
func TestWorkflowLiveModelBindingResolvesRun(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	// The runtime confirms the Node binding once the session is dispatched — the state the model
	// gateway's certificate-bound lookups run against.
	if _, err := store.Pool.Exec(`UPDATE runtime_controls SET binding_confirmed=true,
		bound_sandbox_id=(SELECT sandbox_instance_id FROM node_instances WHERE workspace_id=$1 AND ended_at IS NULL)
		WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	var bindingID, tenant string
	var generation int64
	if err := store.Pool.QueryRow(`SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 AND slot_name='default'`, runID).Scan(&bindingID); err != nil {
		t.Fatalf("read frozen binding: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT tenant_id, runtime_generation FROM workspaces WHERE id=$1`, wsID).Scan(&tenant, &generation); err != nil {
		t.Fatal(err)
	}
	scope := ModelRuntimeScope{TenantID: tenant, WorkspaceID: wsID, Generation: generation}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		b := liveModelBinding(tx, scope, bindingID, execution)
		if b.S("runId") != runID || b.S("nodeId") != nodeService {
			t.Fatalf("binding = %v; want run %s on node %s", b, runID, nodeService)
		}
		return Object{}
	}); err != nil {
		t.Fatalf("live binding: %v", err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		liveModelBinding(tx, scope, bindingID, uuid.NewString())
		return Object{}
	}); err == nil || ErrorCode(err).Code != "model_grant_revoked" {
		t.Fatalf("foreign execution: err=%v; want model_grant_revoked", err)
	}
}

// A run freezes one binding per executor slot (gateGraph references `default` and `strong`), so
// "the binding belongs to this execution's run" admits more than one row. A session dispatched for
// node a1 under `default` must not be able to name `strong`'s binding and be handed that slot's
// credential and model: the binding id is the caller's to state, because a model mTLS certificate is
// bound to a tenant, workspace and runtime generation and never to a slot.
//
// What settles it is the execution's own frozen modelBindingId, which Cloud wrote when it enqueued
// the work and which no request body can restate.
func TestWorkflowLiveModelBindingRefusesAnotherSlotsBinding(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, gateGraph())
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a1")
	execution := dispatchSession(t, store, runID, "a1", nodeService, spec)
	if _, err := store.Pool.Exec(`UPDATE runtime_controls SET binding_confirmed=true,
		bound_sandbox_id=(SELECT sandbox_instance_id FROM node_instances WHERE workspace_id=$1 AND ended_at IS NULL)
		WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	var own, other, tenant string
	var generation int64
	if err := store.Pool.QueryRow(`SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 AND slot_name='default'`, runID).Scan(&own); err != nil {
		t.Fatalf("read the default binding: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 AND slot_name='strong'`, runID).Scan(&other); err != nil {
		t.Fatalf("read the strong binding: %v", err)
	}
	if spec.S("modelBindingId") != own || own == other {
		t.Fatalf("dispatched with %q; want the default slot's %q, a distinct row from strong's %q", spec.S("modelBindingId"), own, other)
	}
	if err := store.Pool.QueryRow(`SELECT tenant_id, runtime_generation FROM workspaces WHERE id=$1`, wsID).Scan(&tenant, &generation); err != nil {
		t.Fatal(err)
	}
	scope := ModelRuntimeScope{TenantID: tenant, WorkspaceID: wsID, Generation: generation}
	// The slot the session was dispatched under still resolves, so the refusal below is about the
	// binding and not about the scene failing some other gate.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		if b := liveModelBinding(tx, scope, own, execution); b.S("runId") != runID || b.S("nodeId") != nodeService {
			t.Fatalf("binding = %v; want run %s on node %s", b, runID, nodeService)
		}
		return Object{}
	}); err != nil {
		t.Fatalf("own slot's binding: %v", err)
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		liveModelBinding(tx, scope, other, execution)
		return Object{}
	}); err == nil || ErrorCode(err).Code != "model_grant_revoked" {
		t.Fatalf("another slot's binding for this execution: err=%v; want model_grant_revoked", err)
	}
}
