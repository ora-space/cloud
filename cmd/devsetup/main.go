// devsetup turns config.toml into everything the local development stack
// needs: the purpose-separated Ed25519 key pairs and PKCE key the Gateway and
// Cloud share, the external provider's client secret file the Gateway reads, the
// environment file Taskfile.yml loads (CLOUD_*/GATEWAY_* overrides plus
// TEST_DATABASE_URL), and the applied database schema.
//
// configs/*.yaml stay the authoritative service configuration; config.toml
// only supplies the per-developer values, so production deployments never see
// this command. It is safe to rerun: existing keys are kept, the secret and
// env files are rewritten from config.toml, migrations are idempotent. It
// writes only under .local/ (ignored by Git) and never prints private
// material or the DSN, which may carry a password.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/wanglongan587/cloud/internal/config"
	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gateway"
	"github.com/wanglongan587/cloud/internal/repository"
)

// devConfig is the schema of config.toml; config.toml.template documents each field.
type devConfig struct {
	Database struct {
		DSN string `toml:"dsn"`
	} `toml:"database"`
	GitHub struct {
		ClientID     string `toml:"client_id"`
		ClientSecret string `toml:"client_secret"`
	} `toml:"github"`
	IDaaS struct {
		BaseURL          string `toml:"base_url"`
		ClientID         string `toml:"client_id"`
		ClientSecret     string `toml:"client_secret"`
		DisplayNameField string `toml:"display_name_field"`
	} `toml:"idaas"`
	// Plugins stays a pointer so an absent [plugins] section (an older config.toml)
	// is distinguishable from a present-but-empty one: the former is backfilled
	// with the template defaults, the latter is kept exactly as written.
	Plugins *devPluginsConfig `toml:"plugins"`
}

// devPluginsConfig mirrors the service's config.PluginConfig; the interval is a
// string here because TOML durations are spelled "5m" and the service parses it.
type devPluginsConfig struct {
	MarketplaceURL    string `toml:"marketplace_url"`
	MarketplaceBranch string `toml:"marketplace_branch"`
	SyncInterval      string `toml:"sync_interval"`
	SyncEnabled       bool   `toml:"sync_enabled"`
}

// defaultPluginsConfig is the backfill for config.toml files predating the
// [plugins] section; it matches config.toml.template and configs/config.yaml.
func defaultPluginsConfig() *devPluginsConfig {
	return &devPluginsConfig{
		MarketplaceURL:    "https://github.com/ora-space/marketplace",
		MarketplaceBranch: "main",
		SyncInterval:      "5m",
		SyncEnabled:       true,
	}
}

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.toml", "developer configuration; copy config.toml.template")
	dir := flag.String("dir", filepath.Join(".local", "gateway"), "directory that configs/config.yaml and configs/gateway.yaml point at")
	envPath := flag.String("env", filepath.Join(".local", "dev.env"), "environment file that Taskfile.yml loads")
	flag.Parse()
	cfg, e := loadConfig(*configPath)
	if e != nil {
		return e
	}
	if e := materialize(&cfg, *dir, *envPath); e != nil {
		return e
	}
	if e := migrate(context.Background(), cfg.Database.DSN); e != nil {
		return e
	}
	fmt.Println("migrations applied; run `task dev` and open http://localhost:5173")
	return nil
}

// loadConfig decodes config.toml strictly so a misspelled key fails here
// instead of silently leaving an external login unconfigured. [github] and
// [idaas] are optional as a whole and mutually exclusive (the Gateway has one
// external provider): the development provider signs developers in without
// either, but a half-filled section is a mistake, not a choice.
func loadConfig(path string) (devConfig, error) {
	var cfg devConfig
	f, e := os.Open(path)
	if errors.Is(e, fs.ErrNotExist) {
		return cfg, fmt.Errorf("%s not found: copy config.toml.template to %s and fill in [github]", path, path)
	}
	if e != nil {
		return cfg, e
	}
	defer f.Close()
	dec := toml.NewDecoder(f)
	dec.DisallowUnknownFields()
	if e = dec.Decode(&cfg); e != nil {
		var strict *toml.StrictMissingError
		if errors.As(e, &strict) {
			return cfg, fmt.Errorf("parse %s: unknown keys (see config.toml.template):\n%s", path, strict.String())
		}
		return cfg, fmt.Errorf("parse %s: %w", path, e)
	}
	cfg.Database.DSN = strings.TrimSpace(cfg.Database.DSN)
	cfg.GitHub.ClientID = strings.TrimSpace(cfg.GitHub.ClientID)
	cfg.GitHub.ClientSecret = strings.TrimSpace(cfg.GitHub.ClientSecret)
	cfg.IDaaS.BaseURL = strings.TrimSpace(cfg.IDaaS.BaseURL)
	cfg.IDaaS.ClientID = strings.TrimSpace(cfg.IDaaS.ClientID)
	cfg.IDaaS.ClientSecret = strings.TrimSpace(cfg.IDaaS.ClientSecret)
	cfg.IDaaS.DisplayNameField = strings.TrimSpace(cfg.IDaaS.DisplayNameField)
	if cfg.Plugins == nil {
		cfg.Plugins = defaultPluginsConfig()
	} else {
		cfg.Plugins.MarketplaceURL = strings.TrimSpace(cfg.Plugins.MarketplaceURL)
		cfg.Plugins.MarketplaceBranch = strings.TrimSpace(cfg.Plugins.MarketplaceBranch)
		cfg.Plugins.SyncInterval = strings.TrimSpace(cfg.Plugins.SyncInterval)
		if cfg.Plugins.SyncEnabled && cfg.Plugins.MarketplaceURL == "" {
			return cfg, fmt.Errorf("%s: plugins.marketplace_url must be set when plugins.sync_enabled is true (see config.toml.template)", path)
		}
		if cfg.Plugins.SyncEnabled {
			if _, e := time.ParseDuration(cfg.Plugins.SyncInterval); e != nil {
				return cfg, fmt.Errorf("%s: plugins.sync_interval must be a duration like 5m: %w", path, e)
			}
		}
	}
	if cfg.Database.DSN == "" {
		return cfg, fmt.Errorf("%s: database.dsn must be set (see config.toml.template)", path)
	}
	if (cfg.GitHub.ClientID == "") != (cfg.GitHub.ClientSecret == "") {
		return cfg, fmt.Errorf("%s: github.client_id and github.client_secret must be set together, or both left empty", path)
	}
	if (cfg.IDaaS.ClientID == "") != (cfg.IDaaS.ClientSecret == "") {
		return cfg, fmt.Errorf("%s: idaas.client_id and idaas.client_secret must be set together, or both left empty", path)
	}
	if cfg.GitHub.ClientID != "" && cfg.IDaaS.ClientID != "" {
		return cfg, fmt.Errorf("%s: the Gateway has one external provider; fill in [github] or [idaas], not both", path)
	}
	return cfg, nil
}

// externalProvider is the Gateway's login.provider implied by config.toml: the section that is
// filled in, or none for a development-login-only Gateway.
func (c *devConfig) externalProvider() string {
	switch {
	case c.GitHub.ClientID != "":
		return gateway.ProviderGitHub
	case c.IDaaS.ClientID != "":
		return gateway.ProviderHuaweiIDaaS
	default:
		return ""
	}
}

// materialize writes the files the services and Taskfile read. Keys are only
// ever created; the secret and env files are derived from config.toml on
// every run so editing config.toml is enough to change them.
func materialize(cfg *devConfig, dir, envPath string) error {
	if e := os.MkdirAll(dir, 0o700); e != nil {
		return e
	}
	for _, name := range []string{"gateway-service", "user-identity"} {
		created, e := writeKeyPair(filepath.Join(dir, name+".key"), filepath.Join(dir, name+".pem"))
		if e != nil {
			return e
		}
		report(name+" key pair", created)
	}
	pkce := make([]byte, 32)
	if _, e := rand.Read(pkce); e != nil {
		return e
	}
	created, e := writeNew(filepath.Join(dir, "gateway-pkce.key"), pkce)
	if e != nil {
		return e
	}
	report("PKCE key", created)
	// Exactly the selected provider's secret exists afterwards: a stale secret from an earlier
	// config.toml must not outlive its client id.
	for name, secret := range map[string]string{"github-client-secret": cfg.GitHub.ClientSecret, "idaas-client-secret": cfg.IDaaS.ClientSecret} {
		secretPath := filepath.Join(dir, name)
		if secret == "" {
			if e := os.Remove(secretPath); e != nil && !errors.Is(e, fs.ErrNotExist) {
				return e
			}
			continue
		}
		if e := writePrivate(secretPath, []byte(secret)); e != nil {
			return e
		}
		fmt.Printf("wrote %s\n", secretPath)
	}
	if cfg.externalProvider() == "" {
		fmt.Println("no external login configured ([github] and [idaas] empty): only the development login is available")
	}
	if e := os.MkdirAll(filepath.Dir(envPath), 0o700); e != nil {
		return e
	}
	if e := writePrivate(envPath, []byte(renderEnv(cfg))); e != nil {
		return e
	}
	fmt.Printf("wrote %s\n", envPath)
	return nil
}

// renderEnv produces the dotenv file Taskfile.yml loads. Values are
// double-quoted because the DSN contains spaces; the escaping matches the
// dotenv dialect Task parses.
func renderEnv(cfg *devConfig) string {
	var b strings.Builder
	b.WriteString("# Generated by `task setup` from config.toml; edit config.toml and rerun instead.\n")
	// Every variable the file owns is written even when empty so a rerun never leaves a stale
	// value behind; the Gateway treats an empty login.provider (with development_provider) as
	// "development login only".
	for _, kv := range [][2]string{
		{"CLOUD_DATABASE_DSN", cfg.Database.DSN},
		{"GATEWAY_DATABASE_DSN", cfg.Database.DSN},
		{"TEST_DATABASE_URL", cfg.Database.DSN},
		{"GATEWAY_LOGIN_PROVIDER", cfg.externalProvider()},
		{"GATEWAY_GITHUB_CLIENT_ID", cfg.GitHub.ClientID},
		{"GATEWAY_IDAAS_BASE_URL", cfg.IDaaS.BaseURL},
		{"GATEWAY_IDAAS_CLIENT_ID", cfg.IDaaS.ClientID},
		{"GATEWAY_IDAAS_DISPLAY_NAME_FIELD", cfg.IDaaS.DisplayNameField},
		{"CLOUD_PLUGINS_MARKETPLACE_URL", cfg.Plugins.MarketplaceURL},
		{"CLOUD_PLUGINS_MARKETPLACE_BRANCH", cfg.Plugins.MarketplaceBranch},
		{"CLOUD_PLUGINS_SYNC_INTERVAL", cfg.Plugins.SyncInterval},
		{"CLOUD_PLUGINS_SYNC_ENABLED", strconv.FormatBool(cfg.Plugins.SyncEnabled)},
	} {
		fmt.Fprintf(&b, "%s=%s\n", kv[0], quoteEnv(kv[1]))
	}
	return b.String()
}

func quoteEnv(value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(value) + `"`
}

// migrate applies the schema with the DSN from config.toml, the same path
// cloudctl migrate takes, so `task dev` starts against a verified schema.
func migrate(ctx context.Context, dsn string) error {
	db, e := repository.InitDB(ctx, config.DatabaseConfig{
		Driver:          "postgres",
		DSN:             dsn,
		MaxIdleConns:    1,
		MaxOpenConns:    2,
		ConnMaxLifetime: time.Minute,
	})
	if e != nil {
		return fmt.Errorf("connect with database.dsn from config.toml (is PostgreSQL running? try `docker compose up -d --wait`): %w", e)
	}
	s, e := core.NewStore(db)
	if e != nil {
		return e
	}
	defer s.Pool.Close()
	if e = s.Migrate(ctx); e != nil {
		return fmt.Errorf("apply migrations: %w", e)
	}
	return nil
}

func report(what string, created bool) {
	if created {
		fmt.Printf("generated %s\n", what)
		return
	}
	fmt.Printf("kept existing %s\n", what)
}

// writeKeyPair stores a PKCS#8 private key for the Gateway and the matching
// PKIX public key for Cloud. Both files are created together or not at all so
// the pair never drifts.
func writeKeyPair(privatePath, publicPath string) (bool, error) {
	if exists(privatePath) || exists(publicPath) {
		if exists(privatePath) != exists(publicPath) {
			return false, fmt.Errorf("%s and %s must exist together; remove both to regenerate", privatePath, publicPath)
		}
		return false, nil
	}
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return false, e
	}
	privDER, e := x509.MarshalPKCS8PrivateKey(priv)
	if e != nil {
		return false, e
	}
	pubDER, e := x509.MarshalPKIXPublicKey(pub)
	if e != nil {
		return false, e
	}
	if _, e = writeNew(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})); e != nil {
		return false, e
	}
	if _, e = writeNew(publicPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})); e != nil {
		return false, e
	}
	return true, nil
}

// writeNew creates the file owner-readable only and reports false when it
// already exists, so a rerun never replaces keys that a running Cloud trusts.
func writeNew(path string, content []byte) (bool, error) {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(e, fs.ErrExist) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	_, writeErr := f.Write(content)
	return true, errors.Join(writeErr, f.Close())
}

// writePrivate replaces the file's content, keeping it owner-readable only
// even when an earlier run created it with a different mode.
func writePrivate(path string, content []byte) error {
	if e := os.WriteFile(path, content, 0o600); e != nil {
		return e
	}
	return os.Chmod(path, 0o600)
}

func exists(path string) bool {
	_, e := os.Stat(path)
	return e == nil
}
