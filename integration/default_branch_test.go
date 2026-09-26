package integration

import (
	"testing"

	"github.com/wanglongan587/cloud/internal/core"
)

// Cloud never reads the remote repository, so a Project must name a concrete default branch: an
// omitted, blank or HEAD branch is refused before anything is written, and an isolated
// Workspace's baseRef HEAD is stored as the Project's concrete default branch. A Project created
// before the rule, which still stores HEAD, cannot turn baseRef HEAD into a branch.
//
// Evidence for specs test-cases/cloud/operation/workspace-runtime-lifecycle.md
// #a-workspace-never-stores-head-as-its-requested-ref.
func TestProjectsNameAConcreteDefaultBranch(t *testing.T) {
	f := setup(t)
	for key, body := range map[string]core.Object{
		"omitted": {"name": "P", "repositoryUrl": "https://example.invalid/repo.git"},
		"blank":   {"name": "P", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "  "},
		"head":    {"name": "P", "repositoryUrl": "https://example.invalid/repo.git", "defaultBranch": "HEAD"},
	} {
		if out := f.call("POST", f.path("/projects"), body, "branch-"+key, 400); out.S("code") != "default_branch_required" {
			t.Fatalf("%s default branch: expected default_branch_required, got %v", key, out)
		}
	}
	if f.scalar("SELECT count(*) FROM projects") != 0 {
		t.Fatal("a refused Project was written")
	}

	created := f.create("branch-main")
	pid := created.O("resource").S("id")
	f.drain()
	isolated := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Task", "baseRef": "HEAD"}, "isolated-head", 202)
	if ref := isolated.O("resource").S("requestedRef"); ref != "main" {
		t.Fatalf("baseRef HEAD must be stored as the Project's default branch, got %q", ref)
	}

	f.drain()
	_, e := f.store.Pool.Exec("UPDATE projects SET default_branch='HEAD' WHERE id=$1", pid)
	must(t, e)
	if out := f.call("POST", f.path("/projects/"+pid+"/workspaces"), core.Object{"title": "Legacy", "baseRef": "HEAD"}, "isolated-legacy", 400); out.S("code") != "default_branch_required" {
		t.Fatalf("a legacy HEAD Project must refuse baseRef HEAD, got %v", out)
	}
}
