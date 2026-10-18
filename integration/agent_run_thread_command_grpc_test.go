package integration

// The Phase 4C S3 Thread command path over the real internal gRPC surface (bufconn + real
// PostgreSQL): ClaimThreadCommands → RecordThreadCommandDelivered → ClaimThreadCommands. The core
// DB tests prove the control-plane decisions; this proves the wire contract a Controller actually
// speaks — the lease fencing, the command oneof, the target Node, and that a registered delivery
// removes the command from the backlog.
//
// The command row is seeded at the storage boundary rather than through EnqueueThreadCommand,
// because the A→B seam takes an unexported transaction the business layer owns; the seam itself is
// driven by the core DB tests. What is under test here is everything above it.

import (
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// threadCommandScene is one agent run that a Controller can be handed commands for.
type threadCommandScene struct {
	runID       string
	tenantID    string
	workspaceID string
}

// seedThreadCommandRun declares the run only. It deliberately does not register an execution, so a
// test can observe the gating rule before and after RecordDispatch would have run.
func seedThreadCommandRun(t *testing.T, h *controlHarness) threadCommandScene {
	t.Helper()
	seed := seedAgentIssueRunSkeleton(t, h.store.Pool)
	return threadCommandScene{runID: seed.runID, tenantID: seed.tenantID, workspaceID: seed.runWorkspaceID}
}

// registerThreadCommandExecution records the run's agent_session execution the way RecordDispatch
// does, which is what makes the run's commands claimable.
func registerThreadCommandExecution(t *testing.T, h *controlHarness, s threadCommandScene, execution, node string, epoch int64) {
	t.Helper()
	workID := newTestID()
	_, e := h.store.Pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target)
		VALUES($1,$2,'agent_session','{}','{}')`, workID, s.runID)
	must(t, e)
	// RecordDispatch registers the work item and the execution together (D6); the work row's own
	// execution_id is what keeps a second un-registered work item per run impossible.
	_, e = h.store.Pool.Exec(`UPDATE execution_work SET execution_id=$1 WHERE id=$2`, execution, workID)
	must(t, e)
	_, e = h.store.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
		VALUES($1,'agent_session',$2,$3,$4,$5,$1,'{}',$6)`, execution, s.runID, workID, s.workspaceID, node, epoch)
	must(t, e)
	// A claimed command carries the Node identity Cloud would deliver it to, and that target is read
	// from the run Workspace's live runtime: the sandbox instance the execution's workspace currently
	// runs in, and the Node serving it. The scene therefore has to carry a provisioned runtime, not
	// only the execution row, or the claim would have no address to hand back.
	sandboxID, nodeRowID := newTestID(), newTestID()
	_, e = h.store.Pool.Exec(`UPDATE workspaces SET runtime_generation=1 WHERE id=$1`, s.workspaceID)
	must(t, e)
	_, e = h.store.Pool.Exec(`INSERT INTO sandbox_instances(id,workspace_id,generation,observed_state) VALUES($1,$2,1,'running')`, sandboxID, s.workspaceID)
	must(t, e)
	// node_id is the identity the Node service owns (text) and the row id is Cloud's own handle
	// (uuid), so the identity is bound once as its own parameter.
	_, e = h.store.Pool.Exec(`INSERT INTO node_instances(id,workspace_id,sandbox_instance_id,service_subject,connection_state,protocol_version,initialized,node_id,node_incarnation_id)
		VALUES($1,$2,$3,'node','connected',1,true,$4,$5)`, nodeRowID, s.workspaceID, sandboxID, node, "inc-"+node)
	must(t, e)
}

// seedThreadCommand writes one command row the way the enqueue seam would.
func seedThreadCommand(t *testing.T, h *controlHarness, runID, kind, body string) string {
	t.Helper()
	id := newTestID()
	_, e := h.store.Pool.Exec(`INSERT INTO thread_commands(id,run_id,kind,body) VALUES($1,$2,$3,$4)`, id, runID, kind, body)
	must(t, e)
	return id
}

// TestControlGRPCThreadCommandDeliveryLoop drives the Controller-visible contract end to end.
func TestControlGRPCThreadCommandDeliveryLoop(t *testing.T) {
	h := newControlHarness(t)
	holder := asController("controller-a")
	acquired, e := controlpb.NewControllerLeaseServiceClient(h.conn).AcquireLease(holder, &controlpb.AcquireLeaseRequest{})
	must(t, e)
	epoch := acquired.GetLease().GetEpoch()
	client := controlpb.NewAgentRunServiceClient(h.conn)

	// A run whose session execution is not registered yet holds commands that must stay in Cloud: the
	// Controller may not deliver to an execution Cloud never registered.
	scene := seedThreadCommandRun(t, h)
	turnID := "22222222-2222-2222-2222-222222222222"
	// The durable body is the flat canonical shape the business layer writes and validateThreadCommand
	// enforces: the turn id and the content blocks, not a nested turn object.
	commandID := seedThreadCommand(t, h, scene.runID, "submit_user_turn",
		`{"turnId":"`+turnID+`","content":[{"text":"are you there?"}]}`)
	endID := seedThreadCommand(t, h, scene.runID, "end_session", `{"reason":"user_ended"}`)
	early, e := client.ClaimThreadCommands(holder, &controlpb.ClaimThreadCommandsRequest{Epoch: epoch, Limit: 100})
	must(t, e)
	if len(early.GetCommands()) != 0 {
		t.Fatalf("a run without a registered session execution must yield no commands, got %v", early.GetCommands())
	}

	// RecordDispatch registers the session execution; the same commands become claimable.
	execution, node := "exec-thread-1", "node-thread-1"
	registerThreadCommandExecution(t, h, scene, execution, node, epoch)
	runID := scene.runID
	claimed, e := client.ClaimThreadCommands(holder, &controlpb.ClaimThreadCommandsRequest{Epoch: epoch, Limit: 100})
	must(t, e)
	commands := claimed.GetCommands()
	if len(commands) != 2 {
		t.Fatalf("claim returned %d commands, want the run's 2", len(commands))
	}
	turn := commands[0]
	if turn.GetCommandId() != commandID || turn.GetRunId() != runID || turn.GetExecutionId() != execution {
		t.Fatalf("claimed identity = %v, want id=%s run=%s execution=%s", turn, commandID, runID, execution)
	}
	if turn.GetTarget().GetNodeId() != node {
		t.Fatalf("claimed target node = %q, want the registered %q", turn.GetTarget().GetNodeId(), node)
	}
	if got := turn.GetSubmitUserTurn().GetTurn(); got.GetTurnId() != turnID || got.GetContent()[0].GetText().GetText() != "are you there?" {
		t.Fatalf("claimed turn = %v, want the stored turn_id and text", got)
	}
	if commands[1].GetEndSession().GetReason() != controlpb.EndSessionReason_END_SESSION_REASON_USER_ENDED {
		t.Fatalf("end_session reason = %v, want user_ended", commands[1].GetEndSession().GetReason())
	}

	// Registration is a state change under the caller's submission identity: the first call records
	// the delivery and a replay of the same submission returns the same success without moving it.
	delivered := &controlpb.RecordThreadCommandDeliveredRequest{SubmissionId: "s-deliver-1", Epoch: epoch, CommandId: commandID, ExecutionId: execution}
	if _, e = client.RecordThreadCommandDelivered(holder, delivered); e != nil {
		t.Fatalf("register the delivery: %v", e)
	}
	if _, e = client.RecordThreadCommandDelivered(holder, delivered); e != nil {
		t.Fatalf("a replayed submission must succeed: %v", e)
	}
	var recordedExecution string
	must(t, h.store.Pool.QueryRow(`SELECT delivered_execution_id FROM thread_commands WHERE id=$1`, commandID).Scan(&recordedExecution))
	if recordedExecution != execution {
		t.Fatalf("recorded execution = %q, want %q", recordedExecution, execution)
	}

	// A delivered command leaves the backlog; the still-undelivered one stays.
	remaining, e := client.ClaimThreadCommands(holder, &controlpb.ClaimThreadCommandsRequest{Epoch: epoch, Limit: 100})
	must(t, e)
	if len(remaining.GetCommands()) != 1 || remaining.GetCommands()[0].GetCommandId() != endID {
		t.Fatalf("a registered command must not be claimed again, got %v", remaining.GetCommands())
	}

	// A different execution of the same run can never take the registration over. In the merged
	// control plane that is structural as well as contractual: a run holds at most one in-flight
	// agent_session execution (one_pending_run_execution), so no competing execution can be
	// registered in the first place.
	otherWork := newTestID()
	_, e = h.store.Pool.Exec(`INSERT INTO execution_work(id,run_id,kind,input,target) VALUES($1,$2,'agent_session','{}','{}')`, otherWork, scene.runID)
	must(t, e)
	_, e = h.store.Pool.Exec(`INSERT INTO node_executions(execution_id,kind,operation_id,work_id,workspace_id,node_id,node_operation_id,input,dispatched_epoch)
		VALUES('exec-thread-2','agent_session',$1,$2,$3,$4,'exec-thread-2','{}',$5)`, scene.runID, otherWork, scene.workspaceID, node, epoch)
	wantPGError(t, e, "23505")
	// And an execution Cloud never registered can never take the delivered registration over either,
	// which is the answer a Controller gets when it guesses an execution identity.
	_, e = client.RecordThreadCommandDelivered(holder, &controlpb.RecordThreadCommandDeliveredRequest{
		SubmissionId: "s-deliver-2", Epoch: epoch, CommandId: commandID, ExecutionId: "exec-thread-2",
	})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// The lease fences the whole path: a stale epoch may neither claim nor register.
	_, e = client.ClaimThreadCommands(holder, &controlpb.ClaimThreadCommandsRequest{Epoch: epoch + 1, Limit: 100})
	expectStatus(t, e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)
	_, e = client.RecordThreadCommandDelivered(holder, &controlpb.RecordThreadCommandDeliveredRequest{
		SubmissionId: "s-deliver-3", Epoch: epoch + 1, CommandId: endID, ExecutionId: execution,
	})
	expectStatus(t, e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)

	// The claim bound is the contract's own 1..100.
	_, e = client.ClaimThreadCommands(holder, &controlpb.ClaimThreadCommandsRequest{Epoch: epoch, Limit: 0})
	expectStatus(t, e, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_INVALID_INPUT)
}

// TestControlGRPCThreadCommandRejectsUnknownDelivery pins the delivery registration's failure shape:
// an unknown command is a not-found and an execution Cloud never registered is a conflict, so a
// Controller can tell "already gone" from "wrong execution" without seeing any SQL.
func TestControlGRPCThreadCommandRejectsUnknownDelivery(t *testing.T) {
	h := newControlHarness(t)
	holder := asController("controller-a")
	acquired, e := controlpb.NewControllerLeaseServiceClient(h.conn).AcquireLease(holder, &controlpb.AcquireLeaseRequest{})
	must(t, e)
	epoch := acquired.GetLease().GetEpoch()
	client := controlpb.NewAgentRunServiceClient(h.conn)
	scene := seedThreadCommandRun(t, h)
	registerThreadCommandExecution(t, h, scene, "exec-thread-1", "node-thread-1", epoch)
	commandID := seedThreadCommand(t, h, scene.runID, "end_session", `{"reason":"cancelled"}`)

	_, e = client.RecordThreadCommandDelivered(holder, &controlpb.RecordThreadCommandDeliveredRequest{
		SubmissionId: "s-unknown-command", Epoch: epoch, CommandId: newTestID(), ExecutionId: "exec-thread-1",
	})
	expectStatus(t, e, codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND)
	_, e = client.RecordThreadCommandDelivered(holder, &controlpb.RecordThreadCommandDeliveredRequest{
		SubmissionId: "s-unknown-execution", Epoch: epoch, CommandId: commandID, ExecutionId: "exec-never-registered",
	})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	var delivered *string
	must(t, h.store.Pool.QueryRow(`SELECT delivered_at::text FROM thread_commands WHERE id=$1`, commandID).Scan(&delivered))
	if delivered != nil {
		t.Fatalf("a refused registration must not mark the command delivered: %v", *delivered)
	}
}

// newTestID is a local id generator so this file needs nothing from the core package.
func newTestID() string { return uuid.NewString() }
