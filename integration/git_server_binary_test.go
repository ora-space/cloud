package integration

import (
	"os"
	"testing"
)

func TestGitServerBinaryIsReadyBeforeFixtures(t *testing.T) {
	// This test deliberately creates no fixture: startup must have completed the shared build.
	info, err := os.Stat(gitServerBinary)
	if err != nil {
		t.Fatalf("Git Server binary must exist before fixture setup: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		t.Fatalf("Git Server binary is not a non-empty regular file: %v", info)
	}
}
