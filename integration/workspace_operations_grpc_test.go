package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// grpcController is a test Controller that drives runtime Workspace operations only through the
// gRPC contract, against the simulator Substrate over HTTP: the path a Rust Controller takes.
type grpcController struct {
	t          *testing.T
	f          *fixture
	ctx        context.Context
	operations controlpb.WorkspaceOperationServiceClient
	nodes      controlpb.NodeReportServiceClient
	epoch      int64
	submission int
}

func newGRPCController(t *testing.T, f *fixture) *grpcController {
	t.Helper()
	ctx := asController(f.client.Subject)
	lease, e := controlpb.NewControllerLeaseServiceClient(f.controlConn).AcquireLease(ctx, &controlpb.AcquireLeaseRequest{})
	must(t, e)
	return &grpcController{t: t, f: f, ctx: ctx, operations: controlpb.NewWorkspaceOperationServiceClient(f.controlConn), nodes: controlpb.NewNodeReportServiceClient(f.controlConn), epoch: lease.GetLease().GetEpoch()}
}

func (c *grpcController) sub() string {
	c.submission++
	return fmt.Sprintf("submission-%d", c.submission)
}

func (c *grpcController) claim() *controlpb.OperationSnapshot {
	c.t.Helper()
	out, e := c.operations.ClaimOperation(c.ctx, &controlpb.ClaimOperationRequest{Epoch: c.epoch})
	must(c.t, e)
	if out.GetSnapshot() == nil {
		c.t.Fatal("no operation to claim")
	}
	return out.GetSnapshot()
}

func (c *grpcController) advance(op *controlpb.Operation) (*controlpb.Operation, error) {
	out, e := c.operations.AdvanceOperation(c.ctx, &controlpb.AdvanceOperationRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion()})
	return out.GetOperation(), e
}

// effect plans one effect, has the simulator Substrate perform it, and records its evidence.
func (c *grpcController) effect(op *controlpb.Operation, kind controlpb.EffectKind, workspaceID string) (*controlpb.Operation, *controlpb.Effect) {
	c.t.Helper()
	planned, e := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion(), Kind: kind, WorkspaceId: workspaceID})
	must(c.t, e)
	request := core.Object{"kind": name(kind), "projectId": op.GetProjectId(), "workspaceId": workspaceID}
	if terminate := planned.GetEffect().GetRequest().GetSandboxTerminate(); terminate != nil {
		request["sandboxInstanceId"] = terminate.GetSandboxInstanceId()
	}
	external := c.substrate(planned.GetEffect().GetId(), request)
	evidence := &controlpb.EffectEvidence{}
	result := external.O("result")
	switch kind {
	case controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE:
		evidence.Evidence = &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: result.S("sandboxInstanceId"), NodeId: result.S("nodeId")}}
	case controlpb.EffectKind_EFFECT_KIND_SANDBOX_TERMINATE:
		evidence.Evidence = &controlpb.EffectEvidence_SandboxTerminated{SandboxTerminated: &controlpb.SandboxTerminated{}}
	case controlpb.EffectKind_EFFECT_KIND_WORKSPACE_DATA_DELETE:
		evidence.Evidence = &controlpb.EffectEvidence_WorkspaceDataDeleted{WorkspaceDataDeleted: &controlpb.WorkspaceDataDeleted{}}
	}
	recorded, e := c.operations.RecordEffectResult(c.ctx, &controlpb.RecordEffectResultRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: planned.GetOperation().GetVersion(), EffectId: planned.GetEffect().GetId(), State: controlpb.EffectState_EFFECT_STATE_SUCCEEDED, ExternalId: external.S("externalId"), Evidence: evidence})
	must(c.t, e)
	return recorded.GetOperation(), recorded.GetEffect()
}

func name(kind controlpb.EffectKind) string {
	return map[controlpb.EffectKind]string{controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE: "sandbox_ensure", controlpb.EffectKind_EFFECT_KIND_SANDBOX_TERMINATE: "sandbox_terminate", controlpb.EffectKind_EFFECT_KIND_WORKSPACE_DATA_DELETE: "workspace_data_delete"}[kind]
}

func (c *grpcController) substrate(id string, request core.Object) core.Object {
	c.t.Helper()
	return c.put("/effects/"+id, request)
}

func (c *grpcController) put(path string, body core.Object) core.Object {
	c.t.Helper()
	raw, e := json.Marshal(body)
	must(c.t, e)
	req, e := http.NewRequestWithContext(context.Background(), "PUT", c.f.external.URL+path, bytes.NewReader(raw))
	must(c.t, e)
	res, e := http.DefaultClient.Do(req)
	must(c.t, e)
	defer res.Body.Close()
	out := core.Object{}
	must(c.t, json.NewDecoder(res.Body).Decode(&out))
	if res.StatusCode != http.StatusOK {
		c.t.Fatalf("simulator %s: %d %v", path, res.StatusCode, out)
	}
	return out
}

func (c *grpcController) register(sandbox string, generation int64, node, incarnation string) (*controlpb.NodeRecord, error) {
	out, e := c.nodes.RegisterNode(c.ctx, &controlpb.RegisterNodeRequest{SubmissionId: c.sub(), Epoch: c.epoch, SandboxInstanceId: sandbox, Generation: generation, Node: &controlpb.NodeIdentity{NodeId: node, NodeIncarnationId: incarnation}, ProtocolVersion: 1})
	return out.GetNode(), e
}

func (c *grpcController) status(n *controlpb.NodeRecord, connection controlpb.NodeConnection) (*controlpb.NodeRecord, error) {
	out, e := c.nodes.ReportNodeStatus(c.ctx, &controlpb.ReportNodeStatusRequest{SubmissionId: c.sub(), Epoch: c.epoch, NodeInstanceId: n.GetId(), Version: n.GetVersion(), Connection: connection, Initialized: true})
	return out.GetNode(), e
}

func cloneSpec(repository, branch string) *controlpb.ExecutionInput {
	return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: &controlpb.CloneSpec{Repository: repository, Branch: branch}}}
}

// A Rust-shaped Controller creates a Project purely over gRPC: sandbox, then the Controller reports
// the sandbox's Node, then the Workspace's clone is registered against Cloud-decided input and
// Node; admission opens only once the clone succeeded. Stop then quiesces with Controller-reported
// idle evidence and terminates, after which every report about the old sandbox is fenced.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #workspace-opens-admission-only-after-node-and-clone-succeed and
// test-cases/cloud/controller-integration/workspace-operations-and-node-reports.md.
func TestGRPCControllerDrivesWorkspaceThroughNodeAndClone(t *testing.T) {
	f := setup(t)
	c := newGRPCController(t, f)
	created := f.create("grpc-create")
	wid := created.O("workspace").S("id")
	snap := c.claim()
	op := snap.GetOperation()
	if op.GetKind() != controlpb.OperationKind_OPERATION_KIND_CREATE_PROJECT || op.GetStep() != controlpb.OperationStep_OPERATION_STEP_SANDBOX || snap.GetWorkspaces()[0].GetRequestedRef() != "main" || snap.GetProject().GetRepositoryUrl() != "https://example.invalid/repo.git" {
		t.Fatalf("unexpected snapshot: %v", snap)
	}
	_, e := c.operations.PlanEffect(c.ctx, &controlpb.PlanEffectRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), Version: op.GetVersion(), Kind: controlpb.EffectKind_EFFECT_KIND_WORKSPACE_DATA_DELETE, WorkspaceId: wid})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	op, ensure := c.effect(op, controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, wid)
	nodeID := ensure.GetEvidence().GetSandboxEnsured().GetNodeId()
	if ensure.GetRequest().GetSandboxEnsure().GetWorkspaceId() != wid || nodeID == "" {
		t.Fatalf("sandbox_ensure must carry no repository and report a node id: %v", ensure)
	}
	op, e = c.advance(op)
	must(t, e)
	if op.GetStep() != controlpb.OperationStep_OPERATION_STEP_NODE {
		t.Fatalf("sandbox must advance to node: %v", op)
	}
	_, e = c.advance(op)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// Only the lease holder may report, only the ensured NodeId, only for the current generation.
	sandbox := ensure.GetId()
	_, e = c.nodes.RegisterNode(c.ctx, &controlpb.RegisterNodeRequest{SubmissionId: c.sub(), Epoch: c.epoch + 1, SandboxInstanceId: sandbox, Generation: 1, Node: &controlpb.NodeIdentity{NodeId: nodeID, NodeIncarnationId: "inc-1"}, ProtocolVersion: 1})
	expectStatus(t, e, codes.FailedPrecondition, controlpb.ErrorCode_ERROR_CODE_STALE_CONTROLLER)
	_, e = c.register(sandbox, 1, "some-other-node", "inc-1")
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	_, e = c.register(sandbox, 2, nodeID, "inc-1")
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if f.scalar("SELECT count(*) FROM node_instances WHERE sandbox_instance_id=$1", sandbox) != 0 {
		t.Fatal("a refused report wrote a Node")
	}
	register := &controlpb.RegisterNodeRequest{SubmissionId: "register-inc-1", Epoch: c.epoch, SandboxInstanceId: sandbox, Generation: 1, Node: &controlpb.NodeIdentity{NodeId: nodeID, NodeIncarnationId: "inc-1"}, ProtocolVersion: 1}
	registered, e := c.nodes.RegisterNode(c.ctx, register)
	must(t, e)
	replayed, e := c.nodes.RegisterNode(c.ctx, register)
	must(t, e)
	if replayed.GetNode().GetId() != registered.GetNode().GetId() || !registered.GetNode().GetInitialized() || registered.GetNode().GetIdentity().GetNodeId() != nodeID {
		t.Fatalf("register replay must return the original incarnation: %v vs %v", replayed.GetNode(), registered.GetNode())
	}
	register.Node.NodeIncarnationId = "inc-2"
	_, e = c.nodes.RegisterNode(c.ctx, register)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	_, e = c.register(sandbox, 1, nodeID, "inc-2")
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	_, e = c.status(registered.GetNode(), controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
	must(t, e)
	op, e = c.advance(op)
	must(t, e)
	if op.GetStep() != controlpb.OperationStep_OPERATION_STEP_CLONE {
		t.Fatalf("a new Workspace must clone after its Node connects: %v", op)
	}
	// The Node is connected but the Workspace has no code yet: no execution is admitted.
	if closed := f.internal("/internal/v1/access", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "epoch": c.epoch}, 409); closed.S("code") != "execution_closed" {
		t.Fatalf("execution before the clone must be refused as execution_closed: %v", closed)
	}

	// Cloud, not the Controller, decides the clone's input and target Node.
	executions := f.executions
	dispatch := func(repository, branch, node, execution string) error {
		_, err := executions.RecordDispatch(c.ctx, &controlpb.RecordDispatchRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), ExecutionId: execution, NodeId: node, Input: cloneSpec(repository, branch)})
		return err
	}
	expectStatus(t, dispatch("https://example.invalid/other.git", "main", nodeID, "exec-a"), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	expectStatus(t, dispatch("https://example.invalid/repo.git", "other", nodeID, "exec-a"), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	expectStatus(t, dispatch("https://example.invalid/repo.git", "main", "not-this-node", "exec-a"), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	must(t, dispatch("https://example.invalid/repo.git", "main", nodeID, "exec-a"))
	expectStatus(t, dispatch("https://example.invalid/repo.git", "main", nodeID, "exec-b"), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	// A tenant clone request and the Workspace clone coexist: recovery sees both, ClaimWork only the
	// tenant request.
	tenantRequest, e := f.store.EnqueueClone(context.Background(), f.tid, f.uid, "tenant-request", "https://example.invalid/repo.git", "main")
	must(t, e)
	work, e := executions.ClaimWork(c.ctx, &controlpb.ClaimWorkRequest{SubmissionId: c.sub(), Epoch: c.epoch})
	must(t, e)
	if work.GetItem().GetOperationId() != tenantRequest.S("id") {
		t.Fatalf("ClaimWork must offer only tenant clone requests: %v", work.GetItem())
	}
	must(t, func() error {
		_, err := executions.RecordDispatch(c.ctx, &controlpb.RecordDispatchRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: tenantRequest.S("id"), ExecutionId: "exec-tenant", NodeId: "fixed-node", Input: work.GetItem().GetInput()})
		return err
	}())
	pending, e := executions.ListPendingDispatches(c.ctx, &controlpb.ListPendingDispatchesRequest{})
	must(t, e)
	if len(pending.GetRecords()) != 2 {
		t.Fatalf("recovery must list the Workspace and the tenant clone: %v", pending.GetRecords())
	}
	_, e = c.advance(op)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	outcome := c.put("/clones/"+"00000000-0000-4000-8000-000000000001", core.Object{"workspaceId": wid, "repositoryUrl": "https://example.invalid/repo.git", "branch": "main"})
	ready := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: nodeID, NodeIncarnationId: "inc-1"}, Outcome: &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: outcome.S("path"), Commit: outcome.S("commit")}}}
	_, e = executions.RecordQueriedResult(c.ctx, &controlpb.RecordQueriedResultRequest{SubmissionId: c.sub(), Epoch: c.epoch, OperationId: op.GetId(), ExecutionId: "exec-a", Result: ready})
	must(t, e)
	op, e = c.advance(op)
	must(t, e)
	w := f.ws(wid)
	if op.GetState() != controlpb.OperationState_OPERATION_STATE_SUCCEEDED || w.S("observedState") != "ready" || !w.B("admissionOpen") || w.S("baseCommitId") != f.commit {
		t.Fatalf("clone success must open the Workspace with its baseline: %v %v", op, w)
	}
	f.internal("/internal/v1/access", core.Object{"tenantId": f.tid, "workspaceId": wid, "action": "execute", "epoch": c.epoch}, 200)

	// Stop: the Controller gives idle evidence for its Node, then terminates the sandbox.
	f.call("POST", f.path("/workspaces/"+wid+"/stop"), core.Object{"version": w.N("version")}, "grpc-stop", 202)
	snap = c.claim()
	op = snap.GetOperation()
	node := snap.GetNodes()[0]
	idle, e := c.nodes.ReportNodeIdle(c.ctx, &controlpb.ReportNodeIdleRequest{SubmissionId: c.sub(), Epoch: c.epoch, NodeInstanceId: node.GetId(), Version: node.GetVersion(), OperationId: op.GetId(), AdmissionEpoch: snap.GetWorkspaces()[0].GetAdmissionEpoch(), Idle: true})
	must(t, e)
	if !idle.GetAccepted() || idle.GetNode().GetIdleAdmissionEpoch() != snap.GetWorkspaces()[0].GetAdmissionEpoch() {
		t.Fatalf("idle evidence was not recorded: %v", idle)
	}
	op, e = c.advance(op)
	must(t, e)
	op, _ = c.effect(op, controlpb.EffectKind_EFFECT_KIND_SANDBOX_TERMINATE, wid)
	op, e = c.advance(op)
	must(t, e)
	if op.GetState() != controlpb.OperationState_OPERATION_STATE_SUCCEEDED || f.ws(wid).S("observedState") != "stopped" {
		t.Fatalf("stop did not complete: %v", op)
	}
	// Reports about the terminated sandbox are fenced and change nothing.
	before := f.scalar("SELECT version FROM node_instances WHERE id=$1", node.GetId())
	_, e = c.status(idle.GetNode(), controlpb.NodeConnection_NODE_CONNECTION_CONNECTED)
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	_, e = c.register(sandbox, 1, nodeID, "inc-3")
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if f.scalar("SELECT version FROM node_instances WHERE id=$1", node.GetId()) != before || f.scalar("SELECT count(*) FROM node_instances WHERE sandbox_instance_id=$1", sandbox) != 1 {
		t.Fatal("a fenced report changed the database")
	}
}

// A Node that reconnects as a new incarnation is registered only after the Controller ended the
// previous one, and ending is idempotent.
//
// Evidence for specs test-cases/cloud/controller-integration/workspace-operations-and-node-reports.md
// #only-the-lease-holder-can-report-nodes-for-the-current-generation.
func TestGRPCNodeIncarnationIsReplacedOnlyAfterEnd(t *testing.T) {
	f := setup(t)
	c := newGRPCController(t, f)
	created := f.create("incarnation")
	wid := created.O("workspace").S("id")
	op := c.claim().GetOperation()
	op, ensure := c.effect(op, controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE, wid)
	_, e := c.advance(op)
	must(t, e)
	nodeID := ensure.GetEvidence().GetSandboxEnsured().GetNodeId()
	first, e := c.register(ensure.GetId(), 1, nodeID, "inc-1")
	must(t, e)
	_, e = c.register(ensure.GetId(), 1, nodeID, "inc-2")
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	_, e = c.nodes.EndNode(c.ctx, &controlpb.EndNodeRequest{SubmissionId: c.sub(), Epoch: c.epoch, NodeInstanceId: first.GetId(), Version: first.GetVersion() + 1})
	expectStatus(t, e, codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	ended, e := c.nodes.EndNode(c.ctx, &controlpb.EndNodeRequest{SubmissionId: c.sub(), Epoch: c.epoch, NodeInstanceId: first.GetId(), Version: first.GetVersion()})
	must(t, e)
	if ended.GetNode().GetConnection() != controlpb.NodeConnection_NODE_CONNECTION_ENDED {
		t.Fatalf("incarnation not ended: %v", ended.GetNode())
	}
	again, e := c.nodes.EndNode(c.ctx, &controlpb.EndNodeRequest{SubmissionId: c.sub(), Epoch: c.epoch, NodeInstanceId: first.GetId(), Version: first.GetVersion()})
	must(t, e)
	if again.GetNode().GetVersion() != ended.GetNode().GetVersion() {
		t.Fatal("ending an ended incarnation changed it")
	}
	second, e := c.register(ensure.GetId(), 1, nodeID, "inc-2")
	must(t, e)
	if second.GetId() == first.GetId() || f.scalar("SELECT count(*) FROM node_instances WHERE sandbox_instance_id=$1 AND ended_at IS NULL", ensure.GetId()) != 1 {
		t.Fatal("a new incarnation must be a new record while the old one stays ended")
	}
}

// OperationAvailable follows the commit that queued a Workspace operation, just like WorkAvailable
// follows a tenant clone request.
func TestGRPCWatchDeliversOperationAvailable(t *testing.T) {
	f := setup(t)
	c := newGRPCController(t, f)
	watchCtx, stop := context.WithTimeout(c.ctx, 10*time.Second)
	defer stop()
	stream, e := controlpb.NewControlSignalServiceClient(f.controlConn).Watch(watchCtx, &controlpb.WatchRequest{Epoch: c.epoch})
	must(t, e)
	_, e = stream.Header()
	must(t, e)
	created := f.create("watched-create")
	msg, e := stream.Recv()
	must(t, e)
	if msg.GetOperationAvailable().GetOperationId() != created.O("operation").S("id") {
		t.Fatalf("expected OperationAvailable for %s, got %v", created.O("operation").S("id"), msg)
	}
}
