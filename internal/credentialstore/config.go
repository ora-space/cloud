package credentialstore

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// envPrefix prefixes every environment variable the command reads.
const envPrefix = "CREDENTIAL_STORE_"

// Config is the command's deployment configuration, read from CREDENTIAL_STORE_* variables. It
// holds file references and addresses only, never key or token material.
type Config struct {
	// Root is the Store volume (ROOT).
	Root string
	// KeyringFile is the read-only KEK keyring mount (KEYRING_FILE).
	KeyringFile string
	// CertFile and KeyFile are the service certificate the three mTLS listeners present.
	CertFile, KeyFile string
	// ClientCAFile is the CA that issues executor, credential-manager and KEK-operator client
	// certificates (CLIENT_CA_FILE).
	ClientCAFile string
	// ExecutionPublicKeysFile and SubmissionPublicKeysFile are Cloud's two distinct verification
	// key sets (PEM PUBLIC KEY blocks).
	ExecutionPublicKeysFile, SubmissionPublicKeysFile string
	// RedemptionAddr, ManagementAddr, KeyAddr and IngressAddr are the four listener addresses.
	RedemptionAddr, ManagementAddr, KeyAddr, IngressAddr string
	// IngressCertFile and IngressKeyFile are the public certificate of the browser origin.
	IngressCertFile, IngressKeyFile string
	// IngressAllowedOrigins are the exact frontend origins CORS admits.
	IngressAllowedOrigins []string
	// IngressRatePerMinute and IngressRateBurst bound submissions per remote IP.
	IngressRatePerMinute, IngressRateBurst int
	// LogLevel is the zap level name (LOG_LEVEL), default info.
	LogLevel string
	// Identities are the client identities the listeners trust. They are the fixed SPIFFE names of
	// the deployed topology; Validate still refuses any overlap.
	Identities Identities
}

// LoadConfig reads CREDENTIAL_STORE_* through lookup (os.Getenv in the command) and validates the
// result. It refuses missing file references, non-absolute paths, duplicate or malformed
// addresses, invalid origins and overlapping identities, so a misconfigured deployment fails at
// startup instead of serving a weaker boundary.
func LoadConfig(lookup func(string) string) (Config, error) {
	value := func(key, fallback string) string {
		if v := strings.TrimSpace(lookup(envPrefix + key)); v != "" {
			return v
		}
		return fallback
	}
	cfg := Config{
		Root:                     value("ROOT", ""),
		KeyringFile:              value("KEYRING_FILE", ""),
		CertFile:                 value("CERT_FILE", ""),
		KeyFile:                  value("KEY_FILE", ""),
		ClientCAFile:             value("CLIENT_CA_FILE", ""),
		ExecutionPublicKeysFile:  value("EXECUTION_PUBLIC_KEYS_FILE", ""),
		SubmissionPublicKeysFile: value("SUBMISSION_PUBLIC_KEYS_FILE", ""),
		RedemptionAddr:           value("REDEMPTION_ADDR", ":8461"),
		ManagementAddr:           value("MANAGEMENT_ADDR", ":8462"),
		KeyAddr:                  value("KEY_ADDR", ":8463"),
		IngressAddr:              value("INGRESS_ADDR", ":8460"),
		IngressCertFile:          value("INGRESS_CERT_FILE", ""),
		IngressKeyFile:           value("INGRESS_KEY_FILE", ""),
		LogLevel:                 value("LOG_LEVEL", "info"),
		Identities:               DefaultIdentities(),
	}
	for _, origin := range strings.Split(value("INGRESS_ALLOWED_ORIGINS", ""), ",") {
		if origin = strings.TrimSpace(origin); origin != "" {
			cfg.IngressAllowedOrigins = append(cfg.IngressAllowedOrigins, origin)
		}
	}
	var err error
	if cfg.IngressRatePerMinute, err = positive(value("INGRESS_RATE_PER_MINUTE", strconv.Itoa(DefaultRatePerMinute))); err != nil {
		return Config{}, fmt.Errorf("%sINGRESS_RATE_PER_MINUTE %w", envPrefix, err)
	}
	if cfg.IngressRateBurst, err = positive(value("INGRESS_RATE_BURST", strconv.Itoa(DefaultRateBurst))); err != nil {
		return Config{}, fmt.Errorf("%sINGRESS_RATE_BURST %w", envPrefix, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func positive(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("must be a positive integer")
	}
	return n, nil
}

// Validate checks the configuration without touching the filesystem.
func (c *Config) Validate() error {
	files := []struct{ key, path string }{
		{"ROOT", c.Root},
		{"KEYRING_FILE", c.KeyringFile},
		{"CERT_FILE", c.CertFile},
		{"KEY_FILE", c.KeyFile},
		{"CLIENT_CA_FILE", c.ClientCAFile},
		{"EXECUTION_PUBLIC_KEYS_FILE", c.ExecutionPublicKeysFile},
		{"SUBMISSION_PUBLIC_KEYS_FILE", c.SubmissionPublicKeysFile},
		{"INGRESS_CERT_FILE", c.IngressCertFile},
		{"INGRESS_KEY_FILE", c.IngressKeyFile},
	}
	for _, f := range files {
		if f.path == "" || !filepath.IsAbs(f.path) {
			return fmt.Errorf("%s%s must be an absolute path", envPrefix, f.key)
		}
	}
	if filepath.Clean(c.ExecutionPublicKeysFile) == filepath.Clean(c.SubmissionPublicKeysFile) {
		return errors.New("execution and submission public keys must be distinct files")
	}
	seen := make(map[string]string, 4)
	for _, a := range []struct{ key, addr string }{
		{"REDEMPTION_ADDR", c.RedemptionAddr},
		{"MANAGEMENT_ADDR", c.ManagementAddr},
		{"KEY_ADDR", c.KeyAddr},
		{"INGRESS_ADDR", c.IngressAddr},
	} {
		if _, _, err := net.SplitHostPort(a.addr); err != nil {
			return fmt.Errorf("%s%s is not a host:port address", envPrefix, a.key)
		}
		if other, dup := seen[a.addr]; dup {
			return fmt.Errorf("%s%s and %s%s must be distinct listeners", envPrefix, other, envPrefix, a.key)
		}
		seen[a.addr] = a.key
	}
	if _, err := parseOrigins(c.IngressAllowedOrigins); err != nil {
		return fmt.Errorf("%sINGRESS_ALLOWED_ORIGINS: %w", envPrefix, err)
	}
	if c.IngressRatePerMinute <= 0 || c.IngressRateBurst <= 0 {
		return errors.New("ingress rate limit must be positive")
	}
	return c.Identities.Validate()
}

// ServiceTLS returns the TLS 1.3 configuration of the three mTLS listeners: the service
// certificate, and a required client certificate chained to the client CA. Each handler then
// admits exactly one identity.
func (c *Config) ServiceTLS() (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, errors.New("credential store service certificate is unavailable")
	}
	raw, err := os.ReadFile(c.ClientCAFile)
	if err != nil {
		return nil, errors.New("credential store client CA is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(raw) {
		return nil, errors.New("credential store client CA holds no certificate")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

// IngressTLS returns the TLS 1.3 configuration of the browser ingress, which requests no client
// certificate: the submission grant is the only authentication there.
func (c *Config) IngressTLS() (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.IngressCertFile, c.IngressKeyFile)
	if err != nil {
		return nil, errors.New("credential store ingress certificate is unavailable")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, nil
}
