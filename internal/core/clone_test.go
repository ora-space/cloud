package core

import "testing"

// Both upload grants are issued against a registered execution, so they must reach the execution
// registry's handler rather than the operation command path. A new grant action that stops being
// routed is a silent capability outage: the RPC answers with a different handler's error.
func TestIsCloneActionRoutesBothUploadGrants(t *testing.T) {
	for _, action := range []string{
		"grant_revision_upload",
		"grant_scheduled_artifact_upload",
		"clone_takeover",
		"thread_events",
	} {
		if !isCloneAction(action) {
			t.Fatalf("isCloneAction(%q) = false, want the execution registry to answer it", action)
		}
	}
	for _, action := range []string{"grant", "grant_", "operation_command", ""} {
		if isCloneAction(action) {
			t.Fatalf("isCloneAction(%q) = true, want it to stay on the operation command path", action)
		}
	}
}
