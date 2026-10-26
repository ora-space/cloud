package core

import (
	"regexp"
	"slices"
)

// PersonalAgentRuntime describes the deployed adapter contract and one known execution target.
// Production must leave it nil until the Node/Controller adapter path can consume frozen inputs.
// Declaration metadata alone never establishes that execution capability.
type PersonalAgentRuntime struct {
	Target          string
	ContractVersion uint32
	Protocols       []string
}

const claudeAgentCLI = "official/ora-space.claude"

var agentPackageDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// declaredAgentRunModel resolves and freezes the private Agent configuration in the run's
// creation transaction. False preserves only existing undeclared routes; Claude never falls
// back to a host login or OpenCode's default when its declaration is absent. A non-empty
// selectedModel is a scheduled definition's explicit override: it is resolved and validated by the
// same protocol gates as the declared configuration, so an incompatible override is refused here
// rather than frozen into a binding the adapter cannot execute.
func declaredAgentRunModel(t *transaction, runID, uid string, input Object, selectedModel string) (handled bool, connection, model Object) {
	pluginID := input.S("agentPluginId")
	context := t.one(`SELECT sa.space_id
		FROM issue_runs r JOIN space_agents sa ON sa.id=r.executor_id AND sa.tenant_id=r.tenant_id
		WHERE r.id=$1 AND sa.status='active' AND sa.plugin_id=$2`, runID, pluginID)
	if context == nil {
		require(pluginID != claudeAgentCLI, 409, "agent_declaration_required")
		return false, nil, nil
	}
	frozen, connection, model := resolveDeclaredAgentRun(t, uid, context.S("spaceId"), pluginID, input.S("agentPluginVersion"), selectedModel)
	if frozen == nil {
		return false, nil, nil
	}
	t.exec(`INSERT INTO run_agent_configurations(run_id,plugin_id,plugin_version,target,package_sha256,
		declaration_schema_version,declaration_fingerprint,adapter_contract_version,configuration_revision,selection_revision,settings,release)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, runID, pluginID, frozen.S("pluginVersion"),
		frozen.S("target"), frozen.S("packageSha256"), frozen.N("schemaVersion"), frozen.S("fingerprint"),
		frozen.N("contractVersion"), frozen.N("revision"), frozen.N("selectionRevision"),
		jsonText(frozen.O("settings")), jsonText(frozen.O("release")))
	return true, connection, model
}

// resolveDeclaredAgentRun is the read-only policy shared by readiness, queue admission and
// final run creation. The caller either reports its fault or atomically persists its result.
// selectedModel is a scheduled definition's optional model override: when set, it replaces the
// declared connection/model and the full protocol gates below judge the final pair, so an override
// that lands on a connection the Agent or runtime cannot execute fails here exactly like a broken
// declaration instead of being frozen and rejected later by the adapter.
func resolveDeclaredAgentRun(t *transaction, uid, spaceID, pluginID, pluginVersion, selectedModel string) (snapshot, connection, model Object) {
	namespace, identifier, ok := pluginIdentity(pluginID)
	require(ok, 400, "invalid_plugin_id")
	selected := t.spacePluginRow(spaceID, namespace, identifier)
	require(selected.O("selectedRelease").S("agentConfigurationIssue") == "", 409, "agent_declaration_invalid")
	if selected == nil || selected.O("selectedRelease").O("agentConfiguration").S("declaration") == "" {
		require(pluginID != claudeAgentCLI, 409, "agent_declaration_required")
		return nil, nil, nil
	}
	selection, declaration := agentConfigurationContext(t, uid, spaceID, pluginID)
	require(selection.S("desiredVersion") == pluginVersion, 409, "agent_release_conflict")
	configuration := personalAgentConfiguration(t, uid, pluginID)
	if pluginID != opencodeAgentCLI || len(declaration.Settings) > 0 || !configuration.B("cleared") {
		require(!configuration.B("cleared"), 409, "agent_configuration_required")
		require(configuration.S("declarationFingerprint") == declaration.Fingerprint, 409, "agent_configuration_repair_required")
	}
	settings := resolveAgentSettings(declaration, configuration.O("settings"))
	var reason string
	if pluginID == opencodeAgentCLI {
		reason, connection, model = defaultExecutorGap(t, uid)
	} else {
		reason, connection, model = personalExecutorGap(t, uid, configuration.S("connectionId"), configuration.S("modelId"))
	}
	// A scheduled definition may pin a model on the creator's current default connection. Resolve it
	// before the protocol gates below: the override must be judged by the same rules as the declared
	// pair, so a Claude declaration backed by Anthropic plus a task model on an OpenAI default cannot
	// produce an OpenAI binding the Claude runtime refuses after dispatch.
	if selectedModel != "" {
		selection := modelDefault(t, uid)
		reason, connection, model = personalExecutorGap(t, uid, selection.S("connectionId"), selectedModel)
	}
	require(reason == "", 409, safeAgentModelGap(reason))
	require(slices.Contains(declaration.ModelAccess.Protocols, connection.S("protocol")), 409, "agent_model_protocol_mismatch")
	require(pluginID != claudeAgentCLI || connection.S("protocol") == "anthropic-messages", 409, "agent_model_protocol_mismatch")
	runtime := t.store.PersonalAgentRuntime
	require(runtime != nil && runtime.Target != "" &&
		runtime.ContractVersion == declaration.ModelAccess.ContractVersion, 409, "agent_adapter_unavailable")
	require(slices.Contains(runtime.Protocols, connection.S("protocol")), 409, "agent_adapter_unavailable")
	release := selection.O("selectedRelease")
	digest := agentTargetDigest(release, runtime.Target)
	require(agentPackageDigest.MatchString(digest), 409, "agent_target_unavailable")
	return Object{
		"pluginVersion": release.S("version"), "target": runtime.Target, "packageSha256": digest,
		"schemaVersion": int64(declaration.SchemaVersion), "fingerprint": declaration.Fingerprint,
		"contractVersion": int64(runtime.ContractVersion), "revision": configuration.N("revision"),
		"selectionRevision": selection.N("desiredRevision"), "settings": settings, "release": release,
	}, connection, model
}

// frozenAgentPluginSnapshot overrides the executing Agent even when an administrator has
// upgraded or removed its current selection while this run waited for a Workspace.
func frozenAgentPluginSnapshot(t *transaction, operation Object, plugins, meta []Object) (frozenPlugins, frozenMeta []Object) {
	frozen := t.one(`SELECT c.* FROM run_agent_configurations c JOIN workspaces w ON w.issue_run_id=c.run_id WHERE w.id=$1`, operation.S("workspaceId"))
	if frozen == nil {
		return plugins, meta
	}
	payload := pluginPayload(frozen.O("release"))
	if targets := objectsOf(payload["targets"]); len(targets) > 0 {
		delete(payload, "targets")
		for _, target := range targets {
			if target.S("target") == frozen.S("target") {
				payload["targets"] = []Object{target}
				delete(payload, "universal")
				break
			}
		}
	}
	for i, plugin := range plugins {
		if plugin.S("pluginId") == frozen.S("pluginId") {
			plugins[i] = payload
			for j := range meta {
				if meta[j].S("pluginId") == frozen.S("pluginId") {
					meta[j]["desiredRevision"] = frozen.N("selectionRevision")
				}
			}
			return plugins, meta
		}
	}
	return append(plugins, payload), append(meta, Object{"pluginId": frozen.S("pluginId"), "desiredRevision": frozen.N("selectionRevision")})
}

// safeAgentModelGap keeps admission failures actionable without exposing another user's reference.
func safeAgentModelGap(reason string) string {
	switch reason {
	case "":
		return ""
	case slotReasonNotConfigured:
		return "agent_configuration_required"
	case slotReasonConnectionDown:
		return "model_connection_unavailable"
	case slotReasonCredentialMissing:
		return "model_credential_required"
	case slotReasonModelUnavailable:
		return "model_not_found"
	default:
		return "model_default_required"
	}
}

// agentTargetDigest freezes one exact artifact; a target-specific artifact takes precedence
// over a universal package, matching the installation plan's target selection.
func agentTargetDigest(release Object, target string) string {
	for _, candidate := range objectsOf(release["targets"]) {
		if candidate.S("target") == target {
			return candidate.S("sha256")
		}
	}
	if release.S("url") != "" {
		return release.S("sha256")
	}
	return ""
}
