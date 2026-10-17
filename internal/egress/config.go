package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
)

// Load reads a bounded deployment file once. Empty path selects the legacy public policy.
func Load(path string) (*Policy, error) {
	if path == "" {
		return Parse([]byte(`{"schemaVersion":1,"mode":"public"}`))
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("deployment egress configuration unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("deployment egress configuration too large or unreadable")
	}
	return Parse(data)
}

// Parse validates schema, canonicalizes identities and freezes all configuration values.
func Parse(data []byte) (*Policy, error) {
	invalid := errors.New("invalid deployment egress configuration")
	if len(data) > 1<<20 || uniqueJSON(json.NewDecoder(bytes.NewReader(data)), 0) != nil {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var c configuration
	if decoder.Decode(&c) != nil || c.SchemaVersion != 1 {
		return nil, invalid
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, invalid
	}
	p := &Policy{config: c, targets: map[string]rule{}, hosts: map[netip.Addr]bool{}}
	if c.Mode == "public" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || len(fields) != 2 {
			return nil, invalid
		}
		return p, nil
	}
	if c.Mode != "enterprise" || c.Targets == nil || c.Topology == nil || len(c.Topology.ClusterCIDRs) == 0 || len(c.Topology.HostAddresses) == 0 {
		return nil, invalid
	}
	var err error
	p.cluster, err = prefixes(c.Topology.ClusterCIDRs)
	if err != nil {
		return nil, invalid
	}
	hosts, err := addresses(c.Topology.HostAddresses)
	if err != nil {
		return nil, invalid
	}
	for _, a := range hosts {
		p.hosts[a] = true
	}
	if c.Resolver != nil {
		a, err := netip.ParseAddrPort(c.Resolver.Address)
		if err != nil || a.Port() == 0 || a.Addr().Zone() != "" || a.Addr().Unmap().IsUnspecified() || a.Addr().Unmap().IsMulticast() || c.Resolver.Transport != "tcp" {
			return nil, invalid
		}
		c.Resolver.Address = netip.AddrPortFrom(a.Addr().Unmap(), a.Port()).String()
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		t.Host, err = hostname(t.Host)
		if err != nil || t.Port < 1 || t.Port > 65535 || t.AllowedCIDRs != nil && len(t.AllowedCIDRs) == 0 || t.StaticAddresses != nil && len(t.StaticAddresses) == 0 {
			return nil, invalid
		}
		key := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		if _, exists := p.targets[key]; exists {
			return nil, invalid
		}
		r := rule{private: t.AllowPrivate}
		r.prefixes, err = prefixes(t.AllowedCIDRs)
		if err != nil {
			return nil, invalid
		}
		r.static, err = addresses(t.StaticAddresses)
		if err != nil || len(r.static) == 0 && c.Resolver == nil {
			return nil, invalid
		}
		for _, a := range r.static {
			if !p.permits(r, a) {
				return nil, invalid
			}
		}
		p.targets[key] = r
	}
	sort.Slice(c.Targets, func(i, j int) bool {
		if c.Targets[i].Host == c.Targets[j].Host {
			return c.Targets[i].Port < c.Targets[j].Port
		}
		return c.Targets[i].Host < c.Targets[j].Host
	})
	p.config = c
	return p, nil
}

// uniqueJSON rejects duplicate keys, null fields and excessive nesting before typed decoding.
// Null must not turn an explicitly supplied constraint into an omitted one.
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 16 {
		return ErrForbidden
	}
	t, err := d.Token()
	if err != nil || t == nil {
		return ErrForbidden
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrForbidden
			}
			s, ok := key.(string)
			if !ok || seen[s] || !schemaKey(s) {
				return ErrForbidden
			}
			seen[s] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return ErrForbidden
	}
	_, err = d.Token()
	return err
}

// encoding/json otherwise accepts case-insensitive aliases for tagged fields.
func schemaKey(key string) bool {
	switch key {
	case "schemaVersion", "mode", "resolver", "targets", "topology", "transport", "address", "host", "port", "allowPrivate", "allowedCidrs", "staticAddresses", "clusterCidrs", "hostAddresses":
		return true
	default:
		return false
	}
}
