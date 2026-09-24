// Package config handles application configuration loading and parsing.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/logger"
)

// Config holds all configuration of the application.
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Logger        logger.Config       `mapstructure:"logger"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Auth          AuthConfig          `mapstructure:"auth"`
	Collaboration CollaborationConfig `mapstructure:"collaboration"`
	Control       ControlConfig       `mapstructure:"control"`
	Plugins       PluginConfig        `mapstructure:"plugins"`
}

// PluginConfig is the cloud-side plugin marketplace configuration. Leaf keys
// are bound to CLOUD_PLUGINS_* environment overrides like every other section.
type PluginConfig struct {
	// MarketplaceURL is the git repository the marketplace catalog is synced from.
	MarketplaceURL string `mapstructure:"marketplace_url"`
	// MarketplaceBranch is the branch checked out and fast-forwarded on every sync.
	MarketplaceBranch string `mapstructure:"marketplace_branch"`
	// SyncInterval is the period between catalog syncs; the first sync runs at startup.
	SyncInterval time.Duration `mapstructure:"sync_interval"`
	// SyncEnabled turns the catalog sync loop on. When false the config section may
	// stay empty: no marketplace source is seeded and no sync loop runs.
	SyncEnabled bool `mapstructure:"sync_enabled"`
}

// CollaborationConfig gates optional collaboration-capability wiring on the Store.
type CollaborationConfig struct {
	// DevelopmentFixtures installs the in-memory dev/demo Agent/Team/Workflow fixtures
	// (internal/collab) onto the Store's collaboration ports. Development-only and explicitly
	// enabled: production must leave it false (default), in which case only human targets are
	// served. It must never be coupled to whether an external login provider is enabled.
	DevelopmentFixtures bool `mapstructure:"development_fixtures"`
}

// ControlConfig binds the Controller-facing gRPC listener. Until the authentication ADR adds TLS,
// the address must stay on a loopback or private network; the contract carries bearer credentials
// only.
type ControlConfig struct {
	GRPCAddr string `mapstructure:"grpc_addr"`
}

// AuthConfig contains only internal verification keys, never an external login SDK.
type AuthConfig struct {
	Audience string            `mapstructure:"audience"`
	Keys     []core.TrustedKey `mapstructure:"keys"`
}

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	Port         int           `mapstructure:"port"`
	Mode         string        `mapstructure:"mode"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

// DatabaseConfig holds database connection parameters.
type DatabaseConfig struct {
	Driver          string        `mapstructure:"driver"`
	DSN             string        `mapstructure:"dsn"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
}

// Load reads configuration from file and environment variables.
func Load(configPath string) (*Config, error) {
	v := viper.New()

	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("../configs")
		v.AddConfigPath(".")
	}

	// Read environment variables (e.g., CLOUD_SERVER_PORT=8080)
	v.SetEnvPrefix("CLOUD")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := BindEnvKeys(v, Config{}); err != nil {
		return nil, err
	}

	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	if cfg.Server.ReadTimeout <= 0 || cfg.Server.WriteTimeout <= 0 || cfg.Database.ConnMaxLifetime <= 0 {
		return nil, fmt.Errorf("server timeouts and database.conn_max_lifetime must be positive durations")
	}
	if cfg.Control.GRPCAddr == "" {
		return nil, fmt.Errorf("control.grpc_addr is required")
	}
	if err := validatePlugins(cfg.Plugins); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validatePlugins enforces the plugin marketplace configuration contract. An
// empty section is legal while sync is disabled; once sync is enabled the
// marketplace address and branch must be present and well-formed so a typo
// fails at startup instead of surfacing as a silently empty catalog.
func validatePlugins(p PluginConfig) error {
	if !p.SyncEnabled {
		return nil
	}
	if p.MarketplaceURL == "" {
		return fmt.Errorf("plugins.marketplace_url is required when plugins.sync_enabled is true")
	}
	parsed, err := url.Parse(p.MarketplaceURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("plugins.marketplace_url must be an https git URL without query or fragment")
	}
	if p.MarketplaceBranch == "" {
		return fmt.Errorf("plugins.marketplace_branch must not be empty when plugins.sync_enabled is true")
	}
	if !validBranch(p.MarketplaceBranch) {
		return fmt.Errorf("plugins.marketplace_branch %q is not a valid git branch name", p.MarketplaceBranch)
	}
	if p.SyncInterval <= 0 {
		return fmt.Errorf("plugins.sync_interval must be a positive duration when plugins.sync_enabled is true")
	}
	return nil
}

// validBranch rejects branch names that a clone/checkout could mistake for
// something else: path separators, parent traversal, leading dashes (option
// injection), whitespace and control characters. It is deliberately stricter
// than git's own ref rules because the value comes from deployment
// configuration and is passed to a shelled-out git process.
func validBranch(s string) bool {
	if s == "" || len(s) > 200 || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return false
	}
	return !strings.ContainsAny(s, "/\\ \t\r\n\x00~^:?*[")
}
