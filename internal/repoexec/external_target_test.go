package repoexec

import (
	"context"
	"errors"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
	"github.com/wanglongan587/cloud/internal/egress"
)

// tableResolver answers from a fixed table; literal addresses resolve to themselves, like the
// deployment resolver does.
type tableResolver map[string][]string

func (r tableResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
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

// sequenceResolver returns one prepared answer per call, so a test can prove that each external
// subprocess resolves again instead of reusing an earlier result.
type sequenceResolver struct {
	mu      sync.Mutex
	answers [][]string
}

func (r *sequenceResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.answers) == 0 {
		return nil, errors.New("no such host")
	}
	next := r.answers[0]
	r.answers = r.answers[1:]
	var out []netip.Addr
	for _, v := range next {
		out = append(out, netip.MustParseAddr(v))
	}
	return out, nil
}

func publicPolicy(t *testing.T) *egress.Policy {
	t.Helper()
	policy, err := egress.Parse([]byte(`{"schemaVersion":1,"mode":"public"}`))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func enterprisePolicy(t *testing.T) *egress.Policy {
	t.Helper()
	raw := `{"schemaVersion":1,"mode":"enterprise","targets":[{"host":"github.example.invalid","port":443,"staticAddresses":["140.82.112.3"]}],"topology":{"clusterCidrs":["172.30.0.0/16"],"hostAddresses":["10.60.0.1"]}}`
	policy, err := egress.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func testEgress(t *testing.T, policy *egress.Policy, mutate ...func(*EgressConfig)) *Egress {
	t.Helper()
	config := EgressConfig{Policy: policy, DNS: egress.DNSConfig{HTTPSURL: "https://dns.example.invalid/query", BootstrapIPs: []string{"1.1.1.1"}}}
	for _, m := range mutate {
		m(&config)
	}
	e, err := NewEgress(&config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func TestEgressPolicy(t *testing.T) {
	e := testEgress(t, publicPolicy(t))
	e.resolver = tableResolver{
		"github.com":            {"140.82.112.3"},
		"rebind.example.com":    {"140.82.112.3", "10.0.0.7"},
		"internal.example.com":  {"192.168.1.10"},
		"cgnat.example.com":     {"100.64.0.1"},
		"v6.example.com":        {"2606:4700::1111"},
		"ula.example.com":       {"fd12:3456::1"},
		"mapped.example.com":    {"::ffff:127.0.0.1"},
		"git.corp.example.com":  {"140.82.112.4"},
		"metadata.example.com":  {"169.254.169.254"},
		"multicast.example.com": {"224.0.0.1"},
	}
	for _, c := range []struct {
		url   string
		allow bool
	}{
		{"https://github.com/org/repo", true},
		{"https://github.com:8443/org/repo", true},
		{"https://v6.example.com/org/repo", true},
		{"https://140.82.112.3/org/repo", true},
		{"http://github.com/org/repo", false},
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
		{"https://ora-git-service:443/t/p.git", false},
		{"https://credential-store/x", false},
		{"https://localhost/x", false},
		{"https://unknown.example.com/x", false},
		{"https://github.com/org/repo?ref=main", false},
		{"https://user@github.com/org/repo", false},
	} {
		err := e.Check(context.Background(), c.url)
		if (err == nil) != c.allow {
			t.Errorf("Check(%s) = %v, want allowed=%v", c.url, err, c.allow)
		}
		if err != nil && !errors.Is(err, ErrEgressDenied) {
			t.Errorf("Check(%s) error %v is not ErrEgressDenied", c.url, err)
		}
	}
	// The probe's dialer re-applies the same policy at connect time.
	if _, err := e.DialContext(context.Background(), "tcp", "internal.example.com:443"); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("dial of an internal name = %v", err)
	}
}

// Every external Git subprocess resolves again: what one subprocess was given is pinned, and a name
// that later resolves into the cluster network is refused before the next process starts.
func TestEgressPinsCheckedAddressesPerSubprocess(t *testing.T) {
	e := testEgress(t, publicPolicy(t))
	sequence := &sequenceResolver{answers: [][]string{
		{"140.82.112.3"},
		{"172.30.4.5"},
	}}
	e.resolver = sequence
	ctx := context.Background()

	target, err := e.resolve(ctx, "https://git.example.com/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http.curloptResolve=git.example.com:443:140.82.112.3"}; !reflect.DeepEqual(target.pins(), want) {
		t.Fatalf("pins = %v, want %v", target.pins(), want)
	}
	if _, err := e.resolve(ctx, "https://git.example.com/org/repo"); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("second resolution into the cluster network = %v", err)
	}
}

func TestExternalTargetPinsBothFamilies(t *testing.T) {
	target := externalTarget{
		URL: "https://git.example.com/org/repo", host: "git.example.com", port: "443",
		addresses: []netip.Addr{netip.MustParseAddr("140.82.112.3"), netip.MustParseAddr("2606:4700:4700::1111")},
	}
	want := []string{"http.curloptResolve=git.example.com:443:140.82.112.3,[2606:4700:4700::1111]"}
	if got := target.pins(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pins = %v, want %v", got, want)
	}
}

// The pinned option belongs to one command only; the task's shared configuration stays unchanged.
func TestExternalGitCopiesConfiguration(t *testing.T) {
	base := &git{config: []string{"protocol.allow=never"}}
	run := &taskRun{git: base}
	target := externalTarget{host: "git.example.com", port: "443", addresses: []netip.Addr{netip.MustParseAddr("140.82.112.3")}}
	pinned := run.externalGit(target)
	if want := []string{"protocol.allow=never", "http.curloptResolve=git.example.com:443:140.82.112.3"}; !reflect.DeepEqual(pinned.config, want) {
		t.Fatalf("pinned config = %v, want %v", pinned.config, want)
	}
	if want := []string{"protocol.allow=never"}; !reflect.DeepEqual(base.config, want) {
		t.Fatalf("base config changed = %v", base.config)
	}
}

// The fixture exception is exact-host, public-mode and development-only.
func TestDevelopmentFixtureException(t *testing.T) {
	e := testEgress(t, publicPolicy(t), func(c *EgressConfig) {
		c.Development = true
		c.DevelopmentHosts = []string{"private-git-fixture"}
	})
	e.fixtures = tableResolver{"private-git-fixture": {"172.30.0.7"}}
	e.resolver = tableResolver{}
	target, err := e.resolve(context.Background(), "https://private-git-fixture:8443/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http.curloptResolve=private-git-fixture:8443:172.30.0.7"}; !reflect.DeepEqual(target.pins(), want) {
		t.Fatalf("pins = %v, want %v", target.pins(), want)
	}
	// The exception is exact: a sibling or parent name is not covered.
	if _, err := e.resolve(context.Background(), "https://push-git-fixture:8443/org/repo"); !errors.Is(err, ErrEgressDenied) {
		t.Fatalf("unlisted fixture host = %v", err)
	}
	// The internal peers stay refused even in development mode.
	if _, err := e.resolve(context.Background(), "https://ora-git-service/x"); err == nil {
		t.Fatal("internal peer accepted in development mode")
	}
	for _, config := range []EgressConfig{
		{Policy: publicPolicy(t), DNS: egress.DefaultDNSConfig(), Development: true, DevelopmentHosts: []string{"*.fixture"}},
		{Policy: publicPolicy(t), DNS: egress.DefaultDNSConfig(), Development: true, DevelopmentHosts: []string{"fixture:8443"}},
	} {
		if _, err := NewEgress(&config); err == nil {
			t.Fatalf("invalid development host accepted: %v", config.DevelopmentHosts)
		}
	}
}

func TestEnterpriseEgressUsesSharedTargets(t *testing.T) {
	e := testEgress(t, enterprisePolicy(t))
	target, err := e.resolve(context.Background(), "https://GitHub.Example.Invalid/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http.curloptResolve=github.example.invalid:443:140.82.112.3"}; !reflect.DeepEqual(target.pins(), want) {
		t.Fatalf("pins = %v, want %v", target.pins(), want)
	}
	for _, raw := range []string{
		"https://other.example.invalid/org/repo",
		"https://github.example.invalid:8443/org/repo",
		"https://140.82.112.3/org/repo",
		"https://github.example.invalid/org/repo?x=1",
	} {
		if _, err := e.resolve(context.Background(), raw); !errors.Is(err, ErrEgressDenied) {
			t.Errorf("enterprise policy accepted %s", raw)
		}
	}
	// The shared decision rejects the combination: an enterprise target list plus fixture hosts.
	if _, err := NewEgress(&EgressConfig{Policy: enterprisePolicy(t), Development: true, DevelopmentHosts: []string{"fixture.example.com"}}); err == nil {
		t.Fatal("enterprise policy accepted development fixture hosts")
	}
}

// Without the exception, loopback fixtures, the Git Server's own address and internal names are
// refused before anything is redeemed.
func TestExecutorRefusesInternalAddresses(t *testing.T) {
	f := newFixture(t)
	repo, _ := f.ext.repository(f, "egress")
	strict := testEgress(t, publicPolicy(t))
	strict.resolver = tableResolver{"git-server.example.com": {"10.0.0.5"}}
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
