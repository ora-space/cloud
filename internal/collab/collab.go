// Package collab provides dev/demo implementations of the Issue collaboration ports: an in-memory
// fixture directory for the target types Cloud has no backend for (agent/team), a deterministic
// ContextBuilder (no AI), a mock ExecutionDispatcher that drives the observer lifecycle to completion
// synchronously, and a deterministic InputAssistProvider.
//
// These are fixtures only — no database tables, no sim_* tables, no mock domain tables, no real
// Agent/Team backend. They are wired off by default in production; cmd/ora-web and the integration
// suite opt in. Workflow targets and their form descriptors are NOT here: Cloud owns the workflow
// document, so core serves both from the `workflows` table in every deployment.
package collab

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wanglongan587/cloud/internal/core"
)

// Stable fixture identities (valid v4 UUIDs) so demo/dev data is reproducible across restarts.
const (
	BackendAgentID = "11111111-1111-4111-8111-111111111111"
	ReviewAgentID  = "22222222-2222-4222-8222-222222222222"
	PlatformTeamID = "33333333-3333-4333-8333-333333333333"
)

// FallbackDirectory composes the development fixtures with the real directory: every target the
// primary can serve is served by it, and the fixtures answer only what it does not. Cloud has no
// Agent/Team backend, so those two target types exist in development and nowhere else; workflow
// targets always come from the real, database-backed directory, even in development.
//
// A nil primary is not a misconfiguration: a store assembled without a pool (unit tests) has nothing
// to delegate to, and the fixtures then answer alone.
type FallbackDirectory struct{ Workflows core.CollaborationDirectory }

// FixtureAgentTeamDirectory resolves the demo agent and team targets from a fixed in-memory catalog.
// Human targets never come from here (they are derived from real tenant memberships), and neither do
// workflow targets (they come from the workflows table).
type FixtureAgentTeamDirectory struct{}

var fixtureTargets = []core.CollaborationTargetSummary{
	{Type: "agent", ID: BackendAgentID, DisplayName: "Backend Agent", Description: "Demo backend execution agent", InteractionDescriptor: core.InteractionDescriptor{Mode: "task", RequiresTask: true}},
	{Type: "agent", ID: ReviewAgentID, DisplayName: "Review Agent", Description: "Demo code-review agent", InteractionDescriptor: core.InteractionDescriptor{Mode: "task", RequiresTask: true}},
	{Type: "team", ID: PlatformTeamID, DisplayName: "Platform Team", Description: "Demo platform team", InteractionDescriptor: core.InteractionDescriptor{Mode: "task", RequiresTask: true}},
}

// MockInputAssistProvider produces deterministic AI Assist suggestions: the same issue text and the
// same current values always yield the same patch. It performs no LLM call, uses no randomness, and
// only ever *suggests* — applying and confirming stay user actions (Issue collaboration D9).
//
// It recognizes a small set of well-known demo field names and emits a suggestion only for a key the
// *current* descriptor declares, so it is a no-op for a workflow that names its inputs differently
// and can never widen a form (Issue collaboration D9).
type MockInputAssistProvider struct{}

//nolint:gocritic // value-typed assist input mirrors the provider interface signature this mock satisfies
func (MockInputAssistProvider) Suggest(_ context.Context, in core.AssistInput) (core.AssistSuggestion, error) {
	values := core.Object{}
	explanations := core.Object{}
	text := strings.ToLower(in.IssueTitle + " " + in.IssueDescription)

	// Suggestions are keyed by well-known field names but only ever emitted for keys the *current*
	// descriptor actually declares, so one mock serves every fixture workflow. A key the user has
	// already filled in is never suggested over — assist must not clobber a typed value.
	declared := map[string]bool{}
	for i := range in.Descriptor.Fields {
		declared[in.Descriptor.Fields[i].Key] = true
	}
	suggest := func(key string, value any, reason string) {
		if !declared[key] || in.CurrentValues.S(key) != "" {
			return
		}
		values[key] = value
		explanations[key] = reason
	}

	security := strings.Contains(text, "security") || strings.Contains(text, "auth") || strings.Contains(text, "token")
	suggest("scope", "changed-files", "Demo suggestion: this issue describes a change, so reviewing the changed files is the narrowest useful scope.")
	if security {
		suggest("severity", "high", "Demo suggestion: the issue text mentions security/auth, so raise the severity.")
	} else {
		suggest("severity", "medium", "Demo suggestion: default demo severity for a non-security issue.")
	}
	suggest("branch", "main", "Demo suggestion: review the default branch when none is given.")
	suggest("version", "v1.2.3", "Demo suggestion: demo release version.")
	suggest("rolloutPercent", 10, "Demo suggestion: start with a small rollout percentage.")
	suggest("environments", []any{"staging"}, "Demo suggestion: validate in staging before production.")
	return core.AssistSuggestion{Values: values, Explanations: explanations}, nil
}

func (FixtureAgentTeamDirectory) ListTargets(_ context.Context, _, query string) ([]core.CollaborationTargetSummary, error) {
	if query == "" {
		out := make([]core.CollaborationTargetSummary, len(fixtureTargets))
		copy(out, fixtureTargets)
		return out, nil
	}
	q := strings.ToLower(query)
	out := []core.CollaborationTargetSummary{}
	for _, t := range fixtureTargets {
		if strings.Contains(strings.ToLower(t.DisplayName), q) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (FixtureAgentTeamDirectory) ResolveTarget(_ context.Context, _, targetType, targetID string) (core.CollaborationTargetSummary, bool, error) {
	for _, t := range fixtureTargets {
		if t.Type == targetType && t.ID == targetID {
			return t, true, nil
		}
	}
	return core.CollaborationTargetSummary{}, false, nil
}

// ListTargets is the fixtures' matches followed by the primary's. The order is the composition's own
// (humans are prepended by the caller anyway), so neither source has to know about the other.
func (d FallbackDirectory) ListTargets(ctx context.Context, tenantID, query string) ([]core.CollaborationTargetSummary, error) {
	fixtures, err := FixtureAgentTeamDirectory{}.ListTargets(ctx, tenantID, query)
	if err != nil {
		return nil, err
	}
	if d.Workflows == nil {
		return fixtures, nil
	}
	primary, err := d.Workflows.ListTargets(ctx, tenantID, query)
	if err != nil {
		return nil, err
	}
	return append(fixtures, primary...), nil
}

// ResolveTarget asks the primary first. A target type the fixtures own (agent/team) is never a
// workflow, so the primary declines it and the fixtures answer; a workflow id the primary does not
// know is not-found rather than a fixture fallback, which is what keeps an archived workflow from
// staying reachable through a stale fixture.
func (d FallbackDirectory) ResolveTarget(ctx context.Context, tenantID, targetType, targetID string) (core.CollaborationTargetSummary, bool, error) {
	if d.Workflows != nil {
		if summary, ok, err := d.Workflows.ResolveTarget(ctx, tenantID, targetType, targetID); ok || err != nil {
			return summary, ok, err
		}
	}
	return FixtureAgentTeamDirectory{}.ResolveTarget(ctx, tenantID, targetType, targetID)
}

// DeterministicContextBuilder assembles a fixed, reproducible execution context snapshot. The
// bounded recent-comments window and the explicit context refs are supplied by the Store; this
// builder only lays them out — it performs no AI.
type DeterministicContextBuilder struct{}

//nolint:gocritic // value-typed context input mirrors the ContextBuilder port; the fixture only lays the snapshot out
func (DeterministicContextBuilder) Build(_ context.Context, in core.ContextInput) (core.Object, error) {
	values := in.InteractionValues
	if values == nil {
		values = core.Object{}
	}
	return core.Object{
		"task":              in.Task,
		"interactionValues": values,
		"target":            core.Object{"type": in.TargetType, "id": in.TargetID},
		"issue":             core.Object{"title": in.IssueTitle, "description": in.IssueDescription},
		"recentComments":    in.RecentComments,
		"contextRefs":       in.ContextRefs,
	}, nil
}

// MockExecutionDispatcher reports an accepted handoff with a synthetic external id and completes
// synchronously by driving the observer lifecycle. All mock behavior (including the fixed reply text)
// lives here, not in the Issue core — and nothing here performs a real scan, review or LLM call.
type MockExecutionDispatcher struct{}

//nolint:gocritic // value-typed dispatch request mirrors the ExecutionDispatcher port; the mock only records the handoff
func (MockExecutionDispatcher) Dispatch(_ context.Context, req core.DispatchRequest) (core.DispatchResult, error) {
	return core.DispatchResult{Accepted: true, ExternalExecutionID: "mock-" + req.RunID}, nil
}

// Execute drives one run to completion. Agent/team runs reply as a comment (queued -> dispatched ->
// running -> reply -> completed); a workflow run reports coarse progress and a human-readable result,
// which the Store projects as `system` activities rather than comments (Issue collaboration D2).
//
//nolint:gocritic // value-typed dispatch request mirrors the ExecutionDispatcher port; the mock drives the observer synchronously
func (MockExecutionDispatcher) Execute(ctx context.Context, req core.DispatchRequest, obs core.ExecutionObserver) {
	_, _ = obs.ObserveStarted(ctx, req.TenantID, req.RunID)
	if req.ExecutorType == "workflow" {
		_, _ = obs.ObserveProgress(ctx, req.TenantID, req.RunID, "Reading repository metadata")
		_, _ = obs.ObserveMessage(ctx, req.TenantID, req.RunID, mockWorkflowResult(req.Input))
		_, _ = obs.ObserveCompleted(ctx, req.TenantID, req.RunID, core.Object{"mock": true, "executorType": "workflow", "executorId": req.ExecutorID})
		return
	}
	_, _ = obs.ObserveMessage(ctx, req.TenantID, req.RunID, mockReply(req.ExecutorType))
	_, _ = obs.ObserveCompleted(ctx, req.TenantID, req.RunID, core.Object{"mock": true, "executorType": req.ExecutorType, "executorId": req.ExecutorID})
}

func mockReply(executorType string) string {
	if executorType == "team" {
		return "Demo response: The team received this task and context."
	}
	return "Demo response: I received the task and the supplied Issue context."
}

// mockWorkflowResult is a deterministic, explicitly-simulated summary of the confirmed form values. It
// is descriptor-agnostic (it echoes whatever was confirmed) and must never claim that real work ran.
func mockWorkflowResult(input core.Object) string {
	values := input.O("interactionValues")
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+renderValue(values[key]))
	}
	if len(parts) == 0 {
		parts = append(parts, "no values")
	}
	return "Demo workflow result (simulated; no real work was performed): " + strings.Join(parts, ", ") + "."
}

func renderValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			items = append(items, renderValue(item))
		}
		return "[" + strings.Join(items, ", ") + "]"
	default:
		return fmt.Sprint(typed)
	}
}

// WireDevelopmentFixtures installs the in-memory dev/demo collaboration ports on the Store. It is
// the single composition point for the fixture set; callers must gate it behind an explicit,
// development-only configuration (cmd/server via `collaboration.development_fixtures` /
// CLOUD_COLLABORATION_DEVELOPMENT_FIXTURES; cmd/ora-web and the integration suite are dev edges and
// opt in unconditionally). Production default is OFF — the fixtures are then absent, so no fake
// Agent/Team is exposed and the target catalog is the real one (tenant members plus workflows). It
// must never be enabled as a side effect of any external login provider being configured.
//
// It layers the fixtures in front of the directory the store already has rather than replacing it:
// workflow targets are real in every deployment, so development adds agent/team on top of them. Forms
// is deliberately untouched — workflow form descriptors come from the workflows table, not a fixture.
func WireDevelopmentFixtures(store *core.Store) {
	store.Directory = FallbackDirectory{Workflows: store.Directory}
	store.Context = DeterministicContextBuilder{}
	store.Dispatcher = MockExecutionDispatcher{}
	store.Assist = MockInputAssistProvider{}
	store.Simulator = MockWorkflowRunSimulator{}
}
