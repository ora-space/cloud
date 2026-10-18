package integration

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// invalidDescriptorProvider returns a descriptor that violates the Issue collaboration D7 invariants (a select with no
// options and a duplicated field key), to prove Issues fails closed instead of rendering it.
type invalidDescriptorProvider struct{ FormRef string }

func (p invalidDescriptorProvider) ResolveFormDescriptor(context.Context, string, string, core.IssueFormContext) (core.FormDescriptor, bool, error) {
	return core.FormDescriptor{
		FormRef: p.FormRef,
		Fields: []core.FormField{
			{Key: "scope", Label: "Scope", Type: "select", Required: true},
			{Key: "scope", Label: "Duplicate", Type: "text"},
		},
	}, true, nil
}

// startInputGraph is a graph whose Start node declares an input form. Cloud projects exactly those
// variables onto the @ form, so this graph IS the descriptor the picker renders — there is no
// separate registration step and no fixture identity to keep in sync.
func startInputGraph(variables []any) core.Object { return startPromptGraph(variables, "") }

// startPromptGraph is startInputGraph with the Start node's kickoff prompt set: it is the default the
// platform's prompt field is prefilled from. An empty prompt leaves the key off, the way a Start node
// nobody has typed into is stored.
func startPromptGraph(variables []any, prompt string) core.Object {
	data := core.Object{
		"kind": "start", "title": "开始", "description": "",
		"inputVariables": variables,
	}
	if prompt != "" {
		data["input"] = prompt
	}
	return core.Object{
		"nodes": []any{core.Object{
			"id": "start-1", "type": "workflow", "deletable": false,
			"position": core.Object{"x": 0, "y": 0},
			"data":     data,
		}},
		"edges":    []any{},
		"viewport": core.Object{"x": 0, "y": 0, "zoom": 1},
	}
}

// launchGraph is startInputGraph with the workflow's launch-field declaration in the envelope: the
// author's answer, carried in the document rather than registered anywhere, about which platform
// fields the @ form asks for and which of them it insists on (Issue collaboration D8).
func launchGraph(variables, launchFields []any) core.Object {
	graph := startInputGraph(variables)
	graph["launchFields"] = launchFields
	return graph
}

// orderedFormFields reads a descriptor response's fields in wire order: order is part of what the
// platform fields promise, so a test has to be able to see it.
func orderedFormFields(t *testing.T, d core.Object) []core.Object {
	t.Helper()
	raw, _ := d["fields"].([]any)
	out := []core.Object{}
	for _, item := range raw {
		field, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("field is not an object: %v", item)
		}
		out = append(out, core.Object(field))
	}
	return out
}

// formFields indexes a descriptor response by field key.
func formFields(t *testing.T, d core.Object) map[string]core.Object {
	t.Helper()
	out := map[string]core.Object{}
	for _, field := range orderedFormFields(t, d) {
		out[field.S("key")] = field
	}
	return out
}

// fieldKeys names the projected fields in wire order, for failure messages.
func fieldKeys(fields []core.Object) []string {
	out := []string{}
	for _, field := range fields {
		out = append(out, field.S("key"))
	}
	return out
}

// reviewVariables is the input form the review workflow publishes: a required text field, an optional
// text field with a default, two selects, and a boolean.
func reviewVariables() []any {
	return []any{
		core.Object{"name": "repository", "displayName": "Repository", "fieldType": "text-input", "valueType": "string", "required": true},
		core.Object{"name": "branch", "displayName": "Branch", "fieldType": "text-input", "valueType": "string", "value": "main"},
		core.Object{
			"name": "scope", "displayName": "Review scope", "fieldType": "select", "valueType": "string", "required": true,
			"options": []any{"current-issue", "changed-files", "full-repo"},
		},
		core.Object{
			"name": "severity", "displayName": "Severity", "fieldType": "select", "valueType": "string", "required": true,
			"options": []any{"low", "medium", "high"},
		},
		core.Object{"name": "includeDependencies", "displayName": "Include dependencies", "fieldType": "checkbox", "valueType": "boolean", "value": false},
	}
}

// createWorkflow publishes a workflow whose Start node declares `variables` and returns its id. The
// id is also the formRef the target advertises, because Cloud derives both from the same row.
func createWorkflow(t *testing.T, f *fixture, name, key string, variables []any) string {
	t.Helper()
	return f.call("POST", f.path("/workflows"), core.Object{
		"name": name, "description": name + " (test)", "graph": startInputGraph(variables),
	}, key, 200).O("resource").S("id")
}

// createPromptWorkflow publishes a workflow whose Start node carries a kickoff prompt alongside its
// input variables — the prompt the platform's optional prompt field is prefilled from.
func createPromptWorkflow(t *testing.T, f *fixture, name, key, prompt string, variables []any) string {
	t.Helper()
	return f.call("POST", f.path("/workflows"), core.Object{
		"name": name, "description": name + " (test)", "graph": startPromptGraph(variables, prompt),
	}, key, 200).O("resource").S("id")
}

// createLaunchWorkflow publishes a workflow that declares both an input form and a launch-field
// declaration — the author having answered the editor's `@` form fields dialog.
func createLaunchWorkflow(t *testing.T, f *fixture, name, key string, variables, launchFields []any) string {
	t.Helper()
	return f.call("POST", f.path("/workflows"), core.Object{
		"name": name, "description": name + " (test)", "graph": launchGraph(variables, launchFields),
	}, key, 200).O("resource").S("id")
}

// reviewWorkflow is the workflow every Form Mode test in this file drives.
func reviewWorkflow(t *testing.T, f *fixture) string {
	t.Helper()
	return createWorkflow(t, f, "Security Review Workflow", "wf-review", reviewVariables())
}

// publishWorkflow freezes a workflow's live graph into its next published version. A confirm executes
// a *published* version — the live document is a draft — so a test that drives one has to publish
// first, exactly as the author would.
func publishWorkflow(t *testing.T, f *fixture, wid, key string) core.Object {
	t.Helper()
	return f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{}, key, 200).O("resource")
}

// workflowComment posts a comment addressed at `wid` and returns the form interaction it created.
// Creating the interaction must NOT create a run.
func workflowComment(t *testing.T, f *fixture, wid, iid, key, body string) core.Object {
	t.Helper()
	f.call("POST", f.path("/issues/"+iid+"/comments"), core.Object{
		"body":    body,
		"targets": []any{core.Object{"type": "workflow", "id": wid}},
	}, key, 200)
	for _, it := range issueItems(f.call("GET", f.path("/issues/"+iid+"/interactions"), nil, "", 200)) {
		if it.S("mode") == "form" {
			return it
		}
	}
	t.Fatal("workflow comment did not create a form interaction")
	return nil
}

func confirmPath(f *fixture, iid, ixid string) string {
	return f.path("/issues/" + iid + "/interactions/" + ixid + "/confirm")
}

// refusedFields asserts a confirm was refused because the version it names no longer describes the form
// the client rendered, and returns the fields that refusal names. The fault is the point of the
// assertion: the same submission checked against the version's schema alone would be refused too, by a
// fault about the values — saying a field the user was shown is invalid, or that one they were never
// shown is missing.
func refusedFields(t *testing.T, refusal core.Object) []string {
	t.Helper()
	if got := refusal.S("code"); got != "workflow_version_fields_changed" {
		t.Fatalf("code = %q, want workflow_version_fields_changed: %v", got, refusal)
	}
	raw, _ := refusal.O("params")["fields"].([]any)
	out := []string{}
	for _, item := range raw {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("the refusal named a value that is not a field: %v", item)
		}
		out = append(out, text)
	}
	return out
}

// assistPath targets the stateless pre-confirm assist route: the form is a draft until the user
// confirms it, so assist must work without a persisted interaction.
func assistPath(f *fixture, iid string) string {
	return f.path("/issues/" + iid + "/collaboration/assist")
}

func workflowAssistBody(targetID string, values core.Object) core.Object {
	return core.Object{"targetId": targetID, "values": values}
}

// validValues is a complete, legal submission for the fixture descriptor.
func validValues() core.Object {
	return core.Object{"repository": "ora-space/cloud", "branch": "main", "scope": "full-repo", "severity": "high"}
}

// TestWorkflowCommentCreatesFormInteractionWithoutRun covers Issue collaboration D4/D9: selecting a workflow records
// a configured-but-unconfirmed interaction and produces no run, no dispatch and no execution.
func TestWorkflowCommentCreatesFormInteractionWithoutRun(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Security review"}, "wf-issue", 200).O("resource")

	it := workflowComment(t, f, wid, iss.S("id"), "wf-1", "@Security Review Workflow please review")
	if it.S("targetType") != "workflow" || it.S("targetId") != wid {
		t.Fatalf("form interaction target wrong: %v", it)
	}
	if it["runId"] != nil {
		t.Fatalf("form interaction must not carry a run: %v", it)
	}
	if len(it.O("input")) != 0 {
		t.Fatalf("form interaction input must start empty: %v", it)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("workflow comment created %d runs; selecting is not executing", n)
	}
	// Only the comment is on the timeline — no run activities.
	tl := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/timeline"), nil, "", 200))
	if len(tl) != 1 || tl[0].S("kind") != "comment" {
		t.Fatalf("unexpected timeline for an unconfirmed workflow interaction: %v", tl)
	}
}

// TestFormDescriptorProjection covers the read-only provider-backed descriptor route (Issue collaboration D7) and the
// target projection that advertises the formRef.
func TestFormDescriptorProjection(t *testing.T) {
	f := setup(t)
	review := reviewWorkflow(t, f)
	// A second workflow, whose Start node exercises a control the review form does not: a number with
	// a default. Its form must come out of its own graph, not out of the review workflow's.
	release := createWorkflow(t, f, "Release Readiness Workflow", "wf-release", []any{
		core.Object{"name": "version", "displayName": "Release version", "fieldType": "text-input", "valueType": "string", "required": true},
		core.Object{"name": "rolloutPercent", "displayName": "Rollout percent", "fieldType": "number", "valueType": "number", "required": true, "value": 10},
		core.Object{"name": "notifyOnCall", "displayName": "Notify on-call", "fieldType": "checkbox", "valueType": "boolean", "value": true},
		core.Object{"name": "rollbackPlan", "displayName": "Rollback plan", "fieldType": "paragraph", "valueType": "string"},
	})

	// The picker projection carries an opaque formRef for each workflow target, never the descriptor.
	advertised := map[string]string{}
	for _, target := range issueItems(f.call("GET", f.path("/collaboration/targets"), nil, "", 200)) {
		if target.S("type") != "workflow" {
			continue
		}
		advertised[target.S("id")] = target.O("interactionDescriptor").S("formRef")
		if _, embedded := target["fields"]; embedded {
			t.Fatalf("target list must not embed the form descriptor: %v", target)
		}
	}
	if advertised[review] != review {
		t.Fatalf("review workflow did not advertise its own id as formRef: %v", advertised)
	}
	if advertised[release] != release {
		t.Fatalf("release workflow did not advertise its own id as formRef: %v", advertised)
	}

	d := f.call("GET", f.path("/collaboration/forms/"+advertised[review]), nil, "", 200)
	if d.S("formRef") != review || d.S("title") != "Security Review Workflow" {
		t.Fatalf("descriptor wrong: %v", d)
	}
	if len(orderedFormFields(t, d)) == 0 {
		t.Fatalf("descriptor has no fields: %v", d)
	}
	byKey := formFields(t, d)
	if byKey["scope"].S("type") != "select" || byKey["severity"].S("type") != "select" {
		t.Fatalf("select fields wrong: %v", byKey)
	}
	if opts, _ := byKey["severity"]["options"].([]any); len(opts) != 3 {
		t.Fatalf("severity options wrong: %v", byKey["severity"])
	}
	if !byKey["repository"].B("required") || byKey["branch"].B("required") {
		t.Fatalf("required flags wrong: %v", byKey)
	}
	// A declared default survives the projection; the display name is the label.
	if byKey["branch"]["defaultValue"] != "main" {
		t.Fatalf("text default lost: %v", byKey["branch"])
	}
	if byKey["includeDependencies"]["defaultValue"] != false {
		t.Fatalf("boolean default lost: %v", byKey["includeDependencies"])
	}
	if byKey["repository"].S("label") != "Repository" {
		t.Fatalf("display name is not the label: %v", byKey["repository"])
	}

	// The second workflow's form is its own.
	otherByKey := formFields(t, f.call("GET", f.path("/collaboration/forms/"+release), nil, "", 200))
	if otherByKey["rolloutPercent"].S("type") != "number" || otherByKey["rolloutPercent"].N("defaultValue") != 10 {
		t.Fatalf("number field wrong: %v", otherByKey["rolloutPercent"])
	}
	if otherByKey["rollbackPlan"].S("type") != "textarea" {
		t.Fatalf("paragraph field wrong: %v", otherByKey["rollbackPlan"])
	}
	if _, leaked := otherByKey["repository"]; leaked {
		t.Fatalf("a workflow's form leaked into another's: %v", otherByKey)
	}

	// Unknown refs and cross-tenant guesses never leak: an unknown formRef is a typed 404.
	f.call("GET", f.path("/collaboration/forms/does-not-exist"), nil, "", 404)
	f.call("GET", f.path("/collaboration/forms/"+uuid.NewString()), nil, "", 404)
}

// TestFormDescriptorPlatformFieldsForAnIssue covers the issue-scoped projection (Issue collaboration D8): a
// form opened for one issue carries the launch fields — which repository and branch this run is for,
// which published version to run, what prompt, and which extra context travels with it — prefilled from
// that issue's project and the workflow's own document.
func TestFormDescriptorPlatformFieldsForAnIssue(t *testing.T) {
	f := setup(t)
	// The release workflow declares none of the reserved keys, so every platform field is injected
	// rather than colliding with an author's declaration. Two publishes give the version field a choice.
	release := createPromptWorkflow(t, f, "Release Readiness Workflow", "wf-platform", "Ship the release", []any{
		core.Object{"name": "notes", "displayName": "Release notes", "fieldType": "text-input", "valueType": "string"},
	})
	f.call("POST", f.path("/workflows/"+release+"/publish"), core.Object{}, "wf-platform-pub-1", 200)
	newest := f.call("POST", f.path("/workflows/"+release+"/publish"), core.Object{"name": "Release cut"}, "wf-platform-pub-2", 200).O("resource")

	project := f.create("platform-project").O("resource")
	withProject := f.call("POST", f.path("/issues"), core.Object{
		"title": "Has a project", "projectRef": project.S("id"),
	}, "platform-1", 200).O("resource")
	noProject := f.call("POST", f.path("/issues"), core.Object{"title": "Has no project"}, "platform-2", 200).O("resource")

	// No issue context is not a degraded mode: the descriptor is the one this route always served, with
	// no platform field at all — even though the graph would have supplied a prompt to prefill.
	bare := f.call("GET", f.path("/collaboration/forms/"+release), nil, "", 200)
	if keys := fieldKeys(orderedFormFields(t, bare)); len(keys) != 1 || keys[0] != "notes" {
		t.Fatalf("an issue-less descriptor grew platform fields: %v", keys)
	}

	// With an issue, the platform fields lead the form: they say what the run is about, while the
	// author's variables are its parameters.
	d := f.call("GET", f.path("/collaboration/forms/"+release+"?issueId="+withProject.S("id")), nil, "", 200)
	want := []string{"repository", "branch", "snapshotId", "prompt", "context_refs", "notes"}
	if keys := fieldKeys(orderedFormFields(t, d)); !slices.Equal(keys, want) {
		t.Fatalf("platform fields did not lead the form: %v, want %v", keys, want)
	}
	byKey := formFields(t, d)

	// The repository and the branch pair up, and both are read from the same project row.
	if r := byKey["repository"]; r.S("type") != "text" || !r.B("required") || r["defaultValue"] != project.S("repositoryUrl") {
		t.Fatalf("repository field wrong: %v", r)
	}
	if b := byKey["branch"]; b.S("type") != "text" || !b.B("required") || b["defaultValue"] != project.S("defaultBranch") {
		t.Fatalf("branch field wrong: %v", b)
	}

	// The version field offers the published versions, newest first and newest by default, and carries
	// the snapshot id rather than the version number: the id is what a run is created from.
	version := byKey["snapshotId"]
	if version.S("type") != "select" || version.B("required") || version["defaultValue"] != newest.S("id") {
		t.Fatalf("version field wrong: %v", version)
	}
	options, _ := version["options"].([]any)
	if len(options) != 2 {
		t.Fatalf("version options = %v, want both publishes", options)
	}
	first, _ := options[0].(map[string]any)
	if first["value"] != newest.S("id") || first["label"] != "v2 · Release cut" {
		t.Fatalf("the newest snapshot is not the first option: %v", first)
	}

	// The prompt is optional: leaving it empty is how the run says "use the workflow's own prompt",
	// which is exactly the value it is prefilled with.
	if p := byKey["prompt"]; p.S("type") != "textarea" || p.B("required") || p["defaultValue"] != "Ship the release" {
		t.Fatalf("prompt field wrong: %v", p)
	}

	// The context references are additive: nothing is selected to begin with, and the only thing on
	// offer is the one reference this issue can name for itself — its project.
	refs := byKey["context_refs"]
	if refs.S("type") != "multi_select" || refs.B("required") || refs["defaultValue"] != nil {
		t.Fatalf("context refs field wrong: %v", refs)
	}
	refOptions, _ := refs["options"].([]any)
	if len(refOptions) != 1 {
		t.Fatalf("context ref options = %v, want just the issue's own project", refOptions)
	}
	only, _ := refOptions[0].(map[string]any)
	if only["value"] != "project:"+project.S("id") {
		t.Fatalf("context ref option = %v, want the issue's project", only)
	}

	// An issue with no project still gets the required repository and branch — just with nothing in
	// them, which is what asks the user to fill them in — and loses the reference it cannot name.
	empty := formFields(t, f.call("GET", f.path("/collaboration/forms/"+release+"?issueId="+noProject.S("id")), nil, "", 200))
	for _, key := range []string{"repository", "branch"} {
		if !empty[key].B("required") {
			t.Fatalf("%s lost its requiredness without a default: %v", key, empty[key])
		}
		if got := empty[key]["defaultValue"]; got != nil {
			t.Fatalf("an issue with no project produced a %s default: %v", key, got)
		}
	}
	if _, ok := empty["context_refs"]; ok {
		t.Fatalf("an issue that names no reference grew a context refs field: %v", empty["context_refs"])
	}
	// Neither the version nor the prompt depends on the issue: they come from the workflow itself.
	if empty["snapshotId"]["defaultValue"] != newest.S("id") || empty["prompt"]["defaultValue"] != "Ship the release" {
		t.Fatalf("a workflow-owned default depended on the issue: %v", empty)
	}

	// A workflow nobody has published has no version to offer, and the contract has no way to express a
	// select with no choices — so the field is dropped rather than injected empty.
	unpublished := createPromptWorkflow(t, f, "Unpublished Workflow", "wf-platform-unpub", "Draft", nil)
	unpubKeys := fieldKeys(orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+unpublished+"?issueId="+withProject.S("id")), nil, "", 200)))
	if !slices.Equal(unpubKeys, []string{"repository", "branch", "prompt", "context_refs"}) {
		t.Fatalf("an unpublished workflow grew a version field: %v", unpubKeys)
	}

	// An issue that cannot be resolved is a 404, never a silently dropped context.
	f.call("GET", f.path("/collaboration/forms/"+release+"?issueId="+uuid.NewString()), nil, "", 404)
	f.call("GET", f.path("/collaboration/forms/"+release+"?issueId=not-a-uuid"), nil, "", 404)
}

// TestFormDescriptorPlatformFieldsCollideWithAuthorDeclarations covers the reserved-key rule (Issue collaboration D8):
// a workflow that declares one of the platform's own names keeps its declaration, and the platform
// contributes only the default that declaration can hold.
func TestFormDescriptorPlatformFieldsCollideWithAuthorDeclarations(t *testing.T) {
	f := setup(t)
	// The review workflow declares `repository` and `branch` itself — `branch` with a default of its
	// own — and is published, so the platform would otherwise offer a version too.
	wid := reviewWorkflow(t, f)
	f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{}, "wf-collide-pub", 200)
	// The project's branch differs from the author's, so keeping the author's is a visible choice
	// rather than a coincidence of both being "main".
	project := f.call("POST", f.path("/projects"), core.Object{
		"name": "Collide", "repositoryUrl": "https://example.invalid/collide.git", "defaultBranch": "trunk",
	}, "platform-collide-project", 202).O("resource")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Collides", "projectRef": project.S("id"),
	}, "platform-collide-1", 200).O("resource")

	byKey := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))

	// The author's controls survive; the platform only fills a default they left empty.
	if r := byKey["repository"]; r.S("label") != "Repository" || !r.B("required") || r["defaultValue"] != project.S("repositoryUrl") {
		t.Fatalf("the author's repository declaration was overwritten: %v", r)
	}
	if b := byKey["branch"]; b.S("label") != "Branch" || b.B("required") {
		t.Fatalf("the author's branch declaration was overwritten: %v", b)
	}
	if got := byKey["branch"]["defaultValue"]; got != "main" {
		t.Fatalf("the author's own branch default was replaced: %v, want main", got)
	}
	// The reserved keys the author did not declare are still injected alongside their fields.
	if v := byKey["snapshotId"]; v.S("type") != "select" {
		t.Fatalf("the platform's version field was not injected: %v", v)
	}
	if p := byKey["prompt"]; p.S("type") != "textarea" || p.B("required") {
		t.Fatalf("the platform's prompt field was not injected: %v", p)
	}
	// The colliding keys keep the author's position, so only the two injected fields are prepended.
	if keys := fieldKeys(orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))); keys[0] != "snapshotId" || keys[1] != "prompt" {
		t.Fatalf("the injected fields did not lead the colliding form: %v", keys)
	}
}

// launchDefaults is the declaration an author gets by opening the editor's `@` form fields dialog and
// saving it untouched: every field asked for, at the platform's own requiredness.
func launchDefaults() []any {
	return []any{
		core.Object{"key": "repository", "enabled": true, "required": true},
		core.Object{"key": "branch", "enabled": true, "required": true},
		core.Object{"key": "snapshotId", "enabled": true, "required": false},
		core.Object{"key": "prompt", "enabled": true, "required": false},
		core.Object{"key": "context_refs", "enabled": true, "required": false},
	}
}

// notesVariables is one author variable that collides with none of the platform's reserved keys, so a
// descriptor built from it is the platform's own fields plus this one.
func notesVariables() []any {
	return []any{
		core.Object{"name": "notes", "displayName": "Release notes", "fieldType": "text-input", "valueType": "string"},
	}
}

// launchIssue is an issue on the given project, for a case that needs its own interaction list:
// workflowComment answers with the issue's first form interaction, so two workflows sharing an issue
// would both be driven through whichever of them was commented on first.
func launchIssue(t *testing.T, f *fixture, projectID, title, key string) core.Object {
	t.Helper()
	return f.call("POST", f.path("/issues"), core.Object{
		"title": title, "projectRef": projectID,
	}, key, 200).O("resource")
}

// fieldShapes is a descriptor's fields without their defaults. A default can name a value only one
// workflow has — a version names that workflow's newest snapshot — while the shape is the catalog's.
func fieldShapes(fields []core.Object) []string {
	out := []string{}
	for _, field := range fields {
		out = append(out, fmt.Sprintf("%s %s %s %t", field.S("key"), field.S("label"), field.S("type"), field.B("required")))
	}
	return out
}

// TestFormDescriptorAuthorDeclaredLaunchFields covers Issue collaboration D8: the workflow's author decides which of
// the platform's launch fields the `@` form asks for, and which of them it insists on. The declaration
// rides in the graph, so the live document is what decides — publishing only gives the version field
// something to offer.
func TestFormDescriptorAuthorDeclaredLaunchFields(t *testing.T) {
	f := setup(t)
	project := f.create("launch-project").O("resource")
	repo := project.S("repositoryUrl")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Declared launch fields", "projectRef": project.S("id"),
	}, "launch-issue", 200).O("resource")

	// Drift guard. A declaration that spells out the platform's own answers must be the same form as
	// declaring nothing at all: the catalog the editor's dialog seeds from and the catalog the
	// projection injects have to agree, or the first save from that dialog would change the form.
	plain := createPromptWorkflow(t, f, "Plain", "launch-plain", "Ship it", notesVariables())
	explicit := createLaunchWorkflow(t, f, "Explicit", "launch-explicit", notesVariables(), launchDefaults())
	for _, wid := range []string{plain, explicit} {
		f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{}, "launch-pub-"+wid, 200)
	}
	plainFields := orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+plain+"?issueId="+iss.S("id")), nil, "", 200))
	explicitFields := orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+explicit+"?issueId="+iss.S("id")), nil, "", 200))
	if got, want := fieldShapes(explicitFields), fieldShapes(plainFields); !slices.Equal(got, want) {
		t.Fatalf("an explicit default declaration changed the form:\n got %v\nwant %v", got, want)
	}
	if len(plainFields) != 6 {
		t.Fatalf("the drift guard compared an unexpected form: %v", fieldShapes(plainFields))
	}

	// A withdrawn field is not asked for at all. The repository is the sharpest case: it is one of the
	// two the platform considers essential, and the author may still decide this workflow needs none.
	declared := createLaunchWorkflow(t, f, "Declared", "launch-declared", notesVariables(), []any{
		core.Object{"key": "repository", "enabled": false},
		core.Object{"key": "snapshotId", "enabled": false},
	})
	f.call("POST", f.path("/workflows/"+declared+"/publish"), core.Object{}, "launch-pub-declared", 200)
	d := f.call("GET", f.path("/collaboration/forms/"+declared+"?issueId="+iss.S("id")), nil, "", 200)
	if keys := fieldKeys(orderedFormFields(t, d)); !slices.Equal(keys, []string{"branch", "prompt", "context_refs", "notes"}) {
		t.Fatalf("a withdrawn field was still asked for: %v", keys)
	}
	// The declaration a snapshot published before the rename still holds names that selector `version`,
	// and it has to keep meaning what its author meant: the field stays withdrawn rather than coming
	// back to a form whose author had turned it off.
	preRename := createLaunchWorkflow(t, f, "Pre-rename", "launch-pre-rename", notesVariables(), []any{
		core.Object{"key": "repository", "enabled": false},
		core.Object{"key": "version", "enabled": false},
	})
	f.call("POST", f.path("/workflows/"+preRename+"/publish"), core.Object{}, "launch-pub-pre-rename", 200)
	if keys := fieldKeys(orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+preRename+"?issueId="+iss.S("id")), nil, "", 200))); !slices.Equal(keys, []string{"branch", "prompt", "context_refs", "notes"}) {
		t.Fatalf("a withdrawal written under the old name was lost: %v", keys)
	}
	// The descriptor is the whole contract, and both routes that read it re-resolve the declaration, so
	// a value for a withdrawn field is an unknown key rather than a silently kept one.
	declaredIss := launchIssue(t, f, project.S("id"), "Declared launch fields", "launch-declared-issue")
	it := workflowComment(t, f, declared, declaredIss.S("id"), "launch-declared-1", "configure")
	f.call("POST", confirmPath(f, declaredIss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": repo, "branch": "main",
	}}, "launch-declared-c1", 400)
	// The control for that rejection: the same form without the withdrawn key goes through, so what
	// the 400 above was about is the key and not the rest of the submission.
	f.call("POST", confirmPath(f, declaredIss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"branch": "main",
	}}, "launch-declared-c2", 200)
	f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(declared, core.Object{
		"repository": repo,
	}), "launch-declared-a1", 400)
	f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(declared, core.Object{
		"branch": "main",
	}), "launch-declared-a2", 200)

	// Requiredness is the author's too: the prompt becomes something the form insists on and the branch
	// stops being something it does. Both are enforced on the server, not merely drawn as required.
	strict := createLaunchWorkflow(t, f, "Strict", "launch-strict", notesVariables(), []any{
		core.Object{"key": "prompt", "enabled": true, "required": true},
		core.Object{"key": "branch", "enabled": true, "required": false},
	})
	f.call("POST", f.path("/workflows/"+strict+"/publish"), core.Object{}, "launch-pub-strict", 200)
	byKey := formFields(t, f.call("GET", f.path("/collaboration/forms/"+strict+"?issueId="+iss.S("id")), nil, "", 200))
	if !byKey["prompt"].B("required") {
		t.Fatalf("the author's required prompt was not enforced: %v", byKey["prompt"])
	}
	if byKey["branch"].B("required") {
		t.Fatalf("the author's optional branch was still required: %v", byKey["branch"])
	}
	// Optional is not the same as empty: the field keeps the default the project gives it.
	if got := byKey["branch"]["defaultValue"]; got != project.S("defaultBranch") {
		t.Fatalf("an optional branch lost its default: %v", got)
	}
	strictIss := launchIssue(t, f, project.S("id"), "Strict launch fields", "launch-strict-issue")
	strictIt := workflowComment(t, f, strict, strictIss.S("id"), "launch-strict-1", "configure")
	// A form filled in but for the prompt: what the author made required is what confirm refuses to
	// proceed without, and the branch it made optional is not.
	f.call("POST", confirmPath(f, strictIss.S("id"), strictIt.S("id")), core.Object{"values": core.Object{
		"repository": repo, "branch": "main",
	}}, "launch-strict-c1", 400)
	// An empty optional field is a legal value, and how a run says "whatever the workflow uses".
	f.call("POST", confirmPath(f, strictIss.S("id"), strictIt.S("id")), core.Object{"values": core.Object{
		"repository": repo, "branch": "", "prompt": "Review the auth path",
	}}, "launch-strict-c2", 200)

	// A key the author declared in their own Start node is not the platform's to withdraw: the
	// declaration is inert for that key, control and requiredness alike, and the platform contributes
	// only the default such a control can hold.
	owned := createLaunchWorkflow(t, f, "Author owned", "launch-owned", []any{
		core.Object{"name": "repository", "displayName": "Repository", "fieldType": "text-input", "valueType": "string"},
	}, []any{
		core.Object{"key": "repository", "enabled": false, "required": true},
		core.Object{"key": "snapshotId", "enabled": false},
	})
	f.call("POST", f.path("/workflows/"+owned+"/publish"), core.Object{}, "launch-pub-owned", 200)
	ownedFields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+owned+"?issueId="+iss.S("id")), nil, "", 200))
	if r := ownedFields["repository"]; r.S("label") != "Repository" || r.B("required") {
		t.Fatalf("the declaration reached a key the author owns: %v", r)
	}
	if got := ownedFields["repository"]["defaultValue"]; got != repo {
		t.Fatalf("the author's repository was not prefilled: %v", got)
	}
	// The same declaration did withdraw a field the author does not own, so the inertness above is the
	// author's key being theirs rather than the declaration being ignored.
	if _, ok := ownedFields["snapshotId"]; ok {
		t.Fatalf("a withdrawn field survived: %v", ownedFields["snapshotId"])
	}
}

// TestWorkflowConfirmCarriesPlatformFields covers the round trip across the three paths that must
// agree on one descriptor (Issue collaboration D8): the values the form was rendered with are the values assist
// accepts and the values confirm persists and hands to the run. A descriptor that differed between
// them would reject a submission the user was legitimately shown.
func TestWorkflowConfirmCarriesPlatformFields(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	// Publishing gives the form a version to choose, so the round trip covers every launch field.
	published := f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{}, "platform-confirm-pub", 200).O("resource")
	project := f.create("platform-confirm-project").O("resource")
	repo := project.S("repositoryUrl")
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title": "Platform confirm", "projectRef": project.S("id"),
	}, "platform-confirm-issue", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "platform-confirm-1", "configure the review")

	// Assist accepts a platform key, which it only can if it resolved the same issue-scoped descriptor
	// the form was rendered from: an unknown key is a 400.
	res := f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(wid, core.Object{
		"repository": repo, "prompt": "Focus on the auth path",
	}), "platform-assist-1", 200)
	if res.O("suggestedValues").S("scope") == "" {
		t.Fatalf("assist suggested nothing: %v", res)
	}

	// A value the platform field's type rejects is still rejected.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": 7, "scope": "full-repo", "severity": "high",
	}}, "platform-c1", 400)

	// The context-reference field's value is a list of `refType:refId` pairs; the pairs the surface
	// turns it into are validated like any other, so an unknown ref type is a 400 rather than a
	// silently dropped reference.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{
		"values": core.Object{"repository": repo, "scope": "full-repo", "severity": "high"},
		"contextRefs": []any{core.Object{
			"refType": "not_a_type", "refId": uuid.NewString(),
		}},
	}, "platform-c2", 400)

	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{
		"values": core.Object{
			"repository": repo, "prompt": "Focus on the auth path", "snapshotId": published.S("id"),
			"branch": "main", "scope": "full-repo", "severity": "high",
			"context_refs": []any{"project:" + project.S("id")},
		},
		// What the surface derives from the field above, alongside anything AI Assist applied.
		"contextRefs": []any{core.Object{"refType": "project", "refId": project.S("id")}},
	}, "platform-c3", 200).O("resource")
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)

	// Every launch value reaches the run snapshot, which is what the workflow would execute with.
	executed := run.O("input").O("interactionValues")
	if executed.S("repository") != repo || executed.S("prompt") != "Focus on the auth path" {
		t.Fatalf("platform values did not reach the run: %v", executed)
	}
	if executed.S("snapshotId") != published.S("id") {
		t.Fatalf("the chosen version did not reach the run: %v", executed)
	}
	refs, _ := run.O("input")["contextRefs"].([]any)
	if len(refs) != 1 {
		t.Fatalf("the chosen context ref did not reach the run: %v", run.O("input"))
	}
	if ref, _ := refs[0].(map[string]any); ref["refType"] != "project" || ref["refId"] != project.S("id") {
		t.Fatalf("the run's context ref is wrong: %v", refs[0])
	}

	reloaded := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/interactions"), nil, "", 200))[0]
	if reloaded.O("input").S("prompt") != "Focus on the auth path" || reloaded.O("input").S("repository") != repo {
		t.Fatalf("platform values were not persisted on the interaction: %v", reloaded.O("input"))
	}
	if reloaded.O("input").S("snapshotId") != published.S("id") {
		t.Fatalf("the chosen version was not persisted on the interaction: %v", reloaded.O("input"))
	}
}

// TestAuthorDeclaredSnapshotIDIsNotTheVersionCursor covers the one key a form value and a platform
// cursor can both be called. Issue collaboration D8 keeps a key the author declared in their own Start node, so a
// workflow that declares `snapshotId` gets no version selector and the value under that key is the
// author's variable. Read as a cursor it would make such a form unconfirmable both ways round: a value
// that is not a snapshot id is a 404, and the platform's prefill would have put a snapshot id in the
// author's field, so a user who left it alone would run the newest version while handing their workflow
// that id as its variable.
func TestAuthorDeclaredSnapshotIDIsNotTheVersionCursor(t *testing.T) {
	f := setup(t)
	wid := createWorkflow(t, f, "Release Workflow", "wf-version-owned", []any{
		core.Object{"name": "snapshotId", "displayName": "Release version", "fieldType": "text-input", "valueType": "string", "required": true},
		core.Object{"name": "scope", "displayName": "Scope", "fieldType": "text-input", "valueType": "string", "required": true},
	})
	published := publishWorkflow(t, f, wid, "wf-version-owned-pub")
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Release it"}, "wf-version-issue", 200).O("resource")

	// The form the author's declaration produces: their own control under that key, holding nothing —
	// and no version selector, which is what makes the key theirs to begin with.
	fields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iss.S("id")), nil, "", 200))
	if v := fields["snapshotId"]; v.S("type") != "text" || v.S("label") != "Release version" {
		t.Fatalf("the author's version control was replaced by the platform's: %v", v)
	}
	if _, prefilled := fields["snapshotId"]["defaultValue"]; prefilled {
		t.Fatalf("the author's version was prefilled with a snapshot id: %v", fields["snapshotId"])
	}
	// The control for that missing selector: the key is the platform's in a graph that leaves it alone,
	// so the form above differs because of the declaration rather than because the field is gone.
	plain := reviewWorkflow(t, f)
	publishWorkflow(t, f, plain, "wf-version-plain-pub")
	plainFields := formFields(t, f.call("GET", f.path("/collaboration/forms/"+plain+"?issueId="+iss.S("id")), nil, "", 200))
	if v := plainFields["snapshotId"]; v.S("type") != "select" {
		t.Fatalf("a workflow that declares no version lost the platform's selector: %v", v)
	}

	// A user who types their own value: the run still executes the newest published version — nothing
	// selected it — and the workflow receives what they typed rather than that version's id.
	it := workflowComment(t, f, wid, iss.S("id"), "wf-version-owned-1", "release")
	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "branch": "main", "snapshotId": "1.2.3", "scope": "full-repo",
	}}, "wf-version-owned-c1", 200).O("resource")
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if got := run.O("input").O("interactionValues").S("snapshotId"); got != "1.2.3" {
		t.Fatalf("the workflow did not receive the author's version: %v", run.O("input").O("interactionValues"))
	}
	if got := run.O("input").S("workflowSnapshotId"); got != published.S("id") {
		t.Fatalf("run snapshot = %q, want the newest published version %q", got, published.S("id"))
	}
}

// TestWorkflowConfirmReadsThePreRenameVersionKey covers the other half of the rename (Issue collaboration D8): the
// field is `snapshotId` now, but a form rendered before the deploy answers under `version`, and the
// values are submitted after it. That submission has to be accepted as the version selection it is —
// reading only the new key would start the newest version while the version the user picked is dropped
// without a word — and the interaction has to be stored under the name this build writes, because the
// `@` form is the one surface where the set of keys is the contract and a key the descriptor does not
// declare is refused outright.
func TestWorkflowConfirmReadsThePreRenameVersionKey(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	older := publishWorkflow(t, f, wid, "wf-rename-pub-1")
	newest := f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{"name": "Second cut"}, "wf-rename-pub-2", 200).O("resource")
	if newest.S("id") == older.S("id") {
		t.Fatal("the two publishes are the same snapshot, so a wrong version could pass as the right one")
	}
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Run the old form"}, "wf-rename-issue", 200).O("resource")

	// The submission a client that rendered the pre-rename form sends: the selection under the old key,
	// and no `snapshotId` at all.
	it := workflowComment(t, f, wid, iss.S("id"), "wf-rename-1", "run it")
	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "branch": "main", "version": older.S("id"),
		"prompt": "Review the diff", "scope": "full-repo", "severity": "high",
	}}, "wf-rename-c1", 200).O("resource")
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if got := run.O("input").S("workflowSnapshotId"); got != older.S("id") {
		t.Fatalf("run snapshot = %q, want the version the old form selected %q", got, older.S("id"))
	}
	stored := run.O("input").O("interactionValues")
	if stored.S("snapshotId") != older.S("id") {
		t.Fatalf("the selection was not stored under the new key: %v", stored)
	}
	if _, kept := stored["version"]; kept {
		t.Fatalf("the pre-rename key was stored: %v", stored)
	}

	// The run's copy above is what the executor received; the obligation also names the interaction row,
	// which is what the user configured and what the next reader of the issue sees. They are two columns
	// written from one submission in one transaction, so the fold has to have reached both: reading the
	// row back is also the only assertion that separates a rewritten key from an added one.
	var row core.Object
	for _, item := range issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/interactions"), nil, "", 200)) {
		if item.S("id") == it.S("id") {
			row = item
		}
	}
	if row == nil {
		t.Fatalf("the confirmed interaction is not readable, so its stored keys cannot be checked")
	}
	if row.O("input").S("snapshotId") != older.S("id") {
		t.Fatalf("the interaction did not store the selection under the new key: %v", row.O("input"))
	}
	if _, kept := row.O("input")["version"]; kept {
		t.Fatalf("the interaction stored the pre-rename key: %v", row.O("input"))
	}

	// The same submission against a graph whose author declared `snapshotId` for a variable of their
	// own. There the new key is theirs, so the selection cannot be written under it — but the version
	// the user picked is still the one that runs, and the variable keeps the value they gave it.
	owned := createWorkflow(t, f, "Owned Workflow", "wf-rename-owned", []any{
		core.Object{"name": "snapshotId", "displayName": "Release version", "fieldType": "text-input", "valueType": "string", "required": true},
	})
	ownedOlder := publishWorkflow(t, f, owned, "wf-rename-owned-pub-1")
	ownedNewest := f.call("POST", f.path("/workflows/"+owned+"/publish"), core.Object{"name": "Second cut"}, "wf-rename-owned-pub-2", 200).O("resource")
	if ownedNewest.S("id") == ownedOlder.S("id") {
		t.Fatal("the two publishes are the same snapshot, so a wrong version could pass as the right one")
	}
	// A second issue, because a confirmed interaction is spent: `workflowComment` reads back the issue's
	// form interaction and would hand back the one just confirmed.
	ownedIss := f.call("POST", f.path("/issues"), core.Object{"title": "Run the old form again"}, "wf-rename-issue-2", 200).O("resource")
	it = workflowComment(t, f, owned, ownedIss.S("id"), "wf-rename-owned-1", "run it")
	created = f.call("POST", confirmPath(f, ownedIss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "branch": "main", "version": ownedOlder.S("id"), "snapshotId": "1.2.3",
	}}, "wf-rename-owned-c1", 200).O("resource")
	run = f.call("GET", f.path("/issues/"+ownedIss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if got := run.O("input").S("workflowSnapshotId"); got != ownedOlder.S("id") {
		t.Fatalf("run snapshot = %q, want the version the old form selected %q", got, ownedOlder.S("id"))
	}
	stored = run.O("input").O("interactionValues")
	if stored.S("snapshotId") != "1.2.3" {
		t.Fatalf("the author's variable was overwritten by the version selection: %v", stored)
	}
	if _, kept := stored["version"]; kept {
		t.Fatalf("the pre-rename key was stored: %v", stored)
	}
}

// TestWorkflowAssistReadsThePreRenameVersionKey covers the assist half of the same rename (Issue collaboration D8).
// Assist validates the submission the confirm will, against the descriptor the client rendered from,
// so the two entries have to accept the same keys: the live descriptor no longer declares `version`,
// and reading only the new key refuses the submission as an unknown field. That refusal is the bug —
// one set of values a pre-rename form sent, accepted by the confirm and refused by assist.
//
// What assist can be held to is narrower than what the confirm is, because assist is stateless and
// answers with a patch: it persists no folded values and echoes none of the ones it was given, so
// which of two submitted keys won is not observable through this route. That half is covered where the
// rule itself lives — the foldLegacyVersionAnswer table in internal/core, which is also the only
// definition of it, since assist now calls the same function rather than keeping a rule of its own.
// What this test pins is the wiring: the fold runs before assist validates.
func TestWorkflowAssistReadsThePreRenameVersionKey(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	older := publishWorkflow(t, f, wid, "wf-assist-rename-pub-1")
	newest := f.call("POST", f.path("/workflows/"+wid+"/publish"), core.Object{"name": "Second cut"}, "wf-assist-rename-pub-2", 200).O("resource")
	if newest.S("id") == older.S("id") {
		t.Fatal("the two publishes are the same snapshot, so the test cannot tell which one was read")
	}
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Fill the old form"}, "wf-assist-rename-issue", 200).O("resource")

	// The submission a client that rendered the pre-rename form sends — the selection under the old key
	// and no `snapshotId` at all — driven through both entries in turn. Assist comes first so the 200
	// below is assist's own answer to exactly the values the confirm then accepts.
	preRename := core.Object{
		"repository": "ora-space/cloud", "branch": "main", "version": older.S("id"),
		"prompt": "Review the diff", "scope": "full-repo", "severity": "high",
	}
	f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(wid, preRename), "wf-assist-rename-a1", 200)
	it := workflowComment(t, f, wid, iss.S("id"), "wf-assist-rename-1", "run it")
	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": preRename}, "wf-assist-rename-c1", 200).O("resource")
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if got := run.O("input").S("workflowSnapshotId"); got != older.S("id") {
		t.Fatalf("run snapshot = %q, want the version the old form selected %q", got, older.S("id"))
	}

	// A key the descriptor declares nowhere is still refused, so the acceptance above is the old key
	// being read rather than assist having stopped checking the set of keys at all.
	refusal := f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(wid, core.Object{
		"repository": "ora-space/cloud", "revision": older.S("id"),
	}), "wf-assist-rename-a2", 400)
	if refusal.S("code") != "invalid_field_value" || refusal.O("params").S("field") != "revision" {
		t.Fatalf("an unknown key was refused for the wrong reason: %v", refusal)
	}
	// Both keys at once is the ordinary state mid-deploy, and legal either way: the fold takes the old
	// key out, so the set that reaches validation is the descriptor's own.
	f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(wid, core.Object{
		"repository": "ora-space/cloud", "version": older.S("id"), "snapshotId": newest.S("id"),
	}), "wf-assist-rename-a3", 200)

	// A graph whose author declared `snapshotId` for a variable of their own. There the new key is
	// theirs, so the fold drops the old one rather than writing a snapshot id into their variable —
	// and the submission is accepted all the same, which is the rule the confirm applies to this graph.
	owned := createWorkflow(t, f, "Owned Workflow", "wf-assist-rename-owned", []any{
		core.Object{"name": "snapshotId", "displayName": "Release version", "fieldType": "text-input", "valueType": "string", "required": true},
	})
	publishWorkflow(t, f, owned, "wf-assist-rename-owned-pub")
	ownedIss := f.call("POST", f.path("/issues"), core.Object{"title": "Fill the old form again"}, "wf-assist-rename-issue-2", 200).O("resource")
	f.call("POST", assistPath(f, ownedIss.S("id")), workflowAssistBody(owned, core.Object{
		"repository": "ora-space/cloud", "version": "1.2.3",
	}), "wf-assist-rename-owned-a1", 200)

	// A graph whose author declared `version` for a variable of their own. That key is the author's, so
	// the fold leaves the submission alone and the value stays theirs to give the workflow. The mock
	// assistant never suggests over a key the user has already filled, so its silence about `version`
	// is what shows the value survived — and the unfilled call beside it is the control proving the
	// mock does offer that key, without which the silence would prove nothing.
	legacy := createWorkflow(t, f, "Legacy Workflow", "wf-assist-rename-legacy", []any{
		core.Object{"name": "version", "displayName": "Release version", "fieldType": "text-input", "valueType": "string", "required": true},
	})
	publishWorkflow(t, f, legacy, "wf-assist-rename-legacy-pub")
	legacyIss := f.call("POST", f.path("/issues"), core.Object{"title": "Fill the old form once more"}, "wf-assist-rename-issue-3", 200).O("resource")
	unfilled := f.call("POST", assistPath(f, legacyIss.S("id")), workflowAssistBody(legacy, core.Object{
		"repository": "ora-space/cloud",
	}), "wf-assist-rename-legacy-a1", 200).O("suggestedValues")
	if unfilled.S("version") == "" {
		t.Fatalf("the control did not fire: the mock no longer offers the author's version key, so its silence below would prove nothing: %v", unfilled)
	}
	filled := f.call("POST", assistPath(f, legacyIss.S("id")), workflowAssistBody(legacy, core.Object{
		"repository": "ora-space/cloud", "version": "1.2.3",
	}), "wf-assist-rename-legacy-a2", 200).O("suggestedValues")
	if _, suggested := filled["version"]; suggested {
		t.Fatalf("the author's own version key was taken from them: %v", filled)
	}
}

// TestWorkflowConfirmRefusesAVersionTheLiveFormHasLeft covers the two documents a confirm reconciles.
// The form the client rendered comes from the workflow's *live* document — the frontend resolves a
// descriptor once per formRef and keeps it while the user fills the form in — while the submission
// executes one frozen version. An author who edits between publishing a version and confirming against
// it, which is the ordinary state of a workflow between an edit and its publish, leaves that version
// describing a different form: validating the user's answers against it can only refuse a value they
// were legitimately shown, or demand a field they were never shown. Both are answers about the wrong
// document, so the confirm refuses the mismatch itself, names the fields, executes nothing, and leaves
// the interaction unclaimed — the user then does what the fault says, and publishes or picks a version
// whose fields the form still matches.
func TestWorkflowConfirmRefusesAVersionTheLiveFormHasLeft(t *testing.T) {
	f := setup(t)
	authorField := func(name string) core.Object {
		return core.Object{"name": name, "displayName": name, "fieldType": "text-input", "valueType": "string", "required": true}
	}
	wid := createWorkflow(t, f, "Diverging Workflow", "wf-diverge", []any{authorField("topic")})
	v1 := publishWorkflow(t, f, wid, "wf-diverge-pub-1")
	// edit is an author's edit that is saved but not published: the live document and the newest version
	// part ways here, which is what every step below turns on.
	edit := func(variables []any) {
		t.Helper()
		live := f.call("GET", f.path("/workflows/"+wid), nil, "", 200)
		f.call("PUT", f.path("/workflows/"+wid), core.Object{
			"graph": startInputGraph(variables), "version": live.N("version"),
		}, "", 200)
	}
	newIssue := func(key string) string {
		t.Helper()
		return f.call("POST", f.path("/issues"), core.Object{"title": "Diverging form"}, key, 200).O("resource").S("id")
	}
	rendered := func(iid string) []string {
		t.Helper()
		return fieldKeys(orderedFormFields(t, f.call("GET", f.path("/collaboration/forms/"+wid+"?issueId="+iid), nil, "", 200)))
	}
	withVersion := func(base core.Object, id string) core.Object {
		values := core.Object{"snapshotId": id}
		for key, value := range base {
			values[key] = value
		}
		return values
	}
	// What the form invites once the draft declares both fields: every key it renders, filled in.
	submitted := core.Object{"repository": "ora-space/cloud", "branch": "main", "topic": "auth", "scope": "full-repo"}

	// The draft gains a required field after version 1 was published, so the rendered form asks for
	// `scope` and the version the user's answers would be checked against has never heard of it.
	edit([]any{authorField("topic"), authorField("scope")})
	ahead := newIssue("wf-diverge-issue-1")
	if fields := rendered(ahead); !slices.Contains(fields, "scope") {
		t.Fatalf("the rendered form is not the draft's: %v", fields)
	}
	it := workflowComment(t, f, wid, ahead, "wf-diverge-1", "configure")
	refusal := f.call("POST", confirmPath(f, ahead, it.S("id")), core.Object{"values": submitted}, "wf-diverge-c1", 409)
	if got := refusedFields(t, refusal); !slices.Equal(got, []string{"scope"}) {
		t.Fatalf("the refusal named %v, want the field version 1 does not declare", got)
	}
	// A refused confirm claims nothing: no run was created, and the same interaction is still there to
	// confirm once the form and the version agree again.
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", ahead); n != 0 {
		t.Fatalf("a refused confirm created %d runs", n)
	}
	// Publishing the draft is the first remedy the fault names, and the same submission then goes through.
	v2 := publishWorkflow(t, f, wid, "wf-diverge-pub-2")
	run := f.call("POST", confirmPath(f, ahead, it.S("id")), core.Object{"values": submitted}, "wf-diverge-c2", 200).O("resource")
	if got := f.call("GET", f.path("/issues/"+ahead+"/runs/"+run.S("id")), nil, "", 200).O("input").S("workflowSnapshotId"); got != v2.S("id") {
		t.Fatalf("the run executed %q, want the version published since the refusal %q", got, v2.S("id"))
	}

	// The other remedy: the live document is now version 2, and the same mismatch read from the version
	// side is a user who picks the older version 1 — which does not declare `scope` either. Selecting a
	// version has to be a real way out, so the fix and the mismatch are checked on one interaction.
	picked := newIssue("wf-diverge-issue-2")
	older := workflowComment(t, f, wid, picked, "wf-diverge-2", "configure")
	chose := f.call("POST", confirmPath(f, picked, older.S("id")), core.Object{"values": withVersion(submitted, v1.S("id"))}, "wf-diverge-c3", 409)
	if got := refusedFields(t, chose); !slices.Equal(got, []string{"scope"}) {
		t.Fatalf("selecting version 1 named %v, want the field version 1 does not declare", got)
	}
	run = f.call("POST", confirmPath(f, picked, older.S("id")), core.Object{"values": withVersion(submitted, v2.S("id"))}, "wf-diverge-c4", 200).O("resource")
	if got := f.call("GET", f.path("/issues/"+picked+"/runs/"+run.S("id")), nil, "", 200).O("input").S("workflowSnapshotId"); got != v2.S("id") {
		t.Fatalf("the run executed %q, want the version that was selected %q", got, v2.S("id"))
	}

	// And the direction where the values are complete: `scope` leaves the draft, so the form no longer
	// offers it while version 2 still requires it. Checked against version 2 alone this submission is
	// `required_field_missing` for a field the user was never shown — which is why the version is not
	// asked, and the mismatch is named instead.
	edit([]any{authorField("topic")})
	behind := newIssue("wf-diverge-issue-3")
	if fields := rendered(behind); slices.Contains(fields, "scope") {
		t.Fatalf("the rendered form still asks for a field the draft dropped: %v", fields)
	}
	back := workflowComment(t, f, wid, behind, "wf-diverge-3", "configure")
	// No `scope` in the submission, because no form asked for one.
	thin := core.Object{"repository": "ora-space/cloud", "branch": "main", "topic": "auth"}
	missing := f.call("POST", confirmPath(f, behind, back.S("id")), core.Object{"values": thin}, "wf-diverge-c5", 409)
	if got := refusedFields(t, missing); !slices.Equal(got, []string{"scope"}) {
		t.Fatalf("the refusal named %v, want the field the form no longer asks for", got)
	}
	// Here the remedy is the older version, because version 1's fields are still this form's.
	run = f.call("POST", confirmPath(f, behind, back.S("id")), core.Object{"values": withVersion(thin, v1.S("id"))}, "wf-diverge-c6", 200).O("resource")
	if got := f.call("GET", f.path("/issues/"+behind+"/runs/"+run.S("id")), nil, "", 200).O("input").S("workflowSnapshotId"); got != v1.S("id") {
		t.Fatalf("the run executed %q, want the version whose fields the form still matches %q", got, v1.S("id"))
	}
}

// TestFormDescriptorUnavailableAndInvalid covers the provider failure modes (Issue collaboration D7) and where they
// stop: the read-only descriptor route is 503 with no provider and 500 for a malformed one, never a
// partially usable form, while a confirm does not consult the provider at all. A confirm validates the
// submitted values against the schema of the version being confirmed, which is frozen in the snapshot
// the run will execute — so a provider outage can neither admit a bad submission nor block a good one.
func TestFormDescriptorUnavailableAndInvalid(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	publishWorkflow(t, f, wid, "wf-prov-pub")
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Provider states"}, "wf-prov", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "wf-prov-1", "configure")

	original := f.store.Forms
	t.Cleanup(func() { f.store.Forms = original })

	f.store.Forms = nil
	f.call("GET", f.path("/collaboration/forms/"+wid), nil, "", 503)

	f.store.Forms = invalidDescriptorProvider{FormRef: wid}
	f.call("GET", f.path("/collaboration/forms/"+wid), nil, "", 500)

	// The snapshot's own schema is still enforced while the provider is unusable...
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")),
		core.Object{"values": core.Object{"repository": "ora-space/cloud"}}, "wf-prov-c1", 400)
	// ...and a legal submission is confirmed rather than held hostage by it.
	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": validValues()}, "wf-prov-c2", 200).O("resource")
	if created.S("executorType") != "workflow" || created.S("executorId") != wid {
		t.Fatalf("confirm with an unusable provider produced the wrong run: %v", created)
	}
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 1 {
		t.Fatalf("confirm produced %d runs, want exactly one", n)
	}
}

// TestWorkflowAssistIsSideEffectFree covers Issue collaboration D9: assist returns a field-level patch, never
// clobbers a filled value, and touches no Issue state.
func TestWorkflowAssistIsSideEffectFree(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	iss := f.call("POST", f.path("/issues"), core.Object{
		"title":       "Auth token leak",
		"description": "the auth token is logged",
	}, "wf-assist-issue", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "wf-assist-1", "please review")

	before := f.scalar("SELECT count(*) FROM issue_comments WHERE issue_id=$1", iss.S("id"))
	res := f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(it.S("targetId"), core.Object{
		"repository": "ora-space/cloud",
	}), "wf-assist-a1", 200)

	suggested := res.O("suggestedValues")
	if suggested.S("scope") == "" || suggested.S("severity") == "" {
		t.Fatalf("assist suggested nothing: %v", res)
	}
	// The issue text mentions auth, so the deterministic mock raises severity.
	if suggested.S("severity") != "high" {
		t.Fatalf("assist severity not deterministic from issue text: %v", suggested)
	}
	// A key the user already filled is never suggested over.
	if _, ok := suggested["repository"]; ok {
		t.Fatalf("assist must not clobber a filled value: %v", suggested)
	}
	if res.O("explanations").S("severity") == "" {
		t.Fatalf("assist explanations missing: %v", res)
	}
	// Suggestions are not persisted anywhere.
	if refs, _ := res["suggestedContextRefs"].([]any); len(refs) != 0 {
		t.Fatalf("fixture assist should suggest no refs: %v", res)
	}

	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("assist created a run")
	}
	if n := f.scalar("SELECT count(*) FROM issue_comments WHERE issue_id=$1", iss.S("id")); n != before {
		t.Fatalf("assist wrote a comment")
	}
	if n := f.scalar("SELECT count(*) FROM issue_context_refs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("assist persisted a context ref")
	}
	if n := f.scalar("SELECT count(*) FROM issue_activities WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("assist wrote an activity")
	}
	reloaded := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/interactions"), nil, "", 200))[0]
	if len(reloaded.O("input")) != 0 {
		t.Fatalf("assist wrote interaction input: %v", reloaded)
	}
}

// TestWorkflowConfirmValidatesAndExecutes covers the confirm boundary end-to-end (Issue collaboration
// D9): authoritative re-validation, exactly one run, the persisted interaction input, the run's
// effective snapshot, provenance, and the workflow timeline representation.
func TestWorkflowConfirmValidatesAndExecutes(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	publishWorkflow(t, f, wid, "wf-confirm-pub")
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Confirm me", "description": "desc"}, "wf-confirm-issue", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "wf-confirm-1", "configure the review")

	// Idempotency-Key is mandatory for POST, like every other mutation.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": validValues()}, "", 400)
	// Missing required field.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{"repository": "ora-space/cloud"}}, "wf-c1", 400)
	// Unknown field key.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "scope": "full-repo", "severity": "high", "bogus": "x",
	}}, "wf-c2", 400)
	// Value outside the declared options.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "scope": "not-an-option", "severity": "high",
	}}, "wf-c3", 400)
	// Wrong scalar type for a boolean field.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "ora-space/cloud", "scope": "full-repo", "severity": "high", "includeDependencies": "yes",
	}}, "wf-c4", 400)
	// A malformed applied-context-ref list.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{
		"values": validValues(), "contextRefs": []any{core.Object{"refType": "not_a_type", "refId": uuid.NewString()}},
	}, "wf-c5", 400)
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 0 {
		t.Fatalf("a rejected confirm created a run")
	}

	created := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": validValues()}, "wf-confirm-ok", 200).O("resource")
	// The confirm response is the committed enqueue: dispatch is post-commit, so execution has not been
	// observed yet. The frontend re-reads the run/timeline afterwards.
	if created.S("status") != "queued" || created.S("executorType") != "workflow" || created.S("executorId") != wid {
		t.Fatalf("workflow run wrong at confirm time: %v", created)
	}
	// Provenance points at the interaction that authorized it.
	if created.S("triggerEvidenceKind") != "interaction" || created.S("triggerEvidenceRefId") != it.S("id") {
		t.Fatalf("workflow run provenance wrong: %v", created)
	}
	// Re-read: the mock dispatcher drove it to completion through the observer seam.
	run := f.call("GET", f.path("/issues/"+iss.S("id")+"/runs/"+created.S("id")), nil, "", 200)
	if run.S("status") != "completed" || run.S("externalExecutionId") == "" {
		t.Fatalf("workflow run did not complete through the observer: %v", run)
	}

	// Exactly one run, and the interaction is now confirmed with its own persisted input.
	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 1 {
		t.Fatalf("confirm produced %d runs", n)
	}
	reloaded := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/interactions"), nil, "", 200))[0]
	if reloaded.S("runId") != run.S("id") {
		t.Fatalf("interaction was not claimed: %v", reloaded)
	}
	if reloaded.O("input").S("scope") != "full-repo" || reloaded.O("input").S("severity") != "high" {
		t.Fatalf("confirmed interaction input not persisted: %v", reloaded.O("input"))
	}
	if _, stored := reloaded.O("input")["fields"]; stored {
		t.Fatalf("the descriptor must never be stored as interaction input")
	}

	// The run snapshot is the effective execution-time input, and it equals what the dispatcher got.
	input := run.O("input")
	if input.S("task") != "" || input.O("interactionValues").S("repository") != "ora-space/cloud" {
		t.Fatalf("run snapshot wrong: %v", input)
	}
	if input.O("issue").S("title") != "Confirm me" || input.O("target").S("type") != "workflow" {
		t.Fatalf("run snapshot lost issue/target context: %v", input)
	}
	if _, ok := input["recentComments"]; !ok {
		t.Fatalf("run snapshot lost the recent-comment window: %v", input)
	}

	// Timeline: comment, enqueued, started, progress, system message, completed — and NO workflow-authored comment.
	tl := issueItems(f.call("GET", f.path("/issues/"+iss.S("id")+"/timeline"), nil, "", 200))
	actions := []string{}
	for _, e := range tl {
		if e.S("kind") == "activity" {
			actions = append(actions, e.S("action"))
		}
	}
	want := []string{"run.enqueued", "run.started", "run.progress", "run.message", "run.completed"}
	if len(actions) != len(want) {
		t.Fatalf("workflow timeline actions = %v, want %v", actions, want)
	}
	for i, a := range want {
		if actions[i] != a {
			t.Fatalf("workflow timeline actions = %v, want %v", actions, want)
		}
	}
	for _, e := range tl {
		if e.S("authorType") == "workflow" {
			t.Fatalf("a workflow must never author content: %v", e)
		}
	}
	message := tl[4]
	if message.S("authorType") != "system" || message.O("details").S("executorType") != "workflow" ||
		message.O("details").S("executorId") != wid || message.O("details").S("message") == "" {
		t.Fatalf("workflow message activity wrong: %v", message)
	}
	// seq is strictly increasing and unique across the merged timeline.
	for i := 1; i < len(tl); i++ {
		if tl[i].N("seq") <= tl[i-1].N("seq") {
			t.Fatalf("timeline seq not strictly increasing: %v", tl)
		}
	}
}

// TestWorkflowConfirmIdempotencyAndDoubleConfirm covers Issue collaboration D9: the same key+body replays, a changed
// body under the same key conflicts, and a second confirm can never produce a second run.
func TestWorkflowConfirmIdempotencyAndDoubleConfirm(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	publishWorkflow(t, f, wid, "wf-idem-pub")
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "Idempotent"}, "wf-idem-issue", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "wf-idem-1", "configure")

	first := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": validValues()}, "wf-idem-confirm", 200).O("resource")
	replay := f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": validValues()}, "wf-idem-confirm", 200).O("resource")
	if replay.S("id") != first.S("id") {
		t.Fatalf("idempotent replay returned a different run: %v vs %v", replay.S("id"), first.S("id"))
	}
	// Same key, different body -> 409 from the shared idempotency infrastructure.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": core.Object{
		"repository": "other/repo", "scope": "full-repo", "severity": "low",
	}}, "wf-idem-confirm", 409)

	// A different key on an already-confirmed interaction is a conflict, not a second execution.
	f.call("POST", confirmPath(f, iss.S("id"), it.S("id")), core.Object{"values": validValues()}, "wf-idem-second", 409)

	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", iss.S("id")); n != 1 {
		t.Fatalf("double confirm produced %d runs", n)
	}
}

// TestWorkflowConfirmScopeAndModeGuards covers the security invariants of Issue collaboration (invariants 3 and 4): a confirm is scoped to
// its own issue and tenant, and only a Form Mode interaction is confirmable/assistable.
func TestWorkflowConfirmScopeAndModeGuards(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	a := f.call("POST", f.path("/issues"), core.Object{"title": "A"}, "wf-scope-a", 200).O("resource")
	b := f.call("POST", f.path("/issues"), core.Object{"title": "B"}, "wf-scope-b", 200).O("resource")
	it := workflowComment(t, f, wid, a.S("id"), "wf-scope-1", "configure")

	// Same tenant, wrong issue -> 404 (never a cross-issue claim).
	f.call("POST", confirmPath(f, b.S("id"), it.S("id")), core.Object{"values": validValues()}, "wf-scope-1c", 404)
	// Unknown interaction -> 404.
	f.call("POST", confirmPath(f, a.S("id"), uuid.NewString()), core.Object{"values": validValues()}, "wf-scope-2c", 404)
	f.call("POST", assistPath(f, a.S("id")), workflowAssistBody(uuid.NewString(), core.Object{}), "wf-scope-2a", 404)

	// A mention/task interaction is not a form and cannot be confirmed or assisted.
	f.call("POST", f.path("/issues/"+a.S("id")+"/comments"), core.Object{
		"body": "hey", "targets": []any{core.Object{"type": "user", "id": f.uid}},
	}, "wf-scope-mention", 200)
	mention := issueItems(f.call("GET", f.path("/issues/"+a.S("id")+"/interactions"), nil, "", 200))
	var mentionID string
	for _, x := range mention {
		if x.S("mode") == "mention" {
			mentionID = x.S("id")
		}
	}
	if mentionID == "" {
		t.Fatal("mention interaction missing")
	}
	f.call("POST", confirmPath(f, a.S("id"), mentionID), core.Object{"values": core.Object{}}, "wf-scope-3c", 409)
	// Assist is stateless and workflow-only: a non-workflow target id does not resolve as a workflow
	// target, so it is a typed 404 rather than a hint that the id exists.
	f.call("POST", assistPath(f, a.S("id")), workflowAssistBody(mentionID, core.Object{}), "wf-scope-3a", 404)

	if n := f.scalar("SELECT count(*) FROM issue_runs WHERE issue_id=$1", a.S("id")); n != 0 {
		t.Fatalf("a guarded confirm still produced a run")
	}
}

// TestWorkflowAssistUnavailable covers the assist capability state: no provider wired is a typed 503,
// not a silent empty suggestion.
func TestWorkflowAssistUnavailable(t *testing.T) {
	f := setup(t)
	wid := reviewWorkflow(t, f)
	iss := f.call("POST", f.path("/issues"), core.Object{"title": "No assist"}, "wf-noassist-issue", 200).O("resource")
	it := workflowComment(t, f, wid, iss.S("id"), "wf-noassist-1", "configure")

	original := f.store.Assist
	t.Cleanup(func() { f.store.Assist = original })
	f.store.Assist = nil
	f.call("POST", assistPath(f, iss.S("id")), workflowAssistBody(it.S("targetId"), core.Object{}), "wf-noassist-a1", 503)
}
