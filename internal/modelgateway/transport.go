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
)

// Reserved ranges cover private, link-local, documentation, translation and special-purpose
// networks that must not become reachable through a user-selected model endpoint.
var reservedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}

func publicAddress(address netip.Addr) bool {
	a := address.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range reservedNetworks {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

type addressResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type secureDialer struct {
	resolver addressResolver
	fixtures addressResolver
	dialer   net.Dialer
	allowed  map[string]struct{}
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
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, endpointResolutionFailed
	}
	for _, a := range addresses {
		if !trustedFixture && !publicAddress(a) {
			return nil, endpointForbidden
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, a := range addresses {
		connection, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if dialErr == nil {
			return connection, nil
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
}

// NewUpstreamClient disables environmental proxies and redirects, and enforces the public-address
// policy at connection time. Exact fixture host exemptions are deployment-owned, development only.
func NewUpstreamClient(config UpstreamConfig) (*http.Client, error) {
	allowed := map[string]struct{}{}
	for _, host := range config.DevelopmentHosts {
		if host == "" || strings.ContainsAny(host, "/:* \r\n\t") {
			return nil, errors.New("invalid model fixture hostname")
		}
		allowed[strings.ToLower(host)] = struct{}{}
	}
	resolver, err := newHTTPSResolver(config.DNS)
	if err != nil {
		return nil, err
	}
	d := &secureDialer{resolver: resolver, fixtures: net.DefaultResolver, dialer: net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, allowed: allowed}
	transport := &http.Transport{Proxy: nil, DialContext: d.dial, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 64, MaxConnsPerHost: 32, MaxResponseHeaderBytes: 64 << 10, ForceAttemptHTTP2: true}
	return &http.Client{Transport: &upstreamTransport{Transport: transport, resolver: resolver}, CheckRedirect: func(*http.Request, []*http.Request) error { return endpointRedirectForbidden }}, nil
}

type upstreamTransport struct {
	*http.Transport
	resolver *httpsResolver
}

// CloseIdleConnections releases both model and DNS pools owned by this client.
func (t *upstreamTransport) CloseIdleConnections() {
	t.Transport.CloseIdleConnections()
	t.resolver.client.CloseIdleConnections()
}

func upstreamURL(base, protocol string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
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
