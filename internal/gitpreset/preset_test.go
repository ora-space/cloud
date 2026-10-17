package gitpreset

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitserver"
)

// presetDB is a migrated, isolated PostgreSQL schema.
func presetDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_POSTGRES") == "1" {
			t.Fatal("TEST_DATABASE_URL is required")
		}
		t.Skip("real PostgreSQL: set TEST_DATABASE_URL")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	schema := "git_preset_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	pool := stdlib.OpenDB(*config)
	t.Cleanup(func() {
		_ = pool.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	if err := (&core.Store{Pool: pool}).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}

// seedProject inserts one active Project with its Binding and main Workspace.
func seedProject(t *testing.T, db *sql.DB) (tenant, project, binding string) {
	t.Helper()
	user, space := uuid.NewString(), uuid.NewString()
	tenant, project, binding = uuid.NewString(), uuid.NewString(), uuid.NewString()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO users(id,display_name,status) VALUES($1,'u','active')", []any{user}},
		{"INSERT INTO tenants(id,name,status) VALUES($1,'t','active')", []any{tenant}},
		{"INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", []any{tenant, user}},
		{"INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'s','s',$3)", []any{space, tenant, user}},
		{"INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle) VALUES($1,$2,$3,$4,'p','active')", []any{project, tenant, user, space}},
		{"INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch) VALUES($1,$2,$3,'https://example.invalid/r.git','main')", []any{binding, tenant, project}},
		{"INSERT INTO workspaces(id,tenant_id,owner_user_id,project_id,kind,desired_state,observed_state,requested_ref) VALUES($1,$2,$3,$4,'main','running','ready','main')", []any{uuid.NewString(), tenant, user, project}},
	} {
		if _, err := tx.Exec(q.query, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return tenant, project, binding
}

func newPreset(t *testing.T) (*Preset, string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required")
	}
	db := presetDB(t)
	tenant, project, binding := seedProject(t, db)
	return &Preset{DB: db, Root: filepath.Join(t.TempDir(), "repositories"), Git: "git"}, tenant, project, binding
}

// expect wraps a test so a (Result, error) call can be checked inline.
type expect struct{ *testing.T }

// ok fails the test unless the call succeeded with ok=true, and returns its result.
func (t expect) ok(r Result, err error) Result {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r["ok"].(bool); !ok {
		t.Fatalf("result not ok: %v", r)
	}
	return r
}

// A synthetic snapshot satisfies the contract: the snapshot and tracking refs exist, a succeeded
// sync is recorded and the current snapshot names it; replaying it is idempotent.
func TestSnapshotProducesAContractConformingProject(t *testing.T) {
	p, tenant, project, binding := newPreset(t)
	ctx := context.Background()
	if r := (expect{t}).ok(p.Check(ctx, project)); r["currentSnapshot"] != nil || r["refs"] != 0 {
		t.Fatalf("an untouched Project = %v, want no snapshot and no refs", r)
	}
	first := (expect{t}).ok(p.Snapshot(ctx, project, "", 3))
	commit := first.S("commit")
	if first["snapshotRef"] != core.GitSnapshotRef(binding, commit) || first["trackingRef"] != core.GitUpstreamHeadPrefix(binding)+"main" {
		t.Fatalf("snapshot = %v", first)
	}
	if count, _ := p.git(ctx, gitserver.RepositoryPath(p.Root, tenant, project), "rev-list", "--count", commit); count != "3" {
		t.Fatalf("synthetic history has %s commits, want 3", count)
	}
	check := (expect{t}).ok(p.Check(ctx, project))
	if current := check["currentSnapshot"].(Result); current["commit"] != commit || current["operationId"] != first["operationId"] {
		t.Fatalf("current snapshot = %v, want the preset's", current)
	}
	again := (expect{t}).ok(p.Snapshot(ctx, project, "", 3))
	if again.S("commit") != commit || again["operationId"] == first["operationId"] {
		t.Fatalf("replay = %v, want the same commit from a new sync", again)
	}
	// An existing commit can be snapshotted directly, and becomes current even if it is older.
	older := (expect{t}).ok(p.Snapshot(ctx, project, "", 1)).S("commit")
	(expect{t}).ok(p.Snapshot(ctx, project, older, 0))
	if current := (expect{t}).ok(p.Check(ctx, project))["currentSnapshot"].(Result); current["commit"] != older {
		t.Fatalf("current snapshot = %v, want the older commit", current)
	}
}

// ref writes any contract ref, and operation writes any contract state: a sync moves the Binding's
// unfinished sync when there is one, so no state collides with the one-unfinished-sync index.
func TestRefAndOperationPresets(t *testing.T) {
	p, _, project, _ := newPreset(t)
	ctx := context.Background()
	commit := (expect{t}).ok(p.Snapshot(ctx, project, "", 2)).S("commit")
	branch := core.GitWorkspaceRefPrefix(uuid.NewString()) + "feature"
	(expect{t}).ok(p.Ref(ctx, project, branch, commit))
	if r := (expect{t}).ok(p.Check(ctx, project)); r["refs"] != 3 {
		t.Fatalf("refs = %v, want snapshot, tracking ref and Workspace branch", r["refs"])
	}
	for _, c := range []struct{ ref, commit string }{
		{"refs/heads/main", commit},
		{core.GitSnapshotRef(uuid.NewString(), commit), commit},
		{branch, strings.ToUpper(commit)},
	} {
		var usage *UsageError
		if _, err := p.Ref(ctx, project, c.ref, c.commit); !errors.As(err, &usage) {
			t.Errorf("ref %s %s: err = %v, want a usage error", c.ref, c.commit, err)
		}
	}
	if _, err := p.Ref(ctx, project, branch, strings.Repeat("e", 40)); err == nil {
		t.Error("a ref to a commit the repository lacks was written")
	}

	// Like every new Project, this one starts with an unfinished sync; each sync state moves that
	// row instead of colliding with it on the one-unfinished-sync index.
	initial := uuid.NewString()
	if _, err := p.DB.Exec("INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger) SELECT $1,'repository_sync',tenant_id,project_id,id,'pending','project_created' FROM repository_bindings WHERE project_id=$2", initial, project); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"leased", "running", "pending"} {
		if r := (expect{t}).ok(p.Operation(ctx, project, "sync", state, "")); r["operationId"] != initial {
			t.Fatalf("sync %s wrote %v, want the unfinished sync %s moved", state, r["operationId"], initial)
		}
	}
	failed := (expect{t}).ok(p.Operation(ctx, project, "sync", "failed", "upstream_unreachable"))
	var code string
	var finished bool
	if err := p.DB.QueryRow("SELECT error_code,finished_at IS NOT NULL FROM repository_operations WHERE id=$1", failed["operationId"]).Scan(&code, &finished); err != nil || failed["operationId"] != initial || code != "upstream_unreachable" || !finished {
		t.Fatalf("failed sync %v = %q finished=%v %v", failed["operationId"], code, finished, err)
	}
	// With no unfinished sync left, a sync state is a new row.
	if r := (expect{t}).ok(p.Operation(ctx, project, "sync", "running", "")); r["operationId"] == initial {
		t.Fatal("a finished sync was moved back to running")
	}
	(expect{t}).ok(p.Operation(ctx, project, "publish", "uncertain", ""))
	for _, c := range []struct{ kind, state string }{{"sync", "uncertain"}, {"publish", "superseded"}, {"clone", "pending"}} {
		var usage *UsageError
		if _, err := p.Operation(ctx, project, c.kind, c.state, ""); !errors.As(err, &usage) {
			t.Errorf("%s %s: err = %v, want a usage error", c.kind, c.state, err)
		}
	}
}

// A Project's unfinished sync, such as the one every new Project starts with, is the sync a preset
// ends: snapshot completes it and a terminal sync state ends it, instead of adding a second row
// beside a sync that would then never finish.
func TestPresetsAdvanceTheUnfinishedSync(t *testing.T) {
	p, tenant, project, binding := newPreset(t)
	ctx := context.Background()
	pending := func() string {
		t.Helper()
		var id string
		if err := p.DB.QueryRow(`INSERT INTO repository_operations(id,type,tenant_id,project_id,binding_id,state,trigger) VALUES(gen_random_uuid(),'repository_sync',$1,$2,$3,'pending','project_created') RETURNING id`, tenant, project, binding).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	state := func(id string) (string, string) {
		t.Helper()
		var s, code string
		if err := p.DB.QueryRow(`SELECT state,COALESCE(error_code,'') FROM repository_operations WHERE id=$1`, id).Scan(&s, &code); err != nil {
			t.Fatal(err)
		}
		return s, code
	}
	first := pending()
	snapshot := (expect{t}).ok(p.Snapshot(ctx, project, "", 2))
	if snapshot["operationId"] != first {
		t.Fatalf("snapshot recorded sync %v, want the pending one %s", snapshot["operationId"], first)
	}
	if s, _ := state(first); s != "succeeded" {
		t.Fatalf("the pending sync is %s, want succeeded", s)
	}
	second := pending()
	failed := (expect{t}).ok(p.Operation(ctx, project, "sync", "failed", "credential_rejected"))
	if failed["operationId"] != second {
		t.Fatalf("failing a sync wrote %v, want the pending one %s", failed["operationId"], second)
	}
	if s, code := state(second); s != "failed" || code != "credential_rejected" {
		t.Fatalf("the pending sync is %s/%s, want failed/credential_rejected", s, code)
	}
	var syncs int
	if err := p.DB.QueryRow(`SELECT count(*) FROM repository_operations WHERE project_id=$1`, project).Scan(&syncs); err != nil || syncs != 2 {
		t.Fatalf("presets added rows beside the syncs they ended: %d %v", syncs, err)
	}
}

// check reports each kind of contract violation without failing to run.
func TestCheckReportsViolations(t *testing.T) {
	p, tenant, project, binding := newPreset(t)
	ctx := context.Background()
	commit := (expect{t}).ok(p.Snapshot(ctx, project, "", 2)).S("commit")
	parent, err := p.git(ctx, gitserver.RepositoryPath(p.Root, tenant, project), "rev-parse", commit+"^")
	if err != nil {
		t.Fatal(err)
	}
	repo := gitserver.RepositoryPath(p.Root, tenant, project)
	for _, args := range [][]string{
		{"update-ref", "refs/heads/main", commit},
		{"update-ref", core.GitSnapshotRef(binding, parent), commit},
		{"update-ref", core.GitUpstreamHeadPrefix(binding) + "dev", parent},
	} {
		if _, err := p.git(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.git(ctx, repo, "update-ref", "-d", core.GitSnapshotRef(binding, commit)); err != nil {
		t.Fatal(err)
	}
	r, err := p.Check(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	violations := strings.Join(r["violations"].([]string), "\n")
	for _, want := range []string{"refs/heads/main is outside", "snapshots/" + parent + " names " + commit, "heads/dev points at " + parent, "heads/main points at " + commit, "current snapshot " + core.GitSnapshotRef(binding, commit)} {
		if !strings.Contains(violations, want) {
			t.Errorf("violations lack %q:\n%s", want, violations)
		}
	}
	if r["ok"] != false {
		t.Fatal("check passed a violating Project")
	}
}

// S reads a string field of a result.
func (r Result) S(key string) string {
	s, _ := r[key].(string)
	return s
}
