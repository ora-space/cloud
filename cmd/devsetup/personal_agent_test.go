package main

import (
	"strings"
	"testing"
)

// Setup must not discard a rollout target explicitly supplied in the template.
func TestPersonalAgentTargetEnvironment(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "[database]\ndsn='x'\n[plugins]\npersonal_agent_target='x86_64-unknown-linux-gnu'\nsync_enabled=false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Plugins.PersonalAgentTarget != "x86_64-unknown-linux-gnu" {
		t.Fatal("target was discarded")
	}
	if !strings.Contains(renderEnv(&cfg), "CLOUD_PLUGINS_PERSONAL_AGENT_TARGET=\"x86_64-unknown-linux-gnu\"\n") {
		t.Fatal("target override was not rendered")
	}
}
