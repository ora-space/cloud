package workflow

import (
	"encoding/json"
	"time"
)

// FailureKind classifies why a node failed, with desktop's NodeFailureKind wire names.
type FailureKind string

// Failure kinds v2 can produce. Desktop kinds tied to unapplied capabilities (Skill
// materialization, baselines) are not listed because v2 never raises them.
const (
	FailureMissingAgentRef           FailureKind = "missing_agent_ref"
	FailureMissingAgentConfig        FailureKind = "missing_agent_config"
	FailureInvalidRunPayload         FailureKind = "invalid_run_payload"
	FailurePromptTemplate            FailureKind = "prompt_template"
	FailureStructuredOutput          FailureKind = "structured_output"
	FailureSessionEndedWithoutReason FailureKind = "session_ended_without_stop_reason"
	FailureSessionBindingRejected    FailureKind = "session_binding_rejected"
	FailureRepository                FailureKind = "repository"
	FailureSession                   FailureKind = "session"
	FailureAgentRefusal              FailureKind = "agent_refusal"
	FailureUnknownStopReason         FailureKind = "unknown_stop_reason"
	FailureInterruptedByRestart      FailureKind = "interrupted_by_restart"
	FailureMultipleOutputs           FailureKind = "multiple_outputs"
	FailureConditionEvaluation       FailureKind = "condition_evaluation"
	FailureAggregatorNoMatch         FailureKind = "aggregator_no_match"
)

func (k FailureKind) resumable() bool {
	switch k {
	case FailureMissingAgentConfig, FailureSession, FailureSessionEndedWithoutReason,
		FailureSessionBindingRejected, FailureInterruptedByRestart, FailureRepository:
		return true
	}
	return false
}

func (k FailureKind) injectsIntoPrompt() bool {
	switch k {
	case FailureStructuredOutput, FailureAgentRefusal, FailureUnknownStopReason, FailureMultipleOutputs:
		return true
	}
	return false
}

func (k FailureKind) autoRetryable() bool {
	switch k {
	case FailureSession, FailureSessionEndedWithoutReason, FailureSessionBindingRejected,
		FailureStructuredOutput, FailureAgentRefusal, FailureUnknownStopReason:
		return true
	}
	return false
}

// Failure is one node failure as reported by a runtime or the execution bridge. Output keeps
// the raw answer of a failed structured response so it stays inspectable.
type Failure struct {
	Kind        FailureKind
	Message     string
	SourceChain []string
	Output      *string
}

// runtimeFailure classifies a swift runtime's refusal, as desktop's NodeFailure::from_runtime.
func runtimeFailure(kind, message string) Failure {
	k := FailureRepository
	switch kind {
	case "output":
		k = FailureMultipleOutputs
	case "condition":
		k = FailureConditionEvaluation
	case "aggregator":
		k = FailureAggregatorNoMatch
	}
	return Failure{Kind: k, Message: message}
}

// Detail renders the payload.error_detail object for the attempt number the store assigned.
// v2 performs no automatic retry or resume, but the flags keep desktop's meaning so the run
// view can explain a failure the same way.
func (f Failure) Detail(attempt int, injectLastFailure bool, now time.Time) map[string]any {
	chain := f.SourceChain
	if chain == nil {
		chain = []string{}
	}
	return map[string]any{
		"kind":                     string(f.Kind),
		"message":                  f.Message,
		"source_chain":             chain,
		"attempt":                  attempt,
		"resumable":                f.Kind.resumable(),
		"injects_previous_failure": f.Kind.injectsIntoPrompt() && injectLastFailure,
		"auto_retryable":           f.Kind.autoRetryable(),
		"recorded_at":              now.UnixMilli(),
	}
}

// MergePayload sets keys on a node payload object, treating NULL or invalid JSON as empty and
// keeping every other key, as desktop's merge_payload_keys.
func MergePayload(existing []byte, keys map[string]any) ([]byte, error) {
	m := map[string]any{}
	if len(existing) != 0 {
		var decoded any
		if json.Unmarshal(existing, &decoded) == nil {
			if obj, ok := decoded.(map[string]any); ok {
				m = obj
			}
		}
	}
	for k, v := range keys {
		m[k] = v
	}
	return json.Marshal(m)
}
