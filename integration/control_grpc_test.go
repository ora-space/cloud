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

// Watch is an accelerator: it needs the current epoch to open, delivers the hints Cloud publishes,
// and ends cleanly with Drain when Cloud shuts the hub. Which commits publish which hint is covered
// with the real producers (TestGRPCWatchDeliversOperationAvailable and the run work tests).
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
	run := bootstrap.S("tenantId")
	h.store.Signals.Publish(core.ControlSignal{Kind: core.SignalWorkAvailable, OperationID: run})
	msg, e := stream.Recv()
	must(t, e)
	if msg.GetWorkAvailable() == nil || msg.GetWorkAvailable().GetOperationId() != run {
		t.Fatalf("expected WorkAvailable for %s, got %v", run, msg)
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
