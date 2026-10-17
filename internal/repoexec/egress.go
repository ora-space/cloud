package repoexec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// ErrEgressDenied reports an external address the executor refuses to contact.
var ErrEgressDenied = errors.New("repoexec: egress denied")

// Resolver resolves host names for the egress check. *net.Resolver satisfies it; tests inject a
// fixed table.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Egress decides which external addresses the executor may contact (ADR D4). The repository URL comes
// from a user, and the executor shares a network with the Git Server's service listener and the
// Credential Store, so without this check a Binding would be an SSRF entry into internal services
// under the executor's identity.
type Egress struct {
	resolver Resolver
	allow    map[string]bool
	deny     []string
	dialer   net.Dialer
}

// NewEgress builds the policy. allow lists "host" or "host:port" entries that bypass every check
// (acceptance fixtures on loopback); deny lists internal host names, matched exactly or as a parent
// domain.
func NewEgress(resolver Resolver, allow, deny []string) (*Egress, error) {
	if resolver == nil {
		return nil, errors.New("egress resolver is required")
	}
	e := &Egress{resolver: resolver, allow: map[string]bool{}}
	for _, entry := range allow {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.ContainsAny(entry, "/@?# \t") {
			return nil, fmt.Errorf("egress allow entry %q is not a host or host:port", entry)
		}
		e.allow[entry] = true
	}
	for _, host := range deny {
		host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
		if host != "" {
			e.deny = append(e.deny, host)
		}
	}
	return e, nil
}

// Check validates an external https URL before any Git command contacts it: scheme, port, host name
// and every address the host resolves to.
//
// Git resolves the name again itself; the window between the two lookups (DNS rebinding) is an open
// item of the ADR. Redirects are never followed, so the first request is the only one that leaves.
func (e *Egress) Check(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("%w: not an https URL", ErrEgressDenied)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	_, err = e.addresses(ctx, strings.ToLower(u.Hostname()), port)
	return err
}

// addresses returns the addresses a connection to host:port may use. An allowlisted entry bypasses
// the checks and returns nil, meaning "dial the name as given".
func (e *Egress) addresses(ctx context.Context, host, port string) ([]netip.Addr, error) {
	if e.allow[host] || e.allow[net.JoinHostPort(host, port)] {
		return nil, nil
	}
	if port != "443" {
		return nil, fmt.Errorf("%w: port %s is not allowed", ErrEgressDenied, port)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !publicAddress(ip) {
			return nil, fmt.Errorf("%w: address is not public", ErrEgressDenied)
		}
		return []netip.Addr{ip}, nil
	}
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".localhost") {
		return nil, fmt.Errorf("%w: host is not a public name", ErrEgressDenied)
	}
	for _, internal := range e.deny {
		if host == internal || strings.HasSuffix(host, "."+internal) {
			return nil, fmt.Errorf("%w: host is internal", ErrEgressDenied)
		}
	}
	ips, err := e.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("%w: host does not resolve", ErrEgressDenied)
	}
	for _, ip := range ips {
		if !publicAddress(ip) {
			return nil, fmt.Errorf("%w: host resolves to a non-public address", ErrEgressDenied)
		}
	}
	return ips, nil
}

// DialContext dials addr only at an address the policy admits, re-validating at dial time. It backs
// the Go HTTP client of the receive-pack probe, so that client has no lookup-to-connect gap.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid address", ErrEgressDenied)
	}
	ips, err := e.addresses(ctx, strings.ToLower(host), port)
	if err != nil {
		return nil, err
	}
	if ips == nil {
		return e.dialer.DialContext(ctx, network, addr)
	}
	var last error
	for _, ip := range ips {
		conn, err := e.dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// nonPublic lists special-purpose ranges netip's predicates do not cover.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),      // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved, includes broadcast
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64: may embed an internal IPv4 address
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("::ffff:0:0/96"),  // IPv4-mapped (checked after Unmap; kept for safety)
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001::/23"),      // IETF protocol assignments (Teredo, ORCHID...)
	netip.MustParsePrefix("169.254.0.0/16"), // link-local, including the metadata service
	netip.MustParsePrefix("fd00:ec2::/32"),  // cloud metadata over IPv6 (within ULA, explicit)
	netip.MustParsePrefix("255.255.255.255/32"),
}

// publicAddress reports whether ip is a globally routable unicast address: not loopback, private
// (RFC 1918, ULA fc00::/7), link-local, multicast, unspecified, CGNAT, metadata or reserved.
func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
