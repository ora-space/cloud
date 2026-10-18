package core

import (
	"reflect"
	"testing"
)

// platformLaunchDefaults is the platform's own answer for each field: asked for, and required — or not.
// It is written out here rather than read back out of the code under test, so the assertions below say
// what the behavior must be instead of restating whatever the catalog happens to say.
var platformLaunchDefaults = []struct {
	Key      string
	Required bool
}{
	{fieldKeyRepository, true},
	{fieldKeyBranch, true},
	{fieldKeySnapshotID, false},
	{fieldKeyPrompt, false},
	{fieldKeyContextRefs, false},
}

// explicitLaunchDefaults is that same answer written as the declaration the editor saves when the author
// changes nothing: all five keys named, `enabled` and `required` both spelled out.
func explicitLaunchDefaults() []any {
	out := []any{}
	for _, entry := range platformLaunchDefaults {
		out = append(out, Object{"key": entry.Key, "enabled": true, "required": entry.Required})
	}
	return out
}

// launchGraph adds the author's launch-field declaration (Issue collaboration D8) to a Start-only graph, the way the
// editor stores it: a top-level array of the document, beside the global variables.
func launchGraph(launchFields []any) Object {
	graph := startGraph(nil, "")
	graph["launchFields"] = launchFields
	return graph
}

// launchDescriptor projects a graph declaring `launchFields` for the standard issue context, with
// `snapshots` published.
func launchDescriptor(t *testing.T, launchFields []any, snapshots []Object) FormDescriptor {
	t.Helper()
	return projectGraph(t, launchGraph(launchFields), snapshots, issueContext())
}

// TestLaunchFieldsDefaultToTheWholeCatalog covers the back-compatibility rule and the shape of the
// catalog at once: a workflow that declares nothing is asked exactly the five platform fields, with
// the platform's own requiredness — and one that spells those defaults out field by field is asked
// precisely the same form, which is what makes the declaration a narrowing rather than a redefinition.
func TestLaunchFieldsDefaultToTheWholeCatalog(t *testing.T) {
	undeclared := projectGraph(t, startGraph(nil, ""), snapshotRows(2, 1), issueContext())
	explicit := launchDescriptor(t, explicitLaunchDefaults(), snapshotRows(2, 1))
	validateFormDescriptor(undeclared)
	validateFormDescriptor(explicit)

	if len(undeclared.Fields) != len(platformLaunchDefaults) {
		t.Fatalf("field count = %d, want %d: %+v", len(undeclared.Fields), len(platformLaunchDefaults), undeclared.Fields)
	}
	for i, want := range platformLaunchDefaults {
		got := undeclared.Fields[i]
		if got.Key != want.Key || got.Required != want.Required {
			t.Fatalf("field %d = %s (required=%v), want %s (required=%v)",
				i, got.Key, got.Required, want.Key, want.Required)
		}
	}
	// Same keys in the same order with the same requiredness: naming the defaults one by one must not be
	// a different form from saying nothing at all.
	if !reflect.DeepEqual(undeclared.Fields, explicit.Fields) {
		t.Fatalf("spelling out the defaults changed the form:\n%+v\n%+v", undeclared.Fields, explicit.Fields)
	}
}

// TestLaunchFieldsDeclarationDropsFields covers the withdrawal: an author who does not want the form to
// ask for a field says so, and it is not asked for. The repository is in here deliberately — it is the
// field the platform considers required, and it is still the author's to withdraw, because a workflow
// that does not care which repository it runs against is a choice it is allowed to make.
func TestLaunchFieldsDeclarationDropsFields(t *testing.T) {
	d := launchDescriptor(t, []any{
		Object{"key": fieldKeyRepository, "enabled": false},
		Object{"key": fieldKeySnapshotID, "enabled": false},
		Object{"key": fieldKeyContextRefs, "enabled": false},
	}, snapshotRows(2, 1))
	validateFormDescriptor(d)

	want := []string{fieldKeyBranch, fieldKeyPrompt}
	if len(d.Fields) != len(want) {
		t.Fatalf("field count = %d, want %d: %+v", len(d.Fields), len(want), d.Fields)
	}
	for i, key := range want {
		if d.Fields[i].Key != key {
			t.Fatalf("field %d = %q, want %q: %+v", i, d.Fields[i].Key, key, d.Fields)
		}
	}

	// Withdrawing every one of them is legal, and asks nothing at all: the author's form is then the
	// whole form, and a descriptor with no fields is one the contract admits.
	none := []any{}
	for _, entry := range platformLaunchDefaults {
		none = append(none, Object{"key": entry.Key, "enabled": false})
	}
	empty := launchDescriptor(t, none, snapshotRows(2, 1))
	validateFormDescriptor(empty)
	if len(empty.Fields) != 0 {
		t.Fatalf("a workflow that withdrew every field still asks something: %+v", empty.Fields)
	}
}

// TestLaunchFieldsDeclarationAcceptsThePreRenameKey covers the declaration written before the field was
// renamed (Issue collaboration D8). A snapshot published then carries `version` where this build reads `snapshotId`,
// and the graph of a published snapshot is immutable — so reading the entry strictly would hand the
// field back to a workflow whose author had withdrawn it, which is the silent-ignore the declaration
// exists to prevent. The rename's other half is unchanged: the entry is read as the snapshot selector
// and nothing else.
func TestLaunchFieldsDeclarationAcceptsThePreRenameKey(t *testing.T) {
	d := launchDescriptor(t, []any{
		Object{"key": legacyFieldKeyVersion, "enabled": false},
	}, snapshotRows(2, 1))
	validateFormDescriptor(d)

	if fieldIndex(d, fieldKeySnapshotID) != -1 {
		t.Fatalf("a withdrawn snapshot field came back: %+v", d.Fields)
	}
	if fieldIndex(d, legacyFieldKeyVersion) != -1 {
		t.Fatalf("the pre-rename key reached the descriptor: %+v", d.Fields)
	}

	// The declaration still carries the author's requiredness for the field, under the old name.
	required := launchDescriptor(t, []any{
		Object{"key": legacyFieldKeyVersion, "enabled": true, "required": true},
	}, snapshotRows(2, 1))
	validateFormDescriptor(required)
	if !fieldOf(t, required, fieldKeySnapshotID).Required {
		t.Fatalf("the pre-rename declaration lost its requiredness: %+v", required.Fields)
	}
}

// TestLaunchFieldsDeclarationSetsRequiredness covers the other half of the declaration: the author
// decides which of the fields they keep must be answered. The prompt becomes required and the branch
// stops being required, both against the platform's own answer — and the fields they said nothing about
// keep it.
func TestLaunchFieldsDeclarationSetsRequiredness(t *testing.T) {
	d := launchDescriptor(t, []any{
		Object{"key": fieldKeyPrompt, "required": true},
		Object{"key": fieldKeyBranch, "required": false},
	}, snapshotRows(2, 1))
	validateFormDescriptor(d)

	if !fieldOf(t, d, fieldKeyPrompt).Required {
		t.Fatal("the prompt the author made required is not required")
	}
	if fieldOf(t, d, fieldKeyBranch).Required {
		t.Fatal("the branch the author made optional is still required")
	}
	if !fieldOf(t, d, fieldKeyRepository).Required {
		t.Fatal("a field the declaration did not mention lost the platform's requiredness")
	}
	if fieldOf(t, d, fieldKeySnapshotID).Required {
		t.Fatal("a field the declaration did not mention gained requiredness")
	}
	// Optional is not empty: the branch is still prefilled from the issue's project, the author has only
	// stopped insisting on it.
	if got := fieldOf(t, d, fieldKeyBranch).DefaultValue; got != issueContext().DefaultBranch {
		t.Fatalf("branch default = %v, want the project's default branch", got)
	}

	// A key named twice is read as the later entry, so a document written by hand cannot make the
	// declaration mean two things at once.
	twice := launchDescriptor(t, []any{
		Object{"key": fieldKeyPrompt, "required": true},
		Object{"key": fieldKeyPrompt, "required": false},
	}, nil)
	if fieldOf(t, twice, fieldKeyPrompt).Required {
		t.Fatal("the first entry for a key won over the last")
	}
}

// TestLaunchFieldsReadsTheDeclarationDefensively covers every shape a hand-edited, imported or older
// document can take. The declaration is read the way the globals projection reads the same document:
// anything it cannot understand contributes nothing rather than failing the request, so a workflow whose
// declaration is nonsense is asked exactly the form it would have had without one.
func TestLaunchFieldsReadsTheDeclarationDefensively(t *testing.T) {
	undeclared := projectGraph(t, startGraph(nil, ""), snapshotRows(2, 1), issueContext())

	malformed := []any{
		"not a list",
		Object{"key": fieldKeyRepository},
		[]any{"not an object", 7, nil},
		[]any{Object{}},
		[]any{Object{"key": 7, "enabled": false}},
		[]any{Object{"key": fieldKeyRepository, "enabled": "false"}},
		[]any{Object{"key": fieldKeyPrompt, "required": "true"}},
		[]any{Object{"key": fieldKeySnapshotID, "required": nil}},
		// A key the platform does not have cannot be withdrawn, and cannot be added either: the
		// declaration names the catalog, it does not extend it.
		[]any{Object{"key": "deploy_target", "enabled": false, "required": true}},
		[]any{Object{"key": "launchFields", "enabled": false}},
	}
	for _, declared := range malformed {
		graph := startGraph(nil, "")
		graph["launchFields"] = declared
		d := projectGraph(t, graph, snapshotRows(2, 1), issueContext())
		validateFormDescriptor(d)
		if !reflect.DeepEqual(undeclared.Fields, d.Fields) {
			t.Fatalf("the declaration %#v changed the form:\n%+v\n%+v", declared, undeclared.Fields, d.Fields)
		}
	}
}

// TestLaunchFieldsCannotOfferWhatThePlatformDoesNotHave covers the rule that outranks the declaration: a
// field with nothing to offer is dropped however plainly the author asks for it. The version field needs
// a published snapshot and the context-refs field needs an issue that can name a reference, and a select
// with no options fails descriptor validation closed — so honoring the request would turn every confirm
// into a 500 rather than producing the field.
func TestLaunchFieldsCannotOfferWhatThePlatformDoesNotHave(t *testing.T) {
	// Nothing published, and the author asked for the version field all the same.
	unpublished := launchDescriptor(t, []any{Object{"key": fieldKeySnapshotID, "enabled": true, "required": true}}, nil)
	validateFormDescriptor(unpublished)
	if fieldIndex(unpublished, fieldKeySnapshotID) != -1 {
		t.Fatalf("an unpublished workflow grew a version field: %+v", unpublished.Fields)
	}

	// An issue that can name neither a project nor a parent, with the context-refs field asked for.
	bare := projectGraph(t, launchGraph([]any{Object{"key": fieldKeyContextRefs, "enabled": true}}), nil, bareIssueContext())
	validateFormDescriptor(bare)
	if fieldIndex(bare, fieldKeyContextRefs) != -1 {
		t.Fatalf("an issue that names no reference grew a context refs field: %+v", bare.Fields)
	}

	// Withdrawing is still the author's to do, and the two rules compose: a field the platform cannot
	// offer is absent whether the declaration asks for it or says nothing.
	withdrawn := launchDescriptor(t, []any{Object{"key": fieldKeySnapshotID, "enabled": false}}, nil)
	validateFormDescriptor(withdrawn)
	if fieldIndex(withdrawn, fieldKeySnapshotID) != -1 {
		t.Fatalf("an unpublished workflow grew a version field: %+v", withdrawn.Fields)
	}
}

// TestLaunchFieldsAreInertForTheAuthorsOwnKeys covers the collision, which is a real case rather than a
// hypothetical one: a workflow whose Start node declares a `repository` variable is how one would have
// expressed this before the platform field existed. The author's control wins outright, and the
// declaration's entry for that key is then inert — it neither withdraws a field the author asked for
// themselves nor sets its requiredness — while the platform still contributes the default they left
// empty. The other keys are unaffected, which is what the branch beside it shows.
func TestLaunchFieldsAreInertForTheAuthorsOwnKeys(t *testing.T) {
	graph := startGraph([]any{Object{"name": fieldKeyRepository, "displayName": "Repo", "valueType": "string"}}, "")
	graph["launchFields"] = []any{
		Object{"key": fieldKeyRepository, "enabled": false, "required": true},
		Object{"key": fieldKeyBranch, "enabled": false},
	}
	d := projectGraph(t, graph, nil, issueContext())
	validateFormDescriptor(d)

	repository := fieldOf(t, d, fieldKeyRepository)
	if repository.Label != "Repo" || repository.Required {
		t.Fatalf("the author's control was overwritten: %+v", repository)
	}
	if repository.DefaultValue != issueContext().RepositoryURL {
		t.Fatalf("the platform's default was withdrawn along with the declaration: %v", repository.DefaultValue)
	}
	if fieldIndex(d, fieldKeyBranch) != -1 {
		t.Fatalf("the declaration did not withdraw a field the author had not declared: %+v", d.Fields)
	}
}
