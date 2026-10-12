package config

import (
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/cloneexec"
	"github.com/wanglongan587/cloud/internal/logger"
)

// CloneExecutorServiceConfig is the credentialed-clone executor's own configuration.
//
// It is loaded by `cmd/clone-executor` from its own file with its own `CLONE_EXECUTOR_` environment
// prefix, because the executor is a separate deployment unit with a separate identity, a separate
// volume, and a separate trust root. Nothing here is read by the Cloud server, and Cloud cannot
// inherit the executor's materialization root or helper path by configuring itself.
//
// The section is deliberately not optional. A deployment that runs this process at all has declared
// that it executes credentialed clones, and the decisions require such a deployment to establish the
// whole boundary or not start (D17): a partially configured executor would accept jobs and fail them
// one at a time, which reads as a flaky clone rather than as a misconfigured deployment.
type CloneExecutorServiceConfig struct {
	// Listen is the address the executor binds, for example `:8444`.
	Listen string `mapstructure:"listen"`
	// MaterialRoot is the directory credential material is staged under. The deployment creates it and
	// owns it as the credential-executing identity; the workload identity must not be able to traverse
	// it (D11).
	MaterialRoot string `mapstructure:"material_root"`
	// CheckoutRoot is where clones are written, and the handoff point to the workload. It must be a
	// different directory from MaterialRoot, and it is the one place the two identities are meant to
	// meet.
	CheckoutRoot string `mapstructure:"checkout_root"`
	// HomeDirectory is the executor's own empty directory, used as Git's HOME so Git finds no
	// configuration. The executor creates it if absent.
	HomeDirectory string `mapstructure:"home_directory"`
	// HelperPath is the absolute path of the deployment-fixed credential helper. It must be a program
	// the credential-executing identity can execute and the workload identity cannot write (D11).
	HelperPath string `mapstructure:"helper_path"`
	// GitPath is the absolute path of the Git binary the executor runs.
	GitPath string `mapstructure:"git_path"`
	// SSLCAInfo is the optional certificate authority file Git verifies the repository's TLS
	// certificate against. A deployment that serves Git from its own authority names it here; an empty
	// value means the system bundle.
	SSLCAInfo string `mapstructure:"ssl_ca_info"`
	// AuthorizationPublicKeyFile is the deployment-delivered Ed25519 public key Cloud signs
	// authorizations with. It is the public half only: an executor whose volume and process are both
	// compromised still cannot mint an authorization. The file may hold several keys, which is how the
	// deployment rotates Cloud's signing key without an outage.
	AuthorizationPublicKeyFile string `mapstructure:"authorization_public_key_file"`
	// CertificateFile, PrivateKeyFile and ClientCAFile are the executor's own server identity and the
	// authority the dispatcher's certificate must chain to.
	CertificateFile string `mapstructure:"certificate_file"`
	PrivateKeyFile  string `mapstructure:"private_key_file"`
	ClientCAFile    string `mapstructure:"client_ca_file"`
	// DispatcherIdentity is the single SPIFFE identity allowed to dispatch jobs. There is exactly one,
	// because the ability to dispatch is the ability to spend an authorization.
	DispatcherIdentity string `mapstructure:"dispatcher_identity"`
	// StoreEndpoint is the Credential Service's HTTPS base URL.
	StoreEndpoint string `mapstructure:"store_endpoint"`
	// StoreCertificateFile and StorePrivateKeyFile hold the executor's `broker-resolver` certificate.
	// It is the only identity the executor holds toward the Store, and the Store grants it `resolve`
	// alone.
	StoreCertificateFile string `mapstructure:"store_certificate_file"`
	StorePrivateKeyFile  string `mapstructure:"store_private_key_file"`
	// StoreCAFile is the authority that signs the Credential Service's server certificate.
	StoreCAFile string `mapstructure:"store_ca_file"`
	// Timeout bounds one clone. Zero selects the executor's default.
	Timeout time.Duration `mapstructure:"timeout"`
	// Logger is the executor's own log destination.
	Logger logger.Config `mapstructure:"logger"`
}

// LoadCloneExecutorConfig reads the executor's own configuration file.
//
// Like the Credential Service, the executor is a separate deployment unit and gets its own loader and
// its own environment prefix: `CLONE_EXECUTOR_*` cannot be set by anything that configures Cloud.
func LoadCloneExecutorConfig(path string) (*CloneExecutorServiceConfig, error) {
	v := viper.New()
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("clone-executor")
		v.SetConfigType("yaml")
		v.AddConfigPath("./configs")
		v.AddConfigPath("../configs")
		v.AddConfigPath(".")
	}
	v.SetEnvPrefix("CLONE_EXECUTOR")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := BindEnvKeys(v, CloneExecutorServiceConfig{}); err != nil {
		return nil, err
	}
	// A missing file is not an error: every value may come from the environment, which is how a
	// container deployment supplies the paths it mounted.
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && path != "" {
			return nil, fmt.Errorf("clone executor configuration is unreadable")
		}
	}
	var cfg CloneExecutorServiceConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("clone executor configuration is invalid: %w", err)
	}
	return &cfg, nil
}

// Executor builds the clone executor, including its redemption client toward the Credential Service.
//
// The order matters: the verification key and the Store client are both built here, so a deployment
// that mounted no verification key or cannot reach a well-formed Store endpoint refuses at startup
// rather than at the first job. That is the D17 rule — a deployment that declares credentialed clone
// establishes the boundary or does not start.
func (c *CloneExecutorServiceConfig) Executor() (*cloneexec.Executor, error) {
	if strings.TrimSpace(c.Listen) == "" {
		return nil, errors.New("clone executor listen address is required")
	}
	keys, err := cloneauth.LoadVerificationKeys(c.AuthorizationPublicKeyFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor: %w", err)
	}
	storeClient, err := c.StoreClient()
	if err != nil {
		return nil, err
	}
	executor, err := cloneexec.New(cloneexec.Config{
		MaterialRoot:     c.MaterialRoot,
		CheckoutRoot:     c.CheckoutRoot,
		HomeDirectory:    c.HomeDirectory,
		HelperPath:       c.HelperPath,
		GitPath:          c.GitPath,
		SSLCAInfo:        c.SSLCAInfo,
		VerificationKeys: keys,
		Store:            storeClient,
		Timeout:          c.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor: %w", err)
	}
	return executor, nil
}

// StoreClient builds the executor's `broker-resolver` client.
//
// It is a client certificate rather than a token, and the deployment's capability table gives that
// certificate `resolve` and nothing else. The executor therefore cannot write, rotate, revoke, or
// enumerate a credential even if it is fully compromised.
func (c *CloneExecutorServiceConfig) StoreClient() (*cloneexec.StoreClient, error) {
	cfg, _, err := loadMutualTLS(c.StoreCertificateFile, c.StorePrivateKeyFile, c.StoreCAFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor credential service: %w", err)
	}
	client, err := cloneexec.NewStoreClient(cloneexec.StoreClientConfig{
		Endpoint: c.StoreEndpoint,
		TLS:      cfg,
	})
	if err != nil {
		return nil, fmt.Errorf("clone executor credential service: %w", err)
	}
	return client, nil
}

// ServerTLS builds the executor's listener configuration. It always requires and verifies a client
// certificate: an anonymous caller must never reach the identity comparison at all.
func (c *CloneExecutorServiceConfig) ServerTLS() (*tls.Config, error) {
	cfg, clientCAs, err := loadMutualTLS(c.CertificateFile, c.PrivateKeyFile, c.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("clone executor: %w", err)
	}
	// The authority in `client_ca_file` verifies the executor's *peers*, which is `ClientCAs` rather
	// than `RootCAs`. The executor does make outbound connections, but they use the Store's authority
	// and are configured separately, so leaving this one unset would mean the listener accepted
	// anonymous callers.
	cfg.ClientCAs = clientCAs
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}

// CloneDispatchConfig is Cloud's half of the credentialed-clone boundary: the executor it dispatches
// jobs to, the identity it authenticates as, and the key it signs authorizations with.
//
// Disabling this section is the approved fail-closed state and the default. A deployment that
// configures no executor admits no credentialed clone at all: `requireRepositoryAccess` refuses every
// Project that names a credential with `repository_credential_executor_unavailable`, exactly as it did
// before this boundary existed. Enabling it is the deployment declaring that it has established the
// whole boundary, which is the shape D17 gives the missing-configuration table.
type CloneDispatchConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Endpoint is the executor's HTTPS base URL.
	Endpoint string `mapstructure:"endpoint"`
	// CertificateFile and PrivateKeyFile hold Cloud's `clone-dispatcher` certificate. They are file
	// references, never inline material.
	CertificateFile string `mapstructure:"certificate_file"`
	PrivateKeyFile  string `mapstructure:"private_key_file"`
	// CAFile is the certificate authority that signs the executor's server certificate.
	CAFile string `mapstructure:"ca_file"`
	// SigningKeyFile is Cloud's Ed25519 private key for clone authorizations. It is the ability to
	// authorize, and it belongs to Cloud alone: the Store and the executor hold only the public half,
	// so neither can mint one.
	SigningKeyFile string `mapstructure:"signing_key_file"`
	// AuthorizationLifetime is how long one authorization lives. It is bounded by the decision's hard
	// ceiling and must not exceed one clone's timeout, because an authorization that expires mid-clone
	// fails a clone that was otherwise permitted.
	AuthorizationLifetime time.Duration `mapstructure:"authorization_lifetime"`
	// Timeout bounds one dispatch. Zero selects the client's default.
	Timeout time.Duration `mapstructure:"timeout"`
}

// DefaultAuthorizationLifetime is the lifetime used when the deployment configures none. It is well
// under `cloneauth.MaxLifetime`, so the ceiling the decision sets is a backstop rather than the
// operating value.
const DefaultAuthorizationLifetime = 10 * time.Minute

// Open builds Cloud's dispatch client and signing key. A disabled section yields neither and no error
// — the capability is simply absent. An enabled but incomplete or unusable section is an error,
// because a deployment must refuse to start a capability it has declared but cannot establish.
func (c *CloneDispatchConfig) Open() (*cloneexec.Client, ed25519.PrivateKey, time.Duration, error) {
	if !c.Enabled {
		return nil, nil, 0, nil
	}
	if strings.TrimSpace(c.SigningKeyFile) == "" {
		return nil, nil, 0, errors.New("clone dispatch: an authorization signing key is required")
	}
	key, err := cloneauth.LoadSigningKey(c.SigningKeyFile)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("clone dispatch: %w", err)
	}
	lifetime := c.AuthorizationLifetime
	if lifetime <= 0 {
		lifetime = DefaultAuthorizationLifetime
	}
	if lifetime > cloneauth.MaxLifetime {
		return nil, nil, 0, fmt.Errorf("clone dispatch: authorization lifetime exceeds %s", cloneauth.MaxLifetime)
	}
	cfg, _, err := loadMutualTLS(c.CertificateFile, c.PrivateKeyFile, c.CAFile)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("clone dispatch: %w", err)
	}
	client, err := cloneexec.NewClient(cloneexec.ClientConfig{
		Endpoint: c.Endpoint,
		TLS:      cfg,
		Timeout:  c.Timeout,
	})
	if err != nil {
		return nil, nil, 0, fmt.Errorf("clone dispatch: %w", err)
	}
	return client, key, lifetime, nil
}
