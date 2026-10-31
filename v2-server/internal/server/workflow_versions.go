package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// analyzeWorkflow derives participation without persisting or executing the document; publish uses
// validateWorkflow to refuse a document that could not execute. Both belong to the workflow domain
// and are injected, so the HTTP layer never reimplements graph rules.
func (h *handler) analyzeWorkflow(graph string) (WorkflowAnalysis, error) {
	if h.opts.AnalyzeWorkflow == nil {
		return WorkflowAnalysis{}, errWorkflowDomainUnavailable
	}
	return h.opts.AnalyzeWorkflow(graph)
}

func (h *handler) validateWorkflow(graph string) error {
	if h.opts.ValidateWorkflow == nil {
		return errWorkflowDomainUnavailable
	}
	return h.opts.ValidateWorkflow(graph)
}

var errWorkflowDomainUnavailable = errors.New("workflow domain unavailable")

// now reads the injectable clock so automatic publish versions stay deterministic in tests.
func (h *handler) now() time.Time {
	if h.opts.Now != nil {
		return h.opts.Now()
	}
	return time.Now()
}

// maxWorkflowVersionBytes keeps a published version addressable as one URL path segment, matching
// desktop's contract.
const maxWorkflowVersionBytes = 128

// maxAutomaticVersionRetries bounds how often an automatic publish retries a colliding version.
const maxAutomaticVersionRetries = 100

func (h *handler) publishWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	var in struct {
		Version *string `json:"version"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	// One clock reading drives the automatic version and every collision retry, so a slow publish
	// cannot silently change the name it was trying to freeze under.
	published := h.now()
	automatic := in.Version == nil
	var version string
	if automatic {
		version = fmt.Sprintf("v%d", published.UnixMilli())
	} else {
		version = *in.Version
		if version == store.WorkflowDraftVersion {
			fault(w, http.StatusBadRequest, "workflow_version_reserved")
			return
		}
		if !validWorkflowVersion(version) {
			fault(w, http.StatusBadRequest, "workflow_version_invalid")
			return
		}
	}
	if h.opts.ValidateWorkflow == nil {
		fault(w, http.StatusServiceUnavailable, "workflow_domain_unavailable")
		return
	}
	for attempt := 0; ; attempt++ {
		snapshot, err := s.PublishWorkflow(r.Context(), tenant, id, version, h.validateWorkflow)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(snapshot)})
			return
		}
		if errors.Is(err, store.ErrWorkflowVersionExists) && automatic && attempt < maxAutomaticVersionRetries {
			version = fmt.Sprintf("v%d-%d", published.UnixMilli(), attempt+1)
			continue
		}
		workflowFault(w, err, "publish workflow", "")
		return
	}
}

func (h *handler) listWorkflowVersions(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	versions, err := s.ListWorkflowVersions(r.Context(), tenant, id)
	if err != nil {
		workflowFault(w, err, "list workflow versions", "")
		return
	}
	views := make([]workflowVersionView, 0, len(versions))
	for _, version := range versions {
		views = append(views, workflowVersionView{ID: version.ID, Version: version.Version, CreatedAt: version.CreatedAt.UnixMilli()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": views})
}

func (h *handler) getWorkflowVersion(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	snapshot, err := s.WorkflowVersion(r.Context(), tenant, id, r.PathValue("version"))
	if err != nil {
		workflowFault(w, err, "get workflow version", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(snapshot)})
}

func (h *handler) getWorkflowSnapshot(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	snapshotID := r.PathValue("sid")
	if !workflowUUID.MatchString(snapshotID) {
		fault(w, http.StatusNotFound, "workflow_snapshot_not_found")
		return
	}
	snapshot, err := s.WorkflowSnapshot(r.Context(), tenant, id, snapshotID)
	if err != nil {
		workflowFault(w, err, "get workflow snapshot", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(snapshot)})
}

func (h *handler) deleteWorkflowVersion(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	deleted, err := s.DeleteWorkflowSnapshot(r.Context(), tenant, id, r.PathValue("version"))
	if err != nil {
		workflowFault(w, err, "delete workflow version", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshotId": deleted.ID, "version": deleted.Version})
}

func (h *handler) rollbackWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	var in struct {
		SnapshotID string `json:"snapshotId"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if !workflowUUID.MatchString(in.SnapshotID) {
		fault(w, http.StatusNotFound, "workflow_snapshot_not_found")
		return
	}
	draft, err := s.RollbackWorkflow(r.Context(), tenant, id, in.SnapshotID)
	if err != nil {
		workflowFault(w, err, "rollback workflow", "workflow_cannot_rollback_to_draft")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(draft)})
}

func (h *handler) activateWorkflow(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return
	}
	id, ok := workflowID(w, r)
	if !ok {
		return
	}
	var in struct {
		SnapshotID string `json:"snapshotId"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if !workflowUUID.MatchString(in.SnapshotID) {
		fault(w, http.StatusNotFound, "workflow_snapshot_not_found")
		return
	}
	draft, err := s.ActivateWorkflow(r.Context(), tenant, id, in.SnapshotID)
	if err != nil {
		workflowFault(w, err, "activate workflow", "workflow_cannot_activate_draft")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": toWorkflowSnapshotView(draft)})
}

func (h *handler) analyzeWorkflowGraph(w http.ResponseWriter, r *http.Request, u store.User) {
	if _, _, ok := h.workflowScope(w, r, u); !ok {
		return
	}
	var in struct {
		Graph string `json:"graph"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if in.Graph == "" {
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	analysis, err := h.analyzeWorkflow(in.Graph)
	if err != nil {
		workflowFault(w, err, "analyze workflow", "")
		return
	}
	unused := analysis.UnusedNodeIDs
	if unused == nil {
		unused = []string{}
	}
	unrecognized := make([]unrecognizedWorkflowNodeView, 0, len(analysis.UnrecognizedNodes))
	for _, node := range analysis.UnrecognizedNodes {
		unrecognized = append(unrecognized, unrecognizedWorkflowNodeView{NodeID: node.NodeID, Kind: node.Kind})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unusedNodeIds":     unused,
		"unrecognizedNodes": unrecognized,
	})
}

// validWorkflowVersion accepts one addressable URL path segment no longer than the contract bound.
// The reserved draft spelling is checked separately so publish reports it as its own refusal.
func validWorkflowVersion(version string) bool {
	if strings.TrimSpace(version) == "" || len(version) > maxWorkflowVersionBytes {
		return false
	}
	if version == "." || version == ".." {
		return false
	}
	for _, r := range version {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}
