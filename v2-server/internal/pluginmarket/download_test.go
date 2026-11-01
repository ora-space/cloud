package pluginmarket

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestPackageStatusErrorClassifiesRetries(t *testing.T) {
	for _, status := range []int{408, 425, 429, 500, 503} {
		if err := packageStatusError(status); !errors.Is(err, ErrTransient) {
			t.Fatalf("status %d = %v, want ErrTransient", status, err)
		}
	}
	for _, status := range []int{400, 401, 403, 404, 409} {
		if err := packageStatusError(status); !errors.Is(err, ErrPackage) {
			t.Fatalf("status %d = %v, want ErrPackage", status, err)
		}
	}
}

func TestDownloadPreservesTerminalRequestErrors(t *testing.T) {
	for _, terminal := range []error{ErrSource, ErrPackage} {
		if got := downloadRequestError(fmt.Errorf("request: %w", terminal)); !errors.Is(got, terminal) {
			t.Fatalf("downloadRequestError(%v) = %v, want %v", terminal, got, terminal)
		}
	}
	if got := downloadRequestError(errors.New("network")); !errors.Is(got, ErrTransient) {
		t.Fatalf("network error = %v, want ErrTransient", got)
	}
}

func TestDownloadProxyDialsPinnedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	seen := make(chan string, 1)
	proxyErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			proxyErr <- err
			return
		}
		defer conn.Close()
		header := make([]byte, 2)
		if _, err := io.ReadFull(conn, header); err != nil {
			proxyErr <- err
			return
		}
		methods := make([]byte, int(header[1]))
		if _, err := io.ReadFull(conn, methods); err != nil {
			proxyErr <- err
			return
		}
		if _, err := conn.Write([]byte{5, 0}); err != nil {
			proxyErr <- err
			return
		}
		request := make([]byte, 4)
		if _, err := io.ReadFull(conn, request); err != nil {
			proxyErr <- err
			return
		}
		if request[0] != 5 || request[1] != 1 || request[3] != 1 {
			proxyErr <- errors.New("proxy received an unsupported SOCKS5 request")
			return
		}
		address := make([]byte, 6)
		if _, err := io.ReadFull(conn, address); err != nil {
			proxyErr <- err
			return
		}
		target := net.JoinHostPort(net.IP(address[:4]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(address[4:]))))
		seen <- target
		if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			proxyErr <- err
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}()

	t.Setenv("V2_MARKET_SOCKS_PROXY", "socks5://"+listener.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialPublic(ctx, "tcp", "140.82.112.3:443", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	select {
	case target := <-seen:
		if target != "140.82.112.3:443" {
			t.Fatalf("proxy target = %q, want pinned address", target)
		}
	case err := <-proxyErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
