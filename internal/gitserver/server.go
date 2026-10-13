// Package gitserver is the read-only Git Server.
//
// It serves Git's smart HTTP read protocol out of a mirror store, and it serves nothing else: no
// receive-pack, no archive, no dumb protocol, no other path. It is closed by default — a request is
// served only when the caller's container identity resolves *and* Cloud's state, read for that one
// request, authorizes that container to read that mirror at that generation. Every refusal is the
// same response with the same body, so a caller cannot use the server to learn which mirrors exist
// (D2, D5).
//
// This process holds no upstream credential and reaches no network other than Cloud's state reader
// and the origin resolver. The commit it serves was fetched by the credentialed executor, which is
// the only component that ever held the credential (D1).
package gitserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/internal/gitread"
	"github.com/wanglongan587/cloud/internal/mirror"
)

// ErrOriginUnresolvable reports that the connection's peer address belongs to no live sandbox
// container. It is a refusal: the caller is not a workspace.
var ErrOriginUnresolvable = errors.New("the caller is not a resolvable sandbox container")

// ErrOriginUnavailable reports that the origin resolver could not answer. It is deliberately distinct
// from ErrOriginUnresolvable: "this is not a workspace" is a decision, and "I cannot tell" must not
// be turned into one.
var ErrOriginUnavailable = errors.New("the origin resolver is unavailable")

// OriginResolver answers which sandbox container a peer address belongs to. It is the only source of
// caller identity, and it must resolve per request from deployment-owned state — an implementation
// that caches, or that accepts an address from anywhere but the connection, defeats the whole design.
type OriginResolver interface {
	Resolve(ctx context.Context, peer netip.Addr) (gitread.Origin, error)
}

// StateReader answers Cloud's authoritative state for one read request. An error means Cloud could
// not be read, which the server answers as unavailability rather than as a refusal.
type StateReader interface {
	Read(ctx context.Context, origin gitread.Origin, mirrorDigest string) (gitread.Snapshot, error)
}

// Config wires the server. Every dependency is required: a Git Server without an origin resolver or
// without a state reader has no way to authorize anything, and starting one anyway is how a
// fail-closed service becomes a fail-open one.
type Config struct {
	// Mirrors is the mirror store the server reads content from.
	Mirrors *mirror.Store
	// Origins resolves the connection's peer to a sandbox container identity.
	Origins OriginResolver
	// State reads Cloud's authoritative state for each request.
	State StateReader
	// GitPath is the git binary used for upload-pack. Empty means "git" from Path.
	GitPath string
	// Path is the PATH given to the git process. It is fixed rather than inherited so the process
	// cannot pick up a different git from the caller's environment.
	Path string
	// MaxRequestBytes bounds a fetch negotiation body.
	MaxRequestBytes int64
	// Timeout bounds one git invocation. A clone that needs longer is a clone that can be retried.
	Timeout time.Duration
	// Logger records refusals with their reason codes. Nil discards them.
	Logger *slog.Logger
}

// Server is the read-only Git Server.
type Server struct {
	cfg     Config
	home    string
	handler http.Handler
}

// New validates the configuration and builds the server. It creates one empty directory to be the
// git child processes' HOME, so upload-pack reads no user configuration from anywhere.
// caller editing its Config afterwards cannot move a running server's boundaries.
//
//nolint:gocritic // By value on purpose: the server is built from a configuration snapshot, so a
func New(cfg Config) (*Server, error) {
	if cfg.Mirrors == nil {
		return nil, errors.New("the read-only Git Server requires a mirror store")
	}
	if cfg.Origins == nil {
		return nil, errors.New("the read-only Git Server requires an origin resolver")
	}
	if cfg.State == nil {
		return nil, errors.New("the read-only Git Server requires a state reader")
	}
	if cfg.GitPath == "" {
		cfg.GitPath = "git"
	}
	if cfg.Path == "" {
		cfg.Path = "/usr/bin:/bin"
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = 1 << 22
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	home, err := os.MkdirTemp("", "ora-git-server-home-")
	if err != nil {
		return nil, fmt.Errorf("create the git process home: %w", err)
	}
	server := &Server{cfg: cfg, home: home}
	server.handler = server.routes()
	return server, nil
}

// Close releases the resources the server owns. The git process home holds nothing: upload-pack
// writes no state there.
func (s *Server) Close() error { return os.RemoveAll(s.home) }

// Handler is the HTTP surface. Mount it on a TLS listener that presents the Git Server's own
// certificate: the sandbox verifies the server against the internal CA it was given, and presents
// nothing of its own. Who the caller is never comes from the transport — it comes from the address
// the connection arrived from, resolved per request — so the listener has to be reached directly
// and must not sit behind a proxy or a load balancer, which would make every caller look like the
// proxy and turn every read into the same refusal.
func (s *Server) Handler() http.Handler { return s.handler }

const mirrorPrefix = "/mirror/v1/"

// MirrorPath is the request path this server serves one mirror at, including the `.git` suffix Git
// clients expect appended to a repository URL.
//
// It is exported because Cloud has to write that address into a Workspace's clone input once the
// mirror is published (D7), and the address a Controller is told to clone has to be an address this
// server actually answers on. Computing it from the same prefix the mux is built from is what keeps
// the two from drifting: a second spelling of the route elsewhere in Cloud would produce a clone
// input that the Node dutifully dispatches and this server 404s.
func MirrorPath(key mirror.Key) string { return mirrorPrefix + key.Digest() + ".git" }

// routes maps the two paths the smart HTTP read protocol uses. There is deliberately no catch-all:
// a path this mux does not name is a 404 from the standard library, not a refusal this code decides.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mirrorPrefix+"{name}/info/refs", s.serveInfoRefs)
	mux.HandleFunc("POST "+mirrorPrefix+"{name}/git-upload-pack", s.serveUploadPack)
	return mux
}

func (s *Server) serveInfoRefs(w http.ResponseWriter, r *http.Request) {
	digest, ok := mirrorDigest(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// The service is what the client says it wants to do. It is parsed rather than trusted: a write is
	// refused before anything else happens.
	operation := gitread.ParseService(r.URL.Query().Get("service"))
	s.serve(w, r, digest, operation, s.writeAdvertisement)
}

func (s *Server) serveUploadPack(w http.ResponseWriter, r *http.Request) {
	digest, ok := mirrorDigest(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serve(w, r, digest, gitread.ParseRPCPath("git-upload-pack"), s.runUploadPack)
}

// serve is the one path every request takes: identify, authorize, open, then produce a response.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, digest string, operation gitread.Operation, produce func(*mirror.Handle, http.ResponseWriter, *http.Request)) {
	if !operation.ReadOnly() {
		// A write, an archive request or an unknown service is refused without consulting anything:
		// there is nothing to consult, because the service is not implemented at all.
		s.refuse(w, digest, gitread.ReasonOperationNotServed)
		return
	}
	peer, err := peerAddress(r)
	if err != nil {
		s.refuse(w, digest, "peer_not_usable")
		return
	}
	origin, err := s.cfg.Origins.Resolve(r.Context(), peer)
	if err != nil {
		switch {
		case errors.Is(err, ErrOriginUnresolvable):
			s.refuse(w, digest, "origin_unresolvable")
		case errors.Is(err, ErrOriginUnavailable):
			s.unavailable(w, r, digest, "origin_resolver_unavailable")
		default:
			s.unavailable(w, r, digest, "origin_resolver_failed")
		}
		return
	}
	snapshot, err := s.cfg.State.Read(r.Context(), origin, digest)
	if err != nil {
		s.unavailable(w, r, digest, "state_unavailable")
		return
	}
	verdict := (gitread.Request{Origin: origin, MirrorDigest: digest, Operation: operation}).Evaluate(snapshot)
	if !verdict.Allow {
		s.refuse(w, digest, verdict.Reason)
		return
	}
	handle, err := s.cfg.Mirrors.Open(verdict.Key)
	if err != nil {
		switch {
		case errors.Is(err, mirror.ErrNotReady):
			// Cloud authorized a mirror the store does not consider readable. The two disagree, and the
			// store is the one holding the bytes, so the read is refused rather than improvised.
			s.refuse(w, digest, "mirror_not_readable")
		default:
			s.unavailable(w, r, digest, "mirror_store_failed")
		}
		return
	}
	if handle.Record.Generation != verdict.Generation {
		// The mirror was republished between the authorization and the read. Serving the newer
		// generation would mean serving content nobody authorized, so the read is refused and the
		// caller retries, which re-authorizes.
		s.refuse(w, digest, gitread.ReasonGenerationChanged)
		return
	}
	produce(&handle, w, r)
}

// writeAdvertisement emits the smart HTTP ref advertisement for a read.
func (s *Server) writeAdvertisement(handle *mirror.Handle, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, pktLine("# service=git-upload-pack\n")+"0000"); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	// --advertise-refs writes the ref advertisement and exits; the body is small and bounded by the
	// mirror's single ref.
	if err := s.runGit(ctx, handle.Dir, gitProtocol(r.Header.Get("Git-Protocol")), w, "--stateless-rpc", "--strict", "--advertise-refs"); err != nil {
		s.cfg.Logger.Warn("git upload-pack --advertise-refs failed", "mirror", handle.Record.Key.String(), "error", err)
	}
}

// runUploadPack proxies one fetch negotiation to git upload-pack.
func (s *Server) runUploadPack(handle *mirror.Handle, w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeout)
	defer cancel()
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	// The status line cannot be un-sent, so the first byte of git's answer is read before any header
	// is written: a git that fails immediately becomes a 500 the client can act on, instead of a 200
	// with an empty body that looks like a protocol error.
	stream, err := s.startGit(ctx, handle.Dir, gitProtocol(r.Header.Get("Git-Protocol")), body, "--stateless-rpc", "--strict")
	if err != nil {
		s.cfg.Logger.Warn("git upload-pack did not start", "mirror", handle.Record.Key.String(), "error", err)
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	defer stream.Close()
	// Peek does not consume, so the byte it returns is still the first byte the copy below writes.
	// Peeking is only how a child that failed before answering becomes a 500 instead of a 200 with a
	// truncated protocol stream in it.
	if _, err := stream.Peek(1); err != nil {
		s.cfg.Logger.Warn("git upload-pack produced no output", "mirror", handle.Record.Key.String(), "error", err)
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, stream); err != nil {
		s.cfg.Logger.Warn("streaming a fetch failed", "mirror", handle.Record.Key.String(), "error", err)
	}
}

// refuse is the single refusal: one status, one body, whatever the reason. The reason is recorded
// where the operator can see it and never where the caller can.
func (s *Server) refuse(w http.ResponseWriter, digest, reason string) {
	s.cfg.Logger.Info("refused a mirror read", "mirrorDigest", digest, "reason", reason)
	http.Error(w, "forbidden", http.StatusForbidden)
}

// unavailable answers a request the server could not decide: a resolver or Cloud read that failed, or
// a store that is damaged. It is not a refusal, because the caller did nothing wrong and retrying is
// the correct response; it also never becomes an allow.
func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, digest, reason string) {
	s.cfg.Logger.Error("could not decide a mirror read", "mirrorDigest", digest, "reason", reason)
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
// else. Forwarding headers are ignored on purpose: this server must never be deployed behind a proxy
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
