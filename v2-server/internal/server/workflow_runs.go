package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// WorkflowRunStore is what the run routes need. It extends the definition store so one membership
// check and the same tenant scoping cover both, while the run lifecycle stays in its own interface.
type WorkflowRunStore interface {
	WorkflowStore
	CreateWorkflowRun(context.Context, string, string, string, store.WorkflowRunInput) (store.WorkflowRun, error)
	WorkflowRuns(context.Context, string, string, string) ([]store.WorkflowRunSummary, error)
	WorkflowRun(context.Context, string, string) (store.WorkflowRun, error)
	WorkflowRunNodes(context.Context, string, string) ([]store.WorkflowNodeRun, error)
	WorkflowRunScopes(context.Context, string, string) ([]store.WorkflowScope, error)
}

type workflowRunView struct {
	ID         string  `json:"id"`
	WorkflowID string  `json:"workflowId"`
	SnapshotID string  `json:"snapshotId"`
	ProjectID  string  `json:"projectId"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	State      *string `json:"state"`
	Input      *string `json:"input"`
	Output     *string `json:"output"`
	Error      *string `json:"error"`
	StartedAt  *int64  `json:"startedAt"`
	FinishedAt *int64  `json:"finishedAt"`
	CreatedAt  int64   `json:"createdAt"`
	UpdatedAt  int64   `json:"updatedAt"`
}

type workflowRunSummaryView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ProjectID  string `json:"projectId"`
	WorkflowID string `json:"workflowId"`
	Version    string `json:"version"`
	Status     string `json:"status"`
	StartedAt  *int64 `json:"startedAt"`
	FinishedAt *int64 `json:"finishedAt"`
	CreatedAt  int64  `json:"createdAt"`
}

type workflowNodeRunView struct {
	ID         string  `json:"id"`
	RunID      string  `json:"runId"`
	ScopeID    string  `json:"scopeId"`
	NodeID     string  `json:"nodeId"`
	NodeType   string  `json:"nodeType"`
	SessionID  *string `json:"sessionId"`
	Status     string  `json:"status"`
	Input      *string `json:"input"`
	Output     *string `json:"output"`
	Error      *string `json:"error"`
	Payload    *string `json:"payload"`
	Iteration  *int    `json:"iteration"`
	StartedAt  *int64  `json:"startedAt"`
	FinishedAt *int64  `json:"finishedAt"`
	CreatedAt  int64   `json:"createdAt"`
	UpdatedAt  int64   `json:"updatedAt"`
}

type workflowScopeView struct {
	ID                  string  `json:"id"`
	RunID               string  `json:"runId"`
	ParentLoopNodeRunID string  `json:"parentLoopNodeRunId"`
	RoundIndex          int     `json:"roundIndex"`
	Status              string  `json:"status"`
	State               *string `json:"state"`
	CreatedAt           int64   `json:"createdAt"`
	UpdatedAt           int64   `json:"updatedAt"`
}

func millis(t time.Time) int64 { return t.UnixMilli() }

func millisPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	value := t.UnixMilli()
	return &value
}

func optionalString(raw []byte) *string {
	if raw == nil {
		return nil
	}
	value := string(raw)
	return &value
}

func toWorkflowRunView(run store.WorkflowRun) workflowRunView {
	return workflowRunView{
		ID:         run.ID,
		WorkflowID: run.WorkflowID,
		SnapshotID: run.SnapshotID,
		ProjectID:  run.ProjectID,
		Name:       run.Name,
		Status:     run.Status,
		State:      optionalString(run.State),
		Input:      run.Input,
		Output:     run.Output,
		Error:      run.Error,
		StartedAt:  millisPtr(run.StartedAt),
		FinishedAt: millisPtr(run.FinishedAt),
		CreatedAt:  millis(run.CreatedAt),
		UpdatedAt:  millis(run.UpdatedAt),
	}
}

func toWorkflowRunSummaryView(run store.WorkflowRunSummary) workflowRunSummaryView {
	return workflowRunSummaryView{
		ID:         run.ID,
		Name:       run.Name,
		ProjectID:  run.ProjectID,
		WorkflowID: run.WorkflowID,
		Version:    run.Version,
		Status:     run.Status,
		StartedAt:  millisPtr(run.StartedAt),
		FinishedAt: millisPtr(run.FinishedAt),
		CreatedAt:  millis(run.CreatedAt),
	}
}

func toWorkflowNodeRunView(node store.WorkflowNodeRun) workflowNodeRunView {
	return workflowNodeRunView{
		ID:         node.ID,
		RunID:      node.RunID,
		ScopeID:    node.ScopeID,
		NodeID:     node.NodeID,
		NodeType:   node.NodeType,
		SessionID:  node.SessionID,
		Status:     node.Status,
		Input:      node.Input,
		Output:     node.Output,
		Error:      node.Error,
		Payload:    optionalString(node.Payload),
		Iteration:  node.Iteration,
		StartedAt:  millisPtr(node.StartedAt),
		FinishedAt: millisPtr(node.FinishedAt),
		CreatedAt:  millis(node.CreatedAt),
		UpdatedAt:  millis(node.UpdatedAt),
	}
}

func toWorkflowScopeView(scope store.WorkflowScope) workflowScopeView {
	view := workflowScopeView{
		ID:        scope.ID,
		RunID:     scope.RunID,
		State:     optionalString(scope.State),
		CreatedAt: millis(scope.CreatedAt),
		UpdatedAt: millis(scope.UpdatedAt),
	}
	if scope.ParentLoopNodeRunID != nil {
		view.ParentLoopNodeRunID = *scope.ParentLoopNodeRunID
	}
	if scope.RoundIndex != nil {
		view.RoundIndex = *scope.RoundIndex
	}
	if scope.Status != nil {
		view.Status = *scope.Status
	}
	return view
}

// workflowRunScope resolves membership once and then requires the run half of the workflow store.
func (h *handler) workflowRunScope(w http.ResponseWriter, r *http.Request, u store.User) (WorkflowRunStore, string, bool) {
	base, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return nil, "", false
	}
	s, ok := base.(WorkflowRunStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "workflow_run_store_unavailable")
		return nil, "", false
	}
	return s, tenant, true
}

// createWorkflowRun freezes the project and one published version into a pending run. Start stays
// with the execution bridge: HTTP only records the intent, never the graph copy or a fake status.
func (h *handler) createWorkflowRun(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowRunScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	var in struct {
		ProjectID   string  `json:"projectId"`
		SnapshotID  string  `json:"snapshotId"`
		Input       *string `json:"input"`
		Name        string  `json:"name"`
		TriggerKind string  `json:"triggerKind"`
		TriggerRef  string  `json:"triggerRef"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ProjectID) == "" {
		fault(w, http.StatusBadRequest, "invalid_configuration")
		return
	}
	if in.TriggerKind != "" && in.TriggerKind != "manual" && in.TriggerKind != "schedule" {
		fault(w, http.StatusBadRequest, "invalid_configuration")
		return
	}
	if in.SnapshotID != "" && !workflowUUID.MatchString(in.SnapshotID) {
		fault(w, http.StatusNotFound, "workflow_snapshot_not_found")
		return
	}
	run, err := s.CreateWorkflowRun(r.Context(), tenant, id, in.SnapshotID, store.WorkflowRunInput{
		ProjectID:   in.ProjectID,
		Name:        strings.TrimSpace(in.Name),
		Input:       in.Input,
		TriggerKind: in.TriggerKind,
		TriggerRef:  in.TriggerRef,
	})
	if err != nil {
		workflowRunFault(w, err, "create workflow run")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run": toWorkflowRunView(run)})
}

// listWorkflowRuns serves both the definition history and the tenant/project history, so the run
// dialog and the workspace list share one shape.
func (h *handler) listWorkflowRuns(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowRunScope(w, r, u)
	if !ok {
		return
	}
	workflowID := r.PathValue("wid")
	if workflowID != "" {
		if !workflowUUID.MatchString(workflowID) {
			fault(w, http.StatusNotFound, "workflow_not_found")
			return
		}
		if _, err := s.Workflow(r.Context(), tenant, workflowID); err != nil {
			workflowRunFault(w, err, "list workflow runs")
			return
		}
	}
	projectID := r.URL.Query().Get("projectId")
	runs, err := s.WorkflowRuns(r.Context(), tenant, workflowID, projectID)
	if err != nil {
		workflowRunFault(w, err, "list workflow runs")
		return
	}
	views := make([]workflowRunSummaryView, 0, len(runs))
	for _, run := range runs {
		views = append(views, toWorkflowRunSummaryView(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": views})
}

func (h *handler) getWorkflowRun(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowRunScope(w, r, u)
	if !ok {
		return
	}
	runID := r.PathValue("rid")
	if !workflowUUID.MatchString(runID) {
		fault(w, http.StatusNotFound, "workflow_run_not_found")
		return
	}
	run, err := s.WorkflowRun(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "get workflow run")
		return
	}
	nodes, err := s.WorkflowRunNodes(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "get workflow run nodes")
		return
	}
	scopes, err := s.WorkflowRunScopes(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "get workflow run scopes")
		return
	}
	nodeViews := make([]workflowNodeRunView, 0, len(nodes))
	for _, node := range nodes {
		nodeViews = append(nodeViews, toWorkflowNodeRunView(node))
	}
	scopeViews := make([]workflowScopeView, 0, len(scopes))
	for _, scope := range scopes {
		scopeViews = append(scopeViews, toWorkflowScopeView(scope))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run":    toWorkflowRunView(run),
		"nodes":  nodeViews,
		"scopes": scopeViews,
	})
}

func (h *handler) listWorkflowRunNodes(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowRunScope(w, r, u)
	if !ok {
		return
	}
	runID := r.PathValue("rid")
	if !workflowUUID.MatchString(runID) {
		fault(w, http.StatusNotFound, "workflow_run_not_found")
		return
	}
	if _, err := s.WorkflowRun(r.Context(), tenant, runID); err != nil {
		workflowRunFault(w, err, "list workflow run nodes")
		return
	}
	nodes, err := s.WorkflowRunNodes(r.Context(), tenant, runID)
	if err != nil {
		workflowRunFault(w, err, "list workflow run nodes")
		return
	}
	views := make([]workflowNodeRunView, 0, len(nodes))
	for _, node := range nodes {
		views = append(views, toWorkflowNodeRunView(node))
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}

// workflowRunFault maps run refusals onto the contract's stable codes and leaves definition errors
// to the shared definition mapping.
func workflowRunFault(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, store.ErrWorkflowRunNotFound):
		fault(w, http.StatusNotFound, "workflow_run_not_found")
	case errors.Is(err, store.ErrWorkflowNoPublishedSnapshot):
		fault(w, http.StatusBadRequest, "workflow_no_published_snapshot")
	case errors.Is(err, store.ErrWorkflowDraftSnapshot):
		fault(w, http.StatusBadRequest, "workflow_run_cannot_use_draft_snapshot")
	case errors.Is(err, store.ErrProjectNotFound):
		fault(w, http.StatusNotFound, "project_not_found")
	case errors.Is(err, store.ErrWorkflowRunState):
		fault(w, http.StatusConflict, "workflow_run_state_conflict")
	default:
		workflowFault(w, err, op, "")
	}
}
