package pluginmarket

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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
		{"missing-copy", "copy unreadable"},
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
			if testCase.name == "missing-copy" && entry.AgentConfigurationIssue != "copy unreadable" {
				t.Fatalf("unreadable copy must not expose server paths: %q", entry.AgentConfigurationIssue)
			}
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

func TestBoundIssueCutsOnARuneBoundary(t *testing.T) {
	// Byte 200 (index 199) falls inside a multi-byte rune: the bounded reason must still be
	// valid UTF-8, or PostgreSQL would reject it as a text parameter and one entry's problem
	// would fail the whole catalog sync transaction — the exact failure this bound exists to
	// prevent ("record a bounded reason; the other valid entries still sync").
	reason := strings.Repeat("a", 198) + "界" + strings.Repeat("b", 40)
	if len(reason) <= agentConfigurationIssueMaximum {
		t.Fatalf("the fixture must exceed the bound: %d bytes", len(reason))
	}
	bounded := boundIssue(reason)
	if !utf8.ValidString(bounded) {
		t.Fatalf("bounded reason is not valid UTF-8: %q", bounded)
	}
	if want := strings.Repeat("a", 198); bounded != want {
		t.Fatalf("the cut must drop the partial rune, not keep it: %q", bounded)
	}
	if boundIssue("copy unreadable") != "copy unreadable" {
		t.Fatal("a short reason must pass through unchanged")
	}
}

func TestBoundIssueRemovesControls(t *testing.T) {
	if got := boundIssue("bad\x00\n\r\t\x7f\u0085copy\xff"); got != "badcopy" {
		t.Fatalf("catalog reason must be bounded database-safe text: %q", got)
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

// writeDeclaredListing writes an agent listing whose association points at copyPath with the
// given schema version and declaration digest, and returns its entry directory.
func writeDeclaredListing(t *testing.T, root, name string, schemaVersion int, declarationDigest, copyPath string) string {
	t.Helper()
	return writeListing(t, root, "a", name, fmt.Sprintf("resolver = 1\nidentifier = %q\nkind = \"agent\"\nversion = \"1.0.0\"\ndescription = \"Declared.\"\nurl = \"https://example.invalid/d.orax\"\nsha256 = %q\n\n[agent_configuration]\nschema_version = %d\ndeclaration_sha256 = %q\ncopy = %q\n", name, strings.Repeat("ab", 32), schemaVersion, declarationDigest, copyPath))
}

// writeFileAt writes content at path, creating its parent directories.
func writeFileAt(t *testing.T, path string, content []byte) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(path, content, 0o600); e != nil {
		t.Fatal(e)
	}
}

// symlinkOrSkip creates link -> target, skipping where the host cannot create symbolic links
// (Windows without developer mode or the symlink privilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(link), 0o700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(target, link); e != nil {
		t.Skipf("symbolic links unavailable on this host: %v", e)
	}
}

// scanOnlyEntry scans root and returns its single catalog entry.
func scanOnlyEntry(t *testing.T, root string) Entry {
	t.Helper()
	entries, skipped, e := Scan(filepath.Join(root, "registry"), "official", "https://example.invalid/market.git")
	if e != nil {
		t.Fatal(e)
	}
	if len(skipped) != 0 || len(entries) != 1 {
		t.Fatalf("an invalid copy must keep exactly its listing: entries=%+v skipped=%v", entries, skipped)
	}
	return entries[0]
}

func TestScanRefusesSymbolicLinksOnTheCopyPath(t *testing.T) {
	// Every link target holds the exact associated bytes, so only the link itself can explain a
	// refusal: a link is refused wherever it sits on the path and wherever it points.
	valid := []byte(declarationFor)
	cases := []struct {
		name string
		link func(t *testing.T, root, entryDir string)
	}{
		{"intermediate-directory-leaves-the-entry", func(t *testing.T, root, entryDir string) {
			writeFileAt(t, filepath.Join(root, "outside", "agent.json"), valid)
			symlinkOrSkip(t, filepath.Join(root, "outside"), filepath.Join(entryDir, "nested"))
		}},
		{"intermediate-directory-stays-in-the-entry", func(t *testing.T, root, entryDir string) {
			writeFileAt(t, filepath.Join(entryDir, "real", "agent.json"), valid)
			symlinkOrSkip(t, "real", filepath.Join(entryDir, "nested"))
		}},
		{"final-segment-leaves-the-entry", func(t *testing.T, root, entryDir string) {
			writeFileAt(t, filepath.Join(root, "outside", "agent.json"), valid)
			symlinkOrSkip(t, filepath.Join(root, "outside", "agent.json"), filepath.Join(entryDir, "nested", "agent.json"))
		}},
		{"final-segment-stays-in-the-entry", func(t *testing.T, root, entryDir string) {
			writeFileAt(t, filepath.Join(entryDir, "nested", "real.json"), valid)
			symlinkOrSkip(t, "real.json", filepath.Join(entryDir, "nested", "agent.json"))
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			entryDir := writeDeclaredListing(t, root, "linked", 2, declarationDigestOf(t, declarationFor), "nested/agent.json")
			testCase.link(t, root, entryDir)
			entry := scanOnlyEntry(t, root)
			if entry.AgentConfiguration != nil {
				t.Fatalf("a linked copy must not become consumable: %+v", entry.AgentConfiguration)
			}
			if entry.AgentConfigurationIssue != "copy path must not traverse a symbolic link" {
				t.Fatalf("issue must name the link refusal without paths: %q", entry.AgentConfigurationIssue)
			}
		})
	}
}

func TestReadDeclarationCopyStaysInsideTheEntryWithoutTheGrammar(t *testing.T) {
	// The manifest grammar rejects parent segments first; this calls the reader directly to show
	// the root bound alone still refuses an escape to a readable, valid file next to the entry.
	root := t.TempDir()
	entryDir := filepath.Join(root, "entry")
	writeFileAt(t, filepath.Join(entryDir, "orax.toml"), []byte("placeholder"))
	writeFileAt(t, filepath.Join(root, "outside", "agent.json"), []byte(declarationFor))
	source, issue := readDeclarationCopy(entryDir, "../outside/agent.json")
	if source != nil || issue != "copy unreadable" {
		t.Fatalf("an escaping path must read nothing and name no path: source=%q issue=%q", source, issue)
	}
}

func TestScanResolvesNestedDeclarationCopies(t *testing.T) {
	root := t.TempDir()
	entryDir := writeDeclaredListing(t, root, "nested", 2, declarationDigestOf(t, declarationFor), "nested/dir/agent-configuration.json")
	writeFileAt(t, filepath.Join(entryDir, "nested", "dir", "agent-configuration.json"), []byte(declarationFor))
	entry := scanOnlyEntry(t, root)
	if entry.AgentConfigurationIssue != "" || entry.AgentConfiguration == nil {
		t.Fatalf("a nested regular copy must resolve: issue=%q", entry.AgentConfigurationIssue)
	}
	if entry.AgentConfiguration.Declaration != declarationFor || entry.AgentConfiguration.CopyPath != "nested/dir/agent-configuration.json" {
		t.Fatalf("nested copy must keep its exact bytes and declared path: %+v", entry.AgentConfiguration)
	}
}

func TestScanRecordsBoundedIssuesForMalformedCopyBytes(t *testing.T) {
	oversized := []byte(strings.Repeat(" ", maxAgentDeclarationBytes+1))
	notUTF8 := []byte("{\"schemaVersion\":2,\xff}")
	cases := []struct {
		name          string
		content       []byte
		schemaVersion int
		issue         string
	}{
		{"exceeds-the-byte-bound", oversized, 2, fmt.Sprintf("copy exceeds %d bytes", maxAgentDeclarationBytes)},
		{"not-utf-8", notUTF8, 2, "copy is not valid UTF-8"},
		{"schema-version-disagrees", []byte(declarationFor), 3, "copy declares schema version 2 but the association declares 3"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			sum := sha256.Sum256(testCase.content)
			entryDir := writeDeclaredListing(t, root, "malformed", testCase.schemaVersion, hex.EncodeToString(sum[:]), "agent-configuration.json")
			writeCopy(t, entryDir, "agent-configuration.json", testCase.content)
			entry := scanOnlyEntry(t, root)
			if entry.AgentConfiguration != nil {
				t.Fatalf("no consumable declaration may survive: %+v", entry.AgentConfiguration)
			}
			if entry.AgentConfigurationIssue != testCase.issue {
				t.Fatalf("issue = %q, want %q", entry.AgentConfigurationIssue, testCase.issue)
			}
		})
	}
}
