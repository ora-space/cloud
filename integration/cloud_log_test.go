package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/wanglongan587/cloud/internal/api/router"
	"github.com/wanglongan587/cloud/internal/core"
)

// syncBuffer collects one Cloud instance's log output so a test can search the bytes it actually
// wrote. Handlers run on the server's own goroutines, so writes are serialized rather than racing on
// a bare bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rawAt is rawAs against a named Cloud instance rather than the fixture's own, so a test can put a
// second router — same store, same routes, one different logger — in front of the same request
// stream.
func (f *fixture) rawAt(base string, user core.Claims, method, path, key, body string) (int, string) {
	f.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, e := http.NewRequestWithContext(context.Background(), method, base+path, reader)
	must(f.t, e)
	req.Header.Set("Content-Type", "application/json")
	service, e := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	must(f.t, e)
	req.Header.Set("Authorization", "Bearer "+service)
	user.Caller = "gateway-a"
	token, e := f.client.Credentials.Token("user", user)
	must(f.t, e)
	req.Header.Set("X-Ora-User-Token", token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, e := http.DefaultClient.Do(req)
	must(f.t, e)
	defer res.Body.Close()
	raw, e := io.ReadAll(res.Body)
	must(f.t, e)
	return res.StatusCode, string(raw)
}

// TestCloudLogsCarryNoPartOfTheIngressToken is the log half of D7's secret boundary.
//
// Everything else that asserts "the token went nowhere" reads a store: a database column, a recorded
// response, the Credential Service's volume. Cloud's own log is the one destination those checks
// cannot see — it is written by the process, at its own level, to a stream no query reaches — and it
// is the destination an operator is most likely to pull into a ticket. This test captures what the
// real router writes, with the real boundary in front of it, and searches every byte.
//
// The stream it drives is chosen so that each of the router's ways of recording an ingress request is
// exercised while a token is in flight:
//
//   - a create and a rotation that hand the token over (the normal path, which writes no line at all);
//   - a refused body that still carried the token, refused at the router's field gate;
//   - D7's reconciliation warning, the one Warn line on this path;
//   - an infrastructure failure behind the ingress, the router's 500 branch, which logs the internal
//     error rather than a stable reason code — the case where whatever the lower layer put in an error
//     value would reach the log.
//
// The two lines the router must have written are asserted before the token is searched for. Without
// that, a capture that was empty or wired to nothing would satisfy "the token is absent" for the wrong
// reason, and the test would keep passing after the sink it depends on had been removed.
func TestCloudLogsCarryNoPartOfTheIngressToken(t *testing.T) {
	f := setup(t)
	secrets := &recordingSecrets{}
	f.store.Secrets = secrets

	capture := &syncBuffer{}
	logger := zap.New(zapcore.NewCore(zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(capture), zapcore.DebugLevel))
	auth, e := core.NewAuthenticator("ora-cloud", f.client.Credentials.Trust)
	must(t, e)
	server := httptest.NewServer(router.NewDevelopment(f.store, auth, logger))
	t.Cleanup(server.Close)

	status, raw := f.rawAt(server.URL, f.user, "POST", f.path("/git-credentials"), uuid.NewString(), credentialBody("work laptop", credentialScope, credentialToken))
	if status != 201 {
		t.Fatalf("create: want 201 got %d %s", status, raw)
	}
	id := decodeObject(t, raw).S("id")
	rotated := credentialToken + "-rotated"
	status, raw = f.rawAt(server.URL, f.user, "PUT", f.path("/git-credentials/"+id), uuid.NewString(), fmt.Sprintf(`{"token":%q,"version":1}`, rotated))
	if status != 200 {
		t.Fatalf("rotate: want 200 got %d %s", status, raw)
	}

	// Refused at the boundary, with the token already in the router's hands: the decoder read the
	// whole body, rejected the shape, and must not have kept any of it.
	status, raw = f.rawAt(server.URL, f.user, "POST", f.path("/git-credentials"), uuid.NewString(), fmt.Sprintf(`{"name":"work laptop","repositoryScope":%q,"token":%q,"tokenRef":"work laptop"}`, credentialScope, credentialToken))
	if code := decodeObject(t, raw).S("code"); status != 400 || code != "unknown_field" {
		t.Fatalf("a body with a field the route does not list must be refused at the boundary: %d %s", status, raw)
	}

	// Revocation commits and the service cannot confirm the material is gone. Cloud's state is
	// authoritative and already committed, so this is a success with residue — and the residue is the
	// router's own Warn line, which names the credential and nothing else.
	secrets.fail = errors.New("dial tcp 10.0.0.9:8443: connection refused")
	status, raw = f.rawAt(server.URL, f.user, "DELETE", f.path("/git-credentials/"+id), uuid.NewString(), `{"version":2}`)
	secrets.fail = nil
	if status != 200 {
		t.Fatalf("revoke: want 200 got %d %s", status, raw)
	}

	// The ingress path's only non-Fault failure is an infrastructure one. A pool that is already
	// closed fails before the request's first statement and touches nothing: it is a second handle on
	// the same schema, never the fixture's own, and the fixture's is restored on every path out.
	broken := stdlib.OpenDB(*f.pgConfig)
	must(t, broken.Close())
	live := f.store.Pool
	f.store.Pool = broken
	defer func() { f.store.Pool = live }()
	status, raw = f.rawAt(server.URL, f.user, "POST", f.path("/git-credentials"), uuid.NewString(), credentialBody("work laptop", credentialScope, credentialToken))
	if status != 500 {
		t.Fatalf("an unreachable database behind the ingress must be an internal error: %d %s", status, raw)
	}
	if code := decodeObject(t, raw).S("code"); code != "internal_error" {
		t.Fatalf("the caller must receive the stable code, never the driver's text: %s", raw)
	}

	text := capture.String()
	for _, want := range []string{"credential material remains after revocation", "credential ingress failed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the capture has no %q line, so it is not this router's output and the search below would pass for the wrong reason:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ghp_synthetic-") {
		t.Fatalf("Cloud's log carries part of the token it was handed:\n%s", text)
	}
}
