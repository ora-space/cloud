package core

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// The retry policy's pure core: reading the frozen `agentConfig.retry`, computing the wait and
// deciding whether a failed attempt is replaced by a committed wait (specs decisions/cloud/workflow
// 20261021-node-retry-and-resume-contract.md D1–D3). The database-facing halves — where the wait
// instant is written and when it becomes due — are covered by workflow_retry_db_test.go.

// retryNodeData wraps a retry value the way a frozen agent node carries it.
func retryNodeData(retry any) Object {
	return Object{"agentConfig": Object{"retry": retry}}
}

// retryPolicy is the full policy object hand-built documents and the editor write.
func retryPolicy(enabled, maxRetries, initialDelaySeconds any) Object {
	return Object{"enabled": enabled, "maxRetries": maxRetries, "initialDelaySeconds": initialDelaySeconds}
}

// The parser is total because a frozen snapshot is never re-gated: absent, null and every value the
// publish gate would refuse read as the default policy rather than making the scheduler unable to
// decide. The accepted numeric forms are the three the document can reach Go as: json.Number from
// the request decoder, float64 from a jsonb column, and the Go integer types hand-built documents
// use.
func TestWorkflowRetryPolicyOf(t *testing.T) {
	def := defaultWorkflowRetryPolicy()
	cases := []struct {
		name string
		data Object
		want workflowRetryPolicy
	}{
		{name: "an agent node with no agentConfig", data: Object{}, want: def},
		{name: "an agentConfig without retry", data: Object{"agentConfig": Object{"prompt": "x"}}, want: def},
		{name: "retry null", data: retryNodeData(nil), want: def},
		{name: "retry not an object", data: retryNodeData(true), want: def},
		{name: "retry an empty object", data: retryNodeData(Object{}), want: def},
		{name: "a partial object", data: retryNodeData(Object{"enabled": true}), want: def},
		{
			// Present with every field, but maxRetries is outside the gate's range: a snapshot imported
			// before the gate existed has no policy Cloud can honor, and inventing a budget from it is
			// worse than the documented default.
			name: "maxRetries past the gate's range", data: retryNodeData(retryPolicy(true, 6, 10)), want: def,
		},
		{name: "a negative initial delay", data: retryNodeData(retryPolicy(true, 2, -1)), want: def},
		{name: "a fractional maxRetries", data: retryNodeData(retryPolicy(true, 1.5, 10)), want: def},
		{name: "enabled of the wrong type", data: retryNodeData(retryPolicy("yes", 2, 10)), want: def},
		{
			name: "a complete policy",
			data: retryNodeData(retryPolicy(false, int64(4), int64(25))),
			want: workflowRetryPolicy{Enabled: false, MaxRetries: 4, InitialDelaySeconds: 25},
		},
		{
			// A document that traveled through jsonb carries float64 numbers; an integral one is the
			// same policy.
			name: "float64 values from jsonb",
			data: retryNodeData(retryPolicy(true, float64(0), float64(300))),
			want: workflowRetryPolicy{Enabled: true, MaxRetries: 0, InitialDelaySeconds: 300},
		},
		{
			name: "json.Number values from the decoder",
			data: retryNodeData(retryPolicy(true, json.Number("5"), json.Number("0"))),
			want: workflowRetryPolicy{Enabled: true, MaxRetries: 5, InitialDelaySeconds: 0},
		},
		{
			// The schema may grow fields Cloud does not execute yet; an unknown key is not a reason to
			// ignore the policy written beside it.
			name: "a policy with an unknown key",
			data: retryNodeData(Object{"enabled": true, "maxRetries": int64(1), "initialDelaySeconds": int64(5), "future": "value"}),
			want: workflowRetryPolicy{Enabled: true, MaxRetries: 1, InitialDelaySeconds: 5},
		},
		{
			// A non-finite value cannot be written to jsonb, but a hand-built document can carry one;
			// it must not be truncated into a policy.
			name: "a non-finite initial delay", data: retryNodeData(retryPolicy(true, 2, math.Inf(1))), want: def,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowRetryPolicyOf(tc.data); got != tc.want {
				t.Fatalf("policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// D1's wait is `initialDelaySeconds × 2^(n-1)` before retry n, capped at 600 seconds, where n is the
// 1-based retry number — the number of the attempt that failed. Both bounds are small, so a table can
// state the complete curve instead of asserting an arithmetic identity.
func TestWorkflowRetryWait(t *testing.T) {
	cases := []struct {
		name          string
		delay         int64
		failedAttempt int64
		want          time.Duration
	}{
		{name: "first retry", delay: 10, failedAttempt: 1, want: 10 * time.Second},
		{name: "second retry doubles", delay: 10, failedAttempt: 2, want: 20 * time.Second},
		{name: "third retry doubles again", delay: 10, failedAttempt: 3, want: 40 * time.Second},
		{name: "fourth retry", delay: 10, failedAttempt: 4, want: 80 * time.Second},
		{name: "fifth retry", delay: 10, failedAttempt: 5, want: 160 * time.Second},
		{name: "sixth retry", delay: 10, failedAttempt: 6, want: 320 * time.Second},
		{name: "a large delay is also capped", delay: 300, failedAttempt: 3, want: 600 * time.Second},
		{name: "zero delay means due at once", delay: 0, failedAttempt: 1, want: 0},
		{name: "an attempt below one reads as the first", delay: 10, failedAttempt: 0, want: 10 * time.Second},
		{name: "an attempt past the curve is clamped to the last retry", delay: 10, failedAttempt: 99, want: 320 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := workflowRetryPolicy{Enabled: true, MaxRetries: 5, InitialDelaySeconds: tc.delay}
			if got := workflowRetryWait(policy, tc.failedAttempt); got != tc.want {
				t.Fatalf("wait after attempt %d with delay %ds = %s, want %s", tc.failedAttempt, tc.delay, got, tc.want)
			}
		})
	}
}

// The retry set is closed (D2): only `agent_failed` retries, and a reason outside the set settles the
// node terminal even when it maps to failed. The budget counts the failed attempt: attempt a may be
// followed by a+1 only while a <= maxRetries, so the run makes at most maxRetries+1 attempts (D1/D6).
func TestWorkflowRetryDecision(t *testing.T) {
	enabled := workflowRetryPolicy{Enabled: true, MaxRetries: 2, InitialDelaySeconds: 10}
	disabled := workflowRetryPolicy{Enabled: false, MaxRetries: 2, InitialDelaySeconds: 10}
	noBudget := workflowRetryPolicy{Enabled: true, MaxRetries: 0, InitialDelaySeconds: 10}
	cases := []struct {
		name   string
		policy workflowRetryPolicy
		state  workflowNodeState
		reason string
		want   bool
	}{
		{name: "agent_failed within budget", policy: enabled, state: workflowNodeState{Attempt: 1}, reason: "agent_failed", want: true},
		{name: "agent_failed on the last allowed attempt", policy: enabled, state: workflowNodeState{Attempt: 2}, reason: "agent_failed", want: true},
		{name: "agent_failed past the budget", policy: enabled, state: workflowNodeState{Attempt: 3}, reason: "agent_failed", want: false},
		{name: "an absent attempt reads as the first", policy: enabled, state: workflowNodeState{}, reason: "agent_failed", want: true},
		{name: "a disabled policy", policy: disabled, state: workflowNodeState{Attempt: 1}, reason: "agent_failed", want: false},
		{name: "a zero budget", policy: noBudget, state: workflowNodeState{Attempt: 1}, reason: "agent_failed", want: false},
		{name: "interrupted is not retryable", policy: enabled, state: workflowNodeState{Attempt: 1}, reason: "interrupted", want: false},
		{name: "a user end is not retryable", policy: enabled, state: workflowNodeState{Attempt: 1}, reason: "user_ended", want: false},
		{name: "a cancelled session is not retryable", policy: enabled, state: workflowNodeState{Attempt: 1}, reason: "cancelled", want: false},
		{name: "an empty reason is not retryable", policy: enabled, state: workflowNodeState{Attempt: 1}, reason: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wait, retry := workflowRetryDecision(tc.policy, tc.state.attemptNumber(), tc.reason)
			if retry != tc.want {
				t.Fatalf("retry(%q at attempt %d) = %v, want %v", tc.reason, tc.state.attemptNumber(), retry, tc.want)
			}
			if retry {
				want := workflowRetryWait(tc.policy, tc.state.attemptNumber())
				if wait != want {
					t.Fatalf("wait = %s, want %s", wait, want)
				}
			} else if wait != 0 {
				t.Fatalf("a non-retry returned a non-zero wait: %s", wait)
			}
		})
	}
}
