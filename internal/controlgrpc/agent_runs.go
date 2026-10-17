package controlgrpc

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// agentRunService is the session side of the control plane: Thread events, Thread commands and
// upload grants. Writes carry a submission identity, the same way ExecutionService does.
type agentRunService struct {
	controlpb.UnimplementedAgentRunServiceServer
	store *core.Store
}

func (s *agentRunService) TakeOverThreadEvents(ctx context.Context, req *controlpb.TakeOverThreadEventsRequest) (*controlpb.TakeOverThreadEventsResponse, error) {
	events := make([]core.Object, 0, len(req.GetEvents()))
	for _, ev := range req.GetEvents() {
		sequence, ok := receiptSequence(ev.GetSequence())
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "invalid_event")
		}
		item := core.Object{"sequence": sequence, "record": ev.GetRecord(), "truncated": ev.GetTruncated()}
		if ev.TurnId != nil {
			item["turnId"] = ev.GetTurnId()
		}
		events = append(events, item)
	}
	body := core.Object{"epoch": req.GetEpoch(), "operationId": req.GetOperationId(), "executionId": req.GetExecutionId(), "events": events}
	out, e := control(ctx, s.store, "thread_events", "", req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	taken := out.N("takenOverThrough")
	if taken < 0 {
		return nil, status.Error(codes.Internal, "invalid_event")
	}
	return &controlpb.TakeOverThreadEventsResponse{TakenOverThrough: uint64(taken)}, nil // #nosec G115 -- taken was rejected when negative.
}

func (s *agentRunService) ClaimThreadCommands(ctx context.Context, req *controlpb.ClaimThreadCommandsRequest) (*controlpb.ClaimThreadCommandsResponse, error) {
	out, e := control(ctx, s.store, "thread_claim", "", "", core.Object{"epoch": req.GetEpoch(), "limit": int64(req.GetLimit())})
	if e != nil {
		return nil, e
	}
	commands := make([]*controlpb.ThreadCommand, 0)
	for _, row := range rows(out["commands"]) {
		commands = append(commands, threadCommand(row))
	}
	return &controlpb.ClaimThreadCommandsResponse{Commands: commands}, nil
}

func (s *agentRunService) RecordThreadCommandDelivered(ctx context.Context, req *controlpb.RecordThreadCommandDeliveredRequest) (*controlpb.RecordThreadCommandDeliveredResponse, error) {
	_, e := control(ctx, s.store, "thread_delivered", "", req.GetSubmissionId(), core.Object{"epoch": req.GetEpoch(), "commandId": req.GetCommandId(), "executionId": req.GetExecutionId()})
	if e != nil {
		return nil, e
	}
	return &controlpb.RecordThreadCommandDeliveredResponse{}, nil
}

func (s *agentRunService) GrantRevisionUpload(ctx context.Context, req *controlpb.GrantRevisionUploadRequest) (*controlpb.GrantRevisionUploadResponse, error) {
	checksums := core.Object{}
	for key, digest := range req.GetChecksums() {
		checksums[key] = digest
	}
	out, e := control(ctx, s.store, "grant_revision_upload", "", "", core.Object{"epoch": req.GetEpoch(), "executionId": req.GetExecutionId(), "checksums": checksums})
	if e != nil {
		return nil, e
	}
	grants := make([]*controlpb.UploadGrant, 0)
	for _, row := range rows(out["grants"]) {
		grants = append(grants, grantMessage(row))
	}
	return &controlpb.GrantRevisionUploadResponse{Grants: grants}, nil
}

func threadCommand(row core.Object) *controlpb.ThreadCommand {
	cmd := &controlpb.ThreadCommand{
		CommandId: row.S("commandId"), RunId: row.S("runId"), ExecutionId: row.S("executionId"),
		Target: &controlpb.WorkTarget{WorkspaceId: row.S("workspaceId"), SandboxInstanceId: row.S("sandboxInstanceId"), NodeId: row.S("nodeId")},
	}
	body := row.O("body")
	if row.S("kind") == "end_session" {
		cmd.Command = &controlpb.ThreadCommand_EndSession{EndSession: &controlpb.EndSession{Reason: controlpb.EndSessionReason(enum(controlpb.EndSessionReason_value, "END_SESSION_REASON_", body.S("reason")))}}
		return cmd
	}
	cmd.Command = &controlpb.ThreadCommand_SubmitUserTurn{SubmitUserTurn: &controlpb.SubmitUserTurn{Turn: &controlpb.UserTurn{TurnId: body.S("turnId"), Content: contentMessages(rows(body["content"]))}}}
	return cmd
}
