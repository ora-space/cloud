package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResidualReconcileIntervalDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, environment string
		want                    time.Duration
	}{
		{"omitted", "", "", time.Minute},
		{"file", "issue_runs: {residual_reconcile_interval: 45s}\n", "", 45 * time.Second},
		{"environment with omitted key", "", "2m", 2 * time.Minute},
		{"environment overrides file", "issue_runs: {residual_reconcile_interval: 45s}\n", "3m", 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUD_ISSUE_RUNS_RESIDUAL_RECONCILE_INTERVAL", tc.environment)
			cfg, err := Load(writeResidualReconcileConfig(t, tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.IssueRuns.ResidualReconcileInterval != tc.want {
				t.Fatalf("residual reconcile interval = %s, want %s", cfg.IssueRuns.ResidualReconcileInterval, tc.want)
			}
		})
	}
}

func TestResidualReconcileIntervalRejectsInvalidConfiguration(t *testing.T) {
	for _, source := range []string{"file", "environment"} {
		for _, value := range []string{"0s", "-1m", "infinite", "999999999999999999h"} {
			t.Run(source+"/"+value, func(t *testing.T) {
				yaml, environment := "issue_runs: {residual_reconcile_interval: "+value+"}\n", ""
				if source == "environment" {
					yaml, environment = "", value
				}
				t.Setenv("CLOUD_ISSUE_RUNS_RESIDUAL_RECONCILE_INTERVAL", environment)
				_, err := Load(writeResidualReconcileConfig(t, yaml))
				if err == nil || !strings.Contains(err.Error(), "residual_reconcile_interval") {
					t.Fatalf("invalid residual reconcile interval %q from %s must reject configuration, got %v", value, source, err)
				}
			})
		}
	}
}

func writeResidualReconcileConfig(t *testing.T, issueRuns string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "server: {read_timeout: 10s, write_timeout: 15s}\ndatabase: {conn_max_lifetime: 1h}\ncontrol: {grpc_addr: '127.0.0.1:8082'}\n" + issueRuns
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
