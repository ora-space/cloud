package integration

// Acceptance for the public Thread POST (Thread D3).
// Every case here goes through real HTTP (Gin router → credentials → Store.Public) and real
// PostgreSQL, because the obligations being proven are exactly the ones a unit test cannot hold:
// the accept matrix against stored Thread state, the seven idempotency cases against the real
// generic mechanism, and the A/B atomicity claim — that a user entry, the Thread state change, the
// control-plane command and the caller's idempotency record commit together or not at all.
//
// The scene is seeded at the storage boundary (issue + agent run + its run Workspace), because run
// creation and CreateRunWorkspace are still placeholders here; everything the POST itself does is
// production code, including the delivery command, which the control plane's own in-transaction seam
// writes (`enqueueThreadCommand`) rather than a second writer on the Cloud side.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

// threadScene is one agent run whose tenant the fixture's verified user is an active member of, so
// the Thread POST's authorization (Thread D3: an active member who can read the Issue) is the real
// membership check and not a bypass.
type threadScene struct {
	seed                     skeletonSeed
	tenantID, issueID, runID string
}

// seedThreadScene seeds a whole ownership chain in its own tenant and joins the fixture's verified
// user to it. The run is left where StartSession leaves a declared session: phase `starting`,
// Thread `pending`, nothing taken over yet.
func seedThreadScene(t *testing.T, f *fixture) threadScene {
	t.Helper()
	seed := seedAgentIssueRunSkeleton(t, f.store.Pool)
	_, e := f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, seed.tenantID, f.uid)
	must(t, e)
	scene := threadScene{seed: seed, tenantID: seed.tenantID, issueID: seed.issueID, runID: seed.runID}
	must(t, f.setThreadState(scene.runID, "starting", "pending"))
	return scene
}

// moreRun adds one more agent run on the scene's issue in the given Thread state, which is what lets
// a test ask what the same Idempotency-Key means against a different run (case 6).
func (s threadScene) moreRun(t *testing.T, f *fixture, state string) threadScene {
	t.Helper()
	runID := seedDeclaredAgentRun(t, f.store.Pool, s.seed, "starting", "dispatched", false, false)
	must(t, f.setThreadState(runID, "starting", state))
	return threadScene{seed: s.seed, tenantID: s.tenantID, issueID: s.issueID, runID: runID}
}

// setThreadState moves a seeded run to the exact (phase, Thread state) pair a case is about. state is
// an `any` so a test can seed the NULL Thread state the invariant rows are about.
func (f *fixture) setThreadState(runID, phase string, state any) error {
	_, e := f.store.Pool.Exec(`
		UPDATE issue_runs SET phase=$2, thread_state=$3,
			idle_since=CASE WHEN $3::text = 'idle' THEN now() ELSE NULL END
		WHERE id=$1`, runID, phase, state)
	return e
}

// bindBusinessHooks wires the shipped B side onto the control plane's five callbacks, exactly as
// cmd/server does. setup() deliberately leaves them unset — an unbound seam is the control-plane-only
// deployment, where the A side keeps working and Cloud's lifecycle stays a no-op handoff — so a test
// that expects a run's phase, Thread state or Workspace to move has to ask for the bound seam.
func (f *fixture) bindBusinessHooks() {
	core.BindBusinessHooks(f.store)
}

func threadMessagesPath(tenant, issue, run string) string {
	return "/api/v1/tenants/" + tenant + "/issues/" + issue + "/runs/" + run + "/thread/messages"
}

// threadRequest performs one authenticated public request whose body is raw JSON text, so a case can
// send a body that no core.Object would marshal: malformed, a second JSON value, an unknown field.
// Both credentials the gateway requires are attached exactly as the simulator client attaches them.
func (f *fixture) threadRequest(method, path, body, key string) (int, core.Object, error) {
	return f.threadRequestAs(f.user, method, path, body, key)
}

// threadRequestAs is threadRequest for an explicit caller. The Thread read's authorization cases need
// it: the read must reject exactly the callers the comments read rejects, and proving that means
// issuing both requests as the same verified non-member (Thread D3/D5).
func (f *fixture) threadRequestAs(user core.Claims, method, path, body, key string) (int, core.Object, error) {
	req, e := http.NewRequestWithContext(context.Background(), method, f.cloud.URL+path, strings.NewReader(body))
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	service, e := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("Authorization", "Bearer "+service)
	// The caller arrives by value, so binding it to this gateway is a local mutation.
	user.Caller = "gateway-a"
	token, e := f.client.Credentials.Token("user", user)
	if e != nil {
		return 0, nil, e
	}
	req.Header.Set("X-Ora-User-Token", token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, e := f.cloud.Client().Do(req)
	if e != nil {
		return 0, nil, e
	}
	defer resp.Body.Close()
	out := core.Object{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if e := json.NewDecoder(resp.Body).Decode(&out); e != nil {
			return resp.StatusCode, nil, e
		}
	}
	return resp.StatusCode, out, nil
}

// postThread is threadRequest with the two assertions almost every case shares: the exact expected
// status, and — for the refusals — the stable public code the client is meant to branch on.
func (f *fixture) postThread(scene threadScene, key, body string, want int, code string) core.Object {
	f.t.Helper()
	status, out, e := f.threadRequest("POST", threadMessagesPath(scene.tenantID, scene.issueID, scene.runID), body, key)
	must(f.t, e)
	if status != want {
		f.t.Fatalf("POST thread/messages: want %d got %d %v", want, status, out)
	}
	if code != "" && out.S("code") != code {
		f.t.Fatalf("POST thread/messages: want code %q got %v", code, out)
	}
	return out
}

// threadBody renders the one request body v1 accepts: a single text block.
func threadBody(text string) string {
	return mustJSONRaw(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
}

func mustJSONRaw(v any) string {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return string(b)
}

// messageText reads the first text block out of an entry as it comes back over HTTP: the record is
// jsonb, so its nested values decode to map[string]any and never to core.Object.
func messageText(t *testing.T, entry core.Object) string {
	t.Helper()
	blocks, ok := entry.O("record")["content"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("entry record carries no content blocks: %v", entry.O("record"))
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("content block is not an object: %T", blocks[0])
	}
	text, _ := block["text"].(string)
	return text
}

func (f *fixture) threadEntries(runID string) int {
	return f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, runID)
}

func (f *fixture) threadCommands(runID string) int {
	return f.scalar(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, runID)
}

// queuedTurns lists the run's user turns in Thread order, which is the durable identity the response
// and the command both have to agree with.
func (f *fixture) queuedTurns(runID string) []string {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`SELECT turn_id FROM thread_entries WHERE run_id=$1 AND source='user' ORDER BY seq`, runID)
	must(f.t, e)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		must(f.t, rows.Scan(&id))
		out = append(out, id)
	}
	must(f.t, rows.Err())
	return out
}

// commandTurns lists the turn_id inside every submit_user_turn command a run holds, oldest first.
func (f *fixture) commandTurns(runID string) []string {
	f.t.Helper()
	// The stored command body is the control plane's flat canonical shape (`turnId` and `content`),
	// which is also what the Node-bound wire message is rendered from.
	rows, e := f.store.Pool.Query(`SELECT body->>'turnId' FROM thread_commands WHERE run_id=$1 AND kind='submit_user_turn' ORDER BY queue_sequence`, runID)
	must(f.t, e)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		must(f.t, rows.Scan(&id))
		out = append(out, id)
	}
	must(f.t, rows.Err())
	return out
}

// The accept matrix and the success shape. `pending`, `active` and
// `idle` all accept a new user turn; the write is one `queued` user entry, the Thread moves to
// `active` with `idle_since` cleared, the response carries exactly the seven contract fields, and
// the run's command backlog grows by exactly one command whose turn_id is the entry's.
func TestThreadMessagePostAcceptsPendingActiveAndIdle(t *testing.T) {
	for _, state := range []string{"pending", "active", "idle"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedThreadScene(t, f)

			out := f.postThread(scene, "key-"+state, threadBody("hello "+state), 201, "")
			resource := out.O("resource")
			if resource.S("source") != "user" || resource.S("kind") != "user_turn" || resource.S("status") != "queued" {
				t.Fatalf("entry must be a queued user turn, got %v", resource)
			}
			if resource.N("seq") != 1 || resource.S("createdAt") == "" {
				t.Fatalf("entry must carry its Cloud-assigned seq and created_at, got %v", resource)
			}
			if got := messageText(t, resource); got != "hello "+state {
				t.Fatalf("stored content = %q, want the text the caller sent", got)
			}

			// The turn identity is Cloud's and it is one identity: the response, the entry
			// and the command body all name the same turn_id.
			turnIDs := f.queuedTurns(scene.runID)
			if len(turnIDs) != 1 || turnIDs[0] != resource.S("turnId") {
				t.Fatalf("entries = %v, want exactly the turn the response named (%s)", turnIDs, resource.S("turnId"))
			}
			if turns := f.commandTurns(scene.runID); len(turns) != 1 || turns[0] != resource.S("turnId") {
				t.Fatalf("commands = %v, want exactly the turn the response named (%s)", turns, resource.S("turnId"))
			}
			if f.threadCommands(scene.runID) != 1 {
				t.Fatalf("the POST must release exactly one command, got %d", f.threadCommands(scene.runID))
			}
			var kind string
			var delivered *string
			must(t, f.store.Pool.QueryRow(`SELECT kind, delivered_at::text FROM thread_commands WHERE run_id=$1`, scene.runID).Scan(&kind, &delivered))
			if kind != "submit_user_turn" || delivered != nil {
				t.Fatalf("command = %s delivered=%v, want an undelivered submit_user_turn", kind, delivered)
			}

			// The Thread state change: `active` from every accepting state, and no idle window left
			// open (Thread D4: a new user turn is what makes the Thread active again).
			var threadState string
			var idleSince *string
			must(t, f.store.Pool.QueryRow(`SELECT thread_state, idle_since::text FROM issue_runs WHERE id=$1`, scene.runID).Scan(&threadState, &idleSince))
			if threadState != "active" || idleSince != nil {
				t.Fatalf("run state = %s idle_since=%v, want active with no idle window", threadState, idleSince)
			}
		})
	}
}

// seedForeignThreadScene seeds one more tenant with an issue and a live agent run of its own, and no
// membership for the fixture's user. It exists so the two different answers can be
// told apart: a run that belongs to another tenant while the caller *is* an
// authorized member of the tenant in the path is a plain not-found — the caller must not be able to
// probe for the existence of other tenants' runs — whereas a caller with no membership at all is
// refused by the shared membership check, exactly as the comment path refuses them.
func seedForeignThreadScene(t *testing.T, f *fixture) threadScene {
	t.Helper()
	tenantID, issueID, runID, strangerID := newTestID(), newTestID(), newTestID(), newTestID()
	tx, e := f.store.Pool.Begin()
	must(t, e)
	defer func() { _ = tx.Rollback() }()
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := tx.Exec(q, args...)
		must(t, err)
	}
	exec(`INSERT INTO users(id,display_name,status) VALUES($1,'Stranger','active')`, strangerID)
	exec(`INSERT INTO tenants(id,name,status) VALUES($1,'Foreign','active')`, tenantID)
	exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenantID, strangerID)
	exec(`INSERT INTO issues(id,tenant_id,creator_user_id,title,number) VALUES($1,$2,$3,'Foreign issue',1)`, issueID, tenantID, strangerID)
	exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,phase,thread_state)
		VALUES($1,$2,$3,'agent',$4,'starting','pending')`, runID, tenantID, issueID, newTestID())
	must(t, tx.Commit())
	return threadScene{tenantID: tenantID, issueID: issueID, runID: runID}
}

// The rows Cloud refuses with a client-actionable status: a closed
// Thread is 409, and a run the caller cannot name is 404 rather than a distinguishable "exists but
// not yours". No refusal writes anything.
func TestThreadMessagePostRejectsClosedAndUnknownRuns(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	foreign := seedForeignThreadScene(t, f)

	for _, state := range []string{"ending", "ended"} {
		must(t, f.setThreadState(scene.runID, "running", state))
		f.postThread(scene, "key-closed-"+state, threadBody("too late"), 409, "thread_closed")
	}

	// Every run the caller cannot name in *this* tenant and Issue answers the same way, so none of
	// them can be used to probe whether a run exists somewhere else.
	for i, c := range []threadScene{
		{tenantID: scene.tenantID, issueID: scene.issueID, runID: foreign.runID},
		{tenantID: scene.tenantID, issueID: foreign.issueID, runID: scene.runID},
		{tenantID: scene.tenantID, issueID: scene.issueID, runID: newTestID()},
	} {
		f.postThread(c, "key-foreign-"+strconv.Itoa(i), threadBody("nowhere"), 404, "not_found")
	}
	_, e := f.store.Pool.Exec(`UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, scene.runID)
	must(t, e)
	f.postThread(scene, "key-deleted", threadBody("gone"), 404, "not_found")
	_, e = f.store.Pool.Exec(`UPDATE issue_runs SET deleted_at=NULL WHERE id=$1`, scene.runID)
	must(t, e)

	// A non-agent run has no Thread sub-resource at all.
	teamRun := newTestID()
	_, e = f.store.Pool.Exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id) VALUES($1,$2,$3,'team',$4)`,
		teamRun, scene.tenantID, scene.issueID, newTestID())
	must(t, e)
	f.postThread(threadScene{tenantID: scene.tenantID, issueID: scene.issueID, runID: teamRun}, "key-team", threadBody("not an agent"), 404, "not_found")

	// No membership at all is the shared refusal, not a resource answer: the same 403 the comment
	// path gives, which is what "authorization same as comments" means (Thread D3).
	f.postThread(foreign, "key-stranger", threadBody("not my tenant"), 403, "membership_required")

	for _, run := range []string{scene.runID, foreign.runID, teamRun} {
		if n := f.threadEntries(run); n != 0 {
			t.Fatalf("a refused POST must write nothing to %s, got %d entries", run, n)
		}
		if n := f.threadCommands(run); n != 0 {
			t.Fatalf("a refused POST must release no command for %s, got %d", run, n)
		}
	}
}

// The two state rows Cloud itself guarantees cannot happen are answered as
// internal invariant failures, never as 409: a 409 would tell a client "the Thread is closed" about
// Cloud's own broken state. The transaction aborts, so nothing is written either way.
func TestThreadMessagePostInvariantBreakIsInternal(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	// StartSession materializes `pending`, so an activated session with no Thread
	// state at all has lost its Thread.
	must(t, f.setThreadState(scene.runID, "starting", nil))
	f.postThread(scene, "key-no-thread", threadBody("orphan"), 500, "internal_error")

	// The only writer of phase `running` (the first-record takeover) sets `active` in the same
	// statement, so a running run still reading `pending` is unreachable.
	must(t, f.setThreadState(scene.runID, "running", "pending"))
	f.postThread(scene, "key-pending-running", threadBody("impossible"), 500, "internal_error")

	if n := f.threadEntries(scene.runID); n != 0 {
		t.Fatalf("an invariant break must write nothing, got %d entries", n)
	}
	if n := f.threadCommands(scene.runID); n != 0 {
		t.Fatalf("an invariant break must release no command, got %d", n)
	}
	var state *string
	must(t, f.store.Pool.QueryRow(`SELECT thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&state))
	if state == nil || *state != "pending" {
		t.Fatalf("an invariant break must not move the Thread, got %v", state)
	}
}

// The first POST creates, and a replay under the same key with
// the same body returns the original response without creating a second turn, a second command or a
// second state change. The replay is the same logical response, not a re-render of current state.
func TestThreadMessagePostReplaysUnderTheSameKey(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	first := f.postThread(scene, "key-replay", threadBody("hello"), 201, "")
	replay := f.postThread(scene, "key-replay", threadBody("hello"), 201, "")
	if mustJSONRaw(replay) != mustJSONRaw(first) {
		t.Fatalf("replay = %v, want the original response %v", replay, first)
	}
	if n := f.threadEntries(scene.runID); n != 1 {
		t.Fatalf("a replay must not append a second entry, got %d", n)
	}
	if n := f.threadCommands(scene.runID); n != 1 {
		t.Fatalf("a replay must not release a second command, got %d", n)
	}
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1`, scene.tenantID); n != 1 {
		t.Fatalf("a replay must not write a second idempotency record, got %d", n)
	}
}

// One key names one request. A different body under the
// same key and the same key against a different run are both conflicts, because the recorded request
// hash covers the method, the path and the body.
func TestThreadMessagePostRejectsIdempotencyConflicts(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	f.postThread(scene, "key-once", threadBody("the first body"), 201, "")

	f.postThread(scene, "key-once", threadBody("a different body"), 409, "idempotency_conflict")

	// Case 6: the same key reused against a different run is a different request, so it conflicts
	// instead of replaying a turn that belongs to another Thread.
	second := scene.moreRun(t, f, "active")
	path := threadMessagesPath(second.tenantID, second.issueID, second.runID)
	status, out, e := f.threadRequest("POST", path, threadBody("the first body"), "key-once")
	must(t, e)
	if status != 409 || out.S("code") != "idempotency_conflict" {
		t.Fatalf("reusing a key against another run: want 409 idempotency_conflict, got %d %v", status, out)
	}

	if n := f.threadEntries(second.runID); n != 0 {
		t.Fatalf("a conflicted key must not append to the second run, got %d entries", n)
	}
}

// A different key with an identical body is a new request: two
// independent user turns, two commands, two Cloud-generated turn_ids, contiguous Thread seq.
func TestThreadMessageDistinctKeysCreateIndependentTurns(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	first := f.postThread(scene, "key-a", threadBody("same words"), 201, "")
	second := f.postThread(scene, "key-b", threadBody("same words"), 201, "")
	if first.O("resource").S("turnId") == second.O("resource").S("turnId") {
		t.Fatalf("two requests must get two turn identities, both were %s", first.O("resource").S("turnId"))
	}
	if first.O("resource").N("seq") != 1 || second.O("resource").N("seq") != 2 {
		t.Fatalf("Thread seq must stay contiguous, got %d then %d", first.O("resource").N("seq"), second.O("resource").N("seq"))
	}
	if n := f.threadEntries(scene.runID); n != 2 {
		t.Fatalf("two requests must leave two entries, got %d", n)
	}
	if n := f.threadCommands(scene.runID); n != 2 {
		t.Fatalf("two requests must leave two commands, got %d", n)
	}
}

// Two concurrent requests sharing one key and one body. The
// barrier releases both at once and the assertion is on the complete permitted outcome set: exactly
// one turn is created and both callers see the same logical response (the winner's 201, replayed).
// No sleeps: the outcome is read after both goroutines have returned.
func TestThreadMessageConcurrentSameKeyCreatesOneTurn(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	path := threadMessagesPath(scene.tenantID, scene.issueID, scene.runID)
	body := threadBody("racing")
	start := make(chan struct{})
	results := make([]core.Object, 2)
	statuses := make([]int, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			statuses[i], results[i], errs[i] = f.threadRequest("POST", path, body, "key-race")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent request %d: %v", i, e)
		}
		if statuses[i] != 201 {
			t.Fatalf("concurrent request %d: want 201 (created or replayed) got %d %v", i, statuses[i], results[i])
		}
		if mustJSONRaw(results[i]) != mustJSONRaw(results[0]) {
			t.Fatalf("both callers must see one logical response: %v vs %v", results[0], results[i])
		}
	}
	if n := f.threadEntries(scene.runID); n != 1 {
		t.Fatalf("concurrent same-key requests must create exactly one turn, got %d entries", n)
	}
	if n := f.threadCommands(scene.runID); n != 1 {
		t.Fatalf("concurrent same-key requests must release exactly one command, got %d", n)
	}
}

// All-or-nothing. The POST writes the user entry, both hint events,
// the Thread-state CAS and the delivery command in one transaction; when a statement of that
// transaction is refused, the whole request rolls back: no entry, no state change, no command and no
// idempotency record. The retry under the same key after the cause is gone is then a clean first
// request — which is exactly what the absent idempotency record proves, because a recorded failure
// would have been replayed instead of the 201.
//
// The refusal is induced at the database boundary because there is no injectable seam left to fail:
// `enqueueThreadCommand` is in-transaction SQL whose body this path builds and validates itself. A
// request the database will not take is the one failure the production path can actually reach, so it
// is the one worth proving the rollback against.
func TestThreadMessageWriteFailureRollsBackEverything(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	before := int64(f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID))
	// `thread_commands` is the transaction's last write, so by the time the constraint refuses it the
	// entry, both hint events and the CAS have all already succeeded inside the doomed transaction.
	_, e := f.store.Pool.Exec(`ALTER TABLE thread_commands ADD CONSTRAINT thread_commands_message_probe CHECK (kind <> 'submit_user_turn')`)
	must(t, e)

	f.postThread(scene, "key-503", threadBody("will not land"), 500, "internal_error")

	if n := f.threadEntries(scene.runID); n != 0 {
		t.Fatalf("a refused write must leave no user entry, got %d", n)
	}
	if n := f.threadCommands(scene.runID); n != 0 {
		t.Fatalf("a refused write must leave no command, got %d", n)
	}
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND key='key-503'`, scene.tenantID); n != 0 {
		t.Fatalf("a refused write must not be recorded as a response, got %d records", n)
	}
	var threadState string
	var version int64
	must(t, f.store.Pool.QueryRow(`SELECT thread_state, version FROM issue_runs WHERE id=$1`, scene.runID).Scan(&threadState, &version))
	if threadState != "pending" || version != before {
		t.Fatalf("a refused write must not move the Thread: state=%s version=%d want pending/%d", threadState, version, before)
	}

	_, e = f.store.Pool.Exec(`ALTER TABLE thread_commands DROP CONSTRAINT thread_commands_message_probe`)
	must(t, e)
	out := f.postThread(scene, "key-503", threadBody("will not land"), 201, "")
	if got := messageText(t, out.O("resource")); got != "will not land" {
		t.Fatalf("the retry must write the original body, got %q", got)
	}
	if n := f.threadEntries(scene.runID); n != 1 {
		t.Fatalf("the retry must leave exactly one entry, got %d", n)
	}
	if n := f.threadCommands(scene.runID); n != 1 {
		t.Fatalf("the retry must leave exactly one command, got %d", n)
	}
}

// The boundary refusals the router owns, at the public contract:
// a missing Idempotency-Key, malformed JSON, a second JSON value, an unknown field, a wrong field
// type, an unsupported content block and content over Thread D3's 64 KiB bound. Every one of them is
// a stable 400 code and none of them reaches the database.
func TestThreadMessageRejectsMalformedAndOversizedRequests(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	cases := []struct {
		name, key, body, code string
		want                  int
	}{
		{"missing key", "", threadBody("no key"), "idempotency_key_required", 400},
		{"malformed json", "k1", `{"content":[`, "invalid_json", 400},
		{"extra json value", "k2", threadBody("two") + `{"content":[]}`, "invalid_json", 400},
		{"unknown field", "k3", `{"content":[{"type":"text","text":"x"}],"turnId":"server-owned"}`, "unknown_field", 400},
		{"content not a list", "k4", `{"content":"text"}`, "invalid_field_type", 400},
		{"block not an object", "k5", `{"content":["text"]}`, "invalid_field_type", 400},
		{"unsupported block", "k6", `{"content":[{"type":"image","text":"x"}]}`, "invalid_field_type", 400},
		{"wrong text type", "k7", `{"content":[{"type":"text","text":42}]}`, "invalid_field_type", 400},
		{"block carries server-owned field", "k8", `{"content":[{"type":"text","text":"x","turnId":"mine"}]}`, "invalid_field_type", 400},
		{"over the text bound", "k9", threadBody(strings.Repeat("a", (64<<10)+1)), "content_too_large", 400},
	}
	for _, c := range cases {
		status, out, e := f.threadRequest("POST", threadMessagesPath(scene.tenantID, scene.issueID, scene.runID), c.body, c.key)
		must(t, e)
		if status != c.want || out.S("code") != c.code {
			t.Fatalf("%s: want %d %s, got %d %v", c.name, c.want, c.code, status, out)
		}
	}

	// Exactly at the bound is a valid message: the limit is on the decoded text, so a body whose text
	// is 64 KiB is accepted and only 64 KiB + 1 is refused.
	atBound := f.postThread(scene, "k-bound", threadBody(strings.Repeat("a", 64<<10)), 201, "")
	if len(messageText(t, atBound.O("resource"))) != 64<<10 {
		t.Fatalf("the boundary message must be stored whole")
	}
	if n := f.threadEntries(scene.runID); n != 1 {
		t.Fatalf("only the boundary message may have been written, got %d entries", n)
	}
}

// Thread D3 — a recorded cancellation request closes the Thread to new user
// turns. Cancel is a *request* (IssueRun D6), not a terminal state, so the run is still live, its
// Thread is still `pending`/`active`/`idle`, and the caller's only signal that this conversation is
// winding down is this refusal. Accepting a turn here would persist a message nothing will ever
// execute and report 201 for it.
//
// The refusal is the *same* `409 thread_closed` the closed states answer, not a fourth status: from
// the client's side the two are one fact — this Thread no longer takes turns — and inventing a second
// code would make clients branch on a distinction they cannot act on differently.
//
// The check lives in two places on purpose. The predicate refuses before anything is
// written; the CAS repeats `cancel_requested_at IS NULL` so the guard a client is refused by and the
// guard the write is fenced by can never drift apart. This test proves the first half directly and
// the second by consequence: nothing was written at all, so the row version — which only the CAS
// bumps — is exactly where it was.
func TestThreadMessageRejectsAfterCancellationRequested(t *testing.T) {
	for _, state := range []string{"pending", "active", "idle"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedThreadScene(t, f)
			must(t, f.setThreadState(scene.runID, "starting", state))

			// The contrast that makes this test about cancellation and not about the state matrix: the
			// very same state accepts a turn while no cancellation is recorded. A2's conjunct is the
			// only difference, so an implementation that refused too much fails here and one that
			// refused too little fails below.
			f.postThread(scene, "key-plain", threadBody("still open"), 201, "")
			_, e := f.store.Pool.Exec(`DELETE FROM thread_entries WHERE run_id=$1`, scene.runID)
			must(t, e)
			_, e = f.store.Pool.Exec(`DELETE FROM thread_commands WHERE run_id=$1`, scene.runID)
			must(t, e)
			must(t, f.setThreadState(scene.runID, "starting", state))

			_, e = f.store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1`, scene.runID)
			must(t, e)
			var beforeState string
			var idleBefore *time.Time
			var versionBefore int64
			must(t, f.store.Pool.QueryRow(`SELECT thread_state, idle_since, version FROM issue_runs WHERE id=$1`, scene.runID).Scan(&beforeState, &idleBefore, &versionBefore))

			f.postThread(scene, "key-cancelled", threadBody("too late"), 409, "thread_closed")

			// Nothing survived the refusal. The entry is the user's message, the command is the work
			// that would never run, and the idempotency record is what would make a later retry under
			// the same key replay this 409 as if it were an outcome.
			if n := f.threadEntries(scene.runID); n != 0 {
				t.Fatalf("a refused turn must leave no entry, got %d", n)
			}
			if n := f.threadCommands(scene.runID); n != 0 {
				t.Fatalf("a refused turn must leave no command, got %d", n)
			}
			if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND key='key-cancelled'`, scene.tenantID); n != 0 {
				t.Fatalf("a refusal must not be recorded as a response, got %d records", n)
			}
			// The row is exactly where it was, including its version — which only the CAS bumps — so
			// the second half of A2 (the CAS's own `cancel_requested_at IS NULL`) is proven by
			// consequence: the write that the predicate refused never reached the CAS, and the CAS's
			// conjunct is the same predicate repeated rather than a second, weaker guard.
			var afterState string
			var idleAfter *time.Time
			var versionAfter int64
			must(t, f.store.Pool.QueryRow(`SELECT thread_state, idle_since, version FROM issue_runs WHERE id=$1`, scene.runID).Scan(&afterState, &idleAfter, &versionAfter))
			if afterState != beforeState || versionAfter != versionBefore {
				t.Fatalf("a refused turn must not move the Thread: %s/version %d became %s/version %d", beforeState, versionBefore, afterState, versionAfter)
			}
			if (idleBefore == nil) != (idleAfter == nil) || (idleBefore != nil && !idleBefore.Equal(*idleAfter)) {
				t.Fatalf("a refused turn must leave idle_since alone: %v became %v", idleBefore, idleAfter)
			}
			// A2 is a refusal, not a transition: the run is not made terminal and the Thread is not
			// moved to `ending`. Ending this session is the cancel reaction's job (IssueRun D6), and a
			// public POST must not be able to take it over.
			var phase, threadState string
			must(t, f.store.Pool.QueryRow(`SELECT phase, thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase, &threadState))
			if phase != "starting" || threadState != beforeState {
				t.Fatalf("a refusal must not transition anything: phase=%s thread_state=%s", phase, threadState)
			}

			// The refusal is per-request, not a lock on the key: clearing the request (this run was
			// never actually cancelled) makes the same key a clean first request, which is the durable
			// proof that no response was recorded above.
			_, e = f.store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=NULL WHERE id=$1`, scene.runID)
			must(t, e)
			out := f.postThread(scene, "key-cancelled", threadBody("welcome back"), 201, "")
			if got := messageText(t, out.O("resource")); got != "welcome back" {
				t.Fatalf("the retry must write the body it sent, got %q", got)
			}
			if n := f.threadEntries(scene.runID); n != 1 {
				t.Fatalf("the retry must leave exactly one entry, got %d", n)
			}
		})
	}
}
