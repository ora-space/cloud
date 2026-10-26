package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

var (
	gitServerBinary           string
	controlledPreparedFixture *fixture
	controlledFixtureCleanups []func() error
)

// TestMain owns the Git Server binary, sealed database template and reusable controlled resources.
// Ordinary fixtures clean up their independent copies before these package resources are released.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ora-cloud-git-server-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-server build directory:", err)
		os.Exit(1)
	}
	gitServerBinary = filepath.Join(dir, "git-server")
	// Build the shipped hook before m.Run can create fixtures with short-lived heartbeats or grants.
	// A cold Go build cache must never consume their freshness window; all harnesses reuse this file.
	build := exec.Command("go", "build", "-o", gitServerBinary, "../cmd/git-server")
	code := 1
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build git-server: %v: %s\n", err, out)
	} else {
		code = m.Run()
	}
	for i := len(controlledFixtureCleanups) - 1; i >= 0; i-- {
		if err := controlledFixtureCleanups[i](); err != nil {
			fmt.Fprintln(os.Stderr, "controlled fixture cleanup:", err)
			code = 1
		}
	}
	if err := closeFixtureTemplate(); err != nil {
		fmt.Fprintln(os.Stderr, "fixture template cleanup:", err)
		code = 1
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "git-server binary cleanup:", err)
		code = 1
	}
	os.Exit(code)
}

// controlledPreparedResources is called only by serial controlled-reclamation tests. It holds no
// servers, credentials or bootstrap rows, so all mutable fixture state remains local to each case.
func controlledPreparedResources(t *testing.T) *fixture {
	t.Helper()
	if controlledPreparedFixture != nil {
		return controlledPreparedFixture
	}
	pool, config := migratedTestSchemaWithCleanup(t, "test_controlled_", func(cleanup func() error) {
		controlledFixtureCleanups = append(controlledFixtureCleanups, cleanup)
	})
	store := &core.Store{Pool: pool}
	root, err := os.MkdirTemp("", "ora-cloud-controlled-")
	must(t, err)
	controlledFixtureCleanups = append(controlledFixtureCleanups, func() error { return os.RemoveAll(root) })
	source, commit := prepareFixtureSource(t, root)
	controlledPreparedFixture = &fixture{store: store, pgConfig: config, source: source, commit: commit}
	return controlledPreparedFixture
}

// controlledFixtureFactory is deliberately scoped to serial controlled-reclamation matrices.
// Claims, reconciliation scans and the Controller lease are schema-wide, so distinct tenant IDs
// alone do not isolate cases. Clear business rows between cases while retaining migration state
// and the migration-created scan singleton. Each child still gets a fresh Store and service stack.
func controlledFixtureFactory(t *testing.T) func(*testing.T) *fixture {
	t.Helper()
	prepared := controlledPreparedResources(t)
	var tables string
	must(t, prepared.store.Pool.QueryRow(`SELECT string_agg(format('%I.%I',schemaname,tablename), ', ' ORDER BY tablename)
 FROM pg_tables WHERE schemaname=current_schema()
 AND tablename NOT IN ('schema_migrations','task_conversation_scan_state')`).Scan(&tables))
	return func(t *testing.T) *fixture {
		t.Helper()
		// All related business tables are named in one statement, preserving FK enforcement without
		// CASCADE. The immutable migration records and independent scan singleton are not truncated.
		_, err := prepared.store.Pool.Exec("TRUNCATE TABLE " + tables)
		must(t, err)
		_, err = prepared.store.Pool.Exec("UPDATE task_conversation_scan_state SET last_created_at=NULL,last_id=NULL")
		must(t, err)
		return setupWithPreparedResources(t, prepared)
	}
}
