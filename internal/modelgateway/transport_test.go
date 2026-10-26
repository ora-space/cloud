package modelgateway

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/egress"
)

func TestUpstreamTimeoutConfiguration(t *testing.T) {
	for _, c := range []struct {
		name             string
		connect, headers time.Duration
		wantConnect      time.Duration
		wantHeaders      time.Duration
	}{
		{"production defaults", 0, 0, 10 * time.Second, 30 * time.Second},
		{"nonpositive retains bounds", -time.Second, -time.Second, 10 * time.Second, 30 * time.Second},
		{"injected limits", 50 * time.Millisecond, 150 * time.Millisecond, 50 * time.Millisecond, 150 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, err := NewUpstreamClientWithTimeouts(UpstreamConfig{DNS: egress.DefaultDNSConfig()}, nil,
				UpstreamTimeouts{Connect: c.connect, ResponseHeader: c.headers})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.CloseIdleConnections)
			transport := client.Transport.(*upstreamTransport)
			if transport.TLSHandshakeTimeout != c.wantConnect || transport.ResponseHeaderTimeout != c.wantHeaders {
				t.Fatalf("timeouts = %s/%s, want %s/%s", transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout, c.wantConnect, c.wantHeaders)
			}
		})
	}
}

type fixedResolver []netip.Addr

func (r fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}

func TestAddressDialBudgetSharesTheRemainingBudgetWithFloor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		addresses int
		want      time.Duration
	}{
		{"single address keeps the whole budget", 10 * time.Second, 1, 10 * time.Second},
		{"two addresses split evenly", 10 * time.Second, 2, 5 * time.Second},
		{"four addresses split evenly", 10 * time.Second, 4, 2500 * time.Millisecond},
		{"floor applies to a small share", 10 * time.Second, 5, minAddressDialBudget},
		{"floor applies below one second", 10 * time.Second, 10, minAddressDialBudget},
		{"expired budget keeps the floor for the parent to clamp", 0, 3, minAddressDialBudget},
		{"negative budget keeps the floor", -time.Second, 2, minAddressDialBudget},
		{"no remaining addresses stays bounded", 6 * time.Second, 0, minAddressDialBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := addressDialBudget(tc.remaining, tc.addresses); got != tc.want {
				t.Fatalf("budget = %s, want %s", got, tc.want)
			}
		})
	}
}

// fixtureDialer returns a checked dialer whose dialer reports synthetic outcomes. The fixture-host
// exemption keeps the public-address gate out of these budget tests, as in the local-listener tests.
func fixtureDialer(resolver fixedResolver, timeout time.Duration, dial func(context.Context, string) (net.Conn, error)) *secureDialer {
	return &secureDialer{
		resolver: resolver,
		dialer:   net.Dialer{Timeout: timeout},
		allowed:  map[string]struct{}{"fixture.invalid": {}},
		dialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dial(ctx, address)
		},
	}
}

func TestDialReachesLaterAddressAfterAnUnresponsiveFirstAddress(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	var attempts []string
	d := fixtureDialer(fixedResolver{netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")}, 4*time.Second,
		func(ctx context.Context, address string) (net.Conn, error) {
			attempts = append(attempts, address)
			if len(attempts) == 1 {
				// The first edge accepts the attempt and never answers.
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return left, nil
		})
	start := time.Now()
	conn, err := d.dial(context.Background(), "tcp", "fixture.invalid:443")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("second address was not reached: %v", err)
	}
	defer conn.Close()
	if want := []string{"198.51.100.1:443", "198.51.100.2:443"}; strings.Join(attempts, ",") != strings.Join(want, ",") {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
	if elapsed < minAddressDialBudget || elapsed >= 4*time.Second {
		t.Fatalf("elapsed = %s, want the first address cut at its share inside the whole budget", elapsed)
	}
}

func TestDialListsEveryAddressWhenTheWholeBudgetExpires(t *testing.T) {
	var attempts []string
	d := fixtureDialer(fixedResolver{netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2"), netip.MustParseAddr("198.51.100.3")}, 100*time.Millisecond,
		func(ctx context.Context, address string) (net.Conn, error) {
			attempts = append(attempts, address)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	start := time.Now()
	_, err := d.dial(context.Background(), "tcp", "fixture.invalid:443")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("all-blackhole dial succeeded")
	}
	// The whole-dial deadline must cut the first attempt, not the two-second address floor.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("elapsed = %s, want the whole-dial budget to bound the attempts", elapsed)
	}
	var failure *dialFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want the checked-dial failure", err)
	}
	if len(failure.attempts) != 3 || len(attempts) != 1 {
		t.Fatalf("attempts = %d recorded, %d labeled; want one tried and every address reported", len(attempts), len(failure.attempts))
	}
	if failure.attempts[0].reason != "timeout" {
		t.Fatalf("first reason = %q, want timeout", failure.attempts[0].reason)
	}
	for _, attempt := range failure.attempts[1:] {
		if attempt.reason != "not attempted" {
			t.Fatalf("unattempted address reason = %q, want not attempted", attempt.reason)
		}
	}
	if code := upstreamFailureCode(err); code != "model_upstream_timeout" {
		t.Fatalf("code = %s, want the whole-budget timeout", code)
	}
	for _, address := range []string{"198.51.100.1:443", "198.51.100.2:443", "198.51.100.3:443"} {
		if !strings.Contains(err.Error(), address) {
			t.Fatalf("error %q does not list %s", err, address)
		}
	}
}

func TestDialKeepsEachAddressReasonWhenEveryAddressFails(t *testing.T) {
	outcomes := []error{context.DeadlineExceeded, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}
	index := 0
	d := fixtureDialer(fixedResolver{netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")}, 10*time.Second,
		func(context.Context, string) (net.Conn, error) {
			outcome := outcomes[index]
			index++
			return nil, outcome
		})
	_, err := d.dial(context.Background(), "tcp", "fixture.invalid:443")
	var failure *dialFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want the checked-dial failure", err)
	}
	if want := []string{"198.51.100.1:443 timeout", "198.51.100.2:443 connection refused"}; failure.DialFailureDetail() != "checked dial failed: "+strings.Join(want, "; ") {
		t.Fatalf("detail = %q", failure.DialFailureDetail())
	}
	if code := upstreamFailureCode(err); code != "model_upstream_unavailable" {
		t.Fatalf("code = %s, want the stable unavailable category", code)
	}
}

func TestModelEndpointPolicyRejectsPrivateReservedAndMixedDNS(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "198.18.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "2001:db8::1", "2001:2::b", "2001:2:0:ffff:ffff:ffff:ffff:ffff", "64:ff9b::a00:1"} {
		if publicAddress(netip.MustParseAddr(value)) {
			t.Errorf("forbidden address accepted: %s", value)
		}
	}
	for _, value := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicAddress(netip.MustParseAddr(value)) {
			t.Errorf("public address rejected: %s", value)
		}
	}
	d := secureDialer{resolver: fixedResolver{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}}
	if _, err := d.dial(context.Background(), "tcp", "models.example.invalid:443"); err == nil {
		t.Fatal("mixed DNS answers reached a dial")
	}
}

func TestProtocolEndpointJoiningPreservesBluezonePrefixes(t *testing.T) {
	for _, test := range []struct{ base, protocol, want string }{{"https://models.example.invalid/v1/", "openai-completions", "https://models.example.invalid/v1/chat/completions"}, {"https://models.example.invalid/compatible", "anthropic-messages", "https://models.example.invalid/compatible/v1/messages"}, {"https://models.example.invalid/v1", "anthropic-messages", "https://models.example.invalid/v1/messages"}} {
		got, err := upstreamURL(test.base, test.protocol)
		if err != nil || got != test.want {
			t.Fatalf("endpoint %s expected %s", got, test.want)
		}
	}
	for _, base := range []string{"http://example.invalid", "https://user:password@example.invalid", "https://example.invalid?key=value", "https://example.invalid#fragment"} {
		if _, err := upstreamURL(base, "openai-completions"); err == nil {
			t.Fatal("unsafe model endpoint accepted")
		}
	}
}

func TestMalformedCredentialPathsCannotFallThroughToCloud(t *testing.T) {
	for _, path := range []string{"/api/v1/me/model-connections/not-a-uuid/credential", "/api/v1/me/model-connections/a/b/credential", "/api/v1/me/model-connections/a/credential/"} {
		if !IsCredentialPath(path) {
			t.Fatal("credential namespace fell through to Cloud")
		}
	}
	if IsCredentialPath("/api/v1/me/model-connections") {
		t.Fatal("metadata route treated as credential write")
	}
}
