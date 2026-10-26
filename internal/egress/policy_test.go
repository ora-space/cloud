package egress

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const enterpriseFixture = `{"schemaVersion":1,"mode":"enterprise","targets":[{"host":"Model.Example.Invalid.","port":443,"allowPrivate":true,"allowedCidrs":["10.60.0.0/16"],"staticAddresses":["10.60.0.10"]}],"topology":{"clusterCidrs":["172.30.0.0/16"],"hostAddresses":["10.60.0.1"]}}`

func TestStrictStartupConfiguration(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate":           strings.Replace(enterpriseFixture, `"mode":"enterprise"`, `"mode":"enterprise","mode":"public"`, 1),
		"unknown":             strings.Replace(enterpriseFixture, `"schemaVersion":1`, `"schemaVersion":1,"unknown":true`, 1),
		"version":             strings.Replace(enterpriseFixture, `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"public-fields":       strings.Replace(enterpriseFixture, `"enterprise"`, `"public"`, 1),
		"null-constraint":     strings.Replace(enterpriseFixture, `["10.60.0.0/16"]`, `null`, 1),
		"empty-constraint":    strings.Replace(enterpriseFixture, `["10.60.0.0/16"]`, `[]`, 1),
		"private-not-allowed": strings.Replace(enterpriseFixture, `"allowPrivate":true`, `"allowPrivate":false`, 1),
		"static-host":         strings.Replace(enterpriseFixture, `"10.60.0.10"`, `"10.60.0.1"`, 1),
		"static-cluster":      strings.Replace(enterpriseFixture, `"10.60.0.10"`, `"172.30.0.10"`, 1),
		"trailing":            enterpriseFixture + `{}`,
		"bad-resolver":        strings.Replace(enterpriseFixture, `"targets"`, `"resolver":{"transport":"tcp","address":"dns.example.invalid:53"},"targets"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	p, err := Parse([]byte(enterpriseFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://model.example.invalid/v1", "https://MODEL.example.invalid.:0443/other"} {
		if err := p.CheckModelURL(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, raw := range []string{"https://other.example.invalid", "https://model.example.invalid:8443", "https://model.example.invalid.attacker.invalid", "https://model.example.invalid..", "https://model.example.invalid:", "https://model.example.invalid?", "https://model.example.invalid?key=secret", "https://model.example.invalid/#fragment", "https://user@model.example.invalid", "https://7.1.2.3", "https://模型.example.invalid"} {
		if err := p.CheckModelURL(context.Background(), raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	u, _ := url.Parse("https://model.example.invalid/package?signature=a%2Fb")
	if _, err := p.Authority(u); err != nil {
		t.Fatal("network policy rejected asset query")
	}
}

func TestDenyPrecedenceAndPerTargetRanges(t *testing.T) {
	p, err := Parse([]byte(enterpriseFixture))
	if err != nil {
		t.Fatal(err)
	}
	r := rule{private: true}
	for _, raw := range []string{"172.30.1.2", "10.60.0.1", "127.0.0.1", "::1", "169.254.169.254", "fe80::1", "::ffff:127.0.0.1", "198.18.0.1", "2001:2::1", "192.0.2.1", "64:ff9b::a00:1", "2002::1", "0.0.0.0", "224.0.0.1"} {
		if p.permits(r, netip.MustParseAddr(raw)) {
			t.Fatalf("denied address accepted %s", raw)
		}
	}
	for _, raw := range []string{"10.60.0.10", "fd12::1", "7.1.2.3", "8.8.8.8"} {
		if !p.permits(r, netip.MustParseAddr(raw)) {
			t.Fatal(raw)
		}
	}
	if p.permits(rule{}, netip.MustParseAddr("10.60.0.10")) {
		t.Fatal("private default allowed")
	}
	limited := p.targets["model.example.invalid:443"]
	if p.permits(limited, netip.MustParseAddr("10.61.0.10")) {
		t.Fatal("range leaked between targets")
	}
}

func TestSnapshotAndCanonicalDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(enterpriseFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := json.Unmarshal([]byte(enterpriseFixture), &parsed); err != nil {
		t.Fatal(err)
	}
	formatted, _ := json.MarshalIndent(parsed, "", "  ")
	q, err := Parse(formatted)
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest("unused-public-dns") != q.Digest(nil) {
		t.Fatal("format affected canonical digest")
	}
	if err := os.WriteFile(path, []byte(`{"schemaVersion":1,"mode":"enterprise","targets":[],"topology":{"clusterCidrs":["172.30.0.0/16"],"hostAddresses":["10.60.0.1"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckModelURL(context.Background(), "https://model.example.invalid"); err != nil {
		t.Fatal("running snapshot changed")
	}
	q, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(q.CheckModelURL(context.Background(), "https://model.example.invalid"), ErrForbidden) {
		t.Fatal("restarted snapshot did not deny")
	}
	if _, err = Load(path + "missing"); err == nil {
		t.Fatal("missing file fell back")
	}
	pub, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if pub.Digest("dns1") == pub.Digest("dns2") {
		t.Fatal("public DNS omitted from identity")
	}
}

func TestResolverAddressCannotBeBusinessDestination(t *testing.T) {
	for _, endpoint := range []string{"10.60.0.53:53", "[fd12::53]:53", "[::ffff:10.60.0.53]:53", "8.8.8.8:53"} {
		t.Run(endpoint, func(t *testing.T) {
			raw := strings.Replace(enterpriseFixture, `"targets"`, `"resolver":{"transport":"tcp","address":"`+endpoint+`"},"targets"`, 1)
			p, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			address := netip.MustParseAddrPort(endpoint).Addr().Unmap()
			if p.permits(rule{private: true}, address) || p.permits(rule{private: true}, netip.AddrFrom16(address.As16())) {
				t.Fatal("resolver address admitted as business destination")
			}
			raw = strings.Replace(raw, `"staticAddresses":["10.60.0.10"]`, `"staticAddresses":["`+address.String()+`"]`, 1)
			raw = strings.Replace(raw, `"allowedCidrs":["10.60.0.0/16"],`, "", 1)
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("resolver address accepted in static target")
			}
		})
	}
}

func TestStartupErrorsIdentifyFieldsWithoutValues(t *testing.T) {
	for _, test := range []struct{ name, raw, field string }{
		{"schema", strings.Replace(enterpriseFixture, `"schemaVersion":1`, `"schemaVersion":2`, 1), "schemaVersion"},
		{"type", strings.Replace(enterpriseFixture, `"port":443`, `"port":"private-value"`, 1), "targets[0].port"},
		{"unknown", strings.Replace(enterpriseFixture, `"schemaVersion":1`, `"schemaVersion":1,"private-value":true`, 1), "configuration"},
		{"duplicate", strings.Replace(enterpriseFixture, `"host":`, `"host":"private-value","host":`, 1), "targets[0].host"},
		{"cidrs", strings.Replace(enterpriseFixture, `["10.60.0.0/16"]`, `[]`, 1), "targets[0].allowedCidrs"},
		{"cidr-value", strings.Replace(enterpriseFixture, `10.60.0.0/16`, `private-value`, 1), "targets[0].allowedCidrs[0]"},
		{"static-value", strings.Replace(enterpriseFixture, `10.60.0.10`, `private-value`, 1), "targets[0].staticAddresses[0]"},
		{"resolver", strings.Replace(enterpriseFixture, `"targets"`, `"resolver":{"transport":"tcp","address":"private-value"},"targets"`, 1), "resolver.address"},
		{"topology", strings.Replace(enterpriseFixture, `172.30.0.0/16`, `private-value`, 1), "topology.clusterCidrs[0]"},
		{"null", strings.Replace(enterpriseFixture, `["10.60.0.0/16"]`, `null`, 1), "targets[0].allowedCidrs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("missing field %s in %v", test.field, err)
			}
			for _, value := range []string{"private-value", "10.60.0.10", "10.60.0.0/16", "Model.Example.Invalid"} {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("configuration value exposed in %v", err)
				}
			}
		})
	}
}
