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
	run := func(j store.SkillJob, outcome store.SkillJobOutcome) {
		t.Helper()
		ok, e := f.store.StartSkillJob(f.ctx, j)
		if !ok || e != nil {
			t.Fatal(e)
		}
		if e := f.store.FinishSkillJob(f.ctx, j, outcome); e != nil {
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
	run(jobs[0], store.SkillJobSucceeded)
	state("pending")
	run(jobs[1], store.SkillJobFailed)
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
		run(j, store.SkillJobSucceeded)
	}
	state("installed")
	addProject("new", space)
	state("pending")
	jobs, e = f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 1 || jobs[0].Project != "new" {
		t.Fatal(jobs, e)
	}
	run(jobs[0], store.SkillJobSucceeded)
	state("installed")
}

func TestTransientSkillFailureRetriesWithBackoff(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", "owner")
	spaces, e := f.store.TenantSpaces(f.ctx, f.tenant, owner)
	if e != nil {
		t.Fatal(e)
	}
	space := spaces[0].ID
	if _, e := f.db.Exec(f.ctx, `insert into v2_projects(id,tenant_id,data,plugin_space_id) values('project',$1,'{}',$2)`, f.tenant, space); e != nil {
		t.Fatal(e)
	}
	entry := pluginmarket.Entry{Namespace: "official", Identifier: "retry", Kind: "skill", Version: "1.0.0", MarketplaceVisible: true, URL: "https://example.org/retry", SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}
	if e := f.store.ReplacePluginCatalog(f.ctx, []pluginmarket.Entry{entry}, time.Now()); e != nil {
		t.Fatal(e)
	}
	selection, e := f.store.InstallSpacePlugin(f.ctx, f.tenant, space, owner, "plugins", "retry-install", store.PluginInstallInput{Identifier: "official/retry"})
	if e != nil {
		t.Fatal(e)
	}
	jobs, e := f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 1 {
		t.Fatal(jobs, e)
	}
	job := jobs[0]
	if ok, e := f.store.StartSkillJob(f.ctx, job); !ok || e != nil {
		t.Fatal(ok, e)
	}
	if e := f.store.FinishSkillJob(f.ctx, job, store.SkillJobRetry); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RefreshSkillStates(f.ctx); e != nil {
		t.Fatal(e)
	}
	list, e := f.store.SpacePlugins(f.ctx, f.tenant, space, owner)
	selected, found := findSpacePlugin(list, selection.ID)
	if e != nil || !found || selected.ObservedState != "failed" || selected.InstallError == nil || *selected.InstallError != "部分项目暂时失败，将自动重试。" {
		t.Fatal(list, e)
	}
	if jobs, e := f.store.SkillJobs(f.ctx); e != nil || len(jobs) != 0 {
		t.Fatalf("retry became due before backoff elapsed: %+v %v", jobs, e)
	}
	var attempts int
	var future bool
	if e := f.db.QueryRow(f.ctx, `select retry_count,retry_at>now() from v2_skill_results where selection_id=$1 and project_id='project'`, selection.ID).Scan(&attempts, &future); e != nil {
		t.Fatal(e)
	}
	if attempts != 1 || !future {
		t.Fatalf("retry metadata attempts=%d future=%t", attempts, future)
	}
	if _, e := f.db.Exec(f.ctx, `update v2_skill_results set retry_at=now()-interval '1 second' where selection_id=$1 and project_id='project'`, selection.ID); e != nil {
		t.Fatal(e)
	}
	jobs, e = f.store.SkillJobs(f.ctx)
	if e != nil || len(jobs) != 1 {
		t.Fatal(jobs, e)
	}
	if ok, e := f.store.StartSkillJob(f.ctx, jobs[0]); !ok || e != nil {
		t.Fatal(ok, e)
	}
	if e := f.store.FinishSkillJob(f.ctx, jobs[0], store.SkillJobSucceeded); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RefreshSkillStates(f.ctx); e != nil {
		t.Fatal(e)
	}
	list, e = f.store.SpacePlugins(f.ctx, f.tenant, space, owner)
	selected, found = findSpacePlugin(list, selection.ID)
	if e != nil || !found || selected.ObservedState != "installed" || selected.InstallError != nil {
		t.Fatal(list, e)
	}
}

func findSpacePlugin(list []store.SpacePlugin, id string) (store.SpacePlugin, bool) {
	for _, item := range list {
		if item.ID == id {
			return item, true
		}
	}
	return store.SpacePlugin{}, false
}
