package core

import (
	"sort"
	"strings"
	"testing"
)

// nodeExecutionInternalColumns are the columns of node_executions that stay Cloud-side. Every other
// column of the table is on the Controller wire and must appear in nodeExecutionColumns.
//
// workflow_node_id is the graph node a session ran for. It is denormalized onto the row (migration
// 0040) so the one_session_per_workflow_node partial index has a predicate to test and so the
// scheduler can attribute a result without joining through execution_work. No Controller contract
// declares it: the proto ExecutionRecord has no such field, and the JSON binding's NodeExecution
// schema is closed.
var nodeExecutionInternalColumns = map[string]bool{
	"workflow_node_id": true,
}

// TestControllerExecutionProjectionIsExplicit is the regression guard for the contract break
// migration 0040 caused. The Controller-facing reads of node_executions were `SELECT *`, so adding
// the column silently put workflowNodeId into response bodies the closed NodeExecution schema
// forbids, and the integration suite failed with:
//
//	Error at "/pluginExecutions/0": property "workflowNodeId" is unsupported
//
// A row that reaches a response body is now projected through nodeExecutionColumns instead. This
// test pins that the projection covers the table exactly: a column added later must be classified
// deliberately — onto the wire, or into nodeExecutionInternalColumns — instead of drifting onto one
// or off the other by omission.
func TestControllerExecutionProjectionIsExplicit(t *testing.T) {
	store := dispatcherDB(t)
	rows, err := store.Pool.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_schema=current_schema() AND table_name='node_executions'`)
	if err != nil {
		t.Fatalf("read node_executions columns: %v", err)
	}
	defer rows.Close()
	var table []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if !nodeExecutionInternalColumns[column] {
			table = append(table, column)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read node_executions columns: %v", err)
	}
	projected := strings.Split(nodeExecutionColumns, ",")
	for i := range projected {
		projected[i] = strings.TrimSpace(projected[i])
	}
	sort.Strings(table)
	sort.Strings(projected)
	if strings.Join(projected, ",") != strings.Join(table, ",") {
		t.Fatalf("the Controller-facing projection drifted from the table:\n  projected:           %v\n  table minus internal: %v", projected, table)
	}
}
