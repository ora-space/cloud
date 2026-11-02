package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/scheduler"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

type scheduleResultResponse struct {
	SessionID      string          `json:"sessionId"`
	TurnID         string          `json:"turnId"`
	Mode           string          `json:"mode"`
	WorkspaceID    string          `json:"workspaceId"`
	SessionEndedAt *time.Time      `json:"sessionEndedAt"`
	StartedAt      time.Time       `json:"startedAt"`
	EndedAt        *time.Time      `json:"endedAt"`
	Result         *scheduleResult `json:"result"`
}

type scheduleResult struct {
	Status         execution.Status `json:"status"`
	Answer         string           `json:"answer"`
	AnswerComplete bool             `json:"answerComplete"`
	StopReason     string           `json:"stopReason"`
	FailureCode    string           `json:"failureCode"`
	Usage          json.RawMessage  `json:"usage"`
}

// Results are separate from occurrence pagination: an unavailable execution backend must
// not hide the recorded trigger, and list requests never fan out into public history reads.
func (h *handler) scheduleResult(w http.ResponseWriter, r *http.Request, u store.User) {
	w.Header().Set("Cache-Control", "no-store")
	tenant, id, oid := r.PathValue("tid"), r.PathValue("id"), r.PathValue("oid")
	if !scheduleUUID.MatchString(tenant) || !scheduleUUID.MatchString(id) || !scheduleUUID.MatchString(oid) {
		fault(w, 404, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		fault(w, 400, "invalid_query")
		return
	}
	s, ok := h.store.(historyStore)
	if !ok {
		fault(w, 503, "schedule_history_unavailable")
		return
	}
	o, err := s.ScheduleHistoryItem(r.Context(), tenant, u.ID, id, oid)
	if historyError(w, err) {
		return
	}
	// The durable occurrence ID is the schedule Caller ID and AttemptID. Ref is the
	// JSON execution.Ref saved by the dispatcher, never a browser-selected session.
	// A workflow occurrence's result is its run, read through the run routes, not a turn.
	if _, workflow := scheduler.WorkflowRunOf(o.ExecutionRef); o.ExecutionRef == "" || workflow {
		fault(w, 404, "execution_result_not_found")
		return
	}
	var ref execution.Ref
	if json.Unmarshal([]byte(o.ExecutionRef), &ref) != nil || ref.SessionID == "" || ref.TurnID == "" {
		fault(w, 502, "execution_reference_mismatch")
		return
	}
	if h.opts.Execution == nil {
		fault(w, 503, "execution_unavailable")
		return
	}
	scope := execution.Scope{TenantID: tenant, UserID: u.ID}
	caller := execution.Caller{Kind: execution.CallerSchedule, ID: o.ID, AttemptID: o.ID}
	turn, err := h.opts.Execution.ReadAttempt(r.Context(), scope, caller)
	if executionReadError(w, err) {
		return
	}
	c := turn.Caller
	if turn.Ref != ref || c.Kind != caller.Kind || c.ID != caller.ID || c.AttemptID != caller.AttemptID || c.NodeID != "" || len(c.Rounds) != 0 {
		fault(w, 502, "execution_reference_mismatch")
		return
	}
	session, err := h.opts.Execution.ReadSession(r.Context(), scope, ref.SessionID)
	if executionReadError(w, err) {
		return
	}
	if session.ID != ref.SessionID {
		fault(w, 502, "execution_reference_mismatch")
		return
	}
	// A missing authoritative terminal result is never reconstructed from occurrence
	// status or a partial stream. These checks do not write back or reconcile anything.
	if turn.Result == nil && (turn.EndedAt != nil || session.EndedAt != nil || o.Status == "succeeded" || o.Status == "failed" || o.Status == "cancelled" || o.Status == "interrupted") {
		fault(w, 502, "execution_result_incomplete")
		return
	}
	out := scheduleResultResponse{SessionID: ref.SessionID, TurnID: ref.TurnID, Mode: "model", WorkspaceID: session.WorkspaceID, SessionEndedAt: session.EndedAt, StartedAt: turn.StartedAt, EndedAt: turn.EndedAt}
	if session.Target != nil {
		out.Mode = "agent"
	}
	if result := turn.Result; result != nil {
		switch result.Status {
		case execution.StatusCompleted, execution.StatusFailed, execution.StatusCancelled, execution.StatusInterrupted:
		default:
			fault(w, 502, "execution_result_incomplete")
			return
		}
		// Preserve incomplete/partial results verbatim, including AnswerComplete=false.
		// The HTTP transport's success does not mean the execution succeeded.
		out.Result = &scheduleResult{result.Status, result.Answer, result.AnswerComplete, result.StopReason, result.FailureCode, result.Usage}
	}
	writeJSON(w, 200, out)
}

func executionReadError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, execution.ErrNotFound):
		fault(w, 404, "execution_result_not_found")
	case errors.Is(err, execution.ErrUnavailable):
		fault(w, 503, "execution_unavailable")
	default:
		fault(w, 502, "execution_read_failed")
	}
	return true
}
