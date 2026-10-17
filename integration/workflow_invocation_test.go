package integration

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// TestWorkflowConfirmCreatesLinkedWorkflowRun checks the persisted execution identity across the
// public confirm boundary, rather than accepting a legacy IssueRun simulation as execution evidence.
func TestWorkflowConfirmCreatesLinkedWorkflowRun(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Linked workflow", "inv-link-wf", nil)
	snapshot := publishWorkflow(t, f, wid, "inv-link-pub")
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Linked execution"}, "inv-link-issue", 200).O("resource").S("id")
	interaction := workflowComment(t, f, wid, iid, "inv-link-comment", "run")
	run := f.call("POST", confirmPath(f, iid, interaction.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "branch": "main", "snapshotId": snapshot.S("id"),
	}}, "inv-link-confirm", 200).O("resource")
	if run.S("workflowInvocationRef") == "" {
		t.Fatalf("confirmed workflow IssueRun has no linked WorkflowRun: %v", run)
	}
	workflowRun := f.call("GET", f.path("/workflows/"+wid+"/runs/"+run.S("workflowInvocationRef")), nil, "", 200)
	if workflowRun.S("snapshotId") != snapshot.S("id") || run.O("input").S("workflowSnapshotId") != snapshot.S("id") {
		t.Fatalf("linked runs disagree about the selected snapshot: issue=%v workflow=%v", run, workflowRun)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wid); n != 1 {
		t.Fatalf("confirm created %d WorkflowRuns, want exactly one", n)
	}
}

// assertUnclaimedWorkflowInvocation checks the entire refused-start transaction, including activity
// and the interaction claim; a status assertion alone would miss a partially committed start.
func assertUnclaimedWorkflowInvocation(t *testing.T, f *fixture, iid, interactionID string, activities int) {
	t.Helper()
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iid); n != 0 {
		t.Fatalf("refused confirm left %d IssueRuns", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs"); n != 0 {
		t.Fatalf("refused confirm left %d WorkflowRuns", n)
	}
	if n := f.scalar("SELECT count(*) FROM issue_activities WHERE issue_id=$1", iid); n != activities {
		t.Fatalf("refused confirm changed activity count from %d to %d", activities, n)
	}
	if n := f.scalar("SELECT count(*) FROM issue_interactions WHERE id=$1 AND run_id IS NULL AND input='{}'::jsonb", interactionID); n != 1 {
		t.Fatal("refused confirm claimed the interaction or changed its input")
	}
}

func invocationVariable(name string) core.Object {
	return core.Object{"name": name, "displayName": name, "fieldType": "text-input", "valueType": "string", "required": true}
}

func TestWorkflowConfirmAdmissionRollsBackBothRuns(t *testing.T) {
	for _, scenario := range []string{"unpublished", "wrong-workflow-snapshot", "foreign-snapshot", "missing-input", "invalid-input-type"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			wid := createWorkflow(t, f, "Admission workflow", "inv-gate-wf", []any{invocationVariable("topic")})
			values := core.Object{"repository": "ora-space/cloud", "branch": "main", "topic": "auth"}
			status, code := 400, ""
			if scenario != "unpublished" {
				values["snapshotId"] = publishWorkflow(t, f, wid, "inv-gate-pub").S("id")
			} else {
				code = "workflow_no_published_snapshot"
			}
			switch scenario {
			case "wrong-workflow-snapshot":
				other := createWorkflow(t, f, "Other workflow", "inv-gate-other", nil)
				values["snapshotId"] = publishWorkflow(t, f, other, "inv-gate-other-pub").S("id")
				status = 404
			case "foreign-snapshot":
				other, err := f.store.Bootstrap(context.Background(), "Other tenant", "corp", "inv-other", "Other")
				must(t, err)
				tenant, subject := f.tid, f.user.Subject
				f.tid, f.user.Subject = other.S("tenantId"), "inv-other"
				foreign := createWorkflow(t, f, "Foreign workflow", "inv-gate-foreign", nil)
				values["snapshotId"] = publishWorkflow(t, f, foreign, "inv-gate-foreign-pub").S("id")
				f.tid, f.user.Subject = tenant, subject
				status = 404
			case "missing-input":
				delete(values, "topic")
				code = "required_field_missing"
			case "invalid-input-type":
				values["topic"] = 42
				code = "invalid_field_value"
			}
			iid := f.call("POST", f.path("/issues"), core.Object{"title": "Rejected start"}, "inv-gate-issue", 200).O("resource").S("id")
			it := workflowComment(t, f, wid, iid, "inv-gate-comment", "run")
			activities := f.scalar("SELECT count(*) FROM issue_activities WHERE issue_id=$1", iid)
			refusal := f.call("POST", confirmPath(f, iid, it.S("id")), core.Object{"values": values}, "inv-gate-confirm", status)
			if code != "" && refusal.S("code") != code {
				t.Fatalf("refused start code = %q, want %q: %v", refusal.S("code"), code, refusal)
			}
			assertUnclaimedWorkflowInvocation(t, f, iid, it.S("id"), activities)
		})
	}
}

func TestWorkflowConfirmFreezesHistoricalSchemaAndInput(t *testing.T) {
	f := setup(t)
	graph := func(variables []any) core.Object {
		out := startInputGraph(variables)
		out["globalVariables"] = []any{core.Object{"name": "review.mode", "valueType": "string", "value": "default"}}
		return out
	}
	wid := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Historical workflow", "graph": graph([]any{invocationVariable("topic")}),
	}, "inv-old-wf", 200).O("resource").S("id")
	older := publishWorkflow(t, f, wid, "inv-old-pub-1")
	edit := func(variables []any, zoom float64) {
		t.Helper()
		live := f.call("GET", f.path("/workflows/"+wid), nil, "", 200)
		updated := graph(variables)
		updated["viewport"] = core.Object{"x": 0, "y": 0, "zoom": zoom}
		f.call("PUT", f.path("/workflows/"+wid), core.Object{
			"graph": updated, "version": live.N("version"),
		}, "", 200)
	}
	// A newer graph with the same fields keeps the live-form compatibility gate satisfied.
	edit([]any{invocationVariable("topic")}, 2)
	newer := publishWorkflow(t, f, wid, "inv-old-pub-2")
	if reflect.DeepEqual(older.O("graph"), newer.O("graph")) {
		t.Fatal("historical snapshot test needs distinct published definitions")
	}
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Historical start"}, "inv-old-issue", 200).O("resource").S("id")
	it := workflowComment(t, f, wid, iid, "inv-old-comment", "run old version")
	created := f.call("POST", confirmPath(f, iid, it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "branch": "main", "snapshotId": older.S("id"), "topic": "auth", "review.mode": "strict",
	}}, "inv-old-confirm", 200).O("resource")
	ref := created.S("workflowInvocationRef")
	if ref == "" {
		t.Fatal("historical confirm did not link a WorkflowRun")
	}
	path := f.path("/workflows/" + wid + "/runs/" + ref)
	before := f.call("GET", path, nil, "", 200)
	if before.S("snapshotId") != older.S("id") || created.O("input").S("workflowSnapshotId") != older.S("id") {
		t.Fatalf("historical snapshot was not selected: issue=%v workflow=%v", created, before)
	}
	if !reflect.DeepEqual(before.O("input"), core.Object{"topic": "auth", "review.mode": "strict"}) {
		t.Fatalf("execution input must contain selected-schema variables only: %v", before.O("input"))
	}
	if !reflect.DeepEqual(before.O("definitionSnapshot"), older.O("graph")) {
		t.Fatalf("definition does not match selected frozen document: %v", before)
	}
	edit([]any{invocationVariable("replacement")}, 3)
	publishWorkflow(t, f, wid, "inv-old-pub-3")
	after := f.call("GET", path, nil, "", 200)
	for _, field := range []string{"snapshotId", "input", "definitionSnapshot"} {
		if !reflect.DeepEqual(after[field], before[field]) {
			t.Fatalf("editing and republishing changed frozen %s: before=%v after=%v", field, before[field], after[field])
		}
	}
}

func TestWorkflowConfirmInvocationIdempotency(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Replay workflow", "inv-idem-wf", nil)
	publishWorkflow(t, f, wid, "inv-idem-pub")
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Replay start"}, "inv-idem-issue", 200).O("resource").S("id")
	it := workflowComment(t, f, wid, iid, "inv-idem-comment", "run")
	body := core.Object{"values": core.Object{"repository": "ora-space/cloud", "branch": "main"}}
	path := confirmPath(f, iid, it.S("id"))
	first := f.call("POST", path, body, "inv-idem-confirm", 200)
	replay := f.call("POST", path, body, "inv-idem-confirm", 200)
	if !reflect.DeepEqual(first, replay) {
		t.Fatalf("same-key confirm did not replay its original response: first=%v replay=%v", first, replay)
	}
	if first.O("resource").S("workflowInvocationRef") == "" {
		t.Fatal("replayed response has no WorkflowRun identity")
	}
	refusal := f.call("POST", path, body, "inv-idem-second", 409)
	if refusal.S("code") != "interaction_already_confirmed" {
		t.Fatalf("second confirm fault = %v", refusal)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iid); n != 1 {
		t.Fatalf("replay created %d IssueRuns", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wid); n != 1 {
		t.Fatalf("replay created %d WorkflowRuns", n)
	}
}

func TestWorkflowConfirmConcurrentInvocationCreatesExactlyOnePair(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Concurrent workflow", "inv-race-wf", nil)
	publishWorkflow(t, f, wid, "inv-race-pub")
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Concurrent start"}, "inv-race-issue", 200).O("resource").S("id")
	it := workflowComment(t, f, wid, iid, "inv-race-comment", "run")
	path := confirmPath(f, iid, it.S("id"))
	start := make(chan struct{})
	var wg sync.WaitGroup
	bodies, statuses, errs := make([]core.Object, 2), make([]int, 2), make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			bodies[i], statuses[i], errs[i] = callRaw(f, "POST", path, core.Object{"values": core.Object{
				"repository": "ora-space/cloud", "branch": "main",
			}}, "inv-race-"+uuid.NewString())
		}(i)
	}
	close(start)
	wg.Wait()
	var accepted core.Object
	for i := range 2 {
		must(t, errs[i])
		if statuses[i] == 200 {
			accepted = bodies[i].O("resource")
		} else if bodies[i].S("code") != "interaction_already_confirmed" {
			t.Fatalf("concurrent refusal = %v", bodies[i])
		}
	}
	sort.Ints(statuses)
	if !reflect.DeepEqual(statuses, []int{200, 409}) {
		t.Fatalf("concurrent statuses = %v, want [200 409]", statuses)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iid); n != 1 {
		t.Fatalf("concurrent confirm created %d IssueRuns", n)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wid); n != 1 {
		t.Fatalf("concurrent confirm created %d WorkflowRuns", n)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs ir JOIN workflow_runs wr ON wr.id=ir.workflow_invocation_ref AND wr.tenant_id=ir.tenant_id JOIN issue_interactions ix ON ix.run_id=ir.id WHERE ix.id=$1 AND ir.id=$2 AND wr.id=$3", it.S("id"), accepted.S("id"), accepted.S("workflowInvocationRef")); n != 1 {
		t.Fatal("winning confirm did not atomically persist the interaction and same-tenant run pair")
	}
}

func TestManualWorkflowInvocationCreatesLinkedRun(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Manual workflow", "inv-manual-wf", []any{invocationVariable("topic")})
	snapshot := publishWorkflow(t, f, wid, "inv-manual-pub")
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Manual start"}, "inv-manual-issue", 200).O("resource").S("id")
	status, body := manualWorkflowRun(t, f, iid, wid, "inv-manual-start", core.Object{"topic": "auth"})
	if status != 200 {
		t.Fatalf("manual start status = %d: %v", status, body)
	}
	run := body.O("resource")
	if run.S("workflowInvocationRef") == "" {
		t.Fatal("manual workflow IssueRun has no WorkflowRun identity")
	}
	linked := f.call("GET", f.path("/workflows/"+wid+"/runs/"+run.S("workflowInvocationRef")), nil, "", 200)
	if linked.S("snapshotId") != snapshot.S("id") || run.O("input").S("workflowSnapshotId") != snapshot.S("id") {
		t.Fatalf("manual run pair froze different snapshots: issue=%v workflow=%v", run, linked)
	}
	if linked.O("input").S("topic") != "auth" {
		t.Fatalf("manual WorkflowRun lost its execution input: %v", linked)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs ir JOIN workflow_runs wr ON wr.id=ir.workflow_invocation_ref AND wr.tenant_id=ir.tenant_id WHERE ir.issue_id=$1", iid); n != 1 {
		t.Fatalf("manual start created %d valid linked pairs, want 1", n)
	}
}
