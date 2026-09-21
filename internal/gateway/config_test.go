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
		{"unknown provider", func(c *Config) { c.Login.Provider = "ldap" }},
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
}
