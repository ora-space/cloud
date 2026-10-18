package core

import "context"

// The three background recovery passes of the delivery/release half (IssueRun D3/D5/D8, operation
// D4). All follow the repository's established bounded-pass shape — a read-only scan
// outside the advisory lock, then one short transaction per run, with the sleep between ticks owned
// by the caller — exactly like the dispatch, session-start, idle-Thread and cancel passes. None of the
// three makes a business decision the durable state has not already made:
//
//	GiveUpStaleDeliveriesOnce      applies D5's two give-up limits to a run the delivery path has
//	                               already left `delivering`; it decides nothing that a fresh
//	                               delivery result would not decide, it only decides it on the clock.
//	RedeclareRunWorkspaceDeletesOnce  re-declares a delete intent that the `releasing` transition
//	                               already committed. The run being `releasing` IS the decision; this
//	                               pass only guarantees the Controller has an operation to execute.
//	GiveUpStaleWorkspaceReleasesOnce  applies D8's single give-up limit to a run the release path has
//	                               already left `releasing`, so a Workspace whose Node never answers
//	                               again cannot hold the run (and its Project's operation slot)
//	                               forever. The run ceases to be retried; it is never reported as
//	                               deleted.
//
// None of them invents a limit. D5 fixes the give-up window and the unreachability window as Cloud
// configuration ("这两个上限是第一版默认值（Cloud 配置项）"), D8 reuses the unreachability one verbatim
// ("本决策不为删除侧引入新的时限") and the delete re-declaration has no limit at all: a `releasing` run
// must reach `done`, and D3's own durability rule ("Controller 丢信号时靠周期领取兜底") is precisely
// that Cloud keeps a durable intent reachable until it is executed — or, under D8, until Cloud
// concludes it will not be.

// agentDeliveryBatchSize bounds one scan pass so a single tick advances at most this many runs.
// Ordering is deterministic (the oldest first) so every tick drains the backlog without starving.
const agentDeliveryBatchSize = 100

// giveUpStaleDelivery releases one `delivering` run whose delivery has exceeded D5's limits, in its
// own short transaction. The re-read repeats the scan's predicate, so a run that settled its delivery
// (or was released by another tick) between the scan and this transaction is a deterministic no-op
// rather than an error.
//
// The whole decision is `deliveryGivenUp`, and it is taken inside the transaction that writes the
// release: a limit that is checked outside the write could release a run whose retry had just
// succeeded, which is exactly the "don't pre-empt a delivery that is still alive" obligation.
func (s *Store) giveUpStaleDelivery(ctx context.Context, runID string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one(`
			SELECT * FROM issue_runs
			WHERE id = $1 AND executor_type = 'agent' AND deleted_at IS NULL AND phase = 'delivering'`, runID)
		if o == nil {
			return Object{}
		}
		if !s.deliveryGivenUp(t, o) {
			return Object{}
		}
		// No Revision id: this path gives a delivery up, so the run's result records an explicit null
		// `revisionId` beside `deliveryState = failed` (Cloud Revision D4, IssueRun D5).
		if err := s.releaseAfterDelivery(t, o, deliveryStateFailed, ""); err != nil {
			panic(databaseFailure{err})
		}
		return Object{}
	})
	return err
}

// scanStaleDeliveringRuns returns the bounded, deterministically ordered ids of `delivering` runs.
// It deliberately does not evaluate D5's limits: the limits are judged on database time against
// configured durations, and repeating them per run in SQL would put the policy in two places. The
// scan only narrows to the phase; giveUpStaleDelivery decides. Runs already past a limit are the
// oldest, so ordering by the delivery's own start keeps the pass from starving them.
func (s *Store) scanStaleDeliveringRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type = 'agent'
		  AND ir.deleted_at IS NULL
		  AND ir.phase = 'delivering'
		ORDER BY (SELECT min(w.created_at) FROM execution_work w
		          WHERE w.run_id = ir.id AND w.kind = 'deliver_revision'), ir.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GiveUpStaleDeliveriesOnce is one bounded pass of D5's give-up policy: find the runs still
// `delivering` (read-only) and release each one whose delivery has exhausted the configured windows,
// in its own short transaction. Per-run failures are deliberately not surfaced — releaseAfterDelivery
// rolls its transaction back and leaves the run `delivering` for a later tick, and the loop must not
// flood logs because one run is transiently stuck. Only a scan-level failure cancels the pass.
//
// A deployment that configured neither limit is a no-op, not an immediate give-up: with both windows
// unset nothing is ever stale (deliveryGivenUp fails closed), so the scan would only burn a query.
func (s *Store) GiveUpStaleDeliveriesOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.DeliveryGiveUpAfter <= 0 && s.DeliveryUnreachableAfter <= 0 {
		return nil
	}
	ids, err := s.scanStaleDeliveringRuns(ctx, agentDeliveryBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = s.giveUpStaleDelivery(ctx, id)
	}
	return nil
}

// redeclareRunWorkspaceDelete re-declares the run Workspace's delete for one `releasing` run whose
// delete intent has no operation in flight, in its own short transaction. The re-read repeats the
// scan's predicate; the seam re-reads the run and Workspace authoritatively again and is idempotent
// on an operation that is still in flight, so a second tick cannot produce a second operation.
//
// A run that is no longer `releasing` is a no-op, and a declaration failure (a busy Project, a
// missing Workspace) rolls the transaction back and leaves the run `releasing` — the intent is not
// lost, it is simply not yet executable, and the next tick retries it.
func (s *Store) redeclareRunWorkspaceDelete(ctx context.Context, runID string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one(`
			SELECT * FROM issue_runs
			WHERE id = $1 AND executor_type = 'agent' AND deleted_at IS NULL AND phase = 'releasing'`, runID)
		if o == nil {
			return Object{}
		}
		if err := s.declareDelete(t, o); err != nil {
			panic(databaseFailure{err})
		}
		return Object{}
	})
	return err
}

// scanUndeclaredReleasingRuns returns the bounded, deterministically ordered ids of `releasing` runs
// whose run Workspace has no unfinished delete_workspace operation. The phase and the missing
// operation are both read here; the authoritative decision is repeated inside each per-run
// transaction and again inside the seam, so a stale id is harmless.
func (s *Store) scanUndeclaredReleasingRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type = 'agent'
		  AND ir.deleted_at IS NULL
		  AND ir.phase = 'releasing'
		  AND NOT EXISTS (
		    SELECT 1 FROM operations o
		    WHERE o.workspace_id = ir.workspace_id
		      AND o.kind = 'delete_workspace'
		      AND o.state IN ('queued','running','retry_wait','blocked'))
		ORDER BY ir.updated_at, ir.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RedeclareRunWorkspaceDeletesOnce is one bounded pass of the release reconciliation: find the
// `releasing` runs whose Workspace delete has no operation in flight (read-only) and re-declare each
// in its own short transaction. Per-run failures are not surfaced, for the same reason as every other
// pass: the transaction rolled back, the run is unchanged, and the next tick retries it. Only a
// scan-level failure cancels the pass.
//
// It exists because a delete operation can end without settling the run: a Node that refuses to
// quiesce fails the operation and restores the Workspace (restoreAdmission), which is operation D4's
// documented "quiesce 失败并按原规则重试" — and for a run Workspace the retrying party is Cloud, since
// the run Workspace has no public API and no user to retry it.
//
// A run D8 gave up on is out of this pass by its own phase: `done` is not `releasing`, so the scan
// cannot re-enter the release, re-create a delete, or re-trigger anything for it. D8's
// residual Workspace is intentionally left with no operation and no retry.
func (s *Store) RedeclareRunWorkspaceDeletesOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanUndeclaredReleasingRuns(ctx, agentDeliveryBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = s.redeclareRunWorkspaceDelete(ctx, id)
	}
	return nil
}

// giveUpStaleWorkspaceRelease applies D8 to one `releasing` run in its own short transaction. The
// re-read repeats the scan's predicate, and the give-up condition is judged from authoritative rows
// inside the same transaction as the write, so a run that settled its release (or whose Node came
// back) between the scan and this transaction is a deterministic no-op rather than a wrongful
// abandonment.
//
// The whole decision is `releaseGivenUp` — the run's own Workspace and its Node heartbeat — and it is
// never taken from the delete operation's state: an operation that has failed repeatedly, or one that
// never existed, is not evidence about reachability, and a refusal by a reachable Node is not a
// reason to give up.
func (s *Store) giveUpStaleWorkspaceRelease(ctx context.Context, runID string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one(`
			SELECT * FROM issue_runs
			WHERE id = $1 AND executor_type = 'agent' AND deleted_at IS NULL AND phase = 'releasing'`, runID)
		if o == nil {
			return Object{}
		}
		if !s.releaseGivenUp(t, o) {
			return Object{}
		}
		if err := s.giveUpRunWorkspaceRelease(t, o); err != nil {
			panic(databaseFailure{err})
		}
		return Object{}
	})
	return err
}

// scanReleasingRuns returns the bounded, deterministically ordered ids of `releasing` runs. Like
// scanStaleDeliveringRuns it deliberately does not evaluate D8's limit: the limit is judged on
// database time against a configured duration, and repeating it per row in SQL would put the policy
// in two places. Ordering by the run's own last write keeps the oldest stuck release first, so a
// backlog drains instead of starving behind newer runs.
func (s *Store) scanReleasingRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT ir.id FROM issue_runs ir
		WHERE ir.executor_type = 'agent'
		  AND ir.deleted_at IS NULL
		  AND ir.phase = 'releasing'
		ORDER BY ir.updated_at, ir.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GiveUpStaleWorkspaceReleasesOnce is one bounded pass of D8's give-up policy: find the runs still
// `releasing` (read-only) and stop waiting for each one whose Workspace's Node state has been unknown
// past the configured window, in its own short transaction. Per-run failures are deliberately not
// surfaced — the transaction rolled back, the run is unchanged, and the next tick retries it — for the
// same reason as the other passes: one transiently stuck run must not flood logs or cancel the pass.
//
// A deployment that configured no unreachability window is a no-op, not an immediate give-up: with the
// window unset nothing is ever stale (`releaseGivenUp` fails closed), so the scan would only burn a
// query. This is also what keeps D8 from becoming a second, differently-tuned limit: it reads the same
// `DeliveryUnreachableAfter` D5's delivery give-up reads, and neither can be enabled without the other.
func (s *Store) GiveUpStaleWorkspaceReleasesOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.DeliveryUnreachableAfter <= 0 {
		return nil
	}
	ids, err := s.scanReleasingRuns(ctx, agentDeliveryBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = s.giveUpStaleWorkspaceRelease(ctx, id)
	}
	return nil
}
