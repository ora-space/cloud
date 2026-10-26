package core

import "context"

// ReconcileAgentRunResidualsOnce scans the ledger, independently of the business Run's phase.
// Scheduling time has no evidentiary meaning. Old unresolved rows rotate behind rows not yet
// checked, so one bounded batch of unknowns cannot starve the remainder of the ledger.
func (s *Store) ReconcileAgentRunResidualsOnce(ctx context.Context) error {
	rows, err := s.Pool.QueryContext(ctx, `SELECT run_id FROM agent_run_residuals
 WHERE reclaim_state IN ('reconciling','reclaiming')
 ORDER BY reclaim_checked_at NULLS FIRST,created_at,run_id LIMIT $1`, agentDeliveryBatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Claim the scheduling position separately: even an invalid historical ownership row
		// whose reconciliation rolls back must yield its scan slot to the next bounded pass.
		_, markErr := s.transact(ctx, func(t *transaction) Object {
			t.exec("UPDATE agent_run_residuals SET reclaim_checked_at=clock_timestamp() WHERE run_id=$1 AND reclaim_state IN ('reconciling','reclaiming')", id)
			return nil
		})
		if markErr != nil {
			continue
		}
		_, _ = s.transact(ctx, func(t *transaction) Object {
			row := t.one("SELECT * FROM agent_run_residuals WHERE run_id=$1 AND reclaim_state IN ('reconciling','reclaiming')", id)
			if row == nil || row["reclaimConflictedAt"] != nil {
				return nil
			}
			row = refreshResidualTargets(t, row)
			if row.S("reclaimState") == "reconciling" {
				return authorizeResidualOwnedDelete(t, id)
			}
			return resumeResidualReclamation(t, row)
		})
	}
	return s.reconcileControlledResiduals(ctx)
}
