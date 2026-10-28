package controlgrpc

import (
	"context"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// ListExecutionFences forwards frozen origins and current fact-query authority without granting
// execution. The Node must atomically fence intake before replying with a non-acceptance proof.
func (s *runtimeControlService) ListExecutionFences(ctx context.Context, r *controlpb.ListExecutionFencesRequest) (*controlpb.ListExecutionFencesResponse, error) {
	out, err := s.command(ctx, "runtime_execution_fences", "", core.Object{"epoch": r.GetEpoch(), "nodeId": r.GetNodeId()})
	if err != nil {
		return nil, err
	}
	response := &controlpb.ListExecutionFencesResponse{}
	for _, e := range rows(out["committed"]) {
		response.CommittedExecutionIds = append(response.CommittedExecutionIds, e.S("executionId"))
	}
	for _, fence := range rows(out["fences"]) {
		o := fence.O("origin")
		response.Fences = append(response.Fences, &controlpb.ExecutionFence{
			Authority: binding(fence.O("authority"), r.GetEpoch()),
			Origin:    &controlpb.ExecutionOrigin{Binding: binding(o.O("binding"), o.O("binding").N("controllerEpoch")), WorkId: o.S("workId"), InputJson: o.S("inputJson")},
		})
	}
	return response, nil
}

// RecordExecutionNonAcceptance returns only after the separate proof has durably committed.
func (s *runtimeControlService) RecordExecutionNonAcceptance(ctx context.Context, r *controlpb.RecordExecutionNonAcceptanceRequest) (*controlpb.RecordExecutionNonAcceptanceResponse, error) {
	p := r.GetProof()
	f := p.GetFence()
	o := f.GetOrigin()
	proof := core.Object{"proofId": p.GetProofId(), "fence": core.Object{
		"authority": bindingObject(f.GetAuthority()),
		"origin":    core.Object{"binding": bindingObject(o.GetBinding()), "workId": o.GetWorkId(), "inputJson": o.GetInputJson()},
	}}
	_, err := s.command(ctx, "runtime_execution_non_acceptance", r.GetSubmissionId(), core.Object{"epoch": r.GetEpoch(), "proof": proof})
	if err != nil {
		return nil, err
	}
	return &controlpb.RecordExecutionNonAcceptanceResponse{}, nil
}

// bindingObject is the lossless reverse of binding; original scope must not inherit a fresh lease.
func bindingObject(b *controlpb.RuntimeBinding) core.Object {
	return core.Object{
		"tenantId": b.GetTenantId(), "workspaceId": b.GetWorkspaceId(), "sandboxId": b.GetSandboxId(),
		"runtimeGeneration": b.GetRuntimeGeneration(), "nodeId": b.GetNodeId(), "nodeIncarnationId": b.GetNodeIncarnationId(),
		"nodeInstanceId": b.GetNodeInstanceId(), "controllerEpoch": b.GetControllerEpoch(), "controlEpoch": b.GetControlEpoch(),
		"controlVersion": b.GetControlVersion(), "sessionId": b.GetSessionId(), "actorUserId": b.GetActorUserId(),
		"operationId": b.GetOperationId(), "executionId": b.GetExecutionId(), "nodeOperationId": b.GetNodeOperationId(),
		"inputClosed": b.GetInputClosed(), "issuedAtMs": b.GetIssuedAtMs(), "expiresAtMs": b.GetExpiresAtMs(),
	}
}
