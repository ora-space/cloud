package modelgateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
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

	// dialContext substitutes the concrete dialer in tests that need a deterministic unresponsive
	// address. Production leaves it nil and uses dialer.DialContext.
	dialContext func(context.Context, string, string) (net.Conn, error)
}

// minAddressDialBudget stops one unresponsive address from spending the time budgeted for the
// addresses after it. The caller's whole-dial deadline still bounds every attempt, so the floor
// never extends the total wait past d.dialer.Timeout.
const minAddressDialBudget = 2 * time.Second

// dial resolves and checks all DNS answers immediately before connecting to a selected numeric
// address. The HTTP URL retains the original hostname for normal TLS hostname verification. The
// whole-dial budget stays d.dialer.Timeout; each address gets an equal share of what remains so a
// silent edge address cannot starve the later answers.
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
	timeout := d.dialer.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	dial := d.dialContext
	if dial == nil {
		dial = d.dialer.DialContext
	}
	failures := make([]addressDialFailure, 0, len(addresses))
	for index, a := range addresses {
		target := net.JoinHostPort(a.String(), port)
		budget := addressDialBudget(time.Until(deadline), len(addresses)-index)
		attemptCtx, attemptCancel := context.WithTimeout(ctx, budget)
		connection, dialErr := dial(attemptCtx, network, target)
		attemptCancel()
		if dialErr == nil {
			if d.policy.Enterprise() {
				return leaseConnection(connection, upstreamConnectionLifetime), nil
			}
			return connection, nil
		}
		failures = append(failures, addressDialFailure{address: target, reason: dialFailureReason(dialErr)})
		if err := ctx.Err(); err != nil {
			// The whole-dial budget is gone. Report the addresses that were never tried so the log
			// does not imply they failed for their own reason.
			for _, remaining := range addresses[index+1:] {
				failures = append(failures, addressDialFailure{address: net.JoinHostPort(remaining.String(), port), reason: "not attempted"})
			}
			return nil, &dialFailure{attempts: failures, cause: err}
		}
	}
	return nil, &dialFailure{attempts: failures}
}

// addressDialBudget divides the remaining whole-dial budget over the addresses still to try. The
// floor keeps one silent address from consuming the share of the addresses after it; the caller's
// parent deadline clamps an attempt whose floor exceeds the leftover budget.
func addressDialBudget(remaining time.Duration, addresses int) time.Duration {
	if addresses < 1 {
		return minAddressDialBudget
	}
	share := remaining / time.Duration(addresses)
	if share < minAddressDialBudget {
		return minAddressDialBudget
	}
	return share
}

// addressDialFailure is one checked address and its bounded reason. It never holds the request URL,
// query, headers or credentials.
type addressDialFailure struct {
	address string
	reason  string
}

// dialFailure reports every checked address after none connected. Unwrap keeps the stable transport
// classification while the text stays address-only for structured logs.
type dialFailure struct {
	attempts []addressDialFailure
	cause    error
}

func (e *dialFailure) Error() string {
	parts := make([]string, len(e.attempts))
	for index, attempt := range e.attempts {
		parts[index] = attempt.address + " " + attempt.reason
	}
	return "checked dial failed: " + strings.Join(parts, "; ")
}

// DialFailureDetail is the bounded address-and-reason summary safe for structured logs. The error
// returned by http.Client wraps a URL, which can carry a signed asset query, so log callers must
// use this method rather than the whole error text.
func (e *dialFailure) DialFailureDetail() string { return e.Error() }

// Unwrap classifies the whole failure. Every address that was tried failed on its own, so the
// stable unavailable code applies; a budget or cancellation terminal keeps its own classification.
func (e *dialFailure) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return endpointUnavailable
}

// dialFailureReason classifies one dial error without copying OS, provider or request text.
func dialFailureReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "timeout"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable"
	}
	return "dial failed"
}

// UpstreamConfig fixes deployment-owned DNS and exact development fixture exemptions.
type UpstreamConfig struct {
	DevelopmentHosts []string
	DNS              egress.DNSConfig
	Policy           *egress.Policy
}

// UpstreamTimeouts bounds individual stages of an upstream request. Nonpositive fields retain
// the production defaults: ten seconds each for TCP/TLS and thirty seconds for headers.
type UpstreamTimeouts struct {
	Connect        time.Duration
	ResponseHeader time.Duration
}

// NewUpstreamClient disables environmental proxies and redirects, and enforces the deployment
// policy on every request and new connection. Exact fixture host exemptions are deployment-owned, development only.
func NewUpstreamClient(config UpstreamConfig) (*http.Client, error) {
	return newUpstreamClient(config, nil, UpstreamTimeouts{})
}

// NewUpstreamClientWithRoots builds the same client but verifies upstream identity against roots
// instead of the platform pool. Hermetic transport tests use it against a controlled HTTPS server
// and keep every other check exactly as deployed; production wiring only calls NewUpstreamClient,
// so upstream identity always comes from the deployment's system roots.
func NewUpstreamClientWithRoots(config UpstreamConfig, roots *x509.CertPool) (*http.Client, error) {
	if roots == nil {
		return nil, errors.New("upstream TLS trust requires an explicit root pool")
	}
	return newUpstreamClient(config, roots, UpstreamTimeouts{})
}

// NewUpstreamClientWithTimeouts injects stage limits without replacing the checked transport.
// A nil root pool uses platform trust, as NewUpstreamClient does; explicit roots retain normal
// TLS verification against that pool. The ordinary constructors keep production defaults.
func NewUpstreamClientWithTimeouts(config UpstreamConfig, roots *x509.CertPool, limits UpstreamTimeouts) (*http.Client, error) {
	return newUpstreamClient(config, roots, limits)
}

func newUpstreamClient(config UpstreamConfig, roots *x509.CertPool, limits UpstreamTimeouts) (*http.Client, error) {
	timeouts := UpstreamTimeouts{Connect: 10 * time.Second, ResponseHeader: 30 * time.Second}
	if limits.Connect > 0 {
		timeouts.Connect = limits.Connect
	}
	if limits.ResponseHeader > 0 {
		timeouts.ResponseHeader = limits.ResponseHeader
	}
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
	var resolver *egress.DNSResolver
	if !config.Policy.Enterprise() {
		var err error
		resolver, err = egress.NewDNSResolver(config.DNS)
		if err != nil {
			return nil, err
		}
	}
	d := &secureDialer{resolver: resolver, fixtures: net.DefaultResolver, dialer: net.Dialer{Timeout: timeouts.Connect, KeepAlive: 30 * time.Second}, allowed: allowed, policy: config.Policy}
	transport := &http.Transport{Proxy: nil, DialContext: d.dial, TLSHandshakeTimeout: timeouts.Connect, ResponseHeaderTimeout: timeouts.ResponseHeader, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 64, MaxConnsPerHost: 32, MaxResponseHeaderBytes: 64 << 10, ForceAttemptHTTP2: !config.Policy.Enterprise()}
	if roots != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	}
	return &http.Client{Transport: &upstreamTransport{Transport: transport, resolver: resolver, policy: config.Policy}, CheckRedirect: func(*http.Request, []*http.Request) error { return endpointRedirectForbidden }}, nil
}

type upstreamTransport struct {
	*http.Transport
	resolver *egress.DNSResolver
	policy   *egress.Policy
}

// CloseIdleConnections releases both model and DNS pools owned by this client.
func (t *upstreamTransport) CloseIdleConnections() {
	t.Transport.CloseIdleConnections()
	if t.resolver != nil {
		t.resolver.CloseIdleConnections()
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
