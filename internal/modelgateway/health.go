package modelgateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// CheckHealth performs the container readiness probe with normal public-CA and hostname
// verification; it does not open the master key or authenticate as a model user.
func CheckHealth(config *Config) error {
	trust, err := os.ReadFile(config.CAFile)
	if err != nil {
		return errors.New("model health trust unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return errors.New("invalid model health trust")
	}
	origin, err := url.Parse(config.PublicOrigin)
	if err != nil {
		return errors.New("invalid model health origin")
	}
	_, port, err := net.SplitHostPort(config.CredentialAddress)
	if err != nil {
		return errors.New("invalid model health address")
	}
	endpoint := &url.URL{Scheme: "https", Host: net.JoinHostPort(origin.Hostname(), port), Path: "/healthz"}
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("model health redirect forbidden") }}
	response, err := client.Get(endpoint.String())
	if err != nil {
		return errors.New("model service not ready")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return errors.New("model service not ready")
	}
	return nil
}
