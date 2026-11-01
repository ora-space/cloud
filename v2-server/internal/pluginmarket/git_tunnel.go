package pluginmarket

import (
	"context"
	"errors"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Git can make secondary requests (for example a dumb HTTP repository's alternates).
// Every CONNECT is restricted to the original host and the vetted IP; curl never gets
// a route to arbitrary hosts, even if an alternate or redirect asks for one.
func gitTunnel(ctx context.Context, host, address, socks string) (string, func(), error) {
	ln, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return "", nil, e
	}
	dial := (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	if socks != "" {
		u, e := url.Parse(socks)
		if e != nil {
			ln.Close()
			return "", nil, e
		}
		forward, e := proxy.SOCKS5("tcp", u.Host, nil, &net.Dialer{Timeout: 10 * time.Second})
		if e != nil {
			ln.Close()
			return "", nil, e
		}
		d, ok := forward.(proxy.ContextDialer)
		if !ok {
			ln.Close()
			return "", nil, errors.New("proxy does not support cancellation")
		}
		dial = d.DialContext
	}
	tunnelCtx, cancel := context.WithCancel(ctx)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || !strings.EqualFold(r.Host, net.JoinHostPort(host, "443")) {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		remote, e := dial(tunnelCtx, "tcp", address)
		if e != nil {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		defer remote.Close()
		client, buffer, e := http.NewResponseController(w).Hijack()
		if e != nil {
			return
		}
		defer client.Close()
		stop := context.AfterFunc(tunnelCtx, func() { remote.Close(); client.Close() })
		defer stop()
		if _, e = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); e != nil || buffer.Flush() != nil {
			return
		}
		go func() { _, _ = io.Copy(remote, buffer); remote.Close() }()
		_, _ = io.Copy(client, remote)
	})}
	go server.Serve(ln)
	return "http://" + ln.Addr().String(), func() { cancel(); _ = server.Close() }, nil
}
