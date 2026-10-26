package core

import (
	"context"
	"encoding/base64"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"
)

// This opt-in measurement uses the production CAS, including transaction commit and projection.
// A dedicated PostgreSQL instance is required because WAL LSN is database-cluster wide.
func TestWorkflowNearLimitCASMeasurement(t *testing.T) {
	if os.Getenv("MEASURE_WORKFLOW_CAS") != "1" {
		t.Skip("opt-in dedicated PostgreSQL measurement")
	}
	store := dispatcherDB(t)
	g := iterationRunGraph("continue", 100, []any{"it", "item"})
	id := seedWorkflowRunScene(t, store, g)
	rng := rand.New(rand.NewSource(660))
	bytes := make([]byte, 48000)
	if _, err := rng.Read(bytes); err != nil {
		t.Fatal(err)
	}
	value := base64.StdEncoding.EncodeToString(bytes)
	s := &workflowIterationRunState{Round: 12, Strategy: "continue", Items: []any{}, Ledger: []any{}, Output: []any{}, Completed: []any{}}
	for i := 0; i < 24; i++ {
		s.Items = append(s.Items, value)
	}
	for i := 0; i < 12; i++ {
		s.Ledger = append(s.Ledger, Object{"item": value, "status": "succeeded", "output": value, "error": nil})
		s.Output = append(s.Output, value)
		s.Completed = append(s.Completed, Object{"index": i, "states": Object{"agg": Object{"status": "succeeded", "output": Object{"output": value}}}})
	}
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"items": s.Items}}, "it": {Status: "running", Iteration: s}, "agg": {Status: "succeeded", Output: Object{"output": value}}, "padding": {Status: "succeeded", Output: Object{"output": ""}}}
	target := workflowStateLimit - workflowFailureReserve - 1024
	padding := target - len(jsonText(workflowEncodedStates(states)))
	if padding < 0 {
		t.Fatal("measurement fixture exceeds target")
	}
	p := states["padding"]
	p.Output["output"] = strings.Repeat("x", padding)
	states["padding"] = p
	encodedBytes := len(jsonText(workflowEncodedStates(states)))
	if over := workflowStateBudget(readWorkflowGraph(g), states); over != nil {
		t.Fatalf("invalid measurement: %+v", over)
	}
	var version int64 = 1
	var before, after, dbVersion, compression string
	if err := store.Pool.QueryRow("SELECT version(),current_setting('wal_compression')").Scan(&dbVersion, &compression); err != nil {
		t.Fatal(err)
	}
	if err := store.Pool.QueryRow("SELECT pg_current_wal_insert_lsn()::text").Scan(&before); err != nil {
		t.Fatal(err)
	}
	const samples = 10
	total := time.Duration(0)
	for i := 0; i < samples; i++ {
		start := time.Now()
		_, err := store.transact(context.Background(), func(tx *transaction) Object {
			if !settleWorkflowRunStates(tx, id, version, "running", "", states) {
				t.Fatal("CAS not applied")
			}
			return Object{}
		})
		elapsed := time.Since(start)
		total += elapsed
		if err != nil {
			t.Fatal(err)
		}
		version++
		t.Logf("sample=%d duration=%s", i+1, elapsed)
	}
	if err := store.Pool.QueryRow("SELECT pg_current_wal_insert_lsn()::text").Scan(&after); err != nil {
		t.Fatal(err)
	}
	var wal int64
	if err := store.Pool.QueryRow("SELECT pg_wal_lsn_diff($1::pg_lsn,$2::pg_lsn)::bigint", after, before).Scan(&wal); err != nil {
		t.Fatal(err)
	}
	t.Logf("PostgreSQL=%s wal_compression=%s encoded_bytes=%d samples=%d total=%s average=%s wal_before=%s wal_after=%s wal_bytes=%d padding=%d; repeated fields: source.items, iteration.items, ledger.item/output, iteration.output, completed.states, current member", dbVersion, compression, encodedBytes, samples, total, total/samples, before, after, wal, padding)
}
