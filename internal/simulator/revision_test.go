package simulator

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wanglongan587/cloud/internal/controlpb"
)

// Snapshot retains user commits and tracked deletion/new files without hooks, branch or index writes,
// and the final commit pushed to a delivery ref restores the cumulative tree elsewhere.
func TestRevisionSnapshotIsRecoverableFromItsDeliveryRef(t *testing.T) {
	ctx := t.Context()
	repo := filepath.Join(t.TempDir(), "repo")
	run := func(args ...string) string {
		t.Helper()
		out, err := git(ctx, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	run("init", "--initial-branch=main", repo)
	run("-C", repo, "config", "user.name", "User")
	run("-C", repo, "config", "user.email", "user@example.invalid")
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked", "base\n")
	write("deleted", "remove me\n")
	write(".gitignore", "ignored\n")
	run("-C", repo, "add", ".")
	run("-C", repo, "commit", "-m", "base")
	write("tracked", "agent commit\n")
	run("-C", repo, "add", "tracked")
	run("-C", repo, "commit", "-m", "agent commit")
	head := run("-C", repo, "rev-parse", "HEAD")
	// A failing hook must never veto the internal snapshot.
	hook := filepath.Join(repo, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	write("tracked", "staged work\n")
	run("-C", repo, "add", "tracked")
	write("tracked", "final working tree\n")
	write("new", "new result\n")
	write("ignored", "never archive\n")
	if err := os.Remove(filepath.Join(repo, "deleted")); err != nil {
		t.Fatal(err)
	}
	indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	statusBefore := run("-C", repo, "status", "--porcelain=v2", "--untracked-files=all")
	ref := "refs/ora/revisions/test-run"
	final, err := snapshotRevision(ctx, repo, t.TempDir(), ref, &controlpb.GitIdentity{Name: "Ada", Email: "ada@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if final == head || run("-C", repo, "rev-parse", "HEAD") != head || run("-C", repo, "status", "--porcelain=v2", "--untracked-files=all") != statusBefore {
		t.Fatal("snapshot changed the branch or working tree")
	}
	indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil || string(indexAfter) != string(indexBefore) {
		t.Fatal("snapshot changed the real index", err)
	}
	server := filepath.Join(t.TempDir(), "server.git")
	run("init", "--bare", "--quiet", server)
	delivery := "refs/ora/deliveries/00000000-0000-4000-8000-000000000001"
	run("-C", repo, "push", "--quiet", "--", server, final+":"+delivery)
	restored := filepath.Join(t.TempDir(), "restored")
	run("init", "--initial-branch=main", restored)
	run("-C", restored, "fetch", "--", server, delivery)
	run("-C", restored, "checkout", "--detach", "FETCH_HEAD")
	if run("-C", restored, "rev-parse", "HEAD") != final || run("-C", restored, "show", "HEAD:tracked") != "final working tree" || run("-C", restored, "show", "HEAD:new") != "new result" {
		t.Fatal("the delivery ref did not restore the cumulative final tree")
	}
	for _, name := range []string{"ignored", "deleted"} {
		if _, err := os.Stat(filepath.Join(restored, name)); !os.IsNotExist(err) {
			t.Fatal("unexpected file in restored tree", name, err)
		}
	}
	if run("-C", repo, "show", "-s", "--format=%an <%ae>|%cn <%ce>", final) != "Ada <ada@example.invalid>|Ora <revision@ora.invalid>" {
		t.Fatal("snapshot identity did not match session input")
	}
}
