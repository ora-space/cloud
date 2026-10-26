package pluginmarket

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Entry is one catalog row the sync writes into plugin_catalog_entries. It
// mirrors the desktop RegistryEntry display fields; the canonical id is the
// namespace/identifier pair the space_plugins selection state addresses.
type Entry struct {
	Namespace          string
	Identifier         string
	Title              string
	Kind               string
	Version            string
	Description        string
	Homepage           string
	License            string
	Logo               any
	URL                string
	SHA256             string
	Targets            []ReleaseTarget
	PackMembers        []string
	Readme             string
	MarketplaceVisible bool
	SourceURL          string
	// AgentConfiguration is the consumable declaration association, set only when the listing
	// declared one and the static copy validated against it. The Declaration field carries the
	// exact copy bytes so the persisted snapshot serves configuration reads without the checkout,
	// the package, or any plugin code.
	AgentConfiguration *AgentConfigurationMetadata
	// AgentConfigurationIssue records the bounded reason a declared association could not be
	// consumed. It is empty for listings that declare nothing and for valid associations: "not
	// declared" and "declared but invalid" are different facts, and only the latter is recorded.
	AgentConfigurationIssue string
}

// AgentConfigurationMetadata is one validated declaration association plus its static copy, as
// the catalog snapshot persists and serves it.
type AgentConfigurationMetadata struct {
	// Declaration schema version the associated copy declares.
	SchemaVersion uint32 `json:"schemaVersion"`
	// SHA-256 of the exact declaration bytes, lowercase hexadecimal.
	DeclarationSha256 string `json:"declarationSha256"`
	// Registry-entry-relative path the copy was read from.
	CopyPath string `json:"copyPath"`
	// The exact static copy bytes; the package's in-archive declaration must match them.
	Declaration string `json:"declaration"`
}

// LogoRole and LogoExtension are the desktop plugin-asset closed sets; the
// stored logo records the composition (role + extension), never the bytes.
type LogoRole string

const (
	RoleUniversal LogoRole = "universal"
	RoleLight     LogoRole = "light"
	RoleDark      LogoRole = "dark"
)

type LogoExtension string

const (
	ExtSvg  LogoExtension = "svg"
	ExtPng  LogoExtension = "png"
	ExtWebp LogoExtension = "webp"
	ExtJpg  LogoExtension = "jpg"
	ExtJpeg LogoExtension = "jpeg"
)

// logoCandidate is one resolved half of an icon composition.
type logoCandidate struct {
	Role      LogoRole      `json:"role"`
	Extension LogoExtension `json:"extension"`
}

// logoVariantsJSON renders the desktop PluginLogoVariants shape: either one
// universal candidate or a themed light/dark pair, null when no icon exists.
func logoVariantsJSON(light, dark, universal *logoCandidate) any {
	candidate := func(c *logoCandidate) map[string]any {
		return map[string]any{"role": string(c.Role), "extension": string(c.Extension)}
	}
	switch {
	case light != nil && dark != nil:
		return map[string]any{"light": candidate(light), "dark": candidate(dark)}
	case light != nil && universal != nil:
		return map[string]any{"light": candidate(light), "dark": candidate(universal)}
	case dark != nil && universal != nil:
		return map[string]any{"light": candidate(universal), "dark": candidate(dark)}
	case light != nil:
		return map[string]any{"universal": candidate(light)}
	case dark != nil:
		return map[string]any{"universal": candidate(dark)}
	case universal != nil:
		return map[string]any{"universal": candidate(universal)}
	}
	return nil
}

// logoExtensionPriority ranks candidate extensions within one role; desktop
// serves the first match so the catalog must record the same winner.
var logoExtensionPriority = []LogoExtension{ExtSvg, ExtPng, ExtWebp, ExtJpg, ExtJpeg}

// resolveLogo picks the icon composition of one entry directory using the
// fixed desktop candidate names (logo.svg, logo.light.png, logo.dark.webp, …).
// Every combination resolves; a directory without any candidate resolves nil.
func resolveLogo(dir string) any {
	best := func(prefix string) *logoCandidate {
		for _, ext := range logoExtensionPriority {
			if _, e := os.Stat(filepath.Join(dir, prefix+string(ext))); e == nil {
				return &logoCandidate{Role: roleFor(prefix), Extension: ext}
			}
		}
		return nil
	}
	light := best("logo.light.")
	dark := best("logo.dark.")
	universal := best("logo.")
	return logoVariantsJSON(light, dark, universal)
}

func roleFor(prefix string) LogoRole {
	switch prefix {
	case "logo.light.":
		return RoleLight
	case "logo.dark.":
		return RoleDark
	}
	return RoleUniversal
}

// readReadme reads the README.md beside a manifest, truncated to
// MaxReadmeBytes so an oversized document never lands in the catalog row in
// full. Invalid UTF-8 reads as no documentation rather than a sync failure.
func readReadme(dir string) string {
	content, e := os.Open(filepath.Join(dir, "README.md"))
	if e != nil {
		return ""
	}
	defer content.Close()
	bounded := &io.LimitedReader{R: content, N: MaxReadmeBytes}
	b, e := io.ReadAll(bounded)
	if e != nil || !utf8.Valid(b) {
		return ""
	}
	return string(b)
}

// Scan builds the catalog entries for one synced checkout directory. Invalid
// listings are skipped individually and reported, never aborting the scan:
// one broken orax.toml must not take the whole marketplace down. A directory
// walk failure is fatal — the caller keeps the previous catalog snapshot.
// Deduplication follows desktop: entries sort by canonical id and the first in
// path order wins, so two directories publishing the same identifier resolve
// deterministically.
func Scan(registryRoot, namespace, sourceURL string) ([]Entry, []string, error) {
	var manifests []string
	e := filepath.WalkDir(registryRoot, func(path string, d fs.DirEntry, e error) error {
		if e != nil || d.IsDir() {
			return e
		}
		if d.Name() == "orax.toml" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if e != nil {
		return nil, nil, fmt.Errorf("walk registry: %w", e)
	}
	sort.Strings(manifests)
	entries := []Entry{}
	skipped := []string{}
	seen := map[string]bool{}
	for _, path := range manifests {
		source, e := os.ReadFile(path)
		if e != nil {
			skipped = append(skipped, path+": "+e.Error())
			continue
		}
		manifest, e := parseManifest(source)
		if e != nil {
			skipped = append(skipped, path+": "+e.Error())
			continue
		}
		// A hidden listing never enters the catalog snapshot; the visibility
		// filter runs before an entry exists, exactly like desktop's index build.
		if !manifest.MarketplaceVisible {
			continue
		}
		// The namespace comes from the publishing source, never the manifest, so
		// the same identifier from two sources stays two distinct entries.
		id := namespace + "/" + manifest.Identifier
		if seen[id] {
			skipped = append(skipped, path+": duplicate identifier "+id)
			continue
		}
		seen[id] = true
		dir := filepath.Dir(path)
		association, issue := resolveAgentConfiguration(dir, manifest)
		entries = append(entries, Entry{
			Namespace:               namespace,
			Identifier:              manifest.Identifier,
			Title:                   manifest.Title,
			Kind:                    manifest.Kind,
			Version:                 manifest.Version,
			Description:             manifest.Description,
			Homepage:                manifest.Homepage,
			License:                 manifest.License,
			Logo:                    resolveLogo(dir),
			URL:                     manifest.URL,
			SHA256:                  manifest.SHA256,
			Targets:                 manifest.Targets,
			PackMembers:             manifest.PackMembers,
			Readme:                  readReadme(dir),
			MarketplaceVisible:      manifest.MarketplaceVisible,
			SourceURL:               sourceURL,
			AgentConfiguration:      association,
			AgentConfigurationIssue: issue,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Namespace+"/"+entries[i].Identifier < entries[j].Namespace+"/"+entries[j].Identifier
	})
	return entries, skipped, nil
}

// agentConfigurationIssueMaximum bounds the recorded reason so a hostile copy cannot balloon the
// catalog row or smuggle content through the issue field.
const agentConfigurationIssueMaximum = 200

// resolveAgentConfiguration reads and validates one listing's static declaration copy inside the
// synced checkout.
//
// Reading the copy is the only filesystem work beyond the manifest and README, and it stays
// inside the entry directory (see readDeclarationCopy); the bytes must then hash and compile to
// exactly the associated identity. A listing that declares nothing consumes nothing. A declared
// association that fails any check leaves the entry visible with a bounded issue and no
// consumable declaration: the plugin stays installable as a package, it just cannot offer
// personal configuration until its publisher fixes the release.
func resolveAgentConfiguration(dir string, manifest *Manifest) (association *AgentConfigurationMetadata, issue string) {
	declared := manifest.AgentConfiguration
	if declared == nil {
		return nil, ""
	}
	source, issue := readDeclarationCopy(dir, declared.CopyPath)
	if issue != "" {
		return nil, issue
	}
	if !utf8.Valid(source) {
		return nil, "copy is not valid UTF-8"
	}
	digest := sha256.Sum256(source)
	if hex.EncodeToString(digest[:]) != declared.DeclarationSha256 {
		return nil, "copy digest does not match the declared declaration_sha256"
	}
	compiled, e := CompileAgentConfiguration(source)
	if e != nil {
		if agentError, ok := e.(*AgentConfigurationError); ok {
			return nil, boundIssue("declaration invalid: " + agentError.Code + ": " + agentError.Reason)
		}
		return nil, "declaration invalid"
	}
	if compiled.SchemaVersion != declared.SchemaVersion {
		return nil, boundIssue(fmt.Sprintf("copy declares schema version %d but the association declares %d", compiled.SchemaVersion, declared.SchemaVersion))
	}
	if compiled.Fingerprint != declared.DeclarationSha256 {
		return nil, "copy fingerprint disagrees with the associated digest"
	}
	return &AgentConfigurationMetadata{
		SchemaVersion:     declared.SchemaVersion,
		DeclarationSha256: declared.DeclarationSha256,
		CopyPath:          declared.CopyPath,
		Declaration:       string(source),
	}, ""
}

// readDeclarationCopy reads the static declaration copy at the slash-separated copyPath relative
// to the entry directory dir, returning the bytes or a bounded issue that never names a server
// path.
//
// The copy is published data and must be self-contained in the entry, so no segment of its path
// may be a symbolic link, wherever it points: a link inside the entry has no legitimate use (the
// release pipeline can copy the file) and does not survive a checkout with core.symlinks=false,
// while a link leaving the entry would let a market repository choose which host file Cloud
// reads. The segment walk enforces that rule; opening through an os.Root bound to dir keeps the
// read inside the entry even if a segment is swapped after the walk, and statting and reading
// the same open file removes the check-then-use window on the final segment. The manifest path
// grammar already excludes parent, absolute and separator escapes; the root is the second line.
func readDeclarationCopy(dir, copyPath string) (source []byte, issue string) {
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, "copy unreadable"
	}
	defer func() { _ = root.Close() }()
	segments := strings.Split(copyPath, "/")
	for index := range segments {
		info, e := root.Lstat(filepath.Join(segments[:index+1]...))
		if e != nil {
			return nil, "copy unreadable"
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, "copy path must not traverse a symbolic link"
		}
	}
	file, e := root.Open(filepath.FromSlash(copyPath))
	if e != nil {
		return nil, "copy unreadable"
	}
	defer func() { _ = file.Close() }()
	info, e := file.Stat()
	if e != nil {
		return nil, "copy unreadable"
	}
	if !info.Mode().IsRegular() {
		return nil, "copy must be a regular file"
	}
	exceeds := boundIssue(fmt.Sprintf("copy exceeds %d bytes", maxAgentDeclarationBytes))
	if info.Size() > maxAgentDeclarationBytes {
		return nil, exceeds
	}
	// The limit reads one byte past the bound so a file that grew after Stat is still refused.
	source, e = io.ReadAll(io.LimitReader(file, maxAgentDeclarationBytes+1))
	if e != nil {
		return nil, "copy unreadable"
	}
	if len(source) > maxAgentDeclarationBytes {
		return nil, exceeds
	}
	return source, ""
}

// boundIssue truncates one recorded reason so no copy content can grow the catalog row.
func boundIssue(reason string) string {
	if len(reason) > agentConfigurationIssueMaximum {
		reason = reason[:agentConfigurationIssueMaximum]
	}
	// A byte cut can split a multi-byte rune, and platform error strings can carry raw bytes;
	// dropping invalid sequences keeps the bounded reason valid UTF-8 so one entry's problem
	// cannot fail the whole catalog sync transaction — the exact failure this bound exists to
	// prevent ("record a bounded reason; the other valid entries still sync").
	// PostgreSQL text also rejects NUL, which is valid UTF-8. Other control characters have
	// no diagnostic value in this public single-line field either.
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(reason, ""))
}
