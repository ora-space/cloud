package pluginmarket

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanBuildsCatalogAndSkipsBrokenListings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	entryDir := filepath.Join(root, "registry", "o", "acme.skill")
	if e := os.MkdirAll(entryDir, 0o755); e != nil {
		t.Fatal(e)
	}
	writeTestFile(t, filepath.Join(entryDir, "orax.toml"), `resolver = 1
title = "Acme Skill"
identifier = "acme.skill"
kind = "skill"
version = "1.2.3"
description = "Safe reusable instructions"
homepage = "https://example.com/acme"
license = "MIT"
url = "https://example.com/acme-1.2.3.orax"
sha256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
`)
	writeTestFile(t, filepath.Join(entryDir, "README.md"), "# Acme Skill\n\nUse it safely.\n")
	writeTestFile(t, filepath.Join(entryDir, "logo.light.svg"), "<svg/>")
	writeTestFile(t, filepath.Join(entryDir, "logo.dark.png"), "png")
	writeTestFile(t, filepath.Join(root, "registry", "a", "agent", "orax.toml"), `resolver = 1
title = "Runtime Agent"
identifier = "runtime.agent"
kind = "agent"
version = "0.3.18"
description = "Managed by the installed runtime"
license = "Apache-2.0"
url = "https://example.com/runtime-agent.orax"
sha256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
`)
	writeTestFile(t, filepath.Join(root, "registry", "x", "broken", "orax.toml"), "resolver = 9\n")

	entries, skipped, e := Scan(filepath.Join(root, "registry"), "official", "https://github.com/ora-space/marketplace")
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 1 || len(skipped) != 1 {
		t.Fatalf("entries=%+v skipped=%+v", entries, skipped)
	}
	entry := entries[0]
	if entry.Namespace != "official" || entry.Identifier != "acme.skill" ||
		entry.Version != "1.2.3" || entry.Readme == "" {
		t.Fatalf("entry = %+v", entry)
	}
	logo, ok := entry.Logo.(map[string]any)
	if !ok || logo["light"] == nil || logo["dark"] == nil {
		t.Fatalf("logo = %#v", entry.Logo)
	}
}

func TestNormalizeSourceURL(t *testing.T) {
	t.Parallel()
	if got, e := NormalizeSourceURL("https://github.com/ora-space/marketplace/"); e != nil ||
		got != "https://github.com/ora-space/marketplace" {
		t.Fatalf("url = %q %v", got, e)
	}
	for _, value := range []string{
		"file:///tmp/marketplace",
		"http://github.com/ora-space/marketplace",
		"https://user@github.com/ora-space/marketplace",
		"https://github.com/ora-space/marketplace?ref=main",
		"https://github.com/ora-space/marketplace#main",
	} {
		if _, e := NormalizeSourceURL(value); e == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0o755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(content), 0o644); e != nil {
		t.Fatal(e)
	}
}
