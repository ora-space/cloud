package modelgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSConfig selects one authenticated RFC 8484 resolver with numeric bootstrap addresses.
// It is deployment configuration, never a per-user model setting. No system-DNS fallback exists.
type DNSConfig struct {
	HTTPSURL     string
	BootstrapIPs []string
}

type httpsResolver struct {
	endpoint string
	client   *http.Client
}

// newHTTPSResolver bootstraps HTTPS without asking the possibly intercepted system resolver.
// The request URL retains the resolver hostname for normal TLS certificate verification.
func newHTTPSResolver(config DNSConfig) (*httpsResolver, error) {
	u, err := url.Parse(config.HTTPSURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || len(config.BootstrapIPs) == 0 {
		return nil, errors.New("model DNS requires an HTTPS endpoint and public bootstrap IPs")
	}
	addresses := make([]netip.Addr, 0, len(config.BootstrapIPs))
	for _, raw := range config.BootstrapIPs {
		address, parseErr := netip.ParseAddr(raw)
		if parseErr != nil || !publicAddress(address) {
			return nil, errors.New("model DNS bootstrap must contain only public IPs")
		}
		addresses = append(addresses, address)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, IdleConnTimeout: time.Minute, MaxResponseHeaderBytes: 8 << 10}
	transport.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
		if target != net.JoinHostPort(u.Hostname(), port) {
			return nil, endpointResolutionFailed
		}
		for _, address := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, endpointResolutionFailed
	}
	return &httpsResolver{endpoint: u.String(), client: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return endpointResolutionFailed }}}, nil
}

// LookupNetIP obtains both families within one bounded budget; failure of either fails closed.
// Development fixture discovery is kept outside this resolver and cannot be a production fallback.
func (r *httpsResolver) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{address}, nil
	}
	if network != "ip" {
		return nil, endpointResolutionFailed
	}
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, endpointResolutionFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var addresses []netip.Addr
	for _, kind := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		answer, err := r.query(ctx, &dnsmessage.Question{Name: name, Type: kind, Class: dnsmessage.ClassINET})
		if err != nil {
			return nil, endpointResolutionFailed
		}
		addresses = append(addresses, answer...)
	}
	if len(addresses) == 0 {
		return nil, endpointResolutionFailed
	}
	return addresses, nil
}

// query consumes bounded wire-format DNS, excluding authority/additional addresses from dialing.
func (r *httpsResolver) query(ctx context.Context, question *dnsmessage.Question) ([]netip.Addr, error) {
	query := dnsmessage.Message{Header: dnsmessage.Header{RecursionDesired: true}, Questions: []dnsmessage.Question{*question}}
	body, err := query.Pack()
	if err != nil {
		return nil, endpointResolutionFailed
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, endpointResolutionFailed
	}
	request.Header.Set("Content-Type", "application/dns-message")
	request.Header.Set("Accept", "application/dns-message")
	response, err := r.client.Do(request)
	if err != nil {
		return nil, endpointResolutionFailed
	}
	defer response.Body.Close()
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || mediaErr != nil || media != "application/dns-message" {
		return nil, endpointResolutionFailed
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(data) > 64<<10 {
		return nil, endpointResolutionFailed
	}
	var answer dnsmessage.Message
	if err := answer.Unpack(data); err != nil || !answer.Response || answer.Truncated || answer.ID != query.ID || answer.RCode != dnsmessage.RCodeSuccess || len(answer.Questions) != 1 || answer.Questions[0] != *question {
		return nil, endpointResolutionFailed
	}
	var addresses []netip.Addr
	for index := range answer.Answers {
		resource := &answer.Answers[index]
		if resource.Header.Class != dnsmessage.ClassINET {
			return nil, endpointResolutionFailed
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.AResource:
			addresses = append(addresses, netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			addresses = append(addresses, netip.AddrFrom16(body.AAAA))
		}
	}
	return addresses, nil
}
