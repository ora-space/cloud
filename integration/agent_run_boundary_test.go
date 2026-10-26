package integration

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/simulator"
)

func (f *fixture) registeredSession(t *testing.T, name string) (run, execution, node, incarnation string) {
	t.Helper()
	created := f.create(name)
	f.drain()
	run = f.insertAgentRun(t, created.O("resource").S("id"))
	wid := f.readyRunWorkspace(t, run)
	sandbox, node, incarnation := f.liveNode(t, wid)
	_, err := f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", sessionInput(), core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, err)
	execution = "session-boundary"
	item, err := f.executions.ClaimWork(asController(f.client.Subject), &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, err)
	_, err = f.executions.RecordDispatch(asController(f.client.Subject), &controlpb.RecordDispatchRequest{SubmissionId: "boundary-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: execution, NodeId: node, Input: item.GetItem().GetInput()})
	must(t, err)
	return run, execution, node, incarnation
}

// Receipts, sequence progress and business writes must commit or roll back together.
// specs/test-cases/cloud/controller-integration/agent-run-executions-and-thread.md#thread-events-are-acknowledged-only-after-ordered-takeover
func TestThreadBusinessHookSharesTheTakeoverTransaction(t *testing.T) {
	f := setup(t)
	run, execution, node, incarnation := f.registeredSession(t, "hook-atomicity")
	_, err := f.store.Pool.Exec("CREATE TABLE hook_evidence(sequence bigint PRIMARY KEY)")
	must(t, err)
	fail := true
	f.store.OnThreadEvents = func(ctx context.Context, tx *sql.Tx, gotRun, gotExecution string, events []core.Object) error {
		if gotRun != run || gotExecution != execution || len(events) != 1 || events[0].S("record") != `{"kind":"assistant"}` {
			t.Fatal("hook did not receive the complete first-taken-over event", events)
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO hook_evidence VALUES($1)", events[0].N("sequence")); e != nil {
			return e
		}
		if fail {
			return errors.New("business transition refused")
		}
		return nil
	}
	client := controlpb.NewAgentRunServiceClient(f.controlConn)
	req := &controlpb.TakeOverThreadEventsRequest{SubmissionId: "atomic-batch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: execution, Events: []*controlpb.ThreadEvent{{Sequence: 1, Record: `{"kind":"assistant"}`}}}
	_, err = client.TakeOverThreadEvents(asController(f.client.Subject), req)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if f.scalar("SELECT count(*) FROM hook_evidence") != 0 || f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id=$1", execution) != 0 || f.scalar("SELECT last_event_sequence FROM node_executions WHERE execution_id=$1", execution) != 0 {
		t.Fatal("failed business hook committed evidence or a receipt")
	}
	fail = false
	_, err = client.TakeOverThreadEvents(asController(f.client.Subject), req)
	must(t, err)
	_, err = client.TakeOverThreadEvents(asController(f.client.Subject), req)
	must(t, err)
	if f.scalar("SELECT count(*) FROM hook_evidence") != 1 {
		t.Fatal("replay called the hook again")
	}
	ended := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED}}}
	_, err = f.executions.TakeOverNodeEvent(asController(f.client.Subject), &controlpb.TakeOverNodeEventRequest{SubmissionId: "hook-end", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: execution, Sequence: 2, Result: ended, Event: []byte("end")})
	must(t, err)
	// A replacement Controller may use a fresh submission ID when replaying already ACKable records.
	req.SubmissionId = "replay-after-end"
	_, err = client.TakeOverThreadEvents(asController(f.client.Subject), req)
	must(t, err)
	if f.scalar("SELECT count(*) FROM hook_evidence") != 1 {
		t.Fatal("ended replay changed business evidence")
	}
	_, err = f.executions.TakeOverNodeEvent(asController(f.client.Subject), &controlpb.TakeOverNodeEventRequest{SubmissionId: "extra-terminal", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: execution, Sequence: 3, Result: ended, Event: []byte("end")})
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
}

func TestPluginEvidenceMustCoverThePlanExactly(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	for _, id := range []string{"official/hello-world", "official/native-tool"} {
		f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": id}, id, 200)
	}
	created := f.create("plugin-completeness")
	oid := created.O("operation").S("id")
	op := f.stepTo(oid, "plugin")
	snapshot := f.internal("/internal/v1/operations/"+oid+"/snapshot", core.Object{"epoch": op.N("epoch"), "version": op.N("version")}, 200).O("operation")
	f.dispatchPlugin(t, snapshot, "evidence")
	installed := &controlpb.PluginItemResult{PluginId: "official/hello-world", Outcome: &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: "1.0.0"}}}
	err := f.reportPlugin(t, snapshot, "evidence", []*controlpb.PluginItemResult{installed, installed})
	expectStatus(t, err, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
	removed := &controlpb.PluginItemResult{PluginId: "official/native-tool", Outcome: &controlpb.PluginItemResult_Removed{Removed: &controlpb.PluginItemRemoved{}}}
	err = f.reportPlugin(t, snapshot, "evidence", []*controlpb.PluginItemResult{installed, removed})
	expectStatus(t, err, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
	if f.scalar("SELECT count(*) FROM node_executions WHERE execution_id='evidence' AND result IS NOT NULL") != 0 || f.scalar("SELECT count(*) FROM workspace_plugin_instances WHERE workspace_id=$1 AND observed_state<>'installing'", snapshot.S("workspaceId")) != 0 {
		t.Fatal("invalid result partially changed the plugin instances")
	}
	must(t, f.reportPlugin(t, snapshot, "evidence", []*controlpb.PluginItemResult{installed, {PluginId: "official/native-tool", Outcome: &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: "2.0.0"}}}}))
	f.controlStep(t, snapshot, "/advance", core.Object{}, 200)
}

func TestAgentSelectionSurvivesCatalogRemoval(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "empty-space-agent", 200)
	if f.scalar("SELECT count(*) FROM space_agents WHERE space_id=$1 AND plugin_id='official/hello-world' AND status='active'", sid) != 1 {
		t.Fatal("installed agent in an empty Space has no Agent row")
	}
	_, err := f.store.Pool.Exec("DELETE FROM plugin_catalog_entries WHERE source_namespace='official'")
	must(t, err)
	current := f.spacePlugin(sid, "official/hello-world")
	f.call("DELETE", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world", "version": current.N("version")}, "removed-catalog-agent", 200)
	if f.scalar("SELECT count(*) FROM space_agents WHERE space_id=$1 AND status='retired'", sid) != 1 {
		t.Fatal("catalog disappearance prevented retiring the Agent")
	}
}

func TestExecutionIdentityCannotCrossRegistries(t *testing.T) {
	f := setup(t)
	run, execution, node, _ := f.registeredSession(t, "registry-identity")
	var cloneID, operation string
	must(t, f.store.Pool.QueryRow("SELECT execution_id,operation_id::text FROM clone_executions LIMIT 1").Scan(&cloneID, &operation))
	ctx := asController(f.client.Subject)
	_, err := f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "reuse-clone", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: cloneID, NodeId: node, Input: &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_AgentSession{AgentSession: &controlpb.AgentSessionSpec{AgentPluginId: "official/hello-world", AgentPluginVersion: "1.0.0", CheckoutExecutionId: "checkout-1", GitIdentity: &controlpb.GitIdentity{Name: "Ada", Email: "ada@example.invalid"}, InitialTurn: &controlpb.UserTurn{TurnId: "turn-1", Content: []*controlpb.ContentBlock{{Block: &controlpb.ContentBlock_Text{Text: &controlpb.TextContent{Text: "fix the bug"}}}}}}}}})
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	_, err = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "reuse-session", Epoch: f.controller.Epoch, OperationId: operation, ExecutionId: execution, NodeId: node, Input: cloneInputSpec(seedCloneInput(f.tid, uuid.NewString(), uuid.NewString(), f.commit))})
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
}

func TestRunTargetIncludesTheCurrentSandbox(t *testing.T) {
	f := setup(t)
	created := f.create("sandbox-scope")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	wid := f.readyRunWorkspace(t, run)
	_, node, _ := f.liveNode(t, wid)
	_, err := f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", sessionInput(), core.Object{"workspaceId": wid, "sandboxInstanceId": uuid.NewString(), "nodeId": node}, time.Time{})
	must(t, err)
	ctx := asController(f.client.Subject)
	claimed, err := f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, err)
	_, err = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "wrong-sandbox", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "wrong-sandbox", NodeId: node, Input: claimed.GetItem().GetInput()})
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
}

func TestPluginExecutionReceivesAFencedRuntimePermit(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	f.spaceProject(t, sid, "plugin-permit")
	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "permit-plugin", 200)
	op := f.claimPluginOp(t, "install_plugin")
	f.dispatchPlugin(t, op, "permit-execution")
	ctx := asController(f.client.Subject)
	client := controlpb.NewRuntimeControlServiceClient(f.controlConn)
	bindings, err := client.ListBindings(ctx, &controlpb.ListBindingsRequest{Epoch: f.controller.Epoch})
	must(t, err)
	if len(bindings.GetBindings()) != 1 {
		t.Fatalf("bindings = %v", bindings.GetBindings())
	}
	b := bindings.GetBindings()[0]
	req := &controlpb.GetExecutionPermitRequest{Epoch: f.controller.Epoch, WorkspaceId: b.GetWorkspaceId(), NodeInstanceId: b.GetNodeInstanceId(), ControlEpoch: b.GetControlEpoch(), ExecutionId: "permit-execution"}
	permit, err := client.GetExecutionPermit(ctx, req)
	must(t, err)
	if permit.GetBinding().GetOperationId() != op.S("id") || permit.GetBinding().GetExecutionId() != "permit-execution" {
		t.Fatal("permit lost the execution scope", permit)
	}
	req.ControlEpoch--
	_, err = client.GetExecutionPermit(ctx, req)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	req.ControlEpoch++
	must(t, f.reportPlugin(t, op, "permit-execution", []*controlpb.PluginItemResult{{PluginId: "official/hello-world", Outcome: &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: "1.0.0"}}}}))
	_, err = client.GetExecutionPermit(ctx, req)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
}

func TestSimulatorRelaysEchoTurnsAcrossControllerRestart(t *testing.T) {
	f := setup(t)
	created := f.create("echo-loop")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	wid := f.readyRunWorkspace(t, run)
	sandbox, node, _ := f.liveNode(t, wid)
	_, err := f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", sessionInput(), core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, err)
	f.drain()
	if f.scalar("SELECT count(*) FROM node_event_receipts") != 1 {
		t.Fatal("simulator did not take over the initial echo")
	}
	_, err = f.store.EnqueueThreadCommand(t.Context(), run, "submit_user_turn", core.Object{"turnId": "turn-2", "content": []core.Object{{"text": "continue"}}})
	must(t, err)
	// A new Controller and reopened Node journal must resume the same execution and event sequence.
	echo, err := simulator.NewAgentNode(filepath.Join(f.root, "agent-node"))
	must(t, err)
	f.controller = &simulator.Controller{Client: f.client, SubstrateURL: f.external.URL, Executions: f.executions, AgentRuns: controlpb.NewAgentRunServiceClient(f.controlConn), AgentNode: echo, Epoch: f.controller.Epoch}
	f.drain()
	if f.scalar("SELECT count(*) FROM node_executions WHERE operation_id=$1", run) != 1 || f.scalar("SELECT count(*) FROM node_event_receipts") != 2 {
		t.Fatal("echo recovery duplicated the execution or turn")
	}
	_, err = f.store.EnqueueThreadCommand(t.Context(), run, "end_session", core.Object{"reason": "user_ended"})
	must(t, err)
	f.drain()
	if f.scalar("SELECT count(*) FROM node_executions WHERE operation_id=$1 AND result->>'outcome'='agent_session_ended'", run) != 1 || f.scalar("SELECT count(*) FROM thread_commands WHERE delivered_at IS NULL") != 0 {
		t.Fatal("echo end command did not settle")
	}
}

// A run keeps its own maintenance epoch after initialization, outside the Project operation slot.
func TestRunExecutionPermitRetainsScopeAndChecksCurrentPermission(t *testing.T) {
	f := setup(t)
	run, execution, _, _ := f.registeredSession(t, "run-permit")
	var wid string
	must(t, f.store.Pool.QueryRow("SELECT id::text FROM workspaces WHERE issue_run_id=$1", run).Scan(&wid))
	ctx := asController(f.client.Subject)
	client := controlpb.NewRuntimeControlServiceClient(f.controlConn)
	bindings, err := client.ListBindings(ctx, &controlpb.ListBindingsRequest{Epoch: f.controller.Epoch})
	must(t, err)
	var binding *controlpb.RuntimeBinding
	for _, b := range bindings.GetBindings() {
		if b.GetWorkspaceId() == wid {
			binding = b
		}
	}
	if binding == nil || binding.GetOperationId() != run || f.scalar("SELECT count(*) FROM operations WHERE workspace_id=$1 AND state IN ('queued','running','blocked','retry_wait')", wid) != 0 {
		t.Fatal("run was not independently reserved", binding)
	}
	req := &controlpb.GetExecutionPermitRequest{Epoch: f.controller.Epoch, WorkspaceId: wid, NodeInstanceId: binding.GetNodeInstanceId(), ControlEpoch: binding.GetControlEpoch(), ExecutionId: execution}
	permit, err := client.GetExecutionPermit(ctx, req)
	must(t, err)
	if permit.GetBinding().GetOperationId() != run || permit.GetBinding().GetExecutionId() != execution {
		t.Fatal("run permit lost its execution scope", permit)
	}
	req.ControlEpoch--
	_, err = client.GetExecutionPermit(ctx, req)
	expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	req.ControlEpoch++
	_, err = f.store.Pool.Exec("UPDATE tenants SET status='disabled' WHERE id=$1", f.tid)
	must(t, err)
	_, err = client.GetExecutionPermit(ctx, req)
	if err == nil {
		t.Fatal("revoked actor retained a run execution permit")
	}
}

// Commands accepted together must preserve insertion order, independent of UUID sort order.
func TestThreadCommandsPreserveOrderWithinOneBusinessTransaction(t *testing.T) {
	f := setup(t)
	run, _, _, _ := f.registeredSession(t, "command-order")
	tx, err := f.store.Pool.BeginTx(t.Context(), nil)
	must(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	ids := []string{"ffffffff-ffff-4fff-8fff-ffffffffffff", "00000000-0000-4000-8000-000000000001"}
	for _, id := range ids {
		_, err = tx.ExecContext(t.Context(), "INSERT INTO thread_commands(id,run_id,kind,body) VALUES($1,$2,'end_session','{\"reason\":\"user_ended\"}')", id, run)
		must(t, err)
	}
	must(t, tx.Commit())
	commands, err := controlpb.NewAgentRunServiceClient(f.controlConn).ClaimThreadCommands(asController(f.client.Subject), &controlpb.ClaimThreadCommandsRequest{Epoch: f.controller.Epoch, Limit: 10})
	must(t, err)
	if len(commands.GetCommands()) != 2 || commands.GetCommands()[0].GetCommandId() != ids[0] || commands.GetCommands()[1].GetCommandId() != ids[1] {
		t.Fatal("transactional command order was reversed", commands)
	}
}

func TestSimulatorServicesEndCommandsWhileRunDeletionIsQueued(t *testing.T) {
	f := setup(t)
	run, execution, _, _ := f.registeredSession(t, "delete-live-echo")
	deleted, err := f.store.DeleteRunWorkspace(t.Context(), run)
	must(t, err)
	_, err = f.store.EnqueueThreadCommand(t.Context(), run, "end_session", core.Object{"reason": "user_ended"})
	must(t, err)
	// The first transition must service the command even though deletion is already queued.
	// Its quiesce still needs the explicit simulated Node binding acknowledgement below.
	_, err = f.controller.Step(t.Context())
	if err != nil && !strings.Contains(err.Error(), "runtime_input_closure_unconfirmed") {
		t.Fatal(err)
	}
	if f.scalar("SELECT count(*) FROM node_executions WHERE execution_id=$1 AND result IS NOT NULL", execution) != 1 {
		t.Fatal("queued deletion starved the end command")
	}
	f.drain()
	if f.scalar("SELECT count(*) FROM node_executions WHERE execution_id=$1 AND result IS NOT NULL", execution) != 1 || f.scalar("SELECT count(*) FROM workspaces WHERE issue_run_id=$1 AND deleted_at IS NOT NULL", run) != 1 {
		t.Fatal("queued deletion starved the end command")
	}
	again, err := f.store.DeleteRunWorkspace(t.Context(), run)
	must(t, err)
	if again.O("workspace").S("id") != deleted.O("workspace").S("id") || f.scalar("SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='delete_workspace'", deleted.O("workspace").S("id")) != 1 {
		t.Fatal("repeated run deletion created another operation")
	}
}

func TestRunWorkspaceUsesTheTriggerActorAndRequiresEvidence(t *testing.T) {
	f := setup(t)
	created := f.create("trigger-identity")
	f.drain()
	_, actor := f.addUser(t, "trigger-user", "Trigger")
	run := f.insertAgentRunFor(t, created.O("resource").S("id"), actor)
	workspace, err := f.store.CreateRunWorkspace(t.Context(), run)
	must(t, err)
	if workspace.O("workspace").S("creatorUserId") != actor || workspace.O("operation").S("actorUserId") != actor || workspace.O("workspace").S("ownerUserId") != f.uid {
		t.Fatal("Issue creator or durable Project owner replaced the trigger actor", workspace)
	}
	other := f.insertAgentRun(t, created.O("resource").S("id"))
	_, err = f.store.Pool.Exec("DELETE FROM issue_activities WHERE details->>'runId'=$1", other)
	must(t, err)
	// Release the Project operation slot without claiming success for the first Workspace.
	_, err = f.store.Pool.Exec("UPDATE operations SET state='failed' WHERE id=$1", workspace.O("operation").S("id"))
	must(t, err)
	_, err = f.store.CreateRunWorkspace(t.Context(), other)
	if err == nil || f.scalar("SELECT count(*) FROM workspaces WHERE issue_run_id=$1", other) != 0 {
		t.Fatal("missing trigger evidence silently inferred an actor")
	}
}
