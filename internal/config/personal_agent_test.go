package config

import "testing"

// Rollout is opt-in; a misspelled target must never silently enable another runtime.
func TestPersonalAgentTargetDeploymentGate(t *testing.T) {
	for _, target := range []string{"", "x86_64-unknown-linux-gnu", "aarch64-unknown-linux-gnu"} {
		if err := validatePlugins(PluginConfig{PersonalAgentTarget: target}); err != nil {
			t.Fatal(err)
		}
	}
	if err := validatePlugins(PluginConfig{PersonalAgentTarget: "linux"}); err == nil {
		t.Fatal("unknown execution target accepted")
	}
}
