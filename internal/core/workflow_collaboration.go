package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Workflow-backed collaboration ports. The workflow domain owns the `workflows` document, so it is
// also the adapter that projects it into the two Issues-facing shapes: a `workflow` @ target, and the
// Form Mode descriptor that target advertises. Both read the same row, so they cannot disagree and no
// separate registration step exists.
//
// These are the production implementations of the ports declared in collaboration.go, wired by
// NewStore rather than by the development fixture gate: a deployment that never enables the Agent/Team
// fixtures still serves real workflow targets. Cloud has no Agent/Team backend, so this directory
// answers the `workflow` target type only — agent and team resolve to not-found, exactly as they do
// with no directory at all.

// WorkflowDirectory lists and resolves the tenant's live workflows as collaboration targets.
type WorkflowDirectory struct{ Pool *sql.DB }

// ListTargets returns the tenant's live workflows, newest name first, optionally filtered by a name
// substring. Archived workflows are never discoverable.
func (d WorkflowDirectory) ListTargets(ctx context.Context, tenantID, query string) ([]CollaborationTargetSummary, error) {
	q := "SELECT id, name, description FROM workflows WHERE tenant_id=$1 AND deleted_at IS NULL"
	args := []any{tenantID}
	if term := strings.TrimSpace(query); term != "" {
		args = append(args, likePattern(term))
		q += " AND name ILIKE $2"
	}
	q += " ORDER BY name, id"
	rows, err := d.Pool.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CollaborationTargetSummary{}
	for rows.Next() {
		var id, name, description string
		if err := rows.Scan(&id, &name, &description); err != nil {
			return nil, err
		}
		out = append(out, workflowTarget(id, name, description))
	}
	return out, rows.Err()
}

// ResolveTarget resolves one live workflow in the tenant. A target type Cloud has no backend for is
// not-found, never an error: the caller turns it into the same 404 an unknown id produces.
func (d WorkflowDirectory) ResolveTarget(ctx context.Context, tenantID, targetType, targetID string) (CollaborationTargetSummary, bool, error) {
	if targetType != "workflow" || !validID(targetID) {
		return CollaborationTargetSummary{}, false, nil
	}
	var name, description string
	err := d.Pool.QueryRowContext(ctx, "SELECT name, description FROM workflows WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", targetID, tenantID).Scan(&name, &description)
	if errors.Is(err, sql.ErrNoRows) {
		return CollaborationTargetSummary{}, false, nil
	}
	if err != nil {
		return CollaborationTargetSummary{}, false, err
	}
	return workflowTarget(targetID, name, description), true, nil
}

// workflowTarget is the frozen projection of one workflow (Issue collaboration D3): Form Mode, no task required, and
// the workflow id as the opaque formRef. Deriving the ref from the id rather than storing one keeps
// the advertised ref and the resolvable workflow from ever drifting apart, and keeps it a valid
// opaque token without a second identifier to constrain.
func workflowTarget(id, name, description string) CollaborationTargetSummary {
	return CollaborationTargetSummary{
		Type:        "workflow",
		ID:          id,
		DisplayName: name,
		Description: description,
		InteractionDescriptor: InteractionDescriptor{
			Mode:         "form",
			RequiresTask: false,
			FormRef:      id,
		},
	}
}

// WorkflowFormDescriptors resolves the Form Mode descriptor of one workflow: the input form its Start
// node declares, projected onto the rendering controls Issues can draw (Issue collaboration D7). It is deliberately
// not the workflow's canonical schema — the graph stays the authority and this is one projection of it.
type WorkflowFormDescriptors struct{ Pool *sql.DB }

// ResolveFormDescriptor resolves `formRef`, which is a workflow id. A ref that is not an id, or names
// no live workflow in this tenant, is not-found. `issue` tailors the form to the issue it was opened
// from; its zero value projects the graph exactly as it would be projected without any issue at all.
//
//nolint:gocritic // value-typed issue context mirrors the FormDescriptorProvider port this provider satisfies
func (p WorkflowFormDescriptors) ResolveFormDescriptor(ctx context.Context, tenantID, formRef string, issue IssueFormContext) (FormDescriptor, bool, error) {
	if !validID(formRef) {
		return FormDescriptor{}, false, nil
	}
	var name, description string
	var raw []byte
	err := p.Pool.QueryRowContext(ctx, "SELECT name, description, graph FROM workflows WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", formRef, tenantID).Scan(&name, &description, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return FormDescriptor{}, false, nil
	}
	if err != nil {
		return FormDescriptor{}, false, err
	}
	// The column is a NOT NULL jsonb object, so a decode failure means the row was not written by the
	// workflow API. An unreadable graph yields an empty form rather than a failed request.
	graph := Object{}
	_ = json.Unmarshal(raw, &graph)
	source := workflowFormSource{Ref: formRef, Title: name, Description: description, Graph: graph}
	// Only an issue-scoped form carries the launch fields, so only it pays for the second query: a
	// caller with no issue context gets the same single lookup it always did.
	if issue.IssueID != "" {
		source.Snapshots, err = p.snapshots(ctx, tenantID, formRef)
		if err != nil {
			return FormDescriptor{}, false, err
		}
	}
	return formDescriptorFromGraph(&source, &issue), true, nil
}

// snapshots lists the workflow's published versions, newest first, capped at the number of options a
// descriptor may carry: a workflow published more than a hundred times would otherwise build a
// descriptor that fails validation closed and takes every confirm down with it.
//
// Snapshots are immutable and never deleted — the table has no `deleted_at` — so there is no
// soft-delete filter, and the ordering is the one the existing list index already serves.
func (p WorkflowFormDescriptors) snapshots(ctx context.Context, tenantID, workflowID string) ([]Object, error) {
	rows, err := p.Pool.QueryContext(ctx, "SELECT id, version, name FROM workflow_snapshots WHERE tenant_id=$1 AND workflow_id=$2 ORDER BY version DESC LIMIT $3", tenantID, workflowID, maxFormOptions)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Object{}
	for rows.Next() {
		var id, name string
		var version int64
		if err := rows.Scan(&id, &version, &name); err != nil {
			return nil, err
		}
		// Scanned straight into int64 rather than read back as a decoded number: `version` is a
		// bigint, and everything that comes through a JSON round-trip arrives as a float64.
		out = append(out, Object{"id": id, "version": version, "name": name})
	}
	return out, rows.Err()
}

// workflowFormSource is everything one descriptor projection reads: the workflow's own identity and
// document, plus the published versions the launch fields offer. Bundled rather than passed one by
// one so the projection keeps a single argument that grows with the form.
type workflowFormSource struct {
	Ref         string
	Title       string
	Description string
	Graph       Object
	Snapshots   []Object
}

// formDescriptorFromGraph projects a stored graph onto a form descriptor. Title and description are
// the workflow's own, clipped to the lengths the descriptor contract accepts: a workflow description
// is free text with no length rule of its own, and a descriptor that fails validation is a 500.
//
// The fields come out in three bands, in this order: the platform's, then the author's Start
// variables, then the graph's global variables. The platform fields are injected only when there is an
// issue to inject them for: a caller with no issue context gets the author's own form, byte for byte
// what it was before the fields existed.
func formDescriptorFromGraph(source *workflowFormSource, issue *IssueFormContext) FormDescriptor {
	fields := workflowInputFields(source.Graph)
	if issue.IssueID != "" {
		fields = injectPlatformFields(fields, issue, workflowStartPrompt(source.Graph), source.Snapshots, launchFieldsFromGraph(source.Graph))
	}
	return FormDescriptor{
		FormRef:     source.Ref,
		Title:       clip(source.Title, 200),
		Description: clip(source.Description, 2000),
		Fields:      fields,
	}
}

// workflowInputFields projects the values one run of this graph may set: the Start node's declared
// variables, then the graph's global variables. It is the schema side of both projections of a graph —
// the form a user fills in and the input a run is admitted with — so the two can never disagree about
// what a variable is called, what type it takes or whether it is required.
//
// A variable the projection cannot express as a valid field is dropped, exactly as it is from the
// form: it could not have been filled in, so treating it as a missing required value would refuse
// every run of a graph that published fine.
func workflowInputFields(graph Object) []FormField {
	fields := []FormField{}
	for _, variable := range workflowStartVariables(graph) {
		if len(fields) == maxFormFields {
			break
		}
		if field, ok := startInputField(variable); ok {
			fields = append(fields, field)
		}
	}
	return appendGlobalVariableFields(fields, graph)
}

// workflowSnapshotOptions lists a workflow's published versions newest first, in the shape the launch
// field offers them. It is the transaction-scoped twin of the descriptor provider's own query, for the
// callers that already hold a transaction and must not open a second connection inside it.
func workflowSnapshotOptions(t *transaction, tid, wid string) []Object {
	return t.list("SELECT id, version, name FROM workflow_snapshots WHERE tenant_id=$1 AND workflow_id=$2 ORDER BY version DESC LIMIT $3", tid, wid, maxFormOptions)
}

// workflowDescriptorSource is the projection input for one graph of a workflow: its live document, or
// one of its frozen versions. Both descriptors a confirm compares are built from here, so the graph is
// the only thing that can differ between them — a source assembled any other way could make them
// differ for a reason that has nothing to do with what the user was shown.
func workflowDescriptorSource(t *transaction, w, graph Object) *workflowFormSource {
	return &workflowFormSource{
		Ref:         w.S("id"),
		Title:       w.S("name"),
		Description: w.S("description"),
		Graph:       graph,
		Snapshots:   workflowSnapshotOptions(t, w.S("tenantId"), w.S("id")),
	}
}

// liveFormDescriptor projects a workflow's live document — the document the client rendered the form
// from, since the read routes resolve it through WorkflowFormDescriptors. A confirm projects it here
// rather than through that provider because the provider may not be wired at all: what a confirm needs
// must not depend on a service being available, which is the same reason it resolves the version itself.
func liveFormDescriptor(t *transaction, w Object, issue *IssueFormContext) FormDescriptor {
	return formDescriptorFromGraph(workflowDescriptorSource(t, w, w.O("graph")), issue)
}

// snapshotFormDescriptor projects one *frozen* snapshot onto the form descriptor a confirm validates
// against, rather than the live document the form was rendered from.
//
// The distinction is the whole point of confirming against a version: an edit made after the last
// publish must not retroactively change what an already-offered version accepts, and a variable an
// older version declared must still be submittable when that version is the one chosen. The
// platform's own launch fields are still injected — they describe the invocation, not the version —
// so the descriptor carries the same field set the form showed for this version.
//
// That last clause is a promise this projection cannot keep on its own: the form was rendered from the
// live document, so the two describe the same fields only while the draft has not moved on from this
// version. A confirm checks that before it validates — see workflowFieldDivergence.
func snapshotFormDescriptor(t *transaction, w, s Object, issue *IssueFormContext) FormDescriptor {
	descriptor := formDescriptorFromGraph(workflowDescriptorSource(t, w, s.O("graph")), issue)
	// Fail closed, exactly as the provider path does: a descriptor that cannot be rendered is a 500
	// rather than a form that silently accepts less than it shows.
	validateFormDescriptor(descriptor)
	return descriptor
}

// judgedField is what a descriptor says about one key that decides whether a submitted value is
// accepted: which control receives it, whether the run must be given one, and which values it offers.
// A label, a description, a placeholder and a default judge none of that — they change what the form
// looks like, not what it takes — so they are deliberately left out and a draft that changed only one
// of them is not a divergence.
type judgedField struct {
	Type     string
	Required bool
	Options  []string
}

// sameAs compares two judged controls. The offered values are compared as a set: reordering a select's
// options changes the order the form draws them in, not which submissions it accepts.
func (f judgedField) sameAs(other judgedField) bool {
	return f.Type == other.Type && f.Required == other.Required && slices.Equal(f.Options, other.Options)
}

// judgedFields reduces a descriptor to the keys that judge a submission, for comparison.
func judgedFields(d FormDescriptor) map[string]judgedField {
	out := make(map[string]judgedField, len(d.Fields))
	for i := range d.Fields {
		field := &d.Fields[i]
		options := make([]string, 0, len(field.Options))
		for _, option := range field.Options {
			options = append(options, option.Value)
		}
		slices.Sort(options)
		out[field.Key] = judgedField{Type: field.Type, Required: field.Required, Options: options}
	}
	return out
}

// workflowFieldDivergence names the fields on which the form the client rendered and the version being
// confirmed disagree, sorted, or nothing when they agree about every field.
//
// A confirm validates the submitted values against the version it will execute, and that is only a
// meaningful thing to do while the version still describes the form the user filled in. The form is
// rendered from the live document — a draft is a legitimate document — so an author editing between
// publishing a version and confirming against it hands the client a form that version does not accept:
// the user is shown a field the version rejects as unknown, or is never shown one the version insists
// on, and either way the submission can only be refused by a fault that blames a value the user was
// legitimately given. Naming the divergence says the true thing instead: this version no longer matches
// the form, so publish it or select another.
//
// The comparison is of the whole descriptor rather than of the submission in hand. A divergence means
// the values are answers to a different question, and accepting them because they happen to fit the
// version too would be luck rather than agreement.
func workflowFieldDivergence(rendered, version FormDescriptor) []string {
	live, frozen := judgedFields(rendered), judgedFields(version)
	diverged := []string{}
	for key, want := range frozen {
		if got, ok := live[key]; !ok || !got.sameAs(want) {
			diverged = append(diverged, key)
		}
	}
	for key := range live {
		if _, ok := frozen[key]; !ok {
			diverged = append(diverged, key)
		}
	}
	slices.Sort(diverged)
	return diverged
}

// Reserved keys of the fields the platform injects into every workflow form (Issue collaboration D8). They
// are reserved in the sense that a workflow declaring a variable under one of these names keeps its own
// declaration — see injectPlatformFields. `context_refs` is reserved in a second sense: the Issues
// surface reads it back out of the values and turns it into the run's context references, so a
// workflow that declares that name takes over a key the platform also interprets.
const (
	fieldKeyPrompt      = "prompt"
	fieldKeyRepository  = "repository"
	fieldKeyBranch      = "branch"
	fieldKeySnapshotID  = "snapshotId"
	fieldKeyContextRefs = "context_refs"
)

// legacyFieldKeyVersion is the name the version selector used to be injected under. Its value was
// always a snapshot id, so the rename to fieldKeySnapshotID is a naming correction and not a change of
// meaning (Issue collaboration D8, specs#99) — but the rename cannot reach data that is already immutable: the `input`
// of an interaction confirmed before the deploy, and the graph of a snapshot published before it. Both
// are read through this key, and nothing is ever written under it.
const legacyFieldKeyVersion = "version"

// launchFieldKeys is every key the platform injects, and the only set a workflow's `launchFields`
// declaration may name: a declaration cannot conjure a field the catalog does not have, so an
// unrecognized key is dropped rather than remembered. The order the fields are *injected* in lives in
// the catalog below, where each field is actually built.
var launchFieldKeys = []string{fieldKeyRepository, fieldKeyBranch, fieldKeySnapshotID, fieldKeyPrompt, fieldKeyContextRefs}

// launchFieldOverride is one entry of a workflow's `launchFields` declaration (Issue collaboration D8): whether the
// form asks for that field, and whether it insists on an answer.
type launchFieldOverride struct {
	Enabled bool
	// Required is nil when the author said nothing, which keeps the catalog's own answer rather than
	// defaulting to false — "optional" and "unstated" are different, and only the author may pick the
	// former.
	Required *bool
}

// launchFieldsFromGraph reads the workflow's declaration of which launch fields its form asks for.
//
// The declaration is read *per key*: one it does not mention keeps the catalog's own answer. That is
// what makes a field added to the catalog later appear for every workflow without re-saving one, and
// what keeps a workflow written before this declaration existed rendering exactly the form it always
// did. An unreadable entry contributes nothing rather than failing the request, the way the globals
// projection reads the same document.
//
// A declaration naming the pre-rename `version` key counts as naming the snapshot selector, because
// that is the field it was written about: the graphs carrying it are published snapshots, which are
// immutable, so reading it strictly would quietly hand a field back to a workflow whose author had
// withdrawn it — the silent-ignore the publish gate exists to prevent, one layer up.
func launchFieldsFromGraph(graph Object) map[string]launchFieldOverride {
	declared, _ := graph["launchFields"].([]any)
	out := map[string]launchFieldOverride{}
	for _, item := range declared {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entry := Object(raw)
		key := launchFieldKey(entry.S("key"))
		if key == "" {
			continue
		}
		override := launchFieldOverride{Enabled: true}
		if enabled, ok := entry["enabled"].(bool); ok {
			override.Enabled = enabled
		}
		if required, ok := entry["required"].(bool); ok {
			override.Required = &required
		}
		out[key] = override
	}
	return out
}

// launchFieldKey resolves a declared key to the catalog key it names, or "" when it names none.
func launchFieldKey(key string) string {
	if key == legacyFieldKeyVersion {
		return fieldKeySnapshotID
	}
	for _, candidate := range launchFieldKeys {
		if candidate == key {
			return key
		}
	}
	return ""
}

// launchFieldEnabled reports whether the form asks for a field. Silence means yes: only an explicit
// `enabled: false` removes one.
func launchFieldEnabled(declaration map[string]launchFieldOverride, key string) bool {
	override, ok := declaration[key]
	return !ok || override.Enabled
}

// launchFieldRequired resolves a field's requiredness: the author's answer when they gave one, the
// catalog's otherwise.
func launchFieldRequired(declaration map[string]launchFieldOverride, key string, fallback bool) bool {
	if override, ok := declaration[key]; ok && override.Required != nil {
		return *override.Required
	}
	return fallback
}

// platformFormFields is the ordered catalog of fields every workflow form carries once it is opened
// for an issue: what this run is *about*. It answers, in order, which repository, which branch of it,
// which published version of the workflow, what prompt, and which extra context travels with it.
//
// Repository and branch are required — a run with no repository has nothing to work on — while the
// rest are optional, because leaving them empty is a meaningful choice: an empty prompt means the
// workflow's own Start prompt, and no extra references means exactly the issue's own.
//
// The version and context-reference fields are omitted outright when they have nothing to offer rather
// than injected empty. A select with no options fails descriptor validation closed, and the contract
// admits no other way to say "no choice available"; the same reasoning that keeps an over-long author
// form from being appended to.
//
// Labels are Chinese because this descriptor is rendered by the Issues surface, which is Chinese
// throughout; author-declared labels arrive in whatever language their author wrote them in.
//
// Each entry also carries whether the platform can *offer* that field at all: a field with nothing to
// offer is dropped even when the author asked for it, because the contract has no way to express a
// select with no choices (Issue collaboration D8).
//
// This is the catalog rather than the form. A workflow's declaration narrows it in
// injectPlatformFields, and only there, because a key the author declared in their own Start node is
// not the platform's to withdraw (Issue collaboration D8).
func platformFormFields(issue *IssueFormContext, startPrompt string, snapshots []Object) []FormField {
	version, hasVersion := versionField(snapshots)
	refs, hasRefs := contextRefsField(issue)
	catalog := []struct {
		field   FormField
		offered bool
	}{
		{
			FormField{
				Key:          fieldKeyRepository,
				Label:        "仓库地址",
				Type:         fieldText,
				Required:     true,
				DefaultValue: platformDefault(issue.RepositoryURL),
			},
			true,
		},
		{
			FormField{
				Key:          fieldKeyBranch,
				Label:        "分支",
				Type:         fieldText,
				Required:     true,
				DefaultValue: platformDefault(issue.DefaultBranch),
			},
			true,
		},
		{version, hasVersion},
		{
			FormField{
				Key:          fieldKeyPrompt,
				Label:        "提示词",
				Type:         fieldTextarea,
				Required:     false,
				DefaultValue: platformDefault(startPrompt),
			},
			true,
		},
		{refs, hasRefs},
	}
	fields := []FormField{}
	for i := range catalog {
		if !catalog[i].offered {
			continue
		}
		fields = append(fields, catalog[i].field)
	}
	return fields
}

// versionField offers the workflow's published versions, newest first and newest by default. The value
// is the snapshot id rather than the version number: the id is what a run is created from, and it
// keeps pointing at the same frozen document. The version number rides in the label, where it is read
// rather than consumed — which is why the field is keyed `snapshotId` and not `version`, the name that
// said the opposite of what the value is (Issue collaboration D8).
//
// A workflow that has never been published has nothing to offer, and an option-less select would fail
// descriptor validation, so the field is dropped rather than injected empty.
func versionField(snapshots []Object) (FormField, bool) {
	options := []FormOption{}
	for _, snapshot := range snapshots {
		id := snapshot.S("id")
		if !validID(id) {
			continue
		}
		label := "v" + strconv.FormatInt(snapshot.N("version"), 10) + " · " + snapshot.S("name")
		options = append(options, FormOption{Value: id, Label: clip(label, 200)})
	}
	if len(options) == 0 {
		return FormField{}, false
	}
	return FormField{
		Key:          fieldKeySnapshotID,
		Label:        "运行版本",
		Type:         fieldSelect,
		Required:     false,
		DefaultValue: options[0].Value,
		Options:      options,
	}, true
}

// contextRefsField offers the references this run may carry *in addition to* the issue's own. It is
// additive by construction rather than a picker over what to keep: the issue's persisted references
// are already on every run it starts, so a checkbox that appeared to remove one would be lying.
//
// Only the two references an issue can name for itself are offered — its project and its parent —
// because a picker over arbitrary issues, pull requests and files does not exist yet. An issue that
// can name neither gets no field at all.
func contextRefsField(issue *IssueFormContext) (FormField, bool) {
	options := []FormOption{}
	if validID(issue.ProjectID) {
		options = append(options, FormOption{Value: contextRefValue("project", issue.ProjectID), Label: "本项目"})
	}
	if validID(issue.ParentIssueID) {
		options = append(options, FormOption{Value: contextRefValue("parent_issue", issue.ParentIssueID), Label: "父任务"})
	}
	if len(options) == 0 {
		return FormField{}, false
	}
	return FormField{
		Key:         fieldKeyContextRefs,
		Label:       "补充上下文引用",
		Type:        fieldMultiSelect,
		Required:    false,
		Description: "本 issue 自己的引用已经包含在内；这里勾选的会额外带上。",
		Options:     options,
	}, true
}

// contextRefValue is the wire form of one context-reference option: the `refType:refId` pair the
// Issues surface splits back into a reference. Kept in one place so the format the descriptor emits
// and the format the surface parses cannot drift apart.
func contextRefValue(refType, refID string) string {
	return refType + ":" + refID
}

// platformDefault turns an optional context value into a field default. An empty value contributes no
// default rather than an empty one: the contract omits absent defaults, and a required field with no
// default is what asks the user to fill it in.
func platformDefault(value string) any {
	if value == "" {
		return nil
	}
	return clip(value, maxFieldValueLength)
}

// injectPlatformFields puts the platform fields in front of the author's own. They come first because
// they say what the run is *about* — which repository, which branch, which version, which prompt,
// which extra context — while the author's variables are its parameters.
//
// A key the author already declared wins outright: their control, label and requiredness are kept, and
// the platform only supplies the default the author left empty, and only when that default is a legal
// value for the author's control. Collisions are a real case rather than a hypothetical one — a
// workflow whose Start node declares a `repository` variable is how one would have expressed this
// before the platform field existed — and so is an *illegal* default: the same URL is not a legal
// value for a `repository` the author declared as a select that does not offer it.
//
// The snapshot selector is the one key the platform contributes nothing to. Its value is not a value
// the workflow receives but the cursor `confirmInteraction` resolves the run's snapshot from, so it is
// never a default the author's own variable of that name could mean: filling it would hand the run a
// snapshot id as the variable's value. The author's field is left exactly as they declared it, and the
// cursor stays the platform's — see workflowVersionCursor.
//
// The workflow's declaration (Issue collaboration D8) is applied here, and only to the fields the platform is
// *adding*. A field the author asked for in their own Start node is asked for because they asked for
// it, so no declaration withdraws it: for that key the declaration is inert, control and requiredness
// alike, and the platform contributes nothing but the default.
func injectPlatformFields(fields []FormField, issue *IssueFormContext, startPrompt string, snapshots []Object, declaration map[string]launchFieldOverride) []FormField {
	declared := map[string]int{}
	for i := range fields {
		declared[fields[i].Key] = i
	}
	offered := platformFormFields(issue, startPrompt, snapshots)
	platform := []FormField{}
	for i := range offered {
		field := &offered[i]
		if i, ok := declared[field.Key]; ok {
			if field.Key != fieldKeySnapshotID {
				fillDefault(&fields[i], field)
			}
			continue
		}
		if !launchFieldEnabled(declaration, field.Key) {
			continue
		}
		field.Required = launchFieldRequired(declaration, field.Key, field.Required)
		// The author's form already fills the descriptor. Appending past the limit would make the
		// descriptor invalid and turn every confirm into a 500, so the author's form is served instead.
		if len(fields)+len(platform) >= maxFormFields {
			break
		}
		platform = append(platform, *field)
	}
	if len(platform) == 0 {
		return fields
	}
	return append(platform, fields...)
}

// appendGlobalVariableFields projects the graph's global variables onto the form, after the author's
// own Start variables: those are the parameters of one run, while a global is a workflow-wide constant
// this invocation may override. Each keeps its declared value as its default, so leaving a field
// untouched runs the workflow with exactly the constant it declares.
//
// A global name always contains a dot — the editor's own guard, mirrored here — which is what keeps
// these keys from colliding with a Start variable or a platform field key. Names that are not valid
// field keys are dropped rather than sanitized: the confirmed values reach the workflow keyed by name,
// so a rewritten key would feed the run the wrong variable.
func appendGlobalVariableFields(fields []FormField, graph Object) []FormField {
	declared, _ := graph["globalVariables"].([]any)
	seen := map[string]bool{}
	for i := range fields {
		seen[fields[i].Key] = true
	}
	for _, item := range declared {
		if len(fields) == maxFormFields {
			break
		}
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		variable := Object(raw)
		name := variable.S("name")
		if !strings.Contains(name, ".") || seen[name] {
			continue
		}
		if field, ok := globalVariableField(variable); ok {
			seen[name] = true
			fields = append(fields, field)
		}
	}
	return fields
}

// globalVariableField maps one global variable onto a control. The mapping mirrors the Start variables'
// own, and a type Issues has no control for degrades to a textarea holding JSON rather than dropping
// the variable: a missing field would silently run the workflow with its declared constant instead of
// the value the user meant to set, which is the one outcome worse than an awkward control.
func globalVariableField(variable Object) (FormField, bool) {
	key := variable.S("name")
	if !validOpaqueToken(key) {
		return FormField{}, false
	}
	field := FormField{Key: key, Label: key, Required: false}
	switch variable.S("valueType") {
	case "number", "integer":
		field.Type = fieldNumber
	case "boolean":
		field.Type = fieldBoolean
	case "string", "secret":
		field.Type = fieldText
	default:
		field.Type = fieldTextarea
	}
	if value, ok := variable["value"]; ok && value != nil && validFieldValue(&field, value) {
		field.DefaultValue = value
	}
	return field, true
}

// fillDefault hands the platform default to an author-declared field, but only when the author left the
// value empty and the default is legal for *their* control.
func fillDefault(field, platform *FormField) {
	if field.DefaultValue != nil || platform.DefaultValue == nil {
		return
	}
	if validFieldValue(field, platform.DefaultValue) {
		field.DefaultValue = platform.DefaultValue
	}
}

// workflowVersionCursor reads the published version a confirmed form asked for, or "" for the newest
// one. It is the only reader of the version keys, in one place, because which of the two things such a
// key can be — the platform's cursor or an author's variable — is a decision that must be made the same
// way wherever it is read.
//
// A key is the platform's own only when the graph's own input fields do not declare it (Issue collaboration D8): an
// author who declares a variable under that name keeps it, no version selector is injected into their
// form, and the value the user typed is theirs to give the workflow. Reading it as a snapshot id would
// either 404 on a value that is not an id or, when the value is one, run the version the user asked for
// while feeding the workflow that same id as its parameter.
//
// There are two keys to try because the field was renamed (Issue collaboration D8) and a rename cannot reach a form
// that was rendered before it: `snapshotId` is the name this build's form asks under, and `version` is
// what the same field was called until then. The new key is tried first, so a submission carrying both
// is answered by the one this build wrote; the old key is the answer for the submissions that carry
// only it, which is the whole reason to read it — the alternative is starting the newest version while
// the version the user picked is dropped without a word.
//
// The graph is the *live* document rather than the version being resolved — the one the form was
// rendered from — because that is what fixes the meaning of the value in hand, and it is decidable
// before the snapshot is resolved. Reading it from the resolved version instead would make the meaning
// of a submitted value depend on which version the same submission selected.
func workflowVersionCursor(graph, values Object) string {
	if !declaresWorkflowInputField(graph, fieldKeySnapshotID) {
		if id := values.S(fieldKeySnapshotID); id != "" {
			return id
		}
	}
	if !declaresWorkflowInputField(graph, legacyFieldKeyVersion) {
		return values.S(legacyFieldKeyVersion)
	}
	return ""
}

// declaresWorkflowInputField reports whether the graph's own Start node declares a variable under key.
func declaresWorkflowInputField(graph Object, key string) bool {
	fields := workflowInputFields(graph)
	for i := range fields {
		if fields[i].Key == key {
			return true
		}
	}
	return false
}

// foldLegacyVersionAnswer takes the pre-rename `version` key back out of a submission, rewriting it
// under `snapshotId` where that key is free.
//
// It runs immediately after workflowVersionCursor, and the order is the point: the old key has to
// survive until the cursor has read it — in a graph whose author declared `snapshotId` for a variable
// of their own, the old key is the *only* place the selection can be read from — and it has to be gone
// afterwards, because the `@` form is the one surface where the set of keys *is* the contract and a key
// the descriptor does not declare fails the confirm outright (see validateFormValues).
//
// The rewrite is what keeps the interaction stored under the name this build uses, as every new one is.
// It is skipped where the author has claimed `snapshotId` for their own variable: there is nowhere to
// write the answer, and writing it anyway would hand the workflow a snapshot id as that variable's
// value. The answer is dropped there, which costs nothing that is not recorded elsewhere — the version
// the run executed is frozen onto the run's own input by the gate that resolved it.
func foldLegacyVersionAnswer(graph, values Object) {
	answer, ok := values[legacyFieldKeyVersion]
	if !ok || declaresWorkflowInputField(graph, legacyFieldKeyVersion) {
		return
	}
	delete(values, legacyFieldKeyVersion)
	if declaresWorkflowInputField(graph, fieldKeySnapshotID) {
		return
	}
	// The same rule the cursor reads by: an answer the client already gave under the new key is the one
	// this build's form wrote, and the old key does not displace it.
	if values.S(fieldKeySnapshotID) == "" {
		values[fieldKeySnapshotID] = answer
	}
}

// workflowStartNode returns the `data` of the graph's first Start node. The graph is a document the API
// never interprets, so its shape is read defensively: anything unexpected contributes nothing rather
// than failing the request. The first Start node wins, matching the editor, which allows at most one.
//
// Reading the prompt and the variables through this one lookup is what keeps them from ever coming
// from two different nodes in a graph that somehow carries more than one.
func workflowStartNode(graph Object) Object {
	nodes, _ := graph["nodes"].([]any)
	for _, raw := range nodes {
		node, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		data, ok := node["data"].(map[string]any)
		if !ok || data["kind"] != "start" {
			continue
		}
		return Object(data)
	}
	return nil
}

// workflowStartVariables reads the input variables the Start node declares.
func workflowStartVariables(graph Object) []Object {
	data := workflowStartNode(graph)
	if data == nil {
		return nil
	}
	declared, _ := data["inputVariables"].([]any)
	out := []Object{}
	for _, item := range declared {
		if variable, ok := item.(map[string]any); ok {
			out = append(out, Object(variable))
		}
	}
	return out
}

// workflowStartPrompt reads the kickoff prompt the Start node carries — the editor's 初始提示词, stored
// as `data.input`. It is the default the platform's prompt field offers, so an empty or whitespace-only
// prompt contributes no default: a field prefilled with blank space would look answered when it is not.
func workflowStartPrompt(graph Object) string {
	data := workflowStartNode(graph)
	if data == nil {
		return ""
	}
	prompt, _ := data["input"].(string)
	return strings.TrimSpace(prompt)
}

// startInputField maps one Start variable onto the control Issues renders. The second result is false
// when the variable cannot be expressed as a valid field at all, which drops it from the form: a
// malformed field would fail descriptor validation closed and take the whole form with it.
//
// The key is the variable's own name, never a derived one: the confirmed values are handed to the
// workflow keyed by name, so a renamed key would silently feed the run the wrong input.
func startInputField(variable Object) (FormField, bool) {
	key := variable.S("name")
	if !validOpaqueToken(key) {
		return FormField{}, false
	}
	label := variable.S("displayName")
	if label == "" {
		label = key
	}
	if len(label) > 200 {
		return FormField{}, false
	}
	field := FormField{Key: key, Label: label, Required: variable.B("required")}
	switch inputFieldType(variable) {
	case "paragraph":
		field.Type = fieldTextarea
	case "number":
		field.Type = fieldNumber
	case "checkbox":
		field.Type = fieldBoolean
	case "select":
		options := inputOptions(variable)
		if len(options) == 0 {
			// A select with nothing to choose from would fail validation closed; a text field keeps the
			// form usable instead of making the workflow unconfirmable.
			field.Type = fieldText
			break
		}
		field.Type = fieldSelect
		field.Options = options
	case "file-list", "json":
		// Issues has no upload or structured-value control. A textarea collects the reference or the
		// document as text, which is what the workflow receives either way.
		field.Type = fieldTextarea
	default:
		// text-input, file, and every control Cloud has no counterpart for degrade to a single line.
		field.Type = fieldText
	}
	if value, ok := variable["value"]; ok && value != nil && validFieldValue(&field, value) {
		field.DefaultValue = value
	}
	return field, true
}

// inputFieldType resolves the control a variable declares, falling back to the one its variable-pool
// type implies. Declarations that predate explicit form-control metadata carry no fieldType, and the
// editor resolves them the same way, so an older graph renders the form it always did.
func inputFieldType(variable Object) string {
	if declared := variable.S("fieldType"); declared != "" {
		return declared
	}
	switch variable.S("valueType") {
	case "number", "integer":
		return "number"
	case "boolean":
		return "checkbox"
	case "file":
		return "file"
	case "array[file]":
		return "file-list"
	case "string", "secret", "":
		return "text-input"
	default:
		return "json"
	}
}

// inputOptions reads a select's choices. The label is the value: a Start variable declares choices as
// plain strings, and the editor offers no separate label to carry.
func inputOptions(variable Object) []FormOption {
	declared, _ := variable["options"].([]any)
	out := []FormOption{}
	seen := map[string]bool{}
	for _, item := range declared {
		value, ok := item.(string)
		if !ok || value == "" || len(value) > 200 || seen[value] || len(out) == maxFormOptions {
			continue
		}
		seen[value] = true
		out = append(out, FormOption{Value: value, Label: value})
	}
	return out
}

// clip shortens s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
