package core

// Workflow run admission: the one place a workflow execution intent is judged before anything is
// created.
//
// Three entries create workflow work — a direct WorkflowRun, a manual workflow IssueRun and an
// interaction confirmed with @ — and they have to agree about what may be started, because a rule one
// entry applies and another skips is not a rule. This file is that agreement: which frozen document a
// run executes, whether the submitted input matches it, and whether the initiator can supply an
// executor for every slot the document references.
//
// Everything here runs inside the caller's transaction and refuses by panicking a Fault, so a rejected
// start leaves no run, no claimed interaction and no activity behind, and never produces a run row that
// could be dispatched. What this file deliberately does not do is freeze the per-slot model bindings
// the run will use: that is the model-access decision's own step, and Cloud's first phase has no
// workflow engine to hand them to.

// Reasons a slot cannot be resolved for the initiating user, reported one per slot in the fault's
// parameters. They are stable codes rather than prose for the same reason every fault is: the client
// localizes them, and the set is small enough to enumerate. They are deliberately distinct — "you
// never configured this" and "the connection you configured was disabled" are different things to do
// about, and a single "unavailable" would leave the user guessing which.
const (
	slotReasonNotConfigured     = "slot_not_configured"
	slotReasonDefaultMissing    = "default_model_missing"
	slotReasonConnectionDown    = "connection_unavailable"
	slotReasonCredentialMissing = "credential_missing"
	slotReasonModelUnavailable  = "model_unavailable"
)

// workflowRunSnapshot resolves the document a run will execute: the caller's explicit choice when it
// made one, otherwise the workflow's newest published version.
//
// Scoping is the tenant's and the workflow's — a cursor to another tenant's or another workflow's
// snapshot is the same 404 as an unknown id, and an archived workflow is 404 before its history is
// consulted at all. A workflow with nothing published is a 400 rather than a 404: the workflow exists
// and belongs to the caller, and the actionable fact is that there is no frozen version to run.
// Executing the live draft instead is exactly what the draft/published split exists to prevent — an
// edit would silently change what an already-created run "executed".
func workflowRunSnapshot(t *transaction, tid, wid, sid string) Object {
	if sid != "" {
		return workflowSnapshot(t, tid, wid, sid)
	}
	s := t.one("SELECT * FROM workflow_snapshots WHERE tenant_id=$1 AND workflow_id=$2 ORDER BY version DESC LIMIT 1", tid, wid)
	require(s != nil, 400, "workflow_no_published_snapshot")
	return s
}

// requireWorkflowRunInput refuses a run whose input does not answer the variables the frozen graph
// declares: a required Start variable with no value, or a value of the wrong type for its control.
//
// Only the declared variables are checked, and keys the graph does not declare are left alone. A run's
// input is an execution input object, not a form submission: it carries the platform's own material
// (context references, the issue context, a frozen executor reference) alongside the workflow's
// parameters, so treating an extra key as an error would refuse input the platform itself writes. The
// @ form is the one place where the set of keys *is* the contract, and it rejects unknown keys on its
// own — see validateFormValues.
//
// The check reads the snapshot's graph, never the live document, so a version published months ago
// still accepts exactly what it accepted then.
func requireWorkflowRunInput(graph, input Object) {
	fields := workflowInputFields(graph)
	for i := range fields {
		field := &fields[i]
		v, present := input[field.Key]
		if !present || v == nil {
			if field.Required {
				rejectWith(400, "required_field_missing", Object{"field": field.Key})
			}
			continue
		}
		if !validFieldValue(field, v) {
			rejectWith(400, "invalid_field_value", Object{"field": field.Key})
		}
	}
}

// requireWorkflowExecutorSlots refuses a start when the initiating user cannot supply an executor for
// some slot the frozen graph references.
//
// The mapping is personal (model-access decision D1): `default` is the user's personal default model
// and is not stored twice, and a named slot is looked up in the user's own slot mappings
// (resolveWorkflowSlotExecutor). There is no fallback of any kind — not to `default`, not to the
// author's `hint`, not to another user's configuration — because a node the author declared as needing
// a strong model running quietly on a weak one is the failure this gate exists to prevent.
//
// Every gap is collected before refusing, because the point of the refusal is that the user can fix
// them: the form shows each slot's description and the nodes that depend on it, and fixing them one
// 409 at a time would be a poor way to find that out.
func requireWorkflowExecutorSlots(t *transaction, graph Object, uid string) {
	declared := map[string]Object{}
	for _, slot := range workflowSlotDeclarations(graph) {
		declared[slot.S("name")] = slot
	}
	gaps := []any{}
	for _, reference := range workflowReferencedSlots(workflowNodes(graph)) {
		// One resolution for both halves of the gate: the same call the freezing step makes, so a
		// slot cannot be admitted by one policy and frozen by another.
		reason, _, _ := resolveWorkflowSlotExecutor(t, uid, reference.Name)
		if reason == "" {
			continue
		}
		gap := Object{"slot": reference.Name, "nodeIds": stringList(reference.Nodes), "reason": reason}
		// The description is the only thing the user has to decide what to map the slot to, and the
		// hint is what the author used — both travel with the refusal so the form can show them
		// inline. The hint never resolves anything; it is offered, not applied.
		if slot, ok := declared[reference.Name]; ok {
			gap["description"] = slot.S("description")
			if hint, ok := slot["hint"]; ok && hint != nil {
				gap["hint"] = hint
			}
		}
		gaps = append(gaps, gap)
	}
	if len(gaps) > 0 {
		rejectWith(409, "workflow_executor_unconfigured", Object{"slots": gaps})
	}
}

// admitWorkflowRun is the shared start gate: it resolves the frozen document the run will execute and
// refuses the start when the initiator cannot supply an executor for every slot it references. It
// returns the snapshot, which is what the caller freezes onto the run and, for a direct WorkflowRun,
// stores as the run's own `snapshot_id`.
//
// Input is validated by each entry against its own shape — a run's input object, or the values of a
// confirmed form — so the gate itself stays about the document rather than about one caller's wire
// format. Both checks are still the same two questions asked of the same frozen graph.
func admitWorkflowRun(t *transaction, tid, wid, sid, uid string) Object {
	s := workflowRunSnapshot(t, tid, wid, sid)
	requireWorkflowExecutorSlots(t, s.O("graph"), uid)
	return s
}

// freezeWorkflowSnapshot records the version a run executes on the run's own input, alongside the
// model binding the agent path freezes the same way. It is server-owned: the value is always the
// snapshot the gate resolved, so a caller cannot claim a run executed a version it did not, and the
// key is what keeps a run legible after the workflow is edited or published again.
func freezeWorkflowSnapshot(input Object, snapshotID string) {
	input["workflowSnapshotId"] = snapshotID
}

// stringList renders a []string as the wire list the fault parameters carry, keeping an empty list an
// empty list rather than null so a client can iterate it unconditionally.
func stringList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}
