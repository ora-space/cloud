package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writePluginConfig writes a minimal valid service config plus the given
// plugins section, so the plugin validation matrix exercises exactly that part.
// The control section is required since upstream's clone merge: Load refuses a
// config without control.grpc_addr before plugin validation runs, so the
// fixtures stay environment-independent (no CLOUD_CONTROL_GRPC_ADDR needed).
func writePluginConfig(t *testing.T, plugins string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "server: {port: 8080, mode: test, read_timeout: 5s, write_timeout: 5s}\n" +
		"logger: {level: info, filename: " + filepath.ToSlash(filepath.Join(t.TempDir(), "app.log")) + ", max_size: 1, max_backups: 1, max_age: 1, compress: false, enable_console: false}\n" +
		"database: {driver: postgres, max_idle_conns: 1, max_open_conns: 1, conn_max_lifetime: 1h}\n" +
		"control: {grpc_addr: 127.0.0.1:8082}\n" +
		plugins
	if e := os.WriteFile(path, []byte(contents), 0o600); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestPluginConfigDecodingMatrix(t *testing.T) {
	cases := []struct {
		name    string
		plugins string
		check   func(t *testing.T, cfg *Config)
	}{
		{
			name:    "complete section decodes",
			plugins: "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: main, sync_interval: 5m, sync_enabled: true}\n",
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				p := cfg.Plugins
				if p.MarketplaceURL != "https://example.invalid/market.git" || p.MarketplaceBranch != "main" || p.SyncInterval != 5*time.Minute || !p.SyncEnabled {
					t.Fatalf("plugins section not decoded: %+v", p)
				}
			},
		},
		{
			name:    "absent section means sync disabled",
			plugins: "",
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Plugins.SyncEnabled {
					t.Fatalf("absent section must default to disabled: %+v", cfg.Plugins)
				}
			},
		},
		{
			name:    "empty url with sync disabled is legal",
			plugins: "plugins: {sync_enabled: false}\n",
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Plugins.SyncEnabled {
					t.Fatalf("sync must be disabled: %+v", cfg.Plugins)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, e := Load(writePluginConfig(t, tc.plugins))
			if e != nil {
				t.Fatal(e)
			}
			tc.check(t, cfg)
		})
	}
}

func TestPluginConfigRejectsMalformedValues(t *testing.T) {
	cases := []struct {
		name    string
		plugins string
		want    string
	}{
		{"missing url", "plugins: {marketplace_branch: main, sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_url"},
		{"non https url", "plugins: {marketplace_url: 'http://example.invalid/market.git', marketplace_branch: main, sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_url"},
		{"url with query", "plugins: {marketplace_url: 'https://example.invalid/market.git?signed=1', marketplace_branch: main, sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_url"},
		{"url with fragment", "plugins: {marketplace_url: 'https://example.invalid/market.git#frag', marketplace_branch: main, sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_url"},
		{"url without host", "plugins: {marketplace_url: 'https:///market.git', marketplace_branch: main, sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_url"},
		{"empty branch", "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: '', sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_branch"},
		{"branch with dots", "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: 'feat..x', sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_branch"},
		{"branch with slash", "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: 'feat/x', sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_branch"},
		{"branch with space", "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: 'feat x', sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_branch"},
		{"branch with leading dash", "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: '-f', sync_interval: 5m, sync_enabled: true}\n", "plugins.marketplace_branch"},
		{"zero interval", "plugins: {marketplace_url: 'https://example.invalid/market.git', marketplace_branch: main, sync_interval: 0s, sync_enabled: true}\n", "plugins.sync_interval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, e := Load(writePluginConfig(t, tc.plugins))
			if e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, e)
			}
		})
	}
}

func TestPluginConfigEnvironmentOverrides(t *testing.T) {
	t.Setenv("CLOUD_PLUGINS_MARKETPLACE_URL", "https://env.invalid/market.git")
	t.Setenv("CLOUD_PLUGINS_MARKETPLACE_BRANCH", "release")
	t.Setenv("CLOUD_PLUGINS_SYNC_INTERVAL", "2m")
	t.Setenv("CLOUD_PLUGINS_SYNC_ENABLED", "true")
	cfg, e := Load(writePluginConfig(t, ""))
	if e != nil {
		t.Fatal(e)
	}
	p := cfg.Plugins
	if p.MarketplaceURL != "https://env.invalid/market.git" || p.MarketplaceBranch != "release" || p.SyncInterval != 2*time.Minute || !p.SyncEnabled {
		t.Fatalf("environment must supply plugin keys absent from the file: %+v", p)
	}
}
