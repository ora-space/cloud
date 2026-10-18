package integration

// Phase 5 Batch 1 acceptance: the authoritative takeover of a session execution's terminal Node
// event, and the three transitions that commit with it (Thread D4's last row, Thread D3's
// `discarded`, IssueRun D3's `delivering`).
//
// The chain under test is the production one, end to end. The terminal event arrives over the real
// gRPC control surface (`ExecutionService.TakeOverNodeEvent` → the `agent_session_takeover` action),
// because that — not the Thread batch RPC — is where approved controller-integration D2 routes an
// execution's terminal fact. The scene is driven through the same production steps Phase 4B/4C use:
// the session declaration, the Controller claim/registration, the public Thread POST and the Thread
// event takeover that delivers a turn. The subscriber side is the real SSE endpoint.
//
// What is asserted, and why it is one assertion rather than several: receipt, durable result, Thread
// `ended`, the queued turns' `discarded`, the run's `delivering` transition, the released delivery
// work item and the sequence advance all live in ONE transaction. Each case therefore asserts
// the whole group — a commit that produced only part of it is the failure mode these tests exist to
// catch, and a per-field test would pass on a partial commit.
//
// What the tests are careful *not* to claim: nothing here asserts anything about DeliverySettled,
// the Revision row, `releasing`, `done` or multi-instance delivery. Those are Phase 5 Batch 2 and
// stay unimplemented here.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/wanglongan587/cloud/internal/controlpb"
	"github.com/wanglongan587/cloud/internal/core"
)

// sessionEndEvent renders the canonical bytes a Controller forwards for one terminal session event.
// The A layer stores them verbatim as the receipt and never parses them — the terminal fact travels
// in the result, and the event's whole job is to make "same sequence, different bytes" detectable —
// so the test's only obligation is determinism: the same event must replay byte-identically, and a
// different one at the same sequence must be a conflict.
func sessionEndEvent(reason controlpb.AgentSessionEndReason) []byte {
	return []byte("agent_session_ended/" + reason.String())
}

// sessionEndResult renders the one terminal result the session path accepts, addressed to the
// scene's own Node so the A layer's node-identity check is satisfied the way a real Controller's
// would be.
func sessionEndResult(scene liveThreadScene, reason controlpb.AgentSessionEndReason, detail string) *controlpb.ExecutionResult {
	res := &controlpb.ExecutionResult{
		Node:    &controlpb.NodeIdentity{NodeId: scene.nodeID, NodeIncarnationId: "inc-" + scene.nodeID[:8]},
		Outcome: &controlpb.ExecutionResult_AgentSessionEnded{AgentSessionEnded: &controlpb.AgentSessionEnded{Reason: reason}},
	}
	if detail != "" {
		res.GetAgentSessionEnded().Detail = &detail
	}
	return res
}

// sessionEnd submits one terminal session event over the real gRPC surface as the lease holder the
// scene's seed installed. It returns the raw error so a case can assert the Fault contract instead
// of dying on the first refusal.
func (f *fixture) sessionEnd(scene liveThreadScene, submission string, sequence uint64, reason controlpb.AgentSessionEndReason, detail, event string) (*controlpb.TakeOverNodeEventResponse, error) {
	if event == "" {
		event = string(sessionEndEvent(reason))
	}
	return f.executions.TakeOverNodeEvent(asController("ctrl-a"), &controlpb.TakeOverNodeEventRequest{
		SubmissionId: submission, Epoch: 1, OperationId: scene.runID, ExecutionId: scene.executionID,
		Sequence: sequence, Result: sessionEndResult(scene, reason, detail), Event: []byte(event),
	})
}

// sessionEndOK is sessionEnd for the cases where acceptance is the point.
func (f *fixture) sessionEndOK(t *testing.T, scene liveThreadScene, submission string, sequence uint64, reason controlpb.AgentSessionEndReason) {
	t.Helper()
	if _, e := f.sessionEnd(scene, submission, sequence, reason, "", ""); e != nil {
		t.Fatalf("the session terminal takeover must be accepted: %v", e)
	}
}

// runningThread takes over the session's first Node record, which is the production authority that
// moves the run `starting/dispatched` → `running/running` and the Thread `pending` → `active`, and
// returns the Node sequence it consumed. A terminal event may only follow it (controller-
// integration D2), so every case that starts from a live session starts here.
func (f *fixture) runningThread(t *testing.T, scene liveThreadScene) int64 {
	t.Helper()
	if _, e := f.takeover(t, scene, "", threadLineObjectWith(t, 1, "", "update", "working on the fix")); e != nil {
		t.Fatalf("the session's first Node record must be taken over: %v", e)
	}
	return 1
}

// threadRows reads the run's whole Thread as durable rows, so a case can compare the log before and
// after a transition instead of only the columns it happened to think of. A regression that rewrote
// an unrelated record, reallocated a seq or flipped a status nobody asked about shows up here.
func (f *fixture) threadRows(runID string) []core.Object {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`
		SELECT seq, source, kind, COALESCE(status,'') AS status, COALESCE(turn_id::text,'') AS turn_id, record::text AS record
		FROM thread_entries WHERE run_id=$1 ORDER BY seq`, runID)
	must(f.t, e)
	defer rows.Close()
	var out []core.Object
	for rows.Next() {
		var seq int64
		var source, kind, status, turnID, record string
		must(f.t, rows.Scan(&seq, &source, &kind, &status, &turnID, &record))
		out = append(out, core.Object{"seq": seq, "source": source, "kind": kind, "status": status, "turnId": turnID, "record": record})
	}
	must(f.t, rows.Err())
	return out
}

// threadIdleSince reads the Thread's recorded idle instant, which `ended` must clear.
func (f *fixture) threadIdleSince(runID string) *time.Time {
	f.t.Helper()
	var idle *time.Time
	must(f.t, f.store.Pool.QueryRow(`SELECT idle_since FROM issue_runs WHERE id=$1`, runID).Scan(&idle))
	return idle
}

// nodeSequence reads the execution's durable sequence high-water mark.
func (f *fixture) nodeSequence(executionID string) int64 {
	f.t.Helper()
	var n int64
	must(f.t, f.store.Pool.QueryRow(`SELECT last_event_sequence FROM node_executions WHERE execution_id=$1`, executionID).Scan(&n))
	return n
}

// receipts lists the sequences of the execution's recorded Node event receipts, oldest first: the
// durable set a rollback must not have added to.
func (f *fixture) receipts(executionID string) []int64 {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`SELECT sequence FROM node_event_receipts WHERE execution_id=$1 ORDER BY sequence`, executionID)
	must(f.t, e)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		must(f.t, rows.Scan(&n))
		out = append(out, n)
	}
	must(f.t, rows.Err())
	return out
}

// deliveryWork lists the run's released Revision delivery work items. Phase 5 Batch 1 releases
// exactly one per terminal takeover; whether the delivery then executes is Batch 2's business, so
// `registered` (a Controller has claimed and dispatched it) is expected to stay false here.
func (f *fixture) deliveryWork(runID string) []core.Object {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`
		SELECT id, kind, input::text AS input, (execution_id IS NOT NULL) AS registered
		FROM execution_work WHERE run_id=$1 AND kind='deliver_revision' ORDER BY created_at, id`, runID)
	must(f.t, e)
	defer rows.Close()
	var out []core.Object
	for rows.Next() {
		var id, kind, input string
		var registered bool
		must(f.t, rows.Scan(&id, &kind, &input, &registered))
		out = append(out, core.Object{"id": id, "kind": kind, "input": input, "registered": registered})
	}
	must(f.t, rows.Err())
	return out
}

// runState reads the run's (phase, status, thread_state) triple with the NULL Thread state spelled
// as the empty string, so a case can compare a run that has no Thread at all — which
// f.threadRunState cannot express, because it scans the column directly.
func (f *fixture) runState(runID string) (phase, status, threadState string) {
	f.t.Helper()
	must(f.t, f.store.Pool.QueryRow(`SELECT phase, status, COALESCE(thread_state,'') FROM issue_runs WHERE id=$1`, runID).Scan(&phase, &status, &threadState))
	return phase, status, threadState
}

// drainSpace collects everything the last commit published, without waiting: Store.transact releases
// a commit's hints before the mutating call returns, so a commit's complete notice set is already in
// the channel once that call has returned. It is how a case states "this commit published exactly
// these notices" rather than "published at least this one" — a spurious extra notice is precisely the
// failure a replay or rollback case is about.
func drainSpace(hints <-chan core.SpaceEvent) []core.SpaceEvent {
	var out []core.SpaceEvent
	for {
		select {
		case ev := <-hints:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// The terminal takeover ends the Thread, discards exactly the queued
// turns, leaves delivered turns and the rest of the log untouched, releases the delivery work item,
// and publishes one `thread_changed` with no `thread_appended` and no new seq.
func TestSessionEndedEndsThreadDiscardsQueuedAndReleasesDelivery(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	stream := f.openThreadStream(t, scene.tenantID, scene.spaceID)
	defer stream.close()

	// (1) The session declaration writes the immutable seq=1 prompt.
	scene.start(t, f)
	stream.nextCommit(t, threadAppendedType, threadChangedType)
	if phase, status, state := f.threadRunState(scene.runID); phase != "starting" || status != "dispatched" || state != "pending" {
		t.Fatalf("a declared session must be starting/dispatched/pending, got %s/%s/%s", phase, status, state)
	}

	// (2) The first Node record is the running authority and appends seq=2.
	f.runningThread(t, scene)
	stream.nextCommit(t, threadAppendedType, threadChangedType)

	// (3) A user turn, then the Node's echo of it: the echo appends nothing and is the only thing
	// that may move `queued → delivered`, so turn A ends this scene delivered.
	turnA := f.postThread(scene.threadScene, "p5-turn-a", threadBody("first ask"), 201, "").O("resource").S("turnId")
	stream.nextCommit(t, threadAppendedType, threadChangedType)
	if _, e := f.takeover(t, scene, "", threadLineObjectWith(t, 2, turnA, "update", "on it")); e != nil {
		t.Fatalf("the echo of turn A must be taken over: %v", e)
	}
	stream.nextCommit(t, threadChangedType)

	// (4) A second user turn that the session never executed: it stays `queued` until the terminal
	// takeover, which is the only place D3 marks such a turn `discarded`.
	turnB := f.postThread(scene.threadScene, "p5-turn-b", threadBody("second ask"), 201, "").O("resource").S("turnId")
	stream.nextCommit(t, threadAppendedType, threadChangedType)

	before := f.threadRows(scene.runID)
	beforeRead := threadItems(t, f.getThread(scene.threadScene, "", 200))
	if len(before) != 4 {
		t.Fatalf("the scene must hold 4 entries before the terminal event, got %d", len(before))
	}
	if got := f.nodeSequence(scene.executionID); got != 2 {
		t.Fatalf("the Node sequence must be at 2 before the terminal event, got %d", got)
	}
	if got := len(f.deliveryWork(scene.runID)); got != 0 {
		t.Fatalf("no delivery work may exist before the session ends, got %d", got)
	}

	// (5) The terminal event. Its notices are read from the hub rather than the SSE stream so the
	// *complete* set this commit published can be asserted, not just its first frame.
	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	f.sessionEndOK(t, scene, "", 3, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)

	published := drainSpace(hints)
	if len(published) != 1 || published[0].Type != threadChangedType {
		t.Fatalf("the terminal commit must publish exactly one %s and nothing else, got %v", threadChangedType, published)
	}
	if published[0].RunID != scene.runID || published[0].IssueID != scene.issueID || published[0].SpaceID != scene.spaceID {
		t.Fatalf("the notice must carry the run's own identity, got %+v", published[0])
	}
	// lastSeq is the Thread's high-water mark: the terminal commit allocated no seq, so it reports the
	// same max(seq) the previous commit did.
	if published[0].LastSeq != 4 {
		t.Fatalf("the notice's lastSeq must be the Thread high-water mark 4, got %d", published[0].LastSeq)
	}
	// No append, so the SSE stream sees the single change and then nothing.
	stream.nextCommit(t, threadChangedType)

	phase, status, threadState := f.threadRunState(scene.runID)
	if threadState != "ended" {
		t.Fatalf("the Thread must be ended, got %q", threadState)
	}
	// IssueRun D3's `delivering` row: entry condition "会话执行有终态结果", status `running`. This is
	// the scope the approved ADR forces into the same transaction as the Thread terminal state.
	if phase != "delivering" || status != "running" {
		t.Fatalf("the run must be delivering/running, got %s/%s", phase, status)
	}

	// The log itself: identical rows, except the one status D3 authorizes.
	after := f.threadRows(scene.runID)
	if len(after) != len(before) {
		t.Fatalf("a terminal takeover appends no entry: %d entries became %d", len(before), len(after))
	}
	for i := range before {
		want := before[i]
		if before[i].S("turnId") == turnB {
			want = core.Object{"seq": before[i].N("seq"), "source": "user", "kind": before[i].S("kind"), "status": "discarded", "turnId": turnB, "record": before[i].S("record")}
			if got := after[i].S("status"); got != "discarded" {
				t.Fatalf("the queued turn must be discarded, got %q", got)
			}
		}
		if fmt.Sprint(after[i]) != fmt.Sprint(want) {
			t.Fatalf("entry %d must be unchanged, got %v want %v", i, after[i], want)
		}
	}
	// The delivered turn is exactly that: an echo, a terminal event and a session end never move it.
	if got := f.turnStatus(scene.runID, turnA); got != "delivered" {
		t.Fatalf("a delivered turn must not regress, got %q", got)
	}
	if got := f.scalar(`SELECT COALESCE(MAX(seq),0) FROM thread_entries WHERE run_id=$1`, scene.runID); got != 4 {
		t.Fatalf("the terminal takeover must allocate no seq, got max(seq)=%d", got)
	}

	// The REST representation moved with the durable one: re-reading the same window through the
	// public GET shows the discarded turn's terminal status, and every other item — content included —
	// byte-identical. This is what makes the commit publish `thread_changed` and, since no entry was
	// appended, no `thread_appended`.
	afterRead := threadItems(t, f.getThread(scene.threadScene, "", 200))
	if len(afterRead) != len(beforeRead) {
		t.Fatalf("the terminal takeover must not add an item to the Thread window: %d became %d", len(beforeRead), len(afterRead))
	}
	for i := range beforeRead {
		// A copy, not the item itself: the expected value differs from the observed one in exactly the
		// field this transition may change, and every other field must still match byte for byte.
		want := core.Object{}
		for k, v := range beforeRead[i] {
			want[k] = v
		}
		if beforeRead[i].S("turnId") == turnB {
			want["status"] = "discarded"
			if got := afterRead[i].S("status"); got != "discarded" {
				t.Fatalf("the discarded turn must be read back as discarded, got %q", got)
			}
		}
		if fmt.Sprint(afterRead[i]) != fmt.Sprint(want) {
			t.Fatalf("item %d must be unchanged by the terminal takeover, got %v want %v", i, afterRead[i], want)
		}
	}

	// The receipt and the sequence advance, the durable basis for the Controller's EventAck.
	if got := f.nodeSequence(scene.executionID); got != 3 {
		t.Fatalf("the Node sequence must advance to 3, got %d", got)
	}
	if got := f.receipts(scene.executionID); len(got) != 3 || got[2] != 3 {
		t.Fatalf("the terminal event must be receipted, got receipts %v", got)
	}

	// The delivery work item released in the same transaction (IssueRun D3, controller-integration D6).
	work := f.deliveryWork(scene.runID)
	if len(work) != 1 {
		t.Fatalf("the terminal takeover must release exactly one delivery work item, got %d", len(work))
	}
	if work[0].B("registered") {
		t.Fatalf("the released work item must not be registered yet, got %v", work[0])
	}

	// Once the terminal transaction wins, the Thread is permanently closed — a later turn can
	// never be accepted into `ended`.
	late := f.postThread(scene.threadScene, "p5-late", threadBody("too late"), 409, "thread_closed")
	if late.S("code") != "thread_closed" {
		t.Fatalf("a POST after the session ended must be refused as closed, got %v", late)
	}
	if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user' AND status='queued'`, scene.runID); n != 0 {
		t.Fatalf("no user turn may remain queued after the terminal takeover, got %d", n)
	}
}

// The same terminal event, delivered twice, is one transition. Both replay shapes a
// Controller can produce are covered: a retry under the same submission identity (answered from the
// recorded response) and a bare re-send of the same bytes (answered from the receipt). Neither
// writes a second receipt, moves the lifecycle again or publishes a second notice.
func TestSessionEndedReplayIsANoOp(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.runningThread(t, scene)
	turnB := f.postThread(scene.threadScene, "p5-replay-turn", threadBody("unexecuted"), 201, "").O("resource").S("turnId")

	f.sessionEndOK(t, scene, "p5-terminal", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	committed := f.threadRows(scene.runID)
	receipts := f.receipts(scene.executionID)
	work := f.deliveryWork(scene.runID)

	// (a) Same submission identity: the recorded response is replayed and nothing runs.
	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	if _, e := f.sessionEnd(scene, "p5-terminal", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", ""); e != nil {
		t.Fatalf("a retry under the original submission identity must replay, not fail: %v", e)
	}
	if got := drainSpace(hints); len(got) != 0 {
		t.Fatalf("a submission replay must publish nothing, got %v", got)
	}

	// (b) No submission identity, same sequence and bytes: the receipt makes it a deterministic no-op.
	hints2, cancel2 := f.watchSpace(scene.spaceID)
	defer cancel2()
	if _, e := f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", ""); e != nil {
		t.Fatalf("a byte-identical replay must be a no-op, not a fault: %v", e)
	}
	if got := drainSpace(hints2); len(got) != 0 {
		t.Fatalf("a receipt replay must publish nothing, got %v", got)
	}

	phase, status, threadState := f.threadRunState(scene.runID)
	if phase != "delivering" || status != "running" || threadState != "ended" {
		t.Fatalf("a replay must not move the lifecycle, got %s/%s/%s", phase, status, threadState)
	}
	if got := fmt.Sprint(f.threadRows(scene.runID)); got != fmt.Sprint(committed) {
		t.Fatalf("a replay must not touch the Thread log:\n got %s\nwant %s", got, fmt.Sprint(committed))
	}
	if got := fmt.Sprint(f.receipts(scene.executionID)); got != fmt.Sprint(receipts) {
		t.Fatalf("a replay must not write a receipt, got %v want %v", got, receipts)
	}
	if got := fmt.Sprint(f.deliveryWork(scene.runID)); got != fmt.Sprint(work) {
		t.Fatalf("a replay must not release a second delivery work item, got %v want %v", got, work)
	}
	if got := f.turnStatus(scene.runID, turnB); got != "discarded" {
		t.Fatalf("a replay must not regress a discarded turn, got %q", got)
	}
	if got := f.nodeSequence(scene.executionID); got != 2 {
		t.Fatalf("a replay must not advance the sequence, got %d", got)
	}
}

// A payload that disagrees with the recorded receipt is a conflict, and the recorded
// receipt stays exactly as it was. The gap, the unknown execution and the malformed result are
// asserted alongside it because they are the same class of refusal: the event is not taken over and
// the Thread, the lifecycle and the sequence are left alone.
func TestSessionEndedRefusalsLeaveNoTrace(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.runningThread(t, scene)
	turn := f.postThread(scene.threadScene, "p5-refuse-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")

	// A gap: the terminal event may only follow every Thread event before it (D2).
	expectStatus(t, errOnly(f.sessionEnd(scene, "", 3, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// An execution Cloud never registered.
	unregistered := scene
	unregistered.executionID = "exec-never-dispatched"
	expectStatus(t, errOnly(f.sessionEnd(unregistered, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")), codes.NotFound, controlpb.ErrorCode_ERROR_CODE_NOT_FOUND)

	// The commit itself, then the two payload conflicts against its recorded receipt.
	f.sessionEndOK(t, scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	receipts := f.receipts(scene.executionID)
	rows := f.threadRows(scene.runID)
	work := f.deliveryWork(scene.runID)

	// Same sequence, different bytes.
	expectStatus(t, errOnly(f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "different-bytes")), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	// Same sequence, same bytes, different result.
	expectStatus(t, errOnly(f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_CANCELLED, "", "")), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	// The rejection refused the event, not the run: nothing the commit wrote moved.
	if got := fmt.Sprint(f.receipts(scene.executionID)); got != fmt.Sprint(receipts) {
		t.Fatalf("a refused event must not rewrite the receipt record: got %v want %v", got, receipts)
	}
	if got := fmt.Sprint(f.threadRows(scene.runID)); got != fmt.Sprint(rows) {
		t.Fatalf("a refused event must not touch the Thread log, got %s", got)
	}
	if got := fmt.Sprint(f.deliveryWork(scene.runID)); got != fmt.Sprint(work) {
		t.Fatalf("a refused event must not release delivery work, got %v", got)
	}
	if got := f.turnStatus(scene.runID, turn); got != "discarded" {
		t.Fatalf("a refused event must not change a turn's status, got %q", got)
	}
	if phase, status, threadState := f.threadRunState(scene.runID); phase != "delivering" || status != "running" || threadState != "ended" {
		t.Fatalf("a refused event must not move the lifecycle, got %s/%s/%s", phase, status, threadState)
	}
}

// errOnly drops the response so an error can be asserted inline. A nil response with a nil error is
// impossible here: every call under test either faults or returns a record.
func errOnly(_ *controlpb.TakeOverNodeEventResponse, err error) error { return err }

// A business-hook failure after the receipt is written rolls the whole takeover back:
// no receipt, no durable terminal result, no `ended`, no `discarded`, no `delivering`, no released
// delivery work, no advanced sequence. The failure is forced the way a real invariant violation
// would arrive — the run Workspace the delivery input must be read from is gone — so the test
// exercises the actual after-receipt failure path rather than a stubbed hook.
func TestSessionEndedHookFailureRollsBackTheWholeTakeover(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.runningThread(t, scene)
	turn := f.postThread(scene.threadScene, "p5-rollback-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")

	var runWorkspace string
	must(t, f.store.Pool.QueryRow(`SELECT workspace_id FROM issue_runs WHERE id=$1`, scene.runID).Scan(&runWorkspace))
	// Soft-deleting the Workspace is what makes the delivery input unreadable. Admission closes with
	// it because that is what deletion means — the schema forbids a deleted Workspace with admission
	// open, exactly as deleteWorkspace writes both in one statement.
	_, e := f.store.Pool.Exec(`UPDATE workspaces SET deleted_at=now(), admission_open=false WHERE id=$1`, runWorkspace)
	must(t, e)

	rows := f.threadRows(scene.runID)
	receipts := f.receipts(scene.executionID)
	phase, status, state := f.threadRunState(scene.runID)

	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	// The seam classifies a hook failure as the business-precondition conflict it is. The obligation
	// under test is the rollback, not the code: the event is not acknowledged, so the Node's durable
	// ledger keeps it and replays it.
	expectStatus(t, errOnly(f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)
	if got := drainSpace(hints); len(got) != 0 {
		t.Fatalf("a rolled-back terminal takeover must publish nothing, got %v", got)
	}

	if got := f.receipts(scene.executionID); len(got) != len(receipts) {
		t.Fatalf("the receipt must roll back with the hook: got %v want %v", got, receipts)
	}
	if got := f.nodeSequence(scene.executionID); got != 1 {
		t.Fatalf("the sequence must not advance past the rolled-back event, got %d", got)
	}
	if got := fmt.Sprint(f.threadRows(scene.runID)); got != fmt.Sprint(rows) {
		t.Fatalf("the Thread log must roll back whole, got %s want %s", got, fmt.Sprint(rows))
	}
	if got := f.turnStatus(scene.runID, turn); got != "queued" {
		t.Fatalf("a rolled-back takeover must leave the turn queued, got %q", got)
	}
	if got := len(f.deliveryWork(scene.runID)); got != 0 {
		t.Fatalf("a rolled-back takeover must release no delivery work, got %d", got)
	}
	if p2, s2, st2 := f.threadRunState(scene.runID); p2 != phase || s2 != status || st2 != state {
		t.Fatalf("a rolled-back takeover must not move the lifecycle: %s/%s/%s became %s/%s/%s", phase, status, state, p2, s2, st2)
	}
}

// The precondition matrix. The approved row is unconditional (Thread D4's last row is
// stated for the session execution's terminal event, and IssueRun D3's `delivering` entry condition
// is "any end reason"), so every live Thread state reaches `ended`; the states the ADR does not
// reach — an already-terminal Thread and a run that is not in a session phase at all — contradict
// the authoritative row, so the seam refuses the call as a conflict and the whole takeover rolls
// back rather than being tolerated.
func TestSessionEndedPreconditionMatrix(t *testing.T) {
	live := []struct {
		name string
		// state is the Thread state the drive must have produced, read back from the durable row
		// rather than assumed, so a case cannot silently test a different state than it names.
		state string
		// recordedIdle marks the one row whose Thread carries a recorded idle instant, so the
		// assertion that the terminal transition clears it observes a value instead of comparing two
		// NULLs.
		recordedIdle bool
		// drive reaches the state and returns the Node sequence the terminal event must follow plus
		// the turn that is `queued` when it arrives ("" when the state cannot hold one). A state
		// holds a queued turn only where production can produce that pair: writing a turn moves the
		// Thread to `active` (D4), and a queued turn blocks `idle` by definition (D4's idle row), so
		// `pending` and `idle` are states the terminal event can find with nothing left to discard.
		drive func(t *testing.T, f *fixture, scene liveThreadScene) (int64, string)
	}{
		{
			// The session is declared and no Node record has been taken over yet: D4's `pending`.
			name: "pending", state: "pending",
			drive: func(t *testing.T, f *fixture, scene liveThreadScene) (int64, string) { return 0, "" },
		},
		{
			// A Node record was taken over: D4's `active`, the state a queued turn is written into.
			name: "active", state: "active",
			drive: func(t *testing.T, f *fixture, scene liveThreadScene) (int64, string) {
				sequence := f.runningThread(t, scene)
				turn := f.postThread(scene.threadScene, "p5-matrix-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")
				return sequence, turn
			},
		},
		{
			// The session reported `turnEnded` with no queued turn: D4's `idle`, the one Thread state
			// that records an idle instant. The turn is delivered first, so the Thread can reach it.
			name: "idle", state: "idle", recordedIdle: true,
			drive: func(t *testing.T, f *fixture, scene liveThreadScene) (int64, string) {
				f.runningThread(t, scene)
				turn := f.postThread(scene.threadScene, "p5-matrix-turn", threadBody("executed"), 201, "").O("resource").S("turnId")
				if _, e := f.takeover(t, scene, "",
					threadLineObjectWith(t, 2, turn, "update", "on it"),
					threadLineObjectWith(t, 3, "", "turnEnded", "")); e != nil {
					t.Fatalf("the echo and the TurnEnded record must be taken over: %v", e)
				}
				return 3, ""
			},
		},
		{
			// The user asked the session to stop: D4's `ending`, reached through the public endpoint.
			// The turn is accepted while the Thread still accepts — `ending` is a request to the
			// session, not a statement that the turn will never run — which is exactly the pair D3's
			// discard rule exists for.
			name: "ending", state: "ending",
			drive: func(t *testing.T, f *fixture, scene liveThreadScene) (int64, string) {
				sequence := f.runningThread(t, scene)
				turn := f.postThread(scene.threadScene, "p5-matrix-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")
				f.endThread(scene.threadScene, "p5-end", 202, "")
				return sequence, turn
			},
		},
	}
	for _, c := range live {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedLiveThreadScene(t, f)
			scene.start(t, f)
			sequence, turn := c.drive(t, f, scene)
			if got, _, _ := f.threadStateOf(scene.runID); got != c.state {
				t.Fatalf("the scene must be in %q, got %q", c.state, got)
			}
			if got := f.threadIdleSince(scene.runID); (got != nil) != c.recordedIdle {
				t.Fatalf("the %s Thread's recorded idle instant = %v, want recorded=%v", c.state, got, c.recordedIdle)
			}
			if turn != "" && f.turnStatus(scene.runID, turn) != "queued" {
				t.Fatalf("the %s scene must hold a queued turn before the terminal event, got %q", c.state, f.turnStatus(scene.runID, turn))
			}

			f.sessionEndOK(t, scene, "", uint64(sequence+1), controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)

			phase, status, state := f.threadRunState(scene.runID)
			if state != "ended" || phase != "delivering" || status != "running" {
				t.Fatalf("the terminal event must end the Thread and start delivering, got %s/%s/%s", phase, status, state)
			}
			if got := f.threadIdleSince(scene.runID); got != nil {
				t.Fatalf("`ended` must clear the recorded idle instant, got %v", got)
			}
			if turn != "" {
				if got := f.turnStatus(scene.runID, turn); got != "discarded" {
					t.Fatalf("the queued turn must be discarded from %s, got %q", c.state, got)
				}
			}
			if got := len(f.deliveryWork(scene.runID)); got != 1 {
				t.Fatalf("the delivery work must be released from %s, got %d", c.state, got)
			}
		})
	}

	refused := []struct {
		name, phase string
		// state is the Thread state the run carries when the terminal event arrives; "" stands for the
		// NULL a run outside a session has, which is what runState reports for it.
		state string
	}{
		// An ended Thread is terminal and absorbing: a fresh terminal event has nothing to advance,
		// and tolerating it would let a stale sequence re-open a closed session.
		{name: "already ended", phase: "delivering", state: "ended"},
		// A run that never reached a session phase cannot have a session to end.
		{name: "still provisioning", phase: "provisioning", state: ""},
		// A run already past the session: the delivery/teardown pipeline owns it now.
		{name: "releasing", phase: "releasing", state: "ending"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedLiveThreadScene(t, f)
			scene.start(t, f)
			f.runningThread(t, scene)
			turn := f.postThread(scene.threadScene, "p5-refused-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")
			var seeded any
			if c.state != "" {
				seeded = c.state
			}
			must(t, f.setThreadState(scene.runID, c.phase, seeded))

			rows := f.threadRows(scene.runID)
			receipts := f.receipts(scene.executionID)
			// runState, not threadRunState: a run outside a session has no Thread state at all, and the
			// comparison below must see the NULL rather than fail while reading it.
			phase, status, state := f.runState(scene.runID)

			// The refusal is a business-precondition contradiction, so the seam's classification is
			// CONFLICT: the control-plane evidence (the receipt that was written) and the business
			// transition roll back together and nothing about the event is committed. "Not committed"
			// is what carries the no-silent-loss obligation — the event is not acknowledged, so the
			// Node's durable ledger still holds it — and the Controller's approved behavior on
			// CONFLICT is to clear its own queue, not acknowledge, and wait for that replay.
			expectStatus(t, errOnly(f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

			if got := f.receipts(scene.executionID); len(got) != len(receipts) {
				t.Fatalf("a refused terminal event must not be receipted: got %v want %v", got, receipts)
			}
			if got := f.nodeSequence(scene.executionID); got != 1 {
				t.Fatalf("a refused terminal event must not advance the sequence, got %d", got)
			}
			if got := fmt.Sprint(f.threadRows(scene.runID)); got != fmt.Sprint(rows) {
				t.Fatalf("a refused terminal event must not mutate the Thread, got %s", got)
			}
			if got := f.turnStatus(scene.runID, turn); got != "queued" {
				t.Fatalf("a refused terminal event must not discard a turn, got %q", got)
			}
			if got := len(f.deliveryWork(scene.runID)); got != 0 {
				t.Fatalf("a refused terminal event must release no delivery work, got %d", got)
			}
			if p2, s2, st2 := f.runState(scene.runID); p2 != phase || s2 != status || st2 != state {
				t.Fatalf("a refused terminal event must not move the lifecycle: %s/%s/%s became %s/%s/%s", phase, status, state, p2, s2, st2)
			}
		})
	}
}

// A terminal session result addressed to a delivery execution is refused rather than projected
// onto the Thread. Delivery executions are registered by the delivery pipeline, which Batch 1 does
// not implement, so the row is seeded directly: the refusal being pinned here is the A layer's own
// kind guard, not the mechanism that creates such a row.
func TestSessionEndedRefusesADeliveryExecution(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.runningThread(t, scene)
	f.sessionEndOK(t, scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)

	work := f.deliveryWork(scene.runID)
	if len(work) != 1 {
		t.Fatalf("the scene must hold one released delivery work item, got %d", len(work))
	}
	deliveryExecution := "exec-delivery-" + scene.runID[:8]
	// The row carries exactly the shape the delivery pipeline registers — the work item it was
	// dispatched from, the run Workspace it targets and the two-part Node identity — so the refusal
	// under test is the A layer's kind guard and not a scene the production path could not produce.
	_, e := f.store.Pool.Exec(`
		INSERT INTO node_executions(execution_id, kind, operation_id, work_id, workspace_id, node_id, node_operation_id, input, dispatched_epoch)
		VALUES($1,'deliver_revision',$2,$3,(SELECT workspace_id FROM issue_runs WHERE id=$2),$4,$1,'{}',1)`,
		deliveryExecution, scene.runID, work[0].S("id"), scene.nodeID)
	must(t, e)

	delivery := scene
	delivery.executionID = deliveryExecution
	receipts := f.receipts(scene.executionID)
	expectStatus(t, errOnly(f.sessionEnd(delivery, "", 1, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")), codes.Aborted, controlpb.ErrorCode_ERROR_CODE_CONFLICT)

	if got := f.receipts(deliveryExecution); len(got) != 0 {
		t.Fatalf("a delivery execution must not be receipted as a session end, got %v", got)
	}
	if got := f.receipts(scene.executionID); fmt.Sprint(got) != fmt.Sprint(receipts) {
		t.Fatalf("the session execution's receipts must be untouched, got %v want %v", got, receipts)
	}
	if phase, status, state := f.threadRunState(scene.runID); phase != "delivering" || status != "running" || state != "ended" {
		t.Fatalf("the Thread must stay ended, got %s/%s/%s", phase, status, state)
	}
}

// `discarded` is written by exactly one transition. Ending a Thread by request leaves the
// unexecuted turn `queued`, because `ending` is a request to the session (IssueRun D6) and not a
// statement that the turn will never run; only the terminal takeover, which knows the session is
// over, may dispose of it.
func TestDiscardedAppearsOnlyOnTheSessionTerminalTakeover(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	f.runningThread(t, scene)
	turn := f.postThread(scene.threadScene, "p5-only-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")

	f.endThread(scene.threadScene, "p5-only-end", 202, "")
	if got, _, _ := f.threadStateOf(scene.runID); got != "ending" {
		t.Fatalf("the end request must leave the Thread ending, got %q", got)
	}
	if got := f.turnStatus(scene.runID, turn); got != "queued" {
		t.Fatalf("ending a Thread must not discard a turn, got %q", got)
	}
	if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND status='discarded'`, scene.runID); n != 0 {
		t.Fatalf("no turn may be discarded before the terminal takeover, got %d", n)
	}
	if len(f.endSessionReasons(scene.runID)) != 1 {
		t.Fatalf("ending the Thread must have released one end_session request, got %v", f.endSessionReasons(scene.runID))
	}

	f.sessionEndOK(t, scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED)
	if got := f.turnStatus(scene.runID, turn); got != "discarded" {
		t.Fatalf("the terminal takeover must discard the turn, got %q", got)
	}
	// The takeover is not a second end request: the request already exists and the terminal event is
	// its answer, so no further command is released.
	if got := f.endSessionReasons(scene.runID); len(got) != 1 {
		t.Fatalf("the terminal takeover must not release another end_session request, got %v", got)
	}
}

// The terminal transaction against the concurrent writers that could otherwise reopen the
// Thread. Every outcome asserted is a legal serialization, and the invariant the whole batch exists
// for is asserted on all of them: once the terminal transaction wins, no user turn is left `queued`
// and the Thread never leaves `ended`.
func TestSessionEndedSerializesWithConcurrentThreadMutations(t *testing.T) {
	t.Run("a new turn posted while the session ends", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		scene := seedLiveThreadScene(t, f)
		scene.start(t, f)
		f.runningThread(t, scene)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var postStatus int
		var postErr, endErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			postStatus, _, postErr = f.threadRequest("POST", threadMessagesPath(scene.tenantID, scene.issueID, scene.runID), threadBody("racing turn"), "p5-race-post")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, endErr = f.sessionEnd(scene, "p5-race-end", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")
		}()
		close(start)
		wg.Wait()

		must(t, postErr)
		if endErr != nil {
			t.Fatalf("the terminal takeover must win or lose legally, got %v", endErr)
		}
		switch postStatus {
		case 201:
			// The POST committed first, so its turn was queued when the session ended and D3 must have
			// disposed of it.
			if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user' AND status='queued'`, scene.runID); n != 0 {
				t.Fatalf("a turn accepted before the session ended must be discarded, %d still queued", n)
			}
			if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user' AND status='discarded'`, scene.runID); n != 1 {
				t.Fatalf("the racing turn must be discarded exactly once, got %d", n)
			}
		case 409:
			// The terminal takeover committed first, so the Thread was already closed.
			if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND source='user'`, scene.runID); n != 0 {
				t.Fatalf("a turn refused as closed must not be persisted, got %d", n)
			}
		default:
			t.Fatalf("a racing POST must be either accepted or refused as closed, got %d", postStatus)
		}
		if phase, status, state := f.threadRunState(scene.runID); phase != "delivering" || status != "running" || state != "ended" {
			t.Fatalf("the terminal takeover must be the final state, got %s/%s/%s", phase, status, state)
		}
		if n := len(f.deliveryWork(scene.runID)); n != 1 {
			t.Fatalf("exactly one delivery work item must be released, got %d", n)
		}
	})

	t.Run("the same terminal event sent twice", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		scene := seedLiveThreadScene(t, f)
		scene.start(t, f)
		f.runningThread(t, scene)
		turn := f.postThread(scene.threadScene, "p5-race-turn", threadBody("never executed"), 201, "").O("resource").S("turnId")

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		wg.Add(2)
		for i := range errs {
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.sessionEnd(scene, "", 2, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")
			}(i)
		}
		close(start)
		wg.Wait()

		// Both may succeed: one commits and the other is the byte-identical replay of a committed
		// event. Neither may fault, because a fault here would make the Node replay forever.
		for i, e := range errs {
			if e != nil {
				t.Fatalf("concurrent duplicate terminal event %d must be a no-op, not a fault: %v", i, e)
			}
		}
		if got := f.receipts(scene.executionID); len(got) != 2 {
			t.Fatalf("the duplicate must produce exactly one receipt, got %v", got)
		}
		if got := f.nodeSequence(scene.executionID); got != 2 {
			t.Fatalf("the duplicate must advance the sequence once, got %d", got)
		}
		if got := f.turnStatus(scene.runID, turn); got != "discarded" {
			t.Fatalf("the turn must be discarded once, got %q", got)
		}
		if n := len(f.deliveryWork(scene.runID)); n != 1 {
			t.Fatalf("exactly one delivery work item must be released, got %d", n)
		}
		if phase, status, state := f.threadRunState(scene.runID); phase != "delivering" || status != "running" || state != "ended" {
			t.Fatalf("the Thread must end once, got %s/%s/%s", phase, status, state)
		}
	})

	t.Run("the idle scanner ticks while the session ends", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		f.store.ThreadIdleTimeout = 15 * time.Minute
		scene := seedLiveThreadScene(t, f)
		scene.start(t, f)
		f.runningThread(t, scene)
		// The session reported the conversation ended and no turn is queued, so the Thread is idle
		// with a recorded window. There is deliberately no queued turn in this case: a queued turn
		// blocks idle (D4), so `idle` and a queued turn cannot coexist — which is exactly why the
		// scanner and the terminal event can only collide on a Thread with nothing left to discard.
		if _, e := f.takeover(t, scene, "", threadLineObjectWith(t, 2, "", "turnEnded", "")); e != nil {
			t.Fatalf("the TurnEnded record must be taken over: %v", e)
		}
		if state, idle, _ := f.threadStateOf(scene.runID); state != "idle" || idle == nil {
			t.Fatalf("the scene must be idle with a recorded window, got %q idle=%v", state, idle)
		}
		// Age the window past the configured timeout, which is the only thing the scan is about.
		_, e := f.store.Pool.Exec(`UPDATE issue_runs SET idle_since=now()-interval '1 hour' WHERE id=$1`, scene.runID)
		must(t, e)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var scanErr, endErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			scanErr = f.store.EndIdleAgentThreadsOnce(context.Background())
		}()
		go func() {
			defer wg.Done()
			<-start
			_, endErr = f.sessionEnd(scene, "", 3, controlpb.AgentSessionEndReason_AGENT_SESSION_END_REASON_USER_ENDED, "", "")
		}()
		close(start)
		wg.Wait()

		must(t, scanErr)
		if endErr != nil {
			t.Fatalf("the terminal takeover must win or lose legally, got %v", endErr)
		}
		if phase, status, state := f.threadRunState(scene.runID); phase != "delivering" || status != "running" || state != "ended" {
			t.Fatalf("the session end must be the final state, got %s/%s/%s", phase, status, state)
		}
		if got := f.threadIdleSince(scene.runID); got != nil {
			t.Fatalf("`ended` must clear the idle window, got %v", got)
		}
		// Two triggers can both fire — the idle window expiring and the session ending — but only one
		// request may reach the session, and the terminal event is the answer to whichever did.
		if got := f.endSessionReasons(scene.runID); len(got) > 1 {
			t.Fatalf("at most one end_session request may be released, got %v", got)
		}
		if n := len(f.deliveryWork(scene.runID)); n != 1 {
			t.Fatalf("exactly one delivery work item must be released, got %d", n)
		}
		if n := f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND status='discarded'`, scene.runID); n != 0 {
			t.Fatalf("no turn was queued, so none may be discarded, got %d", n)
		}
	})
}
