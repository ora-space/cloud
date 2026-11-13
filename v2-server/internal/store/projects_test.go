package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestProjectsSharedOnlyWithinTheirTenant(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	project, err := f.s.CreateProject(ctx, f.scope, store.Project{ID: "project", OwnerUserID: "forged-owner", Name: "Demo", RepositoryURL: "https://example.com/demo.git", Branch: "main", Commit: "fixed", MirrorPath: "/private/demo.git", SnapshotRef: "refs/ora/snapshots/fixed"})
	if err != nil || project.CreatedAt.IsZero() || project.OwnerUserID != f.scope.UserID || project.SnapshotRef != "refs/ora/snapshots/fixed" {
		t.Fatalf("project=%+v err=%v", project, err)
	}
	other := executionMember(t, f.s, "member")
	got, err := f.s.GetProject(ctx, other, project.ID)
	if err != nil || got != project {
		t.Fatalf("tenant share=%+v err=%v", got, err)
	}
	items, err := f.s.ListProjects(ctx, other)
	if err != nil || len(items) != 1 || items[0] != project {
		t.Fatalf("projects=%+v err=%v", items, err)
	}
	if _, err := f.s.CreateProject(ctx, f.scope, project); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("duplicate project=%v", err)
	}
	var tenant string
	if err := f.admin.QueryRow(ctx, `INSERT INTO tenants(name) VALUES('other') RETURNING id`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `INSERT INTO memberships(tenant_id,user_id,role) VALUES($1,$2,'member')`, tenant, other.UserID); err != nil {
		t.Fatal(err)
	}
	other.TenantID = tenant
	if _, err := f.s.GetProject(ctx, other, project.ID); !errors.Is(err, execution.ErrNotFound) {
		t.Fatalf("cross tenant project=%v", err)
	}
	if items, err := f.s.ListProjects(ctx, other); err != nil || len(items) != 0 {
		t.Fatalf("cross tenant projects=%+v err=%v", items, err)
	}
}

func TestRecentProjectsOrdersByLatestConversationThenCreation(t *testing.T) {
	f := executionDatabase(t)
	ctx := t.Context()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "middle", "new"} {
		if _, err := f.s.CreateProject(ctx, f.scope, store.Project{ID: id, Name: id, RepositoryURL: "https://example.com/" + id + ".git", Commit: "fixed", MirrorPath: "/private/" + id + ".git", CreatedAt: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// A conversation makes the oldest project the most recently active one.
	if _, err := f.admin.Exec(ctx, `INSERT INTO v2_chat_conversations(tenant_id,user_id,create_key,project_id,executor_id,title,created_at)
		VALUES($1,$2,'k','old','opencode','t',$3)`, f.scope.TenantID, f.scope.UserID, base.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.RecentProjects(ctx, 2)
	if err != nil || len(got) != 2 || got[0].ID != "old" || got[1].ID != "new" || got[0].MirrorPath != "/private/old.git" {
		t.Fatalf("recent=%+v err=%v", got, err)
	}
}
