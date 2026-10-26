package core

import (
	"encoding/json"
	"math"
	"time"
)

// Workflow agent-node retry: the desktop-compatible `agentConfig.retry` policy and the scheduler's
// retry decision (specs decisions/cloud/workflow 20261021-node-retry-and-resume-contract.md D1–D3).
//
// The publish gate validates the document (validateAgentRetryConfig in workflow_graph.go); this file
// is the runtime read of the same field. The two share the shape and the bounds so a graph the gate
// accepts always parses here. The parser is total because a frozen snapshot is never re-gated: an
// imported or pre-gate document may carry a value the gate would refuse, and the scheduler must
// still reach a decision rather than fail a run it cannot interpret.

const (
	// The default policy every agent node executes with when it declares none: enabled, two extra
	// attempts, ten seconds before the first retry. These are desktop's AgentRetryPolicy::default()
	// values (D1), and the defaults are also what a historical snapshot without `retry` reads.
	workflowRetryDefaultMaxRetries          = 2
	workflowRetryDefaultInitialDelaySeconds = 10

	// The ranges the publish gate accepts (D1). They bound the runtime parser too: a value outside
	// them is not a policy the gate could have frozen, so it reads as the default policy.
	workflowRetryMinMaxRetries          = 0
	workflowRetryMaxMaxRetries          = 5
	workflowRetryMinInitialDelaySeconds = 0
	workflowRetryMaxInitialDelaySeconds = 300
	workflowRetryMaxWaitSeconds         = 600

	// workflowRetryableSessionReason is the one session end reason a failed attempt is retried for.
	// The set is closed (D2): `interrupted` is the host/Node interrupt desktop never retries,
	// `cancelled` is the user's decision, and Cloud's own deadline and resource reasons are already
	// bounded waits whose retry would only multiply the ceiling. Extending this table is an ADR
	// change, not an implementation detail.
	workflowRetryableSessionReason = "agent_failed"
)

// workflowRetryPolicy is the parsed retry configuration of one agent node. It is a plain value: the
// policy is frozen in the snapshot graph and read fresh on every pass, so nothing about the wait is
// carried in memory between ticks.
type workflowRetryPolicy struct {
	Enabled             bool
	MaxRetries          int64
	InitialDelaySeconds int64
}

// defaultWorkflowRetryPolicy is the policy an absent or unusable `agentConfig.retry` reads as.
func defaultWorkflowRetryPolicy() workflowRetryPolicy {
	return workflowRetryPolicy{
		Enabled:             true,
		MaxRetries:          workflowRetryDefaultMaxRetries,
		InitialDelaySeconds: workflowRetryDefaultInitialDelaySeconds,
	}
}

// workflowRetryPolicyOf reads the frozen node's `agentConfig.retry`. Absent and null are the default
// policy (D1), and so is a value the publish gate would refuse: snapshots are not re-gated, and a
// document Cloud imported before this field was executable has no policy to honor — the default is
// the one reading that never invents a wait or a budget out of a malformed value.
func workflowRetryPolicyOf(data Object) workflowRetryPolicy {
	raw, present := data.O("agentConfig")["retry"]
	if !present || raw == nil {
		return defaultWorkflowRetryPolicy()
	}
	config, ok := asWorkflowObject(raw)
	if !ok {
		return defaultWorkflowRetryPolicy()
	}
	enabled, enabledOK := config["enabled"].(bool)
	maxRetries, maxOK := workflowRetryInteger(config["maxRetries"])
	initialDelay, delayOK := workflowRetryInteger(config["initialDelaySeconds"])
	if !enabledOK || !maxOK || !delayOK ||
		maxRetries < workflowRetryMinMaxRetries || maxRetries > workflowRetryMaxMaxRetries ||
		initialDelay < workflowRetryMinInitialDelaySeconds || initialDelay > workflowRetryMaxInitialDelaySeconds {
		return defaultWorkflowRetryPolicy()
	}
	return workflowRetryPolicy{Enabled: enabled, MaxRetries: maxRetries, InitialDelaySeconds: initialDelay}
}

// workflowRetryInteger reads one integral JSON number of a retry policy. The document reaches Cloud
// through either the request decoder (json.Number) or a jsonb column (float64), so both are accepted
// together with the Go integer types hand-built documents use. A fractional value is refused rather
// than truncated: 1.5 retries is not a policy anyone wrote on purpose.
func workflowRetryInteger(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
			return 0, false
		}
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

// workflowRetryWait is the wait before the attempt following failedAttempt: the policy's initial
// delay doubled per retry (`initialDelaySeconds × 2^(n-1)`, n the 1-based retry number, which equals
// the number of the attempt that failed), capped at 600 seconds. Both bounds are small — at most six
// attempts and 300 seconds — so the shift cannot overflow.
func workflowRetryWait(policy workflowRetryPolicy, failedAttempt int64) time.Duration {
	retry := failedAttempt
	if retry < 1 {
		retry = 1
	}
	if retry > workflowRetryMaxMaxRetries+1 {
		retry = workflowRetryMaxMaxRetries + 1
	}
	seconds := policy.InitialDelaySeconds << (retry - 1)
	if seconds > workflowRetryMaxWaitSeconds {
		seconds = workflowRetryMaxWaitSeconds
	}
	return time.Duration(seconds) * time.Second
}

// workflowRetryDecision reports whether a failed attempt is replaced by a committed wait, and for how
// long. The reason is read from the session result before it is mapped to a node status (D2): only
// `agent_failed` retries, so a reason outside the closed set settles the node failed even when it
// also maps to failed. The budget counts the failed attempt: attempt a may be followed by a+1 only
// while a <= maxRetries, so one run makes at most maxRetries+1 attempts (D1/D6).
func workflowRetryDecision(policy workflowRetryPolicy, failedAttempt int64, reason string) (time.Duration, bool) {
	if reason != workflowRetryableSessionReason || !policy.Enabled || policy.MaxRetries < 1 {
		return 0, false
	}
	if failedAttempt > policy.MaxRetries {
		return 0, false
	}
	return workflowRetryWait(policy, failedAttempt), true
}

// workflowRetryingAgentNode returns the id of the node committed `retrying`, if any. A run has at
// most one: a waiting attempt keeps the run's serial agent slot (D3), so no other agent node of the
// same run can be waiting at the same time.
func workflowRetryingAgentNode(states map[string]workflowNodeState) (string, bool) {
	for id := range states {
		state := states[id]
		if state.Status == "retrying" {
			return id, true
		}
	}
	return "", false
}

// workflowRetryAt reads the next attempt's earliest start from the database clock. The instant is
// computed and rendered by PostgreSQL so a process clock skew can neither shorten nor extend the
// committed wait, exactly as the session windows are measured; the text is RFC 3339 in UTC so the
// run viewer reads it like every other timestamp in `node_states`.
func workflowRetryAt(t *transaction, wait time.Duration) string {
	row := t.one(`SELECT to_char((clock_timestamp() + make_interval(secs => $1)) AT TIME ZONE 'UTC',
		'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS retry_at`, wait.Seconds())
	return row.S("retryAt")
}

// workflowRetryDue reports whether the committed retry instant has been reached. It reads the run
// row rather than the pass's decoded copy and compares on the database clock, so every replica,
// restart and replay judges the same instant (D3, invariant 5).
func workflowRetryDue(t *transaction, runID, nodeID string) bool {
	return t.one(`SELECT id FROM workflow_runs
		WHERE id=$1 AND COALESCE(node_states->$2::text->>'retryAt','') <> ''
		  AND (node_states->$2::text->>'retryAt')::timestamptz <= clock_timestamp()`, runID, nodeID) != nil
}
