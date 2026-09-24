package pluginmarket

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeListing drops a manifest (and optional siblings) under registry/<dir>.
func writeListing(t *testing.T, root, dir, name, manifest string) string {
	t.Helper()
	path := filepath.Join(root, "registry", dir, name, "orax.toml")
	if e := os.MkdirAll(filepath.Dir(path), 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, []byte(manifest), 0o600); e != nil {
		t.Fatal(e)
	}
	return filepath.Dir(path)
}

func TestScanSkipsHiddenAndInvalidListings(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("ab", 32)
	writeListing(t, root, "a", "hello-world", "resolver = 1\nidentifier = \"hello-world\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Hi.\"\nurl = \"https://example.invalid/h.orax\"\nsha256 = \""+digest+"\"\n")
	writeListing(t, root, "c", "hidden", "resolver = 1\nidentifier = \"hidden\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Hidden.\"\nmarketplace_visible = false\n")
	writeListing(t, root, "b", "broken", "resolver = 2\nidentifier = \"broken\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Broken.\"\n")
	entries, skipped, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 1 || entries[0].Identifier != "hello-world" {
		t.Fatalf("catalog = %+v", entries)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "broken") {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestScanDeduplicatesByIdentifier(t *testing.T) {
	root := t.TempDir()
	manifest := "resolver = 1\nidentifier = \"dup\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Dup.\"\n"
	writeListing(t, root, "a", "dup", manifest)
	writeListing(t, root, "b", "dup", manifest)
	entries, skipped, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 1 {
		t.Fatalf("duplicate identifier must collapse to the first in path order: %+v", entries)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "duplicate identifier") {
		t.Fatalf("the losing duplicate must be reported: %v", skipped)
	}
}

func TestScanResolvesLogoVariants(t *testing.T) {
	root := t.TempDir()
	manifest := func(id string) string {
		return "resolver = 1\nidentifier = \"" + id + "\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"P.\"\n"
	}
	writeListing(t, root, "a", "universal", manifest("universal"))
	if e := os.WriteFile(filepath.Join(root, "registry", "a", "universal", "logo.png"), []byte("png"), 0o600); e != nil {
		t.Fatal(e)
	}
	writeListing(t, root, "b", "themed", manifest("themed"))
	if e := os.WriteFile(filepath.Join(root, "registry", "b", "themed", "logo.svg"), []byte("svg"), 0o600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "registry", "b", "themed", "logo.dark.svg"), []byte("svg"), 0o600); e != nil {
		t.Fatal(e)
	}
	writeListing(t, root, "c", "bare", manifest("bare"))
	entries, _, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	byID := map[string]any{}
	for _, entry := range entries {
		byID[entry.Identifier] = entry.Logo
	}
	// A lone universal file serves both themes.
	if !reflect.DeepEqual(byID["universal"], map[string]any{"universal": map[string]any{"role": "universal", "extension": "png"}}) {
		t.Fatalf("universal logo = %#v", byID["universal"])
	}
	// logo.svg beside logo.dark.svg is a themed pair whose light half is the
	// universal file, mirroring desktop's from_roles table.
	if !reflect.DeepEqual(byID["themed"], map[string]any{"light": map[string]any{"role": "universal", "extension": "svg"}, "dark": map[string]any{"role": "dark", "extension": "svg"}}) {
		t.Fatalf("themed logo = %#v", byID["themed"])
	}
	if byID["bare"] != nil {
		t.Fatalf("no icon files must resolve nil: %#v", byID["bare"])
	}
}

func TestScanTruncatesOversizedReadme(t *testing.T) {
	root := t.TempDir()
	dir := writeListing(t, root, "a", "long", "resolver = 1\nidentifier = \"long\"\nkind = \"skill\"\nversion = \"1.0.0\"\ndescription = \"Long.\"\n")
	oversized := strings.Repeat("r", MaxReadmeBytes+1000)
	if e := os.WriteFile(filepath.Join(dir, "README.md"), []byte(oversized), 0o600); e != nil {
		t.Fatal(e)
	}
	entries, _, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 1 || len(entries[0].Readme) != MaxReadmeBytes {
		t.Fatalf("readme must be truncated to %d bytes, got %d", MaxReadmeBytes, len(entries[0].Readme))
	}
}

func TestScanReportsWalkFailure(t *testing.T) {
	_, _, e := Scan(filepath.Join(t.TempDir(), "missing"), "official", "https://example.invalid/market.git")
	if e == nil {
		t.Fatal("a missing registry root must be a fatal scan error")
	}
}
