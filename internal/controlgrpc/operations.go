package controlgrpc

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// operationService drives runtime Workspace operations. Each RPC is the JSON internal API's
// operation action of the same meaning (claim, plan, effect_result, advance, defer) run through the
// same Store.Control transaction; only the message shapes differ.
type operationService struct {
	controlpb.UnimplementedWorkspaceOperationServiceServer
	store *core.Store
}

func (s *operationService) ClaimOperation(ctx context.Context, req *controlpb.ClaimOperationRequest) (*controlpb.ClaimOperationResponse, error) {
	out, e := control(ctx, s.store, "claim", "", "", core.Object{"epoch": req.GetEpoch()})
	if e != nil {
		return nil, e
	}
	if len(out.O("operation")) == 0 {
		return &controlpb.ClaimOperationResponse{}, nil
	}
	return &controlpb.ClaimOperationResponse{Snapshot: snapshot(out)}, nil
}

func (s *operationService) PlanEffect(ctx context.Context, req *controlpb.PlanEffectRequest) (*controlpb.PlanEffectResponse, error) {
	if req.GetKind() == controlpb.EffectKind_EFFECT_KIND_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "invalid_effect_kind")
	}
	body := core.Object{"epoch": req.GetEpoch(), "version": req.GetVersion(), "kind": name(req.GetKind().String(), "EFFECT_KIND_"), "workspaceId": req.GetWorkspaceId()}
	out, e := control(ctx, s.store, "plan", req.GetOperationId(), req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.PlanEffectResponse{Effect: effect(out.O("effect")), Operation: operation(out.O("operation"))}, nil
}

func (s *operationService) RecordEffectResult(ctx context.Context, req *controlpb.RecordEffectResultRequest) (*controlpb.RecordEffectResultResponse, error) {
	result := core.Object{}
	switch req.GetState() {
	case controlpb.EffectState_EFFECT_STATE_SUCCEEDED:
		evidence, ok := evidenceObject(req.GetEvidence())
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "evidence_required")
		}
		result = evidence
	case controlpb.EffectState_EFFECT_STATE_FAILED:
		if req.Failure != nil {
			result["error"] = req.GetFailure()
		}
	case controlpb.EffectState_EFFECT_STATE_RUNNING, controlpb.EffectState_EFFECT_STATE_ABSENT:
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid_effect_state")
	}
	body := core.Object{"epoch": req.GetEpoch(), "version": req.GetVersion(), "state": name(req.GetState().String(), "EFFECT_STATE_"), "externalId": req.GetExternalId(), "result": result}
	r := &core.ControlRequest{Action: "effect_result", OperationID: req.GetOperationId(), EffectID: req.GetEffectId(), SubmissionID: req.GetSubmissionId(), Body: body, Service: principal(ctx)}
	out, e := s.store.Control(ctx, r)
	if e != nil {
		return nil, toStatus(e)
	}
	return &controlpb.RecordEffectResultResponse{Effect: effect(out.O("effect")), Operation: operation(out.O("operation"))}, nil
}

func (s *operationService) AdvanceOperation(ctx context.Context, req *controlpb.AdvanceOperationRequest) (*controlpb.AdvanceOperationResponse, error) {
	out, e := control(ctx, s.store, "advance", req.GetOperationId(), req.GetSubmissionId(), core.Object{"epoch": req.GetEpoch(), "version": req.GetVersion()})
	if e != nil {
		return nil, e
	}
	return &controlpb.AdvanceOperationResponse{Operation: operation(out)}, nil
}

func (s *operationService) DeferOperation(ctx context.Context, req *controlpb.DeferOperationRequest) (*controlpb.DeferOperationResponse, error) {
	if req.GetState() == controlpb.DeferState_DEFER_STATE_UNSPECIFIED || req.GetReason() == controlpb.DeferReason_DEFER_REASON_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "invalid_defer")
	}
	body := core.Object{"epoch": req.GetEpoch(), "version": req.GetVersion(), "state": name(req.GetState().String(), "DEFER_STATE_"), "errorCode": name(req.GetReason().String(), "DEFER_REASON_"), "retrySeconds": int64(req.GetRetrySeconds())}
	out, e := control(ctx, s.store, "defer", req.GetOperationId(), req.GetSubmissionId(), body)
	if e != nil {
		return nil, e
	}
	return &controlpb.DeferOperationResponse{Operation: operation(out)}, nil
}

// control runs one action for the verified principal and maps its Fault.
func control(ctx context.Context, store *core.Store, action, operationID, submission string, body core.Object) (core.Object, error) {
	out, e := store.Control(ctx, &core.ControlRequest{Action: action, OperationID: operationID, SubmissionID: submission, Body: body, Service: principal(ctx)})
	if e != nil {
		return nil, toStatus(e)
	}
	return out, nil
}

// name turns a contract enum name (EFFECT_KIND_SANDBOX_ENSURE) into the stored value
// (sandbox_ensure); enum returns the reverse, and 0 (UNSPECIFIED) for a stored value the contract
// does not carry, such as a retired step of a historical operation.
func name(enumName, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(enumName, prefix))
}

func enum(values map[string]int32, prefix, stored string) int32 {
	return values[prefix+strings.ToUpper(stored)]
}

// rows reads a list the store returned, whether freshly queried or replayed from JSON.
func rows(v any) []core.Object {
	switch list := v.(type) {
	case []core.Object:
		return list
	case []any:
		out := make([]core.Object, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, core.Object(m))
			}
		}
		return out
	}
	return nil
}

func optionalString(o core.Object, k string) *string {
	if v, ok := o[k].(string); ok {
		return &v
	}
	return nil
}

func optionalInt(o core.Object, k string) *int64 {
	if o[k] == nil {
		return nil
	}
	v := o.N(k)
	return &v
}

func operation(o core.Object) *controlpb.Operation {
	return &controlpb.Operation{
		Id: o.S("id"), ProjectId: o.S("projectId"), WorkspaceId: optionalString(o, "workspaceId"), Version: o.N("version"),
		Kind:            controlpb.OperationKind(enum(controlpb.OperationKind_value, "OPERATION_KIND_", o.S("kind"))),
		State:           controlpb.OperationState(enum(controlpb.OperationState_value, "OPERATION_STATE_", o.S("state"))),
		Step:            controlpb.OperationStep(enum(controlpb.OperationStep_value, "OPERATION_STEP_", o.S("step"))),
		ControllerEpoch: optionalInt(o, "controllerEpoch"), ErrorCode: optionalString(o, "errorCode"),
	}
}

func snapshot(out core.Object) *controlpb.OperationSnapshot {
	p := out.O("project")
	snap := &controlpb.OperationSnapshot{
		Operation: operation(out.O("operation")),
		Project:   &controlpb.OperationProject{Id: p.S("id"), RepositoryUrl: p.S("repositoryUrl"), DefaultBranch: p.S("defaultBranch"), CredentialRef: optionalString(p, "secretRef")},
	}
	for _, w := range rows(out["workspaces"]) {
		snap.Workspaces = append(snap.Workspaces, &controlpb.OperationWorkspace{
			Id: w.S("id"), Kind: controlpb.WorkspaceKind(enum(controlpb.WorkspaceKind_value, "WORKSPACE_KIND_", w.S("kind"))),
			DesiredState: w.S("desiredState"), ObservedState: w.S("observedState"), RuntimeGeneration: w.N("runtimeGeneration"),
			AdmissionOpen: w.B("admissionOpen"), AdmissionEpoch: w.N("admissionEpoch"), RequestedRef: w.S("requestedRef"),
			BaseCommitId: optionalString(w, "baseCommitId"), Version: w.N("version"),
		})
	}
	for _, sb := range rows(out["sandboxes"]) {
		snap.Sandboxes = append(snap.Sandboxes, &controlpb.SandboxRecord{Id: sb.S("id"), WorkspaceId: sb.S("workspaceId"), Generation: sb.N("generation"), SubstrateSandboxId: optionalString(sb, "substrateSandboxId"), ObservedState: sb.S("observedState")})
	}
	for _, n := range rows(out["nodes"]) {
		snap.Nodes = append(snap.Nodes, node(n))
	}
	for _, e := range rows(out["effects"]) {
		// Retired storage and worktree effects have no contract message; they are history only.
		if enum(controlpb.EffectKind_value, "EFFECT_KIND_", e.S("kind")) != 0 {
			snap.Effects = append(snap.Effects, effect(e))
		}
	}
	for _, c := range rows(out["clones"]) {
		snap.Clones = append(snap.Clones, record(c))
	}
	return snap
}

func effect(e core.Object) *controlpb.Effect {
	kind := controlpb.EffectKind(enum(controlpb.EffectKind_value, "EFFECT_KIND_", e.S("kind")))
	out := &controlpb.Effect{
		Id: e.S("id"), Kind: kind, State: controlpb.EffectState(enum(controlpb.EffectState_value, "EFFECT_STATE_", e.S("state"))),
		WorkspaceId: e.S("workspaceId"), Request: effectRequest(kind, e.O("request")), ExternalId: optionalString(e, "externalId"),
		ReconciledEpoch: e.N("reconciledEpoch"),
	}
	result := e.O("result")
	switch e.S("state") {
	case "succeeded":
		out.Evidence = evidence(kind, result)
	case "failed":
		out.Failure = optionalString(result, "error")
	}
	return out
}

func effectRequest(kind controlpb.EffectKind, r core.Object) *controlpb.EffectRequest {
	pid, wid := r.S("projectId"), r.S("workspaceId")
	switch kind {
	case controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE:
		return &controlpb.EffectRequest{Request: &controlpb.EffectRequest_SandboxEnsure{SandboxEnsure: &controlpb.SandboxEnsureRequest{ProjectId: pid, WorkspaceId: wid}}}
	case controlpb.EffectKind_EFFECT_KIND_SANDBOX_TERMINATE:
		return &controlpb.EffectRequest{Request: &controlpb.EffectRequest_SandboxTerminate{SandboxTerminate: &controlpb.SandboxTerminateRequest{ProjectId: pid, WorkspaceId: wid, SandboxInstanceId: r.S("sandboxInstanceId")}}}
	case controlpb.EffectKind_EFFECT_KIND_WORKSPACE_DATA_DELETE:
		return &controlpb.EffectRequest{Request: &controlpb.EffectRequest_WorkspaceDataDelete{WorkspaceDataDelete: &controlpb.WorkspaceDataDeleteRequest{ProjectId: pid, WorkspaceId: wid}}}
	case controlpb.EffectKind_EFFECT_KIND_PLUGIN_ENSURE:
		ensure := &controlpb.PluginEnsureRequest{ProjectId: pid, WorkspaceId: wid, PluginId: r.S("pluginId"), Version: r.S("version")}
		if u := r.O("universal"); u.S("url") != "" {
			ensure.Universal = &controlpb.PluginArtifact{Url: u.S("url"), Sha256: u.S("sha256")}
		}
		for _, target := range rows(r["targets"]) {
			ensure.Targets = append(ensure.Targets, &controlpb.PluginArtifact{Target: optionalString(target, "target"), Url: target.S("url"), Sha256: target.S("sha256")})
		}
		return &controlpb.EffectRequest{Request: &controlpb.EffectRequest_PluginEnsure{PluginEnsure: ensure}}
	case controlpb.EffectKind_EFFECT_KIND_PLUGIN_DELETE:
		return &controlpb.EffectRequest{Request: &controlpb.EffectRequest_PluginDelete{PluginDelete: &controlpb.PluginDeleteRequest{ProjectId: pid, WorkspaceId: wid, PluginId: r.S("pluginId"), Version: r.S("version")}}}
	default:
		return &controlpb.EffectRequest{}
	}
}

// evidenceObject is the durable JSON form of success evidence, exactly what the JSON internal API
// stores, so both protocols compare and replay against the same record.
func evidenceObject(ev *controlpb.EffectEvidence) (core.Object, bool) {
	switch v := ev.GetEvidence().(type) {
	case *controlpb.EffectEvidence_SandboxEnsured:
		return core.Object{"sandboxInstanceId": v.SandboxEnsured.GetSandboxInstanceId(), "nodeId": v.SandboxEnsured.GetNodeId()}, true
	case *controlpb.EffectEvidence_SandboxTerminated:
		return core.Object{"terminated": true}, true
	case *controlpb.EffectEvidence_WorkspaceDataDeleted:
		return core.Object{"removed": true}, true
	case *controlpb.EffectEvidence_PluginInstalled:
		return core.Object{"installed": true, "version": v.PluginInstalled.GetVersion()}, true
	case *controlpb.EffectEvidence_PluginRemoved:
		return core.Object{"removed": true}, true
	default:
		return nil, false
	}
}

func evidence(kind controlpb.EffectKind, r core.Object) *controlpb.EffectEvidence {
	switch kind {
	case controlpb.EffectKind_EFFECT_KIND_SANDBOX_ENSURE:
		return &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxEnsured{SandboxEnsured: &controlpb.SandboxEnsured{SandboxInstanceId: r.S("sandboxInstanceId"), NodeId: r.S("nodeId")}}}
	case controlpb.EffectKind_EFFECT_KIND_SANDBOX_TERMINATE:
		return &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_SandboxTerminated{SandboxTerminated: &controlpb.SandboxTerminated{}}}
	case controlpb.EffectKind_EFFECT_KIND_WORKSPACE_DATA_DELETE:
		return &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_WorkspaceDataDeleted{WorkspaceDataDeleted: &controlpb.WorkspaceDataDeleted{}}}
	case controlpb.EffectKind_EFFECT_KIND_PLUGIN_ENSURE:
		return &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_PluginInstalled{PluginInstalled: &controlpb.PluginInstalled{Version: r.S("version")}}}
	case controlpb.EffectKind_EFFECT_KIND_PLUGIN_DELETE:
		return &controlpb.EffectEvidence{Evidence: &controlpb.EffectEvidence_PluginRemoved{PluginRemoved: &controlpb.PluginRemoved{}}}
	default:
		return nil
	}
}
