package core

// taskPersonalModelGap reuses the authoritative default-executor policy without returning private
// connection or credential material. It is advisory at read time and rechecked on dispatch.
func taskPersonalModelGap(t *transaction, uid string) string {
	reason, _, _ := defaultExecutorGap(t, uid)
	switch reason {
	case "":
		return ""
	case slotReasonConnectionDown:
		return "model_connection_unavailable"
	case slotReasonCredentialMissing:
		return "model_credential_required"
	default:
		return "model_default_required"
	}
}

func taskAgentReadiness(t *transaction, task Object, uid string) []Object {
	agents := t.list("SELECT id,plugin_id,space_id FROM space_agents WHERE tenant_id=$1 AND status='active' ORDER BY id LIMIT 500", task.S("tenantId"))
	project := t.one("SELECT space_id FROM projects WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", nullable(task.S("projectRef")), task.S("tenantId"))
	readiness := make([]Object, 0, len(agents))
	for _, agent := range agents {
		personal, gap := taskAgentModelGap(t, uid, agent, "")
		if project == nil || project.S("spaceId") != agent.S("spaceId") {
			gap = "issue_project_required"
		}
		readiness = append(readiness, Object{"agentId": agent.S("id"), "requiresPersonalModel": personal, "errorCode": nullable(gap)})
	}
	return readiness
}

// taskAgentModelGap translates only expected admission faults into a safe read projection.
// Database failures still abort the transaction; the resolver performs no writes. selectedModel is a
// scheduled definition's optional override: it is resolved and protocol-checked by the same
// authoritative resolver, so an incompatible override reports its fault instead of being admitted.
func taskAgentModelGap(t *transaction, uid string, agent Object, selectedModel string) (personal bool, gap string) {
	pluginID := agent.S("pluginId")
	personal = pluginID == opencodeAgentCLI || pluginID == claudeAgentCLI
	defer func() {
		if recovered := recover(); recovered != nil {
			if fault, ok := recovered.(*Fault); ok {
				personal, gap = true, fault.Code
			} else {
				panic(recovered)
			}
		}
	}()
	namespace, identifier, ok := pluginIdentity(pluginID)
	if !ok {
		return personal, "invalid_plugin_id"
	}
	selection := t.spacePluginRow(agent.S("spaceId"), namespace, identifier)
	frozen, _, _ := resolveDeclaredAgentRun(t, uid, agent.S("spaceId"), pluginID, selection.S("desiredVersion"), selectedModel)
	if frozen != nil {
		return true, ""
	}
	if pluginID == opencodeAgentCLI {
		return true, taskPersonalModelGap(t, uid)
	}
	return personal, ""
}
