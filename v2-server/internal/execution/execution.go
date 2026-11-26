// Package execution defines the in-process Agent execution boundary shared by v2 pages.
// It contains no Node driver, database implementation, or HTTP routes.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Stable errors distinguish a rejected call from an accepted turn's result.
var (
	ErrUnavailable     = errors.New("execution unavailable")
	ErrNotFound        = errors.New("execution not found")
	ErrInvalidRequest  = errors.New("invalid execution request")
	ErrWorkspaceBusy   = errors.New("workspace busy")
	ErrConflict        = errors.New("execution identity conflict")
	ErrUnsupported     = errors.New("execution capability unsupported")
	ErrStopUnconfirmed = errors.New("execution stop not confirmed")
	ErrSessionEnded    = errors.New("execution session ended")
)

// TurnStopSettleLimit is the longest Cancel of an Agent turn may wait for Node to settle a stop
// that keeps the session, before it ends the session instead. A caller that bounds a stop adds it
// to its own budget, so a Node that settles slowly is not taken for a stop that failed.
const TurnStopSettleLimit = 60 * time.Second

// Scope comes from the authenticated page service or persisted schedule owner, never directly
// from a request body. Background callers do not need an online browser or stored cookie.
// Implementations must check membership and resource ownership on every operation.
// Hidden resources return ErrNotFound, including lookups through caller identity.
type Scope struct {
	TenantID string
	UserID   string
}

// CallerKind identifies the page service that owns the business operation.
type CallerKind string

const (
	CallerChat     CallerKind = "chat"
	CallerWorkflow CallerKind = "workflow"
	CallerSchedule CallerKind = "schedule"
	CallerTask     CallerKind = "task"
)

// Round identifies one enclosing workflow container iteration, from outermost to innermost.
// Index is zero-based. ContainerID prevents two sibling containers from aliasing.
type Round struct {
	ContainerID string
	Index       int
}

// Caller is an immutable attempt identity within Scope. ID is the chat conversation ID,
// workflow run ID, schedule occurrence ID, or task run ID, respectively. AttemptID identifies a single
// turn, including a chat turn. An explicit rerun after termination uses a NEW AttemptID.
// NodeID and Rounds apply only to workflow callers; all fields participate in identity.
type Caller struct {
	Kind      CallerKind
	ID        string
	NodeID    string
	Rounds    []Round
	AttemptID string
}

// Target selects a project. Start creates a conversation directory from its stored repository;
// workflow callers share one directory within the same run. Callers never select a directory.
type Target struct {
	ProjectID string
}

// StartRequest starts a session with its first turn. New callers select an Agent and a model
// independently; ExecutorID is retained for older frozen configs that selected the pair as one
// binding. Neither form contains provider credentials or executable paths.
type StartRequest struct {
	MCP        *MCPSelection
	Skills     *Skills
	Caller     Caller
	Target     Target
	AgentID    string
	ModelID    string
	ExecutorID string
	Prompt     Prompt
	// Lifetime says when the service may end the session on its own; the zero value is
	// LifetimeTurn. Only a workflow Agent session may be held.
	Lifetime Lifetime
	// Host gives this turn its host tools; nil gives none. It is never recorded, and no part of
	// the attempt's identity. Only a held workflow session may have it, on every one of its turns.
	Host HostTools
	// Release, when not nil, holds the first turn back from the Agent until the caller decides:
	// the session and its turn are recorded and Start returns, but no Agent sees the prompt
	// before the caller has done what must precede it (a workflow binds its node to the
	// session). A value received from it lets the turn through; its closing abandons the turn,
	// which then ends at once as never started, without waiting for the session to be ended.
	// Ending the session, stopping the turn or closing the service while it waits ends the turn
	// the same way. A caller that sets it must let the turn through or abandon it on every path;
	// it must be buffered, so letting the turn through never blocks. Only a workflow session may
	// have it; it is no part of the attempt's identity.
	Release <-chan struct{}
}

// Lifetime says when the execution service may end an Agent session without being asked.
type Lifetime string

const (
	// LifetimeTurn is the default: a turn that does not complete ends the session.
	LifetimeTurn Lifetime = ""
	// LifetimeHeld keeps a workflow session live after a turn the Agent itself ended without
	// completing (refused, truncated, an unknown stop or a cancellation the caller did not ask
	// for), so the caller can keep talking. A turn that loses the Agent (a crash, a dropped
	// stream, a failed write) still ends the session. The run starting another session does
	// not: a run's sessions share its directory side by side (see Start).
	LifetimeHeld Lifetime = "held"
)

// AppendRequest adds a turn to a live session, retaining its workspace and executor binding.
// A new caller attempt is required. Dead sessions cannot be silently recreated or replayed.
type AppendRequest struct {
	SessionID string
	Caller    Caller
	Prompt    Prompt
	// Host gives this turn its host tools (see StartRequest.Host).
	Host HostTools
}

// Ref identifies an accepted turn. SessionID is stable across Append calls; TurnID is not.
type Ref struct {
	SessionID string
	TurnID    string
}

// Status describes a turn, independently of the Agent's protocol stop reason.
type Status string

const (
	StatusCompleted   Status = "completed"
	StatusCancelled   Status = "cancelled"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

// Result is written once at termination. Answer is the assembled visible assistant text,
// excluding thought/tool output; it can be empty or partial on failure/cancellation.
// StopReason retains the protocol reason separately. FailureCode is a stable, sanitized code,
// never an infrastructure error string. Consumers decide how these map to their domain status.
type Result struct {
	MCPFailure *MCPFailure `json:",omitempty"`
	Status     Status
	Answer     string
	// AnswerComplete is true only after authoritative full-result retrieval. Completed
	// requires true; missing, damaged or over-limit history produces failed + a reason.
	AnswerComplete bool
	StopReason     string
	FailureCode    string
	// Usage is absent for the fixed Node route, which does not relay PromptResponse.usage.
	// Model-only providers may normalize reported counters to ACP Usage. Never invent zeroes.
	Usage json.RawMessage
	// AgentLost means the turn ended because its Agent session is gone (a crash, a dropped
	// stream, an unreadable history, an ended or stopped session): no further turn can follow,
	// whatever the session's Lifetime. A turn the Agent itself ended, completed or not, leaves it
	// false.
	AgentLost bool `json:",omitempty"`
	// NotStarted means the turn failed before its session's Agent ever started: the project,
	// checkout, plugin or Skills could not be prepared, or the turn was refused before anything
	// was sent to Node (the Agent's configuration or model permit, or a session ended or a Node
	// lost meanwhile). It comes with AgentLost on a session's
	// first turn, and tells a caller that there was never a session to keep, as opposed to one
	// that was lost while it worked. A turn that did not start was not delivered either.
	NotStarted bool `json:",omitempty"`
	// NotDelivered means the turn failed and its prompt never reached the Agent: it was refused
	// before its command was sent to Node (the session had ended, broken or lost its Node, the
	// project could not be resolved, or the turn did not start), or Node refused the command
	// without running it (its session had already ended, or the MCP generation it waited for
	// never came). AgentLost still says whether the session can take another turn. A turn whose
	// sending failed midway may have arrived, so it is not marked. A cancelled turn carries it
	// too when the caller stopped it before its prompt reached the Agent (Node dropped it before
	// admission, or never held its refused submission): it answered nothing.
	NotDelivered bool `json:",omitempty"`
	// SessionKept tells the execution service that the Agent session outlives this turn although
	// it did not complete: the caller stopped it (a driver's StopTurn) and Node kept the session.
	// It is what lets a non-held session take the next turn after a stop. Never persisted: a
	// stored result reports a lost session through AgentLost.
	SessionKept bool `json:"-"`
}

// Failure codes of a turn whose session Node refused to admit to, or could not hand over, a
// checkout that other sessions may share: a restore holds the checkout, it already has its limit
// of unfinished sessions, or its tree is nested too deep to hand over. They are Node's stable
// terminal details; only the refused session ends, the checkout's other sessions keep working.
const (
	FailureCheckoutBusy         = "checkout_busy"
	FailureCheckoutSessionLimit = "checkout_session_limit"
	FailureCheckoutTooDeep      = "checkout_too_deep"
)

// FailureDeliveryUnavailable identifies a terminal fallback when delivery cannot be recorded.
const FailureDeliveryUnavailable = "delivery_unavailable"

// FailureWorkspaceMissing is the failure code of a turn whose checkout, prepared by an earlier Node
// incarnation or process, no longer exists: its directory is gone, it was never recorded with its
// clone execution, or Node no longer knows that clone execution. Nothing can continue the session
// there, so it ends.
const FailureWorkspaceMissing = "workspace_missing"

// FailureNodeLost is the failure code of a turn whose session lost its Node: the Node process
// exited or its connection broke. A held workflow session survives it (its next turn starts a new
// Agent, see Recovery); any other session ends.
const FailureNodeLost = "agent_transport_closed"

// FailureServerRestarted is the failure code of a turn, and the end reason of a host execution, that
// a server restart cut.
const FailureServerRestarted = "server_restarted"

// HostSessionEnded is the end reason of a host execution whose session the server ended. The other
// reasons are FailureServerRestarted, FailureNodeLost, the failure that made the server end the
// session, and those Node reports for an Agent session that ended by itself.
const HostSessionEnded = "session_ended"

// ErrNodeLost is returned with FailureNodeLost when the Node was already gone before the turn
// reached its Agent: nothing of the turn ran, so a replacement Agent can take it at once.
var ErrNodeLost = errors.New("the session's Node was lost before the turn reached its Agent")

// CheckoutRefused reports whether code is one of Node's checkout refusals.
func CheckoutRefused(code string) bool {
	return code == FailureCheckoutBusy || code == FailureCheckoutSessionLimit || code == FailureCheckoutTooDeep
}

// Turn is the readable record of one accepted prompt. Result is nil only while running.
type Turn struct {
	// MCPGeneration is the minimum desired generation when this turn was admitted.
	// A nil value preserves ordinary sessions and turns recorded before lifecycle support.
	MCPGeneration *int64 `json:",omitempty"`
	Ref           Ref
	Caller        Caller
	Prompt        Prompt
	StartedAt     time.Time
	EndedAt       *time.Time
	Result        *Result
	// Host is set, never persisted, while the turn executes (see StartRequest.Host).
	Host HostTools `json:"-"`
}

// Session is shared execution metadata; page titles, ordering and deletion remain page-owned.
// EndedAt means the execution session can no longer accept Append, not merely that a turn ended.
type Session struct {
	MCP    *MCPSelection `json:",omitempty"`
	Skills *Skills       `json:",omitempty"`
	ID     string
	// Target/Executor are nil only for model-only sessions. Selection snapshots never
	// change when the configured catalog changes; no credentials are retained here.
	Target   *Target
	Executor *Executor
	Model    Model
	// WorkspaceID is a server-generated directory identity, empty for model-only sessions.
	// Sessions belonging to the same workflow run share this identity.
	WorkspaceID string
	CreatedAt   time.Time
	EndedAt     *time.Time
	// Recovery is set, never persisted, on the first turn a held session takes after a server
	// restart lost its Agent: the driver starts a new Agent on the same checkout (reattached
	// beforehand, see executionruntime) and hands it the recorded conversation instead of starting
	// from nothing.
	Recovery *Recovery `json:"-"`
}

// Recovery is what a driver needs to bring a held session back after its Agent was lost with the
// server process. The new Agent is a new provider session: it learns the conversation from
// Handoff, which lists tool calls by title and outcome only, and has no prompt cache.
type Recovery struct {
	// Handoff is sent ahead of the turn's prompt as its own block. It is context for the Agent
	// only: the turn records the prompt alone, and the page never shows the block.
	Handoff string
	// History is the settled conversation the page showed before the restart; the new Agent's
	// history is published after it, so the page keeps one continuous conversation.
	History []HistoryRecord
}

// TurnPage lists accepted turns in stable creation order. NextCursor is empty at the current
// end; cursors are opaque and scoped to the session. Events are read separately via Subscribe.
type TurnPage struct {
	Items      []Turn
	NextCursor string
}

// EventKind distinguishes protocol updates from the authoritative turn result.
type EventKind string

const (
	EventHistorySnapshot EventKind = "history_snapshot"
	// EventHistoryPreview replaces only the pending (preview) records of the latest snapshot.
	EventHistoryPreview EventKind = "history_preview"
	EventTurnEnded      EventKind = "turn_ended"
	EventTextDelta      EventKind = "text_delta"
	EventSessionEnded   EventKind = "session_ended"
	// EventPreparation replaces the timed stages of the session's path to first output.
	EventPreparation EventKind = "preparation"
)

// Event is immutable after publication. Sequence starts at 1 and increases without gaps within
// SessionID. Ref.TurnID is empty outside a turn. On history_snapshot, History replaces the entire
// displayed Node projection; on history_preview it holds only the still-pending records and
// replaces the pending records of the latest snapshot. Neither is appended as ACP chunks. Result is set only on TurnEnded, which does
// not prove session shutdown. Thought/tool text is not part of Answer.
type Event struct {
	Ref        Ref
	Sequence   uint64
	OccurredAt time.Time
	Kind       EventKind
	History    *HistorySnapshot
	// TextDelta carries model-only text without inventing ACP tools or Agent events.
	TextDelta string
	Result    *Result
	// Preparation is set only on EventPreparation.
	Preparation *Preparation
}

// Preparation is what a session's first turn waited for, stage by stage. It is diagnostic only.
type Preparation struct {
	Stages []PreparationStage `json:"stages"`
}

// PreparationStage is one timed stage; EndedAt is nil while it runs. Reused means work prepared
// in advance served it.
type PreparationStage struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Reused    bool       `json:"reused,omitempty"`
}

// HistorySnapshot is a full replacement of the available settled/pending Node history.
// Missing ACP chrome/usage is unavailable, not zero. Incomplete concerns process display;
// Result.AnswerComplete independently describes the authoritative final answer.
type HistorySnapshot struct {
	Records    []HistoryRecord `json:"records"`
	Incomplete bool            `json:"incomplete"`
}

// HistoryRecord identifies an upsert position within a Node history, not the session's Event
// cursor. Data is the fixed Node's HistoryLine/preview record, including available _meta and
// tool details, not a raw ACP delta. Preview is provisional and must never form a final result.
type HistoryRecord struct {
	Sequence uint32          `json:"sequence"`
	Data     json.RawMessage `json:"data"`
	Preview  bool            `json:"preview,omitempty"`
}

// Stream replays retained events then follows live ones with no handoff gap. Next returns io.EOF
// only after session_ended is delivered, or immediately if the cursor already acknowledges it.
// turn_ended does not close the stream: later snapshots and subsequent turns remain observable.
// A transport/storage failure is a non-EOF error.
// One goroutine calls Next; Close may run concurrently and must unblock it. Context cancellation
// or Close releases the subscription only: stopping observation must never cancel execution.
type Stream interface {
	Next(context.Context) (Event, error)
	Close() error
}

// Service is the shared execution port. This package defines the contract, not its runtime.
// Start/StartModel/Append record a turn under a process-local mutex and a database write before
// returning its Ref. Their contexts control this call, not the subsequent execution lifetime.
// Within a scope, repeating an identical caller attempt and request returns the original Ref;
// changing its target, executor, session or prompt returns ErrConflict without dispatching again.
// ErrWorkspaceBusy means the previous turn in this session (or conversation) is still active.
// Different conversations and workflow runs do not compete for a directory, even in one project.
// Pages own their queues; this service does not enqueue a rejected request.
type Service interface {
	// Start clones a directory for this conversation or reuses this workflow run's directory.
	// A workflow run's sessions share its directory and stay live side by side: Start never
	// ends the run's earlier sessions, which their caller ends. The Node allows it through its
	// shared_checkout_sessions capability and refuses only a session over its per-checkout
	// limit. Each session executes one turn at a time (ErrWorkspaceBusy); the run's sessions
	// execute theirs concurrently.
	Start(context.Context, Scope, StartRequest) (Ref, error)
	StartModel(context.Context, Scope, ModelStartRequest) (Ref, error)
	// Append keeps the execution mode/binding; ended sessions return ErrSessionEnded.
	Append(context.Context, Scope, AppendRequest) (Ref, error)

	// Subscribe returns events strictly after afterSequence, replaying from the start at zero.
	// Sequences are session-wide. Workflow/schedule consumers filter TurnID and stop observing
	// on their turn's terminal event, not EOF. An ahead-of-history cursor is invalid.
	Subscribe(ctx context.Context, scope Scope, sessionID string, afterSequence uint64) (Stream, error)

	// Cancel waits for execution-side confirmation and durable terminal publication. It is
	// idempotent for terminal turns and returns their actual result, including a racing success.
	// Cancelling an active Agent turn stops only that turn when the session's Node keeps
	// sessions after a cancelled turn (turn_cancel); the session then takes further Append.
	// Otherwise, or when Node decides it cannot keep the session, the entire Node session ends,
	// the result reports AgentLost and subsequent Append returns ErrSessionEnded. It must not
	// replay history or silently replace the Agent to continue it.
	// Model-only cancellation stops just the current provider call and permits later Append.
	// A terminal turn is a no-op and cannot stop a later turn. A deadline or ErrStopUnconfirmed
	// does not confirm stopping; callers observe the actual result through reads/events.
	Cancel(context.Context, Scope, Ref) (Result, error)

	// End closes only this session, stopping its active turn and waiting for confirmation.
	// Successful End is idempotent, keeps history and leaves the directory on disk. It must
	// not stop other sessions on the same project's Node or rewrite a persisted Result.
	// Restart ends old live sessions and marks running turns interrupted before serving requests;
	// it does not replay prompts. Append to any ended session returns ErrSessionEnded.
	End(context.Context, Scope, string) error
	ReadSession(context.Context, Scope, string) (Session, error)
	// ReadTurns accepts a limit from 1 to 100; an empty cursor starts at the first turn.
	ReadTurns(ctx context.Context, scope Scope, sessionID, cursor string, limit int) (TurnPage, error)
	// ReadAttempt looks up a recorded turn by its complete caller identity without dispatching.
	ReadAttempt(context.Context, Scope, Caller) (Turn, error)
}
