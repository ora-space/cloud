//go:build linux

package agentruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
	"github.com/wanglongan587/cloud/v2-server/internal/nodeservices"
)

// cancelCommand is the payload of a cancel_turn the driver sent.
type cancelCommand struct {
	NodeID    string `json:"node_id"`
	CommandID string `json:"command_id"`
	TurnID    string `json:"turn_id"`
}

// nextCancel waits for the driver's cancel_turn and checks it names turn on the session's
// execution with a command ID the settlement can echo.
func nextCancel(t *testing.T, f *fakeNodeIPC, start node.Message, turn string) cancelCommand {
	t.Helper()
	m := nextIPCCommand(t, f, "cancel_turn")
	var c cancelCommand
	if err := json.Unmarshal(m.Payload, &c); err != nil || m.ExecutionID != start.ExecutionID || c.NodeID != f.id.NodeID || c.CommandID == "" || c.TurnID != turn {
		t.Fatalf("cancel_turn = %+v on %s (err %v), want turn %s on %s", c, m.ExecutionID, err, turn, start.ExecutionID)
	}
	return c
}

// settleCancel sends Node's TurnCancelSettled for the command, as the execution's next event.
func (f *fakeNodeIPC) settleCancel(start node.Message, c cancelCommand, outcome string) {
	f.seq[start.ExecutionID]++
	sequence := f.seq[start.ExecutionID]
	payload, _ := json.Marshal(map[string]string{"command_id": c.CommandID, "turn_id": c.TurnID, "outcome": outcome})
	f.in <- node.Message{Type: "turn_cancel_settled", Version: 1, OperationID: start.OperationID, ExecutionID: start.ExecutionID, Sequence: &sequence, Payload: payload}
}

// startSpec decodes the AgentSessionSpec a start_agent_session carried.
func startSpec(t *testing.T, start node.Message) map[string]any {
	t.Helper()
	var started struct{ Spec map[string]any }
	if err := json.Unmarshal(start.Payload, &started); err != nil {
		t.Fatal(err)
	}
	return started.Spec
}

// activeGuard is the retry guard of the session's running turn.
func activeGuard(t *testing.T, h *ipcHarness, sid string) *retryGuard {
	t.Helper()
	h.driver.mu.Lock()
	a := h.driver.sessions[sid]
	h.driver.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active == nil {
		t.Fatal("no running turn")
	}
	return &a.active.guard
}

// cutOnceRetrying records what a stalled turn looks like to the guard: one model call cut at the
// idle limit, and the Agent's retry still running.
func cutOnceRetrying(g *retryGuard) {
	_, cut := g.begin()
	g.finish(cut, false, nodeservices.CutIdle)
	g.begin()
}

// stopTurn asks the driver to stop only turn, as execution.Service.Cancel does, and fails the test
// unless the driver takes the stop.
func stopTurn(t *testing.T, h *ipcHarness, session execution.Session, turn string) {
	t.Helper()
	if !h.driver.StopTurn(execution.Ref{SessionID: session.ID, TurnID: turn}) {
		t.Fatalf("the driver refused to stop only turn %s", turn)
	}
}

// turnEnds collects the TurnEnded publications of a run.
func turnEnds() (Emit, chan execution.Event) {
	events := make(chan execution.Event, 8)
	return func(_ context.Context, event execution.Event) error {
		if event.Kind == execution.EventTurnEnded {
			events <- event
		}
		return nil
	}, events
}

// A stop over a connection that negotiated turn_cancel cancels only the turn: the session chose
// keep_session_on_cancel (and nothing for failures), Node's TurnEnded waits for its settlement,
// a stopped settlement keeps the session, and the next turn goes to the same Agent. No
// EndSession is sent.
func TestDriverIPCStoppedTurnKeepsTheSessionForTheNextTurn(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, ctx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	spec := startSpec(t, start)
	if _, failure := spec["keep_session_on_failure"]; spec["keep_session_on_cancel"] != true || failure {
		t.Fatalf("keep choices in the start spec = %v / %v, want only keep_session_on_cancel", spec["keep_session_on_cancel"], spec["keep_session_on_failure"])
	}
	stopTurn(t, h, session, "first")
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "partial", "cancelled")
	receiveIPC(t, f.acked)
	select {
	case event := <-events:
		t.Fatalf("the stopped turn was published before its settlement: %+v", event)
	case got := <-done:
		t.Fatalf("Run returned before the settlement: %+v", got)
	default:
	}
	f.settleCancel(start, cancel, "stopped")
	if event := receiveIPC(t, events); event.Ref.TurnID != "first" || event.Result == nil || event.Result.Status != execution.StatusCancelled || event.Result.Answer != "partial" {
		t.Fatalf("stopped turn publication = %+v", event)
	}
	if ack := receiveIPC(t, f.acked); ack.Type != "turn_cancel_settled" {
		t.Fatalf("acknowledged %s, want the settlement", ack.Type)
	}
	got := receiveIPC(t, done)
	if got.err != nil || got.result.Status != execution.StatusCancelled || !got.result.SessionKept || got.result.AgentLost || got.result.FailureCode != "" || got.result.NotDelivered {
		t.Fatalf("stopped turn = %+v", got)
	}
	second := startIPCRun(h, ctx, session, "second", emit)
	submit := nextIPCCommand(t, f, "submit_user_turn")
	if submit.ExecutionID != start.ExecutionID {
		t.Fatal("the turn after a stop replaced the physical Agent")
	}
	f.finishTurn(t, submit, "second", "answer")
	if next := receiveIPC(t, second); next.err != nil || next.result.Status != execution.StatusCompleted || next.result.Answer != "answer" {
		t.Fatalf("turn after the stop = %+v", next)
	}
	if sent := sentIPC(f); slices.Contains(sent, "end_session") {
		t.Fatalf("a kept session was ended: %v", sent)
	}
}

// cloud#369: a turn whose model call was cut at the idle limit while the Agent's retry was still
// running fails with the cut when the turn ends cancelled on its own (Node records failures as
// cancelled), but stays cancelled, and its session kept, when the user stopped it.
func TestDriverIPCUserStopAfterACutStaysCancelled(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, ctx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	stopTurn(t, h, session, "first")
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "", "cancelled")
	f.settleCancel(start, cancel, "stopped")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled || event.Result.FailureCode != "" {
		t.Fatalf("a stop after a cut was published as %+v", event.Result)
	}
	got := receiveIPC(t, done)
	if got.err != nil || got.result.Status != execution.StatusCancelled || got.result.FailureCode != "" || !got.result.SessionKept {
		t.Fatalf("user stop after a cut = %+v", got)
	}

	second := startIPCRun(h, ctx, session, "second", emit)
	submit := nextIPCCommand(t, f, "submit_user_turn")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	f.finishTurnAs(t, submit, "second", "", "cancelled")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusFailed || event.Result.FailureCode != nodeservices.CutIdle {
		t.Fatalf("a cut turn nobody stopped was published as %+v", event.Result)
	}
	if next := receiveIPC(t, second); next.err != nil || next.result.Status != execution.StatusFailed || next.result.FailureCode != nodeservices.CutIdle || next.result.SessionKept {
		t.Fatalf("cut turn without a stop = %+v", next)
	}
}

// A Node that will not keep the session settles the stop as session_ending: the turn is the
// user's stop (cut or not), the session is lost, and the driver sends no EndSession of its own;
// End returns once Node's terminal proves the stop.
func TestDriverIPCStopNodeCannotKeepEndsTheSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, ctx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	stopTurn(t, h, session, "first")
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "partial", "cancelled")
	f.settleCancel(start, cancel, "session_ending")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled || !event.Result.AgentLost || event.Result.FailureCode != "" {
		t.Fatalf("an unkept stop was published as %+v", event.Result)
	}
	got := receiveIPC(t, done)
	if !errors.Is(got.err, execution.ErrSessionEnded) || got.result.Status != execution.StatusCancelled || !got.result.AgentLost || got.result.SessionKept || got.result.Answer != "partial" {
		t.Fatalf("unkept stop = %+v", got)
	}
	ended := make(chan error, 1)
	go func() { ended <- h.driver.End(ctx, session.ID) }()
	select {
	case err := <-ended:
		t.Fatalf("End returned before Node's terminal: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	f.terminalAs(start, "agent_failed", "turn_boundary_unverified")
	if err := receiveIPC(t, ended); err != nil {
		t.Fatal(err)
	}
	if sent := sentIPC(f); slices.Contains(sent, "end_session") {
		t.Fatalf("the driver ended a session Node was already ending: %v", sent)
	}
}

// A turn Node dropped before its Agent received it has no TurnEnded: the driver records it as
// cancelled itself and the session takes the next turn.
func TestDriverIPCStopOfATurnNotStartedKeepsTheSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	emit, events := turnEnds()
	done := startIPCRun(h, ctx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	stopTurn(t, h, session, "first")
	f.settleCancel(start, nextCancel(t, f, start, "first"), "not_started")
	if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled {
		t.Fatalf("dropped turn publication = %+v", event)
	}
	if got := receiveIPC(t, done); got.err != nil || got.result.Status != execution.StatusCancelled || !got.result.SessionKept || !got.result.NotDelivered {
		t.Fatalf("dropped turn = %+v, want a kept stop that never reached the Agent", got)
	}
	second := startIPCRun(h, ctx, session, "second", emit)
	f.finishTurn(t, nextIPCCommand(t, f, "submit_user_turn"), "second", "answer")
	if next := receiveIPC(t, second); next.err != nil || next.result.Status != execution.StatusCompleted {
		t.Fatalf("turn after a dropped turn = %+v", next)
	}
}

// A turn that ends on its own before the cancel takes effect keeps its own result; Node then
// settles the cancel as already_ended.
func TestDriverIPCStopRacingTheTurnsEndKeepsItsResult(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	emit, _ := turnEnds()
	done := startIPCRun(h, ctx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	stopTurn(t, h, session, "first")
	cancel := nextCancel(t, f, start, "first")
	f.finishTurn(t, start, "first", "complete answer")
	f.settleCancel(start, cancel, "already_ended")
	if got := receiveIPC(t, done); got.err != nil || got.result.Status != execution.StatusCompleted || got.result.Answer != "complete answer" || got.result.SessionKept {
		t.Fatalf("turn that won the race = %+v", got)
	}
	// A settlement for a turn that already has its result changes nothing, and is acknowledged.
	receiveIPC(t, f.acked)
	receiveIPC(t, f.acked)
	f.settleCancel(start, cancel, "already_ended")
	if ack := receiveIPC(t, f.acked); ack.Type != "turn_cancel_settled" {
		t.Fatalf("acknowledged %s, want the late settlement", ack.Type)
	}
}

// Whenever Node cannot settle the stop for a kept session (the session did not negotiate
// turn_cancel, so StopTurn refuses and the caller cancels the turn; Node refuses the command or
// does not know the turn; or no settlement arrives in time), the stop ends the session through
// EndSession, as before turn_cancel.
func TestDriverIPCStopFallsBackToEndingTheSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		turnCancel bool
		answer     func(f *fakeNodeIPC, start node.Message, cancel node.Message)
		wait       time.Duration
	}{
		{name: "not negotiated"},
		{name: "refused", turnCancel: true, answer: func(f *fakeNodeIPC, start, cancel node.Message) {
			var c cancelCommand
			_ = json.Unmarshal(cancel.Payload, &c)
			f.reply(cancel, "session_command_rejected", map[string]string{"command_id": c.CommandID, "reason": "turn_cancel_unsupported"})
		}},
		{name: "unknown turn", turnCancel: true, answer: func(f *fakeNodeIPC, start, cancel node.Message) {
			var c cancelCommand
			_ = json.Unmarshal(cancel.Payload, &c)
			f.settleCancel(start, c, "unknown")
		}},
		{name: "never settled", turnCancel: true, wait: 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newIPCHarness(t, nil)
			h.turnCancel.Store(tc.turnCancel)
			if tc.wait != 0 {
				h.driver.turnCancelWait = tc.wait
			}
			ctx := ipcContext(t)
			runCtx, end := context.WithCancel(ctx)
			defer end()
			session := h.session("session", "workspace")
			done := startIPCRun(h, runCtx, session, "first", discardIPCEvent)
			f := receiveIPC(t, h.created)
			start := nextIPCCommand(t, f, "start_agent_session")
			if spec := startSpec(t, start); (spec["keep_session_on_cancel"] == true) != tc.turnCancel {
				t.Fatalf("keep_session_on_cancel = %v with turn_cancel negotiated %v", spec["keep_session_on_cancel"], tc.turnCancel)
			}
			if took := h.driver.StopTurn(execution.Ref{SessionID: session.ID, TurnID: "first"}); took != tc.turnCancel {
				t.Fatalf("StopTurn took the stop = %v with turn_cancel negotiated %v", took, tc.turnCancel)
			}
			if !tc.turnCancel {
				end() // what execution.Service.Cancel does when the driver cannot keep the session
			}
			var kinds []string
			for {
				m := receiveIPC(t, f.sent)
				kinds = append(kinds, m.Type)
				if m.Type == "cancel_turn" && tc.answer != nil {
					tc.answer(f, start, m)
				}
				if m.Type == "end_session" {
					f.terminal(m)
					break
				}
			}
			if slices.Contains(kinds, "cancel_turn") != tc.turnCancel {
				t.Fatalf("commands after the stop = %v", kinds)
			}
			got := receiveIPC(t, done)
			if got.err == nil || got.result.Status != execution.StatusCancelled || got.result.SessionKept {
				t.Fatalf("stop that ended the session = %+v", got)
			}
		})
	}
}

// startStoppable starts the session's first turn over a Node with turn_cancel and returns its run,
// the fake Node, the start command and the cancel the stop sent.
func startStoppable(t *testing.T, h *ipcHarness, ctx context.Context, session execution.Session, emit Emit) (<-chan ipcRunOutcome, *fakeNodeIPC, node.Message, cancelCommand) {
	t.Helper()
	h.turnCancel.Store(true)
	done := startIPCRun(h, ctx, session, "first", emit)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	stopTurn(t, h, session, "first")
	return done, f, start, nextCancel(t, f, start, "first")
}

// Once Run stopped waiting for the settlement and is ending the session, a settlement that keeps
// the session arrives too late: the turn still reports its session lost, also in what it
// published, so nobody sends another turn to an ending session.
func TestDriverIPCLateSettlementAfterTheWaitNeverKeepsTheSession(t *testing.T) {
	for _, outcome := range []string{"stopped", "not_started"} {
		t.Run(outcome, func(t *testing.T) {
			h := newIPCHarness(t, nil)
			h.driver.turnCancelWait = 50 * time.Millisecond
			ctx := ipcContext(t)
			session := h.session("session", "workspace")
			emit, events := turnEnds()
			done, f, start, cancel := startStoppable(t, h, ctx, session, emit)
			end := nextIPCCommand(t, f, "end_session")
			if outcome == "stopped" {
				f.finishTurnAs(t, start, "first", "partial", "cancelled")
			}
			f.settleCancel(start, cancel, outcome)
			f.terminal(end)
			if event := receiveIPC(t, events); event.Result == nil || event.Result.Status != execution.StatusCancelled || !event.Result.AgentLost {
				t.Fatalf("late settlement was published as %+v", event.Result)
			}
			got := receiveIPC(t, done)
			if !errors.Is(got.err, execution.ErrSessionEnded) || got.result.SessionKept || !got.result.AgentLost || got.result.Status != execution.StatusCancelled {
				t.Fatalf("turn whose session was ended = %+v", got)
			}
		})
	}
}

// The wait for a settlement restarts on every message of the session (cancel-turn ADR D4: a
// backlog that is still being delivered is not a Node that does not settle), but never runs past
// its limit.
func TestDriverIPCSettlementWaitFollowsDeliveryProgress(t *testing.T) {
	// The window is ten event intervals long, so a slow, loaded runner never misses a restart;
	// the session keeps delivering for four windows.
	const window, every = 500 * time.Millisecond, 50 * time.Millisecond
	for _, tc := range []struct {
		name  string
		limit time.Duration
		kept  bool
	}{
		{name: "events keep the wait going", limit: time.Minute, kept: true},
		{name: "the limit ends it", limit: 2 * window},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newIPCHarness(t, nil)
			h.driver.turnCancelWait = window
			h.driver.turnCancelLimit = tc.limit
			// Longer than the waits below, so only the driver's own limit can end this wait.
			ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			t.Cleanup(stop)
			session := h.session("session", "workspace")
			done, f, start, cancel := startStoppable(t, h, ctx, session, discardIPCEvent)
			accepted, _ := json.Marshal(map[string]string{"command_id": cancel.CommandID})
			deliver := func() bool {
				f.in <- node.Message{Type: "session_command_accepted", Version: 1, OperationID: start.OperationID, ExecutionID: start.ExecutionID, Payload: accepted}
				time.Sleep(every)
				return slices.Contains(sentIPC(f), "end_session")
			}
			if tc.kept {
				for deadline := time.Now().Add(4 * window); time.Now().Before(deadline); {
					if deliver() {
						t.Fatal("EndSession sent while the session kept delivering")
					}
				}
				f.finishTurnAs(t, start, "first", "", "cancelled")
				f.settleCancel(start, cancel, "stopped")
				if got := receiveIPC(t, done); got.err != nil || !got.result.SessionKept {
					t.Fatalf("stop settled after a long delivery = %+v", got)
				}
				return
			}
			// Delivery never pauses, so only the limit can end the wait: wait for its EndSession.
			began := time.Now()
			for !deliver() {
				if time.Since(began) > 10*tc.limit {
					t.Fatal("the limit never ended a wait the session kept restarting")
				}
			}
			if waited := time.Since(began); waited < tc.limit-every || waited > 5*tc.limit {
				t.Fatalf("EndSession after %v, want it at the limit of %v", waited, tc.limit)
			}
			f.terminal(start)
			if got := receiveIPC(t, done); got.err == nil || got.result.SessionKept {
				t.Fatalf("stop past the limit = %+v", got)
			}
		})
	}
}

// End, Close and shutdown cancel Run's context, which a stop does not: while a stop awaits its
// settlement they still end the session at once, without waiting out the window.
func TestDriverIPCEndDuringTheSettlementWaitEndsAtOnce(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.driver.turnCancelWait = time.Hour
	ctx := ipcContext(t)
	runCtx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	session := h.session("session", "workspace")
	done, f, _, _ := startStoppable(t, h, runCtx, session, discardIPCEvent)
	shutdown()
	f.terminal(nextIPCCommand(t, f, "end_session"))
	if got := receiveIPC(t, done); got.err == nil || got.result.Status != execution.StatusCancelled || got.result.SessionKept {
		t.Fatalf("turn ended by shutdown during a stop = %+v", got)
	}
}

// After the wait fell back to EndSession, Node's terminal may give any reason (here: the Agent never
// answered the cancel); the turn is still the caller's stop.
func TestDriverIPCStoppedTurnEndedByAnyTerminalIsCancelled(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.driver.turnCancelWait = 50 * time.Millisecond
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	done, f, _, _ := startStoppable(t, h, ctx, session, discardIPCEvent)
	f.terminalAs(nextIPCCommand(t, f, "end_session"), "agent_failed", "turn_cancel_unanswered")
	if got := receiveIPC(t, done); got.result.Status != execution.StatusCancelled || got.result.FailureCode != "" || !got.result.AgentLost || got.err == nil {
		t.Fatalf("stopped turn ended by an agent_failed terminal = %+v", got)
	}
}

// A terminal that ends the session under a stopped turn hands the turn its published outcome
// before the session reads as stopped: the turn's fall-back End, which waits for that stop, must
// find it rather than make up a result of its own. The ACK of the terminal is held, so the receive
// loop's own hand-over after the ACK cannot come first.
func TestDriverIPCTerminalHandsItsOutcomeOverBeforeTheStop(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.driver.turnCancelWait = 50 * time.Millisecond
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	done, f, _, _ := startStoppable(t, h, ctx, session, discardIPCEvent)
	end := nextIPCCommand(t, f, "end_session")
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	f.holdAck.Store(&map[string]chan struct{}{"agent_session_ended": gate})
	f.terminalAs(end, "agent_failed", "turn_cancel_unanswered")
	if got := receiveIPC(t, done); got.result.Status != execution.StatusCancelled || !got.result.AgentLost || !errors.Is(got.err, execution.ErrSessionEnded) {
		t.Fatalf("stopped turn whose terminal is not yet acknowledged = %+v, want the published outcome", got)
	}
}

// cloud#369: a turn that ended on its own (cancelled after a cut) before the stop took effect keeps
// its own result, the cut included, when Node settles the stop as already_ended.
func TestDriverIPCAlreadyEndedTurnKeepsItsCut(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	done := startIPCRun(h, ctx, session, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	cutOnceRetrying(activeGuard(t, h, session.ID))
	stopTurn(t, h, session, "first")
	cancel := nextCancel(t, f, start, "first")
	f.finishTurnAs(t, start, "first", "", "cancelled")
	f.settleCancel(start, cancel, "already_ended")
	if got := receiveIPC(t, done); got.err != nil || got.result.Status != execution.StatusFailed || got.result.FailureCode != nodeservices.CutIdle || got.result.SessionKept {
		t.Fatalf("turn that failed with a cut before the stop = %+v", got)
	}
}

// A settlement settles only the cancel it names, for the turn it names: one for another command or
// another turn (a late one of an earlier turn, say) is acknowledged and changes nothing.
func TestDriverIPCSettlementOfAnotherCancelIsIgnored(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	done, f, start, cancel := startStoppable(t, h, ctx, session, discardIPCEvent)
	f.finishTurnAs(t, start, "first", "", "cancelled")
	receiveIPC(t, f.acked)
	for _, other := range []cancelCommand{{CommandID: "cancel-other", TurnID: "first"}, {CommandID: cancel.CommandID, TurnID: "earlier"}} {
		f.settleCancel(start, other, "stopped")
		if ack := receiveIPC(t, f.acked); ack.Type != "turn_cancel_settled" {
			t.Fatalf("acknowledged %s, want the foreign settlement", ack.Type)
		}
		select {
		case got := <-done:
			t.Fatalf("a settlement of %+v settled the turn: %+v", other, got)
		case <-time.After(50 * time.Millisecond):
		}
	}
	f.settleCancel(start, cancel, "stopped")
	if got := receiveIPC(t, done); got.err != nil || !got.result.SessionKept {
		t.Fatalf("turn after its own settlement = %+v", got)
	}
}

// A Node that goes away while a stopped turn awaits its settlement ends the session; the turn is
// still the caller's stop, not a lost turn a held session would recover.
func TestDriverIPCNodeLostDuringTheSettlementWaitIsTheCallersStop(t *testing.T) {
	h := newIPCHarness(t, nil)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	done, f, _, _ := startStoppable(t, h, ctx, session, discardIPCEvent)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got := receiveIPC(t, done)
	if got.err == nil || got.result.Status != execution.StatusCancelled || got.result.FailureCode != "" || !got.result.AgentLost || got.result.SessionKept {
		t.Fatalf("stopped turn whose Node went away = %+v", got)
	}
}

// A stop while Node refused the turn's submission (mcp_update_required) and the driver still waits
// for the refresh to send it again: Node holds no such turn, so no CancelTurn goes out (it would
// only learn unknown); the turn stops here, is never sent again, and the session takes the next.
func TestDriverIPCStopOfARefusedSubmissionKeepsTheSession(t *testing.T) {
	var refuse atomic.Bool
	failedRead := make(chan struct{}, 1)
	h := newIPCHarness(t, func(f *fakeNodeIPC, m node.Message) bool {
		if m.Type == "submit_user_turn" && refuse.Load() {
			var command struct {
				ID string `json:"command_id"`
			}
			if err := json.Unmarshal(m.Payload, &command); err != nil {
				return false
			}
			f.reply(m, "session_command_rejected", map[string]string{"command_id": command.ID, "reason": "mcp_update_required"})
			return true
		}
		return false
	})
	h.turnCancel.Store(true)
	h.driver.mcpRefreshWait = time.Minute
	h.driver.opts.ReadMCPTarget = func(ctx context.Context, _ execution.Scope, _ string, _ *execution.MCPSelection, known int64) (*execution.MCPTarget, error) {
		if known == -1 {
			return &execution.MCPTarget{Configured: *ipcMCP()}, nil
		}
		// Only the refused turn's own refresh fails: the session's MCP watcher (followMCP) reads
		// in the background too, under the project's context, and its read must not pass for
		// the refresh the stop below waits for.
		if refuse.Load() && ctx.Value(refusedTurn{}) != nil {
			signal(failedRead)
			return nil, execution.ErrUnavailable
		}
		return nil, nil
	}
	ctx := ipcContext(t)
	session := h.session("refused-session", "workspace")
	session.MCP = ipcMCP()
	run := func(id string) <-chan ipcRunOutcome {
		done := make(chan ipcRunOutcome, 1)
		go func() {
			turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: id}, Prompt: execution.TextPrompt(id), MCPGeneration: new(int64)}
			runCtx := ctx
			if id == "refused" {
				runCtx = context.WithValue(ctx, refusedTurn{}, true)
			}
			result, err := h.driver.Run(runCtx, h.scope, session, turn, discardIPCEvent)
			done <- ipcRunOutcome{result, err}
		}()
		return done
	}
	first := run("first")
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "first", "ready")
	if got := receiveIPC(t, first); got.err != nil {
		t.Fatal(got.err)
	}
	refuse.Store(true)
	refused := run("refused")
	nextIPCCommand(t, f, "submit_user_turn")
	receiveIPC(t, failedRead) // the driver waits for the refresh
	stopTurn(t, h, session, "refused")
	got := receiveIPC(t, refused)
	if got.err != nil || got.result.Status != execution.StatusCancelled || !got.result.SessionKept || !got.result.NotDelivered {
		t.Fatalf("stop of a refused submission = %+v, want a kept stop that never reached the Agent", got)
	}
	refuse.Store(false)
	next := run("next")
	command := nextIPCCommand(t, f, "submit_user_turn")
	if !bytes.Contains(command.Payload, []byte(digest("next"))) {
		t.Fatalf("the stopped submission was sent again: %s", command.Payload)
	}
	f.finishTurn(t, command, "next", "healthy")
	if got := receiveIPC(t, next); got.err != nil || got.result.Status != execution.StatusCompleted {
		t.Fatalf("turn after the stop = %+v", got)
	}
	if sent := sentIPC(f); slices.Contains(sent, "cancel_turn") || slices.Contains(sent, "end_session") {
		t.Fatalf("commands after the stop = %v", sent)
	}
}

// refusedTurn marks the context of the turn whose submission Node refuses.
type refusedTurn struct{}

// A submission Node refused and the driver then sent again (the MCP refresh came) is Node's turn
// again: a stop must reach Node as CancelTurn, never end the turn locally while Node runs it.
func TestDriverIPCStopAfterAResubmissionGoesToNode(t *testing.T) {
	var submits atomic.Int32
	h := newIPCHarness(t, func(f *fakeNodeIPC, m node.Message) bool {
		if m.Type == "submit_user_turn" && submits.Add(1) == 1 {
			var command struct {
				ID string `json:"command_id"`
			}
			if err := json.Unmarshal(m.Payload, &command); err != nil {
				return false
			}
			f.reply(m, "session_command_rejected", map[string]string{"command_id": command.ID, "reason": "mcp_update_required"})
			return true
		}
		return false
	})
	h.turnCancel.Store(true)
	h.driver.opts.ReadMCPTarget = func(_ context.Context, _ execution.Scope, _ string, _ *execution.MCPSelection, known int64) (*execution.MCPTarget, error) {
		if known == -1 {
			return &execution.MCPTarget{Configured: *ipcMCP()}, nil
		}
		return nil, nil // the refresh has nothing left to deliver: the turn is sent again
	}
	ctx := ipcContext(t)
	session := h.session("resubmitted", "workspace")
	session.MCP = ipcMCP()
	run := func(id string) <-chan ipcRunOutcome {
		done := make(chan ipcRunOutcome, 1)
		go func() {
			turn := execution.Turn{Ref: execution.Ref{SessionID: session.ID, TurnID: id}, Prompt: execution.TextPrompt(id), MCPGeneration: new(int64)}
			result, err := h.driver.Run(ctx, h.scope, session, turn, discardIPCEvent)
			done <- ipcRunOutcome{result, err}
		}()
		return done
	}
	first := run("first")
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "first", "ready")
	if got := receiveIPC(t, first); got.err != nil {
		t.Fatal(got.err)
	}
	done := run("append")
	nextIPCCommand(t, f, "submit_user_turn")           // refused
	resent := nextIPCCommand(t, f, "submit_user_turn") // sent again after the refresh
	stopTurn(t, h, session, "append")
	cancel := nextCancel(t, f, start, "append")
	f.finishTurnAs(t, resent, "append", "partial", "cancelled")
	f.settleCancel(start, cancel, "stopped")
	if got := receiveIPC(t, done); got.err != nil || !got.result.SessionKept || got.result.NotDelivered || got.result.Answer != "partial" {
		t.Fatalf("stop of a resubmitted turn = %+v, want Node's stopped turn", got)
	}
}

// A stop asked for before the turn began (the execution service admitted it, the driver had not
// begun it) is handed to the turn when it begins: the turn is cancelled, the session kept.
func TestDriverIPCStopBeforeTheTurnBeginsReachesIt(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	first := startIPCRun(h, ctx, session, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "first", "done")
	if got := receiveIPC(t, first); got.err != nil {
		t.Fatal(got.err)
	}
	stopTurn(t, h, session, "second")
	done := startIPCRun(h, ctx, session, "second", discardIPCEvent)
	nextIPCCommand(t, f, "submit_user_turn")
	f.settleCancel(start, nextCancel(t, f, start, "second"), "not_started")
	if got := receiveIPC(t, done); got.err != nil || got.result.Status != execution.StatusCancelled || !got.result.SessionKept {
		t.Fatalf("turn stopped before it began = %+v", got)
	}
}

// While a session is still being prepared nothing was started that a stop could keep: StopTurn
// refuses, so the caller ends the session (owner decision: a stop during preparation ends it).
func TestDriverIPCStopTurnRefusesWhilePreparing(t *testing.T) {
	clone := make(chan node.Message, 1)
	h := newIPCHarness(t, func(_ *fakeNodeIPC, m node.Message) bool {
		if m.Type == "clone_repository" {
			clone <- m // held: the checkout is still being prepared
			return true
		}
		return false
	})
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	done := startIPCRun(h, ctx, session, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	m := receiveIPC(t, clone)
	if h.driver.StopTurn(execution.Ref{SessionID: session.ID, TurnID: "first"}) {
		t.Fatal("StopTurn took a stop while the session was still being prepared")
	}
	f.reply(m, "clone_result", map[string]any{"kind": "clone_ready", "result": map[string]string{"path": filepath.Join(f.project.root, "w", "r", m.ExecutionID), "commit": f.project.configuration.Commit}})
	f.finishTurn(t, nextIPCCommand(t, f, "start_agent_session"), "first", "done")
	if got := receiveIPC(t, done); got.err != nil {
		t.Fatal(got.err)
	}
}

// desktop#793's merge settles a cancel of an accepted turn whose MCP wait timed out first as
// already_ended (it used to say unknown). The turn's own failure, which Node writes before the
// settlement, is its result; the late settlement changes nothing and the session takes the next turn.
func TestDriverIPCStopOfATurnWhoseMCPWaitTimedOutKeepsItsFailure(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.turnCancel.Store(true)
	ctx := ipcContext(t)
	session := h.session("session", "workspace")
	first := startIPCRun(h, ctx, session, "first", discardIPCEvent)
	f := receiveIPC(t, h.created)
	start := nextIPCCommand(t, f, "start_agent_session")
	f.finishTurn(t, start, "first", "done")
	if got := receiveIPC(t, first); got.err != nil {
		t.Fatal(got.err)
	}
	done := startIPCRun(h, ctx, session, "second", discardIPCEvent)
	submit := nextIPCCommand(t, f, "submit_user_turn")
	var submission struct {
		CommandID string `json:"command_id"`
	}
	if err := json.Unmarshal(submit.Payload, &submission); err != nil {
		t.Fatal(err)
	}
	stopTurn(t, h, session, "second")
	cancel := nextCancel(t, f, start, "second")
	f.seq[start.ExecutionID]++
	sequence := f.seq[start.ExecutionID]
	failure, _ := json.Marshal(map[string]string{"turn_id": "second", "command_id": submission.CommandID, "code": "mcp_refresh_wait_timeout"})
	f.in <- node.Message{Type: "session_turn_failed", Version: 1, OperationID: start.OperationID, ExecutionID: start.ExecutionID, Sequence: &sequence, Payload: failure}
	f.settleCancel(start, cancel, "already_ended")
	got := receiveIPC(t, done)
	if got.err != nil || got.result.Status != execution.StatusFailed || got.result.FailureCode != "mcp_refresh_wait_timeout" || got.result.AgentLost || got.result.SessionKept {
		t.Fatalf("stopped turn whose MCP wait timed out = %+v", got)
	}
	third := startIPCRun(h, ctx, session, "third", discardIPCEvent)
	f.finishTurn(t, nextIPCCommand(t, f, "submit_user_turn"), "third", "next")
	if got := receiveIPC(t, third); got.err != nil || got.result.Status != execution.StatusCompleted {
		t.Fatalf("turn after it = %+v", got)
	}
	if sent := sentIPC(f); slices.Contains(sent, "end_session") {
		t.Fatalf("the session was ended: %v", sent)
	}
}
