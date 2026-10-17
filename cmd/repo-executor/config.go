package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// config is the executor's deployment configuration, read from REPO_EXECUTOR_* variables. It holds
// file references and limits only; no credential is ever configured here.
type config struct {
	Addr                                  string
	CertFile, KeyFile, ClientCAFile       string
	AuthorizationPublicKeysFile           string
	GitServerURL, GitServerCAFile         string
	CredentialStoreURL, CredentialStoreCA string
	ExternalCAFile                        string
	WorkRoot, HelperPath, GitProgram      string
	Slots                                 int
	MaxTaskBytes                          int64
	SyncTimeoutMax, PublishTimeoutMax     time.Duration
	ProbeTimeoutMax                       time.Duration
	EgressAllow, EgressDenyHosts          []string
	LogLevel                              string
}

// loadConfig reads the environment through lookup with the documented defaults.
func loadConfig(lookup func(string) string) (config, error) {
	value := func(key, fallback string) string {
		if v := strings.TrimSpace(lookup("REPO_EXECUTOR_" + key)); v != "" {
			return v
		}
		return fallback
	}
	c := config{
		Addr:                        value("ADDR", ":8471"),
		CertFile:                    value("CERT_FILE", "/etc/repo-executor/tls/executor.pem"),
		KeyFile:                     value("KEY_FILE", "/etc/repo-executor/tls/executor.key"),
		ClientCAFile:                value("CLIENT_CA_FILE", "/etc/repo-executor/tls/dispatcher-ca.pem"),
		AuthorizationPublicKeysFile: value("AUTHORIZATION_PUBLIC_KEYS_FILE", "/etc/repo-executor/authorization-public-keys.pem"),
		GitServerURL:                value("GIT_SERVER_URL", "https://ora-git-service:8447"),
		GitServerCAFile:             value("GIT_SERVER_CA_FILE", "/etc/repo-executor/tls/git-server-ca.pem"),
		CredentialStoreURL:          value("CREDENTIAL_STORE_URL", "https://credential-store:8461"),
		CredentialStoreCA:           value("CREDENTIAL_STORE_CA_FILE", "/etc/repo-executor/tls/credential-store-ca.pem"),
		ExternalCAFile:              value("EXTERNAL_CA_FILE", ""),
		WorkRoot:                    value("WORK_ROOT", "/var/lib/repo-executor/tasks"),
		HelperPath:                  value("HELPER_PATH", "/usr/local/bin/git-credential-ora"),
		GitProgram:                  value("GIT_PROGRAM", "git"),
		EgressAllow:                 list(value("EGRESS_ALLOW", "")),
		EgressDenyHosts:             list(value("EGRESS_DENY_HOSTS", "")),
		LogLevel:                    value("LOG_LEVEL", "info"),
	}
	var err error
	if c.Slots, err = strconv.Atoi(value("SLOTS", "4")); err != nil || c.Slots <= 0 {
		return config{}, errors.New("REPO_EXECUTOR_SLOTS must be a positive integer")
	}
	if c.MaxTaskBytes, err = strconv.ParseInt(value("MAX_TASK_BYTES", strconv.FormatInt(2<<30, 10)), 10, 64); err != nil || c.MaxTaskBytes <= 0 {
		return config{}, errors.New("REPO_EXECUTOR_MAX_TASK_BYTES must be a positive integer")
	}
	for _, d := range []struct {
		key, fallback string
		target        *time.Duration
	}{
		{"SYNC_TIMEOUT_MAX", "30m", &c.SyncTimeoutMax},
		{"PUBLISH_TIMEOUT_MAX", "5m", &c.PublishTimeoutMax},
		{"PROBE_TIMEOUT_MAX", value("PUBLISH_TIMEOUT_MAX", "5m"), &c.ProbeTimeoutMax},
	} {
		if *d.target, err = time.ParseDuration(value(d.key, d.fallback)); err != nil || *d.target <= 0 {
			return config{}, fmt.Errorf("REPO_EXECUTOR_%s must be a positive duration", d.key)
		}
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		return config{}, errors.New("REPO_EXECUTOR_ADDR is not a listen address")
	}
	for _, u := range []struct{ key, value string }{{"GIT_SERVER_URL", c.GitServerURL}, {"CREDENTIAL_STORE_URL", c.CredentialStoreURL}} {
		parsed, err := url.Parse(u.value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return config{}, fmt.Errorf("REPO_EXECUTOR_%s must be an https URL", u.key)
		}
	}
	if !filepath.IsAbs(c.WorkRoot) {
		return config{}, errors.New("REPO_EXECUTOR_WORK_ROOT must be absolute")
	}
	return c, nil
}

// internalHosts are the executor's own fixed internal peers. They are always denied as external
// repositories, whatever the deployment lists, so a Binding can never name them.
func (c *config) internalHosts() []string {
	hosts := append([]string(nil), c.EgressDenyHosts...)
	for _, raw := range []string{c.GitServerURL, c.CredentialStoreURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

func list(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
