package core

import (
	"slices"
	"strings"

	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

// personalAgentConfiguration retains cleared rows so an old editor can never regain its revision.
func personalAgentConfiguration(t *transaction, uid, pluginID string) Object {
	if row := t.one(`SELECT declaration_fingerprint,settings::text AS settings_json,connection_id,model_id,revision,cleared,updated_at
		FROM personal_agent_configurations WHERE user_id=$1 AND plugin_id=$2`, uid, pluginID); row != nil {
		values, err := decodeAgentJSONValue([]byte(row.S("settingsJson")))
		if err != nil {
			panic(databaseFailure{err})
		}
		row["settings"] = values
		delete(row, "settingsJson")
		return row
	}
	return Object{"revision": int64(0), "cleared": true, "settings": Object{}}
}

// agentConfigurationContext reads only the space's pinned release, never catalog latest.
func agentConfigurationContext(t *transaction, uid, spaceID, pluginID string) (Object, *pluginmarket.CompiledAgentConfiguration) {
	require(validID(spaceID), 404, "not_found")
	spaceMember(t, spaceID, uid)
	namespace, identifier, ok := pluginIdentity(pluginID)
	require(ok, 400, "invalid_plugin_id")
	selection := t.spacePluginRow(spaceID, namespace, identifier)
	require(selection != nil && selection.S("desiredState") == "installed", 409, "agent_plugin_unavailable")
	release := selection.O("selectedRelease")
	require(release.S("kind") == "agent" && release.S("version") == selection.S("desiredVersion"), 409, "agent_plugin_unavailable")
	require(release.S("agentConfigurationIssue") == "", 409, "agent_declaration_invalid")
	association := release.O("agentConfiguration")
	require(association.S("declaration") != "", 409, "agent_declaration_required")
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(association.S("declaration")))
	require(err == nil, 409, "agent_declaration_invalid")
	require(compiled.Fingerprint == association.S("declarationSha256") &&
		int64(compiled.SchemaVersion) == association.N("schemaVersion"), 409, "agent_declaration_invalid")
	return selection, compiled
}

// agentConfigurationView exposes the caller's values alongside the current editing context.
// A changed declaration leaves old values visible for explicit repair, but never applies them.
func agentConfigurationView(t *transaction, uid string, r *PublicRequest) Object {
	selection, declaration := agentConfigurationContext(t, uid, r.SpaceID, r.AgentPluginID)
	row := personalAgentConfiguration(t, uid, r.AgentPluginID)
	row["pluginId"] = r.AgentPluginID
	row["spaceId"] = r.SpaceID
	row["selectionVersion"] = selection.N("version")
	row["pluginVersion"] = selection.S("desiredVersion")
	row["declaration"] = declaration
	row["requiresRepair"] = !row.B("cleared") && row.S("declarationFingerprint") != declaration.Fingerprint
	if r.AgentPluginID == opencodeAgentCLI {
		row["modelDefault"] = modelDefault(t, uid)
	}
	return row
}

// personalAgentConfigurationsPublic owns the user-scoped CAS and idempotency boundary.
// The space in the URL is editing context; administrator privileges never change the owner.
func personalAgentConfigurationsPublic(t *transaction, r *PublicRequest, uid string) (out Object, status int) {
	_, _, valid := pluginIdentity(r.AgentPluginID)
	require(valid, 400, "invalid_plugin_id")
	require(validID(r.SpaceID), 404, "not_found")
	spaceMember(t, r.SpaceID, uid)
	if r.Method == "GET" {
		return agentConfigurationView(t, uid, r), 200
	}
	require(r.Method == "PUT" || r.Method == "DELETE" || r.Method == "POST" && strings.HasSuffix(r.Path, "/repair"), 405, "method_not_allowed")
	require(r.Key != "" && len(r.Key) <= 200, 400, "idempotency_key_required")
	hash := requestHash(r.Method, r.Path, r.Body)
	if previous := personalModelReplay(t, uid, r.Key, hash); previous != nil {
		return previous.O("response"), int(previous.N("status"))
	}
	_, present := r.Body["revision"]
	require(present, 428, "revision_required")
	row := personalAgentConfiguration(t, uid, r.AgentPluginID)
	require(row.N("revision") == r.Body.N("revision"), 409, "revision_conflict")
	if r.Method == "DELETE" {
		require(row.N("revision") > 0, 404, "not_found")
		t.exec(`UPDATE personal_agent_configurations SET cleared=true,settings='{}',connection_id=NULL,
			model_id=NULL,revision=revision+1,updated_at=now() WHERE user_id=$1 AND plugin_id=$2`, uid, r.AgentPluginID)
	} else {
		savePersonalAgentConfiguration(t, r, uid, row)
	}
	out = Object{"resource": personalAgentConfiguration(t, uid, r.AgentPluginID)}
	recordPersonalModelWrite(t, uid, r.Key, hash, out, 200)
	return out, 200
}

// savePersonalAgentConfiguration applies a complete submission against the selected release.
// Repair deliberately has its own operation: ordinary writes cannot silently discard old fields.
func savePersonalAgentConfiguration(t *transaction, r *PublicRequest, uid string, old Object) {
	selection, declaration := agentConfigurationContext(t, uid, r.SpaceID, r.AgentPluginID)
	require(r.Body.N("selectionVersion") == selection.N("version"), 409, "agent_release_conflict")
	require(r.Body.S("declarationFingerprint") == declaration.Fingerprint, 409, "agent_declaration_conflict")
	if !old.B("cleared") && old.S("declarationFingerprint") != declaration.Fingerprint {
		require(r.Method == "POST" && strings.HasSuffix(r.Path, "/repair"), 409, "agent_configuration_repair_required")
	}
	raw, present := r.Body["settings"]
	require(present, 400, "invalid_agent_settings")
	values, ok := raw.(map[string]any)
	if !ok {
		typed, typedOK := raw.(Object)
		require(typedOK, 400, "invalid_agent_settings")
		values = typed
	}
	settings := resolveAgentSettings(declaration, values)
	var connectionID, modelID any
	if r.AgentPluginID == opencodeAgentCLI {
		_, connectionPresent := r.Body["connectionId"]
		_, modelPresent := r.Body["modelId"]
		require(!connectionPresent && !modelPresent, 400, "agent_use_model_default")
	} else {
		connection := modelConnection(t, uid, r.Body.S("connectionId"))
		require(connection.B("enabled"), 409, "model_connection_disabled")
		require(findModel(connection, r.Body.S("modelId")) != nil, 400, "model_not_found")
		require(slices.Contains(declaration.ModelAccess.Protocols, connection.S("protocol")), 409, "agent_model_protocol_mismatch")
		require(r.AgentPluginID != claudeAgentCLI || connection.S("protocol") == "anthropic-messages", 409, "agent_model_protocol_mismatch")
		connectionID, modelID = connection.S("id"), r.Body.S("modelId")
	}
	t.exec(`INSERT INTO personal_agent_configurations(user_id,plugin_id,declaration_fingerprint,settings,connection_id,model_id,revision)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(user_id,plugin_id) DO UPDATE SET declaration_fingerprint=excluded.declaration_fingerprint,
		settings=excluded.settings,connection_id=excluded.connection_id,model_id=excluded.model_id,
		revision=excluded.revision,cleared=false,updated_at=now()`,
		uid, r.AgentPluginID, declaration.Fingerprint, jsonText(settings), connectionID, modelID, old.N("revision")+1)
}
