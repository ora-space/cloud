package gitserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// This file is the read-only mirror surface: the second listener this process serves, and the one a
// Workspace's Node actually clones from.
//
// It is a separate surface from the repository listener in `service.go`, and the separation is the
// point rather than an accident of history. The two answer different callers with different evidence:
//
//   - the repository listener serves the git-access protocol, where the caller presents an mTLS
//     client certificate that names a runtime target, and where writes are possible;
//   - this surface serves a published mirror read, where the caller presents nothing at the
//     transport layer at all and is identified from the address its connection arrived from.
//
// A Node cannot present a client certificate — `desktop/apps/ora-node`'s own clone policy permits
// exactly one `[http]` key, `sslCAInfo`, and no client identity exists for it anywhere in the
// deployment — so folding this surface into the repository listener would leave the read path
// unreachable, and relaxing that listener's `RequireAndVerifyClientCert` would take the transport's
// guarantee away from every route on it. The two therefore have their own listeners, their own TLS
// configurations and their own handlers, and neither handler can reach the other's routes.
//
// What this surface serves is one thing: Git's smart HTTP read protocol out of a mirror store. No
// receive-pack, no archive, no dumb protocol, no other path. It is closed by default — a request is
// served only when the caller's container identity resolves *and* Cloud's state, read for that one
// request, authorizes that container to read that mirror at that generation. Every refusal is the
// same response with the same body, so a caller cannot use the server to learn which mirrors exist
// (D2, D5).
//
// This process holds no upstream credential and reaches no network other than Cloud's state reader
// and the origin resolver. The commit it serves was fetched by the credentialed executor, which is
// the only component that ever held the credential (D1).

// ErrOriginUnresolvable reports that the connection's peer address belongs to no live sandbox
// container. It is a refusal: the caller is not a workspace.
var ErrOriginUnresolvable = errors.New("the caller is not a resolvable sandbox container")

// ErrOriginUnavailable reports that the origin resolver could not answer. It is deliberately distinct
// from ErrOriginUnresolvable: "this is not a workspace" is a decision, and "I cannot tell" must not
// be turned into one.
var ErrOriginUnavailable = errors.New("the origin resolver is unavailable")

// OriginResolver answers which sandbox container a peer address belongs to. It is the only source of
// caller identity on this surface, and it must resolve per request from deployment-owned state — an
// implementation that caches, or that accepts an address from anywhere but the connection, defeats
// the whole design.
type OriginResolver interface {
	Resolve(ctx context.Context, peer netip.Addr) (gitread.Origin, error)
}

// StateReader answers Cloud's authoritative state for one read request. An error means Cloud could
// not be read, which the server answers as unavailability rather than as a refusal.
type StateReader interface {
	Read(ctx context.Context, origin gitread.Origin, mirrorDigest string) (gitread.Snapshot, error)
}

// MirrorOptions wires the mirror surface. Every dependency is required: a mirror surface without an
// origin resolver or without a state reader has no way to authorize anything, and starting one anyway
// is how a fail-closed service becomes a fail-open one.
//
// There is deliberately no repository root, no git program and no logger here. Those three are the
// service's, shared with the repository surface, so one process runs one git binary and writes one
// log; a deployment cannot configure the two surfaces into disagreeing about either.
type MirrorOptions struct {
	// Mirrors is the mirror store the surface reads content from.
	Mirrors *mirror.Store
	// Origins resolves the connection's peer to a sandbox container identity.
	Origins OriginResolver
	// State reads Cloud's authoritative state for each request.
	State StateReader
	// Path is the PATH given to the git process. It is fixed rather than inherited so the process
	// cannot pick up a different git from the caller's environment.
	Path string
	// MaxRequestBytes bounds a fetch negotiation body.
	MaxRequestBytes int64
	// Timeout bounds one git invocation. A clone that needs longer is a clone that can be retried.
	Timeout time.Duration
}

// mirrorRead is the mirror surface of a Service. It owns the routes and the child processes' HOME;
// its lifetime is the Service's, which is why it has no Close of its own.
type mirrorRead struct {
	cfg  MirrorOptions
	git  string
	home string
	log  *zap.Logger
	mux  *http.ServeMux
}

// newMirrorRead builds the surface from a configuration snapshot, the service's resolved git program
// and a HOME directory the service owns. Taking the last two as arguments rather than as fields is
// what makes "one binary, one HOME, one lifecycle" structural: there is no way to construct this
// surface that gives it a git of its own.
func newMirrorRead(o MirrorOptions, git, home string, log *zap.Logger) (*mirrorRead, error) {
	if o.Mirrors == nil {
		return nil, errors.New("the mirror surface requires a mirror store")
	}
	if o.Origins == nil {
		return nil, errors.New("the mirror surface requires an origin resolver")
	}
	if o.State == nil {
		return nil, errors.New("the mirror surface requires a state reader")
	}
	if git == "" {
		return nil, errors.New("the mirror surface requires the git program resolved by the service")
	}
	if home == "" {
		return nil, errors.New("the mirror surface requires the home directory owned by the service")
	}
	if o.Path == "" {
		o.Path = "/usr/bin:/bin"
	}
	if o.MaxRequestBytes <= 0 {
		o.MaxRequestBytes = 1 << 22
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	if log == nil {
		return nil, errors.New("the mirror surface requires the service's logger")
	}
	m := &mirrorRead{cfg: o, git: git, home: home, log: log}
	m.mux = m.routes()
	return m, nil
}

// ServeHTTP is this surface's whole HTTP surface. It is mounted on its own listener, and it is the
// mux's routes and nothing else: the repository routes are not reachable through it, so a request
// that arrives here cannot be answered by the other surface's handler however it is spelled.
func (m *mirrorRead) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// routes maps the two paths the smart HTTP read protocol uses. There is deliberately no catch-all:
// a path this mux does not name is a 404 from the standard library, not a refusal this code decides.
func (m *mirrorRead) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mirror.RoutePrefix+"{name}/info/refs", m.serveInfoRefs)
	mux.HandleFunc("POST "+mirror.RoutePrefix+"{name}/git-upload-pack", m.serveUploadPack)
	return mux
}

func (m *mirrorRead) serveInfoRefs(w http.ResponseWriter, r *http.Request) {
	digest, ok := mirrorDigest(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The service is what the client says it wants to do. It is parsed rather than trusted: a write is
	// refused before anything else happens.
	operation := gitread.ParseService(r.URL.Query().Get("service"))
	m.serve(w, r, digest, operation, m.writeAdvertisement)
}

func (m *mirrorRead) serveUploadPack(w http.ResponseWriter, r *http.Request) {
	digest, ok := mirrorDigest(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	m.serve(w, r, digest, gitread.ParseRPCPath("git-upload-pack"), m.runUploadPack)
}

// serve is the one path every request takes: identify, authorize, open, then produce a response.
func (m *mirrorRead) serve(w http.ResponseWriter, r *http.Request, digest string, operation gitread.Operation, produce func(*mirror.Handle, http.ResponseWriter, *http.Request)) {
	if !operation.ReadOnly() {
		// A write, an archive request or an unknown service is refused without consulting anything:
		// there is nothing to consult, because the service is not implemented at all. This is what
		// keeps the mirror listener from being a way in to the repository surface's write path —
		// receive-pack is refused here before any authorization is even asked for.
		m.refuse(w, digest, gitread.ReasonOperationNotServed)
		return
	}
	peer, err := peerAddress(r)
	if err != nil {
		m.refuse(w, digest, "peer_not_usable")
		return
	}
	origin, err := m.cfg.Origins.Resolve(r.Context(), peer)
	if err != nil {
		switch {
		case errors.Is(err, ErrOriginUnresolvable):
			m.refuse(w, digest, "origin_unresolvable")
		case errors.Is(err, ErrOriginUnavailable):
			m.unavailable(w, r, digest, "origin_resolver_unavailable")
		default:
			m.unavailable(w, r, digest, "origin_resolver_failed")
		}
		return
	}
	snapshot, err := m.cfg.State.Read(r.Context(), origin, digest)
	if err != nil {
		m.unavailable(w, r, digest, "state_unavailable")
		return
	}
	verdict := (gitread.Request{Origin: origin, MirrorDigest: digest, Operation: operation}).Evaluate(snapshot)
	if !verdict.Allow {
		m.refuse(w, digest, verdict.Reason)
		return
	}
	handle, err := m.cfg.Mirrors.Open(verdict.Key)
	if err != nil {
		switch {
		case errors.Is(err, mirror.ErrNotReady):
			// Cloud authorized a mirror the store does not consider readable. The two disagree, and the
			// store is the one holding the bytes, so the read is refused rather than improvised.
			m.refuse(w, digest, "mirror_not_readable")
		default:
			m.unavailable(w, r, digest, "mirror_store_failed")
		}
		return
	}
	if handle.Record.Generation != verdict.Generation {
		// The mirror was republished between the authorization and the read. Serving the newer
		// generation would mean serving content nobody authorized, so the read is refused and the
		// caller retries, which re-authorizes.
		m.refuse(w, digest, gitread.ReasonGenerationChanged)
		return
	}
	produce(&handle, w, r)
}

// writeAdvertisement emits the smart HTTP ref advertisement for a read.
func (m *mirrorRead) writeAdvertisement(handle *mirror.Handle, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, pktLine("# service=git-upload-pack\n")+"0000"); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), m.cfg.Timeout)
	defer cancel()
	// --advertise-refs writes the ref advertisement and exits; the body is small and bounded by the
	// mirror's single ref.
	if err := m.runGit(ctx, handle.Dir, gitProtocol(r.Header.Get("Git-Protocol")), w, "--stateless-rpc", "--strict", "--advertise-refs"); err != nil {
		m.log.Warn("git upload-pack --advertise-refs failed", zap.String("mirror", handle.Record.Key.String()), zap.Error(err))
	}
}

// runUploadPack proxies one fetch negotiation to git upload-pack.
func (m *mirrorRead) runUploadPack(handle *mirror.Handle, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), m.cfg.Timeout)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, m.cfg.MaxRequestBytes)
	// The status line cannot be un-sent, so the first byte of git's answer is read before any header
	// is written: a git that fails immediately becomes a 500 the client can act on, instead of a 200
	// with an empty body that looks like a protocol error.
	stream, err := m.startGit(ctx, handle.Dir, gitProtocol(r.Header.Get("Git-Protocol")), body, "--stateless-rpc", "--strict")
	if err != nil {
		m.log.Warn("git upload-pack did not start", zap.String("mirror", handle.Record.Key.String()), zap.Error(err))
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	defer func() { _ = stream.Close() }()
	// Peek does not consume, so the byte it returns is still the first byte the copy below writes.
	// Peeking is only how a child that failed before answering becomes a 500 instead of a 200 with a
	// truncated protocol stream in it.
	if _, err := stream.Peek(1); err != nil {
		m.log.Warn("git upload-pack produced no output", zap.String("mirror", handle.Record.Key.String()), zap.Error(err))
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, stream); err != nil {
		m.log.Warn("streaming a fetch failed", zap.String("mirror", handle.Record.Key.String()), zap.Error(err))
	}
}

// refuse is the single refusal: one status, one body, whatever the reason. The reason is recorded
// where the operator can see it and never where the caller can.
func (m *mirrorRead) refuse(w http.ResponseWriter, digest, reason string) {
	m.log.Info("refused a mirror read", zap.String("mirrorDigest", digest), zap.String("reason", reason))
	http.Error(w, "forbidden", http.StatusForbidden)
}

// unavailable answers a request the surface could not decide: a resolver or Cloud read that failed,
// or a store that is damaged. It is not a refusal, because the caller did nothing wrong and retrying
// is the correct response; it also never becomes an allow.
func (m *mirrorRead) unavailable(w http.ResponseWriter, _ *http.Request, digest, reason string) {
	m.log.Error("could not decide a mirror read", zap.String("mirrorDigest", digest), zap.String("reason", reason))
	http.Error(w, "unavailable", http.StatusServiceUnavailable)
}

// mirrorDigest turns the URL path component into a mirror digest. The component is "<digest>.git",
// which is what makes the URL look like a repository to git while naming an opaque mirror — the
// digest is not a project, not a repository and not a commit, so a caller cannot construct one from
// facts it holds, and reading a mirror it was not authorized for requires knowing a value only Cloud
// gives out.
func mirrorDigest(name string) (string, bool) {
	digest, found := strings.CutSuffix(name, ".git")
	if !found || len(digest) != 64 {
		return "", false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return digest, true
}

// peerAddress is the address the connection came from, taken from the connection and from nowhere
// else. Forwarding headers are ignored on purpose: this surface must never be deployed behind a proxy
// that would make every caller look like the proxy, and a header a caller can write is not evidence
// of where the caller is.
func peerAddress(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("connection address %q is not usable: %w", r.RemoteAddr, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("connection address %q is not an IP address: %w", host, err)
	}
	return addr.Unmap(), nil
}

// pktLine is Git's length-prefixed packet, used here for the one line the smart HTTP advertisement
// requires the server to write itself.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}
