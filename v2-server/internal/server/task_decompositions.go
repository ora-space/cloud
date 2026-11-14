package server

import (
	"errors"
	"net/http"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/taskdecomp"
)

func (h *handler) taskDecompositionRoutes(rt *router) {
	base := "/api/v2/tenants/{tid}/tasks/{taskid}/decompositions"
	rt.tenant("GET "+base, h.listTaskDecompositions)
	rt.tenant("POST "+base, h.startTaskDecomposition)
	rt.tenant("GET /api/v2/tenants/{tid}/decomposition-workflows", h.listDecompositionWorkflows)
}

// taskDecompositionScope checks the task id and that decompositions are wired.
func (h *handler) taskDecompositionScope(w http.ResponseWriter, r *http.Request, a access) (*taskdecomp.Host, string, string, bool) {
	if h.opts.TaskDecompositions == nil {
		fault(w, http.StatusServiceUnavailable, "workflow_run_host_unavailable")
		return nil, "", "", false
	}
	tenant, id := a.TenantID, r.PathValue("taskid")
	if !taskUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "not_found")
		return nil, "", "", false
	}
	return h.opts.TaskDecompositions, tenant, id, true
}

// taskDecompositionFault maps the decomposition host's and the task store's errors.
func taskDecompositionFault(w http.ResponseWriter, op string, e error) {
	switch {
	case errors.Is(e, store.ErrDecompositionActive):
		fault(w, http.StatusConflict, "decomposition_active")
	case errors.Is(e, taskdecomp.ErrInvalidWorkflow):
		fault(w, http.StatusBadRequest, "invalid_workflow")
	case errors.Is(e, taskdecomp.ErrWorkflowOutputRequired):
		faultWithParams(w, http.StatusBadRequest, "decomposition_output_required", map[string]string{"message": taskdecomp.OutputRequirement})
	case errors.Is(e, taskdecomp.ErrTaskClosed):
		fault(w, http.StatusConflict, "task_closed")
	case errors.Is(e, taskdecomp.ErrSubtask):
		fault(w, http.StatusConflict, "subtask_not_decomposable")
	case taskFault(w, e):
	default:
		internal(w, op, e)
	}
}

func (h *handler) listTaskDecompositions(w http.ResponseWriter, r *http.Request, a access) {
	host, tenant, id, ok := h.taskDecompositionScope(w, r, a)
	if !ok {
		return
	}
	items, e := host.List(r.Context(), tenant, id)
	if e != nil {
		taskDecompositionFault(w, "list task decompositions", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// startTaskDecomposition runs a published workflow over the task; its `tasks` output becomes the
// task's subtasks once the run succeeds.
func (h *handler) startTaskDecomposition(w http.ResponseWriter, r *http.Request, a access) {
	host, tenant, id, ok := h.taskDecompositionScope(w, r, a)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var in struct {
		WorkflowID string `json:"workflowId"`
	}
	if !decodeStrict(w, r, &in) {
		return
	}
	if !workflowUUID.MatchString(in.WorkflowID) {
		fault(w, http.StatusBadRequest, "invalid_workflow")
		return
	}
	d, e := host.Start(r.Context(), tenant, a.UserID, id, in.WorkflowID, key)
	if e != nil {
		taskDecompositionFault(w, "start task decomposition", e)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}
