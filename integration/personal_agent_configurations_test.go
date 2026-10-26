package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

func TestPersonalAgentConfigurationHTTP(t *testing.T) {
	f := setup(t)
	space := f.defaultSpaceID()
	declaration := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}},"settings":{"style":{"type":"string","title":"Style","description":"Style","required":true}}}`
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(declaration))
	must(t, err)
	release := core.Object{
		"id": "official/ora-space.claude", "kind": "agent", "version": "1.0.0",
		"url": "https://packages.example.invalid/claude.orax", "sha256": strings.Repeat("b", 64),
		"agentConfiguration": core.Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": declaration},
	}
	raw, err := json.Marshal(release)
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,selected_release)
		SELECT id,tenant_id,'official','ora-space.claude','installed','1.0.0',$2 FROM collab_workspaces WHERE id=$1`, space, string(raw))
	must(t, err)
	connection := f.call("POST", "/api/v1/me/model-connections", core.Object{
		"name": "Personal", "protocol": "anthropic-messages", "baseUrl": "https://models.example.invalid/v1", "authMode": "bearer",
		"models": []any{core.Object{"id": "model", "name": "Model", "contextWindow": 128000, "maxTokens": 8192}},
	}, "agent-connection", 201).O("resource")
	path := "/api/v1/me/agent-configurations/" + space + "/official/ora-space.claude"
	initial := f.call("GET", path, nil, "", 200)
	if initial.N("revision") != 0 || !initial.B("cleared") {
		t.Fatal(initial)
	}
	body := core.Object{
		"revision": 0, "selectionVersion": 1, "declarationFingerprint": compiled.Fingerprint,
		"settings": core.Object{"style": "brief"}, "connectionId": connection.S("id"), "modelId": "model",
	}
	f.call("PUT", path, body, "", 400)
	saved := f.call("PUT", path, body, "agent-save", 200)
	replayed := f.call("PUT", path, body, "agent-save", 200)
	if saved.O("resource").N("revision") != 1 || replayed.O("resource").N("revision") != 1 {
		t.Fatal(saved, replayed)
	}
	f.call("PUT", path, body, "agent-stale", 409)
	body["revision"] = 1
	body["settings"] = core.Object{"style": false}
	f.call("PUT", path, body, "agent-wrong-type", 400)
	body["settings"] = core.Object{"style": "brief", "apiKey": "must-not-be-a-setting"}
	f.call("PUT", path, body, "agent-extra-setting", 400)
	body["settings"] = core.Object{"style": "brief"}
	body["userId"] = "caller-selected-owner"
	f.call("PUT", path, body, "agent-owner-field", 400)
	delete(body, "userId")
	encoded, err := json.Marshal(f.call("GET", path, nil, "", 200))
	must(t, err)
	for _, forbidden := range []string{"credentialId", "baseUrl", "apiKey", "token"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("private connection field %s in response", forbidden)
		}
	}
	f.call("DELETE", path, core.Object{"revision": 1}, "agent-clear", 200)
	body["revision"] = 0
	f.call("PUT", path, body, "agent-old-create", 409)
	body["revision"] = 2
	f.call("PUT", path, body, "agent-refill", 200)
}

// A repeated write must answer with the values its first answer returned, including numeric
// settings. jsonb stores a literal exactly, so a value that comes back rounded means some boundary
// decoded it into float64; the responses are therefore compared as JSON text rather than as
// Go-decoded numbers.
func TestPersonalAgentConfigurationHTTPReplayPreservesNumberLiterals(t *testing.T) {
	f := setup(t)
	space := f.defaultSpaceID()
	declaration := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}},"settings":{` +
		`"style":{"type":"string","title":"Style","description":"Style","required":true},` +
		`"counter":{"type":"number","title":"Counter","description":"Large integer","default":9007199254740993},` +
		`"ratio":{"type":"number","title":"Ratio","description":"Precise decimal","required":true}}}`
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(declaration))
	must(t, err)
	release := core.Object{
		"id": "official/ora-space.claude", "kind": "agent", "version": "1.0.0",
		"url": "https://packages.example.invalid/claude.orax", "sha256": strings.Repeat("c", 64),
		"agentConfiguration": core.Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": declaration},
	}
	raw, err := json.Marshal(release)
	must(t, err)
	_, err = f.store.Pool.Exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,selected_release)
		SELECT id,tenant_id,'official','ora-space.claude','installed','1.0.0',$2 FROM collab_workspaces WHERE id=$1`, space, string(raw))
	must(t, err)
	connection := f.call("POST", "/api/v1/me/model-connections", core.Object{
		"name": "Personal", "protocol": "anthropic-messages", "baseUrl": "https://models.example.invalid/v1", "authMode": "bearer",
		"models": []any{core.Object{"id": "model", "name": "Model", "contextWindow": 128000, "maxTokens": 8192}},
	}, "agent-number-connection", 201).O("resource")
	path := "/api/v1/me/agent-configurations/" + space + "/official/ora-space.claude"
	body, err := json.Marshal(core.Object{
		"revision": 0, "selectionVersion": 1, "declarationFingerprint": compiled.Fingerprint,
		"settings":     core.Object{"style": "brief", "ratio": json.Number("0.123456789012345678901")},
		"connectionId": connection.S("id"), "modelId": "model",
	})
	must(t, err)
	savedStatus, savedBody := f.personalAgentRaw("PUT", path, string(body), "agent-number-save")
	replayedStatus, replayedBody := f.personalAgentRaw("PUT", path, string(body), "agent-number-save")
	readStatus, readBody := f.personalAgentRaw("GET", path, "", "")
	want := `{"counter":9007199254740993,"ratio":0.123456789012345678901,"style":"brief"}`
	for name, response := range map[string]struct {
		status int
		body   []byte
		nested bool
	}{
		"save":   {savedStatus, savedBody, true},
		"replay": {replayedStatus, replayedBody, true},
		"read":   {readStatus, readBody, false},
	} {
		if response.status != 200 {
			t.Fatalf("%s status = %d: %s", name, response.status, response.body)
		}
		if got := personalAgentSettingsJSON(t, response.body, response.nested); got != want {
			t.Fatalf("%s settings = %s, want %s", name, got, want)
		}
	}
}

// personalAgentRaw performs one authenticated request and returns the undecoded response body, so a
// case can compare the exact JSON text two answers carry.
func (f *fixture) personalAgentRaw(method, path, body, key string) (int, []byte) {
	f.t.Helper()
	req, e := http.NewRequestWithContext(context.Background(), method, f.cloud.URL+path, strings.NewReader(body))
	must(f.t, e)
	req.Header.Set("Content-Type", "application/json")
	service, e := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	must(f.t, e)
	req.Header.Set("Authorization", "Bearer "+service)
	user := f.user
	user.Caller = "gateway-a"
	token, e := f.client.Credentials.Token("user", user)
	must(f.t, e)
	req.Header.Set("X-Ora-User-Token", token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, e := f.cloud.Client().Do(req)
	must(f.t, e)
	defer res.Body.Close()
	payload, e := io.ReadAll(res.Body)
	must(f.t, e)
	return res.StatusCode, payload
}

// personalAgentSettingsJSON returns one response's settings as canonical JSON text while keeping
// every number literal. nested selects the save/replay shape (`{"resource": {...}}`) rather than
// the read shape.
func personalAgentSettingsJSON(t *testing.T, body []byte, nested bool) string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var response map[string]any
	must(t, decoder.Decode(&response))
	if nested {
		resource, ok := response["resource"].(map[string]any)
		if !ok {
			t.Fatalf("response carries no resource object: %s", body)
		}
		response = resource
	}
	settings, ok := response["settings"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no settings object: %s", body)
	}
	encoded, e := json.Marshal(settings)
	must(t, e)
	return string(encoded)
}
