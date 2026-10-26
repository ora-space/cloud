package controlgrpc

import (
	"context"
	"encoding/json"
	"math"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// agentRunService serves durable session operations and the independent optional preview path.
type agentRunService struct {
	controlpb.UnimplementedAgentRunServiceServer
	store *core.Store
}

// PublishThreadPreview accepts a lossy replacement under the current execution and lease fences.
func (s *agentRunService) PublishThreadPreview(ctx context.Context, req *controlpb.PublishThreadPreviewRequest) (*controlpb.PublishThreadPreviewResponse, error) {
	blocks := make([]core.ThreadPreviewBlock, 0, len(req.GetBlocks()))
	for _, block := range req.GetBlocks() {
		blocks = append(blocks, core.ThreadPreviewBlock{HistorySequence: int64(block.GetHistorySequence()), TurnID: block.GetTurnId(), Record: json.RawMessage(block.GetRecord())})
	}
	err := s.store.PublishThreadPreview(ctx, principal(ctx), req.GetEpoch(), req.GetOperationId(), core.ThreadPreviewSnapshot{ExecutionID: req.GetExecutionId(), Version: req.GetVersion(), Blocks: blocks, Incomplete: req.GetIncomplete()})
	if err != nil {
		return nil, toStatus(err)
	}
	return &controlpb.PublishThreadPreviewResponse{}, nil
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
		if ev.VisibleText != nil {
			item["visibleText"] = ev.GetVisibleText()
		}
		if ev.GetVisibleTextTruncated() {
			item["visibleTextTruncated"] = true
		}
		if ev.HistorySequence != nil {
			item["historySequence"] = int64(ev.GetHistorySequence())
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

// GrantScheduledArtifactUpload issues fresh grants for the artifacts a scheduled run declared. Each
// requested entry must name a declared index whose size and digest Cloud persisted; a request that
// tries to describe an object Cloud did not freeze is refused instead of granted.
func (s *agentRunService) GrantScheduledArtifactUpload(ctx context.Context, req *controlpb.GrantScheduledArtifactUploadRequest) (*controlpb.GrantScheduledArtifactUploadResponse, error) {
	artifacts := make([]core.Object, 0, len(req.GetArtifacts()))
	for _, artifact := range req.GetArtifacts() {
		if artifact.GetSize() > math.MaxInt64 {
			return nil, invalidInput("invalid_upload_checksum")
		}
		artifacts = append(artifacts, core.Object{
			// #nosec G115 -- sizes above MaxInt64 were refused just above.
			"index": int64(artifact.GetIndex()), "size": int64(artifact.GetSize()), "sha256": artifact.GetSha256(),
		})
	}
	out, e := control(ctx, s.store, "grant_scheduled_artifact_upload", "", "", core.Object{
		"epoch": req.GetEpoch(), "executionId": req.GetExecutionId(), "artifacts": artifacts,
	})
	if e != nil {
		return nil, e
	}
	grants := make([]*controlpb.UploadGrant, 0)
	for _, row := range rows(out["grants"]) {
		grants = append(grants, grantMessage(row))
	}
	return &controlpb.GrantScheduledArtifactUploadResponse{Grants: grants}, nil
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
