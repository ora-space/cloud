package integration

// Phase 4C S6 acceptance for the Thread change notification (Thread D5, plan §4C.7/§4C.8,
// T4C-25..T4C-27), covering both event types the approved A4/G-023 amendment defines.
//
// The chain under test is the production one, end to end: the session start, the public Thread POST,
// the Controller takeover and the public end endpoint each change the Thread, and each has to release
// its notices *after* that commit, on the tenant's space stream, carrying the run's own identity and
// the commit-time max(seq). The subscriber side is the real SSE endpoint (Gin → credentials → space
// read → SpaceHub), so the payload is asserted as JSON on the wire and not as a Go struct.
//
// The two names answer two questions and are not interchangeable (A4): `thread_appended` still means
// "this commit appended at least one entry", and `thread_changed` means "this commit changed the
// Thread's REST representation", which includes the state-only commits — a turn flipping
// `queued → delivered`, `threadState` moving `active ⇄ idle` or to `ending` — that the append event
// never covered. A commit that did both publishes one of each; a state-only commit publishes only the
// generalized one.
//
// What the tests are careful *not* to claim: neither notice is data, and neither is authoritative.
// Nothing here asserts ordering across concurrent commits or an at-least-once delivery, and a lost
// notice costs nothing because the Thread GET is the only thing that advances a client's cursor.
// Multiple-instance delivery is out of scope (G-020), so what is pinned is the single-process hub.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wanglongan587/cloud/internal/core"
)

// threadAppendedType is Thread D5's public append-event name, written as the wire literal the client
// sees rather than through the core constant: the contract is the name, not the identifier.
const threadAppendedType = "issue_run.thread_appended"

// threadChangedType is A4's generalized invalidation-event name. It is a *separate* name rather than a
// widened `thread_appended`, so a subscriber that filters for new entries keeps its filter — which is
// only true if the wire spelling is asserted independently here.
const threadChangedType = "issue_run.thread_changed"

// threadNoticePayload is the complete field set both notices carry. Asserting it as a set rather than
// field by field is what keeps a later change from widening the payload with something that reads
// like data: a client that can mistake a notice for content would skip the GET that is the only
// authority.
var threadNoticePayload = []string{"type", "spaceId", "issueId", "runId", "lastSeq"}

// liveThreadScene is one seeded agent run driven up to a live session: the fixture's verified user
// is a member of the scene's tenant, so the Thread POST, the Thread GET and the space stream are all
// issued over real HTTP as that user.
type liveThreadScene struct {
	threadScene
	spaceID, nodeID string
	// Filled in by start (the production session-declaration path).
	executionID, initialTurnID string
}

// seedLiveThreadScene seeds the scene at the storage boundary — tenant, project, Space Agent, issue,
// `starting` agent run, its run Workspace and Node — and joins the fixture's user to it. The session
// itself is NOT declared here: the caller subscribes first, then calls start, so the notice the
// declaration publishes is observed rather than missed. Cloud's IssueRun/Thread lifecycle is bound
// onto the control plane's seam exactly as cmd/server wires it (`core.BindBusinessHooks`), so the
// transitions below are the shipped B side and not a test double.
func seedLiveThreadScene(t *testing.T, f *fixture) liveThreadScene {
	t.Helper()
	core.BindBusinessHooks(f.store)
	// The scene is a deployment with object storage configured, because that is the only deployment in
	// which the session's own end has a delivery to declare: Cloud declares a DeliverRevision attempt
	// only when it will be able to verify the objects the Node uploads, and settles the delivery as
	// `skipped` (releasing the run at once) when the Store has no ObjectStore. Tests that need the
	// unconfigured deployment clear the field themselves, and `useObjectStore` is idempotent, so a
	// test that installed its own store first keeps watching the same endpoint.
	f.useObjectStore()

	runID, _, nodeID := seedStartingAgentRun(t, f.store)
	var tenantID, issueID string
	must(t, f.store.Pool.QueryRow(`SELECT tenant_id, issue_id FROM issue_runs WHERE id=$1`, runID).Scan(&tenantID, &issueID))
	_, e := f.store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenantID, f.uid)
	must(t, e)
	var spaceID string
	must(t, f.store.Pool.QueryRow(`SELECT id FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL`, tenantID).Scan(&spaceID))
	return liveThreadScene{
		threadScene: threadScene{tenantID: tenantID, issueID: issueID, runID: runID},
		spaceID:     spaceID, nodeID: nodeID,
	}
}

// start runs the two production steps that make the run a live session: Store.StartQueuedAgentSessionsOnce
// writes the immutable seq=1 first prompt and declares the agent_session work, then a Controller
// claims and registers that work, which is the state the Node must be in before it can send events.
func (s *liveThreadScene) start(t *testing.T, f *fixture) {
	t.Helper()
	ctx := context.Background()
	must(t, f.store.StartQueuedAgentSessionsOnce(ctx))

	claims := &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}}
	picked, e := f.store.Control(ctx, &core.ControlRequest{Action: "clone_claim", Body: core.Object{"epoch": 1}, Service: claims})
	must(t, e)
	work := picked.O("work")
	if work == nil {
		t.Fatal("the started run must have declared exactly one agent_session work item")
	}
	if work.S("runId") != s.runID {
		t.Fatalf("claimed work belongs to run %s, want %s", work.S("runId"), s.runID)
	}
	s.executionID = "exec-sse-" + work.S("id")[:8]
	_, e = f.store.Control(ctx, &core.ControlRequest{
		Action:  "clone_dispatch",
		Body:    core.Object{"operationId": s.runID, "executionId": s.executionID, "nodeId": s.nodeID, "input": work.O("input"), "epoch": 1},
		Service: claims,
	})
	must(t, e)
	must(t, f.store.Pool.QueryRow(`SELECT turn_id FROM thread_entries WHERE run_id=$1 AND seq=1`, s.runID).Scan(&s.initialTurnID))
}

// threadStream is one live SSE subscription. It is addressed by tenant and space rather than through
// f.path because the Thread scenes live in their own tenant: the notice is published on that
// tenant's sole space, which is the stream a Thread subscriber holds.
type threadStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
}

// subscribeTenant opens a raw SSE stream for one tenant's space as an explicit caller. The existing
// f.subscribe helper always addresses the fixture's own tenant; the Thread scenes live in their own,
// and the authorization question is the same one either way (only a member of that tenant may hold
// its space stream), so the closed response is returned for the refusal case too.
func (f *fixture) subscribeTenant(t *testing.T, tid, sid string, u core.Claims, want int) *http.Response {
	t.Helper()
	svc, e := f.client.Credentials.Token("gateway", core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "gateway-a"}})
	must(t, e)
	u.Caller = "gateway-a"
	tok, e := f.client.Credentials.Token("user", u)
	must(t, e)
	req, e := http.NewRequest("GET", f.cloud.URL+"/api/v1/tenants/"+tid+"/spaces/"+sid+"/events", nil)
	must(t, e)
	req.Header.Set("Authorization", "Bearer "+svc)
	req.Header.Set("X-Ora-User-Token", tok)
	res, e := (&http.Client{}).Do(req)
	must(t, e)
	if res.StatusCode != want {
		res.Body.Close()
		t.Fatalf("subscribe to space %s: want %d got %d", sid, want, res.StatusCode)
	}
	return res
}

// openThreadStream subscribes as the fixture's verified user and returns once the stream is open, so
// a notice published after this call is guaranteed to be observable (the handler subscribes before
// it writes the response headers).
func (f *fixture) openThreadStream(t *testing.T, tid, sid string) *threadStream {
	t.Helper()
	//nolint:bodyclose // the body is not leaked, it is the stream: ownership moves to threadStream, whose close() ends the subscription (one test closes it early on purpose, to prove a lost notice costs nothing).
	res := f.subscribeTenant(t, tid, sid, f.user, 200)
	return &threadStream{body: res.Body, reader: bufio.NewReader(res.Body)}
}

// next reads SSE lines until one carries a data frame and requires it to be the given event type.
// Blank separator lines are skipped: one frame is `data: {...}\n\n`, so every second line a reader
// sees is the empty line that terminated the previous frame.
func (s *threadStream) next(t *testing.T, wantType string) core.Object {
	t.Helper()
	ev := s.nextAny(t)
	if got := ev.S("type"); got != wantType {
		t.Fatalf("event type = %q, want %q (%v)", got, wantType, ev)
	}
	return ev
}

// nextAny reads the next data frame whatever its type, which is what a commit that publishes two
// notices needs: D5 guarantees *which* notices a commit publishes and says nothing about their
// relative order, so a case that pinned an order would be asserting an implementation detail rather
// than the contract.
func (s *threadStream) nextAny(t *testing.T) core.Object {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		line, e := s.readLine(deadline)
		if e != nil {
			t.Fatalf("stream ended before the next event: %v", e)
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("not an SSE data frame: %q", line)
		}
		ev := core.Object{}
		must(t, json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &ev))
		return ev
	}
}

// nextCommit reads the notices one Thread commit published, requiring exactly the given types to
// arrive — each once, in any order. It is how a case states the contract at the level the contract is
// written: "this commit publishes these notices", not "this commit publishes them in this sequence".
func (s *threadStream) nextCommit(t *testing.T, want ...string) map[string]core.Object {
	t.Helper()
	got := make(map[string]core.Object, len(want))
	for range want {
		ev := s.nextAny(t)
		typ := ev.S("type")
		if _, dup := got[typ]; dup {
			t.Fatalf("the commit published %s twice: %v", typ, ev)
		}
		got[typ] = ev
	}
	for _, typ := range want {
		if _, ok := got[typ]; !ok {
			t.Fatalf("the commit must publish %v, got the types %v", want, keysOf(got))
		}
	}
	return got
}

func keysOf(m map[string]core.Object) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readLine reads one line on a goroutine so the read is bounded by the caller's deadline rather than
// by the stream: a subscriber that was sent nothing must fail the test instead of hanging it.
func (s *threadStream) readLine(deadline <-chan time.Time) (string, error) {
	type lineOrError struct {
		line string
		e    error
	}
	lines := make(chan lineOrError, 1)
	go func() {
		line, e := s.reader.ReadString('\n')
		lines <- lineOrError{line, e}
	}()
	select {
	case <-deadline:
		return "", fmt.Errorf("no event within the deadline")
	case got := <-lines:
		return got.line, got.e
	}
}

func (s *threadStream) close() { s.body.Close() }

// watchSpace subscribes to the in-process hub so a test can assert that a mutation published
// nothing. The assertion is conclusive rather than a race: a notice is released by Store.transact
// before the mutating call returns, and Publish sends without blocking, so an empty channel after
// the call means nothing was ever published.
func (f *fixture) watchSpace(sid string) (<-chan core.SpaceEvent, func()) {
	return f.store.Events.Subscribe(sid)
}

// requireSilent asserts that a mutation published nothing at all. It drains rather than peeks, so a
// transaction that queued two hints and released one still fails: "rollback publishes nothing" is a
// claim about the whole commit, not about its first event.
func requireSilent(t *testing.T, hints <-chan core.SpaceEvent, what string) {
	t.Helper()
	for {
		select {
		case ev := <-hints:
			t.Fatalf("%s published %v, want no notice at all", what, ev)
		default:
			return
		}
	}
}

// assertNotice checks one notice against Thread D5's payload: the type the caller names, the space the
// subscriber holds, the run's own identity from the database and the commit-time max(seq). It also
// refuses any Thread content and proves the field set is exactly the five contract fields — a notice
// that could be mistaken for data would invite a client to skip the GET that is the only authority.
func assertNotice(t *testing.T, ev core.Object, wantType string, scene liveThreadScene, lastSeq int64, what string) {
	t.Helper()
	if got := ev.S("type"); got != wantType {
		t.Fatalf("%s: type = %q, want %q", what, got, wantType)
	}
	if got := ev.S("spaceId"); got != scene.spaceID {
		t.Fatalf("%s: spaceId = %q, want the tenant's space %s", what, got, scene.spaceID)
	}
	if got := ev.S("issueId"); got != scene.issueID {
		t.Fatalf("%s: issueId = %q, want %s", what, got, scene.issueID)
	}
	if got := ev.S("runId"); got != scene.runID {
		t.Fatalf("%s: runId = %q, want %s", what, got, scene.runID)
	}
	if got := ev.N("lastSeq"); got != lastSeq {
		t.Fatalf("%s: lastSeq = %d, want %d", what, got, lastSeq)
	}
	got := make([]string, 0, len(ev))
	for k := range ev {
		got = append(got, k)
	}
	sort.Strings(got)
	want := append([]string(nil), threadNoticePayload...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: payload field set = %v, want exactly %v — a notification must never carry Thread data", what, got, want)
	}
}

// assertAppended is assertNotice for the append-specific event.
func assertAppended(t *testing.T, ev core.Object, scene liveThreadScene, lastSeq int64, what string) {
	t.Helper()
	assertNotice(t, ev, threadAppendedType, scene, lastSeq, what)
}

// assertChanged is assertNotice for A4's generalized event. It carries the same five fields and the
// same commit-time max(seq) as the append event, which is what lets a client treat the two
// identically: re-read with GET from its own cursor.
func assertChanged(t *testing.T, ev core.Object, scene liveThreadScene, lastSeq int64, what string) {
	t.Helper()
	assertNotice(t, ev, threadChangedType, scene, lastSeq, what)
}

// takeover submits one Thread event batch through the control action a Controller reaches over gRPC
// (Store.Control is the internal command API; the gRPC wire mapping is covered by
// TestAgentRunThreadTakeoverOverGRPC). submissionID is optional: empty submits an unkeyed batch.
func (f *fixture) takeover(t *testing.T, scene liveThreadScene, submissionID string, events ...core.Object) (int64, error) {
	t.Helper()
	claims := &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}}
	out, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action:       "thread_events",
		SubmissionID: submissionID,
		Body: core.Object{
			"operationId": scene.runID, "executionId": scene.executionID, "epoch": 1, "events": events,
		},
		Service: claims,
	})
	if e != nil {
		return 0, e
	}
	return out.N("takenOverThrough"), nil
}

// turnStatus reads one user turn's durable delivery status.
func (f *fixture) turnStatus(runID, turnID string) string {
	var status string
	must(f.t, f.store.Pool.QueryRow(`SELECT status FROM thread_entries WHERE run_id=$1 AND turn_id=$2`, runID, turnID).Scan(&status))
	return status
}

// T4C-25 — the notices are published only after the entry-write transaction commits, with the
// identity of the run that wrote and the max(seq) of that commit. All three entry-write paths are
// exercised on the wire: the session declaration (seq=1), the public POST (a user turn) and the
// Controller takeover (Node records). Each of them both appends an entry and changes the Thread's
// representation, so each publishes one `thread_appended` and one `thread_changed` (A4/G-023). A
// takeover that writes entries and then fails publishes nothing, and the Thread GET still returns
// exactly what committed.
func TestThreadAppendedIsPublishedOnlyAfterCommit(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)

	// The subscriber exists before the first write: a notice nobody was watching for would not prove
	// anything about the ordering this test is about.
	stream := f.openThreadStream(t, scene.tenantID, scene.spaceID)
	defer stream.close()

	// assertCommit reads one commit's two notices and checks both payloads, so the pair is asserted as
	// one fact about one commit rather than as two independent events that happened to be adjacent.
	assertCommit := func(lastSeq int64, what string) {
		t.Helper()
		pair := stream.nextCommit(t, threadAppendedType, threadChangedType)
		assertAppended(t, pair[threadAppendedType], scene, lastSeq, what)
		assertChanged(t, pair[threadChangedType], scene, lastSeq, what)
	}

	// (1) The session declaration writes seq=1 in its own transaction.
	scene.start(t, f)
	assertCommit(1, "session declaration")

	// (2) The public POST writes a user turn in its own transaction.
	posted := f.postThread(scene.threadScene, "sse-turn-1", threadBody("please rename it"), 201, "")
	if posted.O("resource").S("turnId") == "" {
		t.Fatal("the POST response must carry the turn id the entry was written with")
	}
	assertCommit(2, "Thread POST")

	// (3) The takeover writes a Node record, which is also the running authority: one commit, one
	// notice, and lastSeq counts Thread entries (seq=1 prompt + seq=2 user turn + seq=3 record)
	// rather than the Node's own sequence, which is what keeps the two identities unmixed.
	taken, e := f.takeover(t, scene, "", threadLineObjectWith(t, 1, "", "update", "working on the fix"))
	must(t, e)
	if taken != 1 {
		t.Fatalf("takeover reported %d, want 1", taken)
	}
	assertCommit(3, "takeover")
	var phase, status, threadState string
	must(t, f.store.Pool.QueryRow(`SELECT phase,status,thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase, &status, &threadState))
	if phase != "running" || status != "running" || threadState != "active" {
		t.Fatalf("the notice's commit must also be the running authority, got %s/%s/%s", phase, status, threadState)
	}

	// (4) Rollback publishes nothing. This batch is accepted, writes its first record, and then hits an
	// unknown record type: the hook aborts, the entries and receipts roll back together, and both
	// notices queued by the first write are never released.
	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	if _, e := f.takeover(t, scene, "", threadLineObjectWith(t, 2, "", "update", "half a batch"),
		threadLineObjectWith(t, 3, "", "notAHistoryTag", "never lands")); e == nil {
		t.Fatal("a batch with an unknown record type must abort the transaction")
	}
	requireSilent(t, hints, "a rolled-back takeover")
	if n := f.threadEntries(scene.runID); n != 3 {
		t.Fatalf("a rolled-back batch must leave the Thread at 3 entries, got %d", n)
	}
	var lastEventSequence int64
	must(t, f.store.Pool.QueryRow(`SELECT last_event_sequence FROM node_executions WHERE execution_id=$1`, scene.executionID).Scan(&lastEventSequence))
	if lastEventSequence != 1 {
		t.Fatalf("a rolled-back batch must not advance last_event_sequence, got %d", lastEventSequence)
	}

	// The authoritative read is unaffected by any of this: the durable log holds exactly the committed
	// entries, in seq order, and the state the takeover committed.
	window := f.getThread(scene.threadScene, "after=0", 200)
	if got := itemSeqs(t, threadItems(t, window)); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("Thread window = %v, want seq 1..3", got)
	}
	if got := window.S("threadState"); got != "active" {
		t.Fatalf("threadState = %q, want active", got)
	}
}

// noticesOf reads the exact set of notices a mutation published on the hub. Delivery is synchronous
// and non-blocking (SpaceHub.Publish runs inside Store.transact, before the mutating call returns), so
// a channel that is momentarily empty is conclusive rather than a race — which is what lets a case
// assert "exactly these notices" instead of "at least these".
func noticesOf(t *testing.T, hints <-chan core.SpaceEvent, want ...string) map[string]core.SpaceEvent {
	t.Helper()
	got := map[string]core.SpaceEvent{}
drain:
	for {
		select {
		case ev := <-hints:
			if _, dup := got[ev.Type]; dup {
				t.Fatalf("the commit published %s twice: %v", ev.Type, ev)
			}
			got[ev.Type] = ev
		default:
			break drain
		}
	}
	if len(got) != len(want) {
		t.Fatalf("the commit published %v, want exactly %v", noticeNames(got), want)
	}
	for _, typ := range want {
		if _, ok := got[typ]; !ok {
			t.Fatalf("the commit published %v, want exactly %v", noticeNames(got), want)
		}
	}
	return got
}

func noticeNames(m map[string]core.SpaceEvent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// assertHubNotice checks the identity a hub-delivered notice carries. The wire shape is asserted over
// SSE (assertNotice); this is the same contract read one layer in, where a case can also prove that
// nothing else was published for the same commit.
func assertHubNotice(t *testing.T, ev core.SpaceEvent, wantType string, scene liveThreadScene, lastSeq int64, what string) {
	t.Helper()
	if ev.Type != wantType || ev.SpaceID != scene.spaceID || ev.IssueID != scene.issueID || ev.RunID != scene.runID || ev.LastSeq != lastSeq {
		t.Fatalf("%s: notice = %+v, want %s on space %s issue %s run %s lastSeq %d",
			what, ev, wantType, scene.spaceID, scene.issueID, scene.runID, lastSeq)
	}
}

// threadStateOf reads the run's durable Thread state and idle instant, which is the change each notice
// below is supposed to be about.
func (f *fixture) threadStateOf(runID string) (state string, idleSince *time.Time, seq int64) {
	f.t.Helper()
	must(f.t, f.store.Pool.QueryRow(`SELECT thread_state, idle_since, COALESCE((SELECT MAX(seq) FROM thread_entries WHERE run_id=$1),0) FROM issue_runs WHERE id=$1`, runID).Scan(&state, &idleSince, &seq))
	return state, idleSince, seq
}

// T4C-26 (plan §4C.7/§4C.8, A4/G-023) — every commit that changes the Thread's REST representation
// publishes `issue_run.thread_changed`, whether or not it appended an entry.
//
// The matrix drives the changes the production writers make — a user turn moving `queued →
// delivered`, `threadState` moving `active → idle` and back, and `ending` — and reads the durable row
// after each one, so a notice is pinned to a change that actually happened rather than to a code path
// that happens to call the helper. The reader-side half is asserted too: every change is visible in
// the GET, which is what makes the notice a pointer to something rather than a claim of its own.
//
// The two commits that append an entry publish *both* names — the append event keeps its old meaning
// and the generalized one covers the rest of the same commit — while the two state-only commits
// publish only the generalized one. That split is the whole point of A4 being a new event instead of
// a widened one.
func TestThreadChangedCoversStateOnlyCommits(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)

	posted := f.postThread(scene.threadScene, "sse-turn-1", threadBody("please rename it"), 201, "")
	turnID := posted.O("resource").S("turnId")

	// published runs one mutation with a fresh subscription open and returns exactly the notices that
	// one commit released, so each step below is asserted against its own commit rather than against a
	// stream the previous step is still writing to.
	published := func(mutate func(), want ...string) map[string]core.SpaceEvent {
		t.Helper()
		hints, done := f.watchSpace(scene.spaceID)
		defer done()
		mutate()
		return noticesOf(t, hints, want...)
	}

	// (1) The Node echoes the user turn. Cloud takes it over — `queued → delivered` — and appends
	// nothing: no entry, no seq, and one `thread_changed`. This is the commit the append event never
	// covered, and the one a subscriber that only watches appends would sleep through.
	notices := published(func() {
		taken, e := f.takeover(t, scene, "", threadLineObjectWith(t, 1, turnID, "update", "working on the fix"))
		must(t, e)
		if taken != 1 {
			t.Fatalf("takeover reported %d, want 1", taken)
		}
	}, threadChangedType)
	assertHubNotice(t, notices[threadChangedType], threadChangedType, scene, 2, "the echo")
	if got := f.turnStatus(scene.runID, turnID); got != "delivered" {
		t.Fatalf("the echo must still commit queued → delivered, got %q", got)
	}
	if n := f.threadEntries(scene.runID); n != 2 {
		t.Fatalf("an echo must not append an entry, got %d entries", n)
	}
	// The change the notice points at is visible exactly where a client re-reads: the same two
	// entries, the same history bytes, and the turn's status advanced.
	window := f.getThread(scene.threadScene, "after=0", 200)
	items := threadItems(t, window)
	if got := itemSeqs(t, items); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("re-reading the window after an echo = %v, want seq 1..2", got)
	}
	if got := items[1].S("status"); got != "delivered" {
		t.Fatalf("the re-read window must show the turn delivered, got %q", got)
	}
	if got := messageText(t, items[1]); got != "please rename it" {
		t.Fatalf("a status flip must leave the history byte-identical, got %q", got)
	}
	// An echo is not takeover evidence, so the run itself has not moved: the notice reports the Thread
	// change and claims nothing about the run's phase.
	var phase string
	must(t, f.store.Pool.QueryRow(`SELECT phase FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase))
	if phase != "starting" {
		t.Fatalf("an echo is not takeover evidence, so the run must still be starting, got %q", phase)
	}

	// (2) The end of a turn takes the Thread to `idle` (D-4C-04). This commit both appends the
	// TurnEnded record and writes `idle_since`, so it publishes one of each name — and the generalized
	// one is the only one that describes the state half.
	notices = published(func() {
		if _, e := f.takeover(t, scene, "", threadLineObjectWith(t, 2, "", "turnEnded", "")); e != nil {
			t.Fatalf("TurnEnded takeover: %v", e)
		}
	}, threadAppendedType, threadChangedType)
	assertHubNotice(t, notices[threadAppendedType], threadAppendedType, scene, 3, "the TurnEnded record")
	assertHubNotice(t, notices[threadChangedType], threadChangedType, scene, 3, "the TurnEnded record")
	state, idleSince, seq := f.threadStateOf(scene.runID)
	if state != "idle" || idleSince == nil || seq != 3 {
		t.Fatalf("a TurnEnded batch must leave an idle Thread with a start instant and 3 entries, got %s/%v/%d", state, idleSince, seq)
	}
	if got := f.getThread(scene.threadScene, "after=0", 200).S("idleSince"); got == "" {
		t.Fatal("the state change the notice announced must be readable back")
	}

	// (3) A continuation record taken over while the Thread sits idle makes the conversation live
	// again: `idle → active` with `idle_since` cleared, again alongside the appended record.
	notices = published(func() {
		if _, e := f.takeover(t, scene, "", threadLineObjectWith(t, 3, "", "update", "one more thing")); e != nil {
			t.Fatalf("continuation takeover: %v", e)
		}
	}, threadAppendedType, threadChangedType)
	assertHubNotice(t, notices[threadAppendedType], threadAppendedType, scene, 4, "the continuation record")
	assertHubNotice(t, notices[threadChangedType], threadChangedType, scene, 4, "the continuation record")
	state, idleSince, seq = f.threadStateOf(scene.runID)
	if state != "active" || idleSince != nil || seq != 4 {
		t.Fatalf("a continuation must leave an active Thread with no idle instant and 4 entries, got %s/%v/%d", state, idleSince, seq)
	}

	// (4) The user ends the conversation. This commit changes the Thread's representation and appends
	// nothing at all — the pure state-only case — so it publishes exactly one generalized notice and no
	// append event. It is asserted over SSE as well, because this is the path whose payload a client
	// receives on the wire.
	stream := f.openThreadStream(t, scene.tenantID, scene.spaceID)
	defer stream.close()
	notices = published(func() { f.endThread(scene.threadScene, "sse-end", 202, "") }, threadChangedType)
	assertHubNotice(t, notices[threadChangedType], threadChangedType, scene, 4, "the user end")
	assertChanged(t, stream.next(t, threadChangedType), scene, 4, "the user end")
	state, _, seq = f.threadStateOf(scene.runID)
	if state != "ending" || seq != 4 {
		t.Fatalf("ending a Thread must move the state and append nothing, got %s with %d entries", state, seq)
	}
}

// A4's dedupe (plan §4C.7) — one commit changes the Thread's representation at most once as far as the
// notice is concerned. A batch that both delivers an echoed user turn *and* appends records changes two
// things, and a per-change burst of partly-stale notices would tell a subscriber nothing a single hint
// does not: the client re-reads with GET from its own cursor either way.
//
// The append event is deliberately not deduped away — it answers a different question ("this commit
// appended entries") and the batch did append one — so the assertion is one of each, not one overall.
func TestThreadChangedIsDedupedPerCommit(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)
	posted := f.postThread(scene.threadScene, "sse-turn-1", threadBody("please rename it"), 201, "")
	turnID := posted.O("resource").S("turnId")

	hints, done := f.watchSpace(scene.spaceID)
	defer done()
	// One batch, two changes: the echo moves the turn to `delivered`, and the update record appends an
	// entry. The dedupe raises the queued hint's lastSeq rather than adding a second event, so both
	// notices carry the batch's final max(seq) — 3, the appended record — and neither carries the
	// intermediate value the echo alone would have produced.
	taken, e := f.takeover(t, scene, "",
		threadLineObjectWith(t, 1, turnID, "update", "working on the fix"),
		threadLineObjectWith(t, 2, "", "update", "and another"))
	must(t, e)
	// `takenOverThrough` is the batch's highest Node sequence, not an entry count: the echo produces no
	// entry and the update record produces one, and the two facts are asserted separately below.
	if taken != 2 {
		t.Fatalf("the batch must be taken over through node sequence 2, got %d", taken)
	}
	notices := noticesOf(t, hints, threadAppendedType, threadChangedType)
	assertHubNotice(t, notices[threadAppendedType], threadAppendedType, scene, 3, "the echo-plus-append batch")
	assertHubNotice(t, notices[threadChangedType], threadChangedType, scene, 3, "the echo-plus-append batch")
	if got := f.turnStatus(scene.runID, turnID); got != "delivered" {
		t.Fatalf("the echo half of the batch must still commit, got %q", got)
	}
	// Two entries below the batch's max seq: the first prompt and the record. A third would mean the
	// echo was written as an entry as well, which is exactly the dedupe this test is about.
	if n := f.threadEntries(scene.runID); n != 3 {
		t.Fatalf("the append half of the batch must still commit exactly one entry, got %d entries", n)
	}
}

// T4C-25, rollback half — a notice queued by an entry write is released only if the transaction
// commits. The Thread POST queues its hint on the user-turn INSERT and then fails on a later statement
// of that same transaction, which is exactly the shape the guarantee is about: the write happened, the
// hint was queued right after it, and the commit never did. Nothing is durable, so nothing may be
// published.
//
// The failing statement is induced at the database boundary rather than through an injectable seam,
// because the merged design has none: the delivery command is written by in-transaction SQL
// (`enqueueThreadCommand`) with a body this path builds and validates itself, so the only way it can
// fail is the database refusing it — which is what the probe constraint does. The command write is
// the transaction's last statement, so an entry, both hint events and the Thread-state CAS have all
// already succeeded when it is refused.
func TestThreadAppendedRollbackReleasesNothing(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	// The Thread state a declared session would have, so the POST reaches its own writes.
	must(t, f.setThreadState(scene.runID, "starting", "pending"))
	_, e := f.store.Pool.Exec(`ALTER TABLE thread_commands ADD CONSTRAINT thread_commands_rollback_probe CHECK (kind <> 'submit_user_turn')`)
	must(t, e)

	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	f.postThread(scene.threadScene, "sse-rollback", threadBody("will not land"), 500, "internal_error")
	requireSilent(t, hints, "a rolled-back Thread POST")
	if n := f.threadEntries(scene.runID); n != 0 {
		t.Fatalf("a rolled-back POST must leave no entry, got %d", n)
	}
	if n := f.threadCommands(scene.runID); n != 0 {
		t.Fatalf("a rolled-back POST must leave no command, got %d", n)
	}
	var state string
	must(t, f.store.Pool.QueryRow(`SELECT thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&state))
	if state != "pending" {
		t.Fatalf("a rolled-back POST must leave the Thread state alone, got %q", state)
	}
}

// T4C-25, commit half — the rollback shape the POST's own validation cannot produce: every statement
// succeeds, the hint is queued, and the COMMIT itself rejects the transaction. The probe is a deferred
// foreign key on `turn_id` (the only constraint kind PostgreSQL can defer), added to this test's own
// schema after the session is already running, so the failing check is provably the commit's and not
// the POST's. Nothing is durable, so nothing may be published: this is the case the release gate
// exists for, and the one a publish-inside-the-transaction implementation would fail.
func TestThreadAppendedCommitFailureReleasesNothing(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)

	// A deferred FK: the check runs at COMMIT, after the closure has returned. The POST writes a fresh
	// turn_id that is not a user id, so the commit is the statement that fails. NOT VALID skips the
	// rows already in the Thread (the first prompt's turn id is a fresh uuid too) while still checking
	// every row this transaction inserts.
	_, e := f.store.Pool.Exec(`
		ALTER TABLE thread_entries ADD CONSTRAINT thread_entries_commit_probe
		FOREIGN KEY (turn_id) REFERENCES users(id) NOT VALID DEFERRABLE INITIALLY DEFERRED`)
	must(t, e)

	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	status, out, e := f.threadRequest("POST", threadMessagesPath(scene.tenantID, scene.issueID, scene.runID), threadBody("lost to the commit"), "sse-commit-fail")
	must(t, e)
	if status != 500 || out.S("code") != "internal_error" {
		t.Fatalf("a commit failure must surface as the stable internal fault, got %d %v", status, out)
	}
	requireSilent(t, hints, "a POST whose commit failed")
	if n := f.threadEntries(scene.runID); n != 1 {
		t.Fatalf("the failed commit must leave only the first prompt, got %d entries", n)
	}
	if n := f.threadCommands(scene.runID); n != 0 {
		t.Fatalf("the failed commit must leave no command, got %d", n)
	}
	var state string
	must(t, f.store.Pool.QueryRow(`SELECT thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&state))
	if state != "pending" {
		t.Fatalf("the failed commit must leave the Thread state alone, got %q", state)
	}
}

// T4C-27 — duplicates, reordering and loss are all harmless because the cursor is advanced only by
// the Thread GET. A re-delivered batch publishes no second notice and changes no data; a notice that
// nobody receives (no subscriber at all, or a disconnected one) costs nothing, because the next GET
// from the client's own cursor still returns the complete, ordered window.
func TestThreadAppendedDuplicatesAndLossAreHarmless(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	scene.start(t, f)

	stream := f.openThreadStream(t, scene.tenantID, scene.spaceID)
	taken, e := f.takeover(t, scene, "", threadLineObjectWith(t, 1, "", "update", "working on the fix"))
	must(t, e)
	if taken != 1 {
		t.Fatalf("takeover reported %d, want 1", taken)
	}
	assertAppended(t, stream.next(t, threadAppendedType), scene, 2, "takeover")

	// A duplicate: the Controller lost the reply and re-sends the identical batch. The receipts make it
	// a pure replay — no hook, no entry, no second notice — and the log is byte-for-byte what it was.
	hints, cancel := f.watchSpace(scene.spaceID)
	defer cancel()
	replay, e := f.takeover(t, scene, "", threadLineObjectWith(t, 1, "", "update", "working on the fix"))
	must(t, e)
	if replay != taken {
		t.Fatalf("replay reported %d, want %d", replay, taken)
	}
	requireSilent(t, hints, "a replayed takeover batch")
	if n := f.threadEntries(scene.runID); n != 2 {
		t.Fatalf("a replay must not append, got %d entries", n)
	}

	// A loss: the subscriber goes away, and a further write is committed with nobody listening. The
	// notice is dropped (the hub is volatile, buffer 8, no replay) and the data is not.
	stream.close()
	f.postThread(scene.threadScene, "sse-turn-2", threadBody("and again"), 201, "")

	// Recovery is the client's own GET: the whole window, in order, including the entry whose notice
	// was lost. Repeating the same read is stable, so a duplicate or reordered notice can never move
	// what the client converges on.
	window := f.getThread(scene.threadScene, "after=0", 200)
	seqs := itemSeqs(t, threadItems(t, window))
	if len(seqs) != 3 || seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("Thread window after a lost notice = %v, want seq 1..3", seqs)
	}
	again := itemSeqs(t, threadItems(t, f.getThread(scene.threadScene, "after=0", 200)))
	if len(again) != len(seqs) || again[2] != seqs[2] {
		t.Fatalf("re-reading the same cursor must be stable: %v then %v", seqs, again)
	}

	// The notice is not authorization bypass either: a caller who cannot read the space cannot hold its
	// stream, so the run's Thread can never be announced to a non-member.
	outsider := core.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "outsider"}, Source: "corp", DisplayName: "Outsider"}
	refused := f.subscribeTenant(t, scene.tenantID, scene.spaceID, outsider, 403)
	refused.Body.Close()
}

// Mandate §6.2 — `running` has exactly one authority: the first real Node Thread record, taken over
// with its receipt in one commit. This test drives every other write path the run exposes, in the
// order a Controller drives them, and reads the durable row after each one: the session declaration
// (which is also ClaimWork and RecordDispatch registering the execution), the public Thread POST
// (entry + enqueueThreadCommand + post-commit ThreadCommandAvailable), ClaimThreadCommands and
// RecordThreadCommandDelivered. None of them may advance the run; the takeover at the end then
// commits running/running/active in one step, so the invariant is shown to bite rather than merely to
// be never contradicted.
func TestOnlyTheFirstRecordAdvancesRunning(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedLiveThreadScene(t, f)
	claims := &core.Claims{Kind: "service", Role: "controller", RegisteredClaims: jwt.RegisteredClaims{Subject: "ctrl-a"}}

	// readRun is the invariant's own evidence: the durable row, not the response of whichever path
	// just ran.
	readRun := func() (phase, status, threadState string) {
		t.Helper()
		must(t, f.store.Pool.QueryRow(`SELECT phase,status,thread_state FROM issue_runs WHERE id=$1`, scene.runID).Scan(&phase, &status, &threadState))
		return phase, status, threadState
	}
	assertStillStarting := func(step string) {
		t.Helper()
		phase, status, threadState := readRun()
		if phase != "starting" || status != "dispatched" {
			t.Fatalf("%s must not move the run, got %s/%s", step, phase, status)
		}
		if threadState == "running" {
			t.Fatalf("%s must not make the Thread running", step)
		}
	}

	// (1) StartSession declares the session (immutable seq=1, `pending`), and the Controller claims the
	// agent_session work item (ClaimWork) and registers the execution (RecordDispatch).
	scene.start(t, f)
	assertStillStarting("StartSession + ClaimWork + RecordDispatch")
	if _, _, state := readRun(); state != "pending" {
		t.Fatalf("a declared session must leave the Thread pending, got %q", state)
	}

	// (2) The public Thread POST: entry, command and idempotency record commit together, the Thread
	// turns active, and the run is still where StartSession left it.
	posted := f.postThread(scene.threadScene, "key-authority", threadBody("are you there?"), 201, "")
	turnID := posted.O("resource").S("turnId")
	assertStillStarting("the Thread POST")
	if _, _, state := readRun(); state != "active" {
		t.Fatalf("an accepted turn must leave the Thread active, got %q", state)
	}

	// (3) ClaimThreadCommands is a pure read that returns the command the POST released.
	claimed, e := f.store.Control(context.Background(), &core.ControlRequest{
		Action: "thread_claim", Body: core.Object{"epoch": 1, "limit": 100}, Service: claims,
	})
	must(t, e)
	commands, _ := claimed["commands"].([]core.Object)
	if len(commands) != 1 {
		t.Fatalf("the POST's command must be claimable, got %v", commands)
	}
	if got := commands[0].O("body").S("turnId"); got != turnID {
		t.Fatalf("claimed turn_id = %q, want the posted %q", got, turnID)
	}
	assertStillStarting("ClaimThreadCommands")

	// (4) RecordThreadCommandDelivered registers the delivery, and the run has still not moved.
	_, e = f.store.Control(context.Background(), &core.ControlRequest{
		Action: "thread_delivered", SubmissionID: "sub-authority",
		Body: core.Object{"epoch": 1, "commandId": commands[0].S("commandId"), "executionId": scene.executionID}, Service: claims,
	})
	must(t, e)
	assertStillStarting("RecordThreadCommandDelivered")

	// The two delivery facts are different stages, not two names for one thing: the command is now
	// registered delivered (the Node accepted it), while the turn's entry is still `queued` because the
	// Node has not echoed the turn_id yet through the takeover. Registration never writes the entry.
	var registered *string
	must(t, f.store.Pool.QueryRow(`SELECT delivered_at::text FROM thread_commands WHERE id=$1`, commands[0].S("commandId")).Scan(&registered))
	if registered == nil {
		t.Fatal("the delivery registration must record delivered_at")
	}
	if got := f.turnStatus(scene.runID, turnID); got != "queued" {
		t.Fatalf("registering a delivery is not the turn's echo: entry status = %q, want queued", got)
	}

	// (5) The Node's echo of the turn arrives first. It settles the turn — the stage the delivery
	// registration above is *not* — and appends no entry, so it is still not a running authority: the
	// run is exactly where it was.
	if _, e = f.takeover(t, scene, "", threadLineObjectWith(t, 1, turnID, "update", "the agent sees your turn")); e != nil {
		t.Fatalf("echo takeover: %v", e)
	}
	if got := f.turnStatus(scene.runID, turnID); got != "delivered" {
		t.Fatalf("the echo, not the registration, delivers the turn: entry status = %q", got)
	}
	assertStillStarting("the echo takeover")

	// (6) The first real Node Thread record is the one path that advances the run, in the same commit
	// that writes its entry.
	taken, e := f.takeover(t, scene, "", threadLineObjectWith(t, 2, "", "update", "working on the fix"))
	must(t, e)
	if taken != 2 {
		t.Fatalf("takeover reported %d, want 2", taken)
	}
	phase, status, threadState := readRun()
	if phase != "running" || status != "running" || threadState != "active" {
		t.Fatalf("the first real record must be the running authority, got %s/%s/%s", phase, status, threadState)
	}
}

// threadLineObjectWith builds one Node record as the control batch carries it: the flattened
// ora-history line as the JSON *text* the wire's `record` field is (the control plane re-parses it
// and stores the exact bytes as the receipt), plus, for the echoes, the turn_id the Node is
// answering. The inner line is asserted to be a JSON object here so a malformed fixture fails the
// test rather than the batch.
func threadLineObjectWith(t *testing.T, sequence int64, turnID, tag, text string) core.Object {
	t.Helper()
	line := threadLine(tag, text)
	if turnID != "" && tag == "update" {
		// Every caller that names a turn means that turn's echo, which the Node writes as the user
		// message record; an agent reply would carry the turn id too but is not an echo.
		line = userTurnLine(text)
	}
	parsed := core.Object{}
	if e := json.Unmarshal([]byte(line), &parsed); e != nil || parsed == nil {
		t.Fatalf("fixture: thread line %q is not a JSON object: %v", line, e)
	}
	ev := core.Object{"sequence": sequence, "record": line}
	if turnID != "" {
		ev["turnId"] = turnID
	}
	return ev
}
