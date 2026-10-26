package integration

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/objectstore"
)

func (f *fixture) insertAgentRun(t *testing.T, projectID string) string {
	t.Helper()
	return f.insertAgentRunFor(t, projectID, f.uid)
}

func (f *fixture) insertAgentRunFor(t *testing.T, projectID, actor string) string {
	t.Helper()
	issue, run := uuid.NewString(), uuid.NewString()
	_, e := f.store.Pool.Exec(`INSERT INTO issues(id,tenant_id,creator_user_id,title,number,project_ref) VALUES($1,$2,$3,'Agent task',(SELECT COALESCE(max(number),0)+1 FROM issues WHERE tenant_id=$2),$4)`, issue, f.tid, f.uid, projectID)
	must(t, e)
	_, e = f.store.Pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id) VALUES($1,$2,$3,'agent',$4)`, run, f.tid, issue, uuid.NewString())
	must(t, e)
	_, e = f.store.Pool.Exec(`INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details) VALUES($1,$2,$3,1,'user',$4,'run.enqueued',jsonb_build_object('runId',$5::text))`, uuid.NewString(), f.tid, issue, actor, run)
	must(t, e)
	return run
}

func (f *fixture) liveNode(t *testing.T, wid string) (sandboxID, nodeID, incarnation string) {
	t.Helper()
	must(t, f.store.Pool.QueryRow(`SELECT s.id::text,n.node_id,n.node_incarnation_id FROM sandbox_instances s JOIN node_instances n ON n.sandbox_instance_id=s.id WHERE s.workspace_id=$1 AND s.terminated_at IS NULL AND n.ended_at IS NULL`, wid).Scan(&sandboxID, &nodeID, &incarnation))
	return sandboxID, nodeID, incarnation
}

func sessionInput() core.Object {
	return core.Object{
		"kind": "agent_session", "agentPluginId": "official/hello-world", "agentPluginVersion": "1.0.0",
		"checkoutExecutionId": "checkout-1",
		"gitIdentity":         core.Object{"name": "Ada", "email": "ada@example.invalid"},
		"initialTurn":         core.Object{"turnId": "turn-1", "content": []core.Object{{"text": "fix the bug"}}},
	}
}

func (f *fixture) readyRunWorkspace(t *testing.T, run string) string {
	t.Helper()
	created, err := f.store.CreateRunWorkspace(t.Context(), run)
	must(t, err)
	if created.B("busy") {
		t.Fatal("run Project is busy")
	}
	f.drain()
	f.acknowledgeSimulatorBindings()
	return created.O("workspace").S("id")
}

// Session and delivery work is registered only on the Node Cloud chose, and a second execution conflicts.
func TestSessionWorkIsRegisteredOnTheCloudChosenNode(t *testing.T) {
	f := setup(t)
	created := f.create("agent-work")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	wid := f.readyRunWorkspace(t, run)
	sandbox, node, incarnation := f.liveNode(t, wid)
	work, e := f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", sessionInput(), core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, e)
	if work.S("runId") != run {
		t.Fatalf("work = %v", work)
	}
	ctx := asController(f.client.Subject)
	claimed, e := f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, e)
	item := claimed.GetItem()
	if item.GetOperationId() != run || item.GetTarget().GetNodeId() != node || item.GetInput().GetAgentSession().GetInitialTurn().GetTurnId() != "turn-1" {
		t.Fatalf("work item = %v", item)
	}
	wrong := proto.Clone(item.GetInput()).(*controlpb.ExecutionInput)
	_, e = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "bad-node", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", NodeId: "other-node", Input: wrong})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	dispatched, e := f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "session-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", NodeId: node, Input: item.GetInput()})
	must(t, e)
	again, e := f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "session-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", NodeId: node, Input: item.GetInput()})
	must(t, e)
	if again.GetRecord().GetExecutionId() != dispatched.GetRecord().GetExecutionId() {
		t.Fatal("replay changed the execution")
	}
	altered := proto.Clone(item.GetInput()).(*controlpb.ExecutionInput)
	altered.GetAgentSession().AgentPluginVersion = "9.9.9"
	_, e = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "session-other", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-2", NodeId: node, Input: altered})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	pending, e := f.executions.ListPendingDispatches(ctx, &controlpb.ListPendingDispatchesRequest{NodeId: node})
	must(t, e)
	found := false
	for _, record := range pending.GetRecords() {
		if record.GetExecutionId() == "session-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pending = %v", pending.GetRecords())
	}
	empty, e := f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, e)
	if empty.GetItem() != nil {
		t.Fatalf("registered work was offered again: %v", empty.GetItem())
	}
	_ = incarnation
}

// Thread events are stored in order, a gap stores nothing, and the terminal event waits for them.
func TestThreadEventsAreTakenOverInOrder(t *testing.T) {
	f := setup(t)
	created := f.create("thread-events")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	wid := f.readyRunWorkspace(t, run)
	sandbox, node, incarnation := f.liveNode(t, wid)
	_, e := f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", sessionInput(), core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, e)
	ctx := asController(f.client.Subject)
	claimed, e := f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, e)
	_, e = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "thread-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", NodeId: node, Input: claimed.GetItem().GetInput()})
	must(t, e)
	var mu sync.Mutex
	var fresh []int64
	f.store.OnThreadEvents = func(_ context.Context, _ *sql.Tx, _, _ string, events []core.Object) error {
		mu.Lock()
		for _, ev := range events {
			fresh = append(fresh, ev.N("sequence"))
		}
		mu.Unlock()
		return nil
	}
	runs := controlpb.NewAgentRunServiceClient(f.controlConn)
	event := func(seq uint64, record string) *controlpb.ThreadEvent {
		return &controlpb.ThreadEvent{Sequence: seq, Record: record}
	}
	take := func(id string, events ...*controlpb.ThreadEvent) error {
		_, err := runs.TakeOverThreadEvents(ctx, &controlpb.TakeOverThreadEventsRequest{SubmissionId: id, Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", Events: events})
		return err
	}
	must(t, take("batch-1", event(1, `{"kind":"one"}`)))
	if err := take("batch-gap", event(2, `{"kind":"two"}`), event(4, `{"kind":"four"}`)); err == nil {
		t.Fatal("a gap was stored")
	} else {
		expectStatus(t, err, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	}
	if f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id='session-1'") != 1 {
		t.Fatal("rejected batch wrote a receipt")
	}
	must(t, take("batch-2", event(2, `{"kind":"two"}`)))
	must(t, take("batch-1", event(1, `{"kind":"one"}`)))
	ended := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED}}}
	_, e = f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "end-early", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", Sequence: 4, Result: ended, Event: []byte("end")})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	must(t, take("batch-3", event(3, `{"kind":"three"}`)))
	_, e = f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "end", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", Sequence: 4, Result: ended, Event: []byte("end")})
	must(t, e)
	_, e = f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "end", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", Sequence: 4, Result: ended, Event: []byte("end")})
	must(t, e)
	if len(fresh) != 3 || fresh[0] != 1 || fresh[1] != 2 || fresh[2] != 3 {
		t.Fatalf("hook sequences = %v", fresh)
	}
}

// Commands stay claimable until delivery is recorded, and upload grants are not stored.
func TestThreadCommandsAndUploadGrantsAreNotLostOrStored(t *testing.T) {
	f := setup(t)
	created := f.create("thread-commands")
	f.drain()
	run := f.insertAgentRun(t, created.O("resource").S("id"))
	wid := f.readyRunWorkspace(t, run)
	sandbox, node, incarnation := f.liveNode(t, wid)
	_, e := f.store.EnqueueThreadCommand(t.Context(), run, "submit_user_turn", core.Object{"turnId": "turn-2", "content": []core.Object{{"text": "continue"}}})
	must(t, e)
	ctx := asController(f.client.Subject)
	runs := controlpb.NewAgentRunServiceClient(f.controlConn)
	early, e := runs.ClaimThreadCommands(ctx, &controlpb.ClaimThreadCommandsRequest{Epoch: f.controller.Epoch, Limit: 10})
	must(t, e)
	if len(early.GetCommands()) != 0 {
		t.Fatal("command was listed before the session execution existed")
	}
	_, e = f.store.EnqueueExecutionWork(t.Context(), run, "agent_session", sessionInput(), core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, e)
	claimed, e := f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, e)
	_, e = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "cmd-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", NodeId: node, Input: claimed.GetItem().GetInput()})
	must(t, e)
	listed, e := runs.ClaimThreadCommands(ctx, &controlpb.ClaimThreadCommandsRequest{Epoch: f.controller.Epoch, Limit: 10})
	must(t, e)
	if len(listed.GetCommands()) != 1 || listed.GetCommands()[0].GetSubmitUserTurn().GetTurn().GetTurnId() != "turn-2" {
		t.Fatalf("commands = %v", listed.GetCommands())
	}
	again, e := runs.ClaimThreadCommands(ctx, &controlpb.ClaimThreadCommandsRequest{Epoch: f.controller.Epoch, Limit: 10})
	must(t, e)
	if again.GetCommands()[0].GetCommandId() != listed.GetCommands()[0].GetCommandId() {
		t.Fatal("unrecorded command disappeared")
	}
	commandID := listed.GetCommands()[0].GetCommandId()
	_, e = runs.RecordThreadCommandDelivered(ctx, &controlpb.RecordThreadCommandDeliveredRequest{SubmissionId: "delivered", Epoch: f.controller.Epoch, CommandId: commandID, ExecutionId: "session-1"})
	must(t, e)
	_, e = runs.RecordThreadCommandDelivered(ctx, &controlpb.RecordThreadCommandDeliveredRequest{SubmissionId: "delivered", Epoch: f.controller.Epoch, CommandId: commandID, ExecutionId: "session-1"})
	must(t, e)
	done, e := runs.ClaimThreadCommands(ctx, &controlpb.ClaimThreadCommandsRequest{Epoch: f.controller.Epoch, Limit: 10})
	must(t, e)
	if len(done.GetCommands()) != 0 {
		t.Fatal("delivered command was listed again")
	}

	ended := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED}}}
	_, e = f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "end-session", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "session-1", Sequence: 1, Result: ended, Event: []byte("end")})
	must(t, e)
	f.store.ObjectStore = &objectstore.Config{Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "revisions", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret"}
	delivery := core.Object{"kind": "deliver_revision", "sessionExecutionId": "session-1", "checkoutExecutionId": f.checkoutOf(wid), "baseCommit": f.commit, "revisionRef": "refs/ora/revisions/1", "historyKey": "revisions/t/r/w/session.jsonl"}
	_, e = f.store.EnqueueExecutionWork(t.Context(), run, "deliver_revision", delivery, core.Object{"workspaceId": wid, "sandboxInstanceId": sandbox, "nodeId": node}, time.Time{})
	must(t, e)
	claimed, e = f.executions.ClaimWork(ctx, &controlpb.ClaimWorkRequest{Epoch: f.controller.Epoch})
	must(t, e)
	_, e = f.executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "delivery-dispatch", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "delivery-1", NodeId: node, Input: claimed.GetItem().GetInput()})
	must(t, e)
	granted, e := runs.GrantRevisionUpload(ctx, &controlpb.GrantRevisionUploadRequest{Epoch: f.controller.Epoch, ExecutionId: "delivery-1"})
	must(t, e)
	if len(granted.GetGrants()) != 1 || !strings.Contains(granted.GetGrants()[0].GetUrl(), "X-Amz-Signature=") {
		t.Fatalf("grants = %v", granted.GetGrants())
	}
	if stored := storedGrantCount(t, f); stored != 0 {
		t.Fatalf("upload grant was stored in %d columns", stored)
	}
	unverified := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, Outcome: &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{FinalCommit: f.commit, BaseCommit: f.commit, RevisionRef: "refs/ora/revisions/1", History: &controlpb.StoredObject{Key: delivery.S("historyKey"), Sha256: strings.Repeat("a", 64), Size: 1}}}}
	_, e = f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "unverified-delivery", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "delivery-1", Sequence: 1, Result: unverified, Event: []byte("unverified")})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if f.scalar("SELECT count(*) FROM node_event_receipts WHERE execution_id='delivery-1'") != 0 || f.scalar("SELECT count(*) FROM node_executions WHERE execution_id='delivery-1' AND result IS NOT NULL") != 0 {
		t.Fatal("unverified delivery became ACKable")
	}
	failed := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, Outcome: &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{Reason: controlpb.RevisionFailureReason_REVISION_FAILURE_REASON_UPLOAD_FAILED}}}
	_, e = f.executions.TakeOverNodeEvent(ctx, &controlpb.TakeOverNodeEventRequest{SubmissionId: "delivery-end", Epoch: f.controller.Epoch, OperationId: run, ExecutionId: "delivery-1", Sequence: 1, Result: failed, Event: []byte("fail")})
	must(t, e)
	_, e = runs.GrantRevisionUpload(ctx, &controlpb.GrantRevisionUploadRequest{Epoch: f.controller.Epoch, ExecutionId: "delivery-1"})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
}

// A run Workspace is created once, hidden from the public API, and opens admission only after plugins.
func TestRunWorkspaceAndPluginStep(t *testing.T) {
	f := setup(t)
	repo, _ := marketplaceFixture(t, f.root)
	f.syncMarketplace(t, repo)
	sid := f.defaultSpaceID()
	created := f.spaceProject(t, sid, "run-workspace")
	pid := created.O("resource").S("id")
	f.call("POST", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world"}, "install-agent", 200)
	f.completeNextPlugin(t, "install_plugin")
	var status string
	must(t, f.store.Pool.QueryRow(`SELECT status FROM space_agents WHERE space_id=$1 AND plugin_id='official/hello-world'`, sid).Scan(&status))
	if status != "active" {
		t.Fatalf("agent status = %s", status)
	}
	run := f.insertAgentRun(t, pid)
	first, e := f.store.CreateRunWorkspace(t.Context(), run)
	must(t, e)
	second, e := f.store.CreateRunWorkspace(t.Context(), run)
	must(t, e)
	if first.B("busy") || second.O("workspace").S("id") != first.O("workspace").S("id") || f.scalar("SELECT count(*) FROM workspaces WHERE issue_run_id=$1", run) != 1 {
		t.Fatalf("create = %v / %v", first, second)
	}
	if f.scalar("SELECT count(*) FROM operations WHERE workspace_id=$1 AND kind='create_workspace'", first.O("workspace").S("id")) != 1 {
		t.Fatal("create was not idempotent")
	}
	hidden := first.O("workspace").S("id")
	if code := f.call("GET", f.path("/workspaces/"+hidden), nil, "", 404).S("code"); code != "not_found" {
		t.Fatalf("hidden workspace code = %s", code)
	}
	for _, action := range []string{"start", "stop"} {
		f.call("POST", f.path("/workspaces/"+hidden+"/"+action), core.Object{"version": first.O("workspace").N("version")}, "hidden-"+action, 404)
	}
	f.call("DELETE", f.path("/workspaces/"+hidden), core.Object{"version": first.O("workspace").N("version")}, "hidden-delete", 404)
	listed := f.call("GET", f.path("/projects/"+pid+"/workspaces"), nil, "", 200)
	for _, item := range listed["items"].([]any) {
		if core.Object(item.(map[string]any)).S("id") == hidden {
			t.Fatal("run workspace was listed")
		}
	}
	busy, e := f.store.DeleteRunWorkspace(t.Context(), run)
	must(t, e)
	if !busy.B("busy") {
		t.Fatal("delete started while create was still queued")
	}
	f.drain()

	isolated := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Second", "baseRef": "main"}, "second-workspace", 202)
	oid, iwid := isolated.O("operation").S("id"), isolated.O("resource").S("id")
	at := f.stepTo(oid, "plugin")
	if f.ws(iwid).B("admissionOpen") {
		t.Fatal("admission opened before the plugin step")
	}
	op := f.internal("/internal/v1/operations/"+oid+"/snapshot", core.Object{"epoch": at.N("epoch"), "version": at.N("version")}, 200).O("operation")
	op["controllerEpoch"] = at.N("epoch")
	op["id"] = oid
	op["workspaceId"] = iwid
	op["kind"] = "create_workspace"
	extra := pluginInputProto(op)
	extra.GetInstallPlugins().Plugins = append(extra.GetInstallPlugins().Plugins, &controlpb.PluginInstall{PluginId: "official/native-tool", Version: "2.0.0"})
	_, e = f.executions.RecordDispatch(asController(f.client.Subject), &controlpb.RecordDispatchRequest{SubmissionId: "extra-plugin", Epoch: at.N("epoch"), OperationId: oid, ExecutionId: "plugin-extra", NodeId: f.liveNodeID(t, iwid), Input: extra})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	refused := f.internal("/internal/v1/operations/"+oid+"/effects", core.Object{"epoch": at.N("epoch"), "version": at.N("version"), "kind": "plugin_ensure", "workspaceId": iwid}, 409)
	if refused.S("code") != "invalid_step" {
		t.Fatalf("plugin effect = %v", refused)
	}
	f.dispatchPlugin(t, op, "plugin-1")
	must(t, f.reportPlugin(t, op, "plugin-1", []*controlpb.PluginItemResult{{
		PluginId: "official/hello-world",
		Outcome:  &controlpb.PluginItemResult_Failed{Failed: &controlpb.PluginItemFailed{Reason: controlpb.PluginFailureReason_PLUGIN_FAILURE_REASON_CHECKSUM_MISMATCH}},
	}}))
	f.controlStep(t, op, "/advance", core.Object{}, 200)
	if !f.ws(iwid).B("admissionOpen") || f.ws(iwid).S("observedState") != "ready" {
		t.Fatal("plugin item failure blocked readiness", f.ws(iwid))
	}
	var installError string
	must(t, f.store.Pool.QueryRow(`SELECT install_error FROM workspace_plugin_instances WHERE workspace_id=$1 AND identifier='hello-world'`, iwid).Scan(&installError))
	if installError != "checksum_mismatch" {
		t.Fatalf("install error = %s", installError)
	}
	must(t, f.store.Pool.QueryRow(`SELECT status FROM space_agents WHERE space_id=$1 AND plugin_id='official/hello-world'`, sid).Scan(&status))
	if status != "active" {
		t.Fatal("one workspace failure retired the agent")
	}
	current := f.spacePlugin(sid, "official/hello-world")
	f.call("DELETE", f.pluginSpacePath(sid)+"/plugins", core.Object{"identifier": "official/hello-world", "version": current.N("version")}, "retire-agent", 200)
	must(t, f.store.Pool.QueryRow(`SELECT status FROM space_agents WHERE space_id=$1 AND plugin_id='official/hello-world'`, sid).Scan(&status))
	if status != "retired" {
		t.Fatalf("retired status = %s", status)
	}
}

func storedGrantCount(t *testing.T, f *fixture) int {
	t.Helper()
	rows, e := f.store.Pool.Query(`SELECT table_name, column_name FROM information_schema.columns WHERE table_schema=current_schema() AND data_type IN ('text','jsonb','character varying')`)
	must(t, e)
	defer rows.Close()
	total := 0
	for rows.Next() {
		var table, column string
		must(t, rows.Scan(&table, &column))
		if strings.Contains(table, `"`) || strings.Contains(column, `"`) {
			t.Fatalf("unexpected identifier %s.%s", table, column)
		}
		var n int
		must(t, f.store.Pool.QueryRow(`SELECT count(*) FROM "`+table+`" WHERE "`+column+`"::text LIKE '%X-Amz-Signature%'`).Scan(&n))
		total += n
	}
	must(t, rows.Err())
	return total
}

func (f *fixture) liveNodeID(t *testing.T, wid string) string {
	t.Helper()
	_, node, _ := f.liveNode(t, wid)
	return node
}
