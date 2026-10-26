package core

// These named seams are deliberately transaction-scoped. Business code in this package can
// extend them with enqueueExecutionWork/enqueueThreadCommand/createRunWorkspace/deleteRunWorkspace
// without reacquiring the Store's advisory lock. Nil adapters are the agreed A/B no-op handoff.
func threadEventsTakenOver(t *transaction, run, execution string, events []Object) {
	if t.onThreadEvents != nil {
		if err := t.onThreadEvents(t.ctx, t.tx, run, execution, events); err != nil {
			reject(409, "business_hook_failed")
		}
	}
}

func sessionEnded(t *transaction, run, execution string, ended Object) {
	if t.onSessionEnded != nil {
		if err := t.onSessionEnded(t.ctx, t.tx, run, execution, ended); err != nil {
			reject(409, "business_hook_failed")
		}
	}
}

func runWorkspaceSettled(t *transaction, run, status string) {
	if t.onRunWorkspaceSettled != nil {
		if err := t.onRunWorkspaceSettled(t.ctx, t.tx, run, status); err != nil {
			reject(409, "business_hook_failed")
		}
	}
}

func runWorkspaceDeleted(t *transaction, run string) {
	if t.onRunWorkspaceDeleted != nil {
		if err := t.onRunWorkspaceDeleted(t.ctx, t.tx, run); err != nil {
			reject(409, "business_hook_failed")
		}
	}
}

func deliverySettled(t *transaction, run, execution string, settled Object) {
	if t.onDeliverySettled != nil {
		if err := t.onDeliverySettled(t.ctx, t.tx, run, execution, settled); err != nil {
			reject(409, "business_hook_failed")
		}
	}
}

func scheduledArtifactsSettled(t *transaction, run, execution string, settled Object) {
	if t.onScheduledArtifactsSettled != nil {
		if err := t.onScheduledArtifactsSettled(t.ctx, t.tx, run, execution, settled); err != nil {
			reject(409, "business_hook_failed")
		}
	}
}
