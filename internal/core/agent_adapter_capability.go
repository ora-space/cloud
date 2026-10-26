package core

import "slices"

// validAgentAdapterCapability accepts only the deployed v1 surface; absence is honest legacy state.
func validAgentAdapterCapability(capability Object, proxy bool) bool {
	if len(capability) == 0 {
		return true
	}
	if !proxy || capability.N("contractVersion") != 1 || !validAgentTarget(capability.S("target")) {
		return false
	}
	protocols := adapterProtocols(capability["protocols"])
	if len(protocols) == 0 || len(protocols) > 2 {
		return false
	}
	seen := map[string]bool{}
	for _, protocol := range protocols {
		if (protocol != "openai-completions" && protocol != "anthropic-messages") || seen[protocol] {
			return false
		}
		seen[protocol] = true
	}
	return true
}

// validAgentTarget names the Linux targets supported by the current Node execution path.
func validAgentTarget(target string) bool {
	return target == "x86_64-unknown-linux-gnu" || target == "aarch64-unknown-linux-gnu"
}

// declaredAgentCapable rechecks the live incarnation against immutable admission requirements.
func declaredAgentCapable(node, configuration Object) bool {
	if len(configuration) == 0 {
		return true
	}
	capability := node.O("modelAdapter")
	return node.B("modelProxy") && capability.N("contractVersion") == configuration.N("adapterVersion") &&
		capability.S("target") == configuration.S("target") && slices.Contains(adapterProtocols(capability["protocols"]), configuration.S("protocol"))
}

// Adapter reports and PostgreSQL JSON decoding use different concrete slice representations.
func adapterProtocols(value any) []string {
	if protocols, ok := value.([]string); ok {
		return protocols
	}
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	protocols := make([]string, 0, len(values))
	for _, value := range values {
		protocol, ok := value.(string)
		if !ok {
			return nil
		}
		protocols = append(protocols, protocol)
	}
	return protocols
}

// SQL's preselection mirrors declaredAgentCapable so incompatible incarnations are settled promptly.
const declaredAdapterMismatchSQL = `EXISTS (SELECT 1 FROM run_agent_configurations c JOIN run_model_bindings b ON b.run_id=c.run_id
 WHERE c.run_id=ir.id AND (COALESCE(n.model_adapter->>'target','')<>c.target
 OR COALESCE(n.model_adapter->>'contractVersion','0')<>c.adapter_contract_version::text
 OR NOT COALESCE(n.model_adapter->'protocols','[]'::jsonb) ? b.protocol))`
