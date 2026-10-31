// Package pluginmarket mirrors the configured marketplace repository into v2-server's catalog
// snapshot. It owns validation and scanning only; database replacement is the caller's boundary.
package pluginmarket

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	maxManifestBytes = 1 << 20
	maxReadmeBytes   = 256 << 10
)

var pluginKinds = map[string]bool{
	"workbench": true,
	"agent":     true,
	"webview":   true,
	"skill":     true,
	"mcp":       true,
	"hook":      true,
	"pack":      true,
	"workflow":  true,
}

var canonicalTargets = map[string]bool{
	"x86_64-pc-windows-msvc":     true,
	"i686-pc-windows-msvc":       true,
	"aarch64-pc-windows-msvc":    true,
	"x86_64-pc-windows-gnu":      true,
	"aarch64-pc-windows-gnu":     true,
	"x86_64-unknown-linux-gnu":   true,
	"aarch64-unknown-linux-gnu":  true,
	"x86_64-unknown-linux-musl":  true,
	"aarch64-unknown-linux-musl": true,
	"x86_64-apple-darwin":        true,
	"aarch64-apple-darwin":       true,
}

var (
	semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	slugPattern   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

type rawManifest struct {
	Resolver           int64              `toml:"resolver"`
	Identifier         string             `toml:"identifier"`
	Title              string             `toml:"title"`
	Kind               string             `toml:"kind"`
	Version            string             `toml:"version"`
	Description        string             `toml:"description"`
	Homepage           string             `toml:"homepage"`
	License            string             `toml:"license"`
	URL                string             `toml:"url"`
	SHA256             string             `toml:"sha256"`
	Targets            []rawReleaseTarget `toml:"targets"`
	Pack               *rawPack           `toml:"pack"`
	MarketplaceVisible *bool              `toml:"marketplace_visible"`
}

type rawReleaseTarget struct {
	Target string `toml:"target"`
	URL    string `toml:"url"`
	SHA256 string `toml:"sha256"`
}

type rawPack struct {
	Members []string `toml:"members"`
}

// ReleaseTarget is one validated target-specific package download.
type ReleaseTarget struct {
	Target string `json:"target"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Manifest is a validated resolver-1 marketplace listing.
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
	PackMembers        []string
	MarketplaceVisible bool
}

func parseManifest(source []byte) (*Manifest, error) {
	if len(source) > maxManifestBytes {
		return nil, fmt.Errorf("orax.toml exceeds %d bytes", maxManifestBytes)
	}
	var raw rawManifest
	if e := toml.Unmarshal(source, &raw); e != nil {
		return nil, fmt.Errorf("orax.toml: %w", e)
	}
	if raw.Resolver != 1 {
		return nil, fmt.Errorf("resolver: unsupported value %d", raw.Resolver)
	}
	if !validIdentifier(raw.Identifier) {
		return nil, fmt.Errorf("identifier: must be one or two lowercase slug segments")
	}
	title := raw.Title
	if title == "" {
		title = raw.Identifier
	}
	if e := validateText(title, 128, false); e != nil {
		return nil, fmt.Errorf("title: %w", e)
	}
	if !pluginKinds[raw.Kind] {
		return nil, fmt.Errorf("kind: unsupported value %q", raw.Kind)
	}
	if !semverPattern.MatchString(raw.Version) {
		return nil, fmt.Errorf("version: must be a semantic version")
	}
	if e := validateText(raw.Description, 1000, false); e != nil {
		return nil, fmt.Errorf("description: %w", e)
	}
	if raw.Homepage != "" {
		if e := validateHomepage(raw.Homepage); e != nil {
			return nil, fmt.Errorf("homepage: %w", e)
		}
	}
	if raw.License != "" {
		if e := validateText(raw.License, 256, true); e != nil {
			return nil, fmt.Errorf("license: %w", e)
		}
	}
	if raw.URL != "" {
		if e := validateLocator(raw.URL); e != nil {
			return nil, fmt.Errorf("url: %w", e)
		}
	}
	if raw.SHA256 != "" && !sha256Pattern.MatchString(raw.SHA256) {
		return nil, fmt.Errorf("sha256: must be 64 hexadecimal characters")
	}
	hasUniversal := raw.URL != "" || raw.SHA256 != ""
	hasTargets := len(raw.Targets) > 0
	if raw.Kind == "pack" && (hasUniversal || hasTargets) {
		return nil, fmt.Errorf("pack: must not declare a release")
	}
	if hasUniversal && hasTargets {
		return nil, fmt.Errorf("targets: must not be combined with url/sha256")
	}
	if hasUniversal && (raw.URL == "" || raw.SHA256 == "") {
		return nil, fmt.Errorf("url: universal release requires both url and sha256")
	}
	var targets []ReleaseTarget
	if hasTargets {
		if raw.Kind != "agent" && raw.Kind != "hook" {
			return nil, fmt.Errorf("targets: only agent and hook releases may be targeted")
		}
		seen := map[string]bool{}
		for i, target := range raw.Targets {
			if !canonicalTargets[target.Target] {
				return nil, fmt.Errorf("targets[%d].target: unsupported target %q", i, target.Target)
			}
			if seen[target.Target] {
				return nil, fmt.Errorf("targets[%d].target: duplicate target", i)
			}
			if e := validateLocator(target.URL); e != nil {
				return nil, fmt.Errorf("targets[%d].url: %w", i, e)
			}
			if !sha256Pattern.MatchString(target.SHA256) {
				return nil, fmt.Errorf("targets[%d].sha256: must be 64 hexadecimal characters", i)
			}
			seen[target.Target] = true
			targets = append(targets, ReleaseTarget{Target: target.Target, URL: target.URL, SHA256: target.SHA256})
		}
	}
	visible := true
	if raw.MarketplaceVisible != nil {
		visible = *raw.MarketplaceVisible
	}
	var members []string
	if raw.Pack != nil {
		members = append(members, raw.Pack.Members...)
	}
	return &Manifest{
		Identifier: raw.Identifier, Title: title, Kind: raw.Kind, Version: raw.Version,
		Description: raw.Description, Homepage: raw.Homepage, License: raw.License,
		URL: raw.URL, SHA256: raw.SHA256, Targets: targets, PackMembers: members,
		MarketplaceVisible: visible,
	}, nil
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return false
	}
	for _, part := range parts {
		if len(part) > 63 || !slugPattern.MatchString(part) {
			return false
		}
	}
	return true
}

func validateText(value string, maximum int, asciiOnly bool) error {
	if value == "" {
		return fmt.Errorf("must not be empty")
	}
	if len(value) > maximum {
		return fmt.Errorf("exceeds %d bytes", maximum)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("must not have leading or trailing whitespace")
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("must not contain control characters")
		}
		if asciiOnly && char > 0x7f {
			return fmt.Errorf("must be ASCII")
		}
	}
	return nil
}

func validateHomepage(value string) error {
	parsed, e := url.Parse(value)
	if e != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an https URL without query or fragment")
	}
	return nil
}

func validateLocator(value string) error {
	if !strings.Contains(value, "://") {
		if value == "" || len(value) > 1024 || value != strings.TrimSpace(value) {
			return fmt.Errorf("object key must be between 1 and 1024 bytes")
		}
		if strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) {
			return fmt.Errorf("object key must not be absolute")
		}
		for _, part := range strings.FieldsFunc(value, func(char rune) bool { return char == '/' || char == '\\' }) {
			if part == ".." {
				return fmt.Errorf("object key must not contain a parent segment")
			}
		}
		return nil
	}
	parsed, e := url.Parse(value)
	if e != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an https URL or object key")
	}
	return nil
}
