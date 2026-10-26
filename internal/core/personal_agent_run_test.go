package core

import "testing"

// Keep internal executor reasons out of the public readiness/admission vocabulary, including
// an unconfigured pair, which belongs to the plugin's personal configuration rather than default.
func TestSafeAgentModelGap(t *testing.T) {
	for _, test := range []struct{ reason, want string }{
		{"", ""},
		{slotReasonDefaultMissing, "model_default_required"},
		{slotReasonNotConfigured, "agent_configuration_required"},
		{slotReasonConnectionDown, "model_connection_unavailable"},
		{slotReasonCredentialMissing, "model_credential_required"},
		{slotReasonModelUnavailable, "model_not_found"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			if got := safeAgentModelGap(test.reason); got != test.want {
				t.Fatalf("safeAgentModelGap(%q) = %q, want %q", test.reason, got, test.want)
			}
		})
	}
}
