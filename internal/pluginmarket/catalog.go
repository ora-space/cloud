package pluginmarket

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
		entries = append(entries, Entry{
			Namespace:          namespace,
			Identifier:         manifest.Identifier,
			Title:              manifest.Title,
			Kind:               manifest.Kind,
			Version:            manifest.Version,
			Description:        manifest.Description,
			Homepage:           manifest.Homepage,
			License:            manifest.License,
			Logo:               resolveLogo(dir),
			URL:                manifest.URL,
			SHA256:             manifest.SHA256,
			Targets:            manifest.Targets,
			PackMembers:        manifest.PackMembers,
			Readme:             readReadme(dir),
			MarketplaceVisible: manifest.MarketplaceVisible,
			SourceURL:          sourceURL,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Namespace+"/"+entries[i].Identifier < entries[j].Namespace+"/"+entries[j].Identifier
	})
	return entries, skipped, nil
}
