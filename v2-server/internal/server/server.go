// Package server is v2-server's HTTP surface: the Gateway's browser login routes, simplified to a
// development login, and the subset of Cloud's api/openapi.json that v2 uses.
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/chat"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// SessionCookie is the Gateway's development cookie name, so the frontend sees the same session.
const SessionCookie = "ora_session"

// Store is what the handlers need from the database.
type Store interface {
	Ping(ctx context.Context) error
	UserByEmail(ctx context.Context, email string) (store.User, error)
	StartSession(ctx context.Context, userID string, ttl time.Duration) (string, error)
	InviteMember(ctx context.Context, tenant, caller string, in store.Invite) (store.Member, error)
	Logout(ctx context.Context, token string) error
	SessionUser(ctx context.Context, token string) (store.User, error)
	Tenants(ctx context.Context, userID string) ([]store.Tenant, error)
	TenantMembers(ctx context.Context, tenant, caller string) ([]store.Member, error)
	TenantSpaces(ctx context.Context, tenant, caller string) ([]store.Space, error)
	SpaceAgentPlugins(ctx context.Context, tenant, space, caller string) ([]store.SpacePlugin, error)
}

// Options tune the server.
type Options struct {
	// SessionTTL is how long a login lasts.
	SessionTTL time.Duration
	// Projects prepares fixed repository mirrors; nil leaves project operations unavailable.
	Projects *projectrepos.Service
	// Execution is the shared backend for page-owned conversations and runs.
	// Nil uses an unavailable Stub; an explicitly supplied backend is kept unchanged.
	Execution execution.Service
	// Catalog exposes the executable Agent/model choices from that backend.
	// Nil uses an unavailable Stub, independently of Execution.
	Catalog execution.Catalog
	// Schedules is the same host used by the process's periodic scanner. Nil disables commands.
	Schedules ScheduleActions
	// AnalyzeWorkflow derives authoring participation for one graph document; nil keeps the
	// analyze route unavailable. The rule belongs to the workflow domain, not this package.
	AnalyzeWorkflow func(graph string) (WorkflowAnalysis, error)
	// ValidateWorkflow reports whether a document can execute, so publish freezes only a runnable
	// version. Nil keeps publish unavailable instead of accepting a document nothing can run.
	ValidateWorkflow func(graph string) error
	// Now reads the clock automatic publish versions use; nil uses the process clock.
	Now func() time.Time
	// WorkflowRuns is the execution bridge's run lifecycle. Nil keeps start, cancel, input and
	// interactive completion unavailable; definition and history reads still work.
	WorkflowRuns WorkflowRunHost
}

// New returns the handler for every route v2-server serves.
func New(s Store, o Options) http.Handler {
	h := newHandler(s, o)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /auth/providers", h.providers)
	mux.HandleFunc("POST /auth/login", h.login)
	mux.HandleFunc("POST /auth/logout", h.logout)
	mux.HandleFunc("GET /api/v1/me", h.authed(h.me))
	mux.HandleFunc("GET /api/v1/me/tenants", h.authed(h.tenants))
	mux.HandleFunc("GET /api/v1/tenants/{tid}/members", h.authed(h.members))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/members", h.authed(h.invite))
	mux.HandleFunc("GET /api/v1/tenants/{tid}/spaces", h.authed(h.spaces))
	mux.HandleFunc("GET /api/v1/tenants/{tid}/spaces/{sid}/plugins", h.authed(h.spacePlugins))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/projects", h.authed(h.projects))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/projects", h.authed(h.projects))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/projects/{pid}", h.authed(h.projects))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/execution/models", h.authed(h.models))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/execution/executors", h.authed(h.executors))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/schedule-configs", h.authed(h.scheduleConfigs))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/schedule-resources", h.authed(h.scheduleResources))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs", h.authed(h.scheduleHistory))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}", h.authed(h.scheduleHistory))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}/result", h.authed(h.scheduleResult))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/schedule-configs", h.authed(h.scheduleConfigs))
	mux.HandleFunc("PUT /api/v2/tenants/{tid}/schedule-configs/{id}", h.authed(h.scheduleConfigs))
	mux.HandleFunc("PATCH /api/v2/tenants/{tid}/schedule-configs/{id}/enabled", h.authed(h.scheduleConfigs))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/schedule-configs/{id}/run", h.authed(h.scheduleAction))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}/cancel", h.authed(h.scheduleAction))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/squads", h.authed(h.listSquads))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/squads", h.authed(h.createSquad))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/squads/{sid}", h.authed(h.getSquad))
	mux.HandleFunc("PUT /api/v2/tenants/{tid}/squads/{sid}", h.authed(h.updateSquad))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/squads/{sid}/archive", h.authed(h.archiveSquad))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/squads/{sid}/members", h.authed(h.addSquadMember))
	mux.HandleFunc("PUT /api/v2/tenants/{tid}/squads/{sid}/members/{mid}", h.authed(h.updateSquadMember))
	mux.HandleFunc("DELETE /api/v2/tenants/{tid}/squads/{sid}/members/{mid}", h.authed(h.removeSquadMember))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows", h.authed(h.listWorkflows))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflows", h.authed(h.createWorkflow))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflows/analyze", h.authed(h.analyzeWorkflowGraph))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows/{wid}", h.authed(h.getWorkflow))
	mux.HandleFunc("PUT /api/v2/tenants/{tid}/workflows/{wid}", h.authed(h.updateWorkflow))
	mux.HandleFunc("DELETE /api/v2/tenants/{tid}/workflows/{wid}", h.authed(h.deleteWorkflow))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows/{wid}/draft", h.authed(h.getWorkflowDraft))
	mux.HandleFunc("PUT /api/v2/tenants/{tid}/workflows/{wid}/draft", h.authed(h.updateWorkflowDraft))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflows/{wid}/publish", h.authed(h.publishWorkflow))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows/{wid}/versions", h.authed(h.listWorkflowVersions))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows/{wid}/versions/{version}", h.authed(h.getWorkflowVersion))
	mux.HandleFunc("DELETE /api/v2/tenants/{tid}/workflows/{wid}/versions/{version}", h.authed(h.deleteWorkflowVersion))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflows/{wid}/activate", h.authed(h.activateWorkflow))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflows/{wid}/rollback", h.authed(h.rollbackWorkflow))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows/{wid}/snapshots/{sid}", h.authed(h.getWorkflowSnapshot))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflows/{wid}/runs", h.authed(h.createWorkflowRun))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflows/{wid}/runs", h.authed(h.listWorkflowRuns))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflow-runs", h.authed(h.listWorkflowRuns))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflow-runs/{rid}", h.authed(h.getWorkflowRun))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/snapshot", h.authed(h.getWorkflowRunSnapshot))
	mux.HandleFunc("DELETE /api/v2/tenants/{tid}/workflow-runs/{rid}", h.authed(h.deleteWorkflowRun))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/start", h.authed(h.startWorkflowRun))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/cancel", h.authed(h.cancelWorkflowRun))
	mux.HandleFunc("PATCH /api/v2/tenants/{tid}/workflow-runs/{rid}/input", h.authed(h.updateWorkflowRunInput))
	mux.HandleFunc("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeId}/complete", h.authed(h.completeWorkflowNode))
	mux.HandleFunc("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes", h.authed(h.listWorkflowRunNodes))
	h.chatRoutes(mux)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { fault(w, http.StatusNotFound, "not_found") })
	return mux
}

type handler struct {
	store Store
	opts  Options
	// chats runs the chat routes on the injected execution service and catalog; nil when the store
	// cannot keep conversations.
	chats *chat.Service
}

// newHandler makes omitted dependencies safe without inferring one interface from another.
func newHandler(s Store, o Options) *handler {
	unavailable := &executiontest.Stub{}
	if o.Execution == nil {
		o.Execution = unavailable
	}
	if o.Catalog == nil {
		o.Catalog = unavailable
	}
	h := &handler{store: s, opts: o}
	if cs, ok := s.(chat.Store); ok {
		h.chats = chat.New(cs, o.Execution, o.Catalog, chat.Options{})
	}
	return h
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if e := h.store.Ping(r.Context()); e != nil {
		slog.Error("health", "error", e)
		fault(w, http.StatusServiceUnavailable, "database_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"dependencies": map[string]string{"objectStore": "unconfigured", "gitServer": "unconfigured"},
	})
}

// providers matches the Gateway: v2-server offers only the email login.
func (h *handler) providers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": []string{LoginProviderEmail}, "default": LoginProviderEmail})
}

// LoginProviderEmail is the only login provider: the address alone identifies the account.
const LoginProviderEmail = "email"

// login is the Gateway's POST /auth/login with the provider round trip skipped: the email login
// signs in at once and sends the browser straight back to returnTo. Only an address the startup
// preset lists or an owner already invited may sign in; an unknown one is rejected, never created.
func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		Email    string `json:"email"`
		ReturnTo string `json:"returnTo"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&in); e != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	if in.Provider != "" && in.Provider != LoginProviderEmail {
		fault(w, http.StatusBadRequest, "unknown_provider")
		return
	}
	email, e := store.NormalizeEmail(in.Email)
	if e != nil {
		fault(w, http.StatusBadRequest, "invalid_email")
		return
	}
	u, e := h.store.UserByEmail(r.Context(), email)
	if errors.Is(e, store.ErrNotFound) {
		fault(w, http.StatusForbidden, "email_not_invited")
		return
	}
	if e != nil {
		internal(w, "find login account", e)
		return
	}
	token, e := h.store.StartSession(r.Context(), u.ID, h.opts.SessionTTL)
	if e != nil {
		internal(w, "login", e)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(h.opts.SessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]string{"authorizationUrl": localPath(in.ReturnTo)})
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie(SessionCookie); e == nil {
		if e := h.store.Logout(r.Context(), c.Value); e != nil {
			internal(w, "logout", e)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// authed resolves the session cookie to a user, answering 401 like Cloud when there is none.
func (h *handler) authed(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie(SessionCookie)
		if e != nil {
			fault(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		u, e := h.store.SessionUser(r.Context(), c.Value)
		if errors.Is(e, store.ErrNotFound) {
			fault(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		if e != nil {
			internal(w, "session", e)
			return
		}
		next(w, r, u)
	}
}

func (h *handler) me(w http.ResponseWriter, _ *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, u)
}

// tenants returns every membership in one page; v2 users belong to a handful of tenants.
func (h *handler) tenants(w http.ResponseWriter, r *http.Request, u store.User) {
	items, e := h.store.Tenants(r.Context(), u.ID)
	if e != nil {
		internal(w, "tenants", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": ""})
}

// localPath keeps a post-login redirect on this origin.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if e := json.NewEncoder(w).Encode(v); e != nil {
		slog.Error("write response", "error", e)
	}
}

// fault writes Cloud's Error shape.
func fault(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"code": code, "params": map[string]any{}, "requestId": requestID()})
}

func internal(w http.ResponseWriter, op string, e error) {
	slog.Error(op, "error", e)
	fault(w, http.StatusInternalServerError, "internal")
}

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
