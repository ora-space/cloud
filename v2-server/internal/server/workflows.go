package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// WorkflowStore is what the definition routes need from the database. It is asserted from the
// server's store so the login and member routes stay independent of workflow storage.
type WorkflowStore interface {
	Member(context.Context, string, string) (bool, error)
	Workflows(context.Context, string) ([]store.WorkflowSummary, error)
	Workflow(context.Context, string, string) (store.Workflow, error)
	CreateWorkflow(context.Context, string, string, string) (store.Workflow, store.WorkflowSnapshot, error)
	UpdateWorkflowName(context.Context, string, string, string) (store.Workflow, error)
	UpdateWorkflowDraft(context.Context, string, string, string) (store.WorkflowSnapshot, error)
	WorkflowDraft(context.Context, string, string) (store.WorkflowSnapshot, error)
	WorkflowSnapshot(context.Context, string, string, string) (store.WorkflowSnapshot, error)
	PublishWorkflow(context.Context, string, string, string, func(string) error) (store.WorkflowSnapshot, error)
	RollbackWorkflow(context.Context, string, string, string) (store.WorkflowSnapshot, error)
	ActivateWorkflow(context.Context, string, string, string) (store.WorkflowSnapshot, error)
	ListWorkflowVersions(context.Context, string, string) ([]store.WorkflowVersion, error)
	WorkflowVersion(context.Context, string, string, string) (store.WorkflowSnapshot, error)
	DeleteWorkflowSnapshot(context.Context, string, string, string) (store.WorkflowVersion, error)
	DeleteWorkflow(context.Context, string, string) error
}

// WorkflowAnalysis is the authoring participation the canvas explains: nodes the execution path
// never reaches, and node kinds this version cannot run.
type WorkflowAnalysis struct {
	UnusedNodeIDs     []string
	UnrecognizedNodes []UnrecognizedWorkflowNode
}

// UnrecognizedWorkflowNode names a node whose kind the canvas or engine does not register.
type UnrecognizedWorkflowNode struct {
	NodeID string
	Kind   string
}

var workflowUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// defaultWorkflowGraph is desktop's initial document for a definition created without one: a single
// Start node the editor can build on.
const defaultWorkflowGraph = `{"nodes":[{"id":"start","type":"workflow","deletable":false,"position":{"x":120,"y":260},"data":{"kind":"start","title":"开始","description":"接收工作流输入"}}],"edges":[]}`

// maxWorkflowGraphBytes bounds documents; the editor refuses to import anything larger than 5 MiB.
const maxWorkflowGraphBytes = 6 << 20

type workflowView struct {
	ID                  string  `json:"id"`
	Namespace           string  `json:"namespace"`
	Name                string  `json:"name"`
	PublishedSnapshotID *string `json:"publishedSnapshotId"`
	CreatedAt           int64   `json:"createdAt"`
	UpdatedAt           int64   `json:"updatedAt"`
}

type workflowSummaryView struct {
	ID               string  `json:"id"`
	Namespace        string  `json:"namespace"`
	Name             string  `json:"name"`
	PublishedVersion *string `json:"publishedVersion"`
	CreatedAt        int64   `json:"createdAt"`
	UpdatedAt        int64   `json:"updatedAt"`
}

type workflowSnapshotView struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflowId"`
	Version    string `json:"version"`
	Graph      string `json:"graph"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  *int64 `json:"updatedAt"`
}

type workflowVersionView struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	CreatedAt int64  `json:"createdAt"`
}

type unrecognizedWorkflowNodeView struct {
	NodeID string `json:"nodeId"`
	Kind   string `json:"kind"`
}

func toWorkflowView(w store.Workflow) workflowView {
	return workflowView{
		ID:                  w.ID,
		Namespace:           w.Namespace,
		Name:                w.Name,
		PublishedSnapshotID: w.PublishedSnapshotID,
		CreatedAt:           w.CreatedAt.UnixMilli(),
		UpdatedAt:           w.UpdatedAt.UnixMilli(),
	}
}

func toWorkflowSummaryView(w store.WorkflowSummary) workflowSummaryView {
	return workflowSummaryView{
		ID:               w.ID,
		Namespace:        w.Namespace,
		Name:             w.Name,
		PublishedVersion: w.PublishedVersion,
		CreatedAt:        w.CreatedAt.UnixMilli(),
		UpdatedAt:        w.UpdatedAt.UnixMilli(),
	}
}

func toWorkflowSnapshotView(s store.WorkflowSnapshot) workflowSnapshotView {
	view := workflowSnapshotView{
		ID:         s.ID,
		WorkflowID: s.WorkflowID,
		Version:    s.Version,
		Graph:      s.Graph,
		CreatedAt:  s.CreatedAt.UnixMilli(),
	}
	if s.UpdatedAt != nil {
		millis := s.UpdatedAt.UnixMilli()
		view.UpdatedAt = &millis
	}
	return view
}

// decodeWorkflow reads one strict JSON body: unknown fields, trailing content, and oversized bodies
// are refused instead of being silently accepted.
func decodeWorkflow(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWorkflowGraphBytes))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fault(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}

// workflowScope resolves the workflow store, the tenant, and current membership before any handler
// touches a definition. Authorization failures answer 404 so hidden tenants stay indistinguishable.
func (h *handler) workflowScope(w http.ResponseWriter, r *http.Request, u store.User) (WorkflowStore, string, bool) {
	s, ok := h.store.(WorkflowStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workflow_store_unavailable")
		return nil, "", false
	}
	tenant := r.PathValue("tid")
	if !workflowUUID.MatchString(tenant) {
		fault(w, http.StatusNotFound, "not_found")
		return nil, "", false
	}
	member, err := s.Member(r.Context(), tenant, u.ID)
	if err != nil {
		internal(w, "workflow membership", err)
		return nil, "", false
	}
	if !member {
		fault(w, http.StatusNotFound, "not_found")
		return nil, "", false
	}
	return s, tenant, true
}

// workflowID reads the definition id from the route and answers 404 for anything that cannot be one.
func workflowID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("wid")
	if !workflowUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "workflow_not_found")
		return "", false
	}
	return id, true
}

// workflowFault maps domain refusals onto the contract's stable codes. draftSource is the code an
// operation uses when its target is the draft itself, because rollback and activate name that
// refusal differently.
func workflowFault(w http.ResponseWriter, err error, op, draftSource string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		fault(w, http.StatusNotFound, "workflow_not_found")
	case errors.Is(err, store.ErrWorkflowSnapshotNotFound):
		fault(w, http.StatusNotFound, "workflow_snapshot_not_found")
	case errors.Is(err, store.ErrWorkflowNameConflict):
		fault(w, http.StatusConflict, "workflow_name_conflict")
	case errors.Is(err, store.ErrWorkflowVersionExists):
		fault(w, http.StatusConflict, "workflow_version_already_exists")
	case errors.Is(err, store.ErrWorkflowDraftSource) && draftSource != "":
		fault(w, http.StatusBadRequest, draftSource)
	case errors.Is(err, store.ErrWorkflowDraftDeletion):
		fault(w, http.StatusBadRequest, "workflow_cannot_delete_draft")
	case errors.Is(err, store.ErrWorkflowActiveVersion):
		fault(w, http.StatusBadRequest, "workflow_cannot_delete_active_version")
	case errors.Is(err, store.ErrWorkflowSnapshotInUse):
		fault(w, http.StatusBadRequest, "workflow_snapshot_in_use")
	case errors.Is(err, store.ErrWorkflowActiveRuns):
		fault(w, http.StatusBadRequest, "workflow_active_runs")
	case errors.Is(err, store.ErrWorkflowDocumentInvalid):
		fault(w, http.StatusBadRequest, "workflow_run_graph_parse")
	case errors.Is(err, errWorkflowDomainUnavailable):
		fault(w, http.StatusServiceUnavailable, "workflow_domain_unavailable")
	default:
		internal(w, op, err)
	}
}

func (h *handler) listWorkflows(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	summaries, err := s.Workflows(r.Context(), tenant)
	if err != nil {
		workflowFault(w, err, "list workflows", "")
		return
	}
	views := make([]workflowSummaryView, 0, len(summaries))
	for _, summary := range summaries {
		views = append(views, toWorkflowSummaryView(summary))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": views})
}

func (h *handler) createWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	var in struct {
		Name  string  `json:"name"`
		Graph *string `json:"graph"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		fault(w, http.StatusBadRequest, "workflow_name_blank")
		return
	}
	graph := defaultWorkflowGraph
	if in.Graph != nil {
		graph = *in.Graph
	}
	workflow, draft, err := s.CreateWorkflow(r.Context(), tenant, name, graph)
	if err != nil {
		workflowFault(w, err, "create workflow", "")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"workflow": toWorkflowView(workflow),
		"draft":    toWorkflowSnapshotView(draft),
	})
}

func (h *handler) getWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	workflow, err := s.Workflow(r.Context(), tenant, id)
	if err != nil {
		workflowFault(w, err, "get workflow", "")
		return
	}
	draft, err := s.WorkflowDraft(r.Context(), tenant, id)
	if err != nil {
		workflowFault(w, err, "get workflow draft", "")
		return
	}
	var published *workflowSnapshotView
	if workflow.PublishedSnapshotID != nil {
		snapshot, err := s.WorkflowSnapshot(r.Context(), tenant, id, *workflow.PublishedSnapshotID)
		if err != nil {
			workflowFault(w, err, "get published version", "")
			return
		}
		view := toWorkflowSnapshotView(snapshot)
		published = &view
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workflow":  toWorkflowView(workflow),
		"draft":     toWorkflowSnapshotView(draft),
		"published": published,
	})
}

func (h *handler) updateWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		fault(w, http.StatusBadRequest, "workflow_name_blank")
		return
	}
	workflow, err := s.UpdateWorkflowName(r.Context(), tenant, id, name)
	if err != nil {
		workflowFault(w, err, "rename workflow", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflow": toWorkflowView(workflow)})
}

func (h *handler) deleteWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	if err := s.DeleteWorkflow(r.Context(), tenant, id); err != nil {
		workflowFault(w, err, "delete workflow", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflowId": id})
}

func (h *handler) getWorkflowDraft(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	if _, err := s.Workflow(r.Context(), tenant, id); err != nil {
		workflowFault(w, err, "get workflow", "")
		return
	}
	draft, err := s.WorkflowDraft(r.Context(), tenant, id)
	if err != nil {
		workflowFault(w, err, "get draft", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(draft)})
}

func (h *handler) updateWorkflowDraft(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	var in struct {
		Graph string `json:"graph"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if in.Graph == "" {
		// The contract requires the graph string; accepting an absent one would overwrite a saved
		// draft with an empty document the editor never sent.
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	draft, err := s.UpdateWorkflowDraft(r.Context(), tenant, id, in.Graph)
	if err != nil {
		workflowFault(w, err, "save draft", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(draft)})
}
