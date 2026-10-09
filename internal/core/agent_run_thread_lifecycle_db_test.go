package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
)

// Phase 4C S5 DB tests (T4C-19, T4C-20, T4C-28, T4C-29, T4C-30): the Thread delivery lifecycle the
// takeover owns — a Cloud-written user turn echoed by the Node, and the `active ⇄ idle` oscillation
// of Thread D4.
//
// They drive the real control action ("thread_events") through Store.Control with the production hook
// set wired exactly the way cmd/server wires it (BindBusinessHooks), so what is proven is the shipped
// path and not a double. White-box (package core) only because the A→B seam takes the unexported
// *transaction and because one test must replay the hook past the A-side receipt guard; there is no
// in-memory substitute for PostgreSQL anywhere in this file.

// seedQueuedUserTurn writes exactly what the Thread POST writes — the same columns, the same
// 'queued' lifecycle, the same `thread_state='active'` activation — directly, so an S5 test can
// state its own precondition instead of inheriting it from the POST path.
//
// The POST itself is covered end to end over real HTTP and PostgreSQL by
// integration/agent_run_thread_message_test.go; what S5 owns is what the *takeover* does with the
// row afterwards, so the row is seeded rather than posted. Seeding it here also keeps the two
// halves of "the entry exists" and "the takeover delivered it" independently observable.
func seedQueuedUserTurn(t *testing.T, store *Store, runID, text string) (string, int64) {
	t.Helper()
	turnID := newID()
	var seq int64
	if err := store.Pool.QueryRow(`
		INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id, status)
		VALUES($1, (SELECT COALESCE(MAX(seq),0)+1 FROM thread_entries WHERE run_id=$1), 'user','user_turn',$2,$3,'queued')
		RETURNING seq`,
		runID, jsonText(Object{"content": []Object{{"type": "text", "text": text}}}), turnID).Scan(&seq); err != nil {
		t.Fatalf("seed queued user turn: %v", err)
	}
	if _, err := store.Pool.Exec(`
		UPDATE issue_runs SET thread_state='active', idle_since=NULL, version=version+1, updated_at=now()
		WHERE id=$1`, runID); err != nil {
		t.Fatalf("activate the Thread for the seeded turn: %v", err)
	}
	return turnID, seq
}

// threadEntryRow captures the columns no 4C transition may rewrite (Thread D1/invariant 4: `record`
// is never written and `seq` never renumbered; the only mutable column is `status`, one way). A test
// compares the whole tuple before and after rather than asserting field by field.
func threadEntryRow(t *testing.T, store *Store, runID string, seq int64) (record Object, turnID, status, createdAt string) {
	t.Helper()
	var raw []byte
	if err := store.Pool.QueryRow(`
		SELECT record, COALESCE(turn_id::text,''), COALESCE(status,''), created_at::text
		FROM thread_entries WHERE run_id=$1 AND seq=$2`, runID, seq).Scan(&raw, &turnID, &status, &createdAt); err != nil {
		t.Fatalf("read thread entry %d: %v", seq, err)
	}
	return mustObject(t, raw), turnID, status, createdAt
}

// idleAge reads `idle_since` together with how old the *database* considers it, in one statement, so
// a test proves the instant came from this database's clock (Thread D4 invariant 5) instead of from
// whatever time.Now() a process happened to hold.
func idleAge(t *testing.T, store *Store, runID string) (sql.NullString, sql.NullFloat64) {
	t.Helper()
	var since sql.NullString
	var age sql.NullFloat64
	if err := store.Pool.QueryRow(`
		SELECT idle_since, extract(epoch FROM (now()-idle_since)) FROM issue_runs WHERE id=$1`, runID).Scan(&since, &age); err != nil {
		t.Fatalf("read idle_since: %v", err)
	}
	return since, age
}

// replayHook drives the B-owned takeover core directly, inside a real caller-owned transaction, so a
// test can replay a batch the A-side receipt guard would otherwise have deduped. That is the only way
// to observe the hook's *own* idempotency rather than the classification layer's.
//
// The batch is handed over in the wire shape and parsed exactly as the bound seam parses it
// (business_hooks.onThreadEvents): the record travels as the opaque JSON string `ora-history` owns,
// and the business core works on the object.
func replayHook(t *testing.T, store *Store, scene takeoverScene, events []Object) error {
	t.Helper()
	parsed := make([]Object, 0, len(events))
	for _, ev := range events {
		var record Object
		if err := json.Unmarshal([]byte(ev.S("record")), &record); err != nil || record == nil {
			t.Fatalf("replay hook: event %d carries a record that is not a JSON object", ev.N("sequence"))
		}
		parsed = append(parsed, Object{"sequence": ev.N("sequence"), "turnId": ev.S("turnId"), "record": record})
	}
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		if e := store.settleThreadEvents(tx, scene.run, scene.execution, parsed); e != nil {
			panic(databaseFailure{e})
		}
		return Object{}
	})
	return err
}

// TestThreadTakeoverUserTurnEchoMarksDelivered (T4C-19, D-4C-08/D-4C-13-1): a Node record carrying
// the turn_id of an existing Cloud-written user turn takes that turn over — the row becomes
// `delivered` and nothing else happens: no new entry, no new seq, no rewrite of the stored record,
// no new command, no re-enqueue.
func TestThreadTakeoverUserTurnEchoMarksDelivered(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)

	// The first real record makes it running/active, exactly as Phase 4B leaves it.
	if _, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(1, threadRecord("update", 0, "a"), "")}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	turnID, turnSeq := seedQueuedUserTurn(t, store, scene.run, "hello agent")
	beforeRecord, beforeTurn, beforeStatus, beforeCreated := threadEntryRow(t, store, scene.run, turnSeq)
	if beforeStatus != "queued" {
		t.Fatalf("a posted user turn starts queued, got %q", beforeStatus)
	}
	beforeSeqs := threadSeqs(t, store, scene.run)
	beforeCommands := countCommands(t, store, scene.run)

	out, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(2, userTurnRecord(1, "the agent sees the user turn"), turnID),
	})
	if err != nil {
		t.Fatalf("echo batch: %v", err)
	}
	// The echo is an event like any other: it is receipted and advances the execution's sequence,
	// which is what the ack is for.
	if out.N("takenOverThrough") != 2 {
		t.Fatalf("the echo still advances the execution sequence, got %d", out.N("takenOverThrough"))
	}
	if n := countReceipts(t, store, scene.execution); n != 2 {
		t.Fatalf("the echo must be receipted, got %d receipts", n)
	}

	if got := threadSeqs(t, store, scene.run); len(got) != len(beforeSeqs) {
		t.Fatalf("an echoed user turn must not add an entry: had %v, now %v", beforeSeqs, got)
	}
	if _, _, kind, _, _, present := nodeEntry(t, store, scene.run, 2); present {
		t.Fatalf("the echoed user turn must not be stored as a node entry, got kind=%q", kind)
	}
	afterRecord, afterTurn, afterStatus, afterCreated := threadEntryRow(t, store, scene.run, turnSeq)
	if afterStatus != "delivered" {
		t.Fatalf("the takeover must mark the echoed user turn delivered, got %q", afterStatus)
	}
	if jsonText(afterRecord) != jsonText(beforeRecord) || afterTurn != beforeTurn || afterCreated != beforeCreated {
		t.Fatalf("only status may change: record/turn_id/created_at must be untouched (%v %s %s)",
			afterRecord, afterTurn, afterCreated)
	}
	if afterTurn != turnID {
		t.Fatalf("the entry must keep the Cloud-generated turn_id %s, got %s", turnID, afterTurn)
	}
	if got := countCommands(t, store, scene.run); got != beforeCommands {
		t.Fatalf("taking over an echo must create no command: had %d, now %d", beforeCommands, got)
	}
	// A delivered continuation record leaves the Thread live: the batch's last record is not a turn
	// end and no turn is still queued, so nothing moves it off `active`.
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "active" {
		t.Fatalf("a mid-conversation echo must leave the Thread active, got %v", state)
	}
}

// TestThreadTakeoverUserTurnLifecycleIsOneWay (T4C-20, D-4C-06/invariant 4): a queued turn stays
// `queued` until its echo arrives, and once `delivered` it is never written again — neither by a
// replay of the same batch nor by anything else in the takeover path.
func TestThreadTakeoverUserTurnLifecycleIsOneWay(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)

	if _, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(1, threadRecord("update", 0, "a"), "")}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	turnID, turnSeq := seedQueuedUserTurn(t, store, scene.run, "still waiting")

	// An unrelated batch — other records, no echo — must leave the queued turn alone. `queued` means
	// "the Node has not taken this turn over yet", and nothing but that evidence may clear it.
	if _, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(2, threadRecord("update", 1, "b"), "")}); err != nil {
		t.Fatalf("unrelated batch: %v", err)
	}
	if _, _, status, _ := threadEntryRow(t, store, scene.run, turnSeq); status != "queued" {
		t.Fatalf("a queued turn stays queued until its own echo, got %q", status)
	}

	echo := threadEvent(3, userTurnRecord(2, "the user turn"), turnID)
	if _, err := takeOver(t, store, scene, scene.execution, []Object{echo}); err != nil {
		t.Fatalf("echo batch: %v", err)
	}
	record, turn, status, createdAt := threadEntryRow(t, store, scene.run, turnSeq)
	if status != "delivered" {
		t.Fatalf("the echo delivered the turn, got %q", status)
	}

	// The A-side classifier would dedupe a replayed batch by receipt; calling the hook directly
	// bypasses that guard, so what this asserts is the hook's own idempotency. Reading `delivered`
	// back unchanged afterwards is the observable form of "never regresses to queued": the only
	// status write on this path is the `queued → delivered` CAS.
	if err := replayHook(t, store, scene, []Object{echo}); err != nil {
		t.Fatalf("replaying the echo must be a no-op, got %v", err)
	}
	replayRecord, replayTurn, replayStatus, replayCreated := threadEntryRow(t, store, scene.run, turnSeq)
	if replayStatus != "delivered" {
		t.Fatalf("the delivered lifecycle must not be rewritten, got %q", replayStatus)
	}
	if jsonText(replayRecord) != jsonText(record) || replayTurn != turn || replayCreated != createdAt {
		t.Fatalf("a replay must leave the entry byte-for-byte identical")
	}
	if got := threadSeqs(t, store, scene.run); len(got) != 4 || got[3] != 4 {
		t.Fatalf("a replay must allocate no seq: expected 1..4, got %v", got)
	}
}

// TestThreadTakeoverIdleWhenTurnEndedAndNoQueuedTurn (T4C-28, D-4C-04/D-4C-09): a batch whose last
// effective record is `TurnEnded` and which leaves no queued user turn moves an active Thread to
// `idle` with `idle_since` from the database clock; a queued user turn blocks it.
func TestThreadTakeoverIdleWhenTurnEndedAndNoQueuedTurn(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)

	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(1, threadRecord("update", 0, "a"), ""),
		threadEvent(2, threadRecord("turnEnded", 1, ""), ""),
	}); err != nil {
		t.Fatalf("turnEnded batch: %v", err)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("the end of the agent's turn must idle the Thread, got %v", state)
	}
	since, age := idleAge(t, store, scene.run)
	if !since.Valid {
		t.Fatal("idle_since must be written with the idle state")
	}
	if !age.Valid || age.Float64 < 0 || age.Float64 > 60 {
		t.Fatalf("idle_since must be the database's own now(), got age %v", age)
	}

	// A queued user turn is work the Node has not taken over yet, so the Thread is not idle — the
	// predicate is "no queued user turn left", not "the batch ended".
	other := dispatcherDB(t)
	second := seedTakeoverScene(t, other)
	if _, err := takeOver(t, other, second, second.execution, []Object{threadEvent(1, threadRecord("update", 0, "a"), "")}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	seedQueuedUserTurn(t, other, second.run, "waiting for the node")
	if _, err := takeOver(t, other, second, second.execution, []Object{threadEvent(2, threadRecord("turnEnded", 1, ""), "")}); err != nil {
		t.Fatalf("turnEnded batch with a queued turn: %v", err)
	}
	if state := runThreadState(t, other, second.run); !state.Valid || state.String != "active" {
		t.Fatalf("a queued user turn must block idle, got %v", state)
	}
	if since, _ := idleAge(t, other, second.run); since.Valid {
		t.Fatalf("a Thread that did not go idle must not carry idle_since, got %v", since)
	}
}

// TestThreadTakeoverIdleNeedsTheLastRecordToBeTurnEnded (T4C-29, D-4C-09): idle is decided by the
// batch's *last* taken-over record, never by "a TurnEnded appeared somewhere in the batch" — any
// record the agent produced afterwards means the conversation is still going.
func TestThreadTakeoverIdleNeedsTheLastRecordToBeTurnEnded(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)

	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(1, threadRecord("update", 0, "a"), ""),
		threadEvent(2, threadRecord("turnEnded", 1, ""), ""),
		threadEvent(3, threadRecord("update", 2, "but actually one more thing"), ""),
	}); err != nil {
		t.Fatalf("batch ending on a record after turnEnded: %v", err)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "active" {
		t.Fatalf("a TurnEnded the agent followed with another record must not idle the Thread, got %v", state)
	}
	if since, _ := idleAge(t, store, scene.run); since.Valid {
		t.Fatalf("no idle transition happened, so idle_since must stay NULL, got %v", since)
	}

	// The same holds across batches: a turn end followed later by a record leaves the final state
	// active, and a *later* turnEnded on its own does idle it — the last record decides.
	other := dispatcherDB(t)
	second := seedTakeoverScene(t, other)
	if _, err := takeOver(t, other, second, second.execution, []Object{
		threadEvent(1, threadRecord("turnEnded", 0, ""), ""),
	}); err != nil {
		t.Fatalf("turnEnded batch: %v", err)
	}
	if state := runThreadState(t, other, second.run); !state.Valid || state.String != "idle" {
		t.Fatalf("a batch that ends on turnEnded idles the Thread, got %v", state)
	}
	if _, err := takeOver(t, other, second, second.execution, []Object{threadEvent(2, threadRecord("update", 1, "resumed"), "")}); err != nil {
		t.Fatalf("resuming batch: %v", err)
	}
	if state := runThreadState(t, other, second.run); !state.Valid || state.String != "active" {
		t.Fatalf("a record arriving after the turn end returns the Thread to active, got %v", state)
	}
}

// TestThreadTakeoverReturnsIdleThreadToActive (T4C-30, D-4C-09): taking over a continuation record on
// an idle Thread clears `idle_since` with the state, so a stale instant can never be read as an
// expired idle window; and a run the running gate refused — cancelled, or no longer starting —
// receives no lifecycle write at all.
func TestThreadTakeoverReturnsIdleThreadToActive(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedTakeoverScene(t, store)

	if _, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(1, threadRecord("turnEnded", 0, ""), "")}); err != nil {
		t.Fatalf("idling batch: %v", err)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("precondition: the Thread must be idle, got %v", state)
	}
	if since, _ := idleAge(t, store, scene.run); !since.Valid {
		t.Fatal("precondition: idle_since must be set")
	}

	// A user turn posted into the idle window is the other way back to active, but this batch is a
	// plain continuation record the Node produced without a Cloud turn: the takeover itself restores
	// `active` and clears the instant.
	if _, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(2, threadRecord("update", 1, "carrying on"), "")}); err != nil {
		t.Fatalf("resuming batch: %v", err)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "active" {
		t.Fatalf("a taken-over continuation record returns an idle Thread to active, got %v", state)
	}
	if since, _ := idleAge(t, store, scene.run); since.Valid {
		t.Fatalf("idle_since must be cleared with the state, got %v", since)
	}

	// A cancelled run still appends the records it was given — a receipted event is never dropped
	// from the conversation (Phase 4B) — but its Thread state is written by nothing here: the
	// running gate refused the batch, so the lifecycle decision must refuse it too.
	cancelled := dispatcherDB(t)
	second := seedTakeoverScene(t, cancelled)
	if _, err := cancelled.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now(), version=version+1, updated_at=now() WHERE id=$1`, second.run); err != nil {
		t.Fatalf("request the cancel: %v", err)
	}
	if _, err := takeOver(t, cancelled, second, second.execution, []Object{
		threadEvent(1, threadRecord("update", 0, "a"), ""),
		threadEvent(2, threadRecord("turnEnded", 1, ""), ""),
	}); err != nil {
		t.Fatalf("cancelled run batch: %v", err)
	}
	if got := threadSeqs(t, cancelled, second.run); len(got) != 3 {
		t.Fatalf("a cancelled run still records the events it was given, got %v", got)
	}
	if state := runThreadState(t, cancelled, second.run); !state.Valid || state.String != "pending" {
		t.Fatalf("a cancelled run's Thread state must not be advanced, got %v", state)
	}
	phase, status, _, _, _, cancelAt := runFields(t, cancelled, second.run)
	if phase.String != "starting" || status != "dispatched" || !cancelAt.Valid {
		t.Fatalf("Phase 4B cancel-first semantics must be intact: %s/%s cancel=%v", phase.String, status, cancelAt)
	}
}

// TestThreadTakeoverInitialEchoLeavesLifecycleAlone (S5 regression, T4B-13/D-4C-13-1): the echo of
// the *first prompt* stays what Phase 4B made it — a receipt and nothing else — even now that echoes
// of user turns have a lifecycle meaning. It must not deliver anything, must not allocate a seq and
// must not move a Thread state, so the Cloud-authored seq=1 stays the immutable first prompt.
func TestThreadTakeoverInitialEchoLeavesLifecycleAlone(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)

	out, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(1, userTurnRecord(0, "the first prompt"), scene.initialTurnID),
	})
	if err != nil {
		t.Fatalf("initial echo batch: %v", err)
	}
	if out.N("takenOverThrough") != 1 {
		t.Fatalf("the receipt still advances, got %d", out.N("takenOverThrough"))
	}
	got := threadSeqs(t, store, scene.run)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("the first prompt's echo must not add an entry, got %v", got)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "pending" {
		t.Fatalf("an echo-only batch is not takeover evidence, got %v", state)
	}
	source, kind, _, turnID, present := firstTurn(t, store, scene.run)
	id, _ := turnID.(*string)
	if !present || source != "system" || kind != "user_turn" || id == nil || *id != scene.initialTurnID {
		t.Fatalf("seq=1 must stay the Cloud-authored prompt, got %s/%s turn=%v", source, kind, turnID)
	}
}

// TestThreadTakeoverKeepsTheTurnsOwnRecords pins the shape a real Node sends (Node protocol D2):
// every record of a user turn carries that turn's id, not only the user message that echoes it.
// Only the user message record is an echo; the agent's reply and the turn's `turnEnded` in the same
// turn are ordinary entries that keep the turn id. Before this rule the whole first turn of a real
// echo agent was dropped as an "echo" and the Thread never showed a reply or went idle.
func TestThreadTakeoverKeepsTheTurnsOwnRecords(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)

	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(1, threadRecord("meta", 0, ""), ""),
		threadEvent(2, userTurnRecord(1, "the first prompt"), scene.initialTurnID),
		threadEvent(3, threadRecord("update", 2, "echo: the first prompt"), scene.initialTurnID),
		threadEvent(4, threadRecord("turnEnded", 3, ""), scene.initialTurnID),
	}); err != nil {
		t.Fatalf("first turn batch: %v", err)
	}
	if got := threadSeqs(t, store, scene.run); !slices.Equal(got, []int64{1, 2, 3, 4}) {
		t.Fatalf("the first prompt's echo alone is skipped; meta, reply and turnEnded are entries, got %v", got)
	}
	for sequence, want := range map[int64]string{3: "update", 4: "turnEnded"} {
		_, source, kind, _, turnID, present := nodeEntry(t, store, scene.run, sequence)
		if !present || source != "node" || kind != want || turnID == nil || *turnID != scene.initialTurnID {
			t.Fatalf("node sequence %d must be a %s entry keeping the turn id, got present=%v %s/%s turn=%v", sequence, want, present, source, kind, turnID)
		}
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("a first turn that ended with no queued turn leaves the Thread idle, got %v", state)
	}

	turnID, turnSeq := seedQueuedUserTurn(t, store, scene.run, "and again")
	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(5, userTurnRecord(4, "and again"), turnID),
		threadEvent(6, threadRecord("update", 5, "echo: and again"), turnID),
		threadEvent(7, threadRecord("turnEnded", 6, ""), turnID),
	}); err != nil {
		t.Fatalf("second turn batch: %v", err)
	}
	if _, _, status, _ := threadEntryRow(t, store, scene.run, turnSeq); status != "delivered" {
		t.Fatalf("the user message record delivers its turn, got %q", status)
	}
	if _, _, kind, _, id, present := nodeEntry(t, store, scene.run, 6); !present || kind != "update" || id == nil || *id != turnID {
		t.Fatalf("the reply to a later turn is an entry keeping its turn id, got present=%v kind=%s turn=%v", present, kind, id)
	}
	if _, _, _, _, _, present := nodeEntry(t, store, scene.run, 5); present {
		t.Fatal("the later turn's user message is an echo and must not become a node entry")
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("the later turn ended too, got %v", state)
	}
}
