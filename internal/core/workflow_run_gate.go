package core

// The WorkflowRun creation gate's freezing half (specs decision 20261015-executor-slot-mappings
// D2/D3). Admission (workflow_admission.go) is the half that refuses: it proves the invoker can
// supply an executor for every slot the frozen graph references, and collects every gap before
// saying so. This file is the half that records: each slot the gate admitted is frozen onto the run
// as one run_model_bindings row, inside the run's own creation transaction.
//
// Freezing is what makes a run reproducible. A mapping edited, a connection disabled or a model
// withdrawn after creation cannot change what an already-created run executes, because the session
// starter reads the frozen row rather than resolving the slot again.
//
// The plugin version is frozen too, but not here: it is part of the invocation identity rather than of
// a slot's resolution, so the creation transaction reads it beside the Project it needs — see
// resolveWorkflowRunPluginVersion. What matters is the same in both places: the session starter reads
// the frozen value and never resolves it again.
//
// Resolving a slot is one function (resolveWorkflowSlotExecutor), called by both halves in the same
// transaction, so a slot cannot be admitted by one policy and frozen by another.

// freezeWorkflowRunExecutors freezes one binding per slot the frozen graph references, including the
// implicit `default`. It runs after admission in the same transaction, so every slot it resolves has
// already been proved resolvable and no fallback to another slot, to the author's hint or to the
// default model ever applies.
func freezeWorkflowRunExecutors(t *transaction, runID, uid string, graph Object) {
	for _, reference := range workflowReferencedSlots(workflowNodes(graph)) {
		_, connection, model := resolveWorkflowSlotExecutor(t, uid, reference.Name)
		b := bindingRow(connection, model)
		t.exec(`INSERT INTO run_model_bindings(id,workflow_run_id,slot_name,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, b.S("id"), runID, reference.Name, uid, b.S("connectionId"), b.N("connectionVersion"), b.S("credentialId"), b.S("connectionName"), b.S("protocol"), b.S("baseUrl"), b.S("authMode"), jsonText(b.O("model")))
	}
}

// resolveWorkflowSlotExecutor resolves one executor slot against the invoker's personal
// configuration (model-access decision D1): `default` is the personal default model, a named slot is
// the personal mapping. It returns the reason naming the gap, or the connection and the model
// definition the caller freezes.
//
// A named slot with no mapping is `slot_not_configured` — the one gap the user fixes by writing a
// mapping — while a mapped executor that has since broken reports which part broke, because "you
// never configured this" and "the connection you configured was disabled" are different things to do
// about. A named slot is never resolved through the default model: a node the author declared as
// needing a strong model running quietly on a weak one is the failure the gate exists to prevent.
func resolveWorkflowSlotExecutor(t *transaction, uid, name string) (reason string, connection, model Object) {
	if name == workflowDefaultSlot {
		return defaultExecutorGap(t, uid)
	}
	m := t.one("SELECT connection_id, model_id FROM executor_slot_mappings WHERE user_id=$1 AND slot_name=$2", uid, name)
	if m == nil {
		return slotReasonNotConfigured, nil, nil
	}
	return personalExecutorGap(t, uid, m.S("connectionId"), m.S("modelId"))
}

// personalExecutorGap reports why one (connection, model) pair out of the user's own configuration
// cannot be used, or returns an empty reason when it can. The personal default model is the same pair
// read through it (defaultExecutorGap), so the default slot and a mapped slot are judged by exactly
// the same rules: the connection must still be the user's, enabled and undeleted, it must still carry
// a credential, and the chosen model must still be one the connection offers.
func personalExecutorGap(t *transaction, uid, connectionID, modelID string) (reason string, connection, model Object) {
	c := t.one("SELECT * FROM personal_model_connections WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL AND enabled", connectionID, uid)
	if c == nil {
		return slotReasonConnectionDown, nil, nil
	}
	if c.S("credentialId") == "" {
		return slotReasonCredentialMissing, c, nil
	}
	m := findModel(c, modelID)
	if m == nil {
		return slotReasonModelUnavailable, c, nil
	}
	return "", c, m
}

// bindingRow shapes one frozen binding's columns from a resolved connection + model. The connection's
// facts are copied rather than referenced: the binding is the record of what the run was authorized
// against, so editing the connection afterwards cannot rewrite it.
func bindingRow(c, model Object) Object {
	return Object{
		"id":                newID(),
		"connectionId":      c.S("id"),
		"connectionVersion": c.N("version"),
		"credentialId":      c.S("credentialId"),
		"connectionName":    c.S("name"),
		"protocol":          c.S("protocol"),
		"baseUrl":           c.S("baseUrl"),
		"authMode":          c.S("authMode"),
		"model":             model,
	}
}
