package core

import (
	"encoding/json"
	"strings"
)

// FormDescriptor is the Issues-facing *rendering* descriptor for a Workflow (Form Mode) interaction.
// It answers exactly one question: which input controls should the Issues frontend render, and what is
// the most basic client-side interaction/validation for them (Issue collaboration D7).
//
// It is deliberately NOT a Workflow canonical schema and is bound to no schema technology: a future
// Workflow module may express its schema as JSON Schema, a DSL, protobuf, a database schema, a
// code-defined schema or a remote service contract, and its adapter projects that into this shape
// (Issue collaboration D7). It is also not a capability DSL — conditional visibility, computed fields, cross-field
// expressions, repeaters and nested groups are explicitly out of scope.
type FormDescriptor struct {
	FormRef     string
	Title       string
	Description string
	Fields      []FormField
}

// FormField is one rendered control. `Key` is the machine key used in the form-value map (Issue collaboration D7).
type FormField struct {
	Key          string
	Label        string
	Type         string
	Required     bool
	Description  string
	Placeholder  string
	DefaultValue any
	Options      []FormOption
}

// FormOption is one allowed value for a select / multi_select field.
type FormOption struct {
	Value string
	Label string
}

// First-version field types (Issue collaboration D7). Anything else makes the descriptor invalid.
const (
	fieldText        = "text"
	fieldTextarea    = "textarea"
	fieldNumber      = "number"
	fieldBoolean     = "boolean"
	fieldSelect      = "select"
	fieldMultiSelect = "multi_select"
)

// maxFieldValueLength bounds a single text/textarea form value.
const maxFieldValueLength = 4000

// maxFormFields / maxFormOptions bound one descriptor so no form is unbounded. A provider projecting
// its own schema onto this shape reads the same limits, which is why they are named here rather than
// written into the validator.
const (
	maxFormFields  = 100
	maxFormOptions = 100
)

func supportedFieldType(t string) bool {
	switch t {
	case fieldText, fieldTextarea, fieldNumber, fieldBoolean, fieldSelect, fieldMultiSelect:
		return true
	}
	return false
}

func fieldTakesOptions(t string) bool { return t == fieldSelect || t == fieldMultiSelect }

// validOpaqueToken accepts the grammar used for `formRef` and field keys: a URL-path- and JSON-key-safe
// token. `formRef` is opaque to Issues but appears in a path segment, so it must be constrained.
func validOpaqueToken(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

// validateFormDescriptor enforces the Issue collaboration D7 invariants: unique field keys, options only (and always)
// for select/multi_select, supported types, and a type-consistent default value. A provider that
// returns a malformed descriptor is a provider bug, so this fails closed as 500 invalid_form_descriptor
// — Issues never renders a partially valid form.
func validateFormDescriptor(d FormDescriptor) {
	require(validOpaqueToken(d.FormRef), 500, "invalid_form_descriptor")
	require(len(d.Title) <= 200 && len(d.Description) <= 2000, 500, "invalid_form_descriptor")
	require(len(d.Fields) <= maxFormFields, 500, "invalid_form_descriptor")
	seen := map[string]bool{}
	for i := range d.Fields {
		f := &d.Fields[i]
		require(validOpaqueToken(f.Key) && !seen[f.Key], 500, "invalid_form_descriptor")
		seen[f.Key] = true
		require(supportedFieldType(f.Type), 500, "invalid_form_descriptor")
		require(f.Label != "" && len(f.Label) <= 200, 500, "invalid_form_descriptor")
		require(len(f.Description) <= 1000 && len(f.Placeholder) <= 200, 500, "invalid_form_descriptor")
		if fieldTakesOptions(f.Type) {
			require(len(f.Options) > 0 && len(f.Options) <= maxFormOptions, 500, "invalid_form_descriptor")
			values := map[string]bool{}
			for _, o := range f.Options {
				require(o.Value != "" && len(o.Value) <= 200 && len(o.Label) <= 200, 500, "invalid_form_descriptor")
				require(!values[o.Value], 500, "invalid_form_descriptor")
				values[o.Value] = true
			}
		} else {
			require(len(f.Options) == 0, 500, "invalid_form_descriptor")
		}
		if f.DefaultValue != nil {
			require(validFieldValue(f, f.DefaultValue), 500, "invalid_form_descriptor")
		}
	}
}

// validFieldValue reports whether v is an acceptable value for f: the declared scalar type, a string
// within `options` for select, or a subset of `options` for multi_select. It is shared by descriptor
// validation (defaults), assist filtering (provider output) and confirm validation (client input), so
// all three agree on what a legal value is.
func validFieldValue(f *FormField, v any) bool {
	switch f.Type {
	case fieldText, fieldTextarea:
		s, ok := v.(string)
		return ok && len(s) <= maxFieldValueLength
	case fieldNumber:
		switch n := v.(type) {
		case json.Number:
			_, e := n.Float64()
			return e == nil
		case float64, int64, int:
			return true
		}
		return false
	case fieldBoolean:
		_, ok := v.(bool)
		return ok
	case fieldSelect:
		s, ok := v.(string)
		return ok && optionValue(f, s)
	case fieldMultiSelect:
		list, ok := v.([]any)
		if !ok || len(list) > len(f.Options) {
			return false
		}
		seen := map[string]bool{}
		for _, item := range list {
			s, ok := item.(string)
			if !ok || !optionValue(f, s) || seen[s] {
				return false
			}
			seen[s] = true
		}
		return true
	}
	return false
}

func optionValue(f *FormField, value string) bool {
	for _, o := range f.Options {
		if o.Value == value {
			return true
		}
	}
	return false
}

// validateFormValues re-validates submitted form values against the descriptor the form was rendered
// from (Issue collaboration D9). The frontend descriptor is a rendering hint, never an authority: unknown keys, wrong
// types, values outside `options` and (when requireComplete) missing required fields are all rejected
// here.
//
// Which descriptor that is depends on the caller. Assist and the form read validate against the live
// document, because that is what the user is looking at; a confirm validates against the frozen
// version being confirmed, because that is what will execute. The fault names the field either way:
// a form with several fields has to say which one it is refusing, or the user has to guess.
func validateFormValues(d FormDescriptor, values Object, requireComplete bool) {
	known := map[string]FormField{}
	for i := range d.Fields {
		f := d.Fields[i]
		known[f.Key] = f
	}
	for k := range values {
		if _, ok := known[k]; !ok {
			rejectWith(400, "invalid_field_value", Object{"field": k})
		}
	}
	for i := range d.Fields {
		f := &d.Fields[i]
		v, present := values[f.Key]
		if !present || v == nil {
			if requireComplete && f.Required {
				rejectWith(400, "required_field_missing", Object{"field": f.Key})
			}
			continue
		}
		if !validFieldValue(f, v) {
			rejectWith(400, "invalid_field_value", Object{"field": f.Key})
		}
	}
}

// formDescriptorObject renders the descriptor as the wire object. Only the always-present keys are
// emitted unconditionally; the optional ones are omitted when absent, which is what the closed OpenAPI
// schema (required = key/label/type/required) accepts.
func formDescriptorObject(d FormDescriptor) Object {
	fields := []Object{}
	for i := range d.Fields {
		f := d.Fields[i]
		o := Object{"key": f.Key, "label": f.Label, "type": f.Type, "required": f.Required}
		if f.Description != "" {
			o["description"] = f.Description
		}
		if f.Placeholder != "" {
			o["placeholder"] = f.Placeholder
		}
		if f.DefaultValue != nil {
			o["defaultValue"] = f.DefaultValue
		}
		if len(f.Options) > 0 {
			options := []Object{}
			for _, opt := range f.Options {
				options = append(options, Object{"value": opt.Value, "label": opt.Label})
			}
			o["options"] = options
		}
		fields = append(fields, o)
	}
	out := Object{"formRef": d.FormRef, "fields": fields}
	if d.Title != "" {
		out["title"] = d.Title
	}
	if d.Description != "" {
		out["description"] = d.Description
	}
	return out
}

// assistSuggestionObject renders a provider suggestion for the wire, filtering it defensively: a
// suggested key must exist on the descriptor and carry a legal value, and a suggested context ref must
// be a known ref type with a UUID ref id. A provider can therefore never widen the form or inject a
// malformed reference — unmentioned keys simply keep the user's current value (Issue collaboration D9).
func assistSuggestionObject(d FormDescriptor, s AssistSuggestion) Object {
	values := Object{}
	explanations := Object{}
	for i := range d.Fields {
		f := &d.Fields[i]
		v, ok := s.Values[f.Key]
		if !ok || !validFieldValue(f, v) {
			continue
		}
		values[f.Key] = v
		if text, ok := s.Explanations[f.Key].(string); ok && text != "" && len(text) <= 500 {
			explanations[f.Key] = text
		}
	}
	refs := []Object{}
	for _, rf := range s.ContextRefs {
		if contextRefTypes[rf.S("refType")] && validID(rf.S("refId")) {
			refs = append(refs, Object{"refType": rf.S("refType"), "refId": rf.S("refId")})
		}
	}
	out := Object{"suggestedValues": values, "suggestedContextRefs": refs}
	if len(explanations) > 0 {
		out["explanations"] = explanations
	}
	return out
}

// interactionValues reads the confirmed/edited form values from a request body. Values must be a JSON
// object keyed by field key; anything else is a client error.
func interactionValues(body Object) Object {
	raw, ok := body["values"]
	if !ok || raw == nil {
		return Object{}
	}
	values, ok := raw.(map[string]any)
	require(ok, 400, "invalid_interaction_input")
	return Object(values)
}

// appliedContextRefs reads the optional `contextRefs` list from a confirm body. These are *applied
// suggestions*, not persistent issue context: they join this run's snapshot only and never create an
// `issue_context_ref` row (Issue collaboration D5).
func appliedContextRefs(body Object) []Object {
	raw, ok := body["contextRefs"]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	require(ok, 400, "invalid_interaction_input")
	require(len(list) <= 50, 400, "invalid_interaction_input")
	out := []Object{}
	for _, item := range list {
		m, ok := item.(map[string]any)
		require(ok, 400, "invalid_interaction_input")
		refType, _ := m["refType"].(string)
		refID, _ := m["refId"].(string)
		require(contextRefTypes[refType] && validID(refID), 400, "invalid_interaction_input")
		out = append(out, Object{"refType": refType, "refId": refID})
	}
	return out
}

// workflowFormDescriptor resolves the Form Mode descriptor for a workflow target, server-side and from
// scratch: the target is re-resolved through the directory (which owns the target -> formRef mapping),
// then the provider resolves the descriptor, then the descriptor is validated. Every failure is a typed
// fault, never a partially usable form (Issue collaboration D7).
//
// `issue` is the issue context the descriptor is tailored with. GET and assist must pass the same one:
// assist re-validates submitted values against this descriptor, so a descriptor that disagreed with
// what the client rendered would reject values the user was legitimately shown.
//
// This resolves the workflow's *live* document, which is right for both of them — they render a form
// the user is still filling in, and the version picker has not been used yet. A confirm does not come
// through here: it executes the version that was chosen, so it projects that snapshot
// (snapshotFormDescriptor) and an edit made since the last publish cannot change what an already-offered
// version accepts. It projects the live document as well, but only to refuse a version that no longer
// describes the form the user was handed (workflowFieldDivergence) — and it projects it itself rather
// than through this provider, so that a descriptor outage cannot block a confirm.
func workflowFormDescriptor(t *transaction, tid, targetType, targetID string, issue *IssueFormContext) FormDescriptor {
	require(targetType == "workflow", 409, "interaction_not_confirmable")
	require(t.directory != nil, 503, "form_descriptor_unavailable")
	summary, ok, err := t.directory.ResolveTarget(t.ctx, tid, targetType, targetID)
	if err != nil {
		panic(databaseFailure{err})
	}
	require(ok, 404, "target_not_found")
	require(summary.InteractionDescriptor.Mode == "form", 409, "interaction_not_confirmable")
	formRef := summary.InteractionDescriptor.FormRef
	require(formRef != "" && t.forms != nil, 503, "form_descriptor_unavailable")
	// The port itself stays value-typed — see the nolint on ResolveFormDescriptor — so the context is
	// dereferenced at the one place the two conventions meet.
	descriptor, ok, err := t.forms.ResolveFormDescriptor(t.ctx, tid, formRef, *issue)
	if err != nil {
		panic(databaseFailure{err})
	}
	require(ok, 404, "form_descriptor_not_found")
	validateFormDescriptor(descriptor)
	return descriptor
}

// formDescriptorByRef is the public read path for `GET /collaboration/forms/{formRef}`: a read-only,
// tenant-scoped, provider-backed projection. It exposes no Workflow domain internals.
//
// The optional `issueId` query parameter supplies the issue context, which is what makes the form carry
// the platform fields. Omitting it is not an error and not a degraded mode — it is the projection this
// route served before the fields existed, which is what keeps a client that knows nothing about them
// working.
func formDescriptorByRef(t *transaction, r *PublicRequest) Object {
	ref := strings.TrimSpace(r.FormRef)
	require(validOpaqueToken(ref), 404, "form_descriptor_not_found")
	require(t.forms != nil, 503, "form_descriptor_unavailable")
	issue := issueFormContext(t, r.TenantID, strings.TrimSpace(r.FormIssueID))
	descriptor, ok, err := t.forms.ResolveFormDescriptor(t.ctx, r.TenantID, ref, issue)
	if err != nil {
		panic(databaseFailure{err})
	}
	require(ok, 404, "form_descriptor_not_found")
	validateFormDescriptor(descriptor)
	return formDescriptorObject(descriptor)
}

// workflowSnapshotForm is the public read path for
// `GET /workflows/{wfid}/snapshots/{sid}/form`: the `@` form projected from one frozen version rather
// than from the live document.
//
// It exists because a caller can choose the version *before* rendering — the workflow editor's manual
// run entry does — and such a caller has to be shown the fields of the version it picked. The `@` form
// is rendered from the live document and relies on the confirm's divergence refusal instead, which
// works there because the user never chose a version; a manual run has chosen one, so projecting the
// live document would show a form the chosen version does not accept and then refuse every answer.
// Confirm already projects this way when it validates a submission, so both read the one projection:
// this route adds a caller, not a second rule.
//
// It is a projection and not an admission decision. Reading a descriptor says nothing about whether a
// run may start — publish state, archiving, input dependencies and per-slot resolution stay the start
// gate's answers — so a caller that can read this form can still be refused by `POST …/runs`.
//
// Scoping is the snapshot read's own (§workflowSnapshot): an unknown or archived workflow, a snapshot
// of another workflow or another tenant, and a malformed id are all the same 404 as the direct-run
// entry answers with, so a client shows one message for one situation.
//
// The `issueId` query parameter is optional exactly as it is on the live route, and the response names
// the snapshot it was projected from: the same workflow has a different form per version, so the
// projection has to say which version it describes.
func workflowSnapshotForm(t *transaction, r *PublicRequest) Object {
	w := workflow(t, r.TenantID, r.WorkflowID)
	s := workflowSnapshot(t, r.TenantID, r.WorkflowID, r.SnapshotID)
	issue := issueFormContext(t, r.TenantID, strings.TrimSpace(r.FormIssueID))
	out := formDescriptorObject(snapshotFormDescriptor(t, w, s, &issue))
	out["snapshotId"] = s.S("id")
	return out
}
