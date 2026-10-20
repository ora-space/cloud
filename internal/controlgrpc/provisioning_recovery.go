package controlgrpc

import (
	"context"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// ListPendingProvisioningEffects exposes Cloud's original query responsibilities, not a claim or
// a dispatch permit. All authority and filtering stay in the same core transaction as late facts.
func (s *operationService) ListPendingProvisioningEffects(ctx context.Context, req *controlpb.ListPendingProvisioningEffectsRequest) (*controlpb.ListPendingProvisioningEffectsResponse, error) {
	out, err := control(ctx, s.store, "provisioning_pending_effects", "", "", core.Object{
		"epoch": req.GetEpoch(), "afterEffectId": req.GetAfterEffectId(), "pageSize": int64(req.GetPageSize()),
	})
	if err != nil {
		return nil, err
	}
	response := &controlpb.ListPendingProvisioningEffectsResponse{NextAfterEffectId: out.S("nextAfterEffectId")}
	for _, item := range rows(out["items"]) {
		response.Items = append(response.Items, &controlpb.PendingProvisioningEffect{OperationId: item.S("operationId"), Effect: effect(item.O("effect"))})
	}
	return response, nil
}
