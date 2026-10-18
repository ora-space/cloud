package gitserver

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
)

// Config contains only deployment references. No repository, ref or sandbox permission is
// configured here: every permission comes from Cloud state at request time.
type Config struct {
	// RepositoryAddress serves smart HTTP to sandboxes and joins only the sandbox network.
	RepositoryAddress string
	// ServiceRepositoryAddress serves smart HTTP to the Repo Executor and joins only the internal
	// service network, which sandboxes cannot reach.
	ServiceRepositoryAddress string
	// RefAddress serves the read-only ref query to Cloud and joins only the default network.
	RefAddress string
	// RepositoryRoot is the persistent volume holding /{tenantId}/{projectId}.git bare repositories.
	RepositoryRoot string
	// GitProgram is the git executable that runs http-backend and repository maintenance.
	GitProgram string
	// HealthServerName is the TLS name the local healthcheck verifies on the ref listener.
	HealthServerName                                                    string
	CertificateFile, PrivateKeyFile, SandboxCAFile, CloudClientCertFile string
	// ExecutorCAFile is the CA that issues Repo Executor service certificates. It is distinct from
	// the sandbox CA, so neither listener accepts the other's clients.
	ExecutorCAFile string
}

// LoadConfig reads the git-server process's environment with explicit safe defaults.
func LoadConfig() (Config, error) {
	value := func(key, fallback string) string {
		if v := os.Getenv("GIT_SERVER_" + key); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		RepositoryAddress:        value("REPOSITORY_ADDR", ":8445"),
		ServiceRepositoryAddress: value("SERVICE_REPOSITORY_ADDR", ":8447"),
		ExecutorCAFile:           value("EXECUTOR_CA_FILE", "/etc/ora-git-management/repo-executor-ca.pem"),
		RefAddress:               value("REF_ADDR", ":8446"),
		RepositoryRoot:           value("REPOSITORY_ROOT", "/var/lib/ora-git/repositories"),
		GitProgram:               value("GIT_PROGRAM", "git"),
		HealthServerName:         value("HEALTH_SERVER_NAME", "git-server"),
		CertificateFile:          value("CERTIFICATE_FILE", "/etc/ora-git-management/server.pem"),
		PrivateKeyFile:           value("PRIVATE_KEY_FILE", "/etc/ora-git-management/server.key"),
		SandboxCAFile:            value("SANDBOX_CA_FILE", "/etc/ora-git-management/ca.pem"),
		CloudClientCertFile:      value("CLOUD_CLIENT_CERT_FILE", "/etc/ora-git-management/cloud-client.pem"),
	}
	for _, address := range []string{c.RepositoryAddress, c.ServiceRepositoryAddress, c.RefAddress} {
		if _, _, err := net.SplitHostPort(address); err != nil {
			return Config{}, errors.New("invalid git server listen address")
		}
	}
	if !filepath.IsAbs(c.RepositoryRoot) {
		return Config{}, errors.New("git server repository root must be absolute")
	}
	return c, nil
}

// RepositoryTLS requires a client certificate chained to the sandbox CA. RuntimeScope then
// enforces the git-access purpose, so WSS and model-access certificates from the same CA fail.
func (c *Config) RepositoryTLS() (*tls.Config, error) {
	config, err := c.serverTLS()
	if err != nil {
		return nil, err
	}
	pool, err := c.sandboxTrust()
	if err != nil {
		return nil, err
	}
	config.ClientAuth = tls.RequireAndVerifyClientCert
	config.ClientCAs = pool
	return config, nil
}

// ServiceRepositoryTLS requires a client certificate chained to the executor CA only.
// RequireExecutor then enforces the executor identity.
func (c *Config) ServiceRepositoryTLS() (*tls.Config, error) {
	config, err := c.serverTLS()
	if err != nil {
		return nil, err
	}
	pool, err := trust(c.ExecutorCAFile)
	if err != nil {
		return nil, err
	}
	config.ClientAuth = tls.RequireAndVerifyClientCert
	config.ClientCAs = pool
	return config, nil
}

// RefTLS verifies a client certificate when one is presented; the ref handler additionally pins
// Cloud's exact leaf, and only the unauthenticated health route accepts a certificate-less peer.
func (c *Config) RefTLS() (*tls.Config, error) {
	config, err := c.serverTLS()
	if err != nil {
		return nil, err
	}
	pool, err := c.sandboxTrust()
	if err != nil {
		return nil, err
	}
	config.ClientAuth = tls.VerifyClientCertIfGiven
	config.ClientCAs = pool
	return config, nil
}

// CloudClient returns the DER of the one leaf certificate allowed to query refs.
func (c *Config) CloudClient() ([]byte, error) {
	data, err := os.ReadFile(c.CloudClientCertFile)
	if err != nil {
		return nil, errors.New("git server Cloud client identity unavailable")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("git server Cloud client identity must be exactly one certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return nil, errors.New("invalid git server Cloud client identity")
	}
	return block.Bytes, nil
}

func (c *Config) serverTLS() (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.CertificateFile, c.PrivateKeyFile)
	if err != nil {
		return nil, errors.New("git server TLS identity unavailable")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, nil
}

func (c *Config) sandboxTrust() (*x509.CertPool, error) { return trust(c.SandboxCAFile) }

func trust(file string) (*x509.CertPool, error) {
	trust, err := os.ReadFile(file)
	if err != nil {
		return nil, errors.New("git server trust unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return nil, errors.New("invalid git server trust")
	}
	return pool, nil
}
