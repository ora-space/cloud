package server

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"
)

type scheduleStore interface {
	ScheduleMember(context.Context, string, string) (bool, error)
	ScheduleConfigs(context.Context, string, string) ([]store.ScheduleConfig, error)
	CreateScheduleConfig(context.Context, string, string, store.ScheduleInput) (store.ScheduleConfig, error)
	UpdateScheduleConfig(context.Context, string, string, string, store.ScheduleInput, *bool) (store.ScheduleConfig, error)
}

var scheduleUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
var scheduleTime = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func decodeSchedule(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		fault(w, 400, "invalid_json")
		return false
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		fault(w, 400, "invalid_json")
		return false
	}
	return true
}

func validateSchedule(in *store.ScheduleInput) string {
	for _, part := range []struct {
		raw  json.RawMessage
		keys []string
	}{

		{in.Rule, []string{"kind", "time", "days", "hours", "timezone"}},
	} {
		var fields map[string]json.RawMessage
		if json.Unmarshal(part.raw, &fields) != nil {
			return "invalid_configuration"
		}
		for _, key := range part.keys {
			if len(fields[key]) == 0 || string(fields[key]) == "null" {
				return "invalid_configuration"
			}
		}
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 200 {
		return "invalid_name"
	}
	target, code := decodeScheduleChat(in.Target)
	if code != "" {
		return code
	}
	if target.Kind == "chat" && in.ProjectID != "" && (!scheduleUUID.MatchString(in.ProjectID) || target.ExecutorID == "") {
		return "invalid_project_executor"
	}
	if target.Kind == "workflow" {
		if _, code := decodeScheduleWorkflow(in.Target); code != "" {
			return code
		}
		// Every workflow run executes in a project.
		if !scheduleUUID.MatchString(in.ProjectID) {
			return "project_required"
		}
	}
	var rule struct {
		Kind     string `json:"kind"`
		Time     string `json:"time"`
		Days     []int  `json:"days"`
		Hours    int    `json:"hours"`
		Timezone string `json:"timezone"`
	}
	d := json.NewDecoder(strings.NewReader(string(in.Rule)))
	d.DisallowUnknownFields()
	if d.Decode(&rule) != nil {
		return "invalid_rule"
	}
	if rule.Timezone == "" || rule.Timezone == "Local" {
		return "invalid_timezone"
	}
	if _, e := time.LoadLocation(rule.Timezone); e != nil {
		return "invalid_timezone"
	}
	switch rule.Kind {
	case "hours":
		if rule.Hours < 1 || rule.Hours > 24 {
			return "invalid_interval"
		}
	case "daily", "weekdays", "weekly":
		if !scheduleTime.MatchString(rule.Time) {
			return "invalid_time"
		}
	default:
		return "invalid_rule"
	}
	seen := map[int]bool{}
	for _, day := range rule.Days {
		if day < 0 || day > 6 || seen[day] {
			return "invalid_days"
		}
		seen[day] = true
	}
	if rule.Kind == "weekly" && len(rule.Days) == 0 {
		return "invalid_days"
	}
	return ""
}

func (h *handler) scheduleConfigs(w http.ResponseWriter, r *http.Request, u store.User) {
	s, ok := h.store.(scheduleStore)
	if !ok {
		fault(w, 503, "schedule_config_unavailable")
		return
	}
	tenant := r.PathValue("tid")
	id := r.PathValue("id")
	if !scheduleUUID.MatchString(tenant) || (id != "" && !scheduleUUID.MatchString(id)) {
		fault(w, 404, "not_found")
		return
	}
	member, e := s.ScheduleMember(r.Context(), tenant, u.ID)
	if e != nil {
		internal(w, "schedule membership", e)
		return
	}
	if !member {
		fault(w, 404, "not_found")
		return
	}
	if r.Method == "GET" {
		items, e := s.ScheduleConfigs(r.Context(), tenant, u.ID)
		if e != nil {
			internal(w, "schedule list", e)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items, "nextCursor": ""})
		return
	}
	var in store.ScheduleInput
	var enabled *bool
	if r.Method == "PATCH" {
		var req struct {
			Version int64 `json:"version"`
			Enabled *bool `json:"enabled"`
		}
		if !decodeSchedule(w, r, &req) {
			return
		}
		if req.Enabled == nil {
			fault(w, 400, "enabled_required")
			return
		}
		in.Version = req.Version
		enabled = req.Enabled
	} else {
		if !decodeSchedule(w, r, &in) {
			return
		}
		if code := validateSchedule(&in); code != "" {
			status := 400
			if code == "schedule_target_unavailable" {
				status = 503
			}
			fault(w, status, code)
			return
		}
		target, _ := decodeScheduleChat(in.Target)
		var err error
		if target.Kind == "workflow" {
			workflow, _ := decodeScheduleWorkflow(in.Target)
			err = h.resolveScheduleWorkflow(r.Context(), tenant, u.ID, &in, workflow)
		} else {
			err = h.resolveScheduleChat(r.Context(), tenant, u.ID, &in, target)
		}
		if err != nil {
			switch {
			case errors.Is(err, errScheduleWorkflowsUnavailable):
				fault(w, 503, "schedule_target_unavailable")
			case errors.Is(err, ErrScheduleResourcesUnavailable):
				fault(w, 503, "schedule_resources_unavailable")
			case errors.Is(err, store.ErrNotFound):
				fault(w, 404, "not_found")
			default:
				internal(w, "resolve schedule target", err)
			}
			return
		}
	}
	if r.Method != "POST" && in.Version < 1 {
		fault(w, 428, "version_required")
		return
	}
	var out store.ScheduleConfig
	if r.Method == "POST" {
		if in.Version != 0 {
			fault(w, 400, "invalid_version")
			return
		}
		out, e = s.CreateScheduleConfig(r.Context(), tenant, u.ID, in)
	} else {
		out, e = s.UpdateScheduleConfig(r.Context(), tenant, u.ID, id, in, enabled)
	}
	switch {
	case errors.Is(e, store.ErrNotFound):
		fault(w, 404, "not_found")
	case errors.Is(e, store.ErrConflict):
		fault(w, 409, "version_conflict")
	case e != nil:
		internal(w, "save schedule", e)
	default:
		status := 200
		if r.Method == "POST" {
			status = 201
		}
		writeJSON(w, status, out)
	}
}
