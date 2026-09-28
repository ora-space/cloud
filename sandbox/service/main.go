package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxWebSocketMessage = 16 * 1024 * 1024

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type config struct {
	atespace       string
	template       string
	controllerID   string
	kubectlAte     string
	kubeconfig     string
	context        string
	internalRouter string
	lifecycleAddr  string
	routerAddr     string
	statePath      string
}

type effectRequest struct {
	Kind              string `json:"kind"`
	ProjectID         string `json:"projectId"`
	WorkspaceID       string `json:"workspaceId"`
	SandboxInstanceID string `json:"sandboxInstanceId,omitempty"`
}

type effectEntry struct {
	ID         string         `json:"id"`
	ExternalID string         `json:"externalId"`
	State      string         `json:"state"`
	Request    effectRequest  `json:"request"`
	Result     map[string]any `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type workspaceState struct {
	ProjectID       string          `json:"projectId"`
	WorkspaceID     string          `json:"workspaceId"`
	ActiveSandboxID string          `json:"activeSandboxId,omitempty"`
	CurrentTag      string          `json:"currentTag,omitempty"`
	PendingTag      string          `json:"pendingTag,omitempty"`
	Tombstones      map[string]bool `json:"tombstones,omitempty"`
}

type database struct {
	Effects    map[string]*effectEntry    `json:"effects"`
	Workspaces map[string]*workspaceState `json:"workspaces"`
}

type service struct {
	cfg  config
	mu   sync.Mutex
	ops  sync.Mutex
	db   database
	http *http.Client
}

func main() {
	if err := run(); err != nil {
		log.Printf("ORA sandbox service stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg config
	flag.StringVar(&cfg.atespace, "atespace", "ate-coding-poc", "Substrate atespace")
	flag.StringVar(&cfg.template, "template", "ora-coding-v3", "ActorTemplate used for a new workspace")
	flag.StringVar(&cfg.controllerID, "controller-id", "ora-cloud-controller", "Controller identity accepted by Ora Node")
	flag.StringVar(&cfg.kubectlAte, "kubectl-ate", "/opt/substrate-poc/bin/kubectl-ate", "kubectl-ate binary")
	flag.StringVar(&cfg.kubeconfig, "kubeconfig", "/opt/substrate-poc/config/kubeconfig", "Kubernetes config")
	flag.StringVar(&cfg.context, "context", "kind-substrate-poc", "Kubernetes context")
	flag.StringVar(&cfg.internalRouter, "internal-router", "ws://127.0.0.1:18000", "Substrate atenet ingress")
	flag.StringVar(&cfg.lifecycleAddr, "lifecycle-addr", "127.0.0.1:18002", "Effects HTTP listen address")
	flag.StringVar(&cfg.routerAddr, "router-addr", "127.0.0.1:18001", "Controller WebSocket router listen address")
	flag.StringVar(&cfg.statePath, "state", "/opt/substrate-poc/ora-sandbox-service/state/journal.json", "Persistent journal path")
	flag.Parse()

	s, err := newService(&cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lifecycle := &http.Server{Addr: cfg.lifecycleAddr, Handler: http.HandlerFunc(s.handleEffects), ReadHeaderTimeout: 5 * time.Second}
	routerMux := http.NewServeMux()
	routerMux.HandleFunc("/ora-node/v1", s.handleWebSocket)
	router := &http.Server{Addr: cfg.routerAddr, Handler: routerMux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 2)
	go serveHTTP(lifecycle, errCh)
	go serveHTTP(router, errCh)
	log.Printf("ORA sandbox service ready effects=%s router=%s atespace=%s template=%s", cfg.lifecycleAddr, cfg.routerAddr, cfg.atespace, cfg.template)
	var serveErr error
	select {
	case serveErr = <-errCh:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := errors.Join(lifecycle.Shutdown(shutdownCtx), router.Shutdown(shutdownCtx))
	return errors.Join(serveErr, shutdownErr)
}

func serveHTTP(server *http.Server, errCh chan<- error) {
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- err
	}
}

func newService(cfg *config) (*service, error) {
	s := &service{cfg: *cfg, http: &http.Client{Timeout: 5 * time.Second}}
	s.db.Effects = map[string]*effectEntry{}
	s.db.Workspaces = map[string]*workspaceState{}
	b, err := os.ReadFile(cfg.statePath)
	if err == nil {
		if err = json.Unmarshal(b, &s.db); err != nil {
			return nil, fmt.Errorf("decode journal: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	if s.db.Effects == nil {
		s.db.Effects = map[string]*effectEntry{}
	}
	if s.db.Workspaces == nil {
		s.db.Workspaces = map[string]*workspaceState{}
	}
	return s, nil
}

func (s *service) handleEffects(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/effects/") || strings.Contains(strings.TrimPrefix(r.URL.Path, "/effects/"), "/") {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/effects/")
	if !uuidPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_effect_id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.reconcile(id)
		s.mu.Lock()
		entry, ok := s.db.Effects[id]
		if ok {
			effectSnapshot := cloneEffect(entry)
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, effectSnapshot)
			return
		}
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found")
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		defer r.Body.Close()
		var request effectRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if code := validateRequest(request); code != "" {
			status := http.StatusBadRequest
			if code == "unsupported_kind" {
				status = http.StatusBadRequest
			}
			writeError(w, status, code)
			return
		}
		entry, status, code := s.putEffect(id, request)
		if code != "" {
			writeError(w, status, code)
			return
		}
		writeJSON(w, status, entry)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}
