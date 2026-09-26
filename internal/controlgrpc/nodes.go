package controlgrpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// nodeService records the Controller's reports about the desktop Nodes it holds sessions with.
// Each RPC is one report_node_* control action; the Node-credential JSON actions share the status
// and idle transactions, so both reporters obey the same fencing.
type nodeService struct {
	controlpb.UnimplementedNodeReportServiceServer
	store *core.Store
}

func (s *nodeService) RegisterNode(ctx context.Context, req *controlpb.RegisterNodeRequest) (*controlpb.RegisterNodeResponse, error) {
	identity := req.GetNode()
	body := core.Object{"epoch": req.GetEpoch(), "sandboxInstanceId": req.GetSandboxInstanceId(), "generation": req.GetGeneration(), "nodeId": identity.GetNodeId(), "nodeIncarnationId": identity.GetNodeIncarnationId(), "protocolVersion": int64(req.GetProtocolVersion())}
	out, e := control(ctx, s.store, "report_node_register", "", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.RegisterNodeResponse{Node: node(out)}, nil
}

func (s *nodeService) ReportNodeStatus(ctx context.Context, req *controlpb.ReportNodeStatusRequest) (*controlpb.ReportNodeStatusResponse, error) {
	connection := req.GetConnection()
	if connection != controlpb.NodeConnection_NODE_CONNECTION_CONNECTED && connection != controlpb.NodeConnection_NODE_CONNECTION_DISCONNECTED {
		return nil, status.Error(codes.InvalidArgument, "invalid_node_state")
	}
	body := core.Object{"epoch": req.GetEpoch(), "nodeInstanceId": req.GetNodeInstanceId(), "version": req.GetVersion(), "connectionState": name(connection.String(), "NODE_CONNECTION_"), "initialized": req.GetInitialized()}
	out, e := control(ctx, s.store, "report_node_status", "", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.ReportNodeStatusResponse{Node: node(out)}, nil
}

func (s *nodeService) EndNode(ctx context.Context, req *controlpb.EndNodeRequest) (*controlpb.EndNodeResponse, error) {
	body := core.Object{"epoch": req.GetEpoch(), "nodeInstanceId": req.GetNodeInstanceId(), "version": req.GetVersion()}
	out, e := control(ctx, s.store, "report_node_end", "", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.EndNodeResponse{Node: node(out)}, nil
}

func (s *nodeService) ReportNodeIdle(ctx context.Context, req *controlpb.ReportNodeIdleRequest) (*controlpb.ReportNodeIdleResponse, error) {
	body := core.Object{"epoch": req.GetEpoch(), "nodeInstanceId": req.GetNodeInstanceId(), "version": req.GetVersion(), "operationId": req.GetOperationId(), "admissionEpoch": req.GetAdmissionEpoch(), "idle": req.GetIdle()}
	out, e := control(ctx, s.store, "report_node_idle", "", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	if _, refused := out["accepted"]; refused {
		return &controlpb.ReportNodeIdleResponse{}, nil
	}
	return &controlpb.ReportNodeIdleResponse{Accepted: true, Node: node(out)}, nil
}

func node(n core.Object) *controlpb.NodeRecord {
	out := &controlpb.NodeRecord{
		Id: n.S("id"), SandboxInstanceId: n.S("sandboxInstanceId"), WorkspaceId: n.S("workspaceId"),
		Connection:  controlpb.NodeConnection(enum(controlpb.NodeConnection_value, "NODE_CONNECTION_", n.S("connectionState"))),
		Initialized: n.B("initialized"), Version: n.N("version"), IdleAdmissionEpoch: optionalInt(n, "idleAdmissionEpoch"),
	}
	if n.S("nodeIncarnationId") != "" {
		out.Identity = &controlpb.NodeIdentity{NodeId: n.S("nodeId"), NodeIncarnationId: n.S("nodeIncarnationId")}
	}
	return out
}
