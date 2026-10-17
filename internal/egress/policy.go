// Package egress owns immutable deployment policy shared by model and asset traffic.
package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ErrForbidden and ErrResolution are bounded diagnostics without endpoint details.
var (
	ErrForbidden  = errors.New("deployment egress forbidden")
	ErrResolution = errors.New("deployment egress resolution failed")
)

type target struct {
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	AllowPrivate    bool     `json:"allowPrivate,omitempty"`
	AllowedCIDRs    []string `json:"allowedCidrs,omitempty"`
	StaticAddresses []string `json:"staticAddresses,omitempty"`
}
type resolverConfig struct {
	Transport string `json:"transport"`
	Address   string `json:"address"`
}
type topology struct {
	ClusterCIDRs  []string `json:"clusterCidrs"`
	HostAddresses []string `json:"hostAddresses"`
}
type configuration struct {
	SchemaVersion int             `json:"schemaVersion"`
	Mode          string          `json:"mode"`
	Resolver      *resolverConfig `json:"resolver,omitempty"`
	Targets       []target        `json:"targets,omitempty"`
	Topology      *topology       `json:"topology,omitempty"`
}
type rule struct {
	private  bool
	prefixes []netip.Prefix
	static   []netip.Addr
}

// Policy is an immutable startup snapshot. Its zero value is not a valid enterprise policy.
type Policy struct {
	config  configuration
	targets map[string]rule
	cluster []netip.Prefix
	hosts   map[netip.Addr]bool
}

// Enterprise reports whether all destinations must match deployment-owned targets.
func (p *Policy) Enterprise() bool { return p != nil && p.config.Mode == "enterprise" }

// Digest reports a canonical snapshot identity, including effective public DNS configuration.
// Only the digest, never the canonical input, may appear in readiness responses.
func (p *Policy) Digest(publicDNS any) string {
	var dns any
	if !p.Enterprise() {
		dns = publicDNS
	}
	data, _ := json.Marshal(struct {
		Policy configuration
		DNS    any
	}{p.config, dns})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hostname(raw string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(raw, "."))
	if host == "" || len(host) > 253 {
		return "", ErrForbidden
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", ErrForbidden
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrForbidden
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", ErrForbidden
			}
		}
	}
	return host, nil
}

// Authority validates HTTPS identity without imposing model-only path/query semantics.
func (p *Policy) Authority(u *url.URL) (string, error) {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", ErrForbidden
	}
	host, err := hostname(u.Hostname())
	if err != nil {
		return "", err
	}
	port := 443
	if raw := u.Port(); raw != "" {
		for _, c := range raw {
			if c < '0' || c > '9' {
				return "", ErrForbidden
			}
		}
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return "", ErrForbidden
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", ErrForbidden
	}
	key := net.JoinHostPort(host, strconv.Itoa(port))
	if p.Enterprise() {
		if _, ok := p.targets[key]; !ok {
			return "", ErrForbidden
		}
	}
	return key, nil
}

// CheckModelURL performs enterprise save-time admission outside database transactions.
// Public deployments retain the existing synchronous model metadata validation.
func (p *Policy) CheckModelURL(ctx context.Context, raw string) error {
	if !p.Enterprise() {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.ForceQuery {
		return ErrForbidden
	}
	authority, err := p.Authority(u)
	if err != nil {
		return err
	}
	_, err = p.Resolve(ctx, authority)
	return err
}

// Resolve checks the complete address set for an enterprise authority before any business dial.
func (p *Policy) Resolve(ctx context.Context, authority string) ([]netip.Addr, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return nil, ErrForbidden
	}
	u := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}
	key, err := p.Authority(u)
	if err != nil || !p.Enterprise() {
		return nil, ErrForbidden
	}
	r := p.targets[key]
	addresses := r.static
	if len(addresses) == 0 {
		addresses, err = lookupTCP(ctx, p.config.Resolver.Address, strings.Split(key, ":")[0])
		if err != nil {
			return nil, ErrResolution
		}
	}
	if len(addresses) == 0 {
		return nil, ErrResolution
	}
	for _, address := range addresses {
		if !p.permits(r, address) {
			return nil, ErrForbidden
		}
	}
	return append([]netip.Addr(nil), addresses...), nil
}

func (p *Policy) permits(r rule, address netip.Addr) bool {
	a := address.Unmap()
	if !a.IsValid() || a.Zone() != "" || p.hosts[a] || a.IsLoopback() || a.IsLinkLocalUnicast() || !a.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range p.cluster {
		if prefix.Contains(a) {
			return false
		}
	}
	if a.IsPrivate() {
		if !r.private {
			return false
		}
	} else if !PublicAddress(a) {
		return false
	}
	if len(r.prefixes) == 0 {
		return true
	}
	for _, prefix := range r.prefixes {
		if prefix.Contains(a) {
			return true
		}
	}
	return false
}

// PublicAddress preserves the existing reserved-address policy for public deployments and DNS bootstrap.
func PublicAddress(address netip.Addr) bool {
	a := address.Unmap()
	if !a.IsValid() || a.Zone() != "" || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsUnspecified() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range reservedNetworks {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var reservedNetworks = func() []netip.Prefix {
	var out []netip.Prefix
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001::/32", "2001:2::/48", "2001:db8::/32", "2002::/16", "64:ff9b::/96", "64:ff9b:1::/48"} {
		out = append(out, netip.MustParsePrefix(raw))
	}
	return out
}()

func prefixes(raw []string) ([]netip.Prefix, error) {
	var result []netip.Prefix
	for i, s := range raw {
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Addr().Is4In6() {
			return nil, ErrForbidden
		}
		p = p.Masked()
		raw[i] = p.String()
		result = append(result, p)
	}
	sort.Strings(raw)
	return result, nil
}

func addresses(raw []string) ([]netip.Addr, error) {
	var result []netip.Addr
	for i, s := range raw {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return nil, ErrForbidden
		}
		a = a.Unmap()
		raw[i] = a.String()
		result = append(result, a)
	}
	sort.Strings(raw)
	return result, nil
}
