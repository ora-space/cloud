package core

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// reviewRef is the formRef the tests resolve; any opaque token works, because the projection copies
// the ref through rather than deriving it.
const reviewRef = "44444444-4444-4444-8444-444444444444"

// startGraph builds the stored graph shape the projection reads: one Start node declaring `variables`
// and carrying `prompt` as its kickoff prompt. A prompt of "" leaves the key off entirely, the way a
// Start node nobody has typed into is stored.
func startGraph(variables []any, prompt string) Object {
	data := Object{"kind": "start", "title": "开始", "description": "", "inputVariables": variables}
	if prompt != "" {
		data["input"] = prompt
	}
	return Object{"nodes": []any{Object{
		"id": "start-1", "type": "workflow", "deletable": false,
		"position": Object{"x": 0, "y": 0},
		"data":     data,
	}}}
}

// descriptorFor projects a graph whose Start node declares `variables`, the way a stored document
// reaches the projection: through JSON, as the jsonb column hands it back. It passes no issue context,
// which is the projection every caller got before the platform fields existed.
func descriptorFor(t *testing.T, variables []any) FormDescriptor {
	t.Helper()
	return projectGraph(t, startGraph(variables, ""), nil, IssueFormContext{})
}

// descriptorForIssue projects the same graph for one issue, with `prompt` as the Start node's kickoff
// prompt.
func descriptorForIssue(t *testing.T, variables []any, issue IssueFormContext, prompt string) FormDescriptor {
	t.Helper()
	return projectGraph(t, startGraph(variables, prompt), nil, issue)
}

// projectGraph runs one graph document through the projection the way a resolved descriptor reaches it,
// with the workflow's published snapshots alongside.
func projectGraph(t *testing.T, graph Object, snapshots []Object, issue IssueFormContext) FormDescriptor {
	t.Helper()
	return formDescriptorFromGraph(&workflowFormSource{
		Ref:         reviewRef,
		Title:       "Review flow",
		Description: "Reviews a change",
		Graph:       decodeGraph(t, graph),
		Snapshots:   snapshots,
	}, &issue)
}

// encodeGraph renders a graph to the bytes the column stores, for tests that assert on the stored
// document rather than on the projection.
func encodeGraph(t *testing.T, graph Object) []byte {
	t.Helper()
	raw, err := json.Marshal(graph)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	return raw
}

// decodeGraph round-trips a graph through JSON, for tests that read the document themselves.
func decodeGraph(t *testing.T, graph Object) Object {
	t.Helper()
	decoded := Object{}
	if err := json.Unmarshal(encodeGraph(t, graph), &decoded); err != nil {
		t.Fatalf("unmarshal graph: %v", err)
	}
	return decoded
}

// fieldOf returns the projected field with `key`, or fails naming the keys that were projected.
func fieldOf(t *testing.T, d FormDescriptor, key string) FormField {
	t.Helper()
	for _, f := range d.Fields {
		if f.Key == key {
			return f
		}
	}
	keys := make([]string, 0, len(d.Fields))
	for _, f := range d.Fields {
		keys = append(keys, f.Key)
	}
	t.Fatalf("field %q was not projected (got %v)", key, keys)
	return FormField{}
}

// oneVariable projects a single variable and returns the control it produced.
func oneVariable(t *testing.T, declared Object) FormField {
	t.Helper()
	return fieldOf(t, descriptorFor(t, []any{declared}), "field")
}

// TestFormDescriptorProjectionCopiesWorkflowIdentity covers what the descriptor says about the
// workflow itself: the ref it was asked for, and the workflow's own name and description.
func TestFormDescriptorProjectionCopiesWorkflowIdentity(t *testing.T) {
	d := descriptorFor(t, nil)

	if d.FormRef != reviewRef || d.Title != "Review flow" || d.Description != "Reviews a change" {
		t.Fatalf("descriptor identity wrong: %+v", d)
	}
	if len(d.Fields) != 0 {
		t.Fatalf("a workflow with no Start variables publishes an empty form: %+v", d.Fields)
	}
}

// TestFormDescriptorProjectionMapsControls covers the control each declared Start variable renders as,
// including the controls Issues has no counterpart for and the legacy declarations that predate
// explicit form-control metadata.
func TestFormDescriptorProjectionMapsControls(t *testing.T) {
	cases := []struct {
		name      string
		fieldType string
		valueType string
		want      string
	}{
		{"text input", "text-input", "string", fieldText},
		{"paragraph", "paragraph", "string", fieldTextarea},
		{"number", "number", "number", fieldNumber},
		{"checkbox", "checkbox", "boolean", fieldBoolean},
		{"file degrades to a single line", "file", "file", fieldText},
		{"file list degrades to a textarea", "file-list", "array[file]", fieldTextarea},
		{"json degrades to a textarea", "json", "object", fieldTextarea},
		{"legacy number", "", "number", fieldNumber},
		{"legacy integer", "", "integer", fieldNumber},
		{"legacy boolean", "", "boolean", fieldBoolean},
		{"legacy file", "", "file", fieldText},
		{"legacy file list", "", "array[file]", fieldTextarea},
		{"legacy secret", "", "secret", fieldText},
		{"legacy object", "", "object", fieldTextarea},
		{"nothing declared at all", "", "", fieldText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			declared := Object{"name": "field", "valueType": c.valueType}
			if c.fieldType != "" {
				declared["fieldType"] = c.fieldType
			}
			if got := oneVariable(t, declared).Type; got != c.want {
				t.Fatalf("control = %s, want %s", got, c.want)
			}
		})
	}
}

// TestFormDescriptorProjectionSelectOptions covers the one control that carries choices, and the
// degradation that keeps a select with nothing to choose from from failing validation closed.
func TestFormDescriptorProjectionSelectOptions(t *testing.T) {
	selectVar := Object{
		"name": "field", "valueType": "string", "fieldType": "select",
		"options": []any{"low", "high"},
	}
	field := oneVariable(t, selectVar)

	if field.Type != fieldSelect {
		t.Fatalf("control = %s, want %s", field.Type, fieldSelect)
	}
	if len(field.Options) != 2 || field.Options[0].Value != "low" || field.Options[0].Label != "low" {
		t.Fatalf("options wrong: %+v", field.Options)
	}
	// The projected descriptor is one Issues will render, so it must pass the same validation a
	// provider's output does.
	validateFormDescriptor(FormDescriptor{FormRef: reviewRef, Fields: []FormField{field}})

	// A select with no usable choice is a text field, not an invalid form.
	for _, declared := range []any{nil, []any{}, []any{"", 7}, []any{7, true}} {
		empty := Object{"name": "field", "valueType": "string", "fieldType": "select", "options": declared}
		if got := oneVariable(t, empty).Type; got != fieldText {
			t.Fatalf("an unusable select projected as %s, want %s", got, fieldText)
		}
	}

	// A repeated choice is one choice: a select with a single option is a valid form.
	deduped := oneVariable(t, Object{
		"name": "field", "valueType": "string", "fieldType": "select", "options": []any{"dup", "dup"},
	})
	if deduped.Type != fieldSelect || len(deduped.Options) != 1 {
		t.Fatalf("duplicate options were not collapsed: %+v", deduped)
	}
}

// TestFormDescriptorProjectionDropsUnrepresentableVariables covers the variables that cannot become a
// field: dropping one costs that field, while a malformed field would fail validation and cost the
// whole form.
func TestFormDescriptorProjectionDropsUnrepresentableVariables(t *testing.T) {
	long := strings.Repeat("x", 201)
	bad := []Object{
		{"name": "", "valueType": "string"},
		{"name": "has space", "valueType": "string"},
		{"name": "中文名", "valueType": "string"},
		{"name": "bad/slash", "valueType": "string"},
		{"name": strings.Repeat("n", 201), "valueType": "string"},
		{"name": "ok", "displayName": long, "valueType": "string"},
	}
	d := descriptorFor(t, []any{bad[0], bad[1], bad[2], bad[3], bad[4], bad[5]})
	if len(d.Fields) != 0 {
		t.Fatalf("unrepresentable variables leaked into the form: %+v", d.Fields)
	}

	// A good variable beside a bad one survives; the key is the name, never a sanitized variant.
	d = descriptorFor(t, []any{bad[1], Object{"name": "repository", "valueType": "string"}})
	if len(d.Fields) != 1 || d.Fields[0].Key != "repository" {
		t.Fatalf("a good variable was dropped with the bad ones: %+v", d.Fields)
	}
}

// TestFormDescriptorProjectionLabels covers the label and required flag: the display name is the
// label, the name is the fallback, and `required` is a boolean the variable may simply omit.
func TestFormDescriptorProjectionLabels(t *testing.T) {
	named := oneVariable(t, Object{"name": "field", "displayName": "Review scope", "valueType": "string", "required": true})
	if named.Label != "Review scope" || !named.Required {
		t.Fatalf("label/required wrong: %+v", named)
	}
	plain := oneVariable(t, Object{"name": "field", "valueType": "string"})
	if plain.Label != "field" || plain.Required {
		t.Fatalf("label/required wrong: %+v", plain)
	}
}

// TestFormDescriptorProjectionDefaults covers the declared default: it survives only when the value is
// legal for the control the variable became, so a stale default cannot make the descriptor invalid.
func TestFormDescriptorProjectionDefaults(t *testing.T) {
	cases := []struct {
		name      string
		declared  Object
		wantValue any
	}{
		{"text", Object{"name": "field", "valueType": "string", "value": "main"}, "main"},
		// Numbers arrive from the column as JSON numbers, so the projection sees a float64.
		{"number", Object{"name": "field", "valueType": "number", "value": 10}, float64(10)},
		{"boolean false", Object{"name": "field", "valueType": "boolean", "value": false}, false},
		{
			"select value inside the options",
			Object{"name": "field", "valueType": "string", "fieldType": "select", "options": []any{"low"}, "value": "low"},
			"low",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := oneVariable(t, c.declared).DefaultValue; got != c.wantValue {
				t.Fatalf("default = %v, want %v", got, c.wantValue)
			}
		})
	}

	rejected := []struct {
		name     string
		declared Object
	}{
		{"select value outside the options", Object{"name": "field", "valueType": "string", "fieldType": "select", "options": []any{"low"}, "value": "high"}},
		{"number given a string", Object{"name": "field", "valueType": "number", "value": "ten"}},
		{"boolean given a string", Object{"name": "field", "valueType": "boolean", "value": "yes"}},
		{"text longer than a form value", Object{"name": "field", "valueType": "string", "value": strings.Repeat("x", maxFieldValueLength+1)}},
	}
	for _, c := range rejected {
		t.Run(c.name, func(t *testing.T) {
			if got := oneVariable(t, c.declared).DefaultValue; got != nil {
				t.Fatalf("an illegal default survived: %v", got)
			}
		})
	}
}

// TestFormDescriptorProjectionBounds covers the caps that keep a pathological graph from producing a
// descriptor Issues refuses: the field count, and the workflow's own name and description, which are
// free text with no length rule of their own.
func TestFormDescriptorProjectionBounds(t *testing.T) {
	declared := []any{}
	for i := range maxFormFields + 5 {
		declared = append(declared, Object{"name": "field" + itoa(i), "valueType": "string"})
	}
	d := descriptorFor(t, declared)
	if len(d.Fields) != maxFormFields {
		t.Fatalf("field count = %d, want %d", len(d.Fields), maxFormFields)
	}
	validateFormDescriptor(d)

	// A select cannot declare more choices than a descriptor may carry either.
	options := []any{}
	for i := range maxFormOptions + 5 {
		options = append(options, "option-"+itoa(i))
	}
	wide := oneVariable(t, Object{"name": "field", "valueType": "string", "fieldType": "select", "options": options})
	if len(wide.Options) != maxFormOptions {
		t.Fatalf("option count = %d, want %d", len(wide.Options), maxFormOptions)
	}

	raw := decodeGraph(t, Object{"nodes": []any{Object{
		"id":   "start-1",
		"data": Object{"kind": "start", "inputVariables": []any{Object{"name": "field", "valueType": "string"}}},
	}}})
	// The name column is capped at 200 by the schema; the description is not capped anywhere.
	long := formDescriptorFromGraph(&workflowFormSource{
		Ref: reviewRef, Title: strings.Repeat("好", 100), Description: strings.Repeat("好", 1000), Graph: raw,
	}, &IssueFormContext{})
	if len(long.Title) > 200 || len(long.Description) > 2000 {
		t.Fatalf("descriptor identity was not clipped: title=%d description=%d", len(long.Title), len(long.Description))
	}
	if !utf8.ValidString(long.Title) || !utf8.ValidString(long.Description) {
		t.Fatalf("clipping split a UTF-8 sequence: %+v", long)
	}
}

// TestWorkflowStartVariablesReadsTheDocument covers the defensive read of a document the API never
// interprets: anything unexpected contributes no variables rather than failing the request.
func TestWorkflowStartVariablesReadsTheDocument(t *testing.T) {
	start := func(variables any) Object {
		return Object{"nodes": []any{
			Object{"id": "agent-1", "data": Object{"kind": "agent"}},
			Object{"id": "start-1", "data": Object{"kind": "start", "inputVariables": variables}},
			Object{"id": "start-2", "data": Object{"kind": "start", "inputVariables": []any{Object{"name": "second"}}}},
		}}
	}

	// The first Start node wins, matching the editor, which allows at most one.
	got := workflowStartVariables(decodeGraph(t, start([]any{Object{"name": "first"}})))
	if len(got) != 1 || got[0].S("name") != "first" {
		t.Fatalf("variables wrong: %+v", got)
	}
	none := decodeGraph(t, Object{"nodes": []any{Object{"id": "agent-1", "data": Object{"kind": "agent"}}}})
	if len(workflowStartVariables(none)) != 0 {
		t.Fatal("a graph with no Start node has no variables")
	}

	// Every shape a hand-edited or older document can take.
	malformed := []Object{
		{},
		{"nodes": "not a list"},
		{"nodes": []any{"not an object"}},
		{"nodes": []any{Object{"data": "not an object"}}},
		{"nodes": []any{Object{"data": Object{"kind": "start", "inputVariables": "not a list"}}}},
		{"nodes": []any{Object{"data": Object{"kind": "start", "inputVariables": []any{"not an object", 7}}}}},
	}
	for _, graph := range malformed {
		if got := workflowStartVariables(decodeGraph(t, graph)); len(got) != 0 {
			t.Fatalf("malformed graph %v produced variables: %+v", graph, got)
		}
	}

	// An empty document is an empty form, not a failed request. The column is decoded before it
	// reaches the projection, so this is what a row the workflow API did not write looks like here;
	// the decode's own tolerance is exercised against real jsonb by the integration suite.
	if d := formDescriptorFromGraph(&workflowFormSource{Ref: reviewRef, Graph: Object{}}, &IssueFormContext{}); len(d.Fields) != 0 {
		t.Fatalf("an empty graph produced fields: %+v", d.Fields)
	}
}

// TestClipShortensWithoutSplittingRunes covers the truncation the descriptor's own limits rely on.
func TestClipShortensWithoutSplittingRunes(t *testing.T) {
	if got := clip("short", 200); got != "short" {
		t.Fatalf("clip shortened a short string: %q", got)
	}
	wide := strings.Repeat("好", 100)
	got := clip(wide, 200)
	if len(got) != 198 || !utf8.ValidString(got) || !strings.HasPrefix(wide, got) {
		t.Fatalf("clip(%d bytes of 好, 200) = %d bytes, valid=%v", len(wide), len(got), utf8.ValidString(got))
	}
	if got := clip("abc", 0); got != "" {
		t.Fatalf("clip to nothing = %q", got)
	}
}

// TestWorkflowTargetIsFormMode covers the frozen projection the @ picker reads: Form Mode, no task,
// and the workflow id as the opaque formRef.
func TestWorkflowTargetIsFormMode(t *testing.T) {
	target := workflowTarget(reviewRef, "Review flow", "Reviews a change")

	if target.Type != "workflow" || target.ID != reviewRef || target.DisplayName != "Review flow" {
		t.Fatalf("target identity wrong: %+v", target)
	}
	mode, requiresTask := modeForType("workflow")
	if target.InteractionDescriptor.Mode != mode || target.InteractionDescriptor.RequiresTask != requiresTask {
		t.Fatalf("target disagrees with the frozen default: %+v", target.InteractionDescriptor)
	}
	if target.InteractionDescriptor.FormRef != reviewRef || !validOpaqueToken(target.InteractionDescriptor.FormRef) {
		t.Fatalf("formRef is not a usable opaque token: %q", target.InteractionDescriptor.FormRef)
	}
}

// fieldIndex returns the position of `key` in the projected form, or -1.
func fieldIndex(d FormDescriptor, key string) int {
	for i := range d.Fields {
		if d.Fields[i].Key == key {
			return i
		}
	}
	return -1
}

// issueContext is the context the platform fields are projected for: one issue whose project has a
// repository and a default branch, and which can name both a project and a parent for itself.
func issueContext() IssueFormContext {
	return IssueFormContext{
		IssueID:       "11111111-1111-4111-8111-111111111111",
		RepositoryURL: "https://github.com/ora/cloud",
		DefaultBranch: "main",
		ProjectID:     "22222222-2222-4222-8222-222222222222",
		ParentIssueID: "33333333-3333-4333-8333-333333333333",
	}
}

// bareIssueContext is an issue with no project at all: the context every optional value is missing
// from at once.
func bareIssueContext() IssueFormContext {
	return IssueFormContext{IssueID: "11111111-1111-4111-8111-111111111111"}
}

// snapshotRows builds the published-version rows the launch fields offer, newest first, the way the
// workflow_snapshots query hands them back.
func snapshotRows(versions ...int64) []Object {
	out := []Object{}
	for _, version := range versions {
		out = append(out, Object{
			"id":      fmt.Sprintf("0000000%d-0000-4000-8000-000000000000", version),
			"version": version,
			"name":    fmt.Sprintf("发布 %d", version),
		})
	}
	return out
}

// TestPlatformFieldsLeadTheForm covers the fields a form carries once it is opened for an issue: what
// they are, that they come first, and that each is prefilled from its own source — the issue's project
// repository and branch, the workflow's latest published version, and its Start prompt.
func TestPlatformFieldsLeadTheForm(t *testing.T) {
	d := projectGraph(t,
		startGraph([]any{Object{"name": "scope", "valueType": "string"}}, "Review the diff"),
		snapshotRows(3, 2, 1), issueContext())
	validateFormDescriptor(d)

	want := []string{fieldKeyRepository, fieldKeyBranch, fieldKeySnapshotID, fieldKeyPrompt, fieldKeyContextRefs, "scope"}
	if len(d.Fields) != len(want) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(want), d.Fields)
	}
	// They come first: they say what the run is about, the author's variables are its parameters.
	for i, key := range want {
		if d.Fields[i].Key != key {
			t.Fatalf("field %d = %q, want %q: %+v", i, d.Fields[i].Key, key, d.Fields)
		}
	}

	repository := fieldOf(t, d, fieldKeyRepository)
	if repository.Type != fieldText || !repository.Required || repository.Label == "" {
		t.Fatalf("repository field wrong: %+v", repository)
	}
	if repository.DefaultValue != issueContext().RepositoryURL {
		t.Fatalf("repository default = %v, want the project's repository", repository.DefaultValue)
	}

	// The branch pairs with the repository, and is prefilled from the same project.
	branch := fieldOf(t, d, fieldKeyBranch)
	if branch.Type != fieldText || !branch.Required {
		t.Fatalf("branch field wrong: %+v", branch)
	}
	if branch.DefaultValue != issueContext().DefaultBranch {
		t.Fatalf("branch default = %v, want the project's default branch", branch.DefaultValue)
	}

	// The prompt is optional: leaving it empty is how the run says "use the workflow's own prompt",
	// which is exactly the value it is prefilled with.
	prompt := fieldOf(t, d, fieldKeyPrompt)
	if prompt.Type != fieldTextarea || prompt.Required {
		t.Fatalf("prompt field wrong: %+v", prompt)
	}
	if prompt.DefaultValue != "Review the diff" {
		t.Fatalf("prompt default = %v, want the Start node's prompt", prompt.DefaultValue)
	}
}

// TestVersionFieldOffersPublishedSnapshots covers the run-version field: a select over the workflow's
// published versions, newest first and newest by default. The value is the snapshot id — what a run is
// created from — while the version number rides in the label, where it is read rather than consumed.
func TestVersionFieldOffersPublishedSnapshots(t *testing.T) {
	d := projectGraph(t, startGraph(nil, ""), snapshotRows(3, 2, 1), issueContext())
	validateFormDescriptor(d)

	version := fieldOf(t, d, fieldKeySnapshotID)
	if version.Type != fieldSelect || version.Required {
		t.Fatalf("version field wrong: %+v", version)
	}
	if len(version.Options) != 3 {
		t.Fatalf("option count = %d, want 3: %+v", len(version.Options), version.Options)
	}
	// Newest first, and the newest is what the form opens on.
	if version.Options[0].Value != snapshotRows(3)[0].S("id") || version.DefaultValue != version.Options[0].Value {
		t.Fatalf("version default = %v, want the newest snapshot", version.DefaultValue)
	}
	if version.Options[2].Value != snapshotRows(1)[0].S("id") {
		t.Fatalf("options are not newest first: %+v", version.Options)
	}
	if version.Options[0].Label != "v3 · 发布 3" {
		t.Fatalf("version label = %q, want the version number in it", version.Options[0].Label)
	}
}

// TestVersionFieldIsAbsentWithoutASnapshot covers the field that cannot be injected: a workflow nobody
// has published has no version to offer, and a select with no options fails descriptor validation
// closed, so the field is dropped rather than injected empty.
func TestVersionFieldIsAbsentWithoutASnapshot(t *testing.T) {
	d := projectGraph(t, startGraph(nil, ""), nil, issueContext())
	validateFormDescriptor(d)

	if fieldIndex(d, fieldKeySnapshotID) != -1 {
		t.Fatalf("an unpublished workflow grew a version field: %+v", d.Fields)
	}
	// A snapshot row whose id is not an id cannot become an option either.
	d = projectGraph(t, startGraph(nil, ""), []Object{{"id": "not-an-id", "version": 1}}, issueContext())
	validateFormDescriptor(d)
	if fieldIndex(d, fieldKeySnapshotID) != -1 {
		t.Fatalf("a malformed snapshot row produced a version field: %+v", d.Fields)
	}
}

// TestContextRefsFieldIsAdditiveOnly covers the supplementary context-reference field: it offers the
// two references an issue can name for itself, it opens on nothing selected, and it is dropped when the
// issue can name neither. It is additive because the issue's own references are already on every run it
// starts — a control that appeared to remove one would be lying.
func TestContextRefsFieldIsAdditiveOnly(t *testing.T) {
	d := projectGraph(t, startGraph(nil, ""), nil, issueContext())
	validateFormDescriptor(d)

	refs := fieldOf(t, d, fieldKeyContextRefs)
	if refs.Type != fieldMultiSelect || refs.Required {
		t.Fatalf("context refs field wrong: %+v", refs)
	}
	if refs.DefaultValue != nil {
		t.Fatalf("context refs default = %v, want none selected", refs.DefaultValue)
	}
	want := []string{"project:" + issueContext().ProjectID, "parent_issue:" + issueContext().ParentIssueID}
	if len(refs.Options) != len(want) {
		t.Fatalf("option count = %d, want %d: %+v", len(refs.Options), len(want), refs.Options)
	}
	for i, value := range want {
		if refs.Options[i].Value != value {
			t.Fatalf("option %d = %q, want %q", i, refs.Options[i].Value, value)
		}
	}

	// An issue with no project and no parent has nothing to offer, so the field is dropped: the
	// contract has no way to express a multiple-choice field with no choices.
	if bare := projectGraph(t, startGraph(nil, ""), nil, bareIssueContext()); fieldIndex(bare, fieldKeyContextRefs) != -1 {
		t.Fatalf("an issue that names no reference grew a context refs field: %+v", bare.Fields)
	}
}

// graphWithGlobals adds the graph-level global variables to a Start-only graph, the way the editor
// stores them: a top-level array of the document, never a node's data.
func graphWithGlobals(globals []any) Object {
	graph := startGraph(nil, "")
	graph["globalVariables"] = globals
	return graph
}

// TestGlobalVariablesBecomeFields covers the projection of the graph's global variables: each becomes
// an optional field carrying the constant the workflow declares, so leaving it alone runs the workflow
// with exactly what it declares.
func TestGlobalVariablesBecomeFields(t *testing.T) {
	globals := []any{
		Object{"name": "global.region", "valueType": "string", "value": "cn-north"},
		Object{"name": "global.retries", "valueType": "number", "value": float64(3)},
		Object{"name": "global.dryRun", "valueType": "boolean", "value": true},
		Object{"name": "global.token", "valueType": "secret", "value": "s3cret"},
		// A structured value has no Issues control, so it degrades to JSON text — and the declared
		// array is then not a legal value for that control, exactly as a Start variable's is not.
		Object{"name": "global.labels", "valueType": "array[string]", "value": []any{"a", "b"}},
	}
	d := projectGraph(t, graphWithGlobals(globals), nil, IssueFormContext{})
	validateFormDescriptor(d)

	if len(d.Fields) != len(globals) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(globals), d.Fields)
	}
	want := map[string]string{
		"global.region":  fieldText,
		"global.retries": fieldNumber,
		"global.dryRun":  fieldBoolean,
		"global.token":   fieldText,
		"global.labels":  fieldTextarea,
	}
	for key, kind := range want {
		field := fieldOf(t, d, key)
		if field.Type != kind {
			t.Fatalf("%s type = %q, want %q", key, field.Type, kind)
		}
		// Optional: the declared constant is the default, not a requirement.
		if field.Required {
			t.Fatalf("%s is required, want optional: %+v", key, field)
		}
	}
	for _, key := range []string{"global.region", "global.retries", "global.dryRun", "global.token"} {
		if got := fieldOf(t, d, key).DefaultValue; got == nil {
			t.Fatalf("%s lost its declared default", key)
		}
	}
	if got := fieldOf(t, d, "global.labels").DefaultValue; got != nil {
		t.Fatalf("global.labels default = %v, want none: an array is not a legal textarea value", got)
	}
}

// TestGlobalVariablesAreReadDefensively covers the read of a document the API never interprets: only a
// name the editor would have written becomes a field, and anything else contributes nothing rather
// than failing the request.
func TestGlobalVariablesAreReadDefensively(t *testing.T) {
	globals := []any{
		Object{"name": "noDot", "valueType": "string"},
		Object{"name": "global.ok", "valueType": "string"},
		Object{"name": "global.ok", "valueType": "string"},
		Object{"name": "global spaced", "valueType": "string"},
		Object{"name": "global.bad!", "valueType": "string"},
		"not an object",
	}
	d := projectGraph(t, graphWithGlobals(globals), nil, IssueFormContext{})
	validateFormDescriptor(d)

	// Only the one name that is both dotted and a valid field key survives, and only once.
	if len(d.Fields) != 1 || d.Fields[0].Key != "global.ok" {
		t.Fatalf("globals were not read defensively: %+v", d.Fields)
	}

	for _, graph := range []Object{
		{"globalVariables": "not a list"},
		{"globalVariables": []any{7}},
		{"globalVariables": nil},
	} {
		if got := projectGraph(t, graph, nil, IssueFormContext{}); len(got.Fields) != 0 {
			t.Fatalf("malformed globals %v produced fields: %+v", graph, got.Fields)
		}
	}
}

// TestGlobalsComeAfterTheAuthorsOwnFields covers the three bands the form is built in: the platform's
// fields, then the author's Start variables — this run's parameters — then the graph's global
// constants, which this invocation may override.
func TestGlobalsComeAfterTheAuthorsOwnFields(t *testing.T) {
	graph := startGraph([]any{Object{"name": "scope", "valueType": "string"}}, "")
	graph["globalVariables"] = []any{Object{"name": "global.region", "valueType": "string"}}
	d := projectGraph(t, graph, snapshotRows(1), issueContext())
	validateFormDescriptor(d)

	want := []string{fieldKeyRepository, fieldKeyBranch, fieldKeySnapshotID, fieldKeyPrompt, fieldKeyContextRefs, "scope", "global.region"}
	if len(d.Fields) != len(want) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(want), d.Fields)
	}
	for i, key := range want {
		if d.Fields[i].Key != key {
			t.Fatalf("field %d = %q, want %q: %+v", i, d.Fields[i].Key, key, d.Fields)
		}
	}
}

// TestLaunchKeysCollideLikeTheOlderOnes covers the collision rule for the three new reserved keys: an
// author who declares one keeps their own control, label and requiredness, and the platform only fills
// a default their control can actually hold. The version field is the exception that proves the rule —
// its default is a cursor rather than a value, so no control can hold it.
func TestLaunchKeysCollideLikeTheOlderOnes(t *testing.T) {
	declared := []any{
		Object{"name": fieldKeyBranch, "displayName": "Target branch", "valueType": "string"},
		Object{"name": fieldKeySnapshotID, "displayName": "Release", "valueType": "string"},
		Object{"name": fieldKeyContextRefs, "displayName": "Refs", "valueType": "string"},
	}
	d := projectGraph(t, startGraph(declared, ""), snapshotRows(2, 1), issueContext())
	validateFormDescriptor(d)

	// Nothing is prepended for a key the author declared, but the platform still injects the ones they
	// did not: the two leading fields are the platform's, the three trailing ones the author's.
	want := []string{fieldKeyRepository, fieldKeyPrompt, fieldKeyBranch, fieldKeySnapshotID, fieldKeyContextRefs}
	if len(d.Fields) != len(want) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(want), d.Fields)
	}
	for i, key := range want {
		if d.Fields[i].Key != key {
			t.Fatalf("field %d = %q, want %q: %+v", i, d.Fields[i].Key, key, d.Fields)
		}
	}
	branch := fieldOf(t, d, fieldKeyBranch)
	if branch.Label != "Target branch" || branch.Required {
		t.Fatalf("the author's branch control was overwritten: %+v", branch)
	}
	if branch.DefaultValue != issueContext().DefaultBranch {
		t.Fatalf("branch default = %v, want the project's branch", branch.DefaultValue)
	}
	// The version field is the one the platform contributes nothing to: its value is the cursor confirm
	// resolves the run's snapshot from, not a value the author's variable could hold, so a text control
	// must not be prefilled with a snapshot id — the run would receive that id as its own `version`.
	version := fieldOf(t, d, fieldKeySnapshotID)
	if version.Type != fieldText || version.Label != "Release" {
		t.Fatalf("the author's version control was overwritten: %+v", version)
	}
	if version.DefaultValue != nil {
		t.Fatalf("version default = %v, want none: the cursor is not a value the variable can hold", version.DefaultValue)
	}
	// The platform offers no default for the context refs at all — an empty selection is the only
	// honest starting point — so the author's declaration is left exactly as it was written.
	refs := fieldOf(t, d, fieldKeyContextRefs)
	if refs.Type != fieldText || refs.Label != "Refs" || refs.DefaultValue != nil {
		t.Fatalf("the author's context refs control was changed: %+v", refs)
	}
}

// TestWorkflowVersionCursorBelongsToTheAuthorWhenTheyDeclareIt covers the key a form value and a
// platform cursor can both be called. A graph whose own Start node declares `snapshotId` is asking for
// its own variable of that name, so the value under that key must not also select the snapshot: read as
// a cursor it would 404 on any value that is not a snapshot id, and on one that is, it would run the
// version the user picked while handing the workflow that same id as its parameter.
//
// The second half covers the pre-rename key (Issue collaboration D8). It is still read as a cursor, because a form
// rendered before the rename is confirmed after it and the selection it carries is the user's; the new
// key is read first so a submission carrying both is answered by the one this build wrote.
func TestWorkflowVersionCursorBelongsToTheAuthorWhenTheyDeclareIt(t *testing.T) {
	newest := snapshotRows(2, 1)[0].S("id")
	older := snapshotRows(2, 1)[1].S("id")
	declared := startGraph([]any{Object{"name": fieldKeySnapshotID, "valueType": "string"}}, "")
	declaredLegacy := startGraph([]any{Object{"name": legacyFieldKeyVersion, "valueType": "string"}}, "")
	plain := startGraph([]any{Object{"name": "scope", "valueType": "string"}}, "")
	cases := []struct {
		name   string
		graph  Object
		values Object
		want   string
	}{
		{
			name:   "the platform's own field carries the cursor",
			graph:  plain,
			values: Object{fieldKeySnapshotID: newest},
			want:   newest,
		},
		{
			name:   "no version is the newest published one",
			graph:  plain,
			values: Object{"repository": "ora-space/cloud"},
			want:   "",
		},
		{
			name:   "an author's version is a value, not a version selection",
			graph:  declared,
			values: Object{fieldKeySnapshotID: "1.2.3"},
			want:   "",
		},
		{
			name:   "an author's version holding a snapshot id still selects nothing",
			graph:  declared,
			values: Object{fieldKeySnapshotID: newest},
			want:   "",
		},
		{
			name:   "a form rendered before the rename still selects its version",
			graph:  plain,
			values: Object{legacyFieldKeyVersion: older},
			want:   older,
		},
		{
			name:   "the new key answers for a submission carrying both",
			graph:  plain,
			values: Object{fieldKeySnapshotID: newest, legacyFieldKeyVersion: older},
			want:   newest,
		},
		{
			name:  "the old key still selects when the author took the new one for a variable",
			graph: declared,
			values: Object{
				fieldKeySnapshotID:    "1.2.3",
				legacyFieldKeyVersion: older,
			},
			want: older,
		},
		{
			name:   "an author's old key is a value, not a version selection",
			graph:  declaredLegacy,
			values: Object{legacyFieldKeyVersion: "1.2.3"},
			want:   "",
		},
	}
	for _, tc := range cases {
		if got := workflowVersionCursor(decodeGraph(t, tc.graph), tc.values); got != tc.want {
			t.Fatalf("%s: cursor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFoldLegacyVersionAnswer covers what a confirm does with the pre-rename key once the cursor has
// read it. The values it stores and validates are the answer to the descriptor, and the `@` form is the
// one surface where the set of keys *is* the contract, so a key the descriptor does not declare has to
// be gone — but not before the cursor has seen it, and never by overwriting a variable the author
// declared under the name the platform took over.
func TestFoldLegacyVersionAnswer(t *testing.T) {
	newest := snapshotRows(2, 1)[0].S("id")
	older := snapshotRows(2, 1)[1].S("id")
	plain := decodeGraph(t, startGraph(nil, ""))
	declaredLegacy := decodeGraph(t, startGraph([]any{Object{"name": legacyFieldKeyVersion, "valueType": "string"}}, ""))
	declaredNew := decodeGraph(t, startGraph([]any{Object{"name": fieldKeySnapshotID, "valueType": "string"}}, ""))
	cases := []struct {
		name   string
		graph  Object
		values Object
		want   Object
	}{
		{
			name:   "the old key is rewritten under the new one",
			graph:  plain,
			values: Object{legacyFieldKeyVersion: older, "prompt": "go"},
			want:   Object{fieldKeySnapshotID: older, "prompt": "go"},
		},
		{
			name:   "an answer already under the new key is the one kept",
			graph:  plain,
			values: Object{fieldKeySnapshotID: newest, legacyFieldKeyVersion: older},
			want:   Object{fieldKeySnapshotID: newest},
		},
		{
			name:   "an author's own old key is left where it is",
			graph:  declaredLegacy,
			values: Object{legacyFieldKeyVersion: "1.2.3"},
			want:   Object{legacyFieldKeyVersion: "1.2.3"},
		},
		{
			name:  "the old key goes, and the author's new one is untouched",
			graph: declaredNew,
			values: Object{
				fieldKeySnapshotID:    "1.2.3",
				legacyFieldKeyVersion: older,
			},
			want: Object{fieldKeySnapshotID: "1.2.3"},
		},
		{
			name:   "a submission with no old key is left alone",
			graph:  plain,
			values: Object{fieldKeySnapshotID: newest},
			want:   Object{fieldKeySnapshotID: newest},
		},
	}
	for _, tc := range cases {
		foldLegacyVersionAnswer(tc.graph, tc.values)
		if !reflect.DeepEqual(tc.values, tc.want) {
			t.Fatalf("%s: values = %#v, want %#v", tc.name, tc.values, tc.want)
		}
	}
}

// TestWorkflowFieldDivergence covers what a confirm compares before it validates anything at all. The
// form the client rendered comes from the workflow's live document and the version it is about to
// execute comes from a snapshot, so the two have to agree about every field that decides whether a
// submission is acceptable — otherwise the version's schema refuses a value the user was legitimately
// shown, or insists on a field the form never showed them, and says either thing about the wrong
// document. What they do *not* have to agree about is how a control is drawn: a label, a description, a
// placeholder or a default changes no answer, and an edit that only renames a field must not strand a
// form the user has already filled in.
func TestWorkflowFieldDivergence(t *testing.T) {
	text := func(name string) Object {
		return Object{"name": name, "displayName": name, "fieldType": "text-input", "valueType": "string"}
	}
	required := func(name string) Object {
		return Object{"name": name, "displayName": name, "fieldType": "text-input", "valueType": "string", "required": true}
	}
	optioned := func(name string, values ...any) Object {
		return Object{"name": name, "displayName": name, "fieldType": "select", "valueType": "string", "options": values}
	}
	descriptor := func(graph Object) FormDescriptor {
		return projectGraph(t, graph, snapshotRows(2, 1), issueContext())
	}
	// The documents a confirm holds: one Start-only graph per way an edit can move a field.
	plain := startGraph(nil, "")
	scoped := startGraph([]any{text("scope")}, "")
	scopeRequired := startGraph([]any{required("scope")}, "")
	scopeSelected := startGraph([]any{optioned("scope", "a", "b")}, "")
	scopeWidened := startGraph([]any{optioned("scope", "a", "b", "c")}, "")
	scopeReordered := startGraph([]any{optioned("scope", "b", "a")}, "")
	scopeRenamed := startGraph([]any{Object{
		"name": "scope", "displayName": "评审范围", "fieldType": "text-input", "valueType": "string",
	}}, "")
	scopeDefaulted := startGraph([]any{Object{
		"name": "scope", "displayName": "Scope", "fieldType": "text-input", "valueType": "string", "value": "everything",
	}}, "")
	scopedPrompted := startGraph([]any{text("scope")}, "review the diff")
	// A draft that withdrew a platform field, and one that handed a platform key to the author: the two
	// ways an edit reaches a field the author did not write themselves in the Start node.
	withdrawn := launchGraph([]any{Object{"key": fieldKeyRepository, "enabled": false}})
	owned := startGraph([]any{Object{
		"name": fieldKeyRepository, "displayName": "Repository", "fieldType": "text-input", "valueType": "string",
	}}, "")
	left, right := startGraph([]any{text("left")}, ""), startGraph([]any{text("right")}, "")

	// The failure is a wire list, so the order it is reported in is part of the contract.
	cases := []struct {
		name              string
		rendered, version Object
		want              []string
	}{
		{"the same document on both sides", scoped, scoped, nil},
		{"a field the version does not declare", scoped, plain, []string{"scope"}},
		{"a field the version declares and the form does not", plain, scoped, []string{"scope"}},
		{"a field the version requires and the form does not", scoped, scopeRequired, []string{"scope"}},
		{"a field the form requires and the version does not", scopeRequired, scoped, []string{"scope"}},
		{"the same key as another control", scopeSelected, scoped, []string{"scope"}},
		{"the same control offering other values", scopeSelected, scopeWidened, []string{"scope"}},
		{"a field only one side has, each way", left, right, []string{"left", "right"}},
		{"a platform field the draft withdrew", withdrawn, plain, []string{fieldKeyRepository}},
		{"a platform key the draft handed to the author", owned, plain, []string{fieldKeyRepository}},
		// The same field, drawn differently.
		{"the same options in another order", scopeSelected, scopeReordered, nil},
		{"another label", scoped, scopeRenamed, nil},
		{"another default", scoped, scopeDefaulted, nil},
		{"another Start prompt", scopedPrompted, scoped, nil},
	}
	for _, tc := range cases {
		if got := workflowFieldDivergence(descriptor(tc.rendered), descriptor(tc.version)); !slices.Equal(got, tc.want) {
			t.Fatalf("%s: diverged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPlatformFieldsAreAbsentWithoutAnIssue covers the back-compatibility rule: a descriptor resolved
// with no issue context is the projection callers got before the platform fields existed — even when
// the graph would have supplied a prompt to prefill.
func TestPlatformFieldsAreAbsentWithoutAnIssue(t *testing.T) {
	d := projectGraph(t, startGraph([]any{Object{"name": "scope", "valueType": "string"}}, "Review the diff"), snapshotRows(1), IssueFormContext{})

	if len(d.Fields) != 1 || d.Fields[0].Key != "scope" {
		t.Fatalf("an issue-less descriptor grew platform fields: %+v", d.Fields)
	}
}

// TestPlatformFieldsPrefillOnlyWhatTheContextHas covers the missing halves: an issue with no project
// still gets the required repository and branch, just with nothing in them, and a workflow with no
// Start prompt still gets the optional one.
func TestPlatformFieldsPrefillOnlyWhatTheContextHas(t *testing.T) {
	d := projectGraph(t, startGraph(nil, ""), nil, bareIssueContext())
	validateFormDescriptor(d)

	want := []string{fieldKeyRepository, fieldKeyBranch, fieldKeyPrompt}
	if len(d.Fields) != len(want) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(want), d.Fields)
	}
	for _, key := range want {
		if got := fieldOf(t, d, key).DefaultValue; got != nil {
			t.Fatalf("%s default = %v, want none", key, got)
		}
	}
	// No default is not optionality: the repository and the branch are still what the user must fill in.
	if !fieldOf(t, d, fieldKeyRepository).Required || !fieldOf(t, d, fieldKeyBranch).Required {
		t.Fatal("a required field lost its requiredness along with its default")
	}
}

// TestPlatformPromptDefaultIsClippedAndTrimmed covers what the Start prompt contributes: whitespace is
// not a prompt, and an over-long one is shortened to what a form value may carry without splitting a
// UTF-8 sequence.
func TestPlatformPromptDefaultIsClippedAndTrimmed(t *testing.T) {
	blank := descriptorForIssue(t, nil, issueContext(), "   \n\t ")
	if got := fieldOf(t, blank, fieldKeyPrompt).DefaultValue; got != nil {
		t.Fatalf("a whitespace-only prompt produced a default: %q", got)
	}

	trimmed := descriptorForIssue(t, nil, issueContext(), "  Review the diff\n")
	if got := fieldOf(t, trimmed, fieldKeyPrompt).DefaultValue; got != "Review the diff" {
		t.Fatalf("prompt default = %q, want the trimmed prompt", got)
	}

	wide := strings.Repeat("好", maxFieldValueLength)
	long := descriptorForIssue(t, nil, issueContext(), wide)
	validateFormDescriptor(long)
	got, ok := fieldOf(t, long, fieldKeyPrompt).DefaultValue.(string)
	if !ok || len(got) > maxFieldValueLength || !utf8.ValidString(got) || !strings.HasPrefix(wide, got) {
		t.Fatalf("prompt default was not clipped to a legal value: %d bytes, valid=%v", len(got), utf8.ValidString(got))
	}
}

// TestAuthorDeclaredKeysWin covers the collision rule, which is a real case rather than a hypothetical
// one: a workflow whose Start node declares a `repository` variable is how one would have expressed
// this before the platform field existed. The author's control, label and requiredness are kept, and
// the platform contributes only the default the author left empty.
func TestAuthorDeclaredKeysWin(t *testing.T) {
	declared := []any{
		Object{"name": fieldKeyRepository, "displayName": "Repo", "valueType": "string"},
		Object{"name": fieldKeyPrompt, "displayName": "Instructions", "valueType": "paragraph"},
	}
	d := descriptorForIssue(t, declared, issueContext(), "Review the diff")
	validateFormDescriptor(d)

	// Nothing is prepended for a key the author declared, but the platform fields they did not declare
	// are still injected and still lead the form.
	want := []string{fieldKeyBranch, fieldKeyContextRefs, fieldKeyRepository, fieldKeyPrompt}
	if len(d.Fields) != len(want) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(want), d.Fields)
	}
	for i, key := range want {
		if d.Fields[i].Key != key {
			t.Fatalf("field %d = %q, want %q: %+v", i, d.Fields[i].Key, key, d.Fields)
		}
	}
	repository := fieldOf(t, d, fieldKeyRepository)
	// The author declared it optional, and the platform's own requiredness does not override that: the
	// declaration is the author's, only the default is the platform's.
	if repository.Label != "Repo" || repository.Required {
		t.Fatalf("the author's control was overwritten: %+v", repository)
	}
	if repository.DefaultValue != issueContext().RepositoryURL {
		t.Fatalf("repository default = %v, want the platform's", repository.DefaultValue)
	}
	// The author declared the prompt as a paragraph, so that is what it stays — the platform's own
	// definition of it is a textarea and must not win.
	prompt := fieldOf(t, d, fieldKeyPrompt)
	if prompt.Label != "Instructions" || prompt.Type != fieldTextarea || prompt.Required {
		t.Fatalf("the author's prompt declaration was overwritten: %+v", prompt)
	}
	if prompt.DefaultValue != "Review the diff" {
		t.Fatalf("prompt default = %v, want the Start node's prompt", prompt.DefaultValue)
	}

	// A default the author already wrote is theirs; the platform does not replace it.
	own := descriptorForIssue(t, []any{Object{"name": fieldKeyRepository, "valueType": "string", "value": "https://example.com/own"}}, issueContext(), "")
	if got := fieldOf(t, own, fieldKeyRepository).DefaultValue; got != "https://example.com/own" {
		t.Fatalf("the author's own default was replaced: %v", got)
	}
}

// TestAuthorControlRejectsThePlatformDefault covers the other half of the collision rule: a default
// that is not a legal value for the author's control is dropped rather than forced, because an
// illegal default makes the whole descriptor invalid and takes every field down with it.
func TestAuthorControlRejectsThePlatformDefault(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		declared Object
	}{
		{
			"a repository declared as a select that does not offer it",
			fieldKeyRepository,
			Object{"name": fieldKeyRepository, "valueType": "string", "fieldType": "select", "options": []any{"gitlab", "bitbucket"}},
		},
		{
			"a prompt declared as a number",
			fieldKeyPrompt,
			Object{"name": fieldKeyPrompt, "valueType": "number"},
		},
		{
			"a prompt declared as a checkbox",
			fieldKeyPrompt,
			Object{"name": fieldKeyPrompt, "valueType": "boolean"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Only the colliding key is declared, so the other platform field is still injected: the
			// assertion has to name the field it means rather than read whatever landed first.
			d := descriptorForIssue(t, []any{c.declared}, issueContext(), "Review the diff")
			validateFormDescriptor(d)
			if got := fieldOf(t, d, c.key).DefaultValue; got != nil {
				t.Fatalf("an illegal default was forced onto the author's control: %v", got)
			}
		})
	}
}

// TestPlatformFieldsRespectTheFieldCap covers the cap: a workflow whose own form already fills the
// descriptor gets no platform field rather than a descriptor that fails validation and turns every
// confirm into a 500.
func TestPlatformFieldsRespectTheFieldCap(t *testing.T) {
	declared := []any{}
	for i := range maxFormFields {
		declared = append(declared, Object{"name": "field" + itoa(i), "valueType": "string"})
	}
	d := descriptorForIssue(t, declared, issueContext(), "Review the diff")
	validateFormDescriptor(d)

	if len(d.Fields) != maxFormFields {
		t.Fatalf("field count = %d, want %d", len(d.Fields), maxFormFields)
	}
	if fieldIndex(d, fieldKeyRepository) != -1 || fieldIndex(d, fieldKeyPrompt) != -1 {
		t.Fatalf("platform fields were appended past the cap: %+v", d.Fields)
	}
}

// TestWorkflowStartPromptReadsTheDocument covers the defensive read of the Start prompt, and the rule
// that keeps it from ever describing a different node than the variables do.
func TestWorkflowStartPromptReadsTheDocument(t *testing.T) {
	// The first Start node wins for both reads, so a graph carrying two of them cannot produce a
	// prompt from one and variables from the other.
	twoStarts := decodeGraph(t, Object{"nodes": []any{
		Object{"id": "start-1", "data": Object{"kind": "start", "input": "first prompt", "inputVariables": []any{Object{"name": "first"}}}},
		Object{"id": "start-2", "data": Object{"kind": "start", "input": "second prompt", "inputVariables": []any{Object{"name": "second"}}}},
	}})
	if got := workflowStartPrompt(twoStarts); got != "first prompt" {
		t.Fatalf("prompt = %q, want the first Start node's", got)
	}
	if got := workflowStartVariables(twoStarts); len(got) != 1 || got[0].S("name") != "first" {
		t.Fatalf("variables did not come from the first Start node: %+v", got)
	}

	// Every shape a hand-edited or older document can take contributes no prompt.
	malformed := []Object{
		{},
		{"nodes": "not a list"},
		{"nodes": []any{"not an object"}},
		{"nodes": []any{Object{"data": "not an object"}}},
		{"nodes": []any{Object{"data": Object{"kind": "agent", "input": "ignored"}}}},
		{"nodes": []any{Object{"data": Object{"kind": "start"}}}},
		{"nodes": []any{Object{"data": Object{"kind": "start", "input": 7}}}},
		{"nodes": []any{Object{"data": Object{"kind": "start", "input": nil}}}},
	}
	for _, graph := range malformed {
		if got := workflowStartPrompt(decodeGraph(t, graph)); got != "" {
			t.Fatalf("malformed graph %v produced the prompt %q", graph, got)
		}
	}
}
