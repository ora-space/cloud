package controlgrpc

import (
	"context"
	"encoding/base64"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// executionService is the execution registry of the clone loop. Each RPC is one clone_* control
// action; state-changing calls carry their submission identity so Cloud replays a recorded
// response instead of reapplying when the Controller retries after a lost reply.
type executionService struct {
	controlpb.UnimplementedExecutionServiceServer
	store *core.Store
}

func (s *executionService) ClaimWork(ctx context.Context, req *controlpb.ClaimWorkRequest) (*controlpb.ClaimWorkResponse, error) {
	out, e := s.control(ctx, "clone_claim", "", core.Object{"epoch": req.GetEpoch()})
	if e != nil {
		return nil, e
	}
	if work := out.O("work"); work.S("id") != "" {
		target := work.O("target")
		return &controlpb.ClaimWorkResponse{Item: &controlpb.WorkItem{
			// claimedRunId is the claim's own projection of "the run this work belongs to" — run_id
			// for agent work, workflow_run_id for workflow work (claimWorkItem explains why one
			// column cannot answer for both).
			OperationId: work.S("claimedRunId"),
			Input:       input(work.O("input")),
			Target:      &controlpb.WorkTarget{WorkspaceId: target.S("workspaceId"), SandboxInstanceId: target.S("sandboxInstanceId"), NodeId: target.S("nodeId")},
		}}, nil
	}
	request := out.O("request")
	if request.S("id") == "" {
		return &controlpb.ClaimWorkResponse{}, nil
	}
	return &controlpb.ClaimWorkResponse{Item: &controlpb.WorkItem{OperationId: request.S("id"), Input: input(core.Object{"kind": "clone", "repositoryUrl": request.S("repositoryUrl"), "branch": request.S("branch")})}}, nil
}

func (s *executionService) RecordDispatch(ctx context.Context, req *controlpb.RecordDispatchRequest) (*controlpb.RecordDispatchResponse, error) {
	in, e := inputObject(req.GetInput())
	if e != nil {
		return nil, e
	}
	body := core.Object{"epoch": req.GetEpoch(), "operationId": req.GetOperationId(), "executionId": req.GetExecutionId(), "nodeId": req.GetNodeId(), "input": in}
	out, e := s.control(ctx, "clone_dispatch", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.RecordDispatchResponse{Record: record(out)}, nil
}

func (s *executionService) TakeOverNodeEvent(ctx context.Context, req *controlpb.TakeOverNodeEventRequest) (*controlpb.TakeOverNodeEventResponse, error) {
	res, e := resultObject(req.GetResult())
	if e != nil {
		return nil, e
	}
	sequence, ok := receiptSequence(req.GetSequence())
	if !ok || len(req.GetEvent()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid_receipt")
	}
	body := core.Object{"epoch": req.GetEpoch(), "operationId": req.GetOperationId(), "executionId": req.GetExecutionId(), "sequence": sequence, "result": res, "event": base64.StdEncoding.EncodeToString(req.GetEvent())}
	out, e := s.control(ctx, "clone_takeover", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.TakeOverNodeEventResponse{Record: record(out)}, nil
}

func (s *executionService) RecordQueriedResult(ctx context.Context, req *controlpb.RecordQueriedResultRequest) (*controlpb.RecordQueriedResultResponse, error) {
	res, e := resultObject(req.GetResult())
	if e != nil {
		return nil, e
	}
	body := core.Object{"epoch": req.GetEpoch(), "operationId": req.GetOperationId(), "executionId": req.GetExecutionId(), "result": res}
	out, e := s.control(ctx, "clone_queried", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.RecordQueriedResultResponse{Record: record(out)}, nil
}

func (s *executionService) GetDispatch(ctx context.Context, req *controlpb.GetDispatchRequest) (*controlpb.GetDispatchResponse, error) {
	out, e := s.control(ctx, "clone_get", "", core.Object{"executionId": req.GetExecutionId()})
	if e != nil {
		return nil, e
	}
	return &controlpb.GetDispatchResponse{Record: record(out)}, nil
}

func (s *executionService) ListPendingDispatches(ctx context.Context, req *controlpb.ListPendingDispatchesRequest) (*controlpb.ListPendingDispatchesResponse, error) {
	out, e := s.control(ctx, "clone_pending", "", core.Object{"nodeId": req.GetNodeId()})
	if e != nil {
		return nil, e
	}
	rows, _ := out["executions"].([]core.Object)
	records := make([]*controlpb.ExecutionRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, record(row))
	}
	return &controlpb.ListPendingDispatchesResponse{Records: records}, nil
}

// control runs one clone action for the verified principal and maps its Fault.
func (s *executionService) control(ctx context.Context, action, submission string, body core.Object) (core.Object, error) {
	out, e := s.store.Control(ctx, &core.ControlRequest{Action: action, SubmissionID: submission, Body: body, Service: principal(ctx)})
	if e != nil {
		return nil, toStatus(e)
	}
	return out, nil
}

// receiptSequence narrows the wire sequence to the bigint column; anything larger is not a Node
// sequence this contract can store and is rejected instead of wrapping.
func receiptSequence(sequence uint64) (int64, bool) {
	if sequence > math.MaxInt64 {
		return 0, false
	}
	return int64(sequence), true // #nosec G115 -- bounded above.
}
