package core

import "testing"

func TestPreparationTerminalStatusNeverReturnsWaiting(t *testing.T) {
	for _, tc := range []struct{ status, phase, stage string }{
		{"cancelled", "done", "cancelled"},
		{"cancelled", "starting", "cancelled"},
		{"failed", "", "failed"},
	} {
		t.Run(tc.status+tc.phase, func(t *testing.T) {
			progress := agentRunPreparation(nil, Object{"status": tc.status, "phase": tc.phase})
			if progress == nil || progress.S("stage") != tc.stage {
				t.Fatalf("terminal %s/%s projected as %v", tc.status, tc.phase, progress)
			}
		})
	}
}
