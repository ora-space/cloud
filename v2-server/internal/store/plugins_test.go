package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestPluginMarketLifecycle(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	member := f.login(t, "member", "member")
	spaces, e := f.store.TenantSpaces(ctx, f.tenant, owner)
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	path := "/api/v1/tenants/" + f.tenant + "/spaces/" + space + "/plugins"
	if e := f.store.ReplacePluginCatalog(ctx, []pluginmarket.Entry{
		{Namespace: "official", Identifier: "mattpocock.skills", Title: "Matt Pocock Skills", Kind: "skill", Version: "1.3.1", Description: "Engineering skills", Readme: "# Matt Pocock Skills\n\nReal skills.", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.tavily-search", Title: "Tavily", Kind: "mcp", Version: "0.1.0", Description: "Web search", Readme: "# Tavily", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.workflow-examples", Title: "Workflow Examples", Kind: "workflow", Version: "0.1.0", Description: "Examples", Readme: "# Workflow Examples", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.opencode", Title: "OpenCode", Kind: "agent", Version: "0.6.8", Description: "Agent", Readme: "# OpenCode", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "test-pack", Title: "Test Pack", Kind: "pack", Version: "1.0.0", Description: "Pack", PackMembers: []string{"official/mattpocock.skills"}, MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
	}, time.Now()); e != nil {
		t.Fatal(e)
	}

	catalog, syncedAt, e := f.store.PluginCatalog(ctx, f.tenant, space, owner)
	if e != nil {
		t.Fatal(e)
	}
	if syncedAt.IsZero() || len(catalog) != 5 {
		t.Fatalf("catalog = %d entries at %v", len(catalog), syncedAt)
	}
	kinds := map[string]bool{}
	for _, entry := range catalog {
		kinds[entry.Kind] = true
		if entry.ID == "official/mattpocock.skills" && entry.Readme == nil {
			t.Fatalf("skill package has no document: %+v", entry)
		}
	}
	for _, kind := range []string{"agent", "skill", "mcp", "workflow", "pack"} {
		if !kinds[kind] {
			t.Fatalf("catalog is missing kind %q: %+v", kind, kinds)
		}
	}

	initial, e := f.store.SpacePlugins(ctx, f.tenant, space, member)
	if e != nil || len(initial) != 2 {
		t.Fatalf("initial selections = %+v %v", initial, e)
	}
	installed, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-skill", store.PluginInstallInput{
		Identifier: "official/mattpocock.skills", PluginVersion: "1.3.1", Version: 0,
	})
	if e != nil {
		t.Fatal(e)
	}
	if installed.Kind != "skill" || installed.DesiredState != "installed" ||
		installed.ObservedState != "installed" || installed.ObservedVersion == nil ||
		*installed.ObservedVersion != "1.3.1" || installed.RequestedByUserID == nil ||
		*installed.RequestedByUserID != owner {
		t.Fatalf("installed = %+v", installed)
	}
	if _, e := f.store.CreateSquad(ctx, f.tenant, owner, store.SquadInput{
		Name: "invalid agent", LeaderAgentID: &installed.ID,
	}); !errors.Is(e, store.ErrAgentUnavailable) {
		t.Fatalf("skill as squad agent = %v", e)
	}

	replayed, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-skill", store.PluginInstallInput{
		Identifier: "official/mattpocock.skills", PluginVersion: "1.3.1", Version: 0,
	})
	if e != nil || replayed.Version != installed.Version {
		t.Fatalf("replayed = %+v %v", replayed, e)
	}
	if _, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-skill", store.PluginInstallInput{
		Identifier: "official/ora-space.tavily-search", Version: 0,
	}); !errors.Is(e, store.ErrIdempotencyConflict) {
		t.Fatalf("idempotency conflict = %v", e)
	}

	if _, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-pack", store.PluginInstallInput{
		Identifier: "official/test-pack", Version: 0,
	}); !errors.Is(e, store.ErrPluginKindNotInstallable) {
		t.Fatalf("pack install = %v", e)
	}
	if _, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-agent", store.PluginInstallInput{
		Identifier: "official/ora-space.opencode", Version: 0,
	}); !errors.Is(e, store.ErrPluginKindNotInstallable) {
		t.Fatalf("agent install = %v", e)
	}
	if _, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, member, path, "member-install", store.PluginInstallInput{
		Identifier: "official/ora-space.tavily-search", Version: 0,
	}); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("member install = %v", e)
	}

	removed, e := f.store.RemoveSpacePlugin(ctx, f.tenant, space, owner, path, "remove-skill", store.PluginRemoveInput{
		Identifier: "official/mattpocock.skills", Version: installed.Version,
	})
	if e != nil {
		t.Fatal(e)
	}
	if removed.DesiredState != "removed" || removed.ObservedState != "removed" || removed.ObservedVersion != nil {
		t.Fatalf("removed = %+v", removed)
	}
	if _, e := f.store.RemoveSpacePlugin(ctx, f.tenant, space, owner, path, "stale-remove", store.PluginRemoveInput{
		Identifier: "official/mattpocock.skills", Version: installed.Version,
	}); !errors.Is(e, store.ErrConflict) {
		t.Fatalf("stale removal = %v", e)
	}
	if _, e := f.store.RemoveSpacePlugin(ctx, f.tenant, space, member, path, "member-remove", store.PluginRemoveInput{
		Identifier: "official/mattpocock.skills", Version: removed.Version,
	}); !errors.Is(e, store.ErrForbidden) {
		t.Fatalf("member removal = %v", e)
	}

	mcp, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-mcp", store.PluginInstallInput{
		Identifier: "official/ora-space.tavily-search", Version: 0,
	})
	if e != nil {
		t.Fatal(e)
	}
	if mcp.Kind != "mcp" || mcp.ObservedState != "installed" || mcp.ObservedVersion == nil ||
		*mcp.ObservedVersion != "0.1.0" {
		t.Fatalf("mcp = %+v", mcp)
	}
	if _, e := f.store.RemoveSpacePlugin(ctx, f.tenant, space, owner, path, "remove-mcp", store.PluginRemoveInput{
		Identifier: "official/ora-space.tavily-search", Version: mcp.Version,
	}); e != nil {
		t.Fatal(e)
	}

	selections, e := f.store.SpacePlugins(ctx, f.tenant, space, member)
	if e != nil {
		t.Fatal(e)
	}
	if len(selections) != 4 {
		t.Fatalf("selections after removal = %+v", selections)
	}
	agents, e := f.store.SpaceAgentPlugins(ctx, f.tenant, space, member)
	if e != nil || len(agents) != 2 {
		t.Fatalf("agent selections = %+v %v", agents, e)
	}
}
