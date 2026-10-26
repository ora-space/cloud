package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/pluginmarket"
)

const personalAgentDeclaration = `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["openai-completions","anthropic-messages"],"modelCardinality":"single"}},"settings":{"style":{"type":"string","title":"Style","description":"Output style","required":true}}}`

type personalAgentScene struct {
	store       *Store
	claims      *Claims
	uid, space  string
	seed        dispSeed
	connection  Object
	fingerprint string
}

// newPersonalAgentScene supplies a selected, verified release and an owned connection. It does
// not claim a deployed adapter: individual admission tests must establish that capability.
func newPersonalAgentScene(t *testing.T) personalAgentScene {
	t.Helper()
	s := commandStore(t)
	seed := seedDispatchScene(t, s.Pool, false)
	claims, uid := modelUser(t, s, "agent-owner")
	connection, _ := configuredModel(t, s, claims)
	if _, err := s.Pool.Exec("UPDATE personal_model_connections SET protocol='anthropic-messages' WHERE id=$1", connection.S("id")); err != nil {
		t.Fatal(err)
	}
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(personalAgentDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	var space string
	if err := s.Pool.QueryRow("SELECT space_id FROM projects WHERE id=$1", seed.project).Scan(&space); err != nil {
		t.Fatal(err)
	}
	release := Object{
		"id": claudeAgentCLI, "kind": "agent", "version": "1.0.0", "url": "https://packages.example.invalid/agent.orax", "sha256": strings.Repeat("a", 64),
		"agentConfiguration": Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": personalAgentDeclaration},
	}
	_, err = s.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')", seed.tenant, uid)
		tx.exec("UPDATE space_agents SET plugin_id=$2 WHERE id=$1", seed.agent, claudeAgentCLI)
		tx.exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,selected_release)
			VALUES($1,$2,'official','ora-space.claude','installed','1.0.0',$3)`, space, seed.tenant, jsonText(release))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return personalAgentScene{s, claims, uid, space, seed, connection, compiled.Fingerprint}
}

func (s personalAgentScene) request(method, suffix, key string, body Object) (Object, error) {
	result, _, err := s.store.Public(context.Background(), &PublicRequest{
		Method: method, Path: "/api/v1/me/agent-configurations/" + s.space + "/official/ora-space.claude" + suffix,
		SpaceID: s.space, AgentPluginID: claudeAgentCLI, Identity: s.claims, Key: key, Body: body,
	})
	return result, err
}

func (s personalAgentScene) body(revision int64) Object {
	return Object{
		"revision": revision, "selectionVersion": int64(1), "declarationFingerprint": s.fingerprint,
		"settings": Object{"style": "concise"}, "connectionId": s.connection.S("id"), "modelId": "vendor/model-v1",
	}
}

func TestPersonalAgentConfigurationCASClearAndReplay(t *testing.T) {
	s := newPersonalAgentScene(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := s.request("PUT", "", uuid.NewString(), s.body(0))
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if ErrorCode(err).Code == "revision_conflict" {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	key := uuid.NewString()
	cleared, err := s.request("DELETE", "", key, Object{"revision": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.request("DELETE", "", key, Object{"revision": int64(1)})
	if err != nil || jsonText(cleared) != jsonText(replay) {
		t.Fatalf("replay=%v error=%v", replay, err)
	}
	if cleared.O("resource").N("revision") != 2 || !cleared.O("resource").B("cleared") {
		t.Fatal(cleared)
	}
	_, err = s.request("PUT", "", uuid.NewString(), s.body(0))
	expectModelFault(t, err, "revision_conflict")
	_, err = s.request("PUT", "", uuid.NewString(), s.body(2))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.request("DELETE", "", key, Object{"revision": int64(3)})
	expectModelFault(t, err, "idempotency_conflict")
}

func TestPersonalAgentConfigurationRepairAndOwnership(t *testing.T) {
	s := newPersonalAgentScene(t)
	_, err := s.request("PUT", "", uuid.NewString(), s.body(0))
	if err != nil {
		t.Fatal(err)
	}
	other, otherID := modelUser(t, s.store, "agent-admin")
	if _, err := s.store.Pool.Exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')", s.seed.tenant, otherID); err != nil {
		t.Fatal(err)
	}
	ownerClaims := s.claims
	s.claims = other
	view, err := s.request("GET", "", "", nil)
	if err != nil || view.N("revision") != 0 {
		t.Fatalf("foreign values: %v %v", view, err)
	}
	_, err = s.request("PUT", "", uuid.NewString(), s.body(0))
	expectModelFault(t, err, "not_found")
	s.claims = ownerClaims
	changed := personalAgentDeclaration + "\n"
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Pool.Exec(`UPDATE space_plugins SET version=version+1,
		selected_release=jsonb_set(selected_release,'{agentConfiguration}',$2::jsonb) WHERE space_id=$1`,
		s.space, jsonText(Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": changed})); err != nil {
		t.Fatal(err)
	}
	view, err = s.request("GET", "", "", nil)
	if err != nil || !view.B("requiresRepair") || view.O("settings").S("style") != "concise" {
		t.Fatalf("repair view=%v error=%v", view, err)
	}
	body := s.body(1)
	body["selectionVersion"], body["declarationFingerprint"] = int64(2), compiled.Fingerprint
	_, err = s.request("PUT", "", uuid.NewString(), body)
	expectModelFault(t, err, "agent_configuration_repair_required")
	fixed, err := s.request("POST", "/repair", uuid.NewString(), body)
	if err != nil || fixed.O("resource").N("revision") != 2 {
		t.Fatalf("repair=%v error=%v", fixed, err)
	}
	_, err = s.request("POST", "/repair", uuid.NewString(), body)
	expectModelFault(t, err, "revision_conflict")
}

func TestPersonalAgentRunFreezeAndUnavailableAdapter(t *testing.T) {
	s := newPersonalAgentScene(t)
	_, err := s.request("PUT", "", uuid.NewString(), s.body(0))
	if err != nil {
		t.Fatal(err)
	}
	input := Object{"agentPluginId": claudeAgentCLI, "agentPluginVersion": "1.0.0"}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		bindRunModel(tx, s.seed.run, s.uid, input, "")
		return nil
	})
	expectModelFault(t, err, "agent_adapter_unavailable")
	var count int
	if err := s.store.Pool.QueryRow("SELECT count(*) FROM run_model_bindings").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial binding: %d %v", count, err)
	}
	s.store.PersonalAgentRuntime = &PersonalAgentRuntime{Target: "x86_64-unknown-linux-gnu", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		bindRunModel(tx, s.seed.run, s.uid, input, "")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.store.transact(context.Background(), func(tx *transaction) Object {
		return tx.one("SELECT * FROM run_agent_configurations WHERE run_id=$1", s.seed.run)
	})
	if err != nil {
		t.Fatal(err)
	}
	body := s.body(1)
	body["settings"] = Object{"style": "expanded"}
	if _, err := s.request("PUT", "", uuid.NewString(), body); err != nil {
		t.Fatal(err)
	}
	after, err := s.store.transact(context.Background(), func(tx *transaction) Object {
		return tx.one("SELECT * FROM run_agent_configurations WHERE run_id=$1", s.seed.run)
	})
	if err != nil || jsonText(before) != jsonText(after) || after.O("settings").S("style") != "concise" {
		t.Fatalf("snapshot changed: %v %v", after, err)
	}
	if before.S("packageSha256") != strings.Repeat("a", 64) || before.N("configurationRevision") != 1 {
		t.Fatal(before)
	}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`UPDATE space_plugins SET desired_version='2.0.0',selected_release=jsonb_set(selected_release,'{version}','"2.0.0"') WHERE space_id=$1`, s.space)
		created := createRunWorkspace(tx, s.seed.run)
		plugins, _ := pluginSnapshot(tx, created.O("operation"))
		if len(plugins) != 1 || plugins[0].S("version") != "1.0.0" {
			t.Fatalf("queued release drifted: %v", plugins)
		}
		operation := created.O("operation")
		enterPluginStep(tx, operation)
		operation = tx.one("SELECT * FROM operations WHERE id=$1", operation.S("id"))
		spaceBefore := tx.spacePluginRow(s.space, "official", "ora-space.claude")
		pluginInstanceWriteback(tx, operation, claudeAgentCLI, "installed", "1.0.0", nil, before.N("selectionRevision"))
		spaceAfter := tx.spacePluginRow(s.space, "official", "ora-space.claude")
		if jsonText(spaceBefore) != jsonText(spaceAfter) {
			t.Fatalf("frozen run overwrote current space evidence: before=%v after=%v", spaceBefore, spaceAfter)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersonalAgentAdmissionRollback(t *testing.T) {
	s := newPersonalAgentScene(t)
	_, err := s.request("PUT", "", uuid.NewString(), s.body(0))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("UPDATE issue_runs SET status='completed' WHERE id=$1", s.seed.run)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		return enqueueAdmittedRun(tx, s.seed.tenant, s.seed.issue, "agent", s.seed.agent,
			Object{"agentPluginId": claudeAgentCLI, "agentPluginVersion": "1.0.0"}, "manual", nil, "user", s.uid)
	})
	expectModelFault(t, err, "agent_adapter_unavailable")
	var runs, bindings, snapshots int
	if err := s.store.Pool.QueryRow(`SELECT (SELECT count(*) FROM issue_runs),(SELECT count(*) FROM run_model_bindings),(SELECT count(*) FROM run_agent_configurations)`).
		Scan(&runs, &bindings, &snapshots); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || bindings != 0 || snapshots != 0 {
		t.Fatalf("partial admission: %d %d %d", runs, bindings, snapshots)
	}
}

func TestPersonalAgentConfigurationAcrossSpaces(t *testing.T) {
	s := newPersonalAgentScene(t)
	if _, err := s.request("PUT", "", uuid.NewString(), s.body(0)); err != nil {
		t.Fatal(err)
	}
	second := seedDispatchScene(t, s.store.Pool, false)
	var secondSpace string
	if err := s.store.Pool.QueryRow("SELECT space_id FROM projects WHERE id=$1", second.project).Scan(&secondSpace); err != nil {
		t.Fatal(err)
	}
	changed := personalAgentDeclaration + "\n"
	compiled, err := pluginmarket.CompileAgentConfiguration([]byte(changed))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec("INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')", second.tenant, s.uid)
		tx.exec(`INSERT INTO space_plugins(space_id,tenant_id,source_namespace,identifier,desired_state,desired_version,selected_release)
			SELECT $1,$2,source_namespace,identifier,desired_state,desired_version,
			jsonb_set(selected_release,'{agentConfiguration}',$4::jsonb) FROM space_plugins WHERE space_id=$3`,
			secondSpace, second.tenant, s.space,
			jsonText(Object{"schemaVersion": 2, "declarationSha256": compiled.Fingerprint, "declaration": changed}))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	firstSpace := s.space
	s.space = secondSpace
	view, err := s.request("GET", "", "", nil)
	if err != nil || !view.B("requiresRepair") {
		t.Fatalf("second context: %v %v", view, err)
	}
	body := s.body(1)
	body["declarationFingerprint"] = compiled.Fingerprint
	if _, err := s.request("POST", "/repair", uuid.NewString(), body); err != nil {
		t.Fatal(err)
	}
	s.space = firstSpace
	view, err = s.request("GET", "", "", nil)
	if err != nil || !view.B("requiresRepair") || view.N("revision") != 2 {
		t.Fatalf("first context must now require repair: %v %v", view, err)
	}
}

func TestPersonalAgentThreadOwnershipAndPermanentRevocation(t *testing.T) {
	s := newPersonalAgentScene(t)
	if _, err := s.request("PUT", "", uuid.NewString(), s.body(0)); err != nil {
		t.Fatal(err)
	}
	s.store.PersonalAgentRuntime = &PersonalAgentRuntime{Target: "linux", ContractVersion: 1, Protocols: []string{"anthropic-messages"}}
	_, err := s.store.transact(context.Background(), func(tx *transaction) Object {
		input := Object{"agentPluginId": claudeAgentCLI, "agentPluginVersion": "1.0.0"}
		bindRunModel(tx, s.seed.run, s.uid, input, "")
		run := tx.one("SELECT * FROM issue_runs WHERE id=$1", s.seed.run)
		run["threadState"] = "idle"
		owner := threadModelPermissions(tx, run, s.uid)
		adminID := tx.one("SELECT user_id FROM tenant_memberships WHERE tenant_id=$1 AND role='admin'", s.seed.tenant).S("userId")
		admin := threadModelPermissions(tx, run, adminID)
		if !owner.B("canAppend") || !owner.B("canEnd") || admin.B("canAppend") || !admin.B("canEnd") {
			t.Fatalf("owner=%v admin=%v", owner, admin)
		}
		revokeConnectionModelGrants(tx, s.connection.S("id"))
		tx.exec("UPDATE personal_model_connections SET enabled=true WHERE id=$1", s.connection.S("id"))
		revoked := threadModelPermissions(tx, run, s.uid)
		if revoked.B("canAppend") || !revoked.B("canEnd") {
			t.Fatalf("revocation revived or prevented cleanup: %v", revoked)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersonalAgentInvalidDeclarationCannotUseLegacyFallback(t *testing.T) {
	s := newPersonalAgentScene(t)
	_, err := s.store.Pool.Exec(`UPDATE space_plugins SET selected_release=(selected_release-'agentConfiguration') ||
		'{"agentConfigurationIssue":"invalid declaration"}'::jsonb WHERE space_id=$1`, s.space)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.request("GET", "", "", nil)
	expectModelFault(t, err, "agent_declaration_invalid")
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		_, _, _ = resolveDeclaredAgentRun(tx, s.uid, s.space, claudeAgentCLI, "1.0.0", "")
		return nil
	})
	expectModelFault(t, err, "agent_declaration_invalid")
}

// A numeric setting must keep its literal on every path that answers with it: the first save, the
// read-back and the replay of the same idempotency key. jsonb carries the literal exactly, so a
// boundary that decodes the value into float64 shows up here as a rounded answer to a retry.
func TestPersonalAgentNumericSettingsPreserveLiteralAcrossSaveReadAndReplay(t *testing.T) {
	s := newPersonalAgentScene(t)
	const source = `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}},"settings":{` +
		`"style":{"type":"string","title":"Style","description":"Output style","required":true},` +
		`"counter":{"type":"number","title":"Counter","description":"Large integer","default":9007199254740993},` +
		`"ratio":{"type":"number","title":"Ratio","description":"Precise decimal","required":true}}}`
	declaration, err := pluginmarket.CompileAgentConfiguration([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.Pool.Exec(`UPDATE space_plugins SET selected_release=jsonb_set(selected_release,'{agentConfiguration}',$2::jsonb) WHERE space_id=$1`,
		s.space, jsonText(Object{"schemaVersion": 2, "declarationSha256": declaration.Fingerprint, "declaration": source}))
	if err != nil {
		t.Fatal(err)
	}
	body := s.body(0)
	body["declarationFingerprint"], body["settings"] = declaration.Fingerprint,
		Object{"style": "concise", "ratio": json.Number("0.123456789012345678901")}
	key := uuid.NewString()
	saved, err := s.request("PUT", "", key, body)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.request("GET", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.request("PUT", "", key, body)
	if err != nil {
		t.Fatal(err)
	}
	want := jsonText(Object{"style": "concise", "counter": json.Number("9007199254740993"), "ratio": json.Number("0.123456789012345678901")})
	for name, settings := range map[string]Object{
		"save":   saved.O("resource").O("settings"),
		"read":   read.O("settings"),
		"replay": replayed.O("resource").O("settings"),
	} {
		if got := jsonText(settings); got != want {
			t.Fatalf("%s settings = %s, want %s", name, got, want)
		}
	}
	if replayed.O("resource").N("revision") != saved.O("resource").N("revision") {
		t.Fatalf("replay advanced the revision: %v then %v", saved, replayed)
	}
}

func TestPersonalAgentOpenCodeUsesGlobalDefaultAndRepairsRemovedSettings(t *testing.T) {
	s := newPersonalAgentScene(t)
	_, err := s.store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`UPDATE space_plugins SET identifier='ora-space.opencode',
			selected_release=jsonb_set(selected_release,'{id}','"official/ora-space.opencode"') WHERE space_id=$1`, s.space)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := &PublicRequest{
		Method: "PUT", Path: "/api/v1/me/agent-configurations/" + s.space + "/official/ora-space.opencode",
		SpaceID: s.space, AgentPluginID: opencodeAgentCLI, Identity: s.claims, Key: uuid.NewString(), Body: s.body(0),
	}
	_, _, err = s.store.Public(context.Background(), request)
	expectModelFault(t, err, "agent_use_model_default")
	delete(request.Body, "connectionId")
	delete(request.Body, "modelId")
	_, _, err = s.store.Public(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Method, request.Body = "GET", nil
	view, _, err := s.store.Public(context.Background(), request)
	if err != nil || view.O("modelDefault").S("connectionId") != s.connection.S("id") || view.S("connectionId") != "" {
		t.Fatalf("duplicated default: %v %v", view, err)
	}
	source := `{"schemaVersion":2,"agent":{"modelAccess":{"contractVersion":1,"protocols":["anthropic-messages"],"modelCardinality":"single"}}}`
	declaration, err := pluginmarket.CompileAgentConfiguration([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`UPDATE space_plugins SET selected_release=jsonb_set(selected_release,'{agentConfiguration}',$2::jsonb) WHERE space_id=$1`,
			s.space, jsonText(Object{"schemaVersion": 2, "declarationSha256": declaration.Fingerprint, "declaration": source}))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.store.transact(context.Background(), func(tx *transaction) Object {
		_, _, _ = resolveDeclaredAgentRun(tx, s.uid, s.space, opencodeAgentCLI, "1.0.0", "")
		return nil
	})
	expectModelFault(t, err, "agent_configuration_repair_required")
}
