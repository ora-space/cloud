package store_test

import (
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"testing"
	"time"
)

func TestSkillProjectsRequireEveryBoundNode(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, f.tenant, owner)
	if e != nil {
		t.Fatal(e)
	}
	space := spaces[0].ID
	var other string
	e = f.db.QueryRow(f.ctx, `insert into v2_spaces(tenant_id,name,slug,created_by) values($1,'other','other',$2) returning id`, f.tenant, owner).Scan(&other)
	if e != nil {
		t.Fatal(e)
	}
	addProject := func(id, s string) {
		t.Helper()
		_, e := f.db.Exec(f.ctx, `insert into v2_projects(id,tenant_id,data,plugin_space_id) values($1,$2,'{}',$3)`, id, f.tenant, s)
		if e != nil {
			t.Fatal(e)
		}
	}
	addProject("a", space)
	addProject("b", space)
	addProject("outside", other)
	entry := pluginmarket.Entry{Namespace: "official", Identifier: "fixture", Kind: "skill", Version: "1.0.0", MarketplaceVisible: true, URL: "https://example.org/skill", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	selection, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "first", store.PluginInstallInput{Identifier: "official/fixture"})
	if e != nil {
		t.Fatal(e)
	}
	jobs, e := f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 2 || jobs[0].Project != "a" || jobs[1].Project != "b" {
		t.Fatal(jobs, e)
	}
	run := func(j store.SkillJob, fail bool) {
		t.Helper()
		ok, e := f.store.StartSkillJob(f.ctx, j)
		if !ok || e != nil {
			t.Fatal(e)
		}
		if e := f.store.FinishSkillJob(f.ctx, j, fail); e != nil {
			t.Fatal(e)
		}
	}
	state := func(want string) {
		t.Helper()
		if e := f.store.RefreshSkillStates(f.ctx); e != nil {
			t.Fatal(e)
		}
		list, e := f.store.SpacePlugins(f.ctx, f.tenant, space, owner)
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range list {
			if item.ID == selection.ID {
				if item.ObservedState != want {
					t.Fatalf("want %s, got %s", want, item.ObservedState)
				}
				return
			}
		}
		t.Fatal("selection disappeared")
	}
	state("pending")
	run(jobs[0], false)
	state("pending")
	run(jobs[1], true)
	state("failed")
	result, e := f.store.SkillProgress(f.ctx, f.tenant, space, owner, "official", "fixture")
	if e != nil || len(result) != 2 {
		t.Fatal(e, result)
	}
	if _, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "retry", store.PluginInstallInput{Identifier: "official/fixture", Version: selection.Version}); e != nil {
		t.Fatal(e)
	}
	jobs, e = f.store.SkillJobs(f.ctx)
	if e != nil {
		t.Fatal(e)
	}
	for _, j := range jobs {
		run(j, false)
	}
	state("installed")
	addProject("new", space)
	state("pending")
	jobs, e = f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 1 || jobs[0].Project != "new" {
		t.Fatal(jobs, e)
	}
	run(jobs[0], false)
	state("installed")
}
