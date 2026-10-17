package modelgateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/egress"
)

// publicAddress is retained for public resolver bootstrap and existing transport tests.
func publicAddress(address netip.Addr) bool { return egress.PublicAddress(address) }

type addressResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type secureDialer struct {
	resolver addressResolver
	fixtures addressResolver
	dialer   net.Dialer
	allowed  map[string]struct{}
	policy   *egress.Policy
}

// dial resolves and checks all DNS answers immediately before connecting to a selected numeric
// address. The HTTP URL retains the original hostname for normal TLS hostname verification.
func (d *secureDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, endpointInvalid
	}
	_, trustedFixture := d.allowed[strings.ToLower(host)]
	resolver := d.resolver
	if trustedFixture && d.fixtures != nil {
		resolver = d.fixtures
	}
	var addresses []netip.Addr
	if d.policy.Enterprise() {
		addresses, err = d.policy.Resolve(ctx, address)
		if errors.Is(err, egress.ErrForbidden) {
			return nil, endpointForbidden
		}
	} else {
		addresses, err = resolver.LookupNetIP(ctx, "ip", host)
	}
	if err != nil || len(addresses) == 0 {
		return nil, endpointResolutionFailed
	}
	for _, a := range addresses {
		if !d.policy.Enterprise() && !trustedFixture && !publicAddress(a) {
			return nil, endpointForbidden
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, a := range addresses {
		connection, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if dialErr == nil {
			return leaseConnection(connection, upstreamConnectionLifetime), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, endpointUnavailable
}

// UpstreamConfig fixes deployment-owned DNS and exact development fixture exemptions.
type UpstreamConfig struct {
	DevelopmentHosts []string
	DNS              DNSConfig
	Policy           *egress.Policy
}

// NewUpstreamClient disables environmental proxies and redirects, and enforces the deployment
// policy on every request and new connection. Exact fixture host exemptions are deployment-owned, development only.
func NewUpstreamClient(config UpstreamConfig) (*http.Client, error) {
	if config.Policy.Enterprise() && len(config.DevelopmentHosts) != 0 {
		return nil, errors.New("enterprise egress conflicts with development hosts")
	}
	allowed := map[string]struct{}{}
	for _, host := range config.DevelopmentHosts {
		if host == "" || strings.ContainsAny(host, "/:* \r\n\t") {
			return nil, errors.New("invalid model fixture hostname")
		}
		allowed[strings.ToLower(host)] = struct{}{}
	}
	var resolver *httpsResolver
	if !config.Policy.Enterprise() {
		var err error
		resolver, err = newHTTPSResolver(config.DNS)
		if err != nil {
			return nil, err
		}
	}
	d := &secureDialer{resolver: resolver, fixtures: net.DefaultResolver, dialer: net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, allowed: allowed, policy: config.Policy}
	transport := &http.Transport{Proxy: nil, DialContext: d.dial, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 64, MaxConnsPerHost: 32, MaxResponseHeaderBytes: 64 << 10, ForceAttemptHTTP2: !config.Policy.Enterprise()}
	return &http.Client{Transport: &upstreamTransport{Transport: transport, resolver: resolver, policy: config.Policy}, CheckRedirect: func(*http.Request, []*http.Request) error { return endpointRedirectForbidden }}, nil
}

type upstreamTransport struct {
	*http.Transport
	resolver *httpsResolver
	policy   *egress.Policy
}

// CloseIdleConnections releases both model and DNS pools owned by this client.
func (t *upstreamTransport) CloseIdleConnections() {
	t.Transport.CloseIdleConnections()
	if t.resolver != nil {
		t.resolver.client.CloseIdleConnections()
	}
}

func upstreamURL(base, protocol string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("invalid model endpoint")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	switch protocol {
	case "openai-completions":
		u.Path += "/chat/completions"
	case "anthropic-messages":
		if !strings.HasSuffix(u.Path, "/v1") {
			u.Path += "/v1"
		}
		u.Path += "/messages"
	default:
		return "", errors.New("unsupported model protocol")
	}
	return u.String(), nil
}

// RoundTrip gates every request, even when the HTTP transport reuses a connection.
func (t *upstreamTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.policy.Enterprise() {
		authority, err := t.policy.Authority(r.URL)
		if err != nil {
			if r.Body != nil {
				_ = r.Body.Close()
			}
			return nil, endpointForbidden
		}
		r = r.Clone(r.Context())
		// Canonical authority retains the URL path and signed query unchanged and provides
		// one pool identity and TLS hostname for case/root-dot/default-port variants.
		// Keep the URL's original HTTP authority, including an omitted default port:
		// signed asset URLs can authenticate Host as well as the query string.
		r.Host = r.URL.Host
		r.URL.Host = authority
	}
	return t.Transport.RoundTrip(r)
}
