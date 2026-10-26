package core

import "testing"

// Only bounded classifications become public; each adapter failure keeps its own code (ADR D2).
func TestPublicSessionFailureCode(t *testing.T) {
	for detail, want := range map[string]string{
		"agent_turn_failed":                                    "agent_turn_failed",
		"agent_adapter_unavailable":                            "agent_adapter_unavailable",
		"agent_model_configuration_required":                   "agent_model_configuration_required",
		"agent_model_configuration_invalid":                    "agent_model_configuration_invalid",
		"agent_model_protocol_mismatch":                        "agent_model_protocol_mismatch",
		"model_grant_mismatch":                                 "model_grant_mismatch",
		"agent_model_adapter_failed":                           "agent_model_adapter_failed",
		"agent_model_adapter_not_applied":                      "agent_model_adapter_not_applied",
		"agent_plugin_unavailable":                             "agent_plugin_unavailable",
		"":                                                     "agent_failed",
		"401 from https://provider.example: invalid x-api-key": "agent_failed",
		"agent_model_something_new":                            "agent_failed",
	} {
		if got := publicSessionFailureCode(detail); got != want {
			t.Errorf("publicSessionFailureCode(%q) = %q, want %q", detail, got, want)
		}
	}
}
