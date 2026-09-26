package integration

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// A restarted Controller lists every sandbox it should hold a Node session with before any
// Workspace has a new operation: sandboxes whose ensure effect succeeded (with the reported NodeId
// and the Substrate identity even before the sandbox step advanced), with their unended Node
// incarnations. Sandboxes whose ensure has no evidence yet, that are being terminated, or that
// belong to an old generation are left out, the read changes no operation, and only the lease
// holder may make it.
//
// Evidence for specs test-cases/cloud/controller-integration/workspace-operations-and-node-reports.md
// #a-restarted-controller-lists-the-live-sandboxes-it-must-reconnect.
func TestGRPCListLiveSandboxesRebuildsSessionTargets(t *testing.T) {
	f := setup(t)
	c := newGRPCController(t, f)
	list := func() []*controlpb.LiveSandbox {
		t.Helper()
		out, e := c.operations.ListLiveSandboxes(c.ctx, &controlpb.ListLiveSandboxesRequest{Epoch: c.epoch})
		must(t, e)
		return out.GetSandboxes()
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("no sandbox exists yet: %v", got)
	}

	first := f.create("live-first").O("workspace").S("id")
	op := c.claim().GetOperation()
	op, ensure := c.effect(op, controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, first)
	nodeID := ensure.GetEvidence().GetSandboxEnsured().GetNodeId()
	// Ensured but not advanced: the Substrate identity comes from the effect.
	want := &controlpb.LiveSandbox{Sandbox: &controlpb.SandboxRecord{Id: ensure.GetId(), WorkspaceId: first, Generation: 1, SubstrateSandboxId: proto.String(ensure.GetExternalId()), ObservedState: "allocating"}, NodeId: nodeID}
	if got := list(); len(got) != 1 || !proto.Equal(got[0], want) {
		t.Fatalf("an ensured sandbox must be listed with its NodeId and Substrate identity:\n got %v\nwant %v", got, want)
	}
	op, e := c.advance(op)
	must(t, e)
	registered, e := c.register(ensure.GetId(), 1, nodeID, "inc-1")
	must(t, e)
	want.Sandbox.ObservedState = "starting"
	want.Nodes = []*controlpb.NodeRecord{registered}
	if got := list(); len(got) != 1 || !proto.Equal(got[0], want) {
		t.Fatalf("a listed sandbox must carry its unended Node incarnations:\n got %v\nwant %v", got, want)
	}
	// Park the first operation so the second Project's operation can be claimed.
	parked, e := c.operations.DeferOperation(c.ctx, &controlpb.DeferOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion(), State: controlpb.DeferState_DEFER_STATE_RETRY_WAIT, Reason: controlpb.DeferReason_DEFER_REASON_NODE_UNAVAILABLE, RetrySeconds: 3600})
	must(t, e)

	// A planned ensure without evidence has no NodeId to connect to.
	second := f.create("live-second").O("workspace").S("id")
	next := c.claim().GetOperation()
	if next.GetWorkspaceId() != second {
		t.Fatalf("expected the second Project's operation, got %v", next)
	}
	_, e = c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: next.GetId(), Version: next.GetVersion(), Kind: controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, WorkspaceId: second})
	must(t, e)
	if got := list(); len(got) != 1 || got[0].GetSandbox().GetId() != ensure.GetId() {
		t.Fatalf("a sandbox whose ensure has no evidence must not be listed: %v", got)
	}

	// Listing is a read: it neither claims nor moves any operation.
	list()
	if f.scalar("SELECT version FROM operations WHERE id=$1", op.GetId()) != int(parked.GetOperation().GetVersion()) {
		t.Fatal("listing live sandboxes changed an operation")
	}

	// Only the current lease may list.
	_, e = c.operations.ListLiveSandboxes(c.ctx, &controlpb.ListLiveSandboxesRequest{Epoch: c.epoch + 1})
	expectStatus(t, e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)

	// A sandbox being terminated, or of an old generation, has no session to rebuild.
	exec := func(q string) {
		t.Helper()
		_, err := f.store.Pool.Exec(q, ensure.GetId())
		must(t, err)
	}
	exec("UPDATE sandbox_instances SET observed_state='terminating' WHERE id=$1")
	if got := list(); len(got) != 0 {
		t.Fatalf("a terminating sandbox must not be listed: %v", got)
	}
	exec("UPDATE sandbox_instances SET observed_state='running' WHERE id=$1")
	exec("UPDATE workspaces SET runtime_generation=runtime_generation+1 WHERE id=(SELECT workspace_id FROM sandbox_instances WHERE id=$1)")
	if got := list(); len(got) != 0 {
		t.Fatalf("a sandbox of an old generation must not be listed: %v", got)
	}
}
