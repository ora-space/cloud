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
	Directory     DirectoryConfig     `mapstructure:"directory"`
	Control       ControlConfig       `mapstructure:"control"`
	Plugins       PluginConfig        `mapstructure:"plugins"`
	IssueRuns     IssueRunsConfig     `mapstructure:"issue_runs"`
	ObjectStore   ObjectStoreConfig   `mapstructure:"object_store"`
	GitServer     GitServerConfig     `mapstructure:"git_server"`
}

// IssueRunsConfig holds the Thread lifecycle policy that belongs to Cloud's own dispatch loop
// rather than to any individual run (IssueRun B side). Leaf keys are bound to CLOUD_ISSUE_RUNS_*
// environment overrides like every other section.
type IssueRunsConfig struct {
	// AgentStartTimeout bounds startup waiting on database time. A registered execution retains
	// its Workspace while termination is reconciled; expiry alone is never proof it did not act.
	AgentStartTimeout time.Duration `mapstructure:"agent_start_timeout"`
	// AgentRestoreTimeout replaces the normal startup window for a frozen Revision restore.
	AgentRestoreTimeout time.Duration `mapstructure:"agent_restore_timeout"`
	// AgentEndConfirmTimeout bounds an unresolved startup termination without claiming physical cleanup.
	AgentEndConfirmTimeout time.Duration `mapstructure:"agent_end_confirm_timeout"`
	// ThreadIdleTimeout is Thread D4's idle window: how long a Thread may sit `idle` before Cloud
	// asks the session to end. It is a duration configured per deployment, never a per-run column,
	// and the comparison that judges it runs on database time — no Cloud, Controller or Node process
	// clock decides that a Thread is idle. Zero or negative selects the approved default (15 minutes)
	// rather than a zero-length window that would end every Thread the moment it went idle.
	ThreadIdleTimeout time.Duration `mapstructure:"thread_idle_timeout"`

	// DeliveryGiveUpAfter is IssueRun D5's continuous-delivery-failure window: how long a Revision
	// delivery may keep failing before Cloud abandons it and releases the run's Workspace. It is the
	// limit that keeps a Workspace from being held indefinitely by a delivery that will never
	// succeed. Zero or negative selects the approved default (2 hours).
	DeliveryGiveUpAfter time.Duration `mapstructure:"delivery_give_up_after"`

	// DeliveryUnreachableAfter is IssueRun D5's unreachability window: how long the run Workspace's
	// Node may be unreachable before Cloud treats it as gone and closes the run out in the IssueRun D8
	// give-up path, without waiting out the full failure window. Zero or negative selects the
	// approved default (30 minutes).
	DeliveryUnreachableAfter time.Duration `mapstructure:"delivery_unreachable_after"`
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
	// AssetImportEnabled turns the workflow asset import loop on. It is independent of Node
	// installation: the selected package is downloaded by Cloud and its documents become tenant
	// workflows even when the space has no live runtime.
	AssetImportEnabled bool `mapstructure:"asset_import_enabled"`
	// AssetImportInterval is the period between import passes. The first pass runs at startup.
	AssetImportInterval time.Duration `mapstructure:"asset_import_interval"`
}

// CollaborationConfig gates optional collaboration-capability wiring on the Store.
type CollaborationConfig struct {
	// DevelopmentFixtures installs the in-memory dev/demo Agent/Team/Workflow fixtures
	// (internal/collab) onto the Store's collaboration ports. Development-only and explicitly
	// enabled: production must leave it false (default), in which case only human targets are
	// served. It must never be coupled to whether an external login provider is enabled.
	DevelopmentFixtures bool `mapstructure:"development_fixtures"`
}

// DirectoryConfig holds the fixed Tianzhou machine endpoint and secret file.
// Empty endpoint disables corporate lookup on public deployments.
type DirectoryConfig struct {
	Endpoint    string `mapstructure:"endpoint"`
	HWID        string `mapstructure:"hw_id"`
	Environment string `mapstructure:"environment"`
	AppKeyFile  string `mapstructure:"app_key_file"`
}

// ControlConfig binds the mutually authenticated Controller-facing gRPC listener.
// Deployment files and the verified Controller identity remain separate from user credentials.
type ControlConfig struct {
	GRPCAddr           string `mapstructure:"grpc_addr"`
	CertificateFile    string `mapstructure:"certificate_file"`
	PrivateKeyFile     string `mapstructure:"private_key_file"`
	ClientCAFile       string `mapstructure:"client_ca_file"`
	ControllerIdentity string `mapstructure:"controller_identity"`
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

	v.SetDefault("issue_runs.agent_start_timeout", DefaultAgentStartTimeout)
	v.SetDefault("issue_runs.agent_restore_timeout", DefaultAgentRestoreTimeout)
	v.SetDefault("issue_runs.agent_end_confirm_timeout", DefaultAgentEndConfirmTimeout)
	v.SetDefault("plugins.asset_import_interval", DefaultAssetImportInterval)
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
	// Thread D4's approved default. Applied here rather than left at the zero value so an operator
	// who omits the key (or sets a nonsensical value) gets the approved window instead of a
	// zero-length one that would end every idle Thread immediately.
	if cfg.IssueRuns.ThreadIdleTimeout <= 0 {
		cfg.IssueRuns.ThreadIdleTimeout = DefaultThreadIdleTimeout
	}
	if cfg.IssueRuns.AgentStartTimeout <= 0 || cfg.IssueRuns.AgentRestoreTimeout <= 0 || cfg.IssueRuns.AgentEndConfirmTimeout <= 0 {
		return nil, fmt.Errorf("issue_runs startup, restore and end confirmation timeouts must be positive durations")
	}
	// IssueRun D5's two approved defaults, applied for the same reason: an operator who omits the keys
	// gets the limits the decision fixes, not a zero-length window that would abandon every delivery
	// immediately.
	if cfg.IssueRuns.DeliveryGiveUpAfter <= 0 {
		cfg.IssueRuns.DeliveryGiveUpAfter = DefaultDeliveryGiveUpAfter
	}
	if cfg.IssueRuns.DeliveryUnreachableAfter <= 0 {
		cfg.IssueRuns.DeliveryUnreachableAfter = DefaultDeliveryUnreachableAfter
	}

	return &cfg, nil
}

// DefaultThreadIdleTimeout is Thread D4's first-version idle window, applied when
// issue_runs.thread_idle_timeout is absent or not positive.
const DefaultThreadIdleTimeout = 15 * time.Minute

// DefaultAgentStartTimeout bounds the first-record wait; unresolved dispatches remain recoverable.
const DefaultAgentStartTimeout = 2 * time.Minute

// DefaultAgentRestoreTimeout includes restore and startup within one ready-based window.
const DefaultAgentRestoreTimeout = 6 * time.Minute

// DefaultAgentEndConfirmTimeout bounds business waiting, not physical cleanup.
const DefaultAgentEndConfirmTimeout = 5 * time.Minute

// DefaultDeliveryGiveUpAfter and DefaultDeliveryUnreachableAfter are IssueRun D5's first-version
// give-up limits, applied when the corresponding key is absent or not positive.
const (
	DefaultDeliveryGiveUpAfter      = 2 * time.Hour
	DefaultDeliveryUnreachableAfter = 30 * time.Minute
)

// DefaultAssetImportInterval is the first-version workflow asset import cadence. The pass is a
// bounded scan of installed workflow plugins and downloads only due attempts.
const DefaultAssetImportInterval = 30 * time.Second

// validatePlugins enforces the plugin marketplace configuration contract. An
// empty section is legal while sync is disabled; once sync is enabled the
// marketplace address and branch must be present and well-formed so a typo
// fails at startup instead of surfacing as a silently empty catalog.
func validatePlugins(p PluginConfig) error {
	if p.AssetImportEnabled && p.AssetImportInterval <= 0 {
		return fmt.Errorf("plugins.asset_import_interval must be a positive duration when plugins.asset_import_enabled is true")
	}
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
