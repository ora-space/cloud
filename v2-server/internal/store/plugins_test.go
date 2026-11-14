package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestEmptyPluginCatalogKeepsThePreviousSnapshot(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	entry := pluginmarket.Entry{
		Namespace: "official", Identifier: "stable.skill", Title: "Stable",
		Kind: "skill", Version: "1.0.0", Description: "Stable snapshot",
		MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace",
	}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	if e := f.store.ReplacePluginCatalog(f.ctx, nil, time.Now()); e == nil {
		t.Fatal("empty snapshot replaced the catalog")
	}
	catalog, _, e := f.store.PluginCatalog(f.ctx, f.tenant, space)
	if e != nil || len(catalog) != 1 || catalog[0].ID != "official/stable.skill" {
		t.Fatalf("catalog after empty sync = %+v %v", catalog, e)
	}
}

func TestRemoveSpacePluginDeletesAnUndeliveredSkillSelection(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	entry := pluginmarket.Entry{
		Namespace: "official", Identifier: "pending.skill", Title: "Pending Skill",
		Kind: "skill", Version: "1.0.0", URL: "https://example.org/pending.skill",
		SHA256:             "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace",
	}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	installed, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "install-pending", store.PluginInstallInput{
		Identifier: "official/pending.skill",
	})
	if e != nil {
		t.Fatal(e)
	}
	// A Skill selection is installed as soon as it is written, without a project Node receipt.
	if installed.DesiredState != "installed" || installed.ObservedState != "installed" ||
		installed.ObservedVersion == nil || *installed.ObservedVersion != "1.0.0" {
		t.Fatalf("installed = %+v", installed)
	}
	// No project ever received the package: removal deletes the selection row immediately and no
	// removal job is raised.
	removed, e := f.store.RemoveSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "remove-pending", store.PluginRemoveInput{
		Identifier: "official/pending.skill", Version: installed.Version,
	})
	if e != nil {
		t.Fatal(e)
	}
	if removed.DesiredState != "removed" || removed.ObservedState != "removed" || removed.ObservedVersion != nil {
		t.Fatalf("removed = %+v", removed)
	}
	list, e := f.store.SpacePlugins(f.ctx, f.tenant, space)
	if e != nil {
		t.Fatal(e)
	}
	if _, found := findSpacePlugin(list, installed.ID); found {
		t.Fatalf("removed selection is still listed: %+v", list)
	}
	if jobs, e := f.store.SkillJobs(f.ctx); e != nil || len(jobs) != 0 {
		t.Fatalf("undelivered removal raised a job: %+v %v", jobs, e)
	}
}

func TestRemoveSpacePluginNotifiesDeliveredProjectsAndReinstallWorks(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	addProject := func(id string) {
		t.Helper()
		if _, e := f.db.Exec(f.ctx, `INSERT INTO v2_projects(id,tenant_id,data,plugin_space_id) VALUES($1,$2,'{}',$3)`, id, f.tenant, space); e != nil {
			t.Fatal(e)
		}
	}
	addProject("a")
	entry := pluginmarket.Entry{
		Namespace: "official", Identifier: "notify.skill", Title: "Notify Skill",
		Kind: "skill", Version: "1.0.0", URL: "https://example.org/notify.skill",
		SHA256:             "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace",
	}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	installed, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "install-notify", store.PluginInstallInput{
		Identifier: "official/notify.skill",
	})
	if e != nil {
		t.Fatal(e)
	}
	// The project confirms delivery.
	jobs, e := f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 1 || jobs[0].Project != "a" {
		t.Fatal(jobs, e)
	}
	if ok, e := f.store.StartSkillJob(f.ctx, jobs[0]); !ok || e != nil {
		t.Fatal(e)
	}
	if e := f.store.FinishSkillJob(f.ctx, jobs[0], store.SkillJobSucceeded, ""); e != nil {
		t.Fatal(e)
	}
	// Removal keeps the selection and raises a removal job for the delivered project.
	removed, e := f.store.RemoveSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "remove-notify", store.PluginRemoveInput{
		Identifier: "official/notify.skill", Version: installed.Version,
	})
	if e != nil {
		t.Fatal(e)
	}
	if removed.DesiredState != "removed" || removed.ObservedState != "removing" || removed.ObservedVersion == nil {
		t.Fatalf("removed = %+v", removed)
	}
	if list, e := f.store.SpacePlugins(f.ctx, f.tenant, space); e != nil {
		t.Fatal(e)
	} else if _, found := findSpacePlugin(list, installed.ID); !found {
		t.Fatalf("removal dropped the selection before the project confirmed: %+v", list)
	}
	jobs, e = f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 1 || jobs[0].Project != "a" || jobs[0].Desired != "removed" {
		t.Fatalf("removal jobs = %+v %v", jobs, e)
	}
	if ok, e := f.store.StartSkillJob(f.ctx, jobs[0]); !ok || e != nil {
		t.Fatal(e)
	}
	if e := f.store.FinishSkillJob(f.ctx, jobs[0], store.SkillJobSucceeded, ""); e != nil {
		t.Fatal(e)
	}
	// Once every project confirmed the removal, the settled selection row is cleaned up and no
	// further job is raised.
	if e := f.store.DeleteSettledSkillRemovals(f.ctx); e != nil {
		t.Fatal(e)
	}
	if list, e := f.store.SpacePlugins(f.ctx, f.tenant, space); e != nil {
		t.Fatal(e)
	} else if _, found := findSpacePlugin(list, installed.ID); found {
		t.Fatalf("settled removal left a selection: %+v", list)
	}
	if jobs, e := f.store.SkillJobs(f.ctx); e != nil || len(jobs) != 0 {
		t.Fatalf("settled removal raised a job: %+v %v", jobs, e)
	}
	// Reinstalling on the same project re-creates the selection and delivers again.
	reinstalled, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "reinstall-notify", store.PluginInstallInput{
		Identifier: "official/notify.skill",
	})
	if e != nil {
		t.Fatal(e)
	}
	if reinstalled.DesiredState != "installed" || reinstalled.ObservedState != "installed" {
		t.Fatalf("reinstalled = %+v", reinstalled)
	}
	if jobs, e := f.store.SkillJobs(f.ctx); e != nil || len(jobs) != 1 || jobs[0].Project != "a" {
		t.Fatalf("reinstall jobs = %+v %v", jobs, e)
	} else if ok, e := f.store.StartSkillJob(f.ctx, jobs[0]); !ok || e != nil {
		t.Fatalf("start reinstall job %+v ok=%v err=%v", jobs[0], ok, e)
	} else if e := f.store.FinishSkillJob(f.ctx, jobs[0], store.SkillJobSucceeded, ""); e != nil {
		t.Fatal(e)
	}
	result, e := f.store.SkillProgress(f.ctx, f.tenant, space, "official", "notify.skill")
	if e != nil || len(result) != 1 || result[0].State != "installed" {
		t.Fatalf("reinstall progress = %+v %v", result, e)
	}
}

func TestPluginMarketLifecycle(t *testing.T) {
	f := newSquadFixture(t)
	ctx := f.ctx
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	path := "/api/v1/tenants/" + f.tenant + "/spaces/" + space + "/plugins"
	if e := f.store.ReplacePluginCatalog(ctx, []pluginmarket.Entry{
		{Namespace: "official", Identifier: "mattpocock.skills", Title: "Matt Pocock Skills", URL: "https://example.org/skill.orax", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "skill", Version: "1.3.1", Description: "Engineering skills", Readme: "# Matt Pocock Skills\n\nReal skills.", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.tavily-search", Title: "Tavily", Kind: "mcp", Version: "0.1.0", Description: "Web search", Readme: "# Tavily", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.workflow-examples", Title: "Workflow Examples", Kind: "workflow", Version: "0.1.0", Description: "Examples", Readme: "# Workflow Examples", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.opencode", Title: "OpenCode", Kind: "agent", Version: "0.6.8", Description: "Agent", Readme: "# OpenCode", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "ora-space.codex", Title: "Codex", Kind: "agent", Version: "0.3.14", Description: "Agent", Readme: "# Codex", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
		{Namespace: "official", Identifier: "test-pack", Title: "Test Pack", Kind: "pack", Version: "1.0.0", Description: "Pack", PackMembers: []string{"official/mattpocock.skills"}, MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
	}, time.Now()); e != nil {
		t.Fatal(e)
	}

	catalog, syncedAt, e := f.store.PluginCatalog(ctx, f.tenant, space)
	if e != nil {
		t.Fatal(e)
	}
	if syncedAt.IsZero() || len(catalog) != 6 {
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

	initial, e := f.store.SpacePlugins(ctx, f.tenant, space)
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
	if _, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-preinstalled-agent", store.PluginInstallInput{
		Identifier: "official/ora-space.opencode", Version: 0,
	}); !errors.Is(e, store.ErrPluginRuntimeManaged) {
		t.Fatalf("agent install = %v", e)
	}
	var preinstalled store.SpacePlugin
	for _, item := range initial {
		if item.Identifier == "ora-space.opencode" {
			preinstalled = item
			break
		}
	}
	if _, e := f.store.RemoveSpacePlugin(ctx, f.tenant, space, owner, path, "remove-preinstalled-agent", store.PluginRemoveInput{
		Identifier: "official/ora-space.opencode", Version: preinstalled.Version,
	}); !errors.Is(e, store.ErrPluginRuntimeManaged) {
		t.Fatalf("preinstalled agent removal = %v", e)
	}
	// A market Agent has no runtime package to deliver, so the server rejects a new selection
	// instead of pretending to install it.
	if _, e := f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-codex", store.PluginInstallInput{
		Identifier: "official/ora-space.codex", PluginVersion: "0.3.14", Version: 0,
	}); !errors.Is(e, store.ErrPluginKindNotInstallable) {
		t.Fatalf("market agent install = %v", e)
	}
	if agents, e := f.store.SpaceAgentPlugins(ctx, f.tenant, space); e != nil || len(agents) != 2 {
		t.Fatalf("runnable agents after market agent rejection = %+v %v", agents, e)
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
	}); !errors.Is(e, store.ErrPluginNotInstalled) {
		t.Fatalf("stale removal = %v", e)
	}

	_, e = f.store.InstallSpacePlugin(ctx, f.tenant, space, owner, path, "install-mcp", store.PluginInstallInput{Identifier: "official/ora-space.tavily-search"})
	if !errors.Is(e, store.ErrPluginKindNotInstallable) {
		t.Fatal(e)
	}
	selections, e := f.store.SpacePlugins(ctx, f.tenant, space)
	if e != nil {
		t.Fatal(e)
	}
	if len(selections) != 2 {
		t.Fatalf("selections after removal = %+v", selections)
	}
	agents, e := f.store.SpaceAgentPlugins(ctx, f.tenant, space)
	if e != nil || len(agents) != 2 {
		t.Fatalf("agent selections = %+v %v", agents, e)
	}
}

func TestMarketAgentSelectionUpdateIsRejected(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	path := "/api/v1/tenants/" + f.tenant + "/spaces/" + space + "/plugins"
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{
		{Namespace: "official", Identifier: "ora-space.codex", Title: "Codex", Kind: "agent", Version: "0.3.14", Description: "Agent", Readme: "# Codex", MarketplaceVisible: true, SourceURL: "https://github.com/ora-space/marketplace"},
	}, time.Now()); e != nil {
		t.Fatal(e)
	}
	// Simulate a market Agent selection recorded before the read-only change: the row exists with
	// a user as the requester. The update must be rejected, not silently refreshed.
	if _, e := f.db.Exec(f.ctx, `
		INSERT INTO v2_space_agents
			(tenant_id, space_id, name, status, source_namespace, identifier, kind,
			 desired_state, desired_version, observed_state, observed_version, requested_by_user_id)
		VALUES ($1, $2, 'Codex', 'active', 'official', 'ora-space.codex', 'agent',
			'installed', '0.3.14', 'installed', '0.3.14', $3)`,
		f.tenant, space, owner); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, path, "update-market-agent", store.PluginInstallInput{
		Identifier: "official/ora-space.codex", PluginVersion: "0.3.14", Version: 1,
	}); !errors.Is(e, store.ErrPluginKindNotInstallable) {
		t.Fatalf("market agent update = %v", e)
	}
	agents, e := f.store.SpaceAgentPlugins(f.ctx, f.tenant, space)
	if e != nil || len(agents) != 2 {
		t.Fatalf("market agent leaked into executable Agents: %+v %v", agents, e)
	}
	for _, agent := range agents {
		if agent.Identifier == "ora-space.codex" {
			t.Fatalf("market agent leaked into executable Agents: %+v", agent)
		}
	}
}

func TestHookInstallIsRejected(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces = %v %v", spaces, e)
	}
	space := spaces[0].ID
	path := "/api/v1/tenants/" + f.tenant + "/spaces/" + space + "/plugins"
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{{
		Namespace: "official", Identifier: "acme.hooks", Title: "Acme Hooks", Kind: "hook", Version: "1.0.0",
		Description: "Hooks", MarketplaceVisible: true, SourceURL: "https://example.org",
	}}, time.Now()); e != nil {
		t.Fatal(e)
	}

	if _, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, path, "install-hook", store.PluginInstallInput{
		Identifier: "official/acme.hooks", PluginVersion: "1.0.0", Version: 0,
	}); !errors.Is(e, store.ErrPluginKindNotInstallable) {
		t.Fatalf("hook install accepted = %v", e)
	}

	selections, e := f.store.SpacePlugins(f.ctx, f.tenant, space)
	if e != nil {
		t.Fatal(e)
	}
	for _, selection := range selections {
		if selection.Identifier == "acme.hooks" {
			t.Fatalf("hook leaked into selections: %+v", selection)
		}
	}
	agents, e := f.store.SpaceAgentPlugins(f.ctx, f.tenant, space)
	if e != nil {
		t.Fatal(e)
	}
	for _, agent := range agents {
		if agent.Identifier == "acme.hooks" {
			t.Fatalf("hook leaked into executable Agents: %+v", agent)
		}
	}
}

func TestDisabledOfficialCatalogRetainsSnapshotWithoutOfferingInstall(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, storetest.Access(t, f.store, f.tenant, owner))
	if e != nil || len(spaces) != 1 {
		t.Fatalf("spaces=%v %v", spaces, e)
	}
	space := spaces[0].ID
	entry := pluginmarket.Entry{Namespace: "official", Identifier: "saved.skill", Kind: "skill", Version: "1.0.0", MarketplaceVisible: true}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	f.store.DisableOfficialMarket(true)
	catalog, stamp, e := f.store.PluginCatalog(f.ctx, f.tenant, space)
	if e != nil || len(catalog) != 0 || !stamp.IsZero() {
		t.Fatalf("disabled catalog=%v %v %v", catalog, stamp, e)
	}
	if e := f.store.ChangeMarket(f.ctx, f.tenant, space, "official", "sync", store.MarketSource{}); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("sync=%v", e)
	}
	_, e = f.store.PluginPackageSource(f.ctx, f.tenant, space, "official/saved.skill")
	if !errors.Is(e, store.ErrPluginNotFound) {
		t.Fatalf("package=%v", e)
	}
	f.store.DisableOfficialMarket(false)
	catalog, _, e = f.store.PluginCatalog(f.ctx, f.tenant, space)
	if e != nil || len(catalog) != 1 {
		t.Fatalf("retained snapshot=%v %v", catalog, e)
	}
}
