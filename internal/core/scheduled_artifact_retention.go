package core

import (
	"context"

	"github.com/wanglongan587/cloud/internal/objectstore"
)

// Retention and prefix cleanup of scheduled-run artifacts (scheduled-task verdict/artifacts ADR D5).
//
// Both passes follow the repository's bounded-pass shape: a read-only scan outside the advisory lock,
// the object-store calls between transactions, then one short transaction per run. Neither makes a
// business decision the durable state has not already made - retention only applies the ten-run cost
// boundary, and the prefix sweep only removes objects the index does not back.

const (
	// scheduledArtifactRunsPerTask is D5's cost boundary: ten artifact-bearing runs per task, newest
	// first by the frozen `scheduled_at`, never by an occurrence row the history trim may have removed.
	scheduledArtifactRunsPerTask = 10
	// scheduledArtifactCleanupBatch bounds one pass so a single tick advances at most this many runs.
	scheduledArtifactCleanupBatch = 100
)

// PruneScheduledRunArtifactsOnce drops the artifacts of the runs beyond a task's retention boundary.
//
// Objects are deleted before their index rows, and outside the transaction: a crash between the two
// leaves rows that the next pass repeats, and re-deleting a missing object is success. The row is
// re-checked under the transaction so a delivery that started registering in between is not pruned
// mid-flight.
func (s *Store) PruneScheduledRunArtifactsOnce(ctx context.Context) error {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT issue_run_id FROM (
			SELECT issue_run_id,
			       row_number() OVER (PARTITION BY task_id ORDER BY scheduled_at DESC, issue_run_id DESC) AS position
			FROM (SELECT DISTINCT issue_run_id, task_id, scheduled_at FROM scheduled_run_artifacts) covering
		) ranked
		WHERE position > $1
		ORDER BY issue_run_id
		LIMIT $2`, scheduledArtifactRunsPerTask, scheduledArtifactCleanupBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return err
		}
		runs = append(runs, runID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, runID := range runs {
		keys, err := s.scheduledArtifactKeys(ctx, runID)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := objectstore.Delete(ctx, s.ObjectStore, key); err != nil {
				return err
			}
		}
		if _, err := s.transact(ctx, func(t *transaction) Object {
			if !scheduledRunBeyondRetention(t, runID) {
				return Object{}
			}
			t.exec("DELETE FROM scheduled_run_artifacts WHERE issue_run_id=$1", runID)
			t.exec("DELETE FROM scheduled_run_artifact_rejections WHERE issue_run_id=$1", runID)
			// D6: the pruned run's index changed too, so its readers must re-read.
			invalidateScheduledArtifacts(t, runID)
			return Object{}
		}); err != nil {
			return err
		}
	}
	return nil
}

// scheduledRunBeyondRetention re-reads the retention position of one run inside the pruning
// transaction, and refuses to prune a delivery that is still inside its window.
func scheduledRunBeyondRetention(t *transaction, runID string) bool {
	row := t.one(`
		WITH ranked AS (
			SELECT issue_run_id,
			       row_number() OVER (PARTITION BY task_id ORDER BY scheduled_at DESC, issue_run_id DESC) AS position
			FROM (SELECT DISTINCT issue_run_id, task_id, scheduled_at FROM scheduled_run_artifacts) covering
		)
		SELECT position FROM ranked WHERE issue_run_id=$1`, runID)
	if row == nil || row.N("position") <= scheduledArtifactRunsPerTask {
		return false
	}
	return !artifactDeliveryPending(t, runID)
}

// CollectScheduledArtifactObjectsOnce deletes the objects of a delivery that settled without
// registering them.
//
// Keys are Cloud's own and deterministic (`scheduledArtifactKey`), so the sweep needs no bucket
// listing: it can name every object the run could have written and delete the ones the index does not
// back. Settled runs remain eligible for recurring sweeps: a PUT admitted before grant expiry can
// finish after an earlier DELETE, and retention can remove an index after a successful delivery.
func (s *Store) CollectScheduledArtifactObjectsOnce(ctx context.Context) error {
	if s.ObjectStore == nil {
		return nil
	}
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT issue_run_id, tenant_id FROM scheduled_run_artifact_deliveries
		WHERE state IN ('delivered','failed','partial','skipped')
		  AND (artifacts_gc_at IS NULL OR artifacts_gc_at <= now() - interval '1 minute')
		ORDER BY artifacts_gc_at NULLS FIRST, settled_at, issue_run_id
		LIMIT $1`, scheduledArtifactCleanupBatch)
	if err != nil {
		return err
	}
	defer rows.Close()
	type delivery struct{ runID, tenantID string }
	var pending []delivery
	for rows.Next() {
		var entry delivery
		if err := rows.Scan(&entry.runID, &entry.tenantID); err != nil {
			return err
		}
		pending = append(pending, entry)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, entry := range pending {
		unregistered, err := s.unregisteredScheduledArtifactKeys(ctx, entry.tenantID, entry.runID)
		if err != nil {
			return err
		}
		for _, key := range unregistered {
			if err := objectstore.Delete(ctx, s.ObjectStore, key); err != nil {
				return err
			}
		}
		if _, err := s.transact(ctx, func(t *transaction) Object {
			t.exec(`UPDATE scheduled_run_artifact_deliveries SET artifacts_gc_at=now()
				WHERE issue_run_id=$1 AND state IN ('delivered','failed','partial','skipped')`, entry.runID)
			return Object{}
		}); err != nil {
			return err
		}
	}
	return nil
}

// scheduledArtifactKeys lists the object keys one run's index backs.
func (s *Store) scheduledArtifactKeys(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT object_key FROM scheduled_run_artifacts WHERE issue_run_id=$1 ORDER BY artifact_index`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// unregisteredScheduledArtifactKeys derives the keys a run declared but never registered.
func (s *Store) unregisteredScheduledArtifactKeys(ctx context.Context, tenantID, runID string) ([]string, error) {
	declared, err := s.Pool.QueryContext(ctx, `
		SELECT (entry.item->>'index')::int
		FROM scheduled_run_artifact_deliveries d
		JOIN node_executions e ON e.operation_id = d.issue_run_id AND e.kind = 'agent_session'
		CROSS JOIN LATERAL jsonb_array_elements(COALESCE(e.result->'artifactManifest'->'items','[]'::jsonb)) AS entry(item)
		WHERE d.issue_run_id=$1`, runID)
	if err != nil {
		return nil, err
	}
	defer declared.Close()
	var keys []string
	for declared.Next() {
		var index int
		if err := declared.Scan(&index); err != nil {
			return nil, err
		}
		keys = append(keys, scheduledArtifactKey(tenantID, runID, index))
	}
	if err := declared.Err(); err != nil {
		return nil, err
	}
	registered, err := s.scheduledArtifactKeys(ctx, runID)
	if err != nil {
		return nil, err
	}
	backed := map[string]bool{}
	for _, key := range registered {
		backed[key] = true
	}
	orphans := make([]string, 0, len(keys))
	for _, key := range keys {
		if !backed[key] {
			orphans = append(orphans, key)
		}
	}
	return orphans, nil
}
