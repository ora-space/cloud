package core

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

type modelAttemptScene struct {
	store *Store
	grant ModelGrant
	read  PublicRequest
}

// Exercise real grant authority and frozen binding, not a fabricated attempt scope.
func seedModelAttemptScene(t *testing.T) modelAttemptScene {
	t.Helper()
	s := commandStore(t)
	scene := seedTakeoverScene(t, s)
	historyExec(t, s, "UPDATE node_instances SET model_proxy=true WHERE node_id=$1", scene.node)
	_, err := s.transact(t.Context(), func(tx *transaction) Object {
		c, n := runtimeControl(tx, scene.ws), currentNode(tx, scene.ws)
		return runtimeControlCommand(tx, &ControlRequest{Action: "runtime_ack", Body: Object{"workspaceId": scene.ws, "controlEpoch": c.N("controlEpoch"), "controlVersion": c.N("version"), "nodeInstanceId": n.S("id"), "inputClosed": false, "unfinishedExecutionIds": []any{}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	var uid string
	if err := s.Pool.QueryRow("SELECT owner_user_id FROM projects WHERE id=$1", scene.seed.seed.project).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	claims := &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "attempt-owner"}, Source: "test-model", Caller: "gateway"}
	historyExec(t, s, "INSERT INTO user_identities(user_id,source,subject) VALUES($1,$2,$3)", uid, claims.Source, claims.Subject)
	configuredModel(t, s, claims)
	var binding string
	_, err = s.transact(t.Context(), func(tx *transaction) Object {
		input := tx.one("SELECT input FROM issue_runs WHERE id=$1", scene.run).O("input")
		input["agentPluginId"] = "official/ora-space.opencode"
		bindRunModel(tx, scene.run, uid, input)
		binding = input.S("modelBindingId")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(newID()))
	g, err := s.CreateModelGrant(t.Context(), ModelRuntimeScope{TenantID: scene.seed.seed.tenant, WorkspaceID: scene.ws, Generation: 1}, binding, scene.execution, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	r := PublicRequest{Method: "GET", TenantID: scene.seed.seed.tenant, IssueID: scene.seed.seed.issue, RunID: scene.run, Identity: claims}
	r.Path = "/api/v1/tenants/" + r.TenantID + "/issues/" + r.IssueID + "/runs/" + r.RunID + "/statistics"
	return modelAttemptScene{s, g, r}
}

func metricInt(value int64) *int64 { return &value }

func completeAttempt() ModelAttemptFinish {
	status := 200
	return ModelAttemptFinish{
		Outcome: "succeeded", HTTPStatus: &status, DurationMS: 100, FirstOutputMS: metricInt(20), TerminalObserved: true,
		Usage: ModelAttemptUsage{InputTokens: metricInt(10), OutputTokens: metricInt(0), TotalTokens: metricInt(10), CacheReadTokens: metricInt(0), InputSemantics: "includes_cache", Availability: "complete"},
	}
}

func TestModelAttemptsPersistFrozenScopeAndImmutableCompletion(t *testing.T) {
	scene := seedModelAttemptScene(t)
	s, g, id := scene.store, scene.grant, newID()
	// Untrusted fields other than grant identifiers cannot change persisted scope/model.
	g.Model.ID, g.Protocol = "untrusted-model", "anthropic-messages"
	if err := s.BeginModelAttempt(t.Context(), &g, id, true); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginModelAttempt(t.Context(), &g, id, true); err != nil {
		t.Fatal(err)
	}
	expectModelFault(t, s.BeginModelAttempt(t.Context(), &g, id, false), "model_attempt_conflict")
	wrong := g
	wrong.RunID = newID()
	expectModelFault(t, s.BeginModelAttempt(t.Context(), &wrong, newID(), true), "model_grant_revoked")
	historyExec(t, s, "UPDATE model_access_grants SET revoked_at=now() WHERE id=$1", g.ID)
	expectModelFault(t, s.BeginModelAttempt(t.Context(), &g, newID(), true), "model_grant_revoked")
	finish := completeAttempt()
	for range 2 {
		if err := s.FinishModelAttempt(t.Context(), id, &finish); err != nil {
			t.Fatal(err)
		}
	}
	finish.DurationMS++
	expectModelFault(t, s.FinishModelAttempt(t.Context(), id, &finish), "model_attempt_conflict")
	expectModelFault(t, s.FinishModelAttempt(t.Context(), newID(), &finish), "not_found")
	r := scene.read
	r.Path = strings.TrimSuffix(r.Path, "statistics") + "model-calls"
	out, _, err := s.Public(t.Context(), &r)
	if err != nil {
		t.Fatal(err)
	}
	items := out["items"].([]Object)
	if len(items) != 1 || items[0].S("modelId") != "vendor/model-v1" || items[0].S("protocol") != "openai-completions" || items[0].N("durationMs") != 100 {
		t.Fatalf("attempt facts changed: %v", out)
	}
	for _, field := range []string{"bindingId", "grantId", "executionId", "observation", "credentialId", "tenantId", "runId"} {
		if _, ok := items[0][field]; ok {
			t.Fatalf("private field exposed: %s", field)
		}
	}
}

func TestModelAttemptConcurrentFinalizationHasOneWinner(t *testing.T) {
	scene := seedModelAttemptScene(t)
	id := newID()
	if err := scene.store.BeginModelAttempt(t.Context(), &scene.grant, id, false); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, duration := range []int64{100, 200} {
		wg.Go(func() {
			finish := completeAttempt()
			finish.DurationMS = duration
			results <- scene.store.FinishModelAttempt(t.Context(), id, &finish)
		})
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if ErrorCode(err).Code == "model_attempt_conflict" {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func TestModelAttemptRejectsInvalidMetricsWithoutClosingIntent(t *testing.T) {
	scene := seedModelAttemptScene(t)
	id := newID()
	if err := scene.store.BeginModelAttempt(t.Context(), &scene.grant, id, true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*ModelAttemptFinish)
	}{
		{"negative duration", func(f *ModelAttemptFinish) { f.DurationMS = -1 }},
		{"output after completion", func(f *ModelAttemptFinish) { f.FirstOutputMS = metricInt(101) }},
		{"unsafe integer", func(f *ModelAttemptFinish) { f.Usage.InputTokens = metricInt(maxMetricInteger + 1) }},
		{"negative tokens", func(f *ModelAttemptFinish) { f.Usage.OutputTokens = metricInt(-1) }},
		{"missing terminal", func(f *ModelAttemptFinish) { f.TerminalObserved = false }},
		{"complete missing input", func(f *ModelAttemptFinish) { f.Usage.InputTokens = nil }},
		{"unavailable with counters", func(f *ModelAttemptFinish) { f.Usage.Availability = "unavailable" }},
		{"partial without counters", func(f *ModelAttemptFinish) {
			f.Usage = ModelAttemptUsage{Availability: "partial", InputSemantics: "unknown"}
		}},
		{"provider text in error", func(f *ModelAttemptFinish) { f.ErrorCode = "private provider message" }},
		{"unknown outcome", func(f *ModelAttemptFinish) { f.Outcome = "running" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finish := completeAttempt()
			tc.change(&finish)
			expectModelFault(t, scene.store.FinishModelAttempt(t.Context(), id, &finish), "invalid_model_attempt")
		})
	}
	finish := completeAttempt()
	if err := scene.store.FinishModelAttempt(t.Context(), id, &finish); err != nil {
		t.Fatalf("invalid attempts changed durable intent: %v", err)
	}
}

func TestSessionModelStatisticsCoveragePaginationAndReadOnly(t *testing.T) {
	scene := seedModelAttemptScene(t)
	s := scene.store
	for index := range 3 {
		id := newID()
		if err := s.BeginModelAttempt(t.Context(), &scene.grant, id, true); err != nil {
			t.Fatal(err)
		}
		if index == 2 {
			continue // Durable start with no final observation must not imply zero tokens.
		}
		finish := completeAttempt()
		if index == 1 {
			finish.Outcome, finish.TerminalObserved = "interrupted", false
			finish.DurationMS, finish.FirstOutputMS = 300, nil
			finish.Usage = ModelAttemptUsage{InputTokens: metricInt(5), InputSemantics: "includes_cache", Availability: "partial"}
		}
		if err := s.FinishModelAttempt(t.Context(), id, &finish); err != nil {
			t.Fatal(err)
		}
	}
	s.Pool.SetMaxOpenConns(1)
	historyExec(t, s, "SET default_transaction_read_only=on")
	out, _, err := s.Public(t.Context(), &scene.read)
	if err != nil {
		t.Fatal(err)
	}
	if out.O("counts").N("total") != 3 || out.O("counts").N("started") != 1 || out.O("counts").N("interrupted") != 1 {
		t.Fatalf("counts=%v", out)
	}
	input, output := out.O("usage").O("inputTokens"), out.O("usage").O("outputTokens")
	if input.N("value") != 15 || input.N("knownCount") != 2 || input.N("totalCount") != 3 || input.S("availability") != "partial" || output["value"] == nil || output.N("value") != 0 || out.O("usage").O("cacheWriteTokens")["value"] != nil {
		t.Fatalf("coverage/zero/unknown=%v", out.O("usage"))
	}
	if out.O("timing").O("meanDurationMs").N("value") != 200 || out.O("timing").O("p90DurationMs").N("value") != 300 {
		t.Fatalf("timings=%v", out.O("timing"))
	}
	r := scene.read
	r.Path = strings.TrimSuffix(r.Path, "statistics") + "model-calls"
	r.Limit = 1
	seen := map[string]bool{}
	for range 3 {
		page, _, err := s.Public(t.Context(), &r)
		if err != nil {
			t.Fatal(err)
		}
		items := page["items"].([]Object)
		if len(items) != 1 || seen[items[0].S("id")] {
			t.Fatalf("pagination repeated/skipped: %v", page)
		}
		seen[items[0].S("id")] = true
		r.After = page.S("nextCursor")
	}
	if r.After != "" {
		t.Fatal("last page has a cursor")
	}
	r.After = newID()
	_, _, err = s.Public(t.Context(), &r)
	expectModelFault(t, err, "invalid_pagination")
}
