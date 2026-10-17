package pluginmarket

import (
	"crypto/sha256"
	"encoding/hex"
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

// declarationFor is the shared minimal declaration bytes every copy test uses.
const declarationFor = `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`

// declarationDigestOf hashes the fixture declaration the way the release pipeline would.
func declarationDigestOf(t *testing.T, source string) string {
	t.Helper()
	compiled, e := CompileAgentConfiguration([]byte(source))
	if e != nil {
		t.Fatalf("fixture declaration must compile: %v", e)
	}
	return compiled.Fingerprint
}

// writeCopy writes one static declaration copy beside a listing.
func writeCopy(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if e := os.WriteFile(filepath.Join(dir, name), content, 0o600); e != nil {
		t.Fatal(e)
	}
}

func TestScanResolvesValidatedDeclarationCopies(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("ab", 32)
	valid := writeListing(t, root, "a", "declared", "resolver = 1\nidentifier = \"declared\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Declared.\"\nurl = \"https://example.invalid/d.orax\"\nsha256 = \""+digest+"\"\n\n[agent_configuration]\nschema_version = 2\ndeclaration_sha256 = \""+declarationDigestOf(t, declarationFor)+"\"\ncopy = \"agent-configuration.json\"\n")
	writeCopy(t, valid, "agent-configuration.json", []byte(declarationFor))
	writeListing(t, root, "b", "undeclared", "resolver = 1\nidentifier = \"undeclared\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Undeclared.\"\nurl = \"https://example.invalid/u.orax\"\nsha256 = \""+digest+"\"\n")
	entries, skipped, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	var declared, undeclared *Entry
	for i := range entries {
		switch entries[i].Identifier {
		case "declared":
			declared = &entries[i]
		case "undeclared":
			undeclared = &entries[i]
		}
	}
	if declared == nil || undeclared == nil {
		t.Fatalf("catalog = %+v", entries)
	}
	if declared.AgentConfiguration == nil || declared.AgentConfigurationIssue != "" {
		t.Fatalf("valid association must be consumable: %+v", declared.AgentConfiguration)
	}
	if declared.AgentConfiguration.Declaration != declarationFor {
		t.Fatalf("the exact copy bytes must be retained: %q", declared.AgentConfiguration.Declaration)
	}
	if declared.AgentConfiguration.DeclarationSha256 != declarationDigestOf(t, declarationFor) {
		t.Fatalf("association digest mismatch: %+v", declared.AgentConfiguration)
	}
	// "Not declared" and "declared but invalid" are different facts; both stay visible.
	if undeclared.AgentConfiguration != nil || undeclared.AgentConfigurationIssue != "" {
		t.Fatalf("undeclared listing must carry neither field: %+v", undeclared)
	}
}

func TestScanRecordsBoundedIssuesForInvalidCopies(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	tampered := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["openai-completions"],"modelCardinality":"single"}}}`
	cases := []struct {
		name   string
		issues string
	}{
		{"missing-copy", ""},
		{"digest-mismatch", "digest does not match"},
		{"invalid-declaration", "declaration invalid"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			// The declared digest matches the copy bytes in every case, so each failure surfaces at
			// its own check: a missing copy, bytes that are not what the release associated, and
			// bytes that are exactly what was declared but do not compile.
			copyBytes := []byte(declarationFor)
			declaredDigest := declarationDigestOf(t, declarationFor)
			if testCase.name == "digest-mismatch" {
				copyBytes = []byte(tampered)
			}
			if testCase.name == "invalid-declaration" {
				copyBytes = []byte(`{"schemaVersion":3}`)
				sum := sha256.Sum256(copyBytes)
				declaredDigest = hex.EncodeToString(sum[:])
			}
			entryDir := writeListing(t, root, "a", "broken", "resolver = 1\nidentifier = \"broken\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Broken.\"\nurl = \"https://example.invalid/b.orax\"\nsha256 = \""+digest+"\"\n\n[agent_configuration]\nschema_version = 2\ndeclaration_sha256 = \""+declaredDigest+"\"\ncopy = \"agent-configuration.json\"\n")
			if testCase.name != "missing-copy" {
				writeCopy(t, entryDir, "agent-configuration.json", copyBytes)
			}
			entries, skipped, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
			if e != nil {
				t.Fatal(e)
			}
			if len(skipped) != 0 {
				t.Fatalf("an invalid copy must not skip the listing: %v", skipped)
			}
			if len(entries) != 1 || entries[0].Identifier != "broken" {
				t.Fatalf("catalog = %+v", entries)
			}
			entry := entries[0]
			if entry.AgentConfiguration != nil {
				t.Fatalf("no consumable declaration may survive: %+v", entry.AgentConfiguration)
			}
			if entry.AgentConfigurationIssue == "" || !strings.Contains(entry.AgentConfigurationIssue, testCase.issues) {
				if testCase.issues == "" {
					t.Fatalf("an unbounded issue reason must be recorded: %q", entry.AgentConfigurationIssue)
				} else {
					t.Fatalf("issue must name %q: %q", testCase.issues, entry.AgentConfigurationIssue)
				}
			}
		})
	}
}

func TestScanRejectsDirectoryCopiesAsNonRegular(t *testing.T) {
	root := t.TempDir()
	digest := strings.Repeat("ab", 32)
	entryDir := writeListing(t, root, "a", "dircopy", "resolver = 1\nidentifier = \"dircopy\"\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Dir copy.\"\nurl = \"https://example.invalid/d.orax\"\nsha256 = \""+digest+"\"\n\n[agent_configuration]\nschema_version = 2\ndeclaration_sha256 = \""+declarationDigestOf(t, declarationFor)+"\"\ncopy = \"nested/agent.json\"\n")
	if e := os.MkdirAll(filepath.Join(entryDir, "nested", "agent.json"), 0o700); e != nil {
		t.Fatal(e)
	}
	entries, _, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 1 || entries[0].AgentConfiguration != nil || entries[0].AgentConfigurationIssue == "" {
		t.Fatalf("a directory copy must record an issue without a consumable declaration: %+v", entries)
	}
}
