package modelgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
)

// Store is the authoritative policy consumed by the credential and model boundaries.
// Implementations persist only ciphertext and token digests; all authorization is current.
type Store interface {
	ModelCredential(context.Context, *core.Claims, string) (core.Object, error)
	PutModelCredential(context.Context, *core.Claims, string, int64, string, string, []byte, string) (core.Object, error)
	ClearModelCredential(context.Context, *core.Claims, string, int64, string) (core.Object, error)
	CreateModelGrant(context.Context, core.ModelRuntimeScope, string, string, string) (core.ModelGrant, error)
	RenewModelGrant(context.Context, core.ModelRuntimeScope, string, string) (core.ModelGrant, error)
	RevokeModelGrant(context.Context, core.ModelRuntimeScope, string, string) error
	ResolveModelGrant(context.Context, string) (core.ModelGrant, error)
}

// Authenticator verifies the purpose-bound Gateway and user signatures before policy lookup.
type Authenticator interface {
	Verify(string, string) (*core.Claims, error)
}

// Options injects policy, encryption, transport and the deployment's fixed model-facing origin.
type Options struct {
	Store           Store
	Auth            Authenticator
	Cipher          *Cipher
	PublicOrigin    string
	Upstream        *http.Client
	RecheckInterval time.Duration
	Health          func(context.Context) error
	Logger          *zap.Logger
}

// Service owns the model boundary; it holds no authoritative business or token state in memory.
type Service struct{ options Options }

// New validates complete dependencies so an incomplete secret deployment fails at startup.
func New(input *Options) (*Service, error) {
	if input == nil {
		return nil, errors.New("model gateway options are required")
	}
	o := *input
	if o.Logger == nil {
		o.Logger = zap.NewNop()
	}
	u, err := url.Parse(o.PublicOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("model public origin must be a fixed HTTPS origin")
	}
	if o.Store == nil || o.Auth == nil || o.Cipher == nil || o.Upstream == nil {
		return nil, errors.New("model store, authenticator, cipher and transport are required")
	}
	if o.RecheckInterval == 0 {
		o.RecheckInterval = 500 * time.Millisecond
	}
	if o.RecheckInterval < 10*time.Millisecond || o.RecheckInterval > time.Second {
		return nil, errors.New("model authorization recheck interval must be 10ms..1s")
	}
	return &Service{options: o}, nil
}

// Close releases the service's idle HTTP connections after its request contexts are cancelled.
func (s *Service) Close() { s.options.Upstream.CloseIdleConnections() }

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// fail returns safe codes only. SQL, provider bodies, keys and request content are not diagnostic data.
func fail(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "model_service_unavailable"
	var fault *core.Fault
	if errors.As(err, &fault) {
		status, code = fault.Status, fault.Code
	}
	writeJSON(w, status, map[string]any{"code": code, "params": map[string]any{}, "requestId": uuid.NewString()})
}

func reject(w http.ResponseWriter, status int, code string) {
	fail(w, &core.Fault{Status: status, Code: code})
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		reject(w, 400, "invalid_json")
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		reject(w, 400, "invalid_json")
		return false
	}
	var extra any
	if !errors.Is(d.Decode(&extra), io.EOF) {
		reject(w, 400, "invalid_json")
		return false
	}
	return true
}

// HealthHandler reports dependency readiness without exposing any configuration.
func (s *Service) HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" || r.Method != http.MethodGet {
		reject(w, 404, "not_found")
		return
	}
	if s.options.Health != nil {
		if err := s.options.Health(r.Context()); err != nil {
			reject(w, 503, "database_unavailable")
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}
