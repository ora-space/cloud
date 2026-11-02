package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestTenantSpaces(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	// Logging in again must not add a second space or a second plugin row.
	f.login(t, "owner", "owner")

	items, e := f.store.TenantSpaces(ctx, f.tenant, owner)
	if e != nil {
		t.Fatal(e)
	}
	if len(items) != 1 {
		t.Fatalf("spaces = %+v", items)
	}
	space := items[0]
	if space.TenantID != f.tenant || space.Name != store.DemoSpaceName || space.Slug != store.DemoSpaceSlug {
		t.Fatalf("space = %+v", space)
	}
	if space.Role != "owner" || space.Version != 1 || space.CreatedBy != owner {
		t.Fatalf("space identity = %+v", space)
	}
	if space.ArchivedAt != nil || space.CreatedAt.IsZero() || space.UpdatedAt.IsZero() {
		t.Fatalf("space times = %+v", space)
	}

	// A caller outside the workspace learns nothing about its spaces.
	if _, e := f.store.TenantSpaces(ctx, "00000000-0000-0000-0000-000000000001", owner); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("non-member spaces = %v", e)
	}
}

func TestSpaceAgentPlugins(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(ctx, f.tenant, owner)
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID

	items, e := f.store.SpaceAgentPlugins(ctx, f.tenant, space, owner)
	if e != nil {
		t.Fatal(e)
	}
	// Listed by name: Claude Code, then OpenCode.
	if len(items) != 2 || items[0].Identifier != store.DemoAgents[1].Identifier || items[1].Identifier != store.DemoAgents[0].Identifier {
		t.Fatalf("plugins = %+v", items)
	}
	for i, p := range items {
		want := store.DemoAgents[1-i]
		if p.SpaceID != space || p.TenantID != f.tenant || p.Kind != "agent" {
			t.Fatalf("plugin scope = %+v", p)
		}
		if p.SourceNamespace != want.Namespace || p.Identifier != want.Identifier {
			t.Fatalf("plugin identity = %+v", p)
		}
		if p.DesiredState != "installed" || p.ObservedState != "installed" ||
			p.DesiredVersion != want.Version || p.ObservedVersion == nil ||
			*p.ObservedVersion != want.Version {
			t.Fatalf("plugin state = %+v", p)
		}
		if p.InstallError != nil || p.RequestedByUserID != nil {
			t.Fatalf("plugin extras = %+v", p)
		}
	}

	// A plugin that is not observed as installed is not offered as a member candidate.
	if _, e := f.db.Exec(ctx, `INSERT INTO v2_space_agents (tenant_id, space_id, name, identifier, observed_state)
		VALUES ($1, $2, '失败的 Agent', 'broken', 'failed')`, f.tenant, space); e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(ctx, `INSERT INTO v2_space_agents
		(tenant_id, space_id, name, source_namespace, identifier, kind, observed_state, requested_by_user_id)
		VALUES ($1, $2, 'Codex', 'official', 'ora-space.codex', 'agent', 'installed', $3)`,
		f.tenant, space, owner); e != nil {
		t.Fatal(e)
	}
	if items, e = f.store.SpaceAgentPlugins(ctx, f.tenant, space, owner); e != nil || len(items) != 2 {
		t.Fatalf("plugins after failed install = %+v %v", items, e)
	}

	// A space outside the caller's workspace answers not found, not an empty list.
	if _, e := f.store.SpaceAgentPlugins(ctx, f.tenant, "00000000-0000-0000-0000-000000000001", owner); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("foreign space plugins = %v", e)
	}
}
