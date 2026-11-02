package workflow

import "context"

// LoopRound is an isolated, one-based execution scope. Root attempts have an empty ScopeID
// inside the engine; the repository translates that to the run's durable root scope.
type LoopRound struct {
	ID, ParentID, Status string
	Index                int
	State                []byte
}

// LoopRoundStart opens a round and its Start attempt in the same transaction.
type LoopRoundStart struct {
	Index int
	State []byte
	Entry NodeStart
}

// ContainerResult settles a parent together with its round/ledger and exported variables.
type ContainerResult struct {
	Status        string
	Output, Error *string
	Keys          map[string]any
}

// ContainerAdvance is one atomic container transition. A current Loop scope is closed before
// Next opens; an exit only freezes State and cancels its children until the host acknowledges
// cleanup. Fold changes the root payload, including an Iteration ledger and its continuation.
type ContainerAdvance struct {
	ScopeID, ScopeStatus string
	ScopeState           []byte
	CancelScope          bool
	ExitNodeID           string
	Next                 *LoopRoundStart
	Starts               []NodeStart
	Fold                 func([]byte) ([]byte, error)
	Result               *ContainerResult
}

// ContainerRepository extends the flat execution port with scoped reads and atomic advancement.
type ContainerRepository interface {
	LoopRounds(context.Context, string) ([]LoopRound, error)
	ScopeNodes(context.Context, string, string) ([]NodeRun, error)
	AdvanceContainer(context.Context, string, string, ContainerAdvance) ([]NodeRun, bool, error)
}

// LoopExitDispatcher drains only the exiting scope without holding the engine's run gate.
// It must acknowledge through FinishLoopExit, after the scope's executing turn has stopped.
type LoopExitDispatcher interface {
	CleanupLoopExit(runID, parentID, scopeID string)
}
