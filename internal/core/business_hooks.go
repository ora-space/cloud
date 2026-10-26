package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// The B side of the Agent Run control plane seam: Cloud's IssueRun, Thread, delivery and release
// lifecycle, bound onto the five callbacks an upstream A side leaves nil.
//
// Ownership (controller-integration D6, invariant 7). The control plane owns every table that
// records execution evidence — `execution_work`, `node_executions`, `node_event_receipts`,
// `thread_commands`, `space_agents`, `revisions` and `workspaces.issue_run_id` — and this package's
// B side owns the business tables (`issue_runs.phase` and its Thread columns, `thread_entries`).
// One writer per table: the A side never writes a business fact, and nothing here writes execution
// evidence, so there is exactly one dispatcher and one state machine.
//
// Transaction. Each hook is invoked from inside the control-plane transaction that just persisted
// the evidence it reacts to, so the transition it decides commits or rolls back with that evidence.
// The seam passes the caller's `*sql.Tx`; businessTransaction recovers the enclosing `*transaction`
// so the B side runs on that same transaction rather than on a second view of the handle whose
// enqueues, Thread entries and post-commit hints would be dropped. Nothing here opens a transaction,
// spawns a goroutine, or defers a post-commit effect, and no hook performs external I/O.
//
// Failure. A non-nil error is turned by the seam into `409 business_hook_failed`, which rolls the
// control-plane transaction back: the Node's event is not acknowledged and is replayed. That is why
// the hooks never "tolerate" a contradiction — a business transition that cannot be justified by
// authoritative rows must abort rather than be skipped silently.

// businessTransactionKey names the context slot a transaction publishes itself under for the
// duration of its own body. It is an unexported struct so no other package can collide with it.
type businessTransactionKey struct{}

// businessTransaction returns the transaction a B-side hook is running inside.
//
// The seam hands a hook the caller's `*sql.Tx` and nothing else. That handle is the same one the
// enclosing `*transaction` wraps (transact publishes it on the context before running its body), so
// recovering it here is what keeps the B side's writes, enqueues and hints in one commit with the
// control plane's. A hook reached with a handle that does not belong to the transaction that
// published itself is an invariant violation — running on a foreign handle would silently split the
// settlement across two commits — so it aborts as a database failure.
func businessTransaction(ctx context.Context, tx *sql.Tx) *transaction {
	t, _ := ctx.Value(businessTransactionKey{}).(*transaction)
	if t == nil || t.tx != tx {
		panic(databaseFailure{fmt.Errorf("business hook invoked outside its control-plane transaction")})
	}
	return t
}

// BindBusinessHooks wires Cloud's IssueRun/Thread lifecycle onto the control plane's five
// same-transaction callbacks. It is called once at process start, before any request is served; a
// Store that never binds them keeps the agreed control-plane-only deployment, in which every hook
// is a no-op handoff.
func BindBusinessHooks(s *Store) {
	s.OnThreadEvents = onThreadEvents
	s.OnSessionEnded = onSessionEnded
	s.OnRunWorkspaceSettled = onRunWorkspaceSettled
	s.OnRunWorkspaceDeleted = onRunWorkspaceDeleted
	s.OnDeliverySettled = onDeliverySettled
	s.OnScheduledArtifactsSettled = onScheduledArtifactsSettled
}

// onThreadEvents stores one batch of a session execution's history records as Thread entries and
// decides the Thread's lifecycle from them (`threadEventsTakenOver`, Thread D1..D4).
//
// The wire event carries its record as an opaque JSON *string* — the format belongs to desktop
// `ora-history`, and Cloud stores it without interpreting its business fields. It is parsed here,
// once, at the seam, so the business core works on an object and the stored entry is the record
// itself. A record that is not a JSON object is refused by the A side before this point.
func onThreadEvents(ctx context.Context, tx *sql.Tx, run, execution string, events []Object) error {
	t := businessTransaction(ctx, tx)
	parsed := make([]Object, 0, len(events))
	for _, ev := range events {
		record := ev.S("record")
		var content Object
		if err := json.Unmarshal([]byte(record), &content); err != nil || content == nil {
			return fmt.Errorf("business hook: run %s execution %s event %d carries a record that is not a JSON object", run, execution, ev.N("sequence"))
		}
		item := Object{"sequence": ev.N("sequence"), "turnId": ev.S("turnId"), "record": content}
		if position, present := ev["historySequence"]; present {
			item["historySequence"] = position
		}
		if visible, present := ev["visibleText"]; present {
			item["visibleText"] = visible
		}
		if ev.B("visibleTextTruncated") {
			item["visibleTextTruncated"] = true
		}
		parsed = append(parsed, item)
	}
	return t.store.settleThreadEvents(t, run, execution, parsed)
}

// onSessionEnded settles a session execution's terminal event: the Thread reaches `ended`, every
// still-queued user turn is discarded, the run enters `delivering` and its Revision delivery work
// item is released, all in the control plane's transaction (`settleSessionEnded`, Thread D4,
// IssueRun D3/D4).
func onSessionEnded(ctx context.Context, tx *sql.Tx, run, execution string, ended Object) error {
	t := businessTransaction(ctx, tx)
	return t.store.settleSessionEnded(t, run, execution, ended)
}

// onRunWorkspaceSettled settles the terminal state of a run Workspace's create_workspace operation:
// `provisioning → starting` when the Workspace came up, or `releasing` with the cancelled/failed
// outcome D3's matrix fixes, together with the delete declaration (`settleRunWorkspace`, IssueRun
// D3/D6). status is the operation's own outcome word: `ready` or `failed`.
func onRunWorkspaceSettled(ctx context.Context, tx *sql.Tx, run, status string) error {
	t := businessTransaction(ctx, tx)
	return t.store.settleRunWorkspace(t, run, status)
}

// onRunWorkspaceDeleted settles the terminal state of a run Workspace's delete_workspace operation,
// which is the last transition of the run's lifecycle: `releasing → done` (`settleRunWorkspaceDeleted`,
// IssueRun D3).
func onRunWorkspaceDeleted(ctx context.Context, tx *sql.Tx, run string) error {
	t := businessTransaction(ctx, tx)
	return t.store.settleRunWorkspaceDeleted(t, run)
}

// onDeliverySettled settles one delivery execution's terminal outcome: a verified Revision releases
// the run, a failure either retries with D5's backoff or gives up, and a run Cloud cannot deliver
// for at all is released as `skipped` (`settleDelivery`, IssueRun D3/D4/D5). The settled object is
// the control plane's own verdict, reduced from the Node's result and the object-store check it ran
// outside the transaction.
func onDeliverySettled(ctx context.Context, tx *sql.Tx, run, execution string, settled Object) error {
	t := businessTransaction(ctx, tx)
	return t.store.settleDelivery(t, run, execution, settled)
}

// onScheduledArtifactsSettled registers the artifacts Cloud verified and finishes the run's release
// once its Revision delivery has also settled (scheduled-task verdict/artifacts ADR D3). The settled
// object is the control plane's own verdict: per-object verification and the absolute-deadline
// comparison, both computed outside the transaction and bound to the frozen execution input.
func onScheduledArtifactsSettled(ctx context.Context, tx *sql.Tx, run, execution string, settled Object) error {
	t := businessTransaction(ctx, tx)
	return t.store.settleScheduledArtifacts(t, run, execution, settled)
}
