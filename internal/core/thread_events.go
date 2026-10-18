package core

// Thread append notifications — the notification half of Thread D5.
//
// What this is: after an entry-write transaction commits, Cloud publishes
// `issue_run.thread_appended{issueId, runId, lastSeq}` on the tenant's Space event stream so a live
// subscriber knows to re-read. Nothing here is authoritative and nothing here is load-bearing for
// durability: the durable log is `thread_entries`, the only thing that advances a client's cursor is
// the Thread GET, and a committed entry whose hint is lost is still returned by the next GET.
//
// The publication is deliberately transactional rather than collection-based: a hint is queued on
// the caller's *transaction and released by Store.transact only after Commit returns nil, exactly
// like claimable operations and Thread command signals. That is what makes "rollback publishes
// nothing" structural — no code path can publish from inside a transaction — and it covers every
// entry-write path with one mechanism, including the A→B takeover hook, which has no
// caller-visible event slice to append to.
//
// Two event types are published, and they answer different questions (Thread D5):
//
//   - `issue_run.thread_appended` keeps its append-only meaning: this transaction appended at least
//     one entry. Its name still says what happened.
//   - `issue_run.thread_changed` is the generalized hint: *any* commit that changed the Thread's REST
//     representation, including the state-only changes the appended event never covered — a user
//     turn moving `queued → delivered`, `threadState` moving `active ⇄ idle` or to `ending`, and an
//     `idleSince` write.
//
// Both are lossy invalidation hints and neither is authoritative. A client that saw only
// `thread_changed` re-reads with GET exactly as one that saw `thread_appended` does.

// threadAppendedEvent is Thread D5's append-specific event type, spelled once so the wire name
// cannot drift between the publish sites and their tests.
const threadAppendedEvent = "issue_run.thread_appended"

// threadChangedEvent is A4's generalized invalidation event. It is a *new* name rather than a
// widened `thread_appended`: keeping the append event's meaning intact is what lets a subscriber
// that only cares about new entries keep its existing filter, and the payload shape is identical.
const threadChangedEvent = "issue_run.thread_changed"

// threadAppended queues the post-commit hint for one run whose Thread gained at least one entry in
// this transaction. `run` must be the authoritative row this transaction read: the payload's
// identity comes from the database, never from a request.
//
// lastSeq is read here, after the caller's entry writes, so it is the max(seq) this transaction is
// about to commit — the value the publish-after-commit rule requires. It is a high-water mark, not a promise that every
// seq at or below it was delivered to this subscriber: events may be lost, reordered or duplicated
// and only a GET answers what the log actually holds.
//
// The space is looked up rather than taken from the caller because the event is addressed to the
// tenant's sole collaboration space, which is the stream a Thread subscriber holds. A tenant with no
// live space has no subscriber to reach, and a notification must never fail the mutation it
// describes, so the hint is dropped instead of raised; the same applies to a Store with no hub.
func threadAppended(t *transaction, run Object) {
	space := t.one("SELECT id FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL", run.S("tenantId"))
	if space == nil {
		return
	}
	queueThreadEvent(t, &SpaceEvent{
		Type:    threadAppendedEvent,
		SpaceID: space.S("id"),
		IssueID: run.S("issueId"),
		RunID:   run.S("id"),
		LastSeq: t.one("SELECT COALESCE(MAX(seq),0) AS m FROM thread_entries WHERE run_id=$1", run.S("id")).N("m"),
	})
}

// threadChanged queues A4's generalized hint for one run whose Thread REST representation this
// transaction changed. Call it at the write, not at the call site's exit: the hint must describe a
// mutation this transaction actually made.
//
// At most one `thread_changed` is queued per run per transaction, and a repeat call *raises* the
// queued hint's lastSeq instead of adding a second event. That is what makes a batch which both
// echoed a user turn and appended records — or appended several records — publish one hint rather
// than a burst of partly-stale ones. Raising rather than ignoring matters because the first call may
// happen before the entries are written, so its lastSeq is the lowest value the transaction will
// reach, not the highest.
//
// The space is looked up rather than taken from the caller because the event is addressed to the
// tenant's sole collaboration space, which is the stream a Thread subscriber holds. A tenant with no
// live space has no subscriber to reach, and a notification must never fail the mutation it
// describes, so the hint is dropped instead of raised; the same applies to a Store with no hub.
func threadChanged(t *transaction, run Object) {
	runID := run.S("id")
	space := t.one("SELECT id FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL", run.S("tenantId"))
	if space == nil {
		return
	}
	// Read after the caller's writes so this is the max(seq) the transaction is about to commit. It
	// is a high-water mark ("there is data at or below this seq"), never a cursor: only a GET says
	// what the log actually holds.
	lastSeq := t.one("SELECT COALESCE(MAX(seq),0) AS m FROM thread_entries WHERE run_id=$1", runID).N("m")
	for i := range t.events {
		if t.events[i].Type == threadChangedEvent && t.events[i].RunID == runID {
			t.events[i].LastSeq = lastSeq
			return
		}
	}
	queueThreadEvent(t, &SpaceEvent{
		Type:    threadChangedEvent,
		SpaceID: space.S("id"),
		IssueID: run.S("issueId"),
		RunID:   runID,
		LastSeq: lastSeq,
	})
}

// queueThreadEvent appends one hint to the transaction's own event list — the same list
// Store.transact publishes, in order, only after Commit returns nil. Appending here rather than
// publishing is what makes "rollback publishes nothing" structural: no code path can publish from
// inside a transaction, and every entry-write path, including the A→B takeover hook, reaches the
// subscriber stream through this one mechanism.
//
// The list is deduped per (type, run) by the callers above rather than here, because raising an
// already-queued hint's sequence requires reading it back.
func queueThreadEvent(t *transaction, ev *SpaceEvent) {
	t.events = append(t.events, *ev)
}
