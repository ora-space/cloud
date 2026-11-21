// Package server is v2-server's HTTP surface: the Gateway's browser login routes, simplified to a
// development login, and the subset of Cloud's api/openapi.json that v2 uses.
package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/chat"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/execution/executiontest"
	"github.com/wanglongan587/cloud/v2-server/internal/login"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
	"github.com/wanglongan587/cloud/v2-server/internal/taskrun"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

// Store is what the handlers need from the database.
type Store interface {
	// TenantAccess is the single tenant authorization decision (see router).
	TenantAccess(ctx context.Context, tenant, user string) (store.Access, error)
	OfficialMarketEnabled() bool
	SkillProgress(context.Context, string, string, string, string) ([]store.SkillProgress, error)
	MarketSources(context.Context, string, string) ([]store.MarketSource, error)
	ChangeMarket(context.Context, string, string, string, string, store.MarketSource) error
	Ping(ctx context.Context) error
	UserByEmail(ctx context.Context, email string) (store.User, error)
	SignInExternal(ctx context.Context, source, subject, displayName string) (store.User, error)
	StartSession(ctx context.Context, userID string, ttl time.Duration) (string, error)
	InviteMember(ctx context.Context, tenant string, in store.Invite) (store.Member, error)
	CreateInviteLink(ctx context.Context, tenant, creator, squad string) (store.InviteLink, string, error)
	ListInviteLinks(ctx context.Context, tenant string) ([]store.InviteLink, error)
	RevokeInviteLink(ctx context.Context, tenant, id string) error
	PreviewInvite(ctx context.Context, token, user string) (store.InvitePreview, error)
	AcceptInvite(ctx context.Context, token, user string) (string, error)
	RemoveMember(ctx context.Context, tenant, user string) error
	Logout(ctx context.Context, token string) error
	SessionUser(ctx context.Context, token string) (store.User, error)
	StartDesktopToken(ctx context.Context, userID string) (string, store.User, error)
	DesktopTokenUser(ctx context.Context, token string) (store.User, error)
	RevokeDesktopToken(ctx context.Context, token string) error
	Tenants(ctx context.Context, userID string) ([]store.Tenant, error)
	TenantMembers(ctx context.Context, tenant string) ([]store.Member, error)
	TenantSpaces(ctx context.Context, a store.Access) ([]store.Space, error)
	SpaceAgentPlugins(ctx context.Context, tenant, space string) ([]store.SpacePlugin, error)
	SpacePlugins(ctx context.Context, tenant, space string) ([]store.SpacePlugin, error)
	PluginCatalog(ctx context.Context, tenant, space string) ([]store.PluginCatalogEntry, time.Time, error)
	InstallSpacePlugin(ctx context.Context, tenant, space, caller, path, key string, in store.PluginInstallInput) (store.SpacePlugin, error)
	PluginPackageSource(ctx context.Context, tenant, space, identifier string) (store.PluginPackageSource, error)
	UpsertPluginSkills(ctx context.Context, digest string, names []string) error
	InstalledPluginSkillNames(ctx context.Context, tenant, space string) (map[string][]string, error)
	InstallWorkflowPlugin(ctx context.Context, tenant, space, caller, path, key string, in store.PluginInstallInput, documents []pluginmarket.WorkflowDocument, validate func(string) error) (store.SpacePlugin, error)
	RemoveSpacePlugin(ctx context.Context, tenant, space, caller, path, key string, in store.PluginRemoveInput) (store.SpacePlugin, error)
	ListSkills(ctx context.Context, tenant, caller string) ([]store.Skill, error)
	GetSkill(ctx context.Context, tenant, caller, id string) (store.Skill, error)
	CreateSkill(ctx context.Context, tenant, caller string, in store.SkillInput) (store.Skill, error)
	UpdateSkill(ctx context.Context, tenant, caller, id string, in store.SkillInput) (store.Skill, error)
	DeleteSkill(ctx context.Context, tenant, caller, id string) error
	ImportSkill(ctx context.Context, tenant, caller string, raw []byte, overwrite bool) (store.Skill, error)
	ListRoles(ctx context.Context, tenant string) ([]store.Role, error)
	GetRole(ctx context.Context, tenant, id string) (store.Role, error)
	CreateRole(ctx context.Context, tenant, caller string, in store.RoleInput) (store.Role, error)
	UpdateRole(ctx context.Context, tenant, id string, in store.RoleInput) (store.Role, error)
	DeleteRole(ctx context.Context, tenant, id string) error
	ImportRole(ctx context.Context, tenant, caller string, raw []byte, overwrite bool) (store.Role, error)
}

// PluginPackageDownloader stages one verified marketplace archive and returns its local path.
type PluginPackageDownloader func(context.Context, string, string, pluginmarket.Retrieval, pluginmarket.Credentials) (string, error)

// Options tune the server.
type Options struct {
	// TaskWorkflows owns handling runs and unified task workflow history.
	TaskWorkflows   *taskworkflow.Host
	RefreshOfficial func(context.Context) error
	// SessionTTL is how long a login lasts.
	SessionTTL time.Duration
	// LoginProviders are the login methods V2_LOGIN offers, in display order; empty means email.
	LoginProviders []string
	// LoginRedirects implements every offered login method other than email, by name. Their
	// callback is PublicURL + /auth/callback/<name>, so PublicURL must be set when any is offered.
	LoginRedirects map[string]login.Provider
	// PublicURL is the address browsers use for this server (V2_PUBLIC_URL). When set, it is the
	// only origin cookie-authenticated writes accept, and an https address makes the session cookie
	// Secure with the __Host- prefix. Nil compares Origin with the request's Host instead.
	PublicURL *url.URL
	// Projects prepares fixed repository mirrors; nil leaves project operations unavailable.
	Projects *projectrepos.Service
	// Execution is the shared backend for page-owned conversations and runs.
	// Nil uses an unavailable Stub; an explicitly supplied backend is kept unchanged.
	Execution  execution.Service
	SessionMCP interface {
		ObserveSessionMCP(context.Context, execution.Scope, string, *execution.SessionMCPStatus, string) error
	}
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
	// WorkflowRunEvents announces committed run changes to the run events route; nil keeps that
	// route unavailable, and pages poll instead.
	WorkflowRunEvents WorkflowRunEvents
	// TaskRuns runs task Agents and records comments for the collaboration page; nil keeps those
	// writes and the run event stream unavailable, while task reads and the timeline still work.
	TaskRuns *taskrun.Host
	// TaskDecompositions splits a task into subtasks with a workflow; nil keeps 「拆分」 unavailable.
	TaskDecompositions *taskdecomp.Host
	// DownloadPluginPackage stages a workflow package before import. Nil uses the public
	// marketplace downloader.
	DownloadPluginPackage PluginPackageDownloader
	// Deliveries reads retained turn deliveries for the change view and patch download; nil
	// answers 503 workspace_unavailable wherever a delivery would be read.
	Deliveries DeliveryReader
	// WorkspaceReclaims previews and executes the reclaim of a deleted conversation's checkout;
	// nil (no Agent runtime, so no checkouts) leaves a recorded reclaim to the next start.
	WorkspaceReclaims chat.Reclaims
	// MaxPatchBytes caps a downloadable delivery patch; zero or less uses DefaultMaxPatchBytes.
	MaxPatchBytes int
	// Context is the server's lifetime: work the handler runs beyond any one request, such as a
	// decomposition verdict other requests share, stops when it ends. Nil uses
	// context.Background(), for tests and callers whose handler lives as long as the process.
	Context context.Context
}

// New returns the handler for every route v2-server serves.
func New(s Store, o Options) http.Handler {
	return newRouter(newHandler(s, o)).mux
}

// newRouter registers every route in one table (see router).
func newRouter(h *handler) *router {
	rt := &router{h: h, mux: http.NewServeMux()}
	rt.public("GET /healthz", h.health)
	rt.public("GET /auth/providers", h.providers)
	rt.public("POST /auth/login", h.login)
	rt.public("POST /auth/logout", h.logout)
	rt.public("GET /auth/callback/{provider}", h.callback)
	rt.session("POST /auth/desktop/approve", h.approveDesktop)
	rt.public("POST /auth/desktop/token", h.desktopToken)
	rt.public("POST /auth/desktop/logout", h.desktopLogout)
	rt.session("GET /api/v1/me", h.me)
	rt.session("GET /api/v1/me/tenants", h.tenants)
	rt.tenant("GET /api/v1/tenants/{tid}/members", h.members)
	rt.owner("POST /api/v2/tenants/{tid}/members", h.invite)
	rt.owner("DELETE /api/v2/tenants/{tid}/members/{uid}", h.removeMember)
	rt.owner("POST /api/v2/tenants/{tid}/invite-links", h.createInviteLink)
	rt.owner("GET /api/v2/tenants/{tid}/invite-links", h.inviteLinks)
	rt.owner("DELETE /api/v2/tenants/{tid}/invite-links/{id}", h.revokeInviteLink)
	rt.session("POST /api/v1/invites/preview", h.previewInvite)
	rt.session("POST /api/v1/invites/accept", h.acceptInvite)
	rt.tenant("GET /api/v2/tenants/{tid}/skills", h.listSkills)
	rt.tenant("POST /api/v2/tenants/{tid}/skills", h.createSkill)
	rt.tenant("POST /api/v2/tenants/{tid}/skills/import", h.importSkill)
	rt.tenant("GET /api/v2/tenants/{tid}/skills/{sid}", h.getSkill)
	rt.tenant("PUT /api/v2/tenants/{tid}/skills/{sid}", h.updateSkill)
	rt.tenant("DELETE /api/v2/tenants/{tid}/skills/{sid}", h.deleteSkill)
	rt.tenant("GET /api/v2/tenants/{tid}/roles", h.listRoles)
	rt.tenant("POST /api/v2/tenants/{tid}/roles", h.createRole)
	rt.tenant("POST /api/v2/tenants/{tid}/roles/import", h.importRole)
	rt.tenant("GET /api/v2/tenants/{tid}/roles/{rid}", h.getRole)
	rt.tenant("PUT /api/v2/tenants/{tid}/roles/{rid}", h.updateRole)
	rt.tenant("DELETE /api/v2/tenants/{tid}/roles/{rid}", h.deleteRole)
	rt.tenant("GET /api/v1/tenants/{tid}/spaces", h.spaces)
	rt.tenant("GET /api/v1/tenants/{tid}/mcp", h.listMCP)
	rt.owner("POST /api/v1/tenants/{tid}/spaces/{sid}/mcp", h.installMCP)
	rt.owner("PUT /api/v1/tenants/{tid}/spaces/{sid}/mcp/settings", h.setMCPSettings)
	rt.owner("DELETE /api/v1/tenants/{tid}/spaces/{sid}/mcp", h.removeMCP)
	rt.tenant("GET /api/v1/tenants/{tid}/spaces/{sid}/plugins/sources", h.marketSources)
	rt.owner("POST /api/v1/tenants/{tid}/spaces/{sid}/plugins/sources", h.marketSources)
	rt.owner("PATCH /api/v1/tenants/{tid}/spaces/{sid}/plugins/sources/{source}", h.marketSources)
	rt.owner("DELETE /api/v1/tenants/{tid}/spaces/{sid}/plugins/sources/{source}", h.marketSources)
	rt.owner("POST /api/v1/tenants/{tid}/spaces/{sid}/plugins/sources/{source}/sync", h.marketSources)
	rt.tenant("GET /api/v1/tenants/{tid}/spaces/{sid}/plugins/results/{namespace}/{identifier}", h.skillProgress)
	rt.tenant("GET /api/v1/tenants/{tid}/spaces/{sid}/plugins/catalog", h.pluginCatalog)
	rt.tenant("GET /api/v1/tenants/{tid}/spaces/{sid}/plugins", h.spacePlugins)
	rt.owner("POST /api/v1/tenants/{tid}/spaces/{sid}/plugins", h.installSpacePlugin)
	rt.owner("DELETE /api/v1/tenants/{tid}/spaces/{sid}/plugins", h.removeSpacePlugin)
	rt.tenant("GET /api/v2/tenants/{tid}/projects", h.projects)
	rt.tenant("POST /api/v2/tenants/{tid}/projects", h.projects)
	rt.tenant("GET /api/v2/tenants/{tid}/projects/{pid}", h.projects)
	rt.tenant("POST /api/v2/tenants/{tid}/projects/{pid}/sync", h.projectSync)
	rt.tenant("PUT /api/v2/tenants/{tid}/projects/{pid}/repository-token", h.projectToken)
	rt.tenant("DELETE /api/v2/tenants/{tid}/projects/{pid}/repository-token", h.projectToken)
	rt.tenant("GET /api/v2/tenants/{tid}/execution/models", h.models)
	rt.tenant("GET /api/v2/tenants/{tid}/execution/executors", h.executors)
	rt.tenant("GET /api/v2/tenants/{tid}/schedule-configs", h.scheduleConfigs)
	rt.tenant("GET /api/v2/tenants/{tid}/schedule-resources", h.scheduleResources)
	rt.tenant("GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs", h.scheduleHistory)
	rt.tenant("GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}", h.scheduleHistory)
	rt.tenant("GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}/result", h.scheduleResult)
	rt.tenant("POST /api/v2/tenants/{tid}/schedule-configs", h.scheduleConfigs)
	rt.tenant("PUT /api/v2/tenants/{tid}/schedule-configs/{id}", h.scheduleConfigs)
	rt.tenant("PATCH /api/v2/tenants/{tid}/schedule-configs/{id}/enabled", h.scheduleConfigs)
	rt.tenant("POST /api/v2/tenants/{tid}/schedule-configs/{id}/run", h.scheduleAction)
	rt.tenant("POST /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}/cancel", h.scheduleAction)
	rt.tenant("GET /api/v2/tenants/{tid}/squads", h.listSquads)
	rt.tenant("POST /api/v2/tenants/{tid}/squads", h.createSquad)
	rt.tenant("GET /api/v2/tenants/{tid}/squads/{sid}", h.getSquad)
	rt.tenant("PUT /api/v2/tenants/{tid}/squads/{sid}", h.updateSquad)
	rt.tenant("POST /api/v2/tenants/{tid}/squads/{sid}/archive", h.archiveSquad)
	rt.tenant("POST /api/v2/tenants/{tid}/squads/{sid}/members", h.addSquadMember)
	rt.tenant("PUT /api/v2/tenants/{tid}/squads/{sid}/members/{mid}", h.updateSquadMember)
	rt.tenant("DELETE /api/v2/tenants/{tid}/squads/{sid}/members/{mid}", h.removeSquadMember)
	rt.tenant("GET /api/v2/tenants/{tid}/tasks", h.listTasks)
	rt.tenant("POST /api/v2/tenants/{tid}/tasks", h.createTask)
	rt.tenant("GET /api/v2/tenants/{tid}/tasks/{taskid}", h.getTask)
	rt.tenant("PUT /api/v2/tenants/{tid}/tasks/{taskid}", h.updateTask)
	rt.tenant("POST /api/v2/tenants/{tid}/tasks/{taskid}/status", h.setTaskStatus)
	rt.tenant("PUT /api/v2/tenants/{tid}/tasks/{taskid}/assignment", h.setTaskAssignee)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows", h.listWorkflows)
	rt.tenant("POST /api/v2/tenants/{tid}/workflows", h.createWorkflow)
	rt.tenant("POST /api/v2/tenants/{tid}/workflows/analyze", h.analyzeWorkflowGraph)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows/{wid}", h.getWorkflow)
	rt.tenant("PUT /api/v2/tenants/{tid}/workflows/{wid}", h.updateWorkflow)
	rt.tenant("DELETE /api/v2/tenants/{tid}/workflows/{wid}", h.deleteWorkflow)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows/{wid}/draft", h.getWorkflowDraft)
	rt.tenant("PUT /api/v2/tenants/{tid}/workflows/{wid}/draft", h.updateWorkflowDraft)
	rt.tenant("POST /api/v2/tenants/{tid}/workflows/{wid}/publish", h.publishWorkflow)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows/{wid}/versions", h.listWorkflowVersions)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows/{wid}/versions/{version}", h.getWorkflowVersion)
	rt.tenant("DELETE /api/v2/tenants/{tid}/workflows/{wid}/versions/{version}", h.deleteWorkflowVersion)
	rt.tenant("POST /api/v2/tenants/{tid}/workflows/{wid}/activate", h.activateWorkflow)
	rt.tenant("POST /api/v2/tenants/{tid}/workflows/{wid}/rollback", h.rollbackWorkflow)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows/{wid}/snapshots/{sid}", h.getWorkflowSnapshot)
	rt.tenant("POST /api/v2/tenants/{tid}/workflows/{wid}/runs", h.createWorkflowRun)
	rt.tenant("GET /api/v2/tenants/{tid}/workflows/{wid}/runs", h.listWorkflowRuns)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs", h.listWorkflowRuns)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/form/retry", h.workflowForm)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/form", h.workflowForm)
	rt.tenant("PUT /api/v2/tenants/{tid}/workflow-runs/{rid}/form", h.workflowForm)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/form", h.workflowForm)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}", h.getWorkflowRun)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/snapshot", h.getWorkflowRunSnapshot)
	rt.tenant("DELETE /api/v2/tenants/{tid}/workflow-runs/{rid}", h.deleteWorkflowRun)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/start", h.startWorkflowRun)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/cancel", h.cancelWorkflowRun)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/resume-preview", h.previewWorkflowRunResume)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/resume", h.resumeWorkflowRun)
	rt.tenant("PATCH /api/v2/tenants/{tid}/workflow-runs/{rid}/input", h.updateWorkflowRunInput)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeId}/complete", h.completeWorkflowNode)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeId}/turns", h.sendWorkflowNodeTurn)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeId}/stop", h.stopWorkflowNodeTurn)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes", h.listWorkflowRunNodes)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/events", h.workflowRunEvents)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeRunId}/events", h.workflowNodeEvents)
	rt.tenant("GET /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeRunId}/mcp", h.workflowNodeMCP)
	rt.tenant("POST /api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeRunId}/mcp/probe", h.workflowNodeMCP)
	h.chatRoutes(rt)
	h.dashboardRoutes(rt)
	h.workspaceRoutes(rt)
	h.taskRunRoutes(rt)
	h.notificationRoutes(rt)
	h.taskDecompositionRoutes(rt)
	h.taskWorkflowRoutes(rt)
	rt.public("/api/", func(w http.ResponseWriter, _ *http.Request) { fault(w, http.StatusNotFound, "not_found") })
	return rt
}

type handler struct {
	store Store
	opts  Options
	// chats runs the chat routes on the injected execution service and catalog; nil when the store
	// cannot keep conversations.
	chats *chat.Service
	// verdicts judges and caches the decomposition picker's verdict per immutable published
	// snapshot, shared by every request this handler serves.
	verdicts *decompositionVerdicts
	// patchSlots bounds how many delivery patches are built at once: each holds its whole patch,
	// up to the cap, in memory plus a Git process.
	patchSlots chan struct{}
	// attempts holds the redirect logins in flight, between POST /auth/login and the callback.
	attempts *loginAttempts
	// desktopCodes holds approved desktop logins until the desktop exchanges their code.
	desktopCodes *desktopCodes
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
	lifetime := o.Context
	if lifetime == nil {
		lifetime = context.Background()
	}
	h := &handler{store: s, opts: o, verdicts: newDecompositionVerdicts(lifetime), patchSlots: make(chan struct{}, maxConcurrentPatches), attempts: newLoginAttempts(), desktopCodes: newDesktopCodes()}
	if cs, ok := s.(chat.Store); ok {
		h.chats = chat.New(cs, o.Execution, o.Catalog, chat.Options{Reclaims: o.WorkspaceReclaims})
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

// localPath keeps a post-login redirect on this origin. Control characters are refused too:
// browsers strip tabs and newlines from a Location, so "/\t/evil.example" would become
// "//evil.example".
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") ||
		strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
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
	faultWithParams(w, status, code, map[string]any{})
}

func faultWithParams(w http.ResponseWriter, status int, code string, params any) {
	writeJSON(w, status, map[string]any{"code": code, "params": params, "requestId": requestID()})
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
