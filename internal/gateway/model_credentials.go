package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/wanglongan587/cloud/internal/modelgateway"
)

// isModelCredentialPath shares the service's routing allowlist rather than duplicating spellings.
func isModelCredentialPath(path string) bool { return modelgateway.IsCredentialPath(path) }

// newCredentialProxy retains normal TLS verification using deployment-supplied public trust.
func newCredentialProxy(upstream *url.URL, caFile string, timeout time.Duration) (*proxy, error) {
	if upstream.Scheme != "https" || upstream.Host == "" || upstream.User != nil || upstream.Path != "" || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("model credentials require a fixed HTTPS origin")
	}
	trust, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("model credential trust unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(trust) {
		return nil, errors.New("invalid model credential trust")
	}
	p := newProxy(upstream, timeout)
	transport, ok := p.inner.Transport.(*http.Transport)
	if !ok {
		return nil, errors.New("invalid model credential transport")
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	return p, nil
}
