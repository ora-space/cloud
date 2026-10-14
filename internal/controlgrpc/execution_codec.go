package controlgrpc

import (
	"math"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// The JSON shapes below are the durable form of the contract messages. Conflicts compare this
// encoding, so a replayed dispatch with the same input matches and a different one does not.

func inputObject(in *controlpb.ExecutionInput) (core.Object, error) {
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid_dispatch")
	}
	switch spec := in.GetSpec().(type) {
	case *controlpb.ExecutionInput_Clone:
		if spec.Clone.GetRepository() == "" || spec.Clone.GetBranch() == "" {
			return nil, status.Error(codes.InvalidArgument, "invalid_dispatch")
		}
		return core.Object{"kind": "clone", "repositoryUrl": spec.Clone.GetRepository(), "branch": spec.Clone.GetBranch()}, nil
	case *controlpb.ExecutionInput_InstallPlugins:
		return core.Object{"kind": "install_plugins", "plugins": installObjects(spec.InstallPlugins.GetPlugins())}, nil
	case *controlpb.ExecutionInput_RemovePlugins:
		return core.Object{"kind": "remove_plugins", "plugins": removalObjects(spec.RemovePlugins.GetPlugins())}, nil
	case *controlpb.ExecutionInput_AgentSession:
		return sessionObject(spec.AgentSession)
	case *controlpb.ExecutionInput_DeliverRevision:
		return deliveryObject(spec.DeliverRevision)
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid_dispatch")
	}
}

func input(o core.Object) *controlpb.ExecutionInput {
	switch o.S("kind") {
	case "install_plugins":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_InstallPlugins{InstallPlugins: &controlpb.InstallPluginsSpec{Plugins: installMessages(rows(o["plugins"]))}}}
	case "remove_plugins":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_RemovePlugins{RemovePlugins: &controlpb.RemovePluginsSpec{Plugins: removalMessages(rows(o["plugins"]))}}}
	case "agent_session":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_AgentSession{AgentSession: sessionMessage(o)}}
	case "deliver_revision":
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_DeliverRevision{DeliverRevision: deliveryMessage(o)}}
	default:
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_Clone{Clone: &controlpb.CloneSpec{Repository: o.S("repositoryUrl"), Branch: o.S("branch")}}}
	}
}

func installObjects(plugins []*controlpb.PluginInstall) []core.Object {
	out := make([]core.Object, 0, len(plugins))
	for _, plugin := range plugins {
		item := core.Object{"pluginId": plugin.GetPluginId(), "version": plugin.GetVersion()}
		if plugin.GetUniversal() != nil {
			item["universal"] = core.Object{"url": plugin.GetUniversal().GetUrl(), "sha256": plugin.GetUniversal().GetSha256()}
		}
		if len(plugin.GetTargets()) > 0 {
			targets := make([]core.Object, 0, len(plugin.GetTargets()))
			for _, target := range plugin.GetTargets() {
				targets = append(targets, core.Object{"target": target.GetTarget(), "url": target.GetDownload().GetUrl(), "sha256": target.GetDownload().GetSha256()})
			}
			item["targets"] = targets
		}
		out = append(out, item)
	}
	return out
}

func installMessages(plugins []core.Object) []*controlpb.PluginInstall {
	out := make([]*controlpb.PluginInstall, 0, len(plugins))
	for _, plugin := range plugins {
		item := &controlpb.PluginInstall{PluginId: plugin.S("pluginId"), Version: plugin.S("version")}
		if universal := plugin.O("universal"); universal.S("url") != "" {
			item.Universal = &controlpb.PluginDownload{Url: universal.S("url"), Sha256: universal.S("sha256")}
		}
		for _, target := range rows(plugin["targets"]) {
			item.Targets = append(item.Targets, &controlpb.PluginTargetDownload{Target: target.S("target"), Download: &controlpb.PluginDownload{Url: target.S("url"), Sha256: target.S("sha256")}})
		}
		out = append(out, item)
	}
	return out
}

func removalObjects(plugins []*controlpb.PluginRemoval) []core.Object {
	out := make([]core.Object, 0, len(plugins))
	for _, plugin := range plugins {
		out = append(out, core.Object{"pluginId": plugin.GetPluginId(), "version": plugin.GetVersion()})
	}
	return out
}

func removalMessages(plugins []core.Object) []*controlpb.PluginRemoval {
	out := make([]*controlpb.PluginRemoval, 0, len(plugins))
	for _, plugin := range plugins {
		out = append(out, &controlpb.PluginRemoval{PluginId: plugin.S("pluginId"), Version: plugin.S("version")})
	}
	return out
}

func sessionObject(spec *controlpb.AgentSessionSpec) (core.Object, error) {
	if spec.GetAgentPluginId() == "" || spec.GetCheckoutExecutionId() == "" || spec.GetGitIdentity().GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_dispatch")
	}
	turn := spec.GetInitialTurn()
	out := core.Object{
		"kind": "agent_session", "agentPluginId": spec.GetAgentPluginId(), "agentPluginVersion": spec.GetAgentPluginVersion(),
		"checkoutExecutionId": spec.GetCheckoutExecutionId(),
		"gitIdentity":         core.Object{"name": spec.GetGitIdentity().GetName(), "email": spec.GetGitIdentity().GetEmail()},
		"initialTurn":         core.Object{"turnId": turn.GetTurnId(), "content": contentObjects(turn.GetContent())},
	}
	if id := spec.GetModelBindingId(); id != "" {
		out["modelBindingId"] = id
	}
	if prior := spec.GetPriorRevision(); prior != nil {
		out["priorRevision"] = priorObject(prior)
	}
	return out, nil
}

// priorObject is the durable form of a PriorRevision; a delivery spec's has no bundle.
func priorObject(prior *controlpb.PriorRevision) core.Object {
	out := core.Object{"revisionId": prior.GetRevisionId(), "finalCommit": prior.GetFinalCommit()}
	if prior.GetBundle() != nil {
		out["bundle"] = storedObject(prior.GetBundle())
	}
	return out
}

func priorMessage(o core.Object) *controlpb.PriorRevision {
	if o.S("revisionId") == "" {
		return nil
	}
	prior := &controlpb.PriorRevision{RevisionId: o.S("revisionId"), FinalCommit: o.S("finalCommit")}
	if bundle := o.O("bundle"); len(bundle) > 0 {
		prior.Bundle = storedMessage(bundle)
	}
	return prior
}

func sessionMessage(o core.Object) *controlpb.AgentSessionSpec {
	git := o.O("gitIdentity")
	turn := o.O("initialTurn")
	return &controlpb.AgentSessionSpec{
		AgentPluginId: o.S("agentPluginId"), AgentPluginVersion: o.S("agentPluginVersion"), CheckoutExecutionId: o.S("checkoutExecutionId"),
		ModelBindingId: o.S("modelBindingId"),
		GitIdentity:    &controlpb.GitIdentity{Name: git.S("name"), Email: git.S("email")},
		InitialTurn:    &controlpb.UserTurn{TurnId: turn.S("turnId"), Content: contentMessages(rows(turn["content"]))},
		PriorRevision:  priorMessage(o.O("priorRevision")),
	}
}

func contentObjects(blocks []*controlpb.ContentBlock) []core.Object {
	out := make([]core.Object, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, core.Object{"text": block.GetText().GetText()})
	}
	return out
}

func contentMessages(blocks []core.Object) []*controlpb.ContentBlock {
	out := make([]*controlpb.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, &controlpb.ContentBlock{Block: &controlpb.ContentBlock_Text{Text: &controlpb.TextContent{Text: block.S("text")}}})
	}
	return out
}

func deliveryObject(spec *controlpb.DeliverRevisionSpec) (core.Object, error) {
	if spec.GetSessionExecutionId() == "" || spec.GetBundleKey() == "" || spec.GetHistoryKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_dispatch")
	}
	out := core.Object{
		"kind": "deliver_revision", "sessionExecutionId": spec.GetSessionExecutionId(), "checkoutExecutionId": spec.GetCheckoutExecutionId(),
		"baseCommit": spec.GetBaseCommit(), "revisionRef": spec.GetRevisionRef(), "bundleKey": spec.GetBundleKey(), "historyKey": spec.GetHistoryKey(),
	}
	if prior := spec.GetPriorRevision(); prior != nil {
		out["priorRevision"] = priorObject(prior)
	}
	return out, nil
}

func deliveryMessage(o core.Object) *controlpb.DeliverRevisionSpec {
	return &controlpb.DeliverRevisionSpec{
		SessionExecutionId: o.S("sessionExecutionId"), CheckoutExecutionId: o.S("checkoutExecutionId"), BaseCommit: o.S("baseCommit"),
		RevisionRef: o.S("revisionRef"), BundleKey: o.S("bundleKey"), HistoryKey: o.S("historyKey"),
		PriorRevision: priorMessage(o.O("priorRevision")),
	}
}

func resultObject(res *controlpb.ExecutionResult) (core.Object, error) {
	node := res.GetNode()
	if node == nil || node.GetNodeId() == "" || node.GetNodeIncarnationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid_result")
	}
	out := core.Object{"node": core.Object{"nodeId": node.GetNodeId(), "nodeIncarnationId": node.GetNodeIncarnationId()}}
	switch outcome := res.GetOutcome().(type) {
	case *controlpb.ExecutionResult_CloneReady:
		out["outcome"], out["path"], out["commit"] = "clone_ready", outcome.CloneReady.GetPath(), outcome.CloneReady.GetCommit()
	case *controlpb.ExecutionResult_CloneFailed:
		out["outcome"], out["reason"] = "clone_failed", outcome.CloneFailed.GetReason().String()
		if outcome.CloneFailed.RetainedPath != nil {
			out["retainedPath"] = outcome.CloneFailed.GetRetainedPath()
		}
	case *controlpb.ExecutionResult_PluginsResult:
		out["outcome"] = "plugins_result"
		out["items"] = pluginItemObjects(outcome.PluginsResult.GetItems())
	case *controlpb.ExecutionResult_PluginsFailed:
		out["outcome"], out["reason"] = "plugins_failed", shortEnum(outcome.PluginsFailed.GetReason().String(), "PLUGINS_FAILURE_REASON_")
	case *controlpb.ExecutionResult_AgentSessionEnded:
		out["outcome"], out["reason"] = "agent_session_ended", shortEnum(outcome.AgentSessionEnded.GetReason().String(), "AGENT_SESSION_END_REASON_")
		if outcome.AgentSessionEnded.Detail != nil {
			out["detail"] = outcome.AgentSessionEnded.GetDetail()
		}
	case *controlpb.ExecutionResult_RevisionDelivered:
		out["outcome"] = "revision_delivered"
		delivered := outcome.RevisionDelivered
		out["finalCommit"], out["baseCommit"], out["revisionRef"] = delivered.GetFinalCommit(), delivered.GetBaseCommit(), delivered.GetRevisionRef()
		out["bundle"], out["history"] = storedObject(delivered.GetBundle()), storedObject(delivered.GetHistory())
	case *controlpb.ExecutionResult_RevisionUnchanged:
		out["outcome"] = "revision_unchanged"
		unchanged := outcome.RevisionUnchanged
		out["finalCommit"], out["baseCommit"], out["revisionRef"] = unchanged.GetFinalCommit(), unchanged.GetBaseCommit(), unchanged.GetRevisionRef()
		out["history"] = storedObject(unchanged.GetHistory())
	case *controlpb.ExecutionResult_RevisionFailed:
		out["outcome"], out["reason"] = "revision_failed", shortEnum(outcome.RevisionFailed.GetReason().String(), "REVISION_FAILURE_REASON_")
	default:
		return nil, status.Error(codes.InvalidArgument, "invalid_result")
	}
	return out, nil
}

func pluginItemObjects(items []*controlpb.PluginItemResult) []core.Object {
	out := make([]core.Object, 0, len(items))
	for _, item := range items {
		row := core.Object{"pluginId": item.GetPluginId()}
		switch outcome := item.GetOutcome().(type) {
		case *controlpb.PluginItemResult_Installed:
			row["outcome"], row["version"] = "installed", outcome.Installed.GetVersion()
		case *controlpb.PluginItemResult_Removed:
			row["outcome"] = "removed"
		case *controlpb.PluginItemResult_Failed:
			row["outcome"], row["reason"] = "failed", shortEnum(outcome.Failed.GetReason().String(), "PLUGIN_FAILURE_REASON_")
		}
		out = append(out, row)
	}
	return out
}

func storedObject(o *controlpb.StoredObject) core.Object {
	if o == nil {
		return nil
	}
	size := o.GetSize()
	stored := int64(0)
	if size <= math.MaxInt64 {
		stored = int64(size) // #nosec G115 -- bounded by MaxInt64 above.
	}
	return core.Object{"key": o.GetKey(), "size": stored, "sha256": o.GetSha256()}
}

func result(o core.Object) *controlpb.ExecutionResult {
	node := o.O("node")
	out := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node.S("nodeId"), NodeIncarnationId: node.S("nodeIncarnationId")}}
	switch o.S("outcome") {
	case "clone_ready":
		out.Outcome = &controlpb.ExecutionResult_CloneReady{CloneReady: &controlpb.CloneReady{Path: o.S("path"), Commit: o.S("commit")}}
	case "plugins_result":
		out.Outcome = &controlpb.ExecutionResult_PluginsResult{PluginsResult: &controlpb.PluginsResult{Items: pluginItemMessages(rows(o["items"]))}}
	case "plugins_failed":
		out.Outcome = &controlpb.ExecutionResult_PluginsFailed{PluginsFailed: &controlpb.PluginsFailed{Reason: controlpb.PluginsFailureReason(enum(controlpb.PluginsFailureReason_value, "PLUGINS_FAILURE_REASON_", o.S("reason")))}}
	case "agent_session_ended":
		ended := &controlpb.AgentSessionEnded{Reason: controlpb.AgentSessionEndReason(enum(controlpb.AgentSessionEndReason_value, "AGENT_SESSION_END_REASON_", o.S("reason")))}
		if detail := o.S("detail"); detail != "" {
			ended.Detail = &detail
		}
		out.Outcome = &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: ended}
	case "revision_delivered":
		out.Outcome = &controlpb.ExecutionResult_RevisionDelivered{RevisionDelivered: &controlpb.RevisionDelivered{FinalCommit: o.S("finalCommit"), BaseCommit: o.S("baseCommit"), RevisionRef: o.S("revisionRef"), Bundle: storedMessage(o.O("bundle")), History: storedMessage(o.O("history"))}}
	case "revision_unchanged":
		out.Outcome = &controlpb.ExecutionResult_RevisionUnchanged{RevisionUnchanged: &controlpb.RevisionUnchanged{FinalCommit: o.S("finalCommit"), BaseCommit: o.S("baseCommit"), RevisionRef: o.S("revisionRef"), History: storedMessage(o.O("history"))}}
	case "revision_failed":
		out.Outcome = &controlpb.ExecutionResult_RevisionFailed{RevisionFailed: &controlpb.RevisionFailed{Reason: controlpb.RevisionFailureReason(enum(controlpb.RevisionFailureReason_value, "REVISION_FAILURE_REASON_", o.S("reason")))}}
	default:
		failed := &controlpb.CloneFailed{Reason: controlpb.CloneFailureReason(controlpb.CloneFailureReason_value[o.S("reason")])}
		if retained, ok := o["retainedPath"].(string); ok {
			failed.RetainedPath = &retained
		}
		out.Outcome = &controlpb.ExecutionResult_CloneFailed{CloneFailed: failed}
	}
	return out
}

func pluginItemMessages(items []core.Object) []*controlpb.PluginItemResult {
	out := make([]*controlpb.PluginItemResult, 0, len(items))
	for _, item := range items {
		row := &controlpb.PluginItemResult{PluginId: item.S("pluginId")}
		switch item.S("outcome") {
		case "installed":
			row.Outcome = &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: item.S("version")}}
		case "removed":
			row.Outcome = &controlpb.PluginItemResult_Removed{Removed: &controlpb.PluginItemRemoved{}}
		default:
			row.Outcome = &controlpb.PluginItemResult_Failed{Failed: &controlpb.PluginItemFailed{Reason: controlpb.PluginFailureReason(enum(controlpb.PluginFailureReason_value, "PLUGIN_FAILURE_REASON_", item.S("reason")))}}
		}
		out = append(out, row)
	}
	return out
}

func storedMessage(o core.Object) *controlpb.StoredObject {
	size := o.N("size")
	if size < 0 {
		size = 0
	}
	return &controlpb.StoredObject{Key: o.S("key"), Size: uint64(size), Sha256: o.S("sha256")}
}

func record(row core.Object) *controlpb.ExecutionRecord {
	out := &controlpb.ExecutionRecord{OperationId: row.S("operationId"), NodeOperationId: row.S("nodeOperationId"), ExecutionId: row.S("executionId"), NodeId: row.S("nodeId"), Input: input(row.O("input"))}
	if res := row.O("result"); len(res) > 0 {
		out.Result = result(res)
	}
	return out
}

func shortEnum(enumName, prefix string) string { return name(enumName, prefix) }

// grantMessage converts one in-memory upload grant. The URL is not read back from the database.
func grantMessage(o core.Object) *controlpb.UploadGrant {
	headers := map[string]string{}
	switch raw := o["headers"].(type) {
	case map[string]string:
		headers = raw
	case map[string]any:
		for k, v := range raw {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}
	grant := &controlpb.UploadGrant{ObjectKey: o.S("objectKey"), Url: o.S("url"), Method: o.S("method"), Headers: headers}
	if exp, ok := o["expiresAt"].(time.Time); ok {
		grant.ExpiresAt = timestamppb.New(exp)
	}
	return grant
}
