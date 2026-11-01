// Package nodeservices supplies the loopback Git, package and model services required by
// an unmodified local Node. It owns no page routes, Workspace admission or durable history.
package nodeservices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Model describes a frozen model, never a credential.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow uint64 `json:"contextWindow"`
	MaxTokens     uint64 `json:"maxTokens"`
}

// Provider handles one already-authorized completion. Implementations must honor request
// cancellation, force the supplied model, and never expose their credentials in errors.
type Provider interface {
	Complete(http.ResponseWriter, *http.Request, Model)
}

// Protocol is the model wire protocol a binding serves, spelled as Node and plugins spell it.
type Protocol string

const (
	// OpenAICompletions is OpenAI chat completions, served under /runtime/openai/v1.
	OpenAICompletions Protocol = "openai-completions"
	// AnthropicMessages is the Anthropic Messages API, served under /runtime/anthropic/v1.
	AnthropicMessages Protocol = "anthropic-messages"
)

// proxyPath is the fixed data route Node requires for each protocol's grant.
var proxyPath = map[Protocol]string{OpenAICompletions: "/runtime/openai/v1", AnthropicMessages: "/runtime/anthropic/v1"}

// Binding is trusted backend configuration; the caller cannot supply a provider or credential.
// Its grant token works only on the route of its Protocol.
type Binding struct {
	Model    Model
	Protocol Protocol
	Provider Provider
}

// Package is one preverified plugin archive, served to Node at PackageURLs[Name].
type Package struct {
	Name   string
	Path   string
	SHA256 string
}

// Options selects one local bare repository and preverified packages. The service creates
// a private ephemeral TLS directory beneath StateDirectory, outside all Agent checkouts.
type Options struct {
	StateDirectory string
	Repository     string
	GitBackend     string
	Packages       []Package
	Bindings       map[string]Binding
}

type servedPackage struct {
	file    *os.File
	size    int64
	modTime time.Time
}

// Service owns listeners, ephemeral credentials and execution-scoped model grants. Instantiate
// one per Workspace so its Node identity cannot access another Workspace's repository/grants.
type Service struct {
	URL          string
	PackageURLs  map[string]string
	TLS          TLSFiles
	opts         Options
	directory    string
	marketMu     sync.RWMutex
	packages     map[string]servedPackage
	https, files *http.Server
	workers      sync.WaitGroup
	handlers     sync.WaitGroup
	closeMu      sync.Mutex
	mu           sync.Mutex
	permits      map[string]string
	grants       map[string]*grant
	closed       bool
}

// Start binds only IPv4 loopback. Git and grant endpoints require this instance's mTLS
// identity; Agent model calls require a renewable token. Package bytes are digest pinned.
func Start(opts Options) (_ *Service, resultErr error) {
	if !filepath.IsAbs(opts.StateDirectory) || !filepath.IsAbs(opts.Repository) || !filepath.IsAbs(opts.GitBackend) || len(opts.Packages) == 0 || len(opts.Bindings) == 0 {
		return nil, errors.New("invalid local Node service options")
	}
	bindings := make(map[string]Binding, len(opts.Bindings))
	for id, b := range opts.Bindings {
		if id == "" || b.Provider == nil || proxyPath[b.Protocol] == "" || b.Model.ID == "" || b.Model.Name == "" || b.Model.MaxTokens == 0 || b.Model.ContextWindow == 0 {
			return nil, errors.New("invalid model binding")
		}
		bindings[id] = b
	}
	opts.Bindings = bindings
	opts.Packages = slices.Clone(opts.Packages)
	dir, err := os.MkdirTemp(opts.StateDirectory, "services-")
	if err != nil {
		return nil, err
	}
	s := &Service{opts: opts, directory: dir, packages: map[string]servedPackage{}, PackageURLs: map[string]string{}, permits: map[string]string{}, grants: map[string]*grant{}}
	defer func() {
		if resultErr != nil {
			_ = s.Close()
		}
	}()
	files, config, err := localTLS(dir)
	if err != nil {
		return nil, err
	}
	s.TLS = files
	for _, p := range opts.Packages {
		if err := s.copyPackage(p); err != nil {
			return nil, err
		}
	}
	secure, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			secure.Close()
		}
	}()
	plain, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s.URL = "https://" + secure.Addr().String()
	for name := range s.packages {
		s.PackageURLs[name] = "http://" + plain.Addr().String() + "/" + name + ".orax"
	}
	s.https = &http.Server{Handler: s.track(s.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024, ErrorLog: log.New(io.Discard, "", 0)}
	s.files = &http.Server{Handler: s.track(func(w http.ResponseWriter, r *http.Request) {
		name, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".orax")
		s.marketMu.RLock()
		defer s.marketMu.RUnlock()
		p, found := s.packages[name]
		if !ok || !found || (r.Method != "GET" && r.Method != "HEAD") {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name+".orax", p.modTime, io.NewSectionReader(p.file, 0, p.size))
	}), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	s.workers.Add(2)
	go func() { defer s.workers.Done(); _ = s.https.Serve(tls.NewListener(secure, config)) }()
	go func() { defer s.workers.Done(); _ = s.files.Serve(plain) }()
	return s, nil
}

// copyPackage snapshots one archive into the private directory and verifies the snapshot, so a
// later change to the prepared asset cannot change the bytes Node is served.
func (s *Service) copyPackage(p Package) error {
	if p.Name == "" || strings.ContainsAny(p.Name, "/.") || !filepath.IsAbs(p.Path) {
		return errors.New("invalid package")
	}
	if _, exists := s.packages[p.Name]; exists {
		return errors.New("duplicate package")
	}
	source, err := os.Open(p.Path)
	if err != nil {
		return err
	}
	defer source.Close()
	stat, err := source.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return errors.New("package must be a regular file")
	}
	file, err := os.OpenFile(filepath.Join(s.directory, p.Name+".orax"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	s.packages[p.Name] = servedPackage{file: file, size: stat.Size(), modTime: stat.ModTime()}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(h, file), source); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		return errors.New("package SHA-256 mismatch")
	}
	return nil
}

// StageMarketPackage exposes one digest-verified archive only while Node processes its command.
func (s *Service) StageMarketPackage(pkg Package) (string, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", nil, errors.New("local services closed")
	}
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	if _, exists := s.packages[pkg.Name]; exists {
		return "", nil, errors.New("package already staged")
	}
	cleanup := func() {
		if staged, ok := s.packages[pkg.Name]; ok {
			_ = staged.file.Close()
			delete(s.packages, pkg.Name)
			_ = os.Remove(filepath.Join(s.directory, pkg.Name+".orax"))
		}
	}
	if e := s.copyPackage(pkg); e != nil {
		cleanup()
		return "", nil, e
	}
	addr := strings.TrimSuffix(s.PackageURLs["opencode"], "/opencode.orax")
	return addr + "/" + pkg.Name + ".orax", func() { s.marketMu.Lock(); defer s.marketMu.Unlock(); cleanup() }, nil
}

// Permit authorizes an admitted execution before its Node command is sent. The same execution
// cannot change model. End must be called when admission is abandoned or the session ends.
func (s *Service) Permit(execution, binding string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || execution == "" {
		return errors.New("local services unavailable")
	}
	if _, ok := s.opts.Bindings[binding]; !ok {
		return errors.New("unknown model binding")
	}
	if existing, ok := s.permits[execution]; ok && existing != binding {
		return errors.New("execution model conflict")
	}
	if len(s.permits) >= 1024 && s.permits[execution] == "" {
		return errors.New("too many model sessions")
	}
	s.permits[execution] = binding
	return nil
}

// End revokes grants and cancels in-flight provider calls. It does not send Node EndSession;
// the runtime must coordinate that separately and retain its terminal event.
func (s *Service) End(execution string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.permits, execution)
	for id, g := range s.grants {
		if g.execution == execution {
			g.cancel()
			delete(s.grants, id)
		}
	}
}

// Close revokes model access, closes listeners and removes only the private credential directory.
func (s *Service) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, g := range s.grants {
		g.cancel()
	}
	clear(s.grants)
	clear(s.permits)
	s.mu.Unlock()
	var err error
	if s.https != nil {
		err = errors.Join(err, s.https.Close())
	}
	if s.files != nil {
		err = errors.Join(err, s.files.Close())
	}
	s.workers.Wait()
	s.handlers.Wait()
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	for _, p := range s.packages {
		err = errors.Join(err, p.file.Close())
	}
	return errors.Join(err, os.RemoveAll(s.directory))
}

func (s *Service) track(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			fault(w, 503, "services_closed")
			return
		}
		s.handlers.Add(1)
		s.mu.Unlock()
		defer s.handlers.Done()
		next(w, r)
	})
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/runtime/openai/v1/chat/completions":
		// A header without the Bearer scheme carries no token; model rejects the empty one with 401.
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		s.model(w, r, OpenAICompletions, token)
	case r.URL.Path == "/runtime/anthropic/v1/messages":
		// Claude Code also sends a placeholder bearer; only x-api-key carries the grant token.
		s.model(w, r, AnthropicMessages, r.Header.Get("X-Api-Key"))
	case r.TLS == nil || len(r.TLS.VerifiedChains) == 0:
		fault(w, http.StatusUnauthorized, "node_identity_required")
	case r.URL.Path == "/repo.git/info/refs" || r.URL.Path == "/repo.git/git-upload-pack":
		s.git(w, r)
	case r.URL.Path == "/internal/v1/model-grants":
		s.issue(w, r)
	default:
		s.changeGrant(w, r)
	}
}

func fault(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code})
}
func randomID() (string, error) {
	var b [24]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fault(w, 400, "invalid_request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fault(w, 400, "invalid_request")
		return false
	}
	return true
}

type grant struct {
	execution, binding, token string
	expires                   time.Time
	ctx                       context.Context
	cancel                    context.CancelFunc
}
