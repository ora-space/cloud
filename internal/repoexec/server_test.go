package repoexec

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// blockingRunner holds every task until released.
type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingRunner) Run(ctx context.Context, _ *Task) Result {
	b.started <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return Result{Classification: Applied}
}

// taskListener serves handler with the executor's task TLS and returns clients for identities.
type taskListener struct {
	t          *testing.T
	srv        *httptest.Server
	dispatcher *pki
	serverCA   *pki
}

func newTaskListener(t *testing.T, handler http.Handler) *taskListener {
	t.Helper()
	l := &taskListener{t: t, dispatcher: newPKI(t), serverCA: newPKI(t)}
	server := l.serverCA.issue("executor-server", x509.ExtKeyUsageServerAuth)
	config, err := ServerTLS(server.cert, server.key, l.dispatcher.file)
	if err != nil {
		t.Fatal(err)
	}
	l.srv = httptest.NewUnstartedServer(handler)
	l.srv.TLS = config
	l.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	l.srv.StartTLS()
	t.Cleanup(l.srv.Close)
	return l
}

func (l *taskListener) client(uris ...string) *Client {
	l.t.Helper()
	id := l.dispatcher.issue("dispatcher", x509.ExtKeyUsageClientAuth, uris...)
	config, err := ClientTLS(id.cert, id.key, l.serverCA.file)
	if err != nil {
		l.t.Fatal(err)
	}
	c, err := NewClient(&ClientConfig{Endpoint: l.srv.URL, TLS: config})
	if err != nil {
		l.t.Fatal(err)
	}
	return c
}

func TestServerAcceptsOnlyTheDispatcher(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	close(runner.release)
	server, err := NewServer(runner, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := newTaskListener(t, server.Handler())
	task := &Task{Purpose: cloneauth.PurposeSync, TimeoutSeconds: 5}
	if result, err := l.client(DispatcherIdentity).Run(context.Background(), task); err != nil || result.Classification != Applied {
		t.Fatalf("dispatcher run = %+v, %v", result, err)
	}
	for name, uris := range map[string][]string{
		"another Cloud identity": {"spiffe://ora.local/cloud/credential-manager"},
		"no URI SAN":             nil,
		"two URI SANs":           {DispatcherIdentity, "spiffe://ora.local/cloud/other"},
	} {
		_, err := l.client(uris...).Run(context.Background(), task)
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Status != http.StatusForbidden {
			t.Errorf("%s: error %v, want 403 refusal", name, err)
		}
	}
}

// A full executor refuses with 429 executor_busy and the task does not start.
func TestServerRefusesWhenBusy(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}, 4), release: make(chan struct{})}
	server, err := NewServer(runner, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := newTaskListener(t, server.Handler())
	client := l.client(DispatcherIdentity)
	task := &Task{Purpose: cloneauth.PurposeSync, TimeoutSeconds: 5}
	done := make(chan error, 1)
	go func() {
		_, err := client.Run(context.Background(), task)
		done <- err
	}()
	<-runner.started
	_, err = client.Run(context.Background(), task)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Status != http.StatusTooManyRequests || refused.Code != CodeExecutorBusy {
		t.Fatalf("second task error = %v, want 429 executor_busy", err)
	}
	if len(runner.started) != 0 {
		t.Fatal("the refused task started")
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Malformed and oversized bodies are refused without running anything.
	body := strings.NewReader(`{"purpose":"sync","unknown":1}`)
	if status := l.post(body); status != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", status)
	}
	if status := l.post(strings.NewReader(`{"authorization":"` + strings.Repeat("a", maxTaskBody) + `"}`)); status != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d", status)
	}
}

func (l *taskListener) post(body io.Reader) int {
	l.t.Helper()
	id := l.dispatcher.issue("raw", x509.ExtKeyUsageClientAuth, DispatcherIdentity)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{id.pair}, RootCAs: l.serverCA.pool}}
	defer transport.CloseIdleConnections()
	response, err := (&http.Client{Transport: transport}).Post(l.srv.URL+"/v1/tasks", "application/json", body)
	if err != nil {
		l.t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// The client tells "never delivered" from "lost after sending".
func TestClientDeliveryErrors(t *testing.T) {
	task := &Task{Purpose: cloneauth.PurposeSync, TimeoutSeconds: 5}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	_ = closed.Close()
	l := newTaskListener(t, http.NotFoundHandler())
	unreachable := l.client(DispatcherIdentity)
	unreachable.endpoint = "https://" + address
	var notDelivered *NotDeliveredError
	if _, err := unreachable.Run(context.Background(), task); !errors.As(err, &notDelivered) {
		t.Fatalf("dial failure = %v, want NotDeliveredError", err)
	}

	cut := newTaskListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	var lost *LostError
	if _, err := cut.client(DispatcherIdentity).Run(context.Background(), task); !errors.As(err, &lost) {
		t.Fatalf("connection cut after sending = %v, want LostError", err)
	}

	// The caller giving up after the request was sent is a lost result too, never "not delivered".
	runner := &blockingRunner{started: make(chan struct{}, 1), release: make(chan struct{})}
	server, err := NewServer(runner, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	slow := newTaskListener(t, server.Handler())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-runner.started
		cancel()
	}()
	if _, err := slow.client(DispatcherIdentity).Run(ctx, task); !errors.As(err, &lost) {
		t.Fatalf("cancelled after sending = %v, want LostError", err)
	}
}

// End to end: Cloud's client, the task listener and the real executor, over the wire format.
func TestClientServerExecutorSync(t *testing.T) {
	f := newFixture(t)
	repo, path := f.ext.repository(f, "wire")
	a := f.commit("one")
	f.plain(f.work, "push", "--quiet", path, "HEAD:refs/heads/main")
	server, err := NewServer(f.executor(), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	l := newTaskListener(t, server.Handler())
	result, err := l.client(DispatcherIdentity).Run(context.Background(), f.task(f.syncAuthorization(repo, "main", cloneauth.ZeroOID40), time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	f.checkAfter(&result)
	if result.Classification != Applied || result.CommitOID != a || result.TrackingRef == "" {
		t.Fatalf("result = %+v", result)
	}
}
