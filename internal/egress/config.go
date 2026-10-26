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
		return nil, invalidField("file", "unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, invalidField("file", "too large or unreadable")
	}
	return Parse(data)
}

// Parse validates schema, canonicalizes identities and freezes all configuration values.
func Parse(data []byte) (*Policy, error) {
	if len(data) > 1<<20 {
		return nil, invalidField("configuration", "too large")
	}
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(data)), "", 0); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var c configuration
	if err := decoder.Decode(&c); err != nil {
		return nil, decodingError(err)
	}
	if c.SchemaVersion != 1 {
		return nil, invalidField("schemaVersion", "unsupported or missing")
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return nil, invalidField("configuration", "trailing data")
	}
	p := &Policy{config: c, targets: map[string]rule{}, hosts: map[netip.Addr]bool{}}
	if c.Mode == "public" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || len(fields) != 2 {
			return nil, invalidField("mode", "public mode forbids enterprise fields")
		}
		return p, nil
	}
	if c.Mode != "enterprise" {
		return nil, invalidField("mode", "unsupported or missing")
	}
	if c.Targets == nil {
		return nil, invalidField("targets", "required")
	}
	if c.Topology == nil {
		return nil, invalidField("topology", "required")
	}
	if len(c.Topology.ClusterCIDRs) == 0 {
		return nil, invalidField("topology.clusterCidrs", "nonempty array required")
	}
	if len(c.Topology.HostAddresses) == 0 {
		return nil, invalidField("topology.hostAddresses", "nonempty array required")
	}
	var err error
	p.cluster, err = prefixes(c.Topology.ClusterCIDRs, "topology.clusterCidrs")
	if err != nil {
		return nil, err
	}
	hosts, err := addresses(c.Topology.HostAddresses, "topology.hostAddresses")
	if err != nil {
		return nil, err
	}
	for _, a := range hosts {
		p.hosts[a] = true
	}
	if c.Resolver != nil {
		if c.Resolver.Transport != "tcp" {
			return nil, invalidField("resolver.transport", "must be tcp")
		}
		a, err := netip.ParseAddrPort(c.Resolver.Address)
		if err != nil || a.Port() == 0 || a.Addr().Zone() != "" || a.Addr().Unmap().IsUnspecified() || a.Addr().Unmap().IsMulticast() {
			return nil, invalidField("resolver.address", "numeric unicast IP and nonzero port required")
		}
		p.resolver = a.Addr().Unmap()
		c.Resolver.Address = netip.AddrPortFrom(p.resolver, a.Port()).String()
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		field := indexedField("targets", i)
		t.Host, err = hostname(t.Host)
		if err != nil {
			return nil, invalidField(field+".host", "valid DNS hostname required")
		}
		if t.Port < 1 || t.Port > 65535 {
			return nil, invalidField(field+".port", "must be in 1..65535")
		}
		if t.AllowedCIDRs != nil && len(t.AllowedCIDRs) == 0 {
			return nil, invalidField(field+".allowedCidrs", "empty")
		}
		if t.StaticAddresses != nil && len(t.StaticAddresses) == 0 {
			return nil, invalidField(field+".staticAddresses", "empty")
		}
		key := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		if _, exists := p.targets[key]; exists {
			return nil, invalidField(field, "duplicate host/port")
		}
		r := rule{private: t.AllowPrivate}
		r.prefixes, err = prefixes(t.AllowedCIDRs, field+".allowedCidrs")
		if err != nil {
			return nil, err
		}
		r.static, err = addresses(t.StaticAddresses, field+".staticAddresses")
		if err != nil {
			return nil, err
		}
		if len(r.static) == 0 && c.Resolver == nil {
			return nil, invalidField(field+".staticAddresses", "required without resolver")
		}
		for j, a := range r.static {
			if !p.permits(r, a) {
				return nil, invalidField(indexedField(field+".staticAddresses", j), "forbidden by address policy")
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
