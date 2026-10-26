package simulator

import (
	"context"
	"fmt"
	"math"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/wanglongan587/cloud/internal/controlgrpc"
	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// plugins registers the operation's frozen plugin input and reports a successful item for each one.
// An empty set is advanced by Cloud without an execution. This is a test double of the Node installer,
// not evidence that a package was downloaded or checked.
func (c *Controller) plugins(ctx context.Context, snap core.Object, nodes []core.Object) error {
	if c.Executions == nil {
		return fmt.Errorf("simulator controller has no execution client for the plugin step")
	}
	input := snap.O("pluginInput")
	plugins, e := objects(input, "plugins")
	if e != nil {
		return e
	}
	if len(plugins) == 0 {
		return nil
	}
	wid := c.Operation.S("workspaceId")
	var node core.Object
	for _, candidate := range nodes {
		if candidate.S("workspaceId") == wid && candidate["endedAt"] == nil {
			node = candidate
		}
	}
	if node == nil {
		return fmt.Errorf("plugin step without a node")
	}
	executions, e := objects(snap, "pluginExecutions")
	if e != nil {
		return e
	}
	execution := ""
	for _, existing := range executions {
		if existing.O("result").S("outcome") == "plugins_result" {
			return nil
		}
		if existing["result"] == nil {
			execution = existing.S("executionId")
		}
	}
	ctx = metadata.AppendToOutgoingContext(ctx, controlgrpc.HolderMetadata, c.Client.Subject)
	spec, e := pluginSpec(input.S("kind"), plugins)
	if e != nil {
		return e
	}
	if execution == "" {
		execution = uuid.NewString()
		if _, e = c.Executions.RecordDispatch(ctx, &controlpb.RecordDispatchRequest{SubmissionId: "dispatch-" + execution, Epoch: c.Epoch, OperationId: c.Operation.S("id"), ExecutionId: execution, NodeId: node.S("nodeId"), Input: spec}); e != nil {
			return fmt.Errorf("record plugin dispatch: %w", e)
		}
	}
	result := &controlpb.ExecutionResult{Node: &controlpb.NodeIdentity{NodeId: node.S("nodeId"), NodeIncarnationId: node.S("nodeIncarnationId")}}
	if input.S("kind") == "remove_plugins" {
		result.Outcome = &controlpb.ExecutionResult_PluginsResult{PluginsResult: &controlpb.PluginsResult{Items: removedItems(plugins)}}
	} else {
		result.Outcome = &controlpb.ExecutionResult_PluginsResult{PluginsResult: &controlpb.PluginsResult{Items: installedItems(plugins)}}
	}
	if _, e = c.Executions.RecordQueriedResult(ctx, &controlpb.RecordQueriedResultRequest{SubmissionId: "result-" + execution, Epoch: c.Epoch, OperationId: c.Operation.S("id"), ExecutionId: execution, Result: result}); e != nil {
		return fmt.Errorf("record plugin result: %w", e)
	}
	return nil
}

func pluginSpec(kind string, plugins []core.Object) (*controlpb.ExecutionInput, error) {
	if kind == "remove_plugins" {
		items := make([]*controlpb.PluginRemoval, 0, len(plugins))
		for _, plugin := range plugins {
			items = append(items, &controlpb.PluginRemoval{PluginId: plugin.S("pluginId"), Version: plugin.S("version")})
		}
		return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_RemovePlugins{RemovePlugins: &controlpb.RemovePluginsSpec{Plugins: items}}}, nil
	}
	items := make([]*controlpb.PluginInstall, 0, len(plugins))
	for _, plugin := range plugins {
		item := &controlpb.PluginInstall{PluginId: plugin.S("pluginId"), Version: plugin.S("version")}
		if universal := plugin.O("universal"); universal.S("url") != "" {
			item.Universal = &controlpb.PluginDownload{Url: universal.S("url"), Sha256: universal.S("sha256")}
		}
		targets, err := objects(plugin, "targets")
		if err != nil {
			return nil, err
		}
		for _, target := range targets {
			item.Targets = append(item.Targets, &controlpb.PluginTargetDownload{Target: target.S("target"), Download: &controlpb.PluginDownload{Url: target.S("url"), Sha256: target.S("sha256")}})
		}
		if agent, declared := core.PluginAgentConfiguration(plugin); declared {
			version := agent.N("schemaVersion")
			if version < 0 || version > math.MaxUint32 {
				version = 0
			}
			item.AgentConfiguration = &controlpb.PluginAgentConfiguration{SchemaVersion: uint32(version), DeclarationSha256: agent.S("declarationSha256")} // #nosec G115 -- bounded above.
		}
		items = append(items, item)
	}
	return &controlpb.ExecutionInput{Spec: &controlpb.ExecutionInput_InstallPlugins{InstallPlugins: &controlpb.InstallPluginsSpec{Plugins: items}}}, nil
}

func installedItems(plugins []core.Object) []*controlpb.PluginItemResult {
	items := make([]*controlpb.PluginItemResult, 0, len(plugins))
	for _, plugin := range plugins {
		items = append(items, &controlpb.PluginItemResult{PluginId: plugin.S("pluginId"), Outcome: &controlpb.PluginItemResult_Installed{Installed: &controlpb.PluginItemInstalled{Version: plugin.S("version")}}})
	}
	return items
}

func removedItems(plugins []core.Object) []*controlpb.PluginItemResult {
	items := make([]*controlpb.PluginItemResult, 0, len(plugins))
	for _, plugin := range plugins {
		items = append(items, &controlpb.PluginItemResult{PluginId: plugin.S("pluginId"), Outcome: &controlpb.PluginItemResult_Removed{Removed: &controlpb.PluginItemRemoved{}}})
	}
	return items
}
