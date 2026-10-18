package integration

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// versionFormPath is the read under test: the `@` form projected from one named version rather than
// from the live document.
func versionFormPath(f *fixture, wid, sid string) string {
	return f.path("/workflows/" + wid + "/snapshots/" + sid + "/form")
}

// TestWorkflowSnapshotFormProjectsTheChosenVersion covers the snapshot-scoped form read (Issue collaboration D7): the
// descriptor is projected from the version named in the path, which is what lets a caller pick a
// version before rendering it. The test tells the two documents apart by making them disagree — the
// live document is edited and published again, so a projection of it and a projection of the first
// version cannot share a field set.
func TestWorkflowSnapshotFormProjectsTheChosenVersion(t *testing.T) {
	f := setup(t)
	// The first version declares `notes` and a kickoff prompt; the live document then moves on to
	// `scope` and a different prompt, and is published as the second version.
	wid := createPromptWorkflow(t, f, "Manual Run Workflow", "snap-form-wf", "Ship the release", []any{
		core.Object{"name": "notes", "displayName": "Release notes", "fieldType": "text-input", "valueType": "string"},
	})
	v1 := publishWorkflow(t, f, wid, "snap-form-pub-1")
	live := f.call("GET", f.path("/workflows/"+wid), nil, "", 200)
	f.call("PUT", f.path("/workflows/"+wid), core.Object{
		"graph": startPromptGraph([]any{
			core.Object{"name": "scope", "displayName": "Review scope", "fieldType": "select", "valueType": "string", "options": []any{"changed-files", "full-repo"}},
		}, "Ship it differently"),
		"version": live.N("version"),
	}, "", 200)
	v2 := publishWorkflow(t, f, wid, "snap-form-pub-2")

	// The response names the version it was projected from: the same workflow has a different form per
	// version, so a projection that did not say which version decided the fields would be unusable by
	// the caller that chose one. formRef stays the workflow's identity on both reads — a version is not
	// a form.
	bare := f.call("GET", versionFormPath(f, wid, v1.S("id")), nil, "", 200)
	if bare.S("snapshotId") != v1.S("id") || bare.S("formRef") != wid || bare.S("title") != "Manual Run Workflow" {
		t.Fatalf("the projection does not name what it projected: %v", bare)
	}
	// Each version is projected as its own document, and the live read follows the live one.
	if keys := fieldKeys(orderedFormFields(t, bare)); !slices.Equal(keys, []string{"notes"}) {
		t.Fatalf("the first version was not projected from its own graph: %v", keys)
	}
	newer := f.call("GET", versionFormPath(f, wid, v2.S("id")), nil, "", 200)
	if keys := fieldKeys(orderedFormFields(t, newer)); !slices.Equal(keys, []string{"scope"}) {
		t.Fatalf("the second version was not projected from its own graph: %v", keys)
	}
	if keys := fieldKeys(orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+wid), nil, "", 200))); !slices.Equal(keys, []string{"scope"}) {
		t.Fatalf("the live form did not follow the edit: %v", keys)
	}

	// No issue context is the same non-degraded projection the live read serves without one: no platform
	// field at all.
	if _, ok := formFields(t, bare)["repository"]; ok {
		t.Fatalf("an issue-less projection grew platform fields: %v", bare)
	}

	// With an issue, the platform fields lead the form on this read exactly as they do on the live one,
	// and the ones the workflow owns come from the frozen graph: the prompt default is the one the
	// *published* version carries, not the one the live document has since been given.
	project := f.create("snap-form-project").O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Manual run", "projectRef": project.S("id"),
	}, "snap-form-iss", 200).O("resource")
	withIssue := f.call("GET", versionFormPath(f, wid, v1.S("id"))+"?issueId="+iss.S("id"), nil, "", 200)
	want := []string{"repository", "branch", "snapshotId", "prompt", "context_refs", "notes"}
	if keys := fieldKeys(orderedFormFields(t, withIssue)); !slices.Equal(keys, want) {
		t.Fatalf("platform fields did not lead the projected form: %v, want %v", keys, want)
	}
	byKey := formFields(t, withIssue)
	if byKey["repository"]["defaultValue"] != project.S("repositoryUrl") || !byKey["repository"].B("required") {
		t.Fatalf("repository field wrong: %v", byKey["repository"])
	}
	if p := byKey["prompt"]; p["defaultValue"] != "Ship the release" {
		t.Fatalf("the prompt default did not come from the projected version: %v", p)
	}

	// The version picker is a property of the workflow, not of the projection: both reads offer every
	// published version, newest first and newest by default, so a client's picker behaves identically
	// whichever form it rendered.
	liveForm := f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200)
	picker := byKey["snapshotId"]
	if picker.S("type") != "select" || picker.B("required") || picker["defaultValue"] != v2.S("id") {
		t.Fatalf("version field wrong: %v", picker)
	}
	options, _ := picker["options"].([]any)
	if len(options) != 2 || options[0].(map[string]any)["value"] != v2.S("id") || options[1].(map[string]any)["value"] != v1.S("id") {
		t.Fatalf("the projected form offered %v, want both publishes newest first", options)
	}
	if liveKeys := fieldKeys(orderedFormFields(t, liveForm)); !slices.Equal(liveKeys, []string{"repository", "branch", "snapshotId", "prompt", "context_refs", "scope"}) {
		t.Fatalf("the live read changed shape: %v", liveKeys)
	}
	if livePicker := formFields(t, liveForm)["snapshotId"]; livePicker["defaultValue"] != picker["defaultValue"] {
		t.Fatalf("the picker default differs between the two reads: %v vs %v", livePicker, picker)
	}

	// An issue that cannot be resolved is a 404 here too, never a projection that silently drops the
	// fields the caller asked to have tailored.
	f.call("GET", versionFormPath(f, wid, v1.S("id"))+"?issueId="+uuid.NewString(), nil, "", 404)
	f.call("GET", versionFormPath(f, wid, v1.S("id"))+"?issueId=not-a-uuid", nil, "", 404)
}

// TestWorkflowSnapshotFormScoping covers what this read may be pointed at: one tenant's workflow and
// one of *its* snapshots. Every other cursor is the same 404 the direct-run entry answers for the same
// situation, fault code included — the two routes resolve the version through one scoped read, and the
// test asserts the agreement rather than each answer separately.
func TestWorkflowSnapshotFormScoping(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Scoped Workflow", "snap-scope-wf", nil)
	v1 := publishWorkflow(t, f, wid, "snap-scope-pub")
	other := createWorkflow(t, f, "Other Workflow", "snap-scope-wf-2", nil)
	otherV1 := publishWorkflow(t, f, other, "snap-scope-pub-2")

	// The foreign tenant is a real second tenant with the fixture user as its administrator, so the
	// only thing refusing these reads is the cursor's own scope.
	foreign := f.moreTenant()
	foreignWid := f.call("POST", "/api/v1/tenants/"+foreign+"/workflows", core.Object{
		"name": "Foreign Workflow", "graph": startInputGraph(nil),
	}, "snap-scope-foreign-wf", 200).O("resource").S("id")
	foreignV1 := f.call("POST", "/api/v1/tenants/"+foreign+"/workflows/"+foreignWid+"/publish", core.Object{}, "snap-scope-foreign-pub", 200).O("resource")

	for _, c := range []struct{ name, path string }{
		{"another workflow's snapshot", versionFormPath(f, wid, otherV1.S("id"))},
		{"unknown snapshot", versionFormPath(f, wid, uuid.NewString())},
		{"malformed snapshot id", versionFormPath(f, wid, "not-a-uuid")},
		{"another tenant's snapshot", versionFormPath(f, wid, foreignV1.S("id"))},
		{"unknown workflow", versionFormPath(f, uuid.NewString(), v1.S("id"))},
		{"another tenant's workflow", versionFormPath(f, foreignWid, foreignV1.S("id"))},
	} {
		got := f.call("GET", c.path, nil, "", 404)
		if got.S("code") != "not_found" {
			t.Fatalf("%s: code = %q, want not_found: %v", c.name, got.S("code"), got)
		}
	}
	// The direct-run entry answers the same situation the same way.
	refused := f.call("POST", f.path("/workflows/"+wid+"/runs"), core.Object{"snapshotId": otherV1.S("id")}, "snap-scope-run", 404)
	if refused.S("code") != "not_found" {
		t.Fatalf("the direct-run entry answered %v, so the two routes disagree", refused)
	}

	// Archiving the workflow hides its history from both reads at once: the version-scoped form is not a
	// way around the workflow's own visibility.
	live := f.call("GET", f.path("/workflows/"+wid), nil, "", 200)
	f.call("DELETE", f.path("/workflows/"+wid), core.Object{"version": live.N("version")}, "snap-scope-archive", 200)
	if got := f.call("GET", versionFormPath(f, wid, v1.S("id")), nil, "", 404); got.S("code") != "not_found" {
		t.Fatalf("an archived workflow's form answered %v", got)
	}
	if got := f.call("POST", f.path("/workflows/"+wid+"/runs"), core.Object{"snapshotId": v1.S("id")}, "snap-scope-run-2", 404); got.S("code") != "not_found" {
		t.Fatalf("an archived workflow's run answered %v", got)
	}
}

// TestWorkflowSnapshotFormIsNotARunGate covers the half of the contract that is about what the read
// does *not* decide. The document below cannot be started — nobody has mapped the executor slot it
// references — and reading its frozen version's form still answers 200, because whether a run may
// start is the start gate's answer and a form is rendered before any of that is known.
func TestWorkflowSnapshotFormIsNotARunGate(t *testing.T) {
	f := setup(t)
	// The user's default model is complete, so nothing about the refusal below is about `default`.
	seedPersonalModel(t, f)
	wf := f.call("POST", f.path("/workflows"), core.Object{
		"name": "Unmapped slot flow",
		"graph": agentRunGraph("strong", false, []any{core.Object{
			"name": "strong", "description": "深度推理",
		}}),
	}, "snap-gate-wf", 200).O("resource")
	v1 := publishWorkflow(t, f, wf.S("id"), "snap-gate-pub")

	refused := f.call("POST", f.path("/workflows/"+wf.S("id")+"/runs"), core.Object{}, "snap-gate-run", 409)
	if gaps := slotGaps(t, refused); len(gaps) != 1 || gaps[0].S("slot") != "strong" || gaps[0].S("reason") != "slot_not_configured" {
		t.Fatalf("the start was not refused for the unmapped slot: %v", gaps)
	}

	// The read answers, names its version, and — this graph declaring no input variables — projects no
	// field at all.
	d := f.call("GET", versionFormPath(f, wf.S("id"), v1.S("id")), nil, "", 200)
	if d.S("snapshotId") != v1.S("id") {
		t.Fatalf("the projection does not name its version: %v", d)
	}
	if fields := orderedFormFields(t, d); len(fields) != 0 {
		t.Fatalf("an agent-only workflow projected fields: %v", fieldKeys(fields))
	}
	// Reading a form is not a start: nothing was created, and nothing was claimed.
	if n := f.scalar("SELECT count(*) FROM workflow_runs WHERE workflow_id=$1", wf.S("id")); n != 0 {
		t.Fatalf("reading the form left %d runs behind", n)
	}
}
