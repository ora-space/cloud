package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/logger"
)

// CredentialIngressConfig is Cloud's half of the Credential Service boundary: the endpoint Cloud
// hands user tokens to and the `ingress-writer` identity it authenticates as.
//
// Disabling this optional section leaves the credential API refusing every write with
// `credential_store_unavailable`, which is the approved fail-closed state: no credential may exist
// without a store that can hold it, and no raw token may ever be written to Cloud's own database as
// a substitute.
type CredentialIngressConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Endpoint is the Credential Service's HTTPS base URL.
	Endpoint string `mapstructure:"endpoint"`
	// CertificateFile and PrivateKeyFile hold Cloud's `ingress-writer` certificate. They are file
	// references, never inline material: cloud/AGENTS.md forbids deployment secrets in config files.
	CertificateFile string `mapstructure:"certificate_file"`
	PrivateKeyFile  string `mapstructure:"private_key_file"`
	// CAFile is the certificate authority that signs the Credential Service's server certificate.
	CAFile string `mapstructure:"ca_file"`
	// Timeout bounds one handoff.
	Timeout time.Duration `mapstructure:"timeout"`
}

// Open builds the ingress client. A disabled section yields no client and no error — the capability
// is simply absent. An enabled but incomplete or unusable section is an error, because D6 requires
// Cloud to refuse to start a configured capability it cannot establish rather than fail per request
// at runtime.
func (c *CredentialIngressConfig) Open() (*credentialstore.Client, error) {
	if !c.Enabled {
		return nil, nil
	}
	cfg, _, err := loadMutualTLS(c.CertificateFile, c.PrivateKeyFile, c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	client, err := credentialstore.NewClient(credentialstore.ClientConfig{
		Endpoint: c.Endpoint,
		TLS:      cfg,
		Timeout:  c.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	return client, nil
}

// CredentialStoreServiceConfig is the Credential Service process's own configuration.
//
// It is loaded by `cmd/credential-store` from its own file, with its own `CREDENTIAL_STORE_`
// environment prefix, because the Service is a separate deployment unit with a separate volume and
// a separate trust root (D1). Nothing here is read by the Cloud server.
type CredentialStoreServiceConfig struct {
	// Listen is the address the Service binds, for example `:8443`.
	Listen string `mapstructure:"listen"`
	// Volume is the Service's own private directory. It must not be Cloud's database volume or any
	// path shared with another service (D1).
	Volume string `mapstructure:"volume"`
	// KEKFile is the deployment-delivered keyring. The Service never writes it and refuses to start
	// without it (D2, D8).
	KEKFile string `mapstructure:"kek_file"`
	// CertificateFile, PrivateKeyFile and ClientCAFile are the Service's server identity and the
	// authority every client certificate must chain to.
	CertificateFile string `mapstructure:"certificate_file"`
	PrivateKeyFile  string `mapstructure:"private_key_file"`
	ClientCAFile    string `mapstructure:"client_ca_file"`
	// IngressWriterIdentity, BrokerResolverIdentity and KEKOperatorIdentity are the three fixed
	// SPIFFE identities, each holding exactly one capability (D3). All three are required.
	IngressWriterIdentity  string `mapstructure:"ingress_writer_identity"`
	BrokerResolverIdentity string `mapstructure:"broker_resolver_identity"`
	KEKOperatorIdentity    string `mapstructure:"kek_operator_identity"`
	// SupersededRetention is how long a rotated-away version stays readable. Zero selects the
	// approved default.
	SupersededRetention time.Duration `mapstructure:"superseded_retention"`
	// Logger is the service's own log destination. D7 makes every operation an audit record, so the
	// command refuses to start with no destination rather than discarding them silently.
	Logger logger.Config `mapstructure:"logger"`
}

// LoadCredentialStoreConfig reads the Credential Service's own configuration file.
//
// The Service is a separate deployment unit, so it gets its own loader and its own environment
// prefix rather than a section of the Cloud server's file: `CREDENTIAL_STORE_*` cannot be set by
// anything that configures Cloud, and Cloud cannot accidentally inherit the Service's listen address
// or volume. Environment binding is explicit, matching the rest of the repository, so a key absent
// from the file still resolves.
func LoadCredentialStoreConfig(path string) (*CredentialStoreServiceConfig, error) {
	v := viper.New()
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("credential-store")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("../configs")
		v.AddConfigPath(".")
	}
	v.SetEnvPrefix("CREDENTIAL_STORE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := BindEnvKeys(v, CredentialStoreServiceConfig{}); err != nil {
		return nil, err
	}
	// A missing file is not an error: every value may come from the environment, which is how a
	// container deployment supplies the paths it mounted.
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && path != "" {
			return nil, fmt.Errorf("credential store configuration is unreadable")
		}
	}
	var cfg CredentialStoreServiceConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("credential store configuration is invalid: %w", err)
	}
	return &cfg, nil
}

// Store prepares the Service's volume and key material. It is the startup gate D6 requires, and it
// deliberately runs before the listener is bound so a deployment with no KEK never reaches the point
// of accepting a write.
func (c *CredentialStoreServiceConfig) Store() (*credentialstore.Store, error) {
	if strings.TrimSpace(c.Listen) == "" {
		return nil, errors.New("credential store listen address is required")
	}
	if strings.TrimSpace(c.Volume) == "" {
		return nil, errors.New("credential store volume is required")
	}
	store, err := credentialstore.Open(credentialstore.Config{
		Volume:    c.Volume,
		KeyFile:   c.KEKFile,
		Retention: c.SupersededRetention,
	})
	if err != nil {
		return nil, fmt.Errorf("credential store: %w", err)
	}
	return store, nil
}

// ServerTLS builds the Service's listener configuration. It always requires and verifies a client
// certificate: an anonymous caller must never reach a capability check at all (D3).
func (c *CredentialStoreServiceConfig) ServerTLS() (*tls.Config, error) {
	cfg, clientCAs, err := loadMutualTLS(c.CertificateFile, c.PrivateKeyFile, c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("credential store: %w", err)
	}
	// The authority in `client_ca_file` is what the listener verifies its *peers* against, which is
	// `ClientCAs` rather than `RootCAs`: this service makes no outbound connection, so the server
	// configuration must not leave the client authority unset. The Store's own constructor refuses a
	// configuration without one, because a listener that verified no client certificate would accept
	// anonymous callers (D3).
	cfg.ClientCAs = clientCAs
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// Identities returns the deployment's three fixed service identities.
func (c *CredentialStoreServiceConfig) Identities() credentialstore.IdentityConfig {
	return credentialstore.IdentityConfig{
		IngressWriter:  c.IngressWriterIdentity,
		BrokerResolver: c.BrokerResolverIdentity,
		KEKOperator:    c.KEKOperatorIdentity,
	}
}

// loadMutualTLS reads one certificate pair and one trust pool from mounted files.
//
// Both sides of the boundary share this helper because both need the same three properties: the key
// material comes from files rather than from config values, the connection requires TLS 1.3, and the
// trust pool is explicit rather than the host's. The pool is returned alongside the configuration
// because the two roles use it differently — a client verifies its peer with `RootCAs`, a listener
// verifies its peers with `ClientCAs` — and a caller that took the wrong one would either trust
// nothing or accept anyone. Errors name which file was unusable without echoing any of its contents.
func loadMutualTLS(certificateFile, privateKeyFile, caFile string) (*tls.Config, *x509.CertPool, error) {
	if strings.TrimSpace(certificateFile) == "" || strings.TrimSpace(privateKeyFile) == "" || strings.TrimSpace(caFile) == "" {
		return nil, nil, errors.New("certificate, private key and certificate authority files are all required")
	}
	pair, err := tls.LoadX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return nil, nil, errors.New("cannot load the certificate and private key")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, nil, errors.New("cannot read the certificate authority file")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, nil, errors.New("the certificate authority file holds no usable certificate")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool}, pool, nil
}
