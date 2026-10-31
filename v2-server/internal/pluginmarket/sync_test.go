package pluginmarket

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type recordingSink struct {
	calls   int
	entries []Entry
}

func (s *recordingSink) ReplacePluginCatalog(_ context.Context, entries []Entry, _ time.Time) error {
	s.calls++
	s.entries = append([]Entry(nil), entries...)
	return nil
}

func TestSyncerClonesAndRefreshesARealisticMarketplace(t *testing.T) {
	repo := t.TempDir()
	runTestGit(t, "", "init", "-b", "main", repo)
	runTestGit(t, repo, "config", "user.email", "market@example.test")
	runTestGit(t, repo, "config", "user.name", "Market Test")
	entryDir := filepath.Join(repo, "registry", "o", "acme.skill")
	writeTestFile(t, filepath.Join(entryDir, "orax.toml"), manifestSource("acme.skill", "1.2.3"))
	writeTestFile(t, filepath.Join(entryDir, "README.md"), "# Acme Skill\n\nVersion one.\n")
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "first")

	syncer := &Syncer{
		Source: Source{
			Namespace: "official",
			URL:       "file://" + filepath.ToSlash(repo),
			Branch:    "main",
		},
		WorkDir: filepath.Join(t.TempDir(), "marketplace"),
	}
	sink := &recordingSink{}
	if e := syncer.Sync(context.Background(), sink, time.Now()); e != nil {
		t.Fatal(e)
	}
	if sink.calls != 1 || len(sink.entries) != 1 || sink.entries[0].Version != "1.2.3" {
		t.Fatalf("first sync = %+v", sink)
	}

	writeTestFile(t, filepath.Join(entryDir, "orax.toml"), manifestSource("acme.skill", "1.2.4"))
	writeTestFile(t, filepath.Join(entryDir, "README.md"), "# Acme Skill\n\nVersion two.\n")
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "second")
	if e := syncer.Sync(context.Background(), sink, time.Now()); e != nil {
		t.Fatal(e)
	}
	if sink.calls != 2 || sink.entries[0].Version != "1.2.4" ||
		sink.entries[0].Readme != "# Acme Skill\n\nVersion two.\n" {
		t.Fatalf("second sync = %+v", sink)
	}

	gone := repo + "-gone"
	if e := os.Rename(repo, gone); e != nil {
		t.Fatal(e)
	}
	if e := syncer.Sync(context.Background(), sink, time.Now()); e == nil {
		t.Fatal("expected failed refresh")
	}
	if sink.calls != 2 {
		t.Fatalf("failed refresh replaced snapshot: calls=%d", sink.calls)
	}
}

func manifestSource(identifier, version string) string {
	return `resolver = 1
title = "Acme Skill"
identifier = "` + identifier + `"
kind = "skill"
version = "` + version + `"
description = "Safe reusable instructions"
license = "MIT"
url = "https://example.com/acme.orax"
sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	commandArgs := args
	if dir != "" {
		commandArgs = append([]string{"-C", dir}, args...)
	}
	command := exec.Command("git", commandArgs...)
	if output, e := command.CombinedOutput(); e != nil {
		t.Fatalf("git %v: %v\n%s", commandArgs, e, output)
	}
}
