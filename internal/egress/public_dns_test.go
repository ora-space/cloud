package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSResolverObtainsBothFamiliesAndRetainsEveryAnswer(t *testing.T) {
	for _, test := range []struct {
		name string
		ipv6 string
	}{
		{"public", "2606:4700:4700::1111"},
		{"private", "fd00::1"},
		{"benchmark", "2001:2::b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			questions := make(chan dnsmessage.Type, 8)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || r.Header.Get("Authorization") != "" {
					t.Error("DNS request changed method/media or carried model authentication")
				}
				data, _ := io.ReadAll(r.Body)
				var request dnsmessage.Message
				if err := request.Unpack(data); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				question := request.Questions[0]
				questions <- question.Type
				var body dnsmessage.ResourceBody = &dnsmessage.AResource{A: [4]byte{8, 8, 8, 8}}
				if question.Type == dnsmessage.TypeAAAA {
					body = &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr(test.ipv6).As16()}
				}
				answer := dnsmessage.Message{Header: dnsmessage.Header{ID: request.ID, Response: true}, Questions: request.Questions, Answers: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: question.Class}, Body: body}}}
				packed, err := answer.Pack()
				if err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/dns-message")
				_, _ = w.Write(packed)
			}))
			t.Cleanup(server.Close)
			resolver := &DNSResolver{endpoint: server.URL, client: server.Client()}
			addresses, err := resolver.LookupNetIP(context.Background(), "ip", "models.example.invalid")
			want := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr(test.ipv6)}
			if err != nil || !reflect.DeepEqual(addresses, want) {
				t.Fatalf("resolver did not retain all answers: error=%v", err)
			}
			if got := []dnsmessage.Type{<-questions, <-questions}; !reflect.DeepEqual(got, []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}) {
				t.Fatal("resolver did not query both address families")
			}
		})
	}
}

func TestDNSResolverFailsClosedWithoutSystemFallback(t *testing.T) {
	for _, test := range []struct {
		name  string
		serve func(http.ResponseWriter, *http.Request)
	}{
		{"http-error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1/", http.StatusFound)
		}},
		{"invalid-wire", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/dns-message")
			_, _ = w.Write([]byte("private diagnostic"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(test.serve))
			t.Cleanup(server.Close)
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return ErrResolution }
			resolver := &DNSResolver{endpoint: server.URL, client: client}
			if _, err := resolver.LookupNetIP(context.Background(), "ip", "localhost"); !errors.Is(err, ErrResolution) {
				t.Fatalf("failed trusted lookup fell back to the system resolver: %v", err)
			}
		})
	}
}

func TestDNSResolverConfigurationCannotBootstrapThroughPrivateDNS(t *testing.T) {
	for _, config := range []DNSConfig{
		{HTTPSURL: "http://dns.example.invalid/query", BootstrapIPs: []string{"1.1.1.1"}},
		{HTTPSURL: "https://dns.example.invalid/query", BootstrapIPs: []string{"127.0.0.1"}},
		{HTTPSURL: "https://dns.example.invalid/query", BootstrapIPs: []string{"2001:2::b"}},
		{HTTPSURL: "https://dns.example.invalid/query", BootstrapIPs: []string{"dns.example.invalid"}},
		{HTTPSURL: "https://dns.example.invalid/query"},
	} {
		if _, err := NewDNSResolver(config); err == nil {
			t.Fatal("unsafe DNS bootstrap accepted")
		}
	}
	resolver, err := NewDNSResolver(DNSConfig{HTTPSURL: "https://dns.example.invalid/query", BootstrapIPs: []string{"1.1.1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(resolver.CloseIdleConnections)
	transport := resolver.client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("DNS can use an environmental proxy")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "other.example.invalid:443"); !errors.Is(err, ErrResolution) {
		t.Fatal("DNS transport dialed an undeclared target")
	}
}

func TestDeploymentDNSConfigReturnsMutableCopyOfDefaults(t *testing.T) {
	t.Setenv("MODEL_GATEWAY_DNS_HTTPS_URL", "")
	t.Setenv("MODEL_GATEWAY_DNS_BOOTSTRAP_IPS", "")
	first := DeploymentDNSConfig()
	first.BootstrapIPs[0] = "203.0.113.1"
	if again := DeploymentDNSConfig(); again.BootstrapIPs[0] != "1.1.1.1" {
		t.Fatal("deployment DNS configuration shares mutable default state")
	}
}
