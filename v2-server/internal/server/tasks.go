package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// taskStore is the part of the database the task routes need. It lives beside the handlers so the
// rest of the server can run against a store that does not implement tasks yet.
type taskStore interface {
	Tasks(context.Context, string, string) ([]store.Task, error)
	Task(context.Context, string, string, string) (store.Task, error)
	CreateTask(context.Context, string, string, store.TaskInput) (store.Task, error)
	UpdateTask(context.Context, string, string, string, store.TaskInput) (store.Task, error)
	SetTaskStatus(context.Context, string, string, string, string, int64) (store.Task, error)
	SetTaskAssignee(context.Context, string, string, string, *store.TaskAssigneeRef, int64) (store.Task, error)
}

// taskUUID matches the ids this server mints for tasks; anything else cannot exist.
var taskUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Task text limits. The title is the one line a list shows; the description may hold a
// prompt-sized brief but never an unbounded body.
const (
	maxTaskTitle       = 200
	maxTaskDescription = 4000
)

type taskRequest struct {
	Title       string               `json:"title"`
	Description string               `json:"description"`
	ProjectID   string               `json:"projectId"`
	Assignee    *taskAssigneeRequest `json:"assignee"`
	Version     int64                `json:"version"`
}

type taskAssigneeRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type taskStatusRequest struct {
	Status  string `json:"status"`
	Version int64  `json:"version"`
}

type taskAssigneeUpdateRequest struct {
	Assignee json.RawMessage `json:"assignee"`
	Version  int64           `json:"version"`
}

// taskFault writes the stable error for a task store error and reports whether it handled it.
func taskFault(w http.ResponseWriter, e error) bool {
	switch {
	case errors.Is(e, store.ErrNotFound):
		fault(w, http.StatusNotFound, "not_found")
	case errors.Is(e, store.ErrForbidden):
		fault(w, http.StatusForbidden, "forbidden")
	case errors.Is(e, store.ErrConflict):
		fault(w, http.StatusConflict, "version_conflict")
	case errors.Is(e, store.ErrInvalidTransition):
		fault(w, http.StatusBadRequest, "invalid_transition")
	case errors.Is(e, store.ErrInvalidProject):
		fault(w, http.StatusBadRequest, "invalid_project")
	case errors.Is(e, store.ErrInvalidAssignee), errors.Is(e, store.ErrUserUnavailable),
		errors.Is(e, store.ErrAgentUnavailable):
		fault(w, http.StatusBadRequest, "invalid_assignee")
	default:
		return false
	}
	return true
}

// taskScope keeps tenant and task ids trustworthy and answers 404 to anything that cannot exist,
// so another workspace cannot be enumerated.
func (h *handler) taskScope(w http.ResponseWriter, r *http.Request) (taskStore, string, string, bool) {
	s, ok := h.store.(taskStore)
	if !ok {
		fault(w, http.StatusServiceUnavailable, "task_unavailable")
		return nil, "", "", false
	}
	tenant, id := r.PathValue("tid"), r.PathValue("taskid")
	if !taskUUID.MatchString(tenant) || (id != "" && !taskUUID.MatchString(id)) {
		fault(w, http.StatusNotFound, "not_found")
		return nil, "", "", false
	}
	return s, tenant, id, true
}

// validTaskText checks the request's lengths; an empty title is the one thing a task cannot lose.
func validTaskText(title, description string) string {
	if trimmed := strings.TrimSpace(title); trimmed == "" || utf8.RuneCountInString(trimmed) > maxTaskTitle {
		return "invalid_title"
	}
	if utf8.RuneCountInString(description) > maxTaskDescription {
		return "invalid_description"
	}
	return ""
}

// validAssigneeRef accepts exactly the three typed assignee kinds with a minted id.
func validAssigneeRef(in *taskAssigneeRequest) *store.TaskAssigneeRef {
	if in == nil {
		return nil
	}
	switch in.Type {
	case store.AssigneeHuman, store.AssigneeAgent, store.AssigneeSquad:
	default:
		return nil
	}
	if !taskUUID.MatchString(in.ID) {
		return nil
	}
	return &store.TaskAssigneeRef{Type: in.Type, ID: in.ID}
}

func (h *handler) listTasks(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, _, ok := h.taskScope(w, r)
	if !ok {
		return
	}
	items, e := s.Tasks(r.Context(), tenant, u.ID)
	if e != nil {
		if taskFault(w, e) {
			return
		}
		internal(w, "list tasks", e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": ""})
}

func (h *handler) createTask(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, _, ok := h.taskScope(w, r)
	if !ok {
		return
	}
	var req taskRequest
	if !decodeTask(w, r, &req) {
		return
	}
	if req.Version != 0 {
		fault(w, http.StatusBadRequest, "invalid_version")
		return
	}
	if code := validTaskText(req.Title, req.Description); code != "" {
		fault(w, http.StatusBadRequest, code)
		return
	}
	if req.ProjectID == "" {
		fault(w, http.StatusBadRequest, "invalid_project")
		return
	}
	if req.Assignee != nil && validAssigneeRef(req.Assignee) == nil {
		fault(w, http.StatusBadRequest, "invalid_assignee")
		return
	}
	in := store.TaskInput{
		Title:       strings.TrimSpace(req.Title),
		Description: req.Description,
		ProjectID:   req.ProjectID,
		Assignee:    validAssigneeRef(req.Assignee),
	}
	task, e := s.CreateTask(r.Context(), tenant, u.ID, in)
	if e != nil {
		if taskFault(w, e) {
			return
		}
		internal(w, "create task", e)
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

func (h *handler) getTask(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, id, ok := h.taskScope(w, r)
	if !ok {
		return
	}
	task, e := s.Task(r.Context(), tenant, u.ID, id)
	if e != nil {
		if taskFault(w, e) {
			return
		}
		internal(w, "get task", e)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *handler) updateTask(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, id, ok := h.taskScope(w, r)
	if !ok {
		return
	}
	var req taskRequest
	if !decodeTask(w, r, &req) {
		return
	}
	if req.Version < 1 {
		fault(w, http.StatusPreconditionRequired, "version_required")
		return
	}
	if code := validTaskText(req.Title, req.Description); code != "" {
		fault(w, http.StatusBadRequest, code)
		return
	}
	in := store.TaskInput{
		Title:       strings.TrimSpace(req.Title),
		Description: req.Description,
		ProjectID:   req.ProjectID,
		Version:     req.Version,
	}
	task, e := s.UpdateTask(r.Context(), tenant, u.ID, id, in)
	if e != nil {
		if taskFault(w, e) {
			return
		}
		internal(w, "update task", e)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// setTaskStatus moves a task by hand. The lifecycle check lives in the store so the execution
// service applies the same rule.
func (h *handler) setTaskStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, id, ok := h.taskScope(w, r)
	if !ok {
		return
	}
	var req taskStatusRequest
	if !decodeTask(w, r, &req) {
		return
	}
	if req.Version < 1 {
		fault(w, http.StatusPreconditionRequired, "version_required")
		return
	}
	task, e := s.SetTaskStatus(r.Context(), tenant, u.ID, id, strings.TrimSpace(req.Status), req.Version)
	if e != nil {
		if taskFault(w, e) {
			return
		}
		internal(w, "set task status", e)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// setTaskAssignee replaces or clears the assignee. An explicit null clears it; a missing field is
// a malformed request, so a client bug cannot silently unassign work.
func (h *handler) setTaskAssignee(w http.ResponseWriter, r *http.Request, u store.User) {
	s, tenant, id, ok := h.taskScope(w, r)
	if !ok {
		return
	}
	var req taskAssigneeUpdateRequest
	if !decodeTask(w, r, &req) {
		return
	}
	if req.Version < 1 {
		fault(w, http.StatusPreconditionRequired, "version_required")
		return
	}
	if len(req.Assignee) == 0 {
		fault(w, http.StatusBadRequest, "invalid_assignee")
		return
	}
	var ref *store.TaskAssigneeRef
	if string(req.Assignee) != "null" {
		var in taskAssigneeRequest
		if json.Unmarshal(req.Assignee, &in) != nil || validAssigneeRef(&in) == nil {
			fault(w, http.StatusBadRequest, "invalid_assignee")
			return
		}
		ref = validAssigneeRef(&in)
	}
	task, e := s.SetTaskAssignee(r.Context(), tenant, u.ID, id, ref, req.Version)
	if e != nil {
		if taskFault(w, e) {
			return
		}
		internal(w, "set task assignee", e)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// decodeTask reads one strict JSON body of a bounded size, like the other v2 writes.
func decodeTask(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fault(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		fault(w, http.StatusBadRequest, "invalid_json")
		return false
	}
	return true
}
