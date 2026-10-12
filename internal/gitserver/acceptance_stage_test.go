//go:build acceptance

package gitserver

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
)

// TestStageAcceptanceStore performs one mirror-store action for scripts/git-read-acceptance.sh, the
// container-level acceptance of the whole read path.
//
// The Git Server decides from the mirror store's own state, so an acceptance that wants to watch it
// serve a published mirror — and refuse a revoked one, or one whose preparation failed — has to put
// the store into those states through the store. Hand-writing record files would mean the harness
// constructing the very state it then asserts on, which proves nothing about the code under test.
//
// It is behind a build tag because it is a harness step rather than a test of this package: without
// the tag it does not exist, so it can never be reported as a passing test. It reuses the package's
// own publishMirror fixture, which is the writer's verification reproduced.
//
//	ORA_ACCEPTANCE_STORE_ACTION=publish ORA_ACCEPTANCE_MIRROR_ROOT=<dir> \
//	ORA_ACCEPTANCE_SOURCE_DIR=<repo> ORA_ACCEPTANCE_PROJECT_ID=<uuid> \
//	ORA_ACCEPTANCE_REPOSITORY=<https url> ORA_ACCEPTANCE_COMMIT=<sha> \
//	go test -tags acceptance ./internal/gitserver/ -run TestStageAcceptanceStore -v
func TestStageAcceptanceStore(t *testing.T) {
	action := required(t, "ORA_ACCEPTANCE_STORE_ACTION")
	key := mirror.Key{
		ProjectID:  required(t, "ORA_ACCEPTANCE_PROJECT_ID"),
		Repository: required(t, "ORA_ACCEPTANCE_REPOSITORY"),
		Commit:     required(t, "ORA_ACCEPTANCE_COMMIT"),
	}
	if action == "digest" {
		// The script addresses a mirror by the digest Cloud would hand the sandbox, and the digest is
		// derived from the key rather than stored, so this is the only place that can produce it.
		fmt.Println("MIRROR_DIGEST=" + key.Digest())
		return
	}
	root := required(t, "ORA_ACCEPTANCE_MIRROR_ROOT")
	store, err := mirror.New(mirror.Config{
		Root:           root,
		MaxMirrorBytes: 64 << 20,
		MaxVolumeBytes: 256 << 20,
		Grace:          time.Hour,
		Clock:          time.Now,
	})
	if err != nil {
		t.Fatalf("mirror.New over %s: %v", root, err)
	}
	ctx := context.Background()
	switch action {
	case "publish":
		record := publishMirror(t, store, key, required(t, "ORA_ACCEPTANCE_SOURCE_DIR"))
		report(record)
	case "fail-preparation":
		// A writer that started and then failed. The record stays failed, which is a state the server
		// must refuse rather than serve whatever partial content is on disk.
		prep, err := store.Begin(ctx, key)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		record, err := prep.Fail(mirror.ReasonWriterFailed)
		if err != nil {
			t.Fatalf("Fail: %v", err)
		}
		_ = prep.Close()
		report(record)
	case "revoke":
		record, err := store.Revoke(key, mirror.ReasonWithdrawn)
		if err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		report(record)
	case "destroy":
		record, err := store.Destroy(key)
		if err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		report(record)
	default:
		t.Fatalf("unknown ORA_ACCEPTANCE_STORE_ACTION %q; choose publish, fail-preparation, revoke or destroy", action)
	}
}

// report prints what the script needs to address the mirror and to assert what changed between two
// actions. It goes to stdout as key=value lines because that is what a shell reads.
func report(record mirror.Record) {
	fmt.Println("MIRROR_DIGEST=" + record.Key.Digest())
	fmt.Println("MIRROR_GENERATION=" + fmt.Sprint(record.Generation))
	fmt.Println("MIRROR_STATE=" + record.State)
}

func required(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must be set for the mirror-store harness step", name)
	}
	return value
}
