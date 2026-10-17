package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/config"
)

func TestPublicOriginRequiresHTTPSOutsideLoopbackDevelopment(t *testing.T) {
	cases := []struct {
		name, in string
		dev      bool
		want     string
		ok       bool
	}{
		{"https origin", "https://app.example.com", false, "https://app.example.com", true},
		{"https with port", "https://app.example.com:8443", false, "https://app.example.com:8443", true},
		{"http in production", "http://app.example.com", false, "", false},
		{"http loopback without development", "http://localhost:5173", false, "", false},
		{"http loopback with development", "http://localhost:5173", true, "http://localhost:5173", true},
		{"http 127.0.0.1 with development", "http://127.0.0.1:8081", true, "http://127.0.0.1:8081", true},
		{"http public host even in development", "http://app.example.com", true, "", false},
		{"path not allowed", "https://app.example.com/app", false, "", false},
		{"query not allowed", "https://app.example.com?x=1", false, "", false},
		{"userinfo not allowed", "https://u:p@app.example.com", false, "", false},
		{"empty", "", false, "", false},
	}
	for _, tc := range cases {
		got, e := PublicOrigin(tc.in, tc.dev)
		if (e == nil) != tc.ok || got != tc.want {
			t.Errorf("%s: PublicOrigin(%q,%v) = (%q,%v) want (%q, ok=%v)", tc.name, tc.in, tc.dev, got, e, tc.want, tc.ok)
		}
	}
}

func validConfig() Config {
	return Config{
		Server:   config.ServerConfig{Port: 8081, Mode: "test", ReadTimeout: time.Second, WriteTimeout: time.Second},
		Database: config.DatabaseConfig{Driver: "postgres", DSN: "x", ConnMaxLifetime: time.Hour},
		Public:   PublicConfig{BaseURL: "https://app.example.com"},
		Login:    LoginConfig{Provider: ProviderGitHub, PKCEKeyFile: "/run/pkce"},
		Cloud:    CloudConfig{Upstream: "http://127.0.0.1:8080"},
		Tokens:   TokenConfig{Issuer: "iss", Audience: "aud", ServiceSubject: "gw", ServiceKeyID: "s", UserKeyID: "u", ServicePrivateKeyFile: "/run/s", UserPrivateKeyFile: "/run/u"},
		GitHub:   GitHubConfig{ClientID: "id", ClientSecretFile: "/run/secret"},
	}
}

func TestConfigDefaultsAndSessionLifetimeBounds(t *testing.T) {
	cfg := validConfig()
	if e := cfg.applyDefaults(); e != nil {
		t.Fatal(e)
	}
	if cfg.Session.TTL != DefaultSessionLifetime || cfg.Login.Provider != ProviderGitHub || cfg.Login.AttemptTTL != 10*time.Minute || cfg.Tokens.Lifetime != time.Minute || cfg.GitHub.Source != "github.com" || cfg.IDaaS.Timeout != 10*time.Second || cfg.Login.RateLimitBurst != cfg.Login.RateLimitPerMinute {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero session ttl is invalid after defaults are skipped", func(c *Config) { c.Session.TTL = -time.Hour }},
		{"session ttl above 90 days", func(c *Config) { c.Session.TTL = MaxSessionLifetime + time.Second }},
		{"attempt ttl below one minute", func(c *Config) { c.Login.AttemptTTL = 30 * time.Second }},
		{"attempt ttl above one hour", func(c *Config) { c.Login.AttemptTTL = 2 * time.Hour }},
		{"token lifetime above cloud ceiling", func(c *Config) { c.Tokens.Lifetime = 6 * time.Minute }},
		{"http public origin", func(c *Config) { c.Public.BaseURL = "http://app.example.com" }},
		{"upstream with path", func(c *Config) { c.Cloud.Upstream = "http://127.0.0.1:8080/api" }},
		{"upstream without scheme", func(c *Config) { c.Cloud.Upstream = "127.0.0.1:8080" }},
		{"missing pkce key", func(c *Config) { c.Login.PKCEKeyFile = "" }},
		{"missing github client id", func(c *Config) { c.GitHub.ClientID = "" }},
		{"github client id without secret file", func(c *Config) { c.GitHub.ClientSecretFile = "" }},
		{"unknown provider", func(c *Config) { c.Login.Provider = "ldap" }},
		{"no provider at all on a production origin", func(c *Config) { c.Login.Provider = "" }},
		{"development provider on a production origin", func(c *Config) { c.Login.DevelopmentProvider = true }},
		{"missing user key id", func(c *Config) { c.Tokens.UserKeyID = "" }},
		{"non-positive rate limit", func(c *Config) { c.Login.RateLimitPerMinute = -1 }},
		{"zero cleanup batch", func(c *Config) { c.Session.CleanupBatch = -5 }},
	}
	for _, tc := range cases {
		c := validConfig()
		if e := c.applyDefaults(); e != nil {
			t.Fatalf("%s: baseline must be valid: %v", tc.name, e)
		}
		tc.mutate(&c)
		if e := c.Validate(); e == nil {
			t.Errorf("%s: expected validation failure", tc.name)
		}
	}

	// The development provider alone is a complete login configuration, but only on a loopback
	// development origin; login.provider then stays empty instead of defaulting to GitHub.
	devOnly := validConfig()
	devOnly.Public = PublicConfig{BaseURL: "http://localhost:5173", Development: true}
	devOnly.Login = LoginConfig{PKCEKeyFile: "/run/pkce", DevelopmentProvider: true}
	devOnly.GitHub = GitHubConfig{}
	if e := devOnly.applyDefaults(); e != nil {
		t.Fatalf("development provider without an external provider must be valid on loopback: %v", e)
	}
	if devOnly.Login.Provider != "" || devOnly.Session.TTL != DefaultSessionLifetime {
		t.Fatalf("dev-only gateway must not invent an external provider: %+v", devOnly.Login)
	}
	// With an external provider configured alongside, that provider keeps its own rules.
	both := devOnly
	both.Login.Provider = ProviderHuaweiIDaaS
	both.Session.TTL = 0
	both.IDaaS = IDaaSConfig{BaseURL: "https://uniportal-beta.huawei.com", ClientID: "client", ClientSecretFile: "/run/idaas-secret"}
	if e := both.applyDefaults(); e != nil || both.Session.TTL != DefaultIDaaSSessionLifetime {
		t.Fatalf("IDaaS plus development provider must be valid with the IDaaS session default: %v %s", e, both.Session.TTL)
	}
}

func TestIDaaSConfigIsSelectedIndependentlyAndDefaultsToTwelveHours(t *testing.T) {
	cfg := validConfig()
	cfg.Login.Provider = ProviderHuaweiIDaaS
	cfg.Session.TTL = 0
	cfg.GitHub = GitHubConfig{}
	cfg.IDaaS = IDaaSConfig{BaseURL: "https://uniportal.huawei.com", ClientID: "client", ClientSecretFile: "/run/idaas-secret", DisplayNameField: "userName"}
	if e := cfg.applyDefaults(); e != nil {
		t.Fatal(e)
	}
	if cfg.Session.TTL != DefaultIDaaSSessionLifetime || cfg.IDaaS.Timeout != 10*time.Second {
		t.Fatalf("IDaaS defaults not applied: %+v", cfg)
	}
	for name, mutate := range map[string]func(*Config){
		"http origin":          func(c *Config) { c.IDaaS.BaseURL = "http://uniportal.huawei.com" },
		"origin with path":     func(c *Config) { c.IDaaS.BaseURL = "https://uniportal.huawei.com/oauth" },
		"missing client id":    func(c *Config) { c.IDaaS.ClientID = "" },
		"missing secret file":  func(c *Config) { c.IDaaS.ClientSecretFile = "" },
		"bad display field":    func(c *Config) { c.IDaaS.DisplayNameField = " userName " },
		"non-positive timeout": func(c *Config) { c.IDaaS.Timeout = -time.Second },
	} {
		copy := cfg
		mutate(&copy)
		if e := copy.Validate(); e == nil {
			t.Errorf("%s: expected validation failure", name)
		}
	}
}

func TestLoadConfigReadsFileAndEnvironmentOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	yaml := strings.TrimSpace(`
server: {port: 8081, mode: test, read_timeout: 5s, write_timeout: 5s}
logger: {level: info, filename: ` + filepath.ToSlash(filepath.Join(dir, "gw.log")) + `, max_size: 1, max_backups: 1, max_age: 1, compress: false, enable_console: false}
database: {driver: postgres, dsn: "host=127.0.0.1", max_idle_conns: 1, max_open_conns: 1, conn_max_lifetime: 1h}
public: {base_url: "https://app.example.com"}
session: {ttl: 240h}
login: {pkce_key_file: /run/pkce}
cloud: {upstream: "http://127.0.0.1:8080"}
tokens: {issuer: iss, audience: aud, service_subject: gw, service_key_id: s, user_key_id: u, service_private_key_file: /run/s, user_private_key_file: /run/u}
github: {client_id: id, client_secret_file: /run/secret}
`)
	if e := os.WriteFile(path, []byte(yaml), 0o600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("GATEWAY_SESSION_TTL", "48h")
	cfg, e := LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Session.TTL != 48*time.Hour {
		t.Fatalf("environment override not applied: %s", cfg.Session.TTL)
	}
	if cfg.Session.CleanupBatch != 500 || cfg.Cloud.Timeout != 30*time.Second {
		t.Fatalf("defaults missing after load: %+v", cfg)
	}
	t.Setenv("GATEWAY_SESSION_TTL", "2400h")
	if _, e = LoadConfig(path); e == nil {
		t.Fatal("session TTL above 90 days must fail at load")
	}

	// Keys the file omits are still overridable from the environment: `task setup` selects the
	// provider with GATEWAY_LOGIN_PROVIDER while the sample gateway.yaml leaves it unset.
	t.Setenv("GATEWAY_SESSION_TTL", "")
	t.Setenv("GATEWAY_LOGIN_PROVIDER", ProviderHuaweiIDaaS)
	t.Setenv("GATEWAY_IDAAS_BASE_URL", "https://uniportal-beta.huawei.com")
	t.Setenv("GATEWAY_IDAAS_CLIENT_ID", "app")
	t.Setenv("GATEWAY_IDAAS_CLIENT_SECRET_FILE", "/run/idaas-secret")
	t.Setenv("GATEWAY_IDAAS_AUTHORIZE_URL", "https://sso.example.org/custom/authorize")
	t.Setenv("GATEWAY_IDAAS_TOKEN_URL", "https://tokens.example.org/custom/token")
	t.Setenv("GATEWAY_IDAAS_USERINFO_URL", "https://profile.example.org/custom/userinfo")
	cfg, e = LoadConfig(path)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Login.Provider != ProviderHuaweiIDaaS || cfg.IDaaS.ClientID != "app" || cfg.Session.TTL != 240*time.Hour {
		t.Fatalf("environment must supply keys absent from the file: %+v %s", cfg.Login, cfg.Session.TTL)
	}
	if cfg.IDaaS.AuthorizeURL != "https://sso.example.org/custom/authorize" || cfg.IDaaS.TokenURL != "https://tokens.example.org/custom/token" || cfg.IDaaS.UserInfoURL != "https://profile.example.org/custom/userinfo" {
		t.Fatal("IDaaS endpoint environment overrides were not loaded")
	}
	t.Setenv("GATEWAY_IDAAS_TOKEN_URL", "https://tokens.example.org/token?secret=redacted")
	if _, e = LoadConfig(path); e == nil || strings.Contains(e.Error(), "redacted") {
		t.Fatal("invalid endpoint must fail without exposing the value")
	}
}
