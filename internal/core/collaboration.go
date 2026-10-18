package core

import "context"

// Collaboration ports (consuming-side seams). The Issue domain owns only the *contract*: the types
// below describe how Issue collaboration asks an external capability to do one thing, without
// defining what that capability is internally (Agent/Team/Workflow internal design is UNKNOWN and
// blocked on external modules). Each of the corresponding Store fields is nil by default
// ("Unavailable"); dev/demo/integration wire the in-memory fixtures, production real adapters later.
//
// Canonical inventory: specs decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md D1.

// InteractionDescriptor is the minimal presentation/permission hint for a CollaborationTarget:
// which mode a @ selection produces, whether that mode requires an explicit task, and — for Form Mode
// — the opaque `formRef` the frontend loads its FormDescriptor with. It is a pure value; when
// unavailable it defaults from the target type (see modeForType). Deliberately *not* a capability DSL.
type InteractionDescriptor struct {
	Mode         string `json:"mode"` // mention | task | form
	RequiresTask bool   `json:"requiresTask"`
	FormRef      string `json:"formRef,omitempty"` // form mode only; opaque to Issues (Issue collaboration D7)
}

// CollaborationTargetSummary is the read-only projection of one selectable @ target. Human targets
// are derived from real tenant memberships; agent/team/workflow targets come from the directory.
type CollaborationTargetSummary struct {
	Type                  string                `json:"type"` // user | agent | team | workflow
	ID                    string                `json:"id"`
	DisplayName           string                `json:"displayName"`
	Description           string                `json:"description"`
	InteractionDescriptor InteractionDescriptor `json:"interactionDescriptor"`
}

// CollaborationDirectory lists/resolves non-human collaboration targets (agent/team/workflow) for
// the Issues-facing target projection. It is deliberately NOT an Agent/Team/Workflow domain API.
type CollaborationDirectory interface {
	ListTargets(ctx context.Context, tenantID, query string) ([]CollaborationTargetSummary, error)
	ResolveTarget(ctx context.Context, tenantID, targetType, targetID string) (CollaborationTargetSummary, bool, error)
}

// DirectoryChain composes directory sources in priority order, for a deployment whose target types
// have separate real backends. Listing concatenates each source's matches in chain order; resolution
// returns the first source that can resolve the target. A source's error stops the chain and is
// returned as-is, so a database failure can never surface as "not found". Cloud serves Agent and
// Workflow this way: the space_agents roster in front of the workflow-backed directory, rather than
// one source replacing the other.
type DirectoryChain []CollaborationDirectory

// ListTargets returns each source's matches for the same tenant and query, in chain order.
func (c DirectoryChain) ListTargets(ctx context.Context, tenantID, query string) ([]CollaborationTargetSummary, error) {
	out := []CollaborationTargetSummary{}
	for _, source := range c {
		items, err := source.ListTargets(ctx, tenantID, query)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// ResolveTarget returns the first source's resolution. A target no source can resolve is not-found,
// never an error: the caller turns it into the same 404 an unknown id produces.
func (c DirectoryChain) ResolveTarget(ctx context.Context, tenantID, targetType, targetID string) (CollaborationTargetSummary, bool, error) {
	for _, source := range c {
		target, ok, err := source.ResolveTarget(ctx, tenantID, targetType, targetID)
		if err != nil || ok {
			return target, ok, err
		}
	}
	return CollaborationTargetSummary{}, false, nil
}

// ContextInput is the deterministic, invocation-time material passed to a ContextBuilder. It is a
// value, not a persisted bundle — the builder's output is snapshotted into IssueRun.input.
//
// One seam serves every interaction mode (Issue collaboration D5): Task Mode uses `Task`, Form Mode uses
// `InteractionValues`; both share the issue context and the selected refs. There is deliberately no
// WorkflowContextBuilder / AgentContextBuilder fork.
type ContextInput struct {
	IssueTitle        string
	IssueDescription  string
	Task              string
	InteractionValues Object
	TargetType        string
	TargetID          string
	RecentComments    []string
	ContextRefs       []Object
}

// IssueFormContext is the already-authorized issue material a descriptor may be tailored with
// (Issue collaboration D8). It is a value the Issue domain resolves inside its own transaction and hands
// over, so a provider never reads the Issues tables and can never resolve the same issue from another
// tenant's point of view. The zero value means "no issue context": the descriptor is then projected
// exactly as it was before this context existed, and no platform field is injected.
//
// Every field is optional in the same sense: an issue with no project leaves the repository, branch
// and project id empty, and the platform fields they feed either lose their default or drop out
// entirely. Nothing here is an error to be missing.
type IssueFormContext struct {
	IssueID       string
	RepositoryURL string
	DefaultBranch string
	// ProjectID and ParentIssueID are the two references an issue can name for itself, and the only
	// options the supplementary context-reference field offers.
	ProjectID     string
	ParentIssueID string
}

// FormDescriptorProvider resolves the Issues-facing FormDescriptor for a `formRef` (Issue collaboration D7). Issues
// never learns whether the descriptor comes from a Workflow database, an HTTP/RPC service, a JSON
// Schema, a DSL, code or protobuf — the adapter owns that mapping.
//
// `issue` carries the invocation-time issue context the descriptor may be tailored with; it is the
// zero value when the caller has none. A provider that ignores it stays correct — it simply never
// contributes platform fields.
//
// ok=false means the provider does not know this formRef (404); a nil provider means the capability is
// not wired in this deployment (503).
type FormDescriptorProvider interface {
	ResolveFormDescriptor(ctx context.Context, tenantID, formRef string, issue IssueFormContext) (FormDescriptor, bool, error)
}

// AssistInput is the invocation-time material for one AI Assist request (Issue collaboration D9).
type AssistInput struct {
	TargetType       string
	TargetID         string
	Descriptor       FormDescriptor
	CurrentValues    Object
	IssueTitle       string
	IssueDescription string
	RecentComments   []string
	ContextRefs      []Object
}

// AssistSuggestion is a *suggestion only*: a field-level patch plus suggested context refs. Applying it
// is a user action and execution stays a separate explicit confirm (Issue collaboration D9). It may never carry side
// effects on Issues.
type AssistSuggestion struct {
	Values       Object // field key -> suggested value (partial; unmentioned keys keep the user's value)
	ContextRefs  []Object
	Explanations Object // field key -> human-readable rationale
}

// InputAssistProvider serves AI Assist on a Workflow form. It may only suggest (Issue collaboration D9).
type InputAssistProvider interface {
	Suggest(ctx context.Context, in AssistInput) (AssistSuggestion, error)
}

// ContextBuilder assembles an execution context snapshot from resolved issue material. The
// deterministic first version does no AI; Unavailable falls back to raw unresolved refs.
type ContextBuilder interface {
	Build(ctx context.Context, in ContextInput) (Object, error)
}

// DispatchRequest is the outbound handoff for one queued IssueRun. The run is already persisted and
// `queued`; dispatch is the async-friendly boundary to external execution.
type DispatchRequest struct {
	TenantID     string
	IssueID      string
	RunID        string
	ExecutorType string
	ExecutorID   string
	Input        Object
}

// DispatchResult reports whether an external executor accepted the handoff.
type DispatchResult struct {
	Accepted            bool
	ExternalExecutionID string
	Reason              string
}

// ExecutionDispatcher is the outbound dispatch seam. A mock returns Accepted plus a synthetic
// external id; a real executor returns its own execution reference; blocked/unavailable returns
// Accepted=false and the run simply stays queued.
type ExecutionDispatcher interface {
	Dispatch(ctx context.Context, req DispatchRequest) (DispatchResult, error)
}

// SyncExecutor is an optional dispatcher capability: an executor that completes synchronously (the
// in-memory mock) drives the observer lifecycle to completion before Dispatch's caller returns. A
// real runtime omits it and reports asynchronously through ExecutionObserver instead.
type SyncExecutor interface {
	Execute(ctx context.Context, req DispatchRequest, obs ExecutionObserver)
}

// ExecutionObserver is the inbound seam. The runtime/dispatcher reports started/progress/message/
// completed/failed/cancelled and the Store maps each onto the IssueRun state machine plus Timeline
// activity (started -> running; progress/message -> activity; completed/failed -> terminal;
// cancelled -> cancelled). An agent/team message becomes a reply Comment; a workflow message becomes a
// `system` activity because a workflow is not an Actor (Issue collaboration D2).
type ExecutionObserver interface {
	ObserveStarted(ctx context.Context, tenantID, runID string) (Object, error)
	ObserveProgress(ctx context.Context, tenantID, runID, message string) (Object, error)
	ObserveMessage(ctx context.Context, tenantID, runID, message string) (Object, error)
	ObserveCompleted(ctx context.Context, tenantID, runID string, result Object) (Object, error)
	ObserveFailed(ctx context.Context, tenantID, runID, reason string) (Object, error)
	ObserveCancelled(ctx context.Context, tenantID, runID string) (Object, error)
}

// validTargetType reports whether t is a selectable collaboration target type.
func validTargetType(t string) bool {
	switch t {
	case "user", "agent", "team", "workflow":
		return true
	}
	return false
}

// modeForType is the frozen target-type -> interaction-mode default (Issue collaboration D3). It is the Unavailable
// fallback for InteractionDescriptor; a directory-backed descriptor must agree with it.
func modeForType(targetType string) (mode string, requiresTask bool) {
	switch targetType {
	case "user":
		return "mention", false
	case "agent", "team":
		return "task", true
	case "workflow":
		return "form", false
	}
	return "", false
}

// dispatchTarget carries the post-commit dispatch work appended during a comment transaction.
type dispatchTarget struct {
	tenantID string
	runID    string
}
