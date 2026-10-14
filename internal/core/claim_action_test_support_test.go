package core

// The two helpers that drive one real `claim` control action and read what it wrote.
//
// They live here rather than beside the scheduling-order tests because they are shared: the tests for
// the order of the claim queue assert which Operation a claim hands back, and the tests for a refused
// credentialed clone assert that a settled Operation is never claimed again. Both need the same
// driver, and one of them is outside the scheduling order's own file.
//
// Assertions on Cloud's scheduling policy never measure time. `claimOperation` runs the action and
// returns the Operation it chose; `operationVersion` reads the version the claim's own UPDATE bumps,
// which is the durable witness for "this Operation was claimed" when every other written value could
// be unchanged.

import (
	"context"
	"testing"
)

// claimOperation drives one real `claim` control action and reports the Operation it handed back.
func claimOperation(t *testing.T, store *Store, epoch int64) (Object, bool) {
	t.Helper()
	out, err := store.Control(context.Background(), &ControlRequest{
		Action: "claim", Body: Object{"epoch": epoch}, Service: controllerClaims(),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if value, ok := out["operation"]; !ok || value == nil {
		return nil, false
	}
	return out.O("operation"), true
}

// operationVersion reads the optimistic version the claim's own UPDATE bumps, which is the durable
// witness for "this Operation was claimed" when every other written value could be unchanged.
func operationVersion(t *testing.T, store *Store, id string) int64 {
	t.Helper()
	return scalarOf(t, store, `SELECT version FROM operations WHERE id=$1`, id)
}
