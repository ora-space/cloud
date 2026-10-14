package core

import "fmt"

// Thread takeover business core (Thread D1..D4, IssueRun D3/D6; plan §4B.4..§4B.8).
//
// It runs on the caller-owned *transaction of the A-side takeover, inside which the Thread entries,
// the run's `starting → running` transition and the A-side receipts must commit or roll back
// together. It never opens its own transaction and never defers a post-commit effect: a non-nil
// error aborts the whole takeover, so the Controller does not ack and the Node replays.
//
// The hook takes identity from the caller and re-reads everything else authoritatively (§14): the
// caller Object carries only the run id, and the run, the execution, the work item and the Thread's
// first entry are all re-read here, so a caller cannot assert a business fact the database does not
// hold. Matching (execution → work → run) is what proves the execution really belongs to this run
// and this session work (§14, §18): a wrong execution is rejected instead of being projected onto
// some other run's Thread.

// threadRecordKinds is the closed set of `ora-history` record type tags Thread D2 allows as a
// Thread entry `kind`. Cloud does not own the record format — desktop `ora-history` does (Thread
// D2) — and the tag is the top-level `type` discriminator of its `HistoryRecord`, a closed tagged
// enum (`#[serde(tag = "type", rename_all = "camelCase")]` in desktop
// `crates/history/src/record.rs`): meta, update, turnEnded, agentSwitched, handoffDelivered, gap.
// These six tags are therefore the "known set" D2 requires Cloud to validate against, and they are
// the only kind Cloud can derive without parsing the business fields D2 forbids it to interpret.
// An unrecognized tag is an invariant error that rolls the batch back (fail closed) rather than a
// kind Cloud invented; widening the set is a coordinated change with the format's owner.
var threadRecordKinds = map[string]bool{
	"meta":             true,
	"update":           true,
	"turnEnded":        true,
	"agentSwitched":    true,
	"handoffDelivered": true,
	"gap":              true,
}

// threadRecordKind returns the Thread entry kind for one taken-over Node record: the type tag the
// record itself carries, validated against the known set above.
func threadRecordKind(record Object) (string, bool) {
	kind := record.S("type")
	return kind, threadRecordKinds[kind]
}

// userTurnEcho reports whether a Node record is the user message a turn starts with — the only
// record that echoes a turn Cloud already wrote. Node protocol D2 tags every record that belongs to
// a user turn with that turn's id, so the agent's own replies and the turn's `turnEnded` carry it
// too; the turn id alone therefore says which turn a record belongs to, never that it is an echo.
func userTurnEcho(record Object) bool {
	return record.S("type") == "update" && record.O("update").S("sessionUpdate") == "user_message_chunk"
}

// settleThreadEvents is the B-owned core behind the A→B hook OnThreadEvents
// (controller-integration D6, IssueRun D3, Thread D1/D4; plan §4B.7).
//
//	authoritative re-read: issue_runs, node_executions and execution_work are read here, never taken
//	                      from the caller; the execution must be an agent_session execution whose
//	                      work item is this run's own session work.
//	echo dedupe:          a user message record (userTurnEcho) whose turn_id is the Thread's first
//	                      prompt turn_id is the echo of the prompt Cloud already wrote as seq=1: it
//	                      is persisted as a receipt by the caller but produces no entry and consumes
//	                      no seq (Thread D3 rule).
//	user turn echo:       a user message record whose turn_id is an existing Cloud-written user turn
//	                      takes that turn over: the row moves `queued → delivered` and nothing else
//	                      changes — no entry, no seq, no rewrite of the stored content, no new
//	                      command (Thread D3, D-4C-08).
//	turn records:         every other record of a turn — the agent's replies, its tool records, the
//	                      turn's `turnEnded` — carries the same turn_id (Node protocol D2) and is an
//	                      ordinary entry that keeps that turn_id.
//	seq allocation:       one gapless run-scoped seq per real record, from MAX(seq)+1 — seq=1 stays
//	                      the immutable Cloud-authored first prompt (D-023, G-009/§21/§23).
//	running authority:    the first real record commits `starting → running` in this same
//	                      transaction, and only when the run is still a valid starting run with a
//	                      usable workspace; nothing else about the run changes (§15, §30).
//	thread lifecycle:     after the batch commits its records, an `active` Thread whose last
//	                      effective record is `turnEnded` with no queued user turn becomes `idle`
//	                      (idle_since from the database clock), and an `idle` Thread that took over a
//	                      continuation record becomes `active` again with idle_since cleared
//	                      (Thread D4, D-4C-04/D-4C-09).
//	stale/terminal:       a run that is no longer `starting`, a cancelled run and an unusable
//	                      workspace are not advanced (G-011 fail-closed, §17/§20) but the taken-over
//	                      records are still appended to the Thread, so an acked event is never
//	                      silently dropped from the conversation.
func (s *Store) settleThreadEvents(t *transaction, runID, executionID string, events []Object) error {
	if !validID(runID) {
		return fmt.Errorf("settleThreadEvents: invalid run id %q", runID)
	}
	if executionID == "" {
		return fmt.Errorf("settleThreadEvents: run %s has no execution identity", runID)
	}

	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if o == nil {
		// No authoritative run to accept the events: an invariant violation, not a replay, so the
		// batch must not be acked (plan §4.10 "unknown run").
		return fmt.Errorf("settleThreadEvents: issue_run %s not found", runID)
	}
	if o.S("executorType") != "agent" {
		return fmt.Errorf("settleThreadEvents: run %s is executor_type=%q, not agent", runID, o.S("executorType"))
	}
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1", executionID)
	if e == nil {
		return fmt.Errorf("settleThreadEvents: execution %s is not registered", executionID)
	}
	if e.S("kind") != "agent_session" {
		return fmt.Errorf("settleThreadEvents: execution %s is kind=%q, not agent_session", executionID, e.S("kind"))
	}
	work := t.one("SELECT * FROM execution_work WHERE id=$1", e.S("workId"))
	if work == nil || work.S("runId") != runID || work.S("kind") != "agent_session" {
		// The execution belongs to a different run's work item: refusing here is what keeps a
		// mis-addressed batch from being written into another run's Thread (§18).
		return fmt.Errorf("settleThreadEvents: execution %s does not belong to run %s agent_session work", executionID, runID)
	}

	// The Thread's first entry is immutable (Phase 3A, §21) and is also the authoritative echo
	// identity: the first prompt's turn_id is the one the Node echoes back on the user turn it was
	// given (Thread D3, Node protocol D2). A run with no first prompt cannot have taken a session
	// execution over, so its absence is an invariant violation rather than a reason to allocate
	// seq=1 for a Node record.
	firstTurn := t.one("SELECT turn_id FROM thread_entries WHERE run_id=$1 AND seq=1", runID)
	if firstTurn == nil {
		return fmt.Errorf("settleThreadEvents: run %s has no first prompt entry (seq=1); refusing to allocate seq=1 to a Node record", runID)
	}
	initialTurnID := firstTurn.S("turnId")

	next := t.one("SELECT COALESCE(MAX(seq),0) AS m FROM thread_entries WHERE run_id=$1", runID).N("m") + 1
	taken := 0
	// lastKind is the kind of the batch's last *effective* Node record — the record the Thread
	// lifecycle decision below is taken on (D-4C-04/D-4C-09). A user turn the Node echoes is
	// effective even though it appends no entry: it is the Node stating it holds that turn, so a
	// batch ending on one means the agent still has work and the Thread is not idle. The first
	// prompt's echo is not effective at all — seq=1 already presents it and it says nothing about
	// what the session did afterwards.
	lastKind := ""
	lastStopReason := ""
	for _, ev := range events {
		echo := userTurnEcho(ev.O("record"))
		if turnID := ev.S("turnId"); echo && turnID != "" && turnID == initialTurnID {
			continue // echo of the first prompt: receipt only, seq=1 already presents it
		}
		kind, ok := threadRecordKind(ev.O("record"))
		if !ok {
			return fmt.Errorf("settleThreadEvents: run %s node sequence %d carries unknown record type %q", runID, ev.N("sequence"), ev.O("record").S("type"))
		}
		lastKind = kind
		lastStopReason = ev.O("record").S("stopReason")
		// A Cloud-written user turn echoed back by the Node (Thread D3, D-4C-08). The event carries
		// the turn_id of the entry Cloud wrote when the turn was accepted, so taking it over is a
		// lifecycle transition on that existing row: never a new entry, a new seq, a rewrite of the
		// original content, a new command or a re-enqueue. Only `queued → delivered` moves; a turn
		// already delivered is an identical replay and is left byte-for-byte alone, so `delivered`
		// never regresses to `queued`. The status CHECK admits exactly queued/delivered/discarded,
		// and only a session end that found the turn still queued writes `discarded` (Thread D3
		// invariant 4, Phase 5) — an echo cannot undo it, which is why the test above needs no third
		// branch and no default.
		if turnID := ev.S("turnId"); echo && turnID != "" {
			if turn := t.one("SELECT seq, status FROM thread_entries WHERE run_id=$1 AND turn_id=$2 AND source='user' AND kind='user_turn'", runID, turnID); turn != nil {
				if turn.S("status") == "queued" {
					// Same hardening as every other Thread CAS: the row was just read under the
					// caller's advisory lock, so zero affected rows means the lifecycle moved under
					// a predicate this transaction did not observe — invariant corruption that rolls
					// the batch back rather than being silently tolerated.
					if moved := t.execRows(`UPDATE thread_entries SET status='delivered' WHERE run_id=$1 AND seq=$2 AND status='queued'`, runID, turn.N("seq")); moved != 1 {
						return fmt.Errorf("settleThreadEvents: run %s user turn %s was not marked delivered (rows affected %d)", runID, turnID, moved)
					}
					// A4: the row moved, so the Thread's REST representation changed even though no
					// entry was appended. A turn already `delivered` (an identical replay) moves
					// nothing and therefore queues nothing — the hint describes a change, not an echo.
					threadChanged(t, o)
				}
				continue
			}
		}
		// Thread D1: the same (node_execution_id, node_sequence) yields at most one entry, even if
		// the A-side batch classification ever let a duplicate through. Identical content is a
		// no-op; different content under the same identity is an invariant error, never a rewrite.
		if prior := t.one("SELECT * FROM thread_entries WHERE node_execution_id=$1 AND node_sequence=$2", executionID, ev.N("sequence")); prior != nil {
			if prior.S("kind") != kind || jsonText(prior.O("record")) != jsonText(ev.O("record")) {
				return fmt.Errorf("settleThreadEvents: run %s already has a different entry for execution %s node sequence %d", runID, executionID, ev.N("sequence"))
			}
			continue
		}
		t.exec(`
			INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id, node_execution_id, node_sequence)
			VALUES($1,$2,'node',$3,$4,$5,$6,$7)`,
			runID, next, kind, jsonText(ev.O("record")), nullable(ev.S("turnId")), executionID, ev.N("sequence"))
		next++
		taken++
	}
	if taken == 0 {
		// No Node record was taken over in this batch, so the run stays exactly where it was — not
		// running, and its Thread state untouched (plan §4B.4/§31). A delivered user turn written
		// above is still committed: that echo is durable Node evidence about a turn Cloud already
		// owns, and it is not a Thread record, so it is not takeover evidence for anything else.
		return nil
	}
	// At least one Node record became a Thread entry, so a live subscriber has something new to
	// read: queue the invalidation hints here, against the authoritative run row re-read above
	// (Thread D5, T4C-25). They are queued after this batch's entry writes, so lastSeq is the batch's
	// max(seq), and the store releases them only if this whole batch commits. Both hints are queued:
	// the append-specific one keeps its meaning, and A4's generalized one covers this same commit.
	// A batch that only delivered an echoed user turn takes the early return above and publishes no
	// append hint — but it did change the read model, so the echo path queued A4's hint for it.
	threadAppended(t, o)
	threadChanged(t, o)

	// The running transition (§15/§30). The gate is re-read here rather than passed in: a run that
	// a cancel, a settlement or an earlier batch already moved must not be moved again, and an
	// unusable workspace fails closed (G-011) instead of being recorded as a terminal state.
	if o.S("phase") == "starting" && o.S("status") == "dispatched" && o.S("cancelRequestedAt") == "" && runWorkspaceLive(t, o.S("workspaceId"), runID) {
		// The CAS must move exactly the row the gate above approved. The gate reads and this write
		// share one transaction snapshot while the caller's advisory lock excludes the other
		// takeover callers, so zero affected rows means some other writer moved the run under a
		// predicate this transaction did not observe — an invariant violation, not a race to
		// tolerate. Returning an error rolls the whole batch back (§13): a receipted record must
		// never be committed without the transition it is the authority for.
		if moved := t.execRows(`
			UPDATE issue_runs
			SET phase='running', status='running', thread_state='active', version=version+1, updated_at=now()
			WHERE id=$1 AND executor_type='agent' AND phase='starting' AND status='dispatched' AND cancel_requested_at IS NULL`, runID); moved != 1 {
			return fmt.Errorf("settleThreadEvents: run %s was not moved to running (rows affected %d)", runID, moved)
		}
	}

	// Post-batch Thread lifecycle (Thread D4, D-4C-04/D-4C-08/D-4C-09). The decision is taken on the
	// batch's own outcome — re-read here, after the writes above, so it sees exactly what this
	// transaction committed to — and it only ever moves a Thread that a running run already owns.
	// A run the running gate above did not move (cancelled, unusable workspace, already past
	// 'starting') keeps whatever state it had, and `pending` stays Phase 4B's business alone.
	//
	// The idle predicate is deliberately NOT "the batch contained a TurnEnded": a TurnEnded the
	// agent followed with another record — an agent message, a tool record, another user turn — is
	// not the end of the conversation, which is why the decision reads the batch's last effective
	// record rather than scanning for a marker. A user turn still `queued` is work the Node has not
	// taken over yet, so it blocks idle for as long as it stays queued; the probe runs after the
	// delivery writes above, so a turn this very batch delivered does not block it.
	//
	// Only a *running* Thread has a lifecycle to decide here. A run still `starting` (its first
	// batch did not pass the gate above: cancelled, unusable workspace), one already `delivering`,
	// or one settled altogether keeps whatever Thread state it had and is written by nothing here —
	// which is also what keeps a stale or cancelled run's late records from reaching the lifecycle
	// at all.
	after := t.one("SELECT phase, status, thread_state FROM issue_runs WHERE id=$1", runID)
	if after.S("phase") != "running" || after.S("status") != "running" {
		return nil
	}
	threadState := after.S("threadState")
	// Cancellation also terminates a turn, but is not evidence of a successful idle session.
	// The subsequent authoritative session end decides its terminal state and safe failure code.
	ended := lastKind == "turnEnded" && lastStopReason != "cancelled"
	queued := t.one("SELECT seq FROM thread_entries WHERE run_id=$1 AND source='user' AND status='queued' LIMIT 1", runID) != nil
	switch {
	case threadState == "active" && ended && !queued:
		// idle_since is written from the database clock: the idle window is judged against the
		// database's own time on every reader, never against a process clock (D-4C-10, §21).
		if moved := t.execRows(`
			UPDATE issue_runs SET thread_state='idle', idle_since=now(), version=version+1, updated_at=now()
			WHERE id=$1 AND thread_state='active'`, runID); moved != 1 {
			return fmt.Errorf("settleThreadEvents: run %s was not moved to idle (rows affected %d)", runID, moved)
		}
		threadChanged(t, o)
	case threadState == "idle" && !ended:
		// A continuation record taken over while the Thread sat idle: the conversation is live
		// again, and idle_since is cleared with the state so a stale instant can never be read as
		// an expired idle window by the ending scan (D-4C-10).
		if moved := t.execRows(`
			UPDATE issue_runs SET thread_state='active', idle_since=NULL, version=version+1, updated_at=now()
			WHERE id=$1 AND thread_state='idle'`, runID); moved != 1 {
			return fmt.Errorf("settleThreadEvents: run %s was not moved back to active (rows affected %d)", runID, moved)
		}
		threadChanged(t, o)
	}
	return nil
}
