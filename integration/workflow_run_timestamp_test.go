package integration

import (
	"testing"

	"github.com/google/uuid"
)

func TestPendingWorkflowRunDetailPreservesNullTimestamps(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Pending timestamps", "pending-timestamps-wf", nil)
	snapshot := publishWorkflow(t, f, wid, "pending-timestamps-pub")
	rid := uuid.NewString()
	_, err := f.store.Pool.Exec(`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name)
		VALUES($1,$2,$3,$4,'Pending timestamps')`, rid, f.tid, wid, snapshot.S("id"))
	must(t, err)
	run := f.call("GET", f.path("/workflows/"+wid+"/runs/"+rid), nil, "", 200)
	if run.S("status") != "pending" {
		t.Fatalf("run status = %v, want pending", run["status"])
	}
	for _, field := range []string{"startedAt", "finishedAt"} {
		if value, present := run[field]; !present || value != nil {
			t.Fatalf("pending run %s = %#v (present=%v), want JSON null", field, value, present)
		}
	}
}
