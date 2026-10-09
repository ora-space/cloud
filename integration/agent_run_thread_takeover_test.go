package integration

// Production-path acceptance for the Phase 4B Thread takeover (mandate §43, IssueRun D3,
// controller-integration D2/D6). The chain under test is the one cmd/server wires, named here so a
// reader can follow it without guessing:
//
//	Controller → controlpb.AgentRunService.TakeOverThreadEvents          (gRPC, this file)
//	  → controlgrpc.agentRunService.TakeOverThreadEvents                 (internal/controlgrpc/agent_runs.go)
//	  → Store.Control(Action: "thread_events")                           (internal/core/control.go)
//	  → Store.transact: lease check + pg_advisory_xact_lock              (internal/core/store.go)
//	  → takeOverThreadEvents: node_event_receipts + per-event fence      (internal/core/thread_commands.go)
//	  → threadEventsTakenOver → Store.OnThreadEvents                     (internal/core/agent_run_hooks.go)
//	  → onThreadEvents → threadEventsTakenOver                           (internal/core/business_hooks.go)
//	  → thread_entries + the Thread lifecycle transition                 (internal/core/agent_run_thread.go)
//	  → COMMIT → TakeOverThreadEventsResponse{ taken_over_through }
//
// Everything before the gRPC call is production code too: the run reaches `starting` through the
// real Phase 3A recovery pass (Store.StartQueuedAgentSessionsOnce) and the session execution is
// registered through the real Phase 4A control actions (the `clone_claim` and `clone_dispatch`
// commands, whose execution-centric names the merged control plane kept). Only the scene itself —
// tenant, project, Space Agent, issue, run and its run Workspace — is seeded directly, because run
// creation and CreateRunWorkspace are still G-001 placeholders.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// seedRunWorkspaceCommit is the 40-hex commit the seeded run Workspace's clone produced, i.e. the
// baseline every Revision bundle is relative to. It is the shape commitID validates, so a scene that
// seeds it can never be the reason a delivery input is rejected.
const seedRunWorkspaceCommit = "0123456789abcdef0123456789abcdef01234567"

// seededAgentSession is one `starting` agent run whose session execution is registered and whose
// Thread is one takeover away from `running`.
type seededAgentSession struct {
	runID         string
	executionID   string
	initialTurnID string
	nodeID        string
}

// seedAgentSessionScene loads the scene and drives the real production path up to (and excluding)
// the takeover: StartQueuedAgentSessionsOnce writes the immutable seq=1 first prompt and declares
// the agent_session execution_work, then a Controller claims and registers it, which is exactly the
// state the Node must be running in before it can send the first Thread event.
func seedAgentSessionScene(t *testing.T, store *core.Store) seededAgentSession {
	t.Helper()
	ctx := context.Background()
	// The production seam, wired exactly as cmd/server/main.go wires it. Without it the takeover below
	// would roll back fail-closed (G-003) rather than prove the real path: the control plane leaves the
	// B side a no-op handoff until Cloud's lifecycle is bound onto it.
	core.BindBusinessHooks(store)

	runID, _, nodeID := seedStartingAgentRun(t, store)
	must(t, store.StartQueuedAgentSessionsOnce(ctx))

	claims := &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}}
	picked, e := store.Control(ctx, &core.ControlRequest{Action: "clone_claim", Body: core.Object{"epoch": 1}, Service: claims})
	must(t, e)
	work := picked.O("work")
	if work == nil {
		t.Fatal("the started run must have declared exactly one agent_session work item")
	}
	executionID := "exec-grpc-" + work.S("id")[:8]
	_, e = store.Control(ctx, &core.ControlRequest{
		Action:  "clone_dispatch",
		Body:    core.Object{"operationId": runID, "executionId": executionID, "nodeId": nodeID, "input": work.O("input"), "epoch": 1},
		Service: claims,
	})
	must(t, e)

	var turnID string
	must(t, store.Pool.QueryRow(`SELECT turn_id FROM thread_entries WHERE run_id=$1 AND seq=1`, runID).Scan(&turnID))
	return seededAgentSession{runID: runID, executionID: executionID, initialTurnID: turnID, nodeID: nodeID}
}

// seedStartingAgentRun seeds the minimal schema-valid scene (tenant, admin member, collaboration
// space, active project, live run Workspace with its task, active Space Agent, issue, `starting`
// agent run with its frozen input snapshot, connected sandbox and Node) plus the global Controller
// lease, and returns the run, its Workspace and its Node.
func seedStartingAgentRun(t *testing.T, store *core.Store) (runID, workspaceID, nodeID string) {
	t.Helper()
	user, tenant, space, project, agent, issue := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	runID, workspaceID, nodeID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	sandboxID := uuid.NewString()
	// The run Workspace's baseline identities. Deterministic per workspace so a test can name them
	// without re-deriving the schema's own linkage.
	baselineOperationID, baselineExecutionID := uuid.NewString(), "exec-baseline-"+workspaceID[:8]
	// The frozen run-create snapshot: renderAgentInitialTurn and the AgentSession spec both read it.
	// `gitIdentity` is part of it because the AgentSession wire contract carries one and
	// runGitIdentity refuses to invent it: a run whose input names none resolves its trigger actor
	// instead, and a directly seeded run has no `run.enqueued` activity to resolve one from.
	input := core.Object{
		"task":               "Fix the auth flow",
		"interactionValues":  core.Object{"scope": "web"},
		"target":             core.Object{"type": "issue", "id": issue},
		"agentPluginId":      "official/hello-world",
		"agentPluginVersion": "1.0.0",
		"gitIdentity":        core.Object{"name": "takeover", "email": user + "@users.noreply.ora.invalid"},
	}
	tx, e := store.Pool.Begin()
	must(t, e)
	exec := func(name, q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	exec("user", `INSERT INTO users(id,display_name,status) VALUES($1,'takeover','active')`, user)
	exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'takeover','active')`, tenant)
	exec("membership", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenant, user)
	exec("space", `INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'takeover',$3,$4)`, space, tenant, "takeover-"+space[:8], user)
	exec("project", `INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle) VALUES($1,$2,$3,$4,'takeover','https://example.invalid/takeover.git','main','active')`, project, tenant, user, space)
	// A live project carries exactly one main workspace (deferred project_main trigger); the run
	// Workspace is the separate kind='isolated' one below.
	exec("main-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$4,'main','running','ready',1,'HEAD')`, uuid.NewString(), tenant, user, project)
	exec("agent", `INSERT INTO space_agents(id,space_id,tenant_id,plugin_id,display_name,status) VALUES($1,$2,$3,'official/hello-world','Hello','active')`, agent, space, tenant)
	exec("issue", `INSERT INTO issues(id,tenant_id,creator_user_id,project_ref,title,number) VALUES($1,$2,$3,$4,'takeover issue',1)`, issue, tenant, user, project)
	exec("run", `INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,phase,input) VALUES($1,$2,$3,'agent',$4,'dispatched','starting',$5)`, runID, tenant, issue, agent, mustJSON(t, input))
	// The run Workspace: isolated, bound to this exact run, live — the G-011 predicate the session
	// start and the takeover both re-read. A live isolated workspace needs exactly one task identity,
	// and the workspace/task inserts must share the transaction for that deferred trigger. Admission
	// is open because that is what `ready` means here: the create_workspace path's last step is
	// openWorkspace, which commits observed_state='ready' and admission_open=true together, so a
	// `ready` Workspace with admission closed is a state the production path never produces.
	//
	// creator_user_id/creator_evidence are part of the scene because the run Workspace's own deletion
	// (the run's terminal settlement releases it) reads the creator as the operation's actor, and
	// insertWorkspace always records the resolved trigger actor with 'verified_request' evidence. A
	// row with the column left NULL is a state createRunWorkspace cannot produce, and the delete would
	// then enqueue an operation with an empty actor.
	exec("run-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,admission_open,runtime_generation,requested_ref,issue_run_id) VALUES($1,$2,$3,$4,'verified_request',$5,'isolated','running','ready',true,1,'HEAD',$6)`, workspaceID, tenant, user, user, project, runID)
	exec("task", `INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'takeover task')`, uuid.NewString(), workspaceID)
	exec("run-workspace-bind", `UPDATE issue_runs SET workspace_id=$2, version=version+1, updated_at=now() WHERE id=$1`, runID, workspaceID)
	// The run Workspace's baseline. A Workspace observed `ready` reached that state through its
	// create_workspace operation, whose clone step recorded the successful clone execution and its
	// commit; the schema holds both, and the session-terminal path reads them to build the delivery
	// input (base_commit plus the checkout execution it came from). Seeding them keeps the scene a
	// state the production path can actually reach.
	exec("run-workspace-operation", `INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,controller_epoch) VALUES($1,$2,$3,$4,$5,'create_workspace','succeeded','done','{}',$6,'baseline-hash',1)`,
		baselineOperationID, tenant, user, project, workspaceID, "run-workspace-"+workspaceID[:8])
	exec("run-workspace-clone", `INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,node_operation_id,input,result,dispatched_epoch) VALUES($1,$2,$3,$4,$5,$6,$7,1)`,
		baselineExecutionID, baselineOperationID, workspaceID, nodeID, baselineExecutionID,
		mustJSON(t, core.Object{"kind": "clone", "repositoryUrl": "https://example.invalid/takeover.git", "branch": "main"}),
		mustJSON(t, core.Object{"node": core.Object{"nodeId": nodeID, "nodeIncarnationId": "inc-" + nodeID[:8]}, "outcome": "clone_ready", "path": "/work", "commit": seedRunWorkspaceCommit}))
	exec("run-workspace-baseline", `UPDATE workspaces SET base_commit_id=$2, version=version+1 WHERE id=$1`, workspaceID, seedRunWorkspaceCommit)
	// The run's runtime binding, in exactly the state the create_workspace operation's last step
	// leaves it (finishRuntimeMaintenance): the maintenance intent has moved from the operation to the
	// run, the epoch has advanced and the Node has not yet confirmed the new binding. Both the session
	// dispatch and every delivery settlement re-read this row, and the Node's own acknowledgement is
	// what closes it — so seeding anything else here would test a state the production path cannot
	// produce. maintenance_operation_id is NULL because 0026 allows exactly one of the two intents.
	exec("run-runtime-control", `INSERT INTO runtime_controls(workspace_id,state,maintenance_run_id,control_epoch,binding_confirmed,input_closed) VALUES($1,'maintenance',$2,2,false,false)`, workspaceID, runID)
	exec("sandbox", `INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,1,'running')`, sandboxID, workspaceID)
	// The Node carries the desktop's two-part identity (0016): `node_id` is the identity the Node
	// service itself owns and the one every control action addresses it by, while the row id is
	// Cloud's own registration handle. Both are the seeded nodeID here so a test names one value.
	// node_id is text and the row id is uuid, so the identity is bound once as its own parameter: one
	// placeholder used for both columns would leave PostgreSQL unable to deduce either type.
	exec("node", `INSERT INTO node_instances(id,workspace_id,sandbox_instance_id,service_subject,connection_state,protocol_version,initialized,node_id,node_incarnation_id) VALUES($1,$2,$3,'node','connected',1,true,$4,$5)`, nodeID, workspaceID, sandboxID, nodeID, "inc-"+nodeID[:8])
	// The global Controller lease. On a fresh schema this seeds it; in a fixture schema that already
	// has one (its own simulator controller) it re-points that row to the Controller the test speaks
	// as, so the lease-validated takeover actions below are validated against exactly this holder.
	exec("lease", `INSERT INTO controller_leases(name,holder_id,epoch,expires_at) VALUES('global','ctrl-a',1,clock_timestamp()+interval '30 minutes')
		ON CONFLICT (name) DO UPDATE SET holder_id='ctrl-a', epoch=1, expires_at=clock_timestamp()+interval '30 minutes'`)
	must(t, tx.Commit())
	return runID, workspaceID, nodeID
}

// mustJSON renders a value as the JSON text a jsonb column expects.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, e := json.Marshal(v)
	must(t, e)
	return string(b)
}

// threadLine renders one settled `ora-history` line as the Node sends it: the HistoryLine's `at`
// and its own line `seq`, flattened with the HistoryRecord's `type` tag (desktop
// crates/history/src/record.rs). The inner `seq` is the Node's line number and is deliberately
// unrelated to the Cloud-assigned Thread seq.
func threadLine(tag, text string) string {
	return historyLine(tag, "agent_message_chunk", text)
}

// userTurnLine renders the user message record the Node writes when it starts a user turn: the
// only record whose turn id makes it an echo of a turn Cloud already wrote. The agent's replies in
// that turn carry the same turn id (Node protocol D2) but render through threadLine.
func userTurnLine(text string) string {
	return historyLine("update", "user_message_chunk", text)
}

// historyLine renders one `ora-history` line with an ACP session update of the given kind.
func historyLine(tag, sessionUpdate, text string) string {
	line := core.Object{"at": "2026-09-30T10:00:00+00:00", "seq": 0, "type": tag}
	if text != "" {
		line["update"] = core.Object{"sessionUpdate": sessionUpdate, "content": core.Object{"type": "text", "text": text}}
	}
	b, e := json.Marshal(line)
	if e != nil {
		panic(e)
	}
	return string(b)
}

// TestAgentRunThreadTakeoverOverGRPC is the end-to-end production acceptance: a Controller takes
// over one contiguous batch through the real gRPC surface. The batch carries the Node's first real
// record plus the echo of the Cloud-authored first prompt, so one call proves both the running
// authority (first real record → running/running/active in the same commit) and the echo dedupe
// (receipt yes, entry no), and the replay call proves the C5 "commit landed, ack lost" case is an
// idempotent no-op on the wire.
func TestAgentRunThreadTakeoverOverGRPC(t *testing.T) {
	h := newControlHarness(t)
	scene := seedAgentSessionScene(t, h.store)
	client := controlpb.NewAgentRunServiceClient(h.conn)

	batch := []*controlpb.ThreadEvent{
		{Sequence: 1, Record: threadLine("update", "working on the fix")},
		{Sequence: 2, TurnId: &scene.initialTurnID, Record: userTurnLine("Fix the auth flow")},
	}
	resp, e := client.TakeOverThreadEvents(asController("ctrl-a"), &controlpb.TakeOverThreadEventsRequest{
		Epoch: 1, OperationId: scene.runID, ExecutionId: scene.executionID, Events: batch,
	})
	must(t, e)
	if resp.GetTakenOverThrough() != 2 {
		t.Fatalf("taken_over_through = %d, want 2 (the echo still advances the execution's sequence)", resp.GetTakenOverThrough())
	}

	// Durable receipts: the ack basis (protocol root D4), one row per event, echo included.
	var receipts int
	must(t, h.store.Pool.QueryRow(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, scene.executionID).Scan(&receipts))
	if receipts != 2 {
		t.Fatalf("receipts = %d, want 2", receipts)
	}
	var lastEventSequence int64
	must(t, h.store.Pool.QueryRow(`SELECT last_event_sequence FROM node_executions WHERE execution_id=$1`, scene.executionID).Scan(&lastEventSequence))
	if lastEventSequence != 2 {
		t.Fatalf("last_event_sequence = %d, want 2", lastEventSequence)
	}
	// The Thread: the immutable Cloud-authored seq=1 plus exactly one node entry for the real record.
	// The echoed prompt is not written a second time.
	var firstSource, firstKind string
	must(t, h.store.Pool.QueryRow(`SELECT source,kind FROM thread_entries WHERE run_id=$1 AND seq=1`, scene.runID).Scan(&firstSource, &firstKind))
	if firstSource != "system" || firstKind != "user_turn" {
		t.Fatalf("seq=1 must stay the Cloud-authored first prompt, got %s/%s", firstSource, firstKind)
	}
	var entries int
	must(t, h.store.Pool.QueryRow(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, scene.runID).Scan(&entries))
	if entries != 2 {
		t.Fatalf("thread entries = %d, want 2 (echo deduped)", entries)
	}
	var nodeSeq int64
	var nodeKind, nodeCollection string
	must(t, h.store.Pool.QueryRow(`SELECT seq,kind,node_execution_id FROM thread_entries WHERE run_id=$1 AND node_sequence=1`, scene.runID).Scan(&nodeSeq, &nodeKind, &nodeCollection))
	if nodeSeq != 2 || nodeKind != "update" || nodeCollection != scene.executionID {
		t.Fatalf("the real record must land as seq=2/kind=update for %s, got seq=%d kind=%s execution=%s", scene.executionID, nodeSeq, nodeKind, nodeCollection)
	}
	// The run itself: the takeover is the only running authority.
	var phase, status, threadState string
	must(t, h.store.Pool.QueryRow(`SELECT phase,status,thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase, &status, &threadState))
	if phase != "running" || status != "running" || threadState != "active" {
		t.Fatalf("first real record must commit running/running/active, got %s/%s/%s", phase, status, threadState)
	}

	// C5: the Controller lost the response and re-sends the same batch. Cloud replays the commit:
	// identical response, no second receipt, no second entry, no re-run of the transition.
	replay, e := client.TakeOverThreadEvents(asController("ctrl-a"), &controlpb.TakeOverThreadEventsRequest{
		Epoch: 1, OperationId: scene.runID, ExecutionId: scene.executionID, Events: batch,
	})
	must(t, e)
	if replay.GetTakenOverThrough() != resp.GetTakenOverThrough() {
		t.Fatalf("replay reported %d, want the original %d", replay.GetTakenOverThrough(), resp.GetTakenOverThrough())
	}
	must(t, h.store.Pool.QueryRow(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, scene.executionID).Scan(&receipts))
	must(t, h.store.Pool.QueryRow(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, scene.runID).Scan(&entries))
	if receipts != 2 || entries != 2 {
		t.Fatalf("a replay must write nothing: receipts=%d entries=%d", receipts, entries)
	}
	must(t, h.store.Pool.QueryRow(`SELECT phase,status,thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase, &status, &threadState))
	if phase != "running" || status != "running" || threadState != "active" {
		t.Fatalf("a replay must not rewrite run state, got %s/%s/%s", phase, status, threadState)
	}
}

// TestAgentRunThreadTakeoverGRPCRejections pins the wire contract for the batches Cloud refuses:
// an empty batch is INVALID_INPUT, a gap or an out-of-order batch is CONFLICT, and an execution
// Cloud never registered is NOT_FOUND (controller-integration D2, plan §4B.14). Every rejection
// leaves the run `starting` with no receipt, so nothing is ever acked on a rejected batch.
func TestAgentRunThreadTakeoverGRPCRejections(t *testing.T) {
	h := newControlHarness(t)
	scene := seedAgentSessionScene(t, h.store)
	client := controlpb.NewAgentRunServiceClient(h.conn)
	call := func(execution string, events ...*controlpb.ThreadEvent) error {
		t.Helper()
		_, e := client.TakeOverThreadEvents(asController("ctrl-a"), &controlpb.TakeOverThreadEventsRequest{
			Epoch: 1, OperationId: scene.runID, ExecutionId: execution, Events: events,
		})
		return e
	}

	expectStatus(t, call(scene.executionID), codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
	expectStatus(t, call(scene.executionID, &controlpb.ThreadEvent{Sequence: 2, Record: threadLine("update", "a")}),
		codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	expectStatus(t, call(scene.executionID,
		&controlpb.ThreadEvent{Sequence: 1, Record: threadLine("update", "a")},
		&controlpb.ThreadEvent{Sequence: 3, Record: threadLine("update", "b")}),
		codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	expectStatus(t, call(uuid.NewString(), &controlpb.ThreadEvent{Sequence: 1, Record: threadLine("update", "a")}),
		codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND)

	var receipts int
	must(t, h.store.Pool.QueryRow(`SELECT count(*) FROM node_event_receipts WHERE execution_id=$1`, scene.executionID).Scan(&receipts))
	if receipts != 0 {
		t.Fatalf("rejected batches must write no receipt (nothing was acked), got %d", receipts)
	}
	var entries int
	must(t, h.store.Pool.QueryRow(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, scene.runID).Scan(&entries))
	if entries != 1 {
		t.Fatalf("rejected batches must leave only the seq=1 first prompt, got %d entries", entries)
	}
	var phase string
	var status sql.NullString
	must(t, h.store.Pool.QueryRow(`SELECT phase,status FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase, &status))
	if phase != "starting" || status.String != "dispatched" {
		t.Fatalf("a rejected batch must not run the run, got %s/%s", phase, status.String)
	}
}
