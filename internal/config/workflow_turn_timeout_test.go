package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// circus#299 option A's window is deployment configuration, not a run column: an operator's file
// that predates the key must still receive the approved two-hour default, an explicit value must
// win, and an explicit zero must select the default rather than a zero-length window that would
// fail every dispatched session on its first pass.
func TestWorkflowSessionTurnTimeoutDefaultsAndEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, environment string
		want                    time.Duration
	}{
		{"missing key", "", "", 2 * time.Hour},
		{"explicit window", "workflow: {session_turn_timeout: 45m}\n", "", 45 * time.Minute},
		{"explicit zero selects the default", "workflow: {session_turn_timeout: 0s}\n", "", 2 * time.Hour},
		{"environment with absent key", "", "3h", 3 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLOUD_WORKFLOW_SESSION_TURN_TIMEOUT", tc.environment)
			path := filepath.Join(t.TempDir(), "config.yaml")
			yaml := "server: {read_timeout: 10s, write_timeout: 15s}\ndatabase: {conn_max_lifetime: 1h}\ncontrol: {grpc_addr: '127.0.0.1:8082'}\n" + tc.yaml
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Workflow.SessionTurnTimeout != tc.want {
				t.Fatalf("session turn timeout = %s, want %s", cfg.Workflow.SessionTurnTimeout, tc.want)
			}
			// The turn window must stay independent of the end-confirmation window: one bounds a
			// session that has not finished its first turn, the other the wait after EndSession.
			if cfg.IssueRuns.AgentEndConfirmTimeout != 5*time.Minute {
				t.Fatalf("end confirmation window = %s, want its own 5m default", cfg.IssueRuns.AgentEndConfirmTimeout)
			}
		})
	}
}
