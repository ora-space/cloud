package core

// bindRunModel freezes the initiating user's personal configuration only for the production
// OpenCode plugin. It runs in enqueueRun's transaction so missing configuration rolls back the
// comment, interaction, activity and run together. Unrelated agents keep their existing contract.
func bindRunModel(t *transaction, runID, uid string, input Object) {
	if input.S("agentPluginId") != "official/ora-space.opencode" {
		return
	}
	d := modelDefault(t, uid)
	require(d.S("connectionId") != "", 409, "model_default_required")
	c := t.one("SELECT * FROM personal_model_connections WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL AND enabled", d.S("connectionId"), uid)
	require(c != nil, 409, "model_connection_unavailable")
	require(c.S("credentialId") != "", 409, "model_credential_required")
	m := findModel(c, d.S("modelId"))
	require(m != nil, 409, "model_default_required")
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
	t.exec(`INSERT INTO run_model_bindings(id,run_id,user_id,connection_id,connection_version,credential_id,connection_name,protocol,base_url,auth_mode,model)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, runID, uid, c.S("id"), c.N("version"), c.S("credentialId"), c.S("name"), c.S("protocol"), c.S("baseUrl"), c.S("authMode"), jsonText(m))
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
