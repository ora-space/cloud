package core

import (
	"net"
	"net/url"
	"strings"
)

// ModelDefinition describes one selectable upstream model. IDs are opaque; slash-bearing IDs
// are preserved exactly rather than split as provider names.
type ModelDefinition struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow"`
	MaxTokens     int64  `json:"maxTokens"`
}

func modelConnection(t *transaction, uid, id string) Object {
	require(validID(id), 404, "not_found")
	c := t.one("SELECT * FROM personal_model_connections WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL", id, uid)
	require(c != nil, 404, "not_found")
	return c
}

// modelConnectionView intentionally allowlists fields: neither the owner nor the credential
// reference, ciphertext or key identity is part of a browser-visible document.
func modelConnectionView(c Object) Object {
	o := Object{}
	for _, k := range []string{"id", "name", "protocol", "baseUrl", "authMode", "models", "enabled", "version", "createdAt", "updatedAt"} {
		o[k] = c[k]
	}
	o["credentialConfigured"] = c.S("credentialId") != ""
	return o
}

func validateModelConnection(body Object) Object {
	name := validText(body.S("name"), 200)
	protocol, auth := body.S("protocol"), body.S("authMode")
	require(protocol == "openai-completions" || protocol == "anthropic-messages", 400, "invalid_model_protocol")
	require(auth == "bearer" || auth == "x-api-key" && protocol == "anthropic-messages", 400, "invalid_model_auth")
	base := strings.TrimRight(strings.TrimSpace(body.S("baseUrl")), "/")
	u, err := url.Parse(base)
	require(err == nil && len(base) <= 2048 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "", 400, "invalid_model_url")
	ip := net.ParseIP(u.Hostname())
	require(ip == nil || !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast(), 400, "invalid_model_url")
	host := strings.ToLower(u.Hostname())
	require(host != "localhost" && !strings.HasSuffix(host, ".localhost") && !strings.HasSuffix(host, ".local"), 400, "invalid_model_url")
	raw, ok := body["models"].([]any)
	require(ok && len(raw) > 0 && len(raw) <= 100, 400, "invalid_model_list")
	models := make([]Object, 0, len(raw))
	seen := map[string]bool{}
	for _, entry := range raw {
		m, ok := entry.(map[string]any)
		require(ok, 400, "invalid_model_list")
		model := Object(m)
		for field := range model {
			require(field == "id" || field == "name" || field == "contextWindow" || field == "maxTokens", 400, "unknown_field")
		}
		id := model.S("id")
		require(id != "" && len(id) <= 500 && id == strings.TrimSpace(id) && !strings.ContainsAny(id, "\r\n\x00") && !seen[id], 400, "invalid_model_id")
		seen[id] = true
		contextWindow, maxTokens := model.N("contextWindow"), model.N("maxTokens")
		require(contextWindow > 0 && contextWindow <= 10000000 && maxTokens > 0 && maxTokens <= contextWindow, 400, "invalid_model_limits")
		models = append(models, Object{"id": id, "name": validText(model.S("name"), 200), "contextWindow": contextWindow, "maxTokens": maxTokens})
	}
	enabled := true
	if v, present := body["enabled"]; present {
		enabled, ok = v.(bool)
		require(ok, 400, "invalid_field_type")
	}
	return Object{"name": name, "protocol": protocol, "baseUrl": base, "authMode": auth, "models": models, "enabled": enabled}
}

func personalModelReplay(t *transaction, uid, key, hash string) Object {
	if key == "" {
		return nil
	}
	require(len(key) <= 200, 400, "idempotency_key_required")
	if old := t.one("SELECT * FROM personal_model_idempotency WHERE user_id=$1 AND key=$2", uid, key); old != nil {
		require(old.S("requestHash") == hash, 409, "idempotency_conflict")
		return old
	}
	return nil
}

func recordPersonalModelWrite(t *transaction, uid, key, hash string, out Object, status int) {
	if key != "" {
		t.exec("INSERT INTO personal_model_idempotency(user_id,key,request_hash,response,status) VALUES($1,$2,$3,$4,$5)", uid, key, hash, jsonText(out), status)
	}
}

func modelDefault(t *transaction, uid string) Object {
	if row := t.one("SELECT connection_id,model_id,version FROM personal_model_defaults WHERE user_id=$1", uid); row != nil {
		return row
	}
	return Object{"connectionId": "", "modelId": "", "version": int64(0)}
}

func findModel(c Object, id string) Object {
	models, _ := c["models"].([]any)
	for _, entry := range models {
		if m, ok := entry.(map[string]any); ok && Object(m).S("id") == id {
			return Object(m)
		}
	}
	return nil
}

func personalModelsPublic(t *transaction, r *PublicRequest, uid string) (out Object, status int) {
	if r.Path == "/api/v1/me/model-default" {
		if r.Method == "GET" {
			return modelDefault(t, uid), 200
		}
		require(r.Method == "PUT", 405, "method_not_allowed")
		_, hasVersion := r.Body["version"]
		require(hasVersion, 428, "version_required")
		old := modelDefault(t, uid)
		require(old.N("version") == r.Body.N("version"), 409, "version_conflict")
		c := modelConnection(t, uid, r.Body.S("connectionId"))
		require(c.B("enabled"), 409, "model_connection_disabled")
		require(findModel(c, r.Body.S("modelId")) != nil, 400, "model_not_found")
		t.exec(`INSERT INTO personal_model_defaults(user_id,connection_id,model_id) VALUES($1,$2,$3)
		 ON CONFLICT(user_id) DO UPDATE SET connection_id=excluded.connection_id,model_id=excluded.model_id,version=personal_model_defaults.version+1`, uid, c.S("id"), r.Body.S("modelId"))
		return modelDefault(t, uid), 200
	}
	if strings.HasSuffix(r.Path, "/credential") {
		reject(404, "credential_gateway_required")
	}
	if r.Method == "GET" {
		if r.ModelConnectionID != "" {
			return modelConnectionView(modelConnection(t, uid, r.ModelConnectionID)), 200
		}
		pageResult := page(t, "SELECT * FROM personal_model_connections WHERE user_id=$1 AND deleted_at IS NULL", []any{uid}, "id", r)
		items, ok := pageResult["items"].([]Object)
		require(ok, 500, "internal_error")
		for i, entry := range items {
			items[i] = modelConnectionView(entry)
		}
		return pageResult, 200
	}
	hash := requestHash(r.Method, r.Path, r.Body)
	if r.Method == "POST" || r.Method == "DELETE" {
		require(r.Key != "", 400, "idempotency_key_required")
		if old := personalModelReplay(t, uid, r.Key, hash); old != nil {
			return old.O("response"), int(old.N("status"))
		}
	}
	status = 200
	var c Object
	switch r.Method {
	case "POST":
		data := validateModelConnection(r.Body)
		id := newID()
		t.exec(`INSERT INTO personal_model_connections(id,user_id,name,protocol,base_url,auth_mode,models,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, uid, data.S("name"), data.S("protocol"), data.S("baseUrl"), data.S("authMode"), jsonText(data["models"]), data.B("enabled"))
		c = modelConnection(t, uid, id)
		status = 201
	case "PUT":
		c = modelConnection(t, uid, r.ModelConnectionID)
		version(c, r.Body.N("version"))
		data := validateModelConnection(r.Body)
		t.exec(`UPDATE personal_model_connections SET name=$2,protocol=$3,base_url=$4,auth_mode=$5,models=$6,enabled=$7,version=version+1,updated_at=now() WHERE id=$1`, c.S("id"), data.S("name"), data.S("protocol"), data.S("baseUrl"), data.S("authMode"), jsonText(data["models"]), data.B("enabled"))
		if !data.B("enabled") {
			revokeConnectionModelGrants(t, c.S("id"))
		}
		c = modelConnection(t, uid, c.S("id"))
	case "DELETE":
		c = modelConnection(t, uid, r.ModelConnectionID)
		version(c, r.Body.N("version"))
		t.exec("UPDATE personal_model_connections SET deleted_at=now(),enabled=false,version=version+1,updated_at=now() WHERE id=$1", c.S("id"))
		revokeConnectionModelGrants(t, c.S("id"))
		c = t.one("SELECT * FROM personal_model_connections WHERE id=$1", c.S("id"))
	default:
		reject(405, "method_not_allowed")
	}
	out = Object{"resource": modelConnectionView(c)}
	if r.Method == "POST" || r.Method == "DELETE" {
		recordPersonalModelWrite(t, uid, r.Key, hash, out, status)
	}
	return out, status
}

func revokeConnectionModelGrants(t *transaction, id string) {
	// Revocation is permanent for frozen run bindings. Re-enabling the connection or writing a
	// new key may start new runs, but cannot re-authorize an old run to use its cleared key.
	t.exec("UPDATE run_model_bindings SET revoked_at=COALESCE(revoked_at,now()) WHERE connection_id=$1", id)
	t.exec(`UPDATE model_access_grants SET revoked_at=COALESCE(revoked_at,now()) WHERE binding_id IN (SELECT id FROM run_model_bindings WHERE connection_id=$1) AND revoked_at IS NULL`, id)
}
