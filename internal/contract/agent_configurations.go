package contract

import "strings"

// personalAgentSchemas exposes only owner-visible configuration and static declaration metadata.
func personalAgentSchemas(s obj) {
	scalar := obj{"oneOf": []any{str(), obj{"type": "number"}, boolean()}}
	settings := obj{"type": "object", "additionalProperties": scalar}
	s["PersonalAgentConfiguration"] = object(obj{
		"revision": number(), "cleared": boolean(), "declarationFingerprint": str(),
		"settings": settings, "connectionId": optional(uuid()), "modelId": optional(str()), "updatedAt": timestamp(),
	}, "revision", "cleared", "settings")
	setting := object(obj{
		"id": str(), "title": str(), "description": str(), "settingType": enumeration("string", "number", "boolean"),
		"required": boolean(), "order": optional(number()), "default": obj{"nullable": true, "description": "A scalar of the declared type, or null when no default exists."},
	}, "id", "title", "description", "settingType", "required", "order", "default")
	declaration := object(obj{
		"schemaVersion": number(), "fingerprint": str(), "settings": array(setting),
		"modelAccess": object(obj{"contractVersion": number(), "protocols": array(str()), "modelCardinality": enumeration("single")},
			"contractVersion", "protocols", "modelCardinality"),
	}, "schemaVersion", "fingerprint", "settings", "modelAccess")
	context := obj{}
	for key, value := range properties(s, "PersonalAgentConfiguration") {
		context[key] = value
	}
	for key, value := range (obj{
		"pluginId": str(), "spaceId": uuid(), "selectionVersion": number(), "pluginVersion": str(),
		"declaration": declaration, "requiresRepair": boolean(), "modelDefault": ref("ModelDefault"),
	}) {
		context[key] = value
	}
	s["PersonalAgentConfigurationContext"] = object(context, "revision", "cleared", "settings",
		"pluginId", "spaceId", "selectionVersion", "pluginVersion", "declaration", "requiresRepair")
}

// personalAgentPath recognizes the owner-scoped route family, including explicit repair.
func personalAgentPath(path string) bool {
	return strings.HasPrefix(path, "/api/v1/me/agent-configurations/")
}
