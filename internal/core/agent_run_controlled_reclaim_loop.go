package core

import "context"

// reconcileControlledResiduals only revisits explicitly registered operator intents. It does
// not discover/authorize old D8 deletions or escalate an automatic owned-delete responsibility.
func (s *Store) reconcileControlledResiduals(ctx context.Context) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		for _, intent := range t.list("SELECT * FROM agent_run_reclamation_intents WHERE state<>'succeeded' ORDER BY last_checked_at NULLS FIRST,registered_at,id LIMIT $1", agentDeliveryBatchSize) {
			root := controlledIntentRoot(t, intent)
			row := refreshControlledTargets(t, root)
			t.exec("UPDATE agent_run_reclamation_intents SET last_checked_at=clock_timestamp() WHERE id=$1", intent.S("id"))
			if row["reclaimConflictedAt"] != nil {
				continue
			}
			if intent.S("commandKind") == "delete_data" && row.S("reclaimOperationId") != "" {
				op := t.one("SELECT * FROM operations WHERE id=$1", row.S("reclaimOperationId"))
				if op != nil && (op.S("state") == "queued" || op.S("state") == "retry_wait" || op.S("state") == "running") {
					t.queued = append(t.queued, op.S("id"))
				}
			}
		}
		return nil
	})
	return err
}
