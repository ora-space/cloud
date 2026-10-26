package core

import "context"

// Expiry of scheduled artifact deliveries (scheduled-task verdict/artifacts ADR D3).
//
// The window is fixed when the declaration is stored and never renewed, so expiry needs no policy of
// its own: a row still `delivering` at or past its deadline is settled as failed, no object is
// registered, and the run is released if its Revision delivery has already settled. Uploaded objects
// that never got registered are cleaned up by the prefix GC, which is the only cleanup that does not
// depend on a receipt arriving at all.

// scheduledArtifactBatchSize bounds one expiry pass so a single tick settles at most this many runs.
const scheduledArtifactBatchSize = 100

// ExpireScheduledArtifactDeliveriesOnce settles the artifact deliveries whose window has closed.
//
// The scan is read-only and the decision is taken again inside each short transaction against the
// database's own clock, so a delivery that settled (or was released by another tick) in between is a
// no-op rather than an error.
func (s *Store) ExpireScheduledArtifactDeliveriesOnce(ctx context.Context) error {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT issue_run_id FROM scheduled_run_artifact_deliveries
		WHERE state='delivering' AND deadline_at <= clock_timestamp()
		ORDER BY deadline_at
		LIMIT $1`, scheduledArtifactBatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	var expired []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return err
		}
		expired = append(expired, runID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, runID := range expired {
		if _, err := s.transact(ctx, func(t *transaction) Object {
			delivery := t.one(`
				SELECT * FROM scheduled_run_artifact_deliveries
				WHERE issue_run_id=$1 AND state='delivering' AND deadline_at <= clock_timestamp()`, runID)
			if delivery == nil {
				return Object{}
			}
			o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
			if o == nil {
				return Object{}
			}
			if err := s.closeArtifactDelivery(t, o, "failed", scheduledArtifactDeadlineExpired); err != nil {
				panic(databaseFailure{err})
			}
			return Object{}
		}); err != nil {
			return err
		}
	}
	return nil
}
