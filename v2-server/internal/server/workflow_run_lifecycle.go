package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// WorkflowRunScope is the authenticated caller the execution bridge must authorize against. The
// bridge owns run execution; HTTP only proves the caller is a current member and passes this scope.
type WorkflowRunScope struct {
	TenantID string
	UserID   string
}

// WorkflowRunHost is the execution bridge's run lifecycle, injected by main. Dispatch, stop
// confirmation, session ownership and start recovery stay with the bridge; these operations are the
// boundary the run page and the schedule seam call. A nil host keeps the routes unavailable instead
// of pretending a run started.
type WorkflowRunHost interface {
	StartRun(context.Context, WorkflowRunScope, string) (store.WorkflowRun, error)
	CancelRun(context.Context, WorkflowRunScope, string) (store.WorkflowRun, error)
	UpdateRunInput(context.Context, WorkflowRunScope, string, *string, map[string]json.RawMessage) (store.WorkflowRun, error)
	CompleteNode(context.Context, WorkflowRunScope, string, string, bool, string) (store.WorkflowRun, error)
}

var (
	// ErrWorkflowRunNotStartable reports a run the bridge refused to start.
	ErrWorkflowRunNotStartable = errors.New("workflow run cannot start")
	// ErrWorkflowRunNotEditable reports input on a run past its pending window.
	ErrWorkflowRunNotEditable = errors.New("workflow run is not editable")
	// ErrWorkflowNodeNotAwaitingInput reports completion of a node that is not parked.
	ErrWorkflowNodeNotAwaitingInput = errors.New("workflow node is not awaiting input")
	// ErrWorkflowNodeNotFound reports a node absent from the run.
	ErrWorkflowNodeNotFound = errors.New("workflow node not found")
	// ErrWorkflowRunStopUnconfirmed reports a cancellation whose stop was not confirmed.
	ErrWorkflowRunStopUnconfirmed = errors.New("workflow run stop is unconfirmed")
)

// WorkflowRunInputInvalid names one rejected start variable, mirroring desktop's error params.
type WorkflowRunInputInvalid struct {
	Variable string
	Reason   string
}

func (e *WorkflowRunInputInvalid) Error() string {
	return fmt.Sprintf("workflow run input %q is invalid: %s", e.Variable, e.Reason)
}

// workflowRunHost resolves the caller's membership and requires the injected execution bridge. Both
// the run store and the bridge are optional, so an unwired deployment answers 503 rather than
// writing a run the process cannot execute.
func (h *handler) workflowRunHost(w http.ResponseWriter, r *http.Request, u store.User) (WorkflowRunHost, WorkflowRunScope, bool) {
	_, tenant, ok := h.workflowScope(w, r, u)
	if !ok {
		return nil, WorkflowRunScope{}, false
	}
	if h.opts.WorkflowRuns == nil {
		fault(w, http.StatusServiceUnavailable, "workflow_run_host_unavailable")
		return nil, WorkflowRunScope{}, false
	}
	return h.opts.WorkflowRuns, WorkflowRunScope{TenantID: tenant, UserID: u.ID}, true
}

// workflowRunID reads the run id from the route and answers 404 for anything that cannot be one.
func workflowRunID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("rid")
	if !workflowUUID.MatchString(id) {
		fault(w, http.StatusNotFound, "workflow_run_not_found")
		return "", false
	}
	return id, true
}

func (h *handler) startWorkflowRun(w http.ResponseWriter, r *http.Request, u store.User) {
	host, scope, ok := h.workflowRunHost(w, r, u)
	if !ok {
		return
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return
	}
	run, err := host.StartRun(r.Context(), scope, runID)
	if err != nil {
		workflowRunLifecycleFault(w, err, "start workflow run")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

func (h *handler) cancelWorkflowRun(w http.ResponseWriter, r *http.Request, u store.User) {
	host, scope, ok := h.workflowRunHost(w, r, u)
	if !ok {
		return
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return
	}
	run, err := host.CancelRun(r.Context(), scope, runID)
	if err != nil {
		workflowRunLifecycleFault(w, err, "cancel workflow run")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

func (h *handler) updateWorkflowRunInput(w http.ResponseWriter, r *http.Request, u store.User) {
	host, scope, ok := h.workflowRunHost(w, r, u)
	if !ok {
		return
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return
	}
	var in struct {
		Input     *string                    `json:"input"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	run, err := host.UpdateRunInput(r.Context(), scope, runID, in.Input, in.Variables)
	if err != nil {
		workflowRunLifecycleFault(w, err, "update workflow run input")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

func (h *handler) completeWorkflowNode(w http.ResponseWriter, r *http.Request, u store.User) {
	host, scope, ok := h.workflowRunHost(w, r, u)
	if !ok {
		return
	}
	runID, ok := workflowRunID(w, r)
	if !ok {
		return
	}
	nodeID := r.PathValue("nodeId")
	if nodeID == "" {
		fault(w, http.StatusNotFound, "workflow_node_not_found")
		return
	}
	var in struct {
		Force     bool   `json:"force"`
		Requester string `json:"requester"`
	}
	if !decodeWorkflow(w, r, &in) {
		return
	}
	if in.Requester != "" && in.Requester != "human" {
		fault(w, http.StatusBadRequest, "invalid_json")
		return
	}
	run, err := host.CompleteNode(r.Context(), scope, runID, nodeID, in.Force, in.Requester)
	if err != nil {
		workflowRunLifecycleFault(w, err, "complete workflow node")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": toWorkflowRunView(run)})
}

// workflowRunLifecycleFault maps bridge refusals onto desktop's stable codes; anything unrecognized
// stays a 500 so an implementation bug is not reported as a client mistake.
func workflowRunLifecycleFault(w http.ResponseWriter, err error, op string) {
	var invalid *WorkflowRunInputInvalid
	switch {
	case errors.Is(err, store.ErrWorkflowRunNotFound):
		fault(w, http.StatusNotFound, "workflow_run_not_found")
	case errors.Is(err, store.ErrWorkflowNodeRunNotFound):
		fault(w, http.StatusNotFound, "workflow_node_not_found")
	case errors.Is(err, ErrWorkflowRunNotStartable):
		fault(w, http.StatusBadRequest, "workflow_run_start_failed")
	case errors.Is(err, ErrWorkflowRunNotEditable):
		fault(w, http.StatusConflict, "workflow_run_not_editable")
	case errors.Is(err, ErrWorkflowNodeNotAwaitingInput):
		fault(w, http.StatusBadRequest, "workflow_node_not_awaiting_input")
	case errors.Is(err, ErrWorkflowNodeNotFound):
		fault(w, http.StatusNotFound, "workflow_node_not_found")
	case errors.Is(err, ErrWorkflowRunStopUnconfirmed):
		fault(w, http.StatusServiceUnavailable, "workflow_run_stop_unconfirmed")
	case errors.Is(err, store.ErrWorkflowRunState), errors.Is(err, store.ErrWorkflowNodeRunState):
		fault(w, http.StatusConflict, "workflow_run_state_conflict")
	case errors.As(err, &invalid):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"code":      "workflow_run_input_invalid",
			"params":    map[string]any{"variable": invalid.Variable, "reason": invalid.Reason},
			"requestId": requestID(),
		})
	default:
		internal(w, op, err)
	}
}
