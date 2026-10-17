package config

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/credentialstore"
	"github.com/wanglongan587/cloud/internal/gitrefs"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

// RepositoryConfig wires the external Git line into Cloud: the Repo Executor Cloud dispatches to,
// the Credential Store's management interface and browser origin, the Git Server ref query, and
// the two distinct Ed25519 keys Cloud signs with (execution authorizations, submission grants).
// Every value is a file or address reference; no key or certificate content lives here.
// Leaf keys are overridable as CLOUD_REPOSITORY_*.
type RepositoryConfig struct {
	// Enabled turns on the repository worker and the credential routes. Disabled, Projects can be
	// created but no sync runs, and credential creation answers 503.
	Enabled bool `mapstructure:"enabled"`

	ExecutorURL        string `mapstructure:"executor_url"`
	ExecutorCAFile     string `mapstructure:"executor_ca_file"`
	DispatcherCertFile string `mapstructure:"dispatcher_cert_file"`
	DispatcherKeyFile  string `mapstructure:"dispatcher_key_file"`
	// AuthorizationKeyFile is the PKCS#8 Ed25519 key signing execution authorizations.
	AuthorizationKeyFile string `mapstructure:"authorization_key_file"`

	CredentialStoreURL    string `mapstructure:"credential_store_url"`
	CredentialStoreCAFile string `mapstructure:"credential_store_ca_file"`
	ManagerCertFile       string `mapstructure:"manager_cert_file"`
	ManagerKeyFile        string `mapstructure:"manager_key_file"`
	// SubmissionKeyFile is the PKCS#8 Ed25519 key signing browser submission grants.
	SubmissionKeyFile string `mapstructure:"submission_key_file"`
	// CredentialIngressURL is the Credential Store's browser origin returned with each grant.
	CredentialIngressURL string `mapstructure:"credential_ingress_url"`

	GitRefsURL      string `mapstructure:"git_refs_url"`
	GitRefsCAFile   string `mapstructure:"git_refs_ca_file"`
	GitRefsCertFile string `mapstructure:"git_refs_cert_file"`
	GitRefsKeyFile  string `mapstructure:"git_refs_key_file"`

	Slots          int           `mapstructure:"slots"`
	Lease          time.Duration `mapstructure:"lease"`
	SyncTimeout    time.Duration `mapstructure:"sync_timeout"`
	PublishTimeout time.Duration `mapstructure:"publish_timeout"`
}

// Repository is the opened external Git line: ready clients and keys.
type Repository struct {
	Executor        *repoexec.Client
	CredentialStore *credentialstore.ManagementClient
	Refs            *gitrefs.Client
	AuthorizationKey,
	SubmissionKey ed25519.PrivateKey
	IngressURL string
}

// Open loads keys and certificates and builds the clients. It performs no network calls.
func (c *RepositoryConfig) Open() (*Repository, error) {
	if !c.Enabled {
		return nil, nil
	}
	authorization, err := cloneauth.LoadPrivateKey(c.AuthorizationKeyFile)
	if err != nil {
		return nil, fmt.Errorf("repository.authorization_key_file: %w", err)
	}
	submission, err := cloneauth.LoadPrivateKey(c.SubmissionKeyFile)
	if err != nil {
		return nil, fmt.Errorf("repository.submission_key_file: %w", err)
	}
	// The two keys must differ, or a submission grant's signature would mean nothing new.
	if authorization.Equal(submission) {
		return nil, errors.New("repository authorization and submission keys must differ")
	}
	dispatcher, err := mutualTLS(c.DispatcherCertFile, c.DispatcherKeyFile, c.ExecutorCAFile)
	if err != nil {
		return nil, fmt.Errorf("repository executor TLS: %w", err)
	}
	executor, err := repoexec.NewClient(&repoexec.ClientConfig{Endpoint: c.ExecutorURL, TLS: dispatcher})
	if err != nil {
		return nil, err
	}
	manager, err := mutualTLS(c.ManagerCertFile, c.ManagerKeyFile, c.CredentialStoreCAFile)
	if err != nil {
		return nil, fmt.Errorf("repository credential store TLS: %w", err)
	}
	store, err := credentialstore.NewManagementClient(&credentialstore.ClientConfig{BaseURL: c.CredentialStoreURL, TLS: manager, Timeout: 10 * time.Second})
	if err != nil {
		return nil, err
	}
	refs, err := gitrefs.New(gitrefs.Config{BaseURL: c.GitRefsURL, CAFile: c.GitRefsCAFile, CertFile: c.GitRefsCertFile, KeyFile: c.GitRefsKeyFile})
	if err != nil {
		return nil, err
	}
	if c.CredentialIngressURL == "" {
		return nil, errors.New("repository.credential_ingress_url is required")
	}
	return &Repository{Executor: executor, CredentialStore: store, Refs: refs, AuthorizationKey: authorization, SubmissionKey: submission, IngressURL: c.CredentialIngressURL}, nil
}

func mutualTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, errors.New("client identity unavailable")
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("trust unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("invalid trust")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool}, nil
}
