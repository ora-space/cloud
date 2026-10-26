package controlgrpc

import (
	"encoding/json"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// agentConfigurationMessage transports only the immutable private configuration snapshot.
func agentConfigurationMessage(o core.Object) *controlpb.AgentModelConfiguration {
	if len(o) == 0 {
		return nil
	}
	model := o.O("model")
	return &controlpb.AgentModelConfiguration{
		PluginId: o.S("pluginId"), PluginVersion: o.S("pluginVersion"), Target: o.S("target"), PackageSha256: o.S("packageSha256"),
		SchemaVersion: boundedSchemaVersion(o.N("schemaVersion")), DeclarationFingerprint: o.S("declarationFingerprint"), AdapterVersion: boundedSchemaVersion(o.N("adapterVersion")),
		ConfigurationRevision: nonnegativeModelValue(o.N("configurationRevision")), SettingsJson: o.S("settingsJson"),
		Protocol: o.S("protocol"), Model: &controlpb.AgentBoundModel{Id: model.S("id"), Name: model.S("name"), ContextWindow: nonnegativeModelValue(model.N("contextWindow")), MaxTokens: nonnegativeModelValue(model.N("maxTokens"))},
	}
}

// agentConfigurationObject retains exact JSON numbers when registering a dispatched execution.
func agentConfigurationObject(p *controlpb.AgentModelConfiguration) (core.Object, error) {
	var settings core.Object
	decoder := json.NewDecoder(strings.NewReader(p.GetSettingsJson()))
	decoder.UseNumber()
	revision, revisionOK := receiptSequence(p.GetConfigurationRevision())
	window, windowOK := receiptSequence(p.GetModel().GetContextWindow())
	tokens, tokensOK := receiptSequence(p.GetModel().GetMaxTokens())
	if !revisionOK || !windowOK || !tokensOK || len(p.GetSettingsJson()) > 256*1024 || decoder.Decode(&settings) != nil || settings == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, status.Error(codes.InvalidArgument, "invalid_dispatch")
	}
	return core.Object{
		"pluginId": p.GetPluginId(), "pluginVersion": p.GetPluginVersion(), "target": p.GetTarget(), "packageSha256": p.GetPackageSha256(),
		"schemaVersion": int64(p.GetSchemaVersion()), "declarationFingerprint": p.GetDeclarationFingerprint(), "adapterVersion": int64(p.GetAdapterVersion()),
		"configurationRevision": revision, "settingsJson": p.GetSettingsJson(),
		"protocol": p.GetProtocol(), "model": core.Object{"id": p.GetModel().GetId(), "name": p.GetModel().GetName(), "contextWindow": window, "maxTokens": tokens},
	}, nil
}

// A corrupt negative durable value becomes unequal wire input, never a wrapped unsigned identity.
func nonnegativeModelValue(value int64) uint64 {
	if value < 0 {
		return 0
	}
	return uint64(value) // #nosec G115 -- negative values are rejected above.
}
