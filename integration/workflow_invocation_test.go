package integration

import (
	"context"
	"reflect"
	"sort"
	"strings"
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
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Linked execution", "projectRef": runProject(t, f, "inv-link-project")}, "inv-link-issue", 200).O("resource").S("id")
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
			iid := f.call("POST", f.path("/issues"), core.Object{"title": "Rejected start", "projectRef": runProject(t, f, "inv-gate-project")}, "inv-gate-issue", 200).O("resource").S("id")
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
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Historical start", "projectRef": runProject(t, f, "inv-old-project")}, "inv-old-issue", 200).O("resource").S("id")
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
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Replay start", "projectRef": runProject(t, f, "inv-idem-project")}, "inv-idem-issue", 200).O("resource").S("id")
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
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Concurrent start", "projectRef": runProject(t, f, "inv-race-project")}, "inv-race-issue", 200).O("resource").S("id")
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
	iid := f.call("POST", f.path("/issues"), core.Object{"title": "Manual start", "projectRef": runProject(t, f, "inv-manual-project")}, "inv-manual-issue", 200).O("resource").S("id")
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

// TestManualWorkflowInvocationCarriesTheKickoff pins that the manual entry treats the submitted
// reserved `input` key as the run's kickoff. The manual entry has no form: its input object *is* the
// execution input, so a kickoff it names has to survive the form-to-execution projection instead of
// being dropped as an undeclared variable — the Start variable pool resolves [start, input] from it,
// and dropping it would silently evaluate the run against the authored instruction.
func TestManualWorkflowInvocationCarriesTheKickoff(t *testing.T) {
	f := setup(t)
	graph := startPromptGraph(nil, "authored prompt")
	graph["nodes"] = append(graph["nodes"].([]any), core.Object{
		"id": "output-1", "type": "workflow", "position": core.Object{"x": 300, "y": 0},
		"data": core.Object{"kind": "output", "title": "Done"},
	})
	graph["edges"] = []any{core.Object{"id": "output-edge", "source": "start-1", "target": "output-1"}}
	wid := f.call("POST", f.path("/workflows"), core.Object{"name": "Manual kickoff", "graph": graph}, "inv-manual-kickoff-wf", 200).O("resource").S("id")
	snapshot := publishWorkflow(t, f, wid, "inv-manual-kickoff-pub")
	iid := launchIssue(t, f, runProject(t, f, "inv-manual-kickoff-project"), "Manual kickoff", "inv-manual-kickoff-issue").S("id")

	status, body := manualWorkflowRun(t, f, iid, wid, "inv-manual-kickoff-start", core.Object{"input": "review the diff"})
	if status != 200 {
		t.Fatalf("manual start status = %d: %v", status, body)
	}
	runID := body.O("resource").S("workflowInvocationRef")
	linked := f.call("GET", f.path("/workflows/"+wid+"/runs/"+runID), nil, "", 200)
	if linked.S("snapshotId") != snapshot.S("id") || linked.O("input").S("input") != "review the diff" {
		t.Fatalf("manual invocation lost its kickoff: %v", linked.O("input"))
	}
	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	linked = f.call("GET", f.path("/workflows/"+wid+"/runs/"+runID), nil, "", 200)
	if linked.S("status") != "succeeded" {
		t.Fatalf("manual invocation did not complete: %v", linked)
	}
	if got := linked.O("nodeStates").O("start-1").O("output").S("input"); got != "review the diff" {
		t.Fatalf("start output input = %q; want the submitted kickoff", got)
	}

	// The reserved selector is string-only on every entry: a manual run may not hand the pool a type
	// the direct-run entry would refuse, or [start, input] would mean different things per entry.
	refusalStatus, refusal := manualWorkflowRun(t, f, iid, wid, "inv-manual-kickoff-bad", core.Object{"input": 7})
	if refusalStatus != 400 || refusal.S("code") != "invalid_field_value" || refusal.O("params").S("field") != "input" {
		t.Fatalf("non-string manual kickoff refusal = %d %v; want 400 invalid_field_value", refusalStatus, refusal)
	}
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wid); n != 1 {
		t.Fatalf("refused manual start left %d WorkflowRuns, want the one admitted run", n)
	}
}

// TestWorkflowConfirmReadsThePromptAsTheStartKickoff covers the third entry. The @ form's prompt
// field is prefilled with the Start node's authored instruction and is what the user edits to
// override it, so the value they confirm has to reach the Start variable pool. Otherwise a condition
// on [start, input] would decide the branch from the authored text while the text the user wrote sits
// unused in the interaction.
func TestWorkflowConfirmReadsThePromptAsTheStartKickoff(t *testing.T) {
	f := setup(t)
	graph := startPromptGraph([]any{invocationVariable("topic")}, "authored prompt")
	graph["nodes"] = append(graph["nodes"].([]any),
		core.Object{
			"id": "condition-1", "type": "workflow", "position": core.Object{"x": 300, "y": 0},
			"data": core.Object{"kind": "condition", "title": "Kickoff branch", "cases": []any{core.Object{
				"id": "literal", "logic": "and", "conditions": []any{core.Object{
					"variableSelector": []any{"start-1", "input"}, "operator": "equals", "value": "review the diff",
				}},
			}}},
		},
		core.Object{
			"id": "taken", "type": "workflow", "position": core.Object{"x": 600, "y": 0},
			"data": core.Object{"kind": "output", "title": "Taken"},
		},
		core.Object{
			"id": "fallback", "type": "workflow", "position": core.Object{"x": 600, "y": 200},
			"data": core.Object{"kind": "output", "title": "Fallback"},
		},
	)
	graph["edges"] = []any{
		core.Object{"id": "start-condition", "source": "start-1", "target": "condition-1"},
		core.Object{"id": "taken-edge", "source": "condition-1", "target": "taken", "sourceHandle": "literal"},
		core.Object{"id": "fallback-edge", "source": "condition-1", "target": "fallback"},
	}
	wid, _, confirmed := confirmInvocationSceneWith(t, f, graph, core.Object{
		"repository": "ora-space/cloud", "branch": "main", "topic": "auth", "prompt": "review the diff",
	}, "inv-kickoff-confirm")
	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	workflow := f.call("GET", f.path("/workflows/"+wid+"/runs/"+confirmed.S("workflowInvocationRef")), nil, "", 200)
	if workflow.S("status") != "succeeded" {
		t.Fatalf("confirmed run did not complete: %v", workflow)
	}
	// The kickoff is frozen onto the run row at confirm time, not re-read from the interaction later.
	if workflow.O("input").S("input") != "review the diff" {
		t.Fatalf("WorkflowRun input lost the confirmed prompt: %v", workflow.O("input"))
	}
	if got := workflow.O("nodeStates").O("start-1").O("output").S("input"); got != "review the diff" {
		t.Fatalf("start output input = %q; want the confirmed prompt", got)
	}
	if branch := workflow.O("nodeStates").O("condition-1").O("output").S("selectedBranch"); branch != "literal" {
		t.Fatalf("condition selected %q; want literal — [start, input] did not read the confirmed prompt", branch)
	}
}

// TestWorkflowConfirmDeclaredPromptStaysAVariable keeps the boundary the platform-field rule draws:
// a workflow declaring its own `prompt` variable owns the key, the platform injects no prompt field,
// and the value the user gives is that variable's rather than the kickoff. The run must then fall back
// to the authored instruction, which is what a form that named no kickoff means.
func TestWorkflowConfirmDeclaredPromptStaysAVariable(t *testing.T) {
	f := setup(t)
	graph := startPromptGraph([]any{invocationVariable("prompt")}, "authored prompt")
	graph["nodes"] = append(graph["nodes"].([]any), core.Object{
		"id": "output-1", "type": "workflow", "position": core.Object{"x": 300, "y": 0},
		"data": core.Object{"kind": "output", "title": "Done"},
	})
	graph["edges"] = []any{core.Object{"id": "output-edge", "source": "start-1", "target": "output-1"}}
	wid, _, confirmed := confirmInvocationSceneWith(t, f, graph, core.Object{
		"repository": "ora-space/cloud", "branch": "main", "prompt": "the author's own parameter",
	}, "inv-declared-prompt")
	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	workflow := f.call("GET", f.path("/workflows/"+wid+"/runs/"+confirmed.S("workflowInvocationRef")), nil, "", 200)
	if workflow.S("status") != "succeeded" {
		t.Fatalf("confirmed run did not complete: %v", workflow)
	}
	if _, ok := workflow.O("input")["input"]; ok {
		t.Fatalf("declared prompt became the kickoff: %v", workflow.O("input"))
	}
	output := workflow.O("nodeStates").O("start-1").O("output")
	if output.S("prompt") != "the author's own parameter" {
		t.Fatalf("declared prompt variable lost its value: %v", output)
	}
	if output.S("input") != "authored prompt" {
		t.Fatalf("start output input = %q; want the authored instruction", output.S("input"))
	}
}

// TestWorkflowConfirmDisabledPromptFallsBackToTheAuthoredInstruction pins the second authored
// fallback the platform-field rule names: when the author's `launchFields` turn the prompt field
// off, the form never asks for it, no submission can carry its key, and the run must use the
// authored instruction rather than an empty kickoff. Projection, validation and kickoff ownership
// have to read the same declaration: a run that took `prompt` anyway would turn a field the author
// withdrew into the kickoff.
func TestWorkflowConfirmDisabledPromptFallsBackToTheAuthoredInstruction(t *testing.T) {
	f := setup(t)
	graph := startPromptGraph([]any{invocationVariable("topic")}, "authored prompt")
	graph["launchFields"] = []any{core.Object{"key": "prompt", "enabled": false}}
	wid, iid, confirmed := confirmInvocationSceneWith(t, f, graph, core.Object{
		"repository": "ora-space/cloud", "branch": "main", "topic": "auth",
	}, "inv-disabled-prompt")

	// The descriptor the user answered is the proof the field was withdrawn; the run assertions below
	// only mean the fallback if the withdrawn field was also the only place a kickoff could come from.
	if _, ok := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iid), nil, "", 200))["prompt"]; ok {
		t.Fatalf("a withdrawn prompt field was still projected")
	}

	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	run := f.call("GET", f.path("/workflows/"+wid+"/runs/"+confirmed.S("workflowInvocationRef")), nil, "", 200)
	if run.S("status") != "succeeded" {
		t.Fatalf("confirmed run did not complete: %v", run)
	}
	if _, ok := run.O("input")["input"]; ok {
		t.Fatalf("a withdrawn prompt became the kickoff: %v", run.O("input"))
	}
	if got := run.O("nodeStates").O("start-1").O("output").S("input"); got != "authored prompt" {
		t.Fatalf("start output input = %q; want the authored instruction", got)
	}
}

// TestWorkflowConfirmReadsTheKickoffFromTheChosenVersion pins which document decides whether the
// submitted prompt is the kickoff: the version the run executes, not the live draft. A confirm
// renders the form, validates the submission and projects the execution input against the selected
// version, so the live draft — the ordinary "edited after the last publish, not yet published"
// state — must not decide what the user's answer means. The two directions fail differently: a draft
// that newly declares `prompt` would silently drop the kickoff the user wrote, and a version that owns
// `prompt` while the draft has since dropped it would turn an author variable into a kickoff.
func TestWorkflowConfirmReadsTheKickoffFromTheChosenVersion(t *testing.T) {
	f := setup(t)
	// graphWithOutput is startPromptGraph plus a draining output node: the run has to complete before
	// the Start node's output, where the variable pool's kickoff resolution lands, can be read.
	graphWithOutput := func(variables []any) core.Object {
		graph := startPromptGraph(variables, "authored prompt")
		graph["nodes"] = append(graph["nodes"].([]any), core.Object{
			"id": "output-1", "type": "workflow", "position": core.Object{"x": 300, "y": 0},
			"data": core.Object{"kind": "output", "title": "Done"},
		})
		graph["edges"] = []any{core.Object{"id": "output-edge", "source": "start-1", "target": "output-1"}}
		return graph
	}
	// draftAndConfirm publishes `published`, overwrites the live document with `draft` without
	// publishing it, then confirms the published version and returns its finished run.
	draftAndConfirm := func(t *testing.T, published, draft, values core.Object, key string) core.Object {
		t.Helper()
		wid := f.call("POST", f.path("/workflows"), core.Object{"name": "Diverging kickoff " + key, "graph": published}, key+"-workflow", 200).O("resource").S("id")
		version := publishWorkflow(t, f, wid, key+"-publish")
		live := f.call("GET", f.path("/workflows/"+wid), nil, "", 200)
		f.call("PUT", f.path("/workflows/"+wid), core.Object{"graph": draft, "version": live.N("version")}, "", 200)
		iid := launchIssue(t, f, runProject(t, f, key+"-project"), "Diverging kickoff", key+"-issue").S("id")
		interaction := workflowComment(t, f, wid, iid, key+"-comment", "run this workflow")
		values["snapshotId"] = version.S("id")
		confirmed := f.call("POST", confirmPath(f, iid, interaction.S("id")), core.Object{"values": values}, key+"-confirm", 200).O("resource")
		for range 2 {
			must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
		}
		run := f.call("GET", f.path("/workflows/"+wid+"/runs/"+confirmed.S("workflowInvocationRef")), nil, "", 200)
		if run.S("status") != "succeeded" {
			t.Fatalf("%s: confirmed run did not complete: %v", key, run)
		}
		return run
	}

	// The draft gained `prompt` as an author variable after version 1 was published. Version 1 still
	// shows the platform's prompt field, so the edit the user confirmed is version 1's kickoff.
	run := draftAndConfirm(t, graphWithOutput(nil), graphWithOutput([]any{invocationVariable("prompt")}), core.Object{
		"repository": "ora-space/cloud", "branch": "main", "prompt": "review the diff",
	}, "inv-version-kickoff")
	if run.O("input").S("input") != "review the diff" {
		t.Fatalf("the draft's own prompt declaration dropped version 1's kickoff: %v", run.O("input"))
	}
	if got := run.O("nodeStates").O("start-1").O("output").S("input"); got != "review the diff" {
		t.Fatalf("start output input = %q; want the prompt the user confirmed against version 1", got)
	}

	// The mirror: version 1 owns `prompt` as its variable and the draft no longer declares it. The form
	// the user answered asked for an author field, so the answer is that variable's — version 1 runs
	// with the authored kickoff, not with the parameter.
	run = draftAndConfirm(t, graphWithOutput([]any{invocationVariable("prompt")}), graphWithOutput(nil), core.Object{
		"repository": "ora-space/cloud", "branch": "main", "prompt": "the author's own parameter",
	}, "inv-version-declared")
	if _, ok := run.O("input")["input"]; ok {
		t.Fatalf("version 1's author variable became the kickoff: %v", run.O("input"))
	}
	output := run.O("nodeStates").O("start-1").O("output")
	if output.S("prompt") != "the author's own parameter" || output.S("input") != "authored prompt" {
		t.Fatalf("version 1's declared prompt did not stay a variable: %v", output)
	}
}

// confirmInvocationScene crosses the public comment/form boundary. Unlike the fixture's direct
// workflow-run POST, Confirm leaves execution pending for the real scheduler used by these tests.
func confirmInvocationScene(t *testing.T, f *fixture, graph core.Object, key string) (wid, iid string, confirmed core.Object) {
	t.Helper()
	return confirmInvocationSceneWith(t, f, graph, core.Object{
		"repository": "ora-space/cloud", "branch": "main",
	}, key)
}

// confirmInvocationSceneWith is confirmInvocationScene with the submitted form values spelled out,
// for the cases whose subject is what the user answered rather than the document alone.
func confirmInvocationSceneWith(t *testing.T, f *fixture, graph, values core.Object, key string) (wid, iid string, confirmed core.Object) {
	t.Helper()
	wid = f.call("POST", f.path("/workflows"), core.Object{
		"name": "Scheduled invocation", "graph": graph,
	}, key+"-workflow", 200).O("resource").S("id")
	publishWorkflow(t, f, wid, key+"-publish")
	iid = launchIssue(t, f, runProject(t, f, key+"-project"), "Scheduled invocation", key+"-issue").S("id")
	interaction := workflowComment(t, f, wid, iid, key+"-comment", "run this workflow")
	confirmed = f.call("POST", confirmPath(f, iid, interaction.S("id")), core.Object{"values": values}, key+"-confirm", 200).O("resource")
	if confirmed.S("status") != "queued" || confirmed.S("workflowInvocationRef") == "" {
		t.Fatalf("Confirm must enqueue a linked invocation before scheduling: %v", confirmed)
	}
	return wid, iid, confirmed
}

// assertInvocationProjection reads both public run resources and the issue timeline, so a scheduler
// state change without its IssueRun projection or lifecycle activity cannot satisfy the test.
func assertInvocationProjection(t *testing.T, f *fixture, wid, iid string, confirmed core.Object, workflowStatus, issueStatus string, actions []string) (core.Object, core.Object) {
	t.Helper()
	workflow := f.call("GET", f.path("/workflows/"+wid+"/runs/"+confirmed.S("workflowInvocationRef")), nil, "", 200)
	var issueRun core.Object
	for _, candidate := range issueItems(f.call("GET", f.path("/issues/"+iid+"/runs"), nil, "", 200)) {
		if candidate.S("id") == confirmed.S("id") {
			issueRun = candidate
			break
		}
	}
	if workflow.S("status") != workflowStatus || issueRun.S("status") != issueStatus {
		t.Fatalf("scheduler projection disagrees: workflow=%v issue=%v; want %s/%s", workflow, issueRun, workflowStatus, issueStatus)
	}
	if issueRun.S("workflowInvocationRef") != workflow.S("id") || issueRun.S("executorId") != wid || issueRun.S("executorType") != "workflow" {
		t.Fatalf("projected IssueRun lost its execution identity: %v", issueRun)
	}
	if workflow.S("startedAt") == "" || issueRun.S("startedAt") != workflow.S("startedAt") || issueRun.S("completedAt") != workflow.S("finishedAt") {
		t.Fatalf("projected lifecycle timestamps disagree: workflow=%v issue=%v", workflow, issueRun)
	}
	if issueRun.S("failureReason") != workflow.S("error") || issueRun.S("error") != workflow.S("error") {
		t.Fatalf("projected failure reason disagrees: workflow=%v issue=%v", workflow, issueRun)
	}
	var got []string
	for _, item := range issueItems(f.call("GET", f.path("/issues/"+iid+"/timeline"), nil, "", 200)) {
		details := item.O("details")
		if item.S("kind") != "activity" || details.S("runId") != confirmed.S("id") {
			continue
		}
		got = append(got, item.S("action"))
		if details.S("executorType") != "workflow" || details.S("executorId") != wid {
			t.Fatalf("activity lost its executor identity: %v", item)
		}
		if item.S("action") != "run.enqueued" && details.S("workflowInvocationRef") != workflow.S("id") {
			t.Fatalf("scheduler activity lost its workflow invocation identity: %v", item)
		}
		if item.S("action") == "run.failed" && details.S("failureReason") != workflow.S("error") {
			t.Fatalf("failure activity lost the authoritative reason: %v", item)
		}
	}
	if !reflect.DeepEqual(got, actions) {
		t.Fatalf("invocation lifecycle activities = %v, want %v", got, actions)
	}
	return workflow, issueRun
}

func TestWorkflowConfirmSchedulerProjectsRunningOnce(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	wid, iid, confirmed := confirmInvocationScene(t, f, runGraph(), "inv-scheduler-running")
	// The Project is still provisioning: session startup waits without Node or cluster execution.
	// A second advance exercises the running-state retry, which must not emit another run.started.
	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	workflow, _ := assertInvocationProjection(t, f, wid, iid, confirmed, "running", "running", []string{"run.enqueued", "run.started"})
	if len(workflow.O("nodeStates")) != 0 || workflow.S("finishedAt") != "" {
		t.Fatalf("waiting invocation fabricated committed nodes or terminal evidence: %v", workflow)
	}
}

func TestWorkflowConfirmSchedulerCompletesAndAllowsNextInvocation(t *testing.T) {
	f := setup(t)
	graph := startInputGraph(nil)
	graph["nodes"] = append(graph["nodes"].([]any), core.Object{
		"id": "output-1", "type": "workflow", "position": core.Object{"x": 300, "y": 0},
		"data": core.Object{"kind": "output", "title": "Done"},
	})
	graph["edges"] = []any{core.Object{"id": "output-edge", "source": "start-1", "target": "output-1"}}
	wid, iid, confirmed := confirmInvocationScene(t, f, graph, "inv-scheduler-completed")
	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	workflow, _ := assertInvocationProjection(t, f, wid, iid, confirmed, "succeeded", "completed", []string{"run.enqueued", "run.started", "run.completed"})
	for _, node := range []string{"start-1", "output-1"} {
		if workflow.O("nodeStates").O(node).S("status") != "succeeded" {
			t.Fatalf("completed invocation did not execute node %s: %v", node, workflow)
		}
	}
	if workflow.S("finishedAt") == "" {
		t.Fatal("completed invocation has no terminal timestamp")
	}

	// workflowComment returns the first form, so find the fresh unclaimed form explicitly on this
	// same Issue. Terminal projection must release the pending-executor dedup for this Confirm.
	f.call("POST", f.path("/issues/"+iid+"/comments"), core.Object{
		"body": "run this workflow again", "targets": []any{core.Object{"type": "workflow", "id": wid}},
	}, "inv-scheduler-completed-next-comment", 200)
	var nextInteraction core.Object
	for _, interaction := range issueItems(f.call("GET", f.path("/issues/"+iid+"/interactions"), nil, "", 200)) {
		if interaction.S("mode") == "form" && interaction.S("runId") == "" {
			nextInteraction = interaction
			break
		}
	}
	if nextInteraction == nil {
		t.Fatal("second workflow comment did not create an unclaimed form")
	}
	next := f.call("POST", confirmPath(f, iid, nextInteraction.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "branch": "main",
	}}, "inv-scheduler-completed-next-confirm", 200).O("resource")
	if next.S("status") != "queued" || next.S("id") == confirmed.S("id") || next.S("workflowInvocationRef") == "" || next.S("workflowInvocationRef") == confirmed.S("workflowInvocationRef") {
		t.Fatalf("second Confirm did not create a new linked queued invocation: first=%v next=%v", confirmed, next)
	}
}

func TestWorkflowConfirmSchedulerProjectsConditionFailure(t *testing.T) {
	f := setup(t)
	graph := startInputGraph(nil)
	// input is a declared Start root and is always a string. Publishing admits the selector, but
	// traversing a nested object path on that string deterministically fails during real execution.
	graph["nodes"] = append(graph["nodes"].([]any), core.Object{
		"id": "condition-1", "type": "workflow", "position": core.Object{"x": 300, "y": 0},
		"data": core.Object{"kind": "condition", "title": "Invalid nested input", "cases": []any{core.Object{
			"id": "matched", "logic": "and", "conditions": []any{core.Object{
				"variableSelector": []any{"start-1", "input", "missing"}, "operator": "equals", "value": "value",
			}},
		}}},
	})
	graph["edges"] = []any{core.Object{"id": "condition-edge", "source": "start-1", "target": "condition-1"}}
	wid, iid, confirmed := confirmInvocationScene(t, f, graph, "inv-scheduler-failed")
	for range 2 {
		must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))
	}
	workflow, _ := assertInvocationProjection(t, f, wid, iid, confirmed, "failed", "failed", []string{"run.enqueued", "run.started", "run.message", "run.failed"})
	failed := workflow.O("nodeStates").O("condition-1")
	if failed.S("status") != "failed" || !strings.Contains(workflow.S("error"), "start-1.input.missing") || failed.S("error") != workflow.S("error") || workflow.S("finishedAt") == "" {
		t.Fatalf("condition failure did not preserve its node reason and terminal evidence: %v", workflow)
	}
}

// TestWorkflowConfirmSchedulerProjectsCancelBeforeStart links a Confirm-created IssueRun and cancels
// it before the scheduler claims the run: the run never starts, so the terminal projection must
// record cancelled without fabricating a start timestamp or a run.started activity.
func TestWorkflowConfirmSchedulerProjectsCancelBeforeStart(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	wid, iid, confirmed := confirmInvocationScene(t, f, runGraph(), "inv-scheduler-cancelled")
	runID := confirmed.S("workflowInvocationRef")

	// Cancel while the run is still pending: it must settle cancelled on the next scheduler pass
	// without ever claiming, so neither the run nor its IssueRun carries a start.
	out := f.call("POST", f.path("/workflows/"+wid+"/runs/"+runID+"/cancel"), core.Object{}, "inv-scheduler-cancelled-cancel", 202)
	if out.S("cancelRequestedAt") == "" {
		t.Fatalf("cancel request reported no recorded instant: %v", out)
	}
	must(t, f.store.AdvanceWorkflowRunsOnce(t.Context()))

	workflow := f.call("GET", f.path("/workflows/"+wid+"/runs/"+runID), nil, "", 200)
	if workflow.S("status") != "cancelled" || workflow.S("startedAt") != "" || workflow.S("finishedAt") == "" {
		t.Fatalf("cancel-before-start settled %q started=%q finished=%q; want cancelled with no start and a terminal finish",
			workflow.S("status"), workflow.S("startedAt"), workflow.S("finishedAt"))
	}
	var issueRun core.Object
	for _, candidate := range issueItems(f.call("GET", f.path("/issues/"+iid+"/runs"), nil, "", 200)) {
		if candidate.S("id") == confirmed.S("id") {
			issueRun = candidate
			break
		}
	}
	if issueRun.S("status") != "cancelled" || issueRun.S("startedAt") != "" || issueRun.S("completedAt") != workflow.S("finishedAt") {
		t.Fatalf("projected IssueRun status=%q started=%q completed=%q; want cancelled with no start and the workflow finish",
			issueRun.S("status"), issueRun.S("startedAt"), issueRun.S("completedAt"))
	}
	var got []string
	for _, item := range issueItems(f.call("GET", f.path("/issues/"+iid+"/timeline"), nil, "", 200)) {
		details := item.O("details")
		if item.S("kind") != "activity" || details.S("runId") != confirmed.S("id") {
			continue
		}
		got = append(got, item.S("action"))
	}
	if !reflect.DeepEqual(got, []string{"run.enqueued", "run.cancelled"}) {
		t.Fatalf("cancel-before-start lifecycle activities = %v, want [run.enqueued run.cancelled]", got)
	}
}
