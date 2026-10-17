package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/gitrefquery"
)

// DefaultGitRefQueryTimeout bounds one ref query when git_server.ref_query_timeout is unset.
const DefaultGitRefQueryTimeout = 10 * time.Second

// GitServerConfig is how Cloud uses the deployment's Git Server (Revision D2): the repository base
// URL sandboxes reach, written into clone, session and delivery inputs, and the ref query listener
// Cloud verifies refs through with its own pinned client identity. The private key stays a file
// reference; it never enters a configuration value, a log or the database. Leaving
// repository_base_url empty runs Cloud without a Git Server: clone steps block as
// git_server_unconfigured and deliveries are skipped. CLOUD_GIT_SERVER_* overrides each key.
type GitServerConfig struct {
	RepositoryBaseURL string        `mapstructure:"repository_base_url"`
	RefQueryURL       string        `mapstructure:"ref_query_url"`
	CAFile            string        `mapstructure:"ca_file"`
	ClientCertFile    string        `mapstructure:"client_cert_file"`
	ClientKeyFile     string        `mapstructure:"client_key_file"`
	RefQueryTimeout   time.Duration `mapstructure:"ref_query_timeout"`
}

// Open validates the section and loads its files. It performs no network call: a Git Server that is
// unreachable or presents another certificate makes verification unavailable, never startup fail.
func (c *GitServerConfig) Open() (*core.GitServer, error) {
	if c.RepositoryBaseURL == "" {
		return nil, nil
	}
	if !core.ValidGitRepositoryBaseURL(c.RepositoryBaseURL) {
		return nil, fmt.Errorf("git_server.repository_base_url must be https without userinfo, query, fragment or trailing slash")
	}
	if c.RefQueryURL == "" || c.CAFile == "" || c.ClientCertFile == "" || c.ClientKeyFile == "" {
		return nil, fmt.Errorf("git_server.ref_query_url, ca_file, client_cert_file and client_key_file are required with repository_base_url")
	}
	timeout := c.RefQueryTimeout
	if timeout <= 0 {
		timeout = DefaultGitRefQueryTimeout
	}
	pair, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load git_server client certificate: %w", err)
	}
	pem, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load git_server CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("invalid git_server CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}}
	// Redirects are never followed: the pinned listener answers itself or not at all.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	refs, err := gitrefquery.New(c.RefQueryURL, client, timeout)
	if err != nil {
		return nil, fmt.Errorf("git_server.ref_query_url: %w", err)
	}
	return &core.GitServer{RepositoryBaseURL: c.RepositoryBaseURL, Refs: refs}, nil
}
