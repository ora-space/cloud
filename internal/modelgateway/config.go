package modelgateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"strings"
)

// Config contains only deployment references; original model keys are received at runtime.
type Config struct {
	CredentialAddress, RuntimeAddress, GrantAddress, PublicOrigin       string
	CertificateFile, PrivateKeyFile, CAFile, MasterKeyFile, MasterKeyID string
	DevelopmentHosts                                                    []string
	DNS                                                                 DNSConfig
}

// LoadConfig reads the model-gateway process's environment with explicit safe defaults.
func LoadConfig() (Config, error) {
	value := func(key, fallback string) string {
		v := os.Getenv("MODEL_GATEWAY_" + key)
		if v == "" {
			return fallback
		}
		return v
	}
	c := Config{CredentialAddress: value("CREDENTIAL_ADDR", ":8083"), RuntimeAddress: value("RUNTIME_ADDR", ":8443"), GrantAddress: value("GRANT_ADDR", ":8444"), PublicOrigin: value("PUBLIC_ORIGIN", "https://ora-model-gateway:8443"), CertificateFile: value("CERTIFICATE_FILE", "/etc/ora-model-management/server-p256.pem"), PrivateKeyFile: value("PRIVATE_KEY_FILE", "/etc/ora-model-management/server-p256.key"), CAFile: value("CA_FILE", "/etc/ora-model-management/ca.pem"), MasterKeyFile: value("MASTER_KEY_FILE", "/etc/ora-model-secrets/master-key"), MasterKeyID: value("MASTER_KEY_ID", "v1")}
	c.DNS = DNSConfig{HTTPSURL: value("DNS_HTTPS_URL", "https://cloudflare-dns.com/dns-query"), BootstrapIPs: strings.Split(value("DNS_BOOTSTRAP_IPS", "1.1.1.1,1.0.0.1"), ",")}
	for _, address := range []string{c.CredentialAddress, c.RuntimeAddress, c.GrantAddress} {
		if _, _, err := net.SplitHostPort(address); err != nil {
			return Config{}, errors.New("invalid model gateway listen address")
		}
	}
	if hosts := os.Getenv("MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS"); hosts != "" {
		if os.Getenv("MODEL_GATEWAY_DEVELOPMENT") != "true" {
			return Config{}, errors.New("fixture model endpoints require explicit development mode")
		}
		c.DevelopmentHosts = strings.Split(hosts, ",")
	}
	return c, nil
}

// ServerTLS loads the server-only identity for the credential and runtime listeners.
func (c *Config) ServerTLS() (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.CertificateFile, c.PrivateKeyFile)
	if err != nil {
		return nil, errors.New("model gateway TLS identity unavailable")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}, nil
}

// GrantTLS additionally requires a trusted client certificate; RuntimeScope enforces its purpose.
func (c *Config) GrantTLS() (*tls.Config, error) {
	config, err := c.ServerTLS()
	if err != nil {
		return nil, err
	}
	trust, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, errors.New("model gateway trust unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return nil, errors.New("invalid model gateway trust")
	}
	config.ClientAuth = tls.RequireAndVerifyClientCert
	config.ClientCAs = pool
	return config, nil
}
