package repoexec

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: test fixture on loopback; Go >= 1.6.3 drops the Proxy header.
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Fixture modes of the external server (the ADR's push-git-fixture modes).
const (
	modeNormal   = ""
	modeMoved    = "moved"     // the target moves when receive-pack is advertised, before the push
	modeDrop     = "drop"      // the push is applied but the response is cut
	modeHang     = "hang"      // the push request never answers
	modeHangRead = "hang-read" // fetches never answer
	modeRedirect = "redirect"  // every request is redirected
)

// externalServer is an external Git host over TLS: git http-backend through net/http/cgi, optional
// basic authentication, and fault modes. It records what reached it so tests can prove what the
// executor did and did not send.
type externalServer struct {
	t      *testing.T
	root   string
	srv    *httptest.Server
	caFile string
	cgi    *cgi.Handler

	mu        sync.Mutex
	mode      string
	username  string
	token     string
	redirect  string
	onMove    func()
	denyPush  bool
	requests  int
	receive   int
	headers   int
	certs     int
	hangs     int
	hangEnded int
	closing   chan struct{}
}

func newExternalServer(t *testing.T) *externalServer {
	t.Helper()
	p := newPKI(t)
	server := p.issue("external", x509.ExtKeyUsageServerAuth)
	x := &externalServer{t: t, root: t.TempDir(), caFile: p.file, closing: make(chan struct{})}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	x.cgi = &cgi.Handler{Path: git, Args: []string{"http-backend"}, Root: "/", Stderr: io.Discard, Logger: log.New(io.Discard, "", 0), Env: []string{
		"GIT_PROJECT_ROOT=" + x.root, "GIT_HTTP_EXPORT_ALL=1", "PATH=" + os.Getenv("PATH"), "HOME=" + x.root,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
	}}
	x.srv = httptest.NewUnstartedServer(x)
	x.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{server.pair}, ClientAuth: tls.RequestClientCert}
	x.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	x.srv.StartTLS()
	t.Cleanup(func() {
		close(x.closing)
		x.srv.Close()
	})
	return x
}

func (x *externalServer) hostPort() string {
	u, _ := url.Parse(x.srv.URL)
	return u.Host
}

// repository creates a bare repository and returns its normalized URL and path on disk.
func (x *externalServer) repository(f *fixture, name string) (string, string) {
	path := filepath.Join(x.root, "org", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		x.t.Fatal(err)
	}
	f.plain(filepath.Dir(path), "init", "--quiet", "--bare", path)
	return x.srv.URL + "/org/" + name, path
}

// requireToken makes every request need basic authentication with exactly this pair.
func (x *externalServer) requireToken(username, token string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.username, x.token = username, token
}

func (x *externalServer) setMode(mode string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.mode = mode
}

// counts returns (all requests, receive-pack requests).
func (x *externalServer) counts() (int, int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.requests, x.receive
}

func (x *externalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	receive := r.URL.Query().Get("service") == "git-receive-pack" || strings.HasSuffix(r.URL.Path, "/git-receive-pack")
	upload := strings.HasSuffix(r.URL.Path, "/git-upload-pack")
	x.mu.Lock()
	x.requests++
	if receive {
		x.receive++
	}
	if len(r.Header.Values("Ora-Git-Operation-Id")) > 0 {
		x.headers++
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		x.certs++
	}
	mode, username, token, redirect := x.mode, x.username, x.token, x.redirect
	move, denyPush := x.onMove, x.denyPush
	x.mu.Unlock()

	if mode == modeRedirect {
		http.Redirect(w, r, redirect+r.URL.RequestURI(), http.StatusFound)
		return
	}
	if token != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != username || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if denyPush && receive {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch {
	case mode == modeMoved && receive && r.Method == http.MethodGet && move != nil:
		move()
	case mode == modeHang && receive && r.Method == http.MethodPost, mode == modeHangRead && upload:
		x.hang(r)
		return
	case mode == modeDrop && receive && r.Method == http.MethodPost:
		// Apply the push in full, then cut the connection before any byte of the answer.
		x.cgi.ServeHTTP(httptest.NewRecorder(), r)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	x.cgi.ServeHTTP(w, r)
}

// hang holds the request until the client goes away (its process group was killed) or the fixture
// closes, and records that the client went away.
func (x *externalServer) hang(r *http.Request) {
	// The server only notices a vanished client once the request body is consumed.
	_, _ = io.Copy(io.Discard, r.Body)
	x.mu.Lock()
	x.hangs++
	x.mu.Unlock()
	select {
	case <-r.Context().Done():
		x.mu.Lock()
		x.hangEnded++
		x.mu.Unlock()
	case <-x.closing:
	}
}

// refuse installs a pre-receive hook in repository path that prints message and declines.
func (x *externalServer) refuse(path, message string) {
	hook := filepath.Join(path, "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o750); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ncat >/dev/null\necho '"+message+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // G306: git must execute the hook.
		x.t.Fatal(err)
	}
}

// scoping returns (requests, receive-pack requests, Operation headers seen, client certificates seen).
func (x *externalServer) scoping() (int, int, int, int) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.requests, x.receive, x.headers, x.certs
}

// hangCounts returns how many requests hung and how many of them ended because the client went
// away. The server notices a closed connection asynchronously, so it waits briefly for the two to
// agree.
func (x *externalServer) hangCounts() (int, int) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		x.mu.Lock()
		hangs, ended := x.hangs, x.hangEnded
		x.mu.Unlock()
		if hangs == ended || time.Now().After(deadline) {
			return hangs, ended
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// moveOnPush makes the moved mode run move when receive-pack is advertised.
func (x *externalServer) moveOnPush(move func()) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.onMove, x.mode = move, modeMoved
}

// denyPushes answers every receive-pack request with 403: a read-only credential.
func (x *externalServer) denyPushes() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.denyPush = true
}
