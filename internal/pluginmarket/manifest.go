// Package pluginmarket syncs the configured plugin marketplace git repository into the cloud's
// PostgreSQL catalog snapshot. It mirrors the desktop marketplace contract (orax.toml layout,
// identifier namespaces, release targets) so the cloud catalog stays byte-compatible with what
// the desktop plugin-manager consumes; the cloud never modifies the desktop repository.
package pluginmarket

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// MaxManifestBytes bounds one orax.toml before parsing so a malicious marketplace
// repository cannot make the sync read unbounded memory.
const MaxManifestBytes = 1 << 20

// MaxReadmeBytes is the stored README cap: a larger document is truncated rather
// than pulled into memory or refused, because the catalog detail page renders
// the stored text directly.
const MaxReadmeBytes = 256 << 10

// pluginKinds is the closed resolver-1 kind set shared with the desktop manifest.
var pluginKinds = map[string]bool{
	"workbench": true, "agent": true, "webview": true, "skill": true,
	"mcp": true, "hook": true, "pack": true, "workflow": true,
}

// canonicalTargetTriples is the desktop allowlist of Rust target triples; an
// unknown string is a packaging typo, not an installable architecture.
var canonicalTargetTriples = map[string]bool{
	"x86_64-pc-windows-msvc": true, "i686-pc-windows-msvc": true, "aarch64-pc-windows-msvc": true,
	"x86_64-pc-windows-gnu": true, "aarch64-pc-windows-gnu": true,
	"x86_64-unknown-linux-gnu": true, "aarch64-unknown-linux-gnu": true,
	"x86_64-unknown-linux-musl": true, "aarch64-unknown-linux-musl": true,
	"x86_64-apple-darwin": true, "aarch64-apple-darwin": true,
}

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// ManifestError describes why one orax.toml failed validation. It is a plain
// error the sync loop records per skipped entry; it never aborts the whole
// catalog build.
type ManifestError struct{ Field, Reason string }

func (e *ManifestError) Error() string { return fmt.Sprintf("%s: %s", e.Field, e.Reason) }

func manifestError(field, reason string) *ManifestError {
	return &ManifestError{Field: field, Reason: reason}
}

// rawManifest is the TOML shape of a marketplace orax.toml. Unknown keys are
// ignored so newer desktop fields do not break the cloud sync.
type rawManifest struct {
	Resolver           int64                  `toml:"resolver"`
	Identifier         string                 `toml:"identifier"`
	Title              string                 `toml:"title"`
	Kind               string                 `toml:"kind"`
	Version            string                 `toml:"version"`
	Description        string                 `toml:"description"`
	Homepage           string                 `toml:"homepage"`
	License            string                 `toml:"license"`
	URL                string                 `toml:"url"`
	SHA256             string                 `toml:"sha256"`
	Targets            []rawReleaseTarget     `toml:"targets"`
	AgentConfiguration *rawAgentConfiguration `toml:"agent_configuration"`
	Pack               *rawPack               `toml:"pack"`
	MarketplaceVisible *bool                  `toml:"marketplace_visible"`
}

// rawAgentConfiguration is the release-form `[agent_configuration]` publication association.
// Unknown keys inside the section are rejected (unlike unknown top-level keys) because the
// association is load-bearing for install verification: a listing carrying fields this parser
// does not know fails loudly instead of being reinterpreted.
type rawAgentConfiguration struct {
	SchemaVersion     uint64 `toml:"schema_version"`
	DeclarationSha256 string `toml:"declaration_sha256"`
	Copy              string `toml:"copy"`
}

type rawReleaseTarget struct {
	Target string `toml:"target"`
	URL    string `toml:"url"`
	SHA256 string `toml:"sha256"`
}

type rawPack struct {
	Members []string `toml:"members"`
}

// ReleaseTarget is one validated [[targets]] triple, field-compatible with the
// desktop PluginReleaseTarget.
type ReleaseTarget struct {
	Target string `json:"target"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Manifest is one validated marketplace listing.
type Manifest struct {
	Identifier         string
	Title              string
	Kind               string
	Version            string
	Description        string
	Homepage           string
	License            string
	URL                string
	SHA256             string
	Targets            []ReleaseTarget
	AgentConfiguration *AgentConfigurationAssociation
	PackMembers        []string
	MarketplaceVisible bool
}

// AgentConfigurationAssociation is one listing's declaration publication binding: the
// declaration schema version, the SHA-256 of the exact declaration bytes every package of the
// release must carry, and the registry-entry-relative path of the static copy the sync reads.
// The namespace half of the canonical plugin id never appears here because it belongs to the
// publishing marketplace source, never the manifest.
type AgentConfigurationAssociation struct {
	SchemaVersion     uint32
	DeclarationSha256 string
	CopyPath          string
}

// parseManifest validates the desktop marketplace orax.toml contract. It
// mirrors desktop's crates/plugin-manifest resolver-1 rules field by field so a
// listing the desktop indexes is accepted and one it rejects is skipped.
func parseManifest(source []byte) (*Manifest, error) {
	if len(source) > MaxManifestBytes {
		return nil, fmt.Errorf("orax.toml exceeds %d bytes", MaxManifestBytes)
	}
	var raw rawManifest
	if e := toml.Unmarshal(source, &raw); e != nil {
		return nil, fmt.Errorf("orax.toml: %w", e)
	}
	if raw.Resolver != 1 {
		return nil, manifestError("resolver", fmt.Sprintf("unsupported resolver %d", raw.Resolver))
	}
	if !validIdentifier(raw.Identifier) {
		return nil, manifestError("identifier", "must be one or two lowercase slug segments joined by a dot, at most 128 bytes")
	}
	title := raw.Title
	if title == "" {
		title = raw.Identifier
	}
	if e := validateText(title, 128, false); e != nil {
		return nil, manifestError("title", e.Error())
	}
	if !pluginKinds[raw.Kind] {
		return nil, manifestError("kind", fmt.Sprintf("unsupported plugin kind %q", raw.Kind))
	}
	if !semverPattern.MatchString(raw.Version) {
		return nil, manifestError("version", "must be a semantic version")
	}
	if e := validateText(raw.Description, 1000, false); e != nil {
		return nil, manifestError("description", e.Error())
	}
	if raw.Homepage != "" {
		if e := validateHomepage(raw.Homepage); e != nil {
			return nil, manifestError("homepage", e.Error())
		}
	}
	if raw.License != "" {
		if e := validateText(raw.License, 256, true); e != nil {
			return nil, manifestError("license", e.Error())
		}
	}
	if raw.URL != "" {
		if e := validateLocator(raw.URL); e != nil {
			return nil, manifestError("url", e.Error())
		}
	}
	if raw.SHA256 != "" && !sha256Pattern.MatchString(raw.SHA256) {
		return nil, manifestError("sha256", "must be exactly 64 hexadecimal characters")
	}
	hasUniversal := raw.URL != "" || raw.SHA256 != ""
	hasTargets := len(raw.Targets) > 0
	// A pack is an orchestration entry, not a package: it carries no
	// downloadable bytes of its own, so any release declaration is a schema
	// violation rather than a selectable release source.
	if raw.Kind == "pack" {
		if hasUniversal {
			return nil, manifestError("url", "a pack listing must not declare a release")
		}
		if hasTargets {
			return nil, manifestError("targets", "a pack listing must not declare a release")
		}
	}
	if hasUniversal && hasTargets {
		return nil, manifestError("targets", "url/sha256 and [[targets]] are mutually exclusive")
	}
	if hasUniversal && (raw.URL == "" || raw.SHA256 == "") {
		return nil, manifestError("url", "a universal release must carry both url and sha256")
	}
	var targets []ReleaseTarget
	if hasTargets {
		// Only kinds that ship native per-target binaries may declare targeted
		// releases; the host checks compatibility against this list before download.
		if raw.Kind != "agent" && raw.Kind != "hook" {
			return nil, manifestError("targets", "only agent and hook kinds may declare targeted releases")
		}
		seen := map[string]bool{}
		for i, t := range raw.Targets {
			if !canonicalTargetTriples[t.Target] {
				return nil, manifestError(fmt.Sprintf("targets[%d].target", i), "unknown Rust target triple")
			}
			if seen[t.Target] {
				return nil, manifestError(fmt.Sprintf("targets[%d].target", i), "duplicate target triple")
			}
			if e := validateLocator(t.URL); e != nil {
				return nil, manifestError(fmt.Sprintf("targets[%d].url", i), e.Error())
			}
			if !sha256Pattern.MatchString(t.SHA256) {
				return nil, manifestError(fmt.Sprintf("targets[%d].sha256", i), "must be exactly 64 hexadecimal characters")
			}
			seen[t.Target] = true
			targets = append(targets, ReleaseTarget(t))
		}
	}
	visible := true
	if raw.MarketplaceVisible != nil {
		visible = *raw.MarketplaceVisible
	}
	association, e := parseAgentConfiguration(source, raw.AgentConfiguration, raw.Kind, hasUniversal, hasTargets)
	if e != nil {
		return nil, e
	}
	return &Manifest{
		Identifier:         raw.Identifier,
		Title:              title,
		Kind:               raw.Kind,
		Version:            raw.Version,
		Description:        raw.Description,
		Homepage:           raw.Homepage,
		License:            raw.License,
		URL:                raw.URL,
		SHA256:             raw.SHA256,
		Targets:            targets,
		AgentConfiguration: association,
		PackMembers:        packMembers(raw.Pack),
		MarketplaceVisible: visible,
	}, nil
}

func packMembers(p *rawPack) []string {
	if p == nil {
		return nil
	}
	return p.Members
}

// maxCopyPathBytes and maxCopyPathSegmentBytes bound the declared static-copy path, mirroring the
// desktop manifest crate byte for byte.
const (
	maxCopyPathBytes        = 256
	maxCopyPathSegmentBytes = 128
)

// parseAgentConfiguration validates the release-form `[agent_configuration]` section, mirroring
// the desktop manifest crate: agent kind only, release source required, positive schema version,
// canonical digest, and the closed copy-path grammar that keeps a validated path containable
// inside the registry entry on every host.
func parseAgentConfiguration(
	source []byte,
	raw *rawAgentConfiguration,
	kind string,
	hasUniversal, hasTargets bool,
) (*AgentConfigurationAssociation, error) {
	if raw == nil {
		return nil, nil
	}
	if kind != "agent" {
		return nil, manifestError("agent_configuration", fmt.Sprintf("only agent listings may declare it (kind is %q)", kind))
	}
	if !hasUniversal && !hasTargets {
		return nil, manifestError("agent_configuration", "requires the listing to declare a release package")
	}
	if e := rejectUnknownAgentConfigurationKeys(source); e != nil {
		return nil, e
	}
	if raw.SchemaVersion == 0 || raw.SchemaVersion > 0xFFFFFFFF {
		return nil, manifestError("agent_configuration.schema_version", "must be a positive integer")
	}
	if !sha256Pattern.MatchString(raw.DeclarationSha256) {
		return nil, manifestError("agent_configuration.declaration_sha256", "must be exactly 64 hexadecimal characters")
	}
	if e := validCopyPath(raw.Copy); e != nil {
		return nil, manifestError("agent_configuration.copy", e.Error())
	}
	return &AgentConfigurationAssociation{
		SchemaVersion:     uint32(raw.SchemaVersion),
		DeclarationSha256: strings.ToLower(raw.DeclarationSha256),
		CopyPath:          raw.Copy,
	}, nil
}

// rejectUnknownAgentConfigurationKeys re-reads the manifest as a generic table so keys inside
// the section fail the listing the way the desktop's strict section deserializer does; the
// top-level document keeps tolerating unknown keys for forward compatibility.
func rejectUnknownAgentConfigurationKeys(source []byte) error {
	var document map[string]any
	if e := toml.Unmarshal(source, &document); e != nil {
		return fmt.Errorf("orax.toml: %w", e)
	}
	section, ok := document["agent_configuration"].(map[string]any)
	if !ok {
		return manifestError("agent_configuration", "must be a table")
	}
	for key := range section {
		switch key {
		case "schema_version", "declaration_sha256", "copy":
		default:
			return manifestError("agent_configuration", "unknown key "+key)
		}
	}
	return nil
}

// validCopyPath applies the closed path grammar shared with the desktop: '/'-separated segments
// matching [A-Za-z0-9][A-Za-z0-9._-]*, which excludes separators, escapes, absolute locations,
// parent segments, Windows drives, whitespace, control characters, and non-ASCII.
func validCopyPath(value string) error {
	if value == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(value) > maxCopyPathBytes {
		return fmt.Errorf("exceeds %d bytes", maxCopyPathBytes)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || len(segment) > maxCopyPathSegmentBytes {
			return fmt.Errorf("segments must be between 1 and %d bytes", maxCopyPathSegmentBytes)
		}
		for index, char := range []byte(segment) {
			isLower := char >= 'a' && char <= 'z'
			isUpper := char >= 'A' && char <= 'Z'
			isDigit := char >= '0' && char <= '9'
			isPunct := char == '.' || char == '_' || char == '-'
			isAlnum := isLower || isUpper || isDigit
			if index == 0 && !isAlnum || index > 0 && !isAlnum && !isPunct {
				return fmt.Errorf("segments must match [A-Za-z0-9][A-Za-z0-9._-]*")
			}
		}
		if segment == "." || segment == ".." {
			return fmt.Errorf("segments must not be relative references")
		}
	}
	return nil
}

// validIdentifier applies the desktop PluginName grammar: at most two
// dot-separated slug segments, each a lowercase ASCII slug without leading,
// trailing or consecutive hyphens.
func validIdentifier(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	segments := strings.Split(s, ".")
	if len(segments) > 2 {
		return false
	}
	for _, segment := range segments {
		if !slugPattern.MatchString(segment) || len(segment) > 63 {
			return false
		}
	}
	return true
}

// validateText applies the shared text policies: non-empty, bounded bytes, no
// leading/trailing whitespace, no control characters, and — for license — ASCII
// only, mirroring desktop validate_text.
func validateText(s string, maximum int, asciiOnly bool) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(s) > maximum {
		return fmt.Errorf("exceeds %d bytes", maximum)
	}
	if s != strings.TrimSpace(s) {
		return fmt.Errorf("must not have leading or trailing whitespace")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
	}
	if asciiOnly {
		for _, r := range s {
			if r > 0x7f {
				return fmt.Errorf("must be ASCII")
			}
		}
	}
	return nil
}

// validateHomepage requires an HTTPS URL with no query or fragment, mirroring
// desktop HomepageUrl.
func validateHomepage(s string) error {
	parsed, e := url.Parse(s)
	if e != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an https URL without query or fragment")
	}
	return nil
}

// validateLocator accepts either a credential-free HTTPS URL (a signing query
// is allowed) or an S3 object key, mirroring desktop ReleaseLocator.
func validateLocator(s string) error {
	if !strings.Contains(s, "://") {
		if s == "" || len(s) > 1024 {
			return fmt.Errorf("object key must be between 1 and 1024 bytes")
		}
		if s != strings.TrimSpace(s) {
			return fmt.Errorf("object key must not have leading or trailing whitespace")
		}
		for _, r := range s {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("object key must not contain control characters")
			}
		}
		if strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) {
			return fmt.Errorf("object key must not be an absolute path")
		}
		for _, segment := range strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '\\' }) {
			if segment == ".." {
				return fmt.Errorf("object key must not contain a parent segment")
			}
		}
		return nil
	}
	parsed, e := url.Parse(s)
	if e != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an https URL or an S3 object key")
	}
	return nil
}
