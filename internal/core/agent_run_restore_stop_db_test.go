package core

import (
	"encoding/json"
	"testing"
)

// Repeated reconciliation must not renew the observation or change the original responsibility.
func TestRestoreStopUnknownAddsOneResidualObservationWithoutReclaimAuthority(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	requestStartupEnd(t, store, scene.dispatchableScene)
	expireConfirmation(t, store, scene.run)
	startupPass(t, store)
	var before, after, responsibility, reason string
	if err := store.Pool.QueryRow(`SELECT stop_evidence::text,responsibility::text,reason
	 FROM agent_run_residuals WHERE run_id=$1`, scene.run).Scan(&before, &responsibility, &reason); err != nil {
		t.Fatal(err)
	}
	var evidence map[string]struct {
		Kind        string `json:"kind"`
		Source      string `json:"source"`
		ExecutionID string `json:"executionId"`
		ObservedAt  string `json:"observedAt"`
	}
	if err := json.Unmarshal([]byte(before), &evidence); err != nil {
		t.Fatal(err)
	}
	fact, ok := evidence["stop_unconfirmed"]
	if !ok || len(evidence) != 1 || fact.Kind != "stop_unconfirmed" || fact.Source != "cloud" || fact.ExecutionID != scene.execution || fact.ObservedAt == "" || reason != "agent_start_failed" {
		t.Fatalf("invalid named stop evidence: %s / %s", before, reason)
	}
	startupPass(t, store)
	startupPass(t, store)
	var currentResponsibility, currentReason string
	if err := store.Pool.QueryRow(`SELECT stop_evidence::text,responsibility::text,reason
	 FROM agent_run_residuals WHERE run_id=$1`, scene.run).Scan(&after, &currentResponsibility, &currentReason); err != nil {
		t.Fatal(err)
	}
	if before != after || responsibility != currentResponsibility || reason != currentReason {
		t.Fatal("stop evidence or original responsibility changed across reconciliation")
	}
	var pending, protected, retained bool
	if err := store.Pool.QueryRow(`SELECT e.result IS NULL,NOT w.admission_open,r.cleared_at IS NULL
	 FROM agent_run_residuals r JOIN node_executions e ON e.execution_id=$2
	 JOIN workspaces w ON w.id=r.workspace_id WHERE r.run_id=$1`, scene.run, scene.execution).Scan(&pending, &protected, &retained); err != nil || !pending || !protected || !retained {
		t.Fatalf("observation fabricated stop or lost residual protection: %v %v %v %v", pending, protected, retained, err)
	}
	if countDeleteOperations(t, store, scene.run) != 0 {
		t.Fatal("unconfirmed stop authorized deletion")
	}
}
