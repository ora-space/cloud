package pluginmarket

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"

	"testing"
	"time"
)

func TestGitTunnelRejectsAlternateHosts(t *testing.T) {
	raw, closeTunnel, e := gitTunnel(context.Background(), "source.example", "127.0.0.1:1", "")
	if e != nil {
		t.Fatal(e)
	}
	defer closeTunnel()
	u, _ := url.Parse(raw)
	c, e := net.DialTimeout("tcp", u.Host, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_, _ = c.Write([]byte("CONNECT alternate.example:443 HTTP/1.1\r\nHost: alternate.example:443\r\n\r\n"))
	r, e := http.ReadResponse(bufio.NewReader(c), nil)
	if e != nil || r.StatusCode != 403 {
		t.Fatal(r, e)
	}
	r.Body.Close()
}
