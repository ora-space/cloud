package server

import (
	"encoding/json"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/scheduler"
)

// ScheduleChatTarget contains only the currently supported conversation binding.
type ScheduleChatTarget struct {
	Kind       string `json:"kind"`
	Prompt     string `json:"prompt"`
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	ExecutorID string `json:"executorId,omitempty"`
}

func decodeScheduleChat(raw json.RawMessage) (ScheduleChatTarget, string) {
	var target ScheduleChatTarget
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return target, "invalid_target"
	}
	var kind string
	if json.Unmarshal(fields["kind"], &kind) != nil {
		return target, "invalid_target"
	}
	// Legacy task references remain readable; their provider contract is not defined here.
	if kind == "task" {
		return target, "schedule_target_unavailable"
	}
	// A workflow target has its own shape (decodeScheduleWorkflow).
	if kind == "workflow" {
		return ScheduleChatTarget{Kind: kind}, ""
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&target) != nil || kind != "chat" || strings.TrimSpace(target.Prompt) == "" || len(target.Prompt) > 32000 {
		return target, "invalid_target"
	}
	for _, key := range []string{"kind", "prompt", "agent", "model"} {
		if len(fields[key]) == 0 || string(fields[key]) == "null" {
			return target, "invalid_target"
		}
	}
	if string(fields["executorId"]) == "null" {
		return target, "invalid_target"
	}
	return target, ""
}

// decodeScheduleWorkflow reads a workflow target strictly: a workflow, the published snapshot of
// it the schedule runs, and the kickoff input, which may be empty.
func decodeScheduleWorkflow(raw json.RawMessage) (scheduler.WorkflowTarget, string) {
	var target scheduler.WorkflowTarget
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&target) != nil || target.Kind != "workflow" || len(target.Input) > 32000 {
		return target, "invalid_target"
	}
	if !workflowUUID.MatchString(target.WorkflowID) || !workflowUUID.MatchString(target.SnapshotID) {
		return target, "invalid_target"
	}
	return target, ""
}
