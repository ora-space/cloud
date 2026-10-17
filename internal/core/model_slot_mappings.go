package core

// Personal executor-slot mappings (specs decision 20261015-executor-slot-mappings D1): the
// verified user's slot name -> executor resolution table. Only the owner reads and writes it —
// administrator roles grant nothing here. `default` is deliberately excluded: it IS the personal
// default model (personal_model_defaults), never a stored mapping. A mapping survives its
// connection being deleted, disabled or losing the model — the run-creation gate reports the
// reason instead of the row vanishing, so the user can see what broke.

const opencodeAgentCLI = "official/ora-space.opencode"

func personalSlotMappingsPublic(t *transaction, r *PublicRequest, uid string) (out Object, status int) {
	if r.SlotName == "" {
		require(r.Method == "GET", 405, "method_not_allowed")
		pageResult := page(t, slotMappingQuery+"WHERE m.user_id=$1", []any{uid}, "m.slot_name", r)
		items, ok := pageResult["items"].([]Object)
		require(ok, 500, "internal_error")
		for i, entry := range items {
			items[i] = slotMappingView(entry)
		}
		return pageResult, 200
	}
	require(workflowSlotNamePattern.MatchString(r.SlotName) && r.SlotName != workflowDefaultSlot, 400, "invalid_slot_name")
	hash := requestHash(r.Method, r.Path, r.Body)
	if r.Method == "DELETE" {
		require(r.Key != "", 400, "idempotency_key_required")
		if old := personalModelReplay(t, uid, r.Key, hash); old != nil {
			return old.O("response"), int(old.N("status"))
		}
	}
	_, hasVersion := r.Body["version"]
	require(hasVersion, 428, "version_required")
	// Every write answers 200 with the resource, the way the connection routes do, and that is also
	// the status the idempotency ledger records — a replayed DELETE returns this value, so leaving
	// the named result at its zero would hand the caller a 0 and store one.
	status = 200
	switch r.Method {
	case "PUT":
		old := slotMapping(t, uid, r.SlotName)
		oldVersion := int64(0)
		if old != nil {
			oldVersion = old.N("version")
		}
		require(oldVersion == r.Body.N("version"), 409, "version_conflict")
		require(r.Body.S("agentCli") == opencodeAgentCLI, 400, "unsupported_agent_cli")
		c := modelConnection(t, uid, r.Body.S("connectionId"))
		require(c.B("enabled"), 409, "model_connection_disabled")
		require(findModel(c, r.Body.S("modelId")) != nil, 400, "model_not_found")
		if old == nil {
			t.exec("INSERT INTO executor_slot_mappings(user_id,slot_name,agent_cli,connection_id,model_id,version) VALUES($1,$2,$3,$4,$5,1)", uid, r.SlotName, opencodeAgentCLI, c.S("id"), r.Body.S("modelId"))
		} else {
			t.exec("UPDATE executor_slot_mappings SET agent_cli=$3,connection_id=$4,model_id=$5,version=version+1,updated_at=now() WHERE user_id=$1 AND slot_name=$2", uid, r.SlotName, opencodeAgentCLI, c.S("id"), r.Body.S("modelId"))
		}
		out = Object{"resource": slotMappingView(slotMapping(t, uid, r.SlotName))}
	case "DELETE":
		// Existence is checked before the CAS: deleting a missing row is 404 whatever the body
		// claims its version was.
		old := slotMapping(t, uid, r.SlotName)
		require(old != nil, 404, "not_found")
		require(old.N("version") == r.Body.N("version"), 409, "version_conflict")
		// The row is hard-deleted, so the response carries the view captured before the delete.
		deleted := slotMappingView(old)
		t.exec("DELETE FROM executor_slot_mappings WHERE user_id=$1 AND slot_name=$2", uid, r.SlotName)
		out = Object{"resource": deleted}
		recordPersonalModelWrite(t, uid, r.Key, hash, out, status)
	default:
		reject(405, "method_not_allowed")
	}
	return out, status
}

const slotMappingQuery = `SELECT m.slot_name, m.agent_cli, m.connection_id, m.model_id, m.version, m.created_at, m.updated_at,
	c.name AS connection_name, c.enabled, c.credential_id, c.models, c.deleted_at AS connection_deleted_at
	FROM executor_slot_mappings m JOIN personal_model_connections c ON c.id=m.connection_id AND c.user_id=m.user_id `

// slotMapping loads one owned mapping with its connection context; a foreign or unknown slot is nil.
func slotMapping(t *transaction, uid, slot string) Object {
	return t.one(slotMappingQuery+"WHERE m.user_id=$1 AND m.slot_name=$2", uid, slot)
}

// slotMappingView allowlists fields: the connection name and model label travel, but no service
// address, credential reference or key — the same surface rule as run bindings. `available`
// computes what the run-creation gate would conclude today, so a kept-but-broken mapping shows
// its reason instead of silently failing at run time.
func slotMappingView(m Object) Object {
	o := Object{}
	for _, k := range []string{"slotName", "agentCli", "connectionId", "modelId", "connectionName", "version", "createdAt", "updatedAt"} {
		o[k] = m[k]
	}
	modelName := ""
	available := true
	reason := ""
	c := Object{"models": m["models"]}
	if m.S("connectionName") == "" || m["connectionDeletedAt"] != nil || !m.B("enabled") {
		available, reason = false, "model_connection_unavailable"
	} else if m.S("credentialId") == "" {
		available, reason = false, "model_credential_required"
	} else if model := findModel(c, m.S("modelId")); model == nil {
		available, reason = false, "model_not_found"
	} else {
		modelName = model.S("name")
	}
	o["modelName"] = modelName
	o["available"] = available
	o["unavailableReason"] = reason
	return o
}
