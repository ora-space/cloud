package core

// defaultExecutorGap reports why the user's personal default executor cannot be used for a run, or
// returns an empty reason when it can. It is the one policy behind both the OpenCode run binding and
// the workflow `default` slot, so "the default model is usable" cannot come to mean two different
// things: the default selection must exist, and then the pair it names is judged by the same rules a
// mapped slot's pair is (personalExecutorGap). The connection and model are returned for the caller
// that goes on to freeze them.
//
// It reports a reason rather than panicking because the callers answer differently: the OpenCode
// binding rejects one run with one fault, while the workflow gate collects every unusable slot before
// refusing so the user can fix them in one pass.
func defaultExecutorGap(t *transaction, uid string) (reason string, connection, model Object) {
	d := modelDefault(t, uid)
	if d.S("connectionId") == "" {
		return slotReasonDefaultMissing, nil, nil
	}
	return personalExecutorGap(t, uid, d.S("connectionId"), d.S("modelId"))
}

// bindRunModel freezes the initiating user's personal configuration only for the production
// OpenCode plugin. It runs in enqueueRun's transaction so missing configuration rolls back the
// comment, interaction, activity and run together. Unrelated agents keep their existing contract.
func bindRunModel(t *transaction, runID, uid string, input Object) {
	if input.S("agentPluginId") != opencodeAgentCLI {
		return
	}
	// The faults are unchanged from before the policy was shared: this path has always named the one
	// thing the user has to fix, and a caller of the OpenCode route must keep seeing the same codes.
	reason, c, m := defaultExecutorGap(t, uid)
	switch reason {
	case "":
	case slotReasonConnectionDown:
		reject(409, "model_connection_unavailable")
	case slotReasonCredentialMissing:
		reject(409, "model_credential_required")
	default:
		reject(409, "model_default_required")
	}
	// Git identity is fixed with the model binding so changes to the profile before dispatch
	// cannot change the attribution of this queued personal-model run.
	git := t.one("SELECT name,email FROM user_git_identities WHERE user_id=$1", uid)
	if git == nil {
		var err error
		git, err = defaultGitIdentity(t, uid)
		if err != nil {
			panic(databaseFailure{err})
		}
	}
	id := newID()
	// An IssueRun binds the implicit `default` slot: the run_model_bindings migration (0039)
	// carries slot_name for both run kinds, so a session's binding lookup is uniform
	// (workflow_run_session.go resolves the same way).
	t.exec(`INSERT INTO run_model_bindings(id,run_id,slot_name,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, runID, "default", uid, c.S("id"), c.N("version"), c.S("credentialId"), c.S("name"), c.S("protocol"), c.S("baseUrl"), c.S("authMode"), jsonText(m))
	input["gitIdentity"] = git
	// modelBindingId is a server-owned reference; no key or model-gateway grant enters the
	// durable execution input or Controller stream.
	input["modelBindingId"] = id
	t.exec("UPDATE issue_runs SET input=$2 WHERE id=$1", runID, jsonText(input))
}

func threadModelPermissions(t *transaction, run Object, uid string) Object {
	out := Object{"initiatorUserId": nil, "model": nil, "canAppend": true, "canEnd": true}
	actors := t.list("SELECT DISTINCT actor_id FROM issue_activities WHERE tenant_id=$1 AND issue_id=$2 AND action='run.enqueued' AND actor_type='user' AND actor_id IS NOT NULL AND details->>'runId'=$3", run.S("tenantId"), run.S("issueId"), run.S("id"))
	if len(actors) == 1 {
		out["initiatorUserId"] = actors[0].S("actorId")
	}
	if binding := t.one("SELECT user_id,connection_name,model,revoked_at FROM run_model_bindings WHERE run_id=$1", run.S("id")); binding != nil {
		out["initiatorUserId"] = binding.S("userId")
		out["model"] = Object{"connectionName": binding.S("connectionName"), "modelId": binding.O("model").S("id"), "modelName": binding.O("model").S("name")}
		owner := uid == binding.S("userId")
		out["canAppend"] = owner && binding["revokedAt"] == nil
		member := t.one("SELECT role FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND status='active'", run.S("tenantId"), uid)
		out["canEnd"] = owner || member != nil && member.S("role") == "admin"
	}
	if !threadAcceptStates[run.S("threadState")] || run.S("cancelRequestedAt") != "" {
		out["canAppend"] = false
		out["canEnd"] = false
	}
	return out
}
