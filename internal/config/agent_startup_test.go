package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Startup policy must reach the server even when an operator's file predates the new key.
func TestAgentStartupBudgetDefaultsAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, environment string
		want                    time.Duration
	}{
		{"missing key", "", "", 2 * time.Minute},
		{"explicit budget", "issue_runs: {agent_start_timeout: 2m}\n", "", 2 * time.Minute},
		{"environment with absent key", "", "3m", 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUD_ISSUE_RUNS_AGENT_START_TIMEOUT", tc.environment)
			path := filepath.Join(t.TempDir(), "config.yaml")
			yaml := "server: {read_timeout: 10s, write_timeout: 15s}\ndatabase: {conn_max_lifetime: 1h}\ncontrol: {grpc_addr: '127.0.0.1:8082'}\n" + tc.yaml
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.IssueRuns.AgentStartTimeout != tc.want {
				t.Fatalf("startup budget = %s, want %s", cfg.IssueRuns.AgentStartTimeout, tc.want)
			}
			if cfg.IssueRuns.AgentRestoreTimeout != 6*time.Minute || cfg.IssueRuns.AgentEndConfirmTimeout != 5*time.Minute {
				t.Fatal("restore and ending defaults must be independent 6m/5m windows")
			}
		})
	}
}

func TestInvalidAgentWindowsRejectStartup(t *testing.T) {
	for _, key := range []string{"agent_start_timeout", "agent_restore_timeout", "agent_end_confirm_timeout"} {
		for _, value := range []string{"0s", "-1m", "infinite", "999999999999999999h"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				content := "server: {read_timeout: 10s, write_timeout: 15s}\ndatabase: {conn_max_lifetime: 1h}\ncontrol: {grpc_addr: '127.0.0.1:8082'}\nissue_runs: {" + key + ": " + value + "}\n"
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Load(path); err == nil {
					t.Fatal("invalid explicit window silently selected a default")
				}
			})
		}
	}
}
