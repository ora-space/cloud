package pluginmarket

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

	replacement := t.TempDir()
	runTestGit(t, "", "init", "-b", "main", replacement)
	runTestGit(t, replacement, "config", "user.email", "market@example.test")
	runTestGit(t, replacement, "config", "user.name", "Market Test")
	replacementEntry := filepath.Join(replacement, "registry", "o", "replacement.skill")
	writeTestFile(t, filepath.Join(replacementEntry, "orax.toml"), manifestSource("replacement.skill", "2.0.0"))
	runTestGit(t, replacement, "add", ".")
	runTestGit(t, replacement, "commit", "-m", "replacement source")
	syncer.Source.URL = "file://" + filepath.ToSlash(replacement)
	if e := syncer.Sync(context.Background(), sink, time.Now()); e != nil {
		t.Fatal(e)
	}
	if sink.calls != 3 || len(sink.entries) != 1 ||
		sink.entries[0].Identifier != "replacement.skill" {
		t.Fatalf("source URL change = %+v", sink)
	}

	gone := replacement + "-gone"
	if e := os.Rename(replacement, gone); e != nil {
		t.Fatal(e)
	}
	if e := syncer.Sync(context.Background(), sink, time.Now()); e == nil {
		t.Fatal("expected failed refresh")
	}
	if sink.calls != 3 {
		t.Fatalf("failed refresh replaced snapshot: calls=%d", sink.calls)
	}
}

func TestSyncerRejectsEmptySnapshot(t *testing.T) {
	repo := t.TempDir()
	runTestGit(t, "", "init", "-b", "main", repo)
	runTestGit(t, repo, "config", "user.email", "market@example.test")
	runTestGit(t, repo, "config", "user.name", "Market Test")
	entryDir := filepath.Join(repo, "registry", "o", "runtime.agent")
	writeTestFile(t, filepath.Join(entryDir, "orax.toml"), manifestKind("agent", "runtime.agent", "0.3.18"))
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "agent only")

	syncer := &Syncer{
		Source: Source{
			Namespace: "official",
			URL:       "file://" + filepath.ToSlash(repo),
			Branch:    "main",
		},
		WorkDir: filepath.Join(t.TempDir(), "marketplace"),
	}
	sink := &recordingSink{}
	if e := syncer.Sync(context.Background(), sink, time.Now()); e == nil {
		t.Fatal("expected empty snapshot refusal")
	}
	if sink.calls != 0 {
		t.Fatalf("empty snapshot reached sink: calls=%d", sink.calls)
	}
}

func TestSyncerTimesOutHungGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell stub")
	}
	bin := t.TempDir()
	git := filepath.Join(bin, "git")
	if e := os.WriteFile(git, []byte("#!/bin/sh\nsleep 30\n"), 0o755); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := t.TempDir()
	if e := os.Mkdir(filepath.Join(work, ".git"), 0o755); e != nil {
		t.Fatal(e)
	}
	syncer := &Syncer{
		Source:  Source{Namespace: "official", URL: "https://example.com/market", Branch: "main"},
		WorkDir: work,
		Timeout: 100 * time.Millisecond,
	}
	sink := &recordingSink{}
	e := syncer.Sync(context.Background(), sink, time.Now())
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", e)
	}
	if sink.calls != 0 {
		t.Fatalf("timed-out sync reached sink: calls=%d", sink.calls)
	}
}

func manifestSource(identifier, version string) string {
	return manifestKind("skill", identifier, version)
}

func manifestKind(kind, identifier, version string) string {
	return `resolver = 1
title = "Acme Skill"
identifier = "` + identifier + `"
kind = "` + kind + `"
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
