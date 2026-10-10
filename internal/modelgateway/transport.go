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
	dialer   net.Dialer
	allowed  map[string]struct{}
}

// dial resolves and checks all DNS answers immediately before connecting to a selected numeric
// address. The HTTP URL retains the original hostname for normal TLS hostname verification.
func (d *secureDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid model endpoint")
	}
	addresses, err := d.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, errors.New("model endpoint resolution failed")
	}
	_, trustedFixture := d.allowed[strings.ToLower(host)]
	for _, a := range addresses {
		if !trustedFixture && !publicAddress(a) {
			return nil, errors.New("private model endpoint forbidden")
		}
	}
	for _, a := range addresses {
		connection, dialErr := d.dialer.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("model endpoint unavailable")
}

// NewUpstreamClient disables environmental proxies and redirects, and enforces the public-address
// policy at connection time. Exact fixture host exemptions are deployment-owned, development only.
func NewUpstreamClient(fixtureHosts []string) (*http.Client, error) {
	allowed := map[string]struct{}{}
	for _, host := range fixtureHosts {
		if host == "" || strings.ContainsAny(host, "/:* \r\n\t") {
			return nil, errors.New("invalid model fixture hostname")
		}
		allowed[strings.ToLower(host)] = struct{}{}
	}
	d := &secureDialer{resolver: net.DefaultResolver, dialer: net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, allowed: allowed}
	transport := &http.Transport{Proxy: nil, DialContext: d.dial, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 64, MaxConnsPerHost: 32, MaxResponseHeaderBytes: 64 << 10, ForceAttemptHTTP2: true}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("model redirects forbidden") }}, nil
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
