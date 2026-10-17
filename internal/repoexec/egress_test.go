package repoexec

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// tableResolver answers from a fixed table.
type tableResolver map[string][]string

func (r tableResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	values, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []netip.Addr
	for _, v := range values {
		out = append(out, netip.MustParseAddr(v))
	}
	return out, nil
}

func TestEgressPolicy(t *testing.T) {
	resolver := tableResolver{
		"github.com":            {"140.82.112.3"},
		"rebind.example.com":    {"140.82.112.3", "10.0.0.7"},
		"internal.example.com":  {"192.168.1.10"},
		"cgnat.example.com":     {"100.64.0.1"},
		"v6.example.com":        {"2606:4700::1111"},
		"ula.example.com":       {"fd12:3456::1"},
		"mapped.example.com":    {"::ffff:127.0.0.1"},
		"git.corp.example.com":  {"140.82.112.4"},
		"fixture.example.com":   {"127.0.0.1"},
		"metadata.example.com":  {"169.254.169.254"},
		"multicast.example.com": {"224.0.0.1"},
	}
	e, err := NewEgress(resolver, []string{"127.0.0.1:8443", "fixture.example.com"}, []string{"corp.example.com", "ora-git-service"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		url   string
		allow bool
	}{
		{"https://github.com/org/repo", true},
		{"https://v6.example.com/org/repo", true},
		{"https://140.82.112.3/org/repo", true},
		{"http://github.com/org/repo", false},
		{"https://github.com:8443/org/repo", false},
		{"https://127.0.0.1/org/repo", false},
		{"https://[::1]/org/repo", false},
		{"https://10.1.2.3/org/repo", false},
		{"https://172.16.0.1/org/repo", false},
		{"https://169.254.169.254/latest", false},
		{"https://0.0.0.0/org/repo", false},
		{"https://internal.example.com/org/repo", false},
		{"https://rebind.example.com/org/repo", false},
		{"https://cgnat.example.com/org/repo", false},
		{"https://ula.example.com/org/repo", false},
		{"https://mapped.example.com/org/repo", false},
		{"https://metadata.example.com/org/repo", false},
		{"https://multicast.example.com/org/repo", false},
		{"https://git.corp.example.com/org/repo", false},
		{"https://ora-git-service:443/t/p.git", false},
		{"https://credential-store/x", false},
		{"https://localhost/x", false},
		{"https://unknown.example.com/x", false},
		// Allowlisted fixtures bypass the checks.
		{"https://127.0.0.1:8443/org/repo", true},
		{"https://fixture.example.com/org/repo", true},
		{"https://127.0.0.1:8444/org/repo", false},
	} {
		err := e.Check(context.Background(), c.url)
		if (err == nil) != c.allow {
			t.Errorf("Check(%s) = %v, want allowed=%v", c.url, err, c.allow)
		}
		if err != nil && !errors.Is(err, ErrEgressDenied) {
			t.Errorf("Check(%s) error %v is not ErrEgressDenied", c.url, err)
		}
	}
	// The probe's dialer applies the same policy at connect time.
	if _, err := e.DialContext(context.Background(), "tcp", "internal.example.com:443"); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("dial of an internal name = %v", err)
	}
}

// Without the allowlist, loopback fixtures, the Git Server's own address and internal names are
// refused before anything is redeemed.
func TestExecutorRefusesInternalAddresses(t *testing.T) {
	f := newFixture(t)
	repo, _ := f.ext.repository(f, "egress")
	strict, err := NewEgress(tableResolver{"git-server.example.com": {"10.0.0.5"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := f.executor(func(c *Config) { c.Egress = strict })
	for _, url := range []string{repo, f.gs.URL + "/" + f.tenant + "/" + f.project + ".git", "https://git-server.example.com/org/repo", "https://ora-git-service/org/repo"} {
		result := f.run(e, f.task(credential(f.syncAuthorization(url, "main", cloneauth.ZeroOID40)), time.Minute))
		if result.Classification != Failed || result.ErrorCode != CodeEgressDenied {
			t.Fatalf("sync of %s = %+v, want egress_denied", url, result)
		}
	}
	if f.redeemer.count() != 0 {
		t.Fatalf("a refused address redeemed %d times", f.redeemer.count())
	}
	if requests, _ := f.ext.counts(); requests != 0 {
		t.Fatalf("the loopback fixture received %d requests", requests)
	}
}
