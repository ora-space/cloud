package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/modelgateway"
	"github.com/wanglongan587/cloud/internal/workflowasset"
)

// boundaryMode selects one served package behavior. The modes map one-to-one onto the
// approved fetch bounds: connection establishment, response headers, the whole-package
// deadline, the streaming size bound, and the repaired package for the retry.
type boundaryMode string

const (
	boundaryHoldTLS   boundaryMode = "hold-tls"
	boundaryNoHeaders boundaryMode = "no-headers"
	boundaryDribble   boundaryMode = "dribble"
	boundaryOversize  boundaryMode = "oversize"
	boundaryGood      boundaryMode = "good"
)

// boundaryOversizePlanned is far past the 16 MiB streaming bound plus every socket buffer
// involved, so the server can never finish writing it after the client stops reading.
const boundaryOversizePlanned int64 = 128 << 20

// testUpstreamIdentity is the CA and leaf the boundary server presents. The fetcher's
// transport is constructed with the CA in its root pool, so the boundary tests verify
// upstream TLS identity exactly as production does, without touching platform roots.
type testUpstreamIdentity struct {
	pool *x509.CertPool
	leaf tls.Certificate
}

func newUpstreamIdentity(t *testing.T) testUpstreamIdentity {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ora-asset-boundary-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	must(t, err)
	ca, err := x509.ParseCertificate(caDER)
	must(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	must(t, err)
	return testUpstreamIdentity{pool: pool, leaf: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}
}

// boundaryServer is the controlled HTTPS server the fetch-boundary tests point the catalog
// at. It owns one TCP port for its whole life so a catalog URL stays valid across the
// failure and the repaired retry. Every connection goes through the production transport's
// real checks: DNS is exempted only for the fixture host 127.0.0.1, TLS identity is
// verified against the generated CA, and the response behavior is exactly the selected
// mode. The mode switch is what models a publisher repairing a broken release.
type boundaryServer struct {
	tlsCfg *tls.Config
	ln     net.Listener
	mode   atomic.Value
	good   []byte

	// written and interrupted report what the oversize stream actually reached: the byte
	// count the socket accepted, and a one-time signal that a write failed before the
	// planned end. They are the server-side half of the "interrupted before the end"
	// evidence; the client-side half is the bounded failure code.
	written     atomic.Int64
	interrupted chan struct{}
	failOnce    sync.Once
}

func newBoundaryServer(t *testing.T, identity testUpstreamIdentity, good []byte, mode boundaryMode) *boundaryServer {
	t.Helper()
	s := &boundaryServer{
		tlsCfg:      &tls.Config{Certificates: []tls.Certificate{identity.leaf}},
		good:        good,
		interrupted: make(chan struct{}),
	}
	s.mode.Store(mode)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	s.ln = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/package.orax", s.serve)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn, mux)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *boundaryServer) url() string             { return "https://" + s.ln.Addr().String() + "/package.orax" }
func (s *boundaryServer) switchTo(m boundaryMode) { s.mode.Store(m) }

// handle routes one accepted connection. The hold-tls mode holds the raw connection
// without any TLS bytes, so the transport's 10-second connection deadline must cancel the
// fetch; every other mode completes the TLS handshake and serves real HTTP over it.
func (s *boundaryServer) handle(conn net.Conn, mux *http.ServeMux) {
	if s.mode.Load() == boundaryHoldTLS {
		// The client speaks first in TLS; drain its ClientHello and stay silent. The
		// deadline bounds the hold so a lost client cannot leak the goroutine.
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		return
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	_ = server.Serve(&singleConnListener{conn: tls.Server(conn, s.tlsCfg)})
}

// singleConnListener hands exactly one connection to http.Server.Serve. Each accepted
// connection gets its own server instance, so nothing is shared between connections.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() { conn = l.conn })
	if conn == nil {
		return nil, net.ErrClosed
	}
	return conn, nil
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func (s *boundaryServer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	switch s.mode.Load() {
	case boundaryNoHeaders:
		// ResponseHeaderTimeout must be what fires, so nothing is written: no status, no
		// headers. The wait ends when the cancelled client closes the connection.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Minute):
		}
	case boundaryDribble:
		// Headers arrive immediately; the body trickles far slower than the 60-second
		// whole-package deadline can consume, and never finishes inside it.
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if _, err := w.Write([]byte("x")); err != nil {
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	case boundaryOversize:
		// No Content-Length: the declared-size refusal cannot fire, so only the streaming
		// count can interrupt the download. The planned volume is far beyond every buffer
		// in the path, so a client that stops reading at the bound leaves the server
		// blocked and then failed before it can finish writing.
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		chunk := make([]byte, 64<<10)
		for s.written.Load() < boundaryOversizePlanned {
			n, err := w.Write(chunk)
			s.written.Add(int64(n))
			if err != nil {
				s.failOnce.Do(func() { close(s.interrupted) })
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(s.good)
	}
}

// realAssetFetcher wires the fetcher over the production upstream transport, not the test
// server's own client: the 10-second connection deadline, the 30-second response-header
// deadline and the checked dialer all come from modelgateway exactly as cmd/server wires
// them. The fixture-host exemption admits only 127.0.0.1, and the injected root pool keeps
// TLS verification real.
func realAssetFetcher(t *testing.T, identity testUpstreamIdentity) *workflowasset.Fetcher {
	t.Helper()
	client, err := modelgateway.NewUpstreamClient(modelgateway.UpstreamConfig{
		DevelopmentHosts: []string{"127.0.0.1"},
		DNS:              modelgateway.DefaultDNSConfig(),
		TLSRoots:         identity.pool,
	})
	must(t, err)
	t.Cleanup(client.CloseIdleConnections)
	return workflowasset.NewFetcher(nil, client.Transport)
}

// TestWorkflowAssetImportCancelsSlowOrOversizePackages covers the fetch-boundary row of
// specs test-cases/cloud/plugin-marketplace/workflow-plugin-assets.md: slow or oversized
// packages are cancelled by the real transport at the approved 10/30/60-second deadlines
// and the 16 MiB streaming bound, the failure is package-level (no document rows, no
// workflows), and the next administrator trigger retries the same URL and succeeds once
// the publisher's storage is repaired. Each case runs through the production transport
// chain; the subtests share nothing (own schema, marketplace and server) and run in
// parallel, so the test costs its longest boundary rather than their sum.
func TestWorkflowAssetImportCancelsSlowOrOversizePackages(t *testing.T) {
	cases := []struct {
		name string
		mode boundaryMode
		code string
		// min and max bracket the deadline that must have cancelled the fetch: the lower
		// bound proves no earlier failure fired, the upper bound (with generous CI slack)
		// distinguishes this deadline from the next-larger one.
		min, max time.Duration
	}{
		{"connection never established within ten seconds", boundaryHoldTLS, workflowasset.FailureUnavailable, 10 * time.Second, 30 * time.Second},
		{"response headers never arrive within thirty seconds", boundaryNoHeaders, workflowasset.FailureUnavailable, 30 * time.Second, 50 * time.Second},
		{"whole package never completes within sixty seconds", boundaryDribble, workflowasset.FailureUnavailable, 60 * time.Second, 80 * time.Second},
		{"streamed package interrupted past the sixteen MiB bound", boundaryOversize, workflowasset.FailureTooLarge, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := setup(t)
			// Two documents, so a package-level failure must leave neither: even the
			// document that would import fine proves the failure is not per-document.
			good := workflowPackage(t, map[string]string{
				"release.json": executableDocument("Release Kit", ""),
				"deploy.json":  executableDocument("Deploy Kit", ""),
			})
			repo := workflowMarketplace(t, f.root)
			identity := newUpstreamIdentity(t)
			server := newBoundaryServer(t, identity, good, c.mode)
			publishWorkflowRelease(t, repo, "slow-kit", "1.0.0", server.url(), good)
			f.syncMarketplace(t, repo)
			f.store.WorkflowAssets = realAssetFetcher(t, identity)

			sid := f.defaultSpaceID()
			f.installWorkflowPlugin(t, sid, "official/slow-kit", "")
			start := time.Now()
			err := f.importWorkflowAssets(t)
			if err == nil || !strings.Contains(err.Error(), c.code) {
				t.Fatalf("slow package must surface its bounded code %s, got %v", c.code, err)
			}
			elapsed := time.Since(start)
			if elapsed < c.min {
				t.Fatalf("the fetch was cancelled early at %s, before its %s deadline", elapsed, c.min)
			}
			if c.max > 0 && elapsed > c.max {
				t.Fatalf("the fetch took %s, past the slack for its %s deadline", elapsed, c.min)
			}
			item := f.assetImport(t, sid, "official/slow-kit")
			if item == nil || item.S("state") != "failed" || item.S("failureCode") != c.code {
				t.Fatalf("package failure = %v", item)
			}
			if n := f.scalar("SELECT count(*) FROM workflow_asset_documents"); n != 0 {
				t.Fatalf("a package failure must not record document outcomes: %d rows", n)
			}
			if n := f.scalar("SELECT count(*) FROM workflows"); n != 0 {
				t.Fatalf("a package failure must not create workflows: %d rows", n)
			}
			if n := f.scalar("SELECT count(*) FROM workflow_snapshots"); n != 0 {
				t.Fatalf("a package failure must not publish snapshots: %d rows", n)
			}
			if c.mode == boundaryOversize {
				// The client stopped reading past the bound; the server must observe the
				// interrupted stream once the closed socket refuses further writes.
				select {
				case <-server.interrupted:
				case <-time.After(10 * time.Second):
					t.Fatal("the server finished writing; the client did not interrupt the stream")
				}
				if written := server.written.Load(); written < (16<<20)+1 || written >= boundaryOversizePlanned {
					t.Fatalf("the server wrote %d bytes; the interruption must land past the bound and before the planned end", written)
				}
			}

			// The publisher repairs the same URL; the next administrator trigger retries
			// and succeeds against the identical catalog selection.
			server.switchTo(boundaryGood)
			f.installWorkflowPlugin(t, sid, "official/slow-kit", "1.0.0")
			must(t, f.importWorkflowAssets(t))
			item = f.assetImport(t, sid, "official/slow-kit")
			if item == nil || item.S("state") != "completed" || item.S("failureCode") != "" {
				t.Fatalf("retried package = %v", item)
			}
			documents := assetDocuments(item)
			if len(documents) != 2 {
				t.Fatalf("retried documents = %v", documents)
			}
			if live := f.liveWorkflows(t); len(live) != 2 || live["Release Kit"] == nil || live["Deploy Kit"] == nil {
				t.Fatalf("retried import did not create the workflows: %v", live)
			}
		})
	}
}
