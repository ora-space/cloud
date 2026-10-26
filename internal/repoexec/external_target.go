package repoexec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/egress"
)

// ErrEgressDenied reports an external address the executor refuses to contact.
var ErrEgressDenied = errors.New("repoexec: egress denied")

// addressResolver resolves host names for the public egress path. *egress.DNSResolver and
// *net.Resolver satisfy it; tests inject a fixed table.
type addressResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// EgressConfig is the deployment-owned startup input of the shared egress policy (ADR D4). It is
// read once: loading failures keep the executor from starting, and there is no fallback to public
// mode when an enterprise file was named.
type EgressConfig struct {
	// Policy is the shared deployment snapshot (internal/egress), the only source of admitted
	// targets, topology and static addresses.
	Policy *egress.Policy
	// DNS is the deployment resolver used in public mode. It must be the same selection Cloud and
	// model-gateway report in their health digest.
	DNS egress.DNSConfig
	// Development enables the isolated test-stack exception of the model-access root decision D5:
	// the exact hosts listed in DevelopmentHosts may resolve to fixture addresses on the cluster
	// network. It exists only in public mode; enterprise mode refuses to start with it.
	Development bool
	// DevelopmentHosts lists exact fixture host names, never patterns, ports or URLs.
	DevelopmentHosts []string
	// InternalHosts lists the executor's own fixed internal peers. They are always refused as
	// external repositories, whatever the shared policy says.
	InternalHosts []string
}

// Egress decides which external addresses the executor may contact, by delegating every rule to the
// shared deployment policy (ADR D4). The repository URL comes from a user, and the executor shares a
// network with the Git Server's service listener and the Credential Store, so without this check a
// Binding would be an SSRF entry into internal services under the executor's identity.
//
// The executor adds no allow list of its own: it only contributes the checked address set to Git
// (http.curloptResolve), so that the name Git sees is resolved exactly once per subprocess.
type Egress struct {
	policy   *egress.Policy
	resolver addressResolver
	close    func()
	fixtures addressResolver
	allowed  map[string]struct{}
	internal []string
	dialer   net.Dialer
}

// NewEgress validates the configuration and prepares the public resolver. The returned value is
// immutable for the process lifetime.
func NewEgress(config *EgressConfig) (*Egress, error) {
	if config.Policy == nil {
		return nil, errors.New("egress policy is required")
	}
	// The shared decision D1 rejects this pair: the fixture exception would widen an enterprise
	// target list, which only deployment-owned targets may define.
	if config.Policy.Enterprise() && (config.Development || len(config.DevelopmentHosts) != 0) {
		return nil, errors.New("enterprise egress conflicts with development hosts")
	}
	e := &Egress{policy: config.Policy, allowed: map[string]struct{}{}, dialer: net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}}
	for _, host := range config.InternalHosts {
		host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
		if host != "" {
			e.internal = append(e.internal, host)
		}
	}
	if config.Policy.Enterprise() {
		return e, nil
	}
	resolver, err := egress.NewDNSResolver(config.DNS)
	if err != nil {
		return nil, err
	}
	e.resolver, e.close = resolver, resolver.CloseIdleConnections
	if config.Development {
		e.fixtures = net.DefaultResolver
		for _, host := range config.DevelopmentHosts {
			host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
			if host == "" || strings.ContainsAny(host, "/:* \r\n\t") {
				resolver.CloseIdleConnections()
				return nil, errors.New("invalid development fixture hostname")
			}
			e.allowed[host] = struct{}{}
		}
	}
	return e, nil
}

// Close releases the public resolver's pooled connections.
func (e *Egress) Close() {
	if e.close != nil {
		e.close()
	}
}

// Check validates the repository URL and every address it resolves to before anything is redeemed.
func (e *Egress) Check(ctx context.Context, raw string) error {
	_, err := e.resolve(ctx, raw)
	return err
}

// externalTarget is one approved external repository URL together with the addresses its host was
// checked at. Git receives the addresses; the URL keeps the host name for TLS.
type externalTarget struct {
	URL       string
	host      string
	port      string
	addresses []netip.Addr
}

// pins is the Git configuration that hands the checked addresses to libcurl for this one subprocess
// (ADR D4). libcurl then dials only these numeric addresses and never resolves the name itself, so a
// name that changes after the check cannot redirect the process. IPv6 addresses are bracketed, the
// syntax curl accepts inside --resolve.
func (t externalTarget) pins() []string {
	values := make([]string, 0, len(t.addresses))
	for _, address := range t.addresses {
		address = address.Unmap()
		if address.Is6() {
			values = append(values, "["+address.String()+"]")
		} else {
			values = append(values, address.String())
		}
	}
	return []string{"http.curloptResolve=" + net.JoinHostPort(t.host, t.port) + ":" + strings.Join(values, ",")}
}

// resolve applies the shared policy to one external repository URL. Enterprise mode admits only the
// deployment's exact host:port targets; public mode resolves through the deployment resolver and
// admits the target only when every address is public.
func (e *Egress) resolve(ctx context.Context, raw string) (externalTarget, error) {
	u, err := url.Parse(raw)
	// Query strings have no meaning for a Git repository URL and would give the target a second
	// identity next to the authority the policy checks.
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery {
		return externalTarget{}, fmt.Errorf("%w: not an https URL", ErrEgressDenied)
	}
	host := strings.ToLower(u.Hostname())
	if e.denied(host) {
		return externalTarget{}, fmt.Errorf("%w: host is internal", ErrEgressDenied)
	}
	target := externalTarget{URL: raw, host: host, port: u.Port()}
	if target.port == "" {
		target.port = "443"
	}
	if e.policy.Enterprise() {
		// Authority canonicalizes the host:port and enforces the target list; Resolve then checks
		// every address against topology, allowPrivate and static mapping.
		authority, err := e.policy.Authority(u)
		if err != nil {
			return externalTarget{}, fmt.Errorf("%w: target is not in the deployment policy", ErrEgressDenied)
		}
		target.addresses, err = e.policy.Resolve(ctx, authority)
		if err != nil {
			return externalTarget{}, fmt.Errorf("%w: target address is not allowed", ErrEgressDenied)
		}
		target.host, target.port, err = net.SplitHostPort(authority)
		if err != nil {
			return externalTarget{}, fmt.Errorf("%w: target is not in the deployment policy", ErrEgressDenied)
		}
		return target, nil
	}
	target.addresses, err = e.addresses(ctx, host)
	if err != nil {
		return externalTarget{}, err
	}
	return target, nil
}

// addresses resolves a public target and requires every answer to be globally routable. A listed
// development fixture host is resolved by the system resolver instead, and only in public mode.
func (e *Egress) addresses(ctx context.Context, host string) ([]netip.Addr, error) {
	if _, fixture := e.allowed[host]; fixture {
		values, err := e.fixtures.LookupNetIP(ctx, "ip", host)
		if err != nil || len(values) == 0 {
			return nil, fmt.Errorf("%w: fixture host does not resolve", ErrEgressDenied)
		}
		return values, nil
	}
	values, err := e.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(values) == 0 {
		return nil, fmt.Errorf("%w: host does not resolve", ErrEgressDenied)
	}
	for _, address := range values {
		if !egress.PublicAddress(address) {
			return nil, fmt.Errorf("%w: host resolves to a non-public address", ErrEgressDenied)
		}
	}
	return values, nil
}

// denied reports an internal peer name, matched exactly or as a parent domain.
func (e *Egress) denied(host string) bool {
	for _, internal := range e.internal {
		if host == internal || strings.HasSuffix(host, "."+internal) {
			return true
		}
	}
	return false
}

// DialContext backs the Go HTTP client of the receive-pack probe (ADR D4): every new connection
// re-resolves and re-checks the name, dials only a checked numeric address, and never consults a
// system resolver outside the development fixture exception.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid address", ErrEgressDenied)
	}
	host = strings.ToLower(host)
	if e.denied(host) {
		return nil, fmt.Errorf("%w: host is internal", ErrEgressDenied)
	}
	var addresses []netip.Addr
	if e.policy.Enterprise() {
		if addresses, err = e.policy.Resolve(ctx, addr); err != nil {
			return nil, fmt.Errorf("%w: target address is not allowed", ErrEgressDenied)
		}
	} else if addresses, err = e.addresses(ctx, host); err != nil {
		return nil, err
	}
	var last error
	for _, address := range addresses {
		connection, dialErr := e.dialer.DialContext(ctx, network, net.JoinHostPort(address.Unmap().String(), port))
		if dialErr == nil {
			return connection, nil
		}
		last = dialErr
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = ErrEgressDenied
	}
	return nil, last
}
