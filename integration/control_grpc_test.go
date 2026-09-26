package integration

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/wanglongan587/cloud/internal/controlgrpc"
	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// controlHarness serves the gRPC control surface over bufconn against an isolated PostgreSQL schema.
type controlHarness struct {
	store *core.Store
	conn  *grpc.ClientConn
}

func newControlHarness(t *testing.T) *controlHarness {
	t.Helper()
	pool, _ := testSchema(t, "grpc_")
	db, e := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, e)
	store, e := core.NewStore(db)
	must(t, e)
	must(t, store.Migrate(context.Background()))
	return &controlHarness{store: store, conn: controlConn(t, store)}
}

// asController returns a context naming the calling Controller; the surface authenticates nobody at this stage.
func asController(controller string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), controlgrpc.HolderMetadata, controller)
}

// expectStatus asserts the status code and the ErrorDetail the contract promises together.
func expectStatus(t *testing.T, err error, code codes.Code, detail controlpb.ErrorCode) {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != code {
		t.Fatalf("status %s (%s), want %s", st.Code(), st.Message(), code)
	}
	got := controlpb.ErrorCode_ERROR_CODE_UNSPECIFIED
	for _, d := range st.Details() {
		if typed, ok := d.(*controlpb.ErrorDetail); ok {
			got = typed.GetCode()
		}
	}
	if got != detail {
		t.Fatalf("detail %s, want %s", got, detail)
	}
}

// The gRPC control surface requires the calling Controller to name itself and runs the same lease
// transaction as the JSON internal API: one holder, monotonic epochs, stale epochs fenced.
func TestControlGRPCLeaseHolderAndFencing(t *testing.T) {
	h := newControlHarness(t)
	client := controlpb.NewControllerLeaseServiceClient(h.conn)
	expect := func(err error, code codes.Code, detail controlpb.ErrorCode) {
		t.Helper()
		expectStatus(t, err, code, detail)
	}

	_, e := client.AcquireLease(context.Background(), &controlpb.AcquireLeaseRequest{})
	expect(e, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_UNSPECIFIED)
	_, e = client.AcquireLease(asController(" "), &controlpb.AcquireLeaseRequest{})
	expect(e, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_UNSPECIFIED)

	first, e := client.AcquireLease(asController("controller-a"), &controlpb.AcquireLeaseRequest{})
	must(t, e)
	if first.GetLease().GetHolderId() != "controller-a" || first.GetLease().GetEpoch() != 1 || first.GetLease().GetExpiresAt() == nil {
		t.Fatalf("unexpected first lease: %v", first.GetLease())
	}
	_, e = client.AcquireLease(asController("controller-b"), &controlpb.AcquireLeaseRequest{})
	expect(e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_LEASE_HELD)
	_, e = client.RenewLease(asController("controller-b"), &controlpb.RenewLeaseRequest{Epoch: 1})
	expect(e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)
	_, e = client.RenewLease(asController("controller-a"), &controlpb.RenewLeaseRequest{Epoch: 7})
	expect(e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)
	renewed, e := client.RenewLease(asController("controller-a"), &controlpb.RenewLeaseRequest{Epoch: 1})
	must(t, e)
	if renewed.GetLease().GetEpoch() != 1 || !renewed.GetLease().GetExpiresAt().AsTime().After(first.GetLease().GetExpiresAt().AsTime()) {
		t.Fatalf("renewal must keep the epoch and extend expiry: %v -> %v", first.GetLease(), renewed.GetLease())
	}
	released, e := client.ReleaseLease(asController("controller-a"), &controlpb.ReleaseLeaseRequest{Epoch: 1})
	must(t, e)
	if released.GetLease().GetEpoch() != 1 {
		t.Fatalf("release changed the epoch: %v", released.GetLease())
	}
	second, e := client.AcquireLease(asController("controller-b"), &controlpb.AcquireLeaseRequest{})
	must(t, e)
	if second.GetLease().GetHolderId() != "controller-b" || second.GetLease().GetEpoch() != 2 {
		t.Fatalf("hand-over must bump the epoch: %v", second.GetLease())
	}
	_, e = client.RenewLease(asController("controller-a"), &controlpb.RenewLeaseRequest{Epoch: 1})
	expect(e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)
}

// The clone loop over the contract: Cloud-accepted work is claimed, registered before dispatch,
// taken over with its exact receipt, and every write replays under its submission identity.
func TestControlGRPCCloneLoopWithSubmissionReplay(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	bootstrap, e := h.store.Bootstrap(ctx, "Clone tenant", "corp", "alice", "Alice")
	must(t, e)
	tid, uid := bootstrap.S("tenantId"), bootstrap.S("userId")
	lease := controlpb.NewControllerLeaseServiceClient(h.conn)
	client := controlpb.NewExecutionServiceClient(h.conn)
	holder := asController("controller-a")
	acquired, e := lease.AcquireLease(holder, &controlpb.AcquireLeaseRequest{})
	must(t, e)
	epoch := acquired.GetLease().GetEpoch()

	// Nothing queued yet; claiming is a read, never an error.
	empty, e := client.ClaimWork(holder, &controlpb.ClaimWorkRequest{SubmissionId: "s-claim-0", Epoch: epoch})
	must(t, e)
	if empty.GetItem() != nil {
		t.Fatalf("claimed work from an empty queue: %v", empty.GetItem())
	}
	request, e := h.store.EnqueueClone(ctx, tid, uid, "browser-request", "https://example.invalid/repo.git", "main")
	must(t, e)
	again, e := h.store.EnqueueClone(ctx, tid, uid, "browser-request", "https://example.invalid/repo.git", "main")
	must(t, e)
	if again.S("id") != request.S("id") {
		t.Fatalf("repeated request must return the original: %v vs %v", again, request)
	}
	if _, e = h.store.EnqueueClone(ctx, tid, uid, "browser-request", "https://example.invalid/repo.git", "other"); core.ErrorCode(e).Code != "idempotency_conflict" {
		t.Fatalf("changed input under the same request id must conflict: %v", e)
	}
	claimed, e := client.ClaimWork(holder, &controlpb.ClaimWorkRequest{SubmissionId: "s-claim-1", Epoch: epoch})
	must(t, e)
	if claimed.GetItem().GetOperationId() != request.S("id") || claimed.GetItem().GetInput().GetClone().GetBranch() != "main" {
		t.Fatalf("unexpected work item: %v", claimed.GetItem())
	}
	// A stale epoch cannot claim or write.
	_, e = client.ClaimWork(holder, &controlpb.ClaimWorkRequest{SubmissionId: "s-claim-2", Epoch: epoch + 1})
	expectStatus(t, e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)

	dispatch := &controlpb.RecordDispatchRequest{SubmissionId: "s-dispatch", Epoch: epoch, OperationId: request.S("id"), ExecutionId: "exec-1", NodeId: "node-a", Input: claimed.GetItem().GetInput()}
	first, e := client.RecordDispatch(holder, dispatch)
	must(t, e)
	if first.GetRecord().GetExecutionId() != "exec-1" || first.GetRecord().Result != nil {
		t.Fatalf("unexpected dispatch record: %v", first.GetRecord())
	}
	// Lost reply: same submission, same content replays the recorded response without a second effect.
	replay, e := client.RecordDispatch(holder, dispatch)
	must(t, e)
	if replay.GetRecord().GetExecutionId() != "exec-1" {
		t.Fatalf("replay must return the original record: %v", replay.GetRecord())
	}
	// Same submission identity with different content is a conflict, and the original stands.
	altered, _ := proto.Clone(dispatch).(*controlpb.RecordDispatchRequest)
	altered.ExecutionId = "exec-2"
	_, e = client.RecordDispatch(holder, altered)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	// A second execution for the same request is refused even under a new submission.
	altered.SubmissionId = "s-dispatch-2"
	_, e = client.RecordDispatch(holder, altered)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	// Once dispatched the request is no longer claimable.
	drained, e := client.ClaimWork(holder, &controlpb.ClaimWorkRequest{SubmissionId: "s-claim-3", Epoch: epoch})
	must(t, e)
	if drained.GetItem() != nil {
		t.Fatalf("dispatched request was offered again: %v", drained.GetItem())
	}
	_, e = client.ListPendingDispatches(context.Background(), &controlpb.ListPendingDispatchesRequest{NodeId: "node-a"})
	expectStatus(t, e, codes.InvalidArgument, controlpb.ErrorCode_ERROR_CODE_UNSPECIFIED)
	pending, e := client.ListPendingDispatches(asController("controller-b"), &controlpb.ListPendingDispatchesRequest{NodeId: "node-a"})
	must(t, e)
	if len(pending.GetRecords()) != 1 || pending.GetRecords()[0].GetExecutionId() != "exec-1" {
		t.Fatalf("recovery read must see the pending execution without a lease: %v", pending.GetRecords())
	}

	node := &controlpb.NodeIdentity{NodeId: "node-a", NodeIncarnationId: "incarnation-1"}
	ready := &controlpb.ExecutionResult{Node: node, Outcome: &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: "/node/checkout", Commit: "0123456789abcdef0123456789abcdef01234567"}}}
	takeover := &controlpb.TakeOverNodeEventRequest{SubmissionId: "s-takeover", Epoch: epoch, OperationId: request.S("id"), ExecutionId: "exec-1", Sequence: 1, Result: ready, Event: []byte("event-1")}
	taken, e := client.TakeOverNodeEvent(holder, takeover)
	must(t, e)
	if taken.GetRecord().GetResult().GetCloneReady().GetCommit() != ready.GetCloneReady().GetCommit() {
		t.Fatalf("takeover did not record the result: %v", taken.GetRecord())
	}
	replayed, e := client.TakeOverNodeEvent(holder, takeover)
	must(t, e)
	if replayed.GetRecord().GetResult().GetCloneReady().GetPath() != "/node/checkout" {
		t.Fatalf("takeover replay must return the original record: %v", replayed.GetRecord())
	}
	// A replayed event with the same sequence but different content must not be acknowledged.
	forged, _ := proto.Clone(takeover).(*controlpb.TakeOverNodeEventRequest)
	forged.SubmissionId, forged.Event = "s-takeover-forged", []byte("event-1-forged")
	_, e = client.TakeOverNodeEvent(holder, forged)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	// A conflicting fact for the same execution leaves the original untouched.
	failed := &controlpb.ExecutionResult{Node: node, Outcome: &controlpb.ExecutionResult_CloneFailed{CloneFailed: &controlpb.CloneFailed{Reason: controlpb.CloneFailureReason_CLONE_FAILURE_REASON_OPERATION_FAILED}}}
	_, e = client.RecordQueriedResult(holder, &controlpb.RecordQueriedResultRequest{SubmissionId: "s-queried-conflict", Epoch: epoch, OperationId: request.S("id"), ExecutionId: "exec-1", Result: failed})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	queried, e := client.RecordQueriedResult(holder, &controlpb.RecordQueriedResultRequest{SubmissionId: "s-queried", Epoch: epoch, OperationId: request.S("id"), ExecutionId: "exec-1", Result: ready})
	must(t, e)
	if queried.GetRecord().GetResult().GetCloneReady().GetCommit() != ready.GetCloneReady().GetCommit() {
		t.Fatalf("identical queried result must be idempotent: %v", queried.GetRecord())
	}
	got, e := client.GetDispatch(asController("controller-b"), &controlpb.GetDispatchRequest{ExecutionId: "exec-1"})
	must(t, e)
	if got.GetRecord().GetOperationId() != request.S("id") || got.GetRecord().GetResult() == nil {
		t.Fatalf("dispatch lookup lost the result: %v", got.GetRecord())
	}
	_, e = client.GetDispatch(holder, &controlpb.GetDispatchRequest{ExecutionId: "absent"})
	expectStatus(t, e, codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND)
	settled, e := client.ListPendingDispatches(holder, &controlpb.ListPendingDispatchesRequest{})
	must(t, e)
	if len(settled.GetRecords()) != 0 {
		t.Fatalf("completed execution still pending: %v", settled.GetRecords())
	}
	var state string
	var receipts int
	must(t, h.store.Pool.QueryRow("SELECT state FROM clone_requests WHERE id=$1", request.S("id")).Scan(&state))
	must(t, h.store.Pool.QueryRow("SELECT count(*) FROM clone_event_receipts WHERE execution_id='exec-1'").Scan(&receipts))
	if state != "succeeded" || receipts != 1 {
		t.Fatalf("request state %q with %d receipts; want succeeded with exactly one receipt", state, receipts)
	}
}

// Watch is an accelerator: it needs the current epoch to open, delivers WorkAvailable after a clone
// request commits, and ends cleanly with Drain when Cloud shuts the hub.
func TestControlGRPCWatchDeliversWorkAndDrain(t *testing.T) {
	h := newControlHarness(t)
	ctx := context.Background()
	bootstrap, e := h.store.Bootstrap(ctx, "Watch tenant", "corp", "alice", "Alice")
	must(t, e)
	lease := controlpb.NewControllerLeaseServiceClient(h.conn)
	signals := controlpb.NewControlSignalServiceClient(h.conn)
	holder := asController("controller-a")
	acquired, e := lease.AcquireLease(holder, &controlpb.AcquireLeaseRequest{})
	must(t, e)
	epoch := acquired.GetLease().GetEpoch()

	stale, e := signals.Watch(holder, &controlpb.WatchRequest{Epoch: epoch + 1})
	must(t, e)
	_, e = stale.Recv()
	expectStatus(t, e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)

	watchCtx, stop := context.WithTimeout(holder, 10*time.Second)
	defer stop()
	stream, e := signals.Watch(watchCtx, &controlpb.WatchRequest{Epoch: epoch})
	must(t, e)
	// The server sends headers once the subscription exists; nothing published after that is missed.
	_, e = stream.Header()
	must(t, e)
	request, e := h.store.EnqueueClone(ctx, bootstrap.S("tenantId"), bootstrap.S("userId"), "watched", "https://example.invalid/repo.git", "main")
	must(t, e)
	msg, e := stream.Recv()
	must(t, e)
	if msg.GetWorkAvailable() == nil || msg.GetWorkAvailable().GetOperationId() != request.S("id") {
		t.Fatalf("expected WorkAvailable for %s, got %v", request.S("id"), msg)
	}
	h.store.Signals.Drain()
	msg, e = stream.Recv()
	must(t, e)
	if msg.GetDrain() == nil {
		t.Fatalf("expected Drain, got %v", msg)
	}
	if _, e = stream.Recv(); !errors.Is(e, io.EOF) {
		t.Fatalf("stream must end cleanly after Drain, got %v", e)
	}
	late, e := signals.Watch(holder, &controlpb.WatchRequest{Epoch: epoch})
	must(t, e)
	_, e = late.Recv()
	expectStatus(t, e, codes.Unavailable, controlpb.ErrorCode_ERROR_CODE_UNSPECIFIED)
}
