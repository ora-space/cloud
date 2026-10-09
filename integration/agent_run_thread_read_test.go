package integration

// Phase 4C S2a acceptance for the public Thread read (Thread D5, plan §4C.3/§4C.16, T4C-7/9/10/11/12),
// covering the whole surface the approved A3/G-017 amendment names.
//
// That surface is the route, the three window forms — no cursor (the tail), `after={seq}`,
// `before={seq}` — their mutual exclusion as `invalid_pagination`, `limit` defaulting to 200 and
// capped at 500, `threadState`, `idleSince`, and the window's `nextCursor`/`prevCursor`. Every window
// comes back ascending by `seq` however it was asked for, and the cursor is always a raw Cloud Thread
// `seq`: never a Node sequence, an SSE event id or an execution id.
//
// Every case goes through real HTTP and real PostgreSQL, because the obligations are the ones a unit
// test cannot hold: the cursor and window contract at the transport boundary (where `limit` is bound),
// authorization against the same membership and Issue scope the comments path uses, the exact field
// set of two different resources, and a walk in both directions that interleaves with real committed
// appends.

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

func threadReadPath(tenant, issue, run string) string {
	return "/api/v1/tenants/" + tenant + "/issues/" + issue + "/runs/" + run + "/thread"
}

// getThread performs one Thread read for the fixture's verified user with the given raw query string
// (empty for none) and asserts the exact expected status.
func (f *fixture) getThread(scene threadScene, query string, want int) core.Object {
	f.t.Helper()
	path := threadReadPath(scene.tenantID, scene.issueID, scene.runID)
	if query != "" {
		path += "?" + query
	}
	status, out, e := f.threadRequest("GET", path, "", "")
	must(f.t, e)
	if status != want {
		f.t.Fatalf("GET thread?%s: want %d got %d %v", query, want, status, out)
	}
	return out
}

// threadItems decodes the response's `items`. The response crosses HTTP, so the nested entries arrive
// as map[string]any and never as core.Object.
func threadItems(t *testing.T, out core.Object) []core.Object {
	t.Helper()
	raw, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("thread response carries no items array: %v", out)
	}
	items := make([]core.Object, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("thread item is not an object: %T", v)
		}
		items = append(items, core.Object(m))
	}
	return items
}

// itemSeqs is the only ordering claim the response makes, in the form a case can compare.
func itemSeqs(t *testing.T, items []core.Object) []int64 {
	t.Helper()
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.N("seq"))
	}
	return out
}

// seedThreadEntry appends one entry at the next seq, which is how a case builds the Thread history it
// wants to read. This is the storage boundary, exactly like the other scene seeds: the writers that
// own these rows (StartSession, the takeover, the Thread POST) each have their own tests, and what is
// under test here is the reader's projection of whatever they left behind.
func (f *fixture) seedThreadEntry(runID, source, kind, record string, turnID, status any) int64 {
	f.t.Helper()
	seq := f.scalar(`SELECT COALESCE(MAX(seq),0)+1 FROM thread_entries WHERE run_id=$1`, runID)
	_, e := f.store.Pool.Exec(`
		INSERT INTO thread_entries(run_id, seq, source, kind, record, turn_id, status)
		VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, runID, seq, source, kind, record, turnID, status)
	must(f.t, e)
	return int64(seq)
}

// seedThreadHistory builds a Thread that carries every shape the reader must project: a system
// declaration, a user turn still queued, a Node record, a user turn the Node has echoed, and another
// Node record. It returns the seqs in order. The user turns carry the block shape the Thread POST
// writes, so the records under test are the records production produces.
func seedThreadHistory(t *testing.T, f *fixture, runID string) []int64 {
	t.Helper()
	return []int64{
		f.seedThreadEntry(runID, "system", "user_turn", `{"content":"Begin this task."}`, nil, nil),
		f.seedThreadEntry(runID, "user", "user_turn", threadRecord("please rename it"), uuid.NewString(), "queued"),
		f.seedThreadEntry(runID, "node", "message", `{"role":"assistant","content":"done"}`, nil, nil),
		f.seedThreadEntry(runID, "user", "user_turn", threadRecord("thanks"), uuid.NewString(), "delivered"),
		f.seedThreadEntry(runID, "node", "message", `{"role":"assistant","content":"welcome"}`, nil, nil),
	}
}

// threadRecord is Thread D3's user-turn record: the canonical block list the POST persists.
func threadRecord(text string) string {
	return mustJSONRaw(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
}

// moreIssue adds a second Issue to the scene's tenant: the cross-Issue case needs the tenant to be
// right and the Issue to be wrong.
func (f *fixture) moreIssue(scene threadScene) string {
	f.t.Helper()
	id := uuid.NewString()
	_, e := f.store.Pool.Exec(`INSERT INTO issues(id, tenant_id, creator_user_id, title, number) VALUES($1,$2,$3,'Other issue',2)`, id, scene.tenantID, scene.seed.userID)
	must(f.t, e)
	return id
}

// moreTenant creates a second tenant the fixture's user is an active member of, which is what the
// cross-tenant case needs: the caller must pass the membership check and then be refused by the
// Issue/run scope, so that a 404 proves the scope and not the membership. It deliberately creates no
// collaboration space, because the shared seed's space slug is globally unique.
func (f *fixture) moreTenant() string {
	f.t.Helper()
	id := uuid.NewString()
	// One transaction: an active tenant must have an active administrator at commit, so the tenant
	// and its membership cannot be inserted separately.
	tx, e := f.store.Pool.Begin()
	must(f.t, e)
	defer func() { _ = tx.Rollback() }()
	_, e = tx.Exec(`INSERT INTO tenants(id, name, status) VALUES($1,'Other tenant','active')`, id)
	must(f.t, e)
	_, e = tx.Exec(`INSERT INTO tenant_memberships(tenant_id, user_id, role, status) VALUES($1,$2,'admin','active')`, id, f.uid)
	must(f.t, e)
	must(f.t, tx.Commit())
	return id
}

// T4C-7 / T4C-10 (plan §4C.3) — the forward window and the snapshot fields. `after=N` is strictly
// `seq > N` and the window is ascending; `after=0` is from the start; no cursor at all is the tail,
// which for a Thread shorter than the limit is the same whole history. `threadState` comes from the
// run in the same transaction, a user turn carries its lifecycle and no other source does, and the
// Node's own identity never appears.
func TestThreadReadReturnsAnAscendingWindowAfterTheCursor(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	must(t, f.setThreadState(scene.runID, "running", "active"))
	seqs := seedThreadHistory(t, f, scene.runID)

	// Every read below asks for the same Thread; only the cursor changes.
	for _, tc := range []struct {
		name  string
		query string
		want  []int64
	}{
		{"no cursor reads the tail, which is the whole short Thread", "", seqs},
		{"after=0 reads from the start", "after=0", seqs},
		{"after=N is strictly greater than N", "after=2", []int64{3, 4, 5}},
		{"after at the end is empty", "after=5", []int64{}},
		{"after beyond the end is empty", "after=99", []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := f.getThread(scene, tc.query, 200)
			if got, want := itemSeqs(t, threadItems(t, out)), tc.want; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("want seqs %v got %v", want, got)
			}
			if out.S("threadState") != "active" {
				t.Fatalf("want threadState active got %q", out.S("threadState"))
			}
		})
	}

	items := threadItems(t, f.getThread(scene, "", 200))
	if len(items) != len(seqs) {
		t.Fatalf("want %d entries got %d", len(seqs), len(items))
	}
	for i, it := range items {
		if it.N("seq") != seqs[i] {
			t.Fatalf("entry %d: want seq %d got %d", i, seqs[i], it.N("seq"))
		}
		// The record is the verbatim line the writer stored, and the Node's identity — Cloud-internal
		// per D-022 — is not part of the public entry.
		for _, leaked := range []string{"nodeExecutionId", "nodeSequence", "runId"} {
			if _, ok := it[leaked]; ok {
				t.Fatalf("entry %v leaks internal field %q", it, leaked)
			}
		}
		source := it.S("source")
		_, hasTurn := it["turnId"]
		_, hasStatus := it["status"]
		switch source {
		case "user":
			if !hasTurn || !hasStatus {
				t.Fatalf("a user turn must carry turnId and status: %v", it)
			}
			if it.S("turnId") == "" {
				t.Fatalf("a user turn must carry its Cloud-generated turnId: %v", it)
			}
		default:
			if hasTurn || hasStatus {
				t.Fatalf("a %s entry must carry neither turnId nor status: %v", source, it)
			}
		}
	}
	// The user turns' lifecycle is read, not computed: the queued turn and the echoed one come back
	// with the values the durable rows hold.
	if got := items[1].S("status"); got != "queued" {
		t.Fatalf("want the second entry queued got %q", got)
	}
	if got := items[3].S("status"); got != "delivered" {
		t.Fatalf("want the fourth entry delivered got %q", got)
	}
	if got := messageText(t, items[1]); got != "please rename it" {
		t.Fatalf("entry content must be returned verbatim, got %q", got)
	}
}

// A Thread with no entries yet is a legal read, not a missing resource: the run exists, its session
// was declared, and the window is simply empty.
func TestThreadReadOfAThreadWithNoEntriesIsEmpty(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)

	out := f.getThread(scene, "", 200)
	if items := threadItems(t, out); len(items) != 0 {
		t.Fatalf("want an empty window got %v", items)
	}
	if out.S("threadState") != "pending" {
		t.Fatalf("want threadState pending got %q", out.S("threadState"))
	}
}

// T4C-9 (plan §4C.3) — the window and cursor contract at the boundary. `limit` defaults to 200 and is
// capped at D5's 500; a limit outside 1..500 is `invalid_pagination` and a cursor that is not a
// non-negative decimal integer is `invalid_cursor`. The two fault names are the existing list errors,
// reused so the Thread read adds no new vocabulary.
//
// The default is proven with more entries than the default rather than asserted from the code, and
// the 500 cap is proven by reading the same Thread with the largest legal window: an off-by-one in
// either bound shows up as a different count, not as a silently different page.
func TestThreadReadLimitAndCursorBounds(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	const entries = 250
	_, e := f.store.Pool.Exec(`
		INSERT INTO thread_entries(run_id, seq, source, kind, record)
		SELECT $1, n, 'node', 'message', '{"role":"assistant"}'::jsonb FROM generate_series(1,$2) n`, scene.runID, entries)
	must(t, e)

	// No cursor with more entries than the default is the tail window: the newest `limit` entries,
	// still ascending. The count is the default and the oldest seq returned is what the default
	// window's lower edge falls on, so a reader that silently clamped or started at 1 fails here.
	if items := threadItems(t, f.getThread(scene, "", 200)); len(items) != core.ThreadPageDefault {
		t.Fatalf("no limit must apply the %d default, got %d items", core.ThreadPageDefault, len(items))
	} else if got, want := items[0].N("seq"), int64(entries-core.ThreadPageDefault+1); got != want {
		t.Fatalf("the default window must be the tail: oldest seq = %d, want %d", got, want)
	}
	if items := threadItems(t, f.getThread(scene, "limit="+strconv.Itoa(core.ThreadPageLimit), 200)); len(items) != entries {
		t.Fatalf("limit=%d must return the whole Thread (%d entries), got %d", core.ThreadPageLimit, entries, len(items))
	}
	// A window smaller than the Thread walks it forward without gaps or repeats, which is what makes
	// `after = my highest seen seq` the client's whole paging algorithm. The tail cases are the same
	// bound read backwards: `limit=1` is the single newest entry, `limit=2` the newest two.
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"after=0&limit=1", []int64{1}},
		{"after=0&limit=2", []int64{1, 2}},
		{"limit=1", []int64{entries}},
		{"limit=2", []int64{entries - 1, entries}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			out := f.getThread(scene, tc.query, 200)
			if got := itemSeqs(t, threadItems(t, out)); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("%s: want %v got %v", tc.query, tc.want, got)
			}
		})
	}
	// The forward walk itself: read, advance to the highest seen seq, repeat.
	var seen []int64
	for cursor := 0; len(seen) < entries; {
		page := itemSeqs(t, threadItems(t, f.getThread(scene, fmt.Sprintf("after=%d&limit=7", cursor), 200)))
		if len(page) == 0 {
			t.Fatalf("forward walk stopped at after=%d after %d of %d entries", cursor, len(seen), entries)
		}
		seen = append(seen, page...)
		cursor = int(page[len(page)-1])
	}
	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("forward walk must be gapless and repeat-free: position %d is seq %d", i, seq)
		}
	}

	for _, tc := range []struct{ name, query, code string }{
		{"limit above the cap", "limit=501", "invalid_pagination"},
		{"limit zero", "limit=0", "invalid_pagination"},
		{"limit negative", "limit=-1", "invalid_pagination"},
		{"limit not a number", "limit=abc", "invalid_pagination"},
		{"cursor not a number", "after=abc", "invalid_cursor"},
		{"cursor negative", "after=-1", "invalid_cursor"},
		{"cursor fractional", "after=1.5", "invalid_cursor"},
		{"cursor not decimal", "after=0x2", "invalid_cursor"},
		{"backward cursor not a number", "before=abc", "invalid_cursor"},
		{"backward cursor negative", "before=-1", "invalid_cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := f.getThread(scene, tc.query, 400)
			if out.S("code") != tc.code {
				t.Fatalf("%s: want code %q got %v", tc.query, tc.code, out)
			}
		})
	}

	// An empty cursor is not a cursor: `after=` reads as absent, so the read falls back to the tail
	// window — the same three newest entries `limit=3` alone returns — and an unknown query parameter
	// is ignored exactly as it is on every other public list.
	if got := itemSeqs(t, threadItems(t, f.getThread(scene, "after=&limit=3", 200))); fmt.Sprint(got) != fmt.Sprint([]int64{entries - 2, entries - 1, entries}) {
		t.Fatalf("an empty cursor must fall back to the tail window, got %v", got)
	}
	if got := itemSeqs(t, threadItems(t, f.getThread(scene, "limit=3&unused=1", 200))); fmt.Sprint(got) != fmt.Sprint([]int64{entries - 2, entries - 1, entries}) {
		t.Fatalf("an unknown query parameter must be ignored, got %v", got)
	}
	if got := itemSeqs(t, threadItems(t, f.getThread(scene, "before=&limit=3", 200))); fmt.Sprint(got) != fmt.Sprint([]int64{entries - 2, entries - 1, entries}) {
		t.Fatalf("an empty backward cursor must fall back to the tail window, got %v", got)
	}
}

// T4C-9 (plan §4C.3, A3/G-017) — the backward window and the two-cursor conflict. `before=N` is the
// `limit` entries closest to N from below, returned ascending; it is the exact inverse of `after` at
// the same point, so a client that pages backwards and forwards over one Thread sees the same entries.
// Asking for both directions at once is a client error rather than a third reading, and it is reported
// with the pagination vocabulary's own code because neither value is wrong on its own.
//
// The window is selected by `seq` range and never by `OFFSET`, which is what the boundary cases pin:
// `before=1` and `before=0` are empty because nothing precedes seq 1, not because a page counter ran
// out, and `before=max+1` is the tail rather than a full page from the start.
func TestThreadReadBeforeCursorSelectsTheWindowBelowIt(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	const entries = 250
	_, e := f.store.Pool.Exec(`
		INSERT INTO thread_entries(run_id, seq, source, kind, record)
		SELECT $1, n, 'node', 'message', '{"role":"assistant"}'::jsonb FROM generate_series(1,$2) n`, scene.runID, entries)
	must(t, e)

	for _, tc := range []struct {
		name, query string
		want        []int64
	}{
		{"above the end is the tail", fmt.Sprintf("before=%d&limit=3", entries+1), []int64{entries - 2, entries - 1, entries}},
		{"at the end excludes the cursor's own seq", fmt.Sprintf("before=%d&limit=3", entries), []int64{entries - 3, entries - 2, entries - 1}},
		{"mid-Thread is the page below the cursor", "before=100&limit=3", []int64{97, 98, 99}},
		{"a single entry below the cursor", "before=2&limit=3", []int64{1}},
		{"nothing precedes seq 1", "before=1&limit=3", []int64{}},
		{"zero has no predecessor", "before=0&limit=3", []int64{}},
		{"a limit larger than the history is the whole history", "before=99&limit=500", seqRange(1, 98)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := f.getThread(scene, tc.query, 200)
			if got := itemSeqs(t, threadItems(t, out)); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("%s: want %v got %v", tc.query, tc.want, got)
			}
		})
	}

	// The two directions are inverses at one point: the page `before=N` returns is exactly the page a
	// forward reader arrives at by `after` on its lower edge, so neither direction can drift into its
	// own private ordering.
	back := itemSeqs(t, threadItems(t, f.getThread(scene, "before=200&limit=10", 200)))
	fwd := itemSeqs(t, threadItems(t, f.getThread(scene, fmt.Sprintf("after=%d&limit=10", back[0]-1), 200)))
	if fmt.Sprint(back) != fmt.Sprint(fwd) {
		t.Fatalf("the two directions must agree at one point: before=%v after=%v", back, fwd)
	}

	// Both cursors at once is the conflict, whatever the values, and it is refused before the window is
	// built: the two questions have different answers and the contract admits no third reading.
	for _, q := range []string{"after=1&before=250", "before=250&after=1", "after=0&before=0", "after=1&before=1&limit=3"} {
		out := f.getThread(scene, q, 400)
		if out.S("code") != "invalid_pagination" {
			t.Fatalf("%s: want invalid_pagination got %v", q, out)
		}
	}
	// A conflict is refused whether or not the individual cursors are legal, and an illegal cursor is
	// still reported as a cursor fault when it stands alone — the two codes answer two different
	// mistakes and neither masks the other.
	if out := f.getThread(scene, "after=abc&before=1", 400); out.S("code") != "invalid_pagination" {
		t.Fatalf("the conflict must be reported even with an illegal cursor, got %v", out)
	}
	if out := f.getThread(scene, "before=abc", 400); out.S("code") != "invalid_cursor" {
		t.Fatalf("a lone illegal backward cursor must be invalid_cursor, got %v", out)
	}
}

// seqRange is the ascending seq list from `from` to `to` inclusive, for the window assertions whose
// expected value is larger than a hand-written literal.
func seqRange(from, to int64) []int64 {
	out := make([]int64, 0, to-from+1)
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

// T4C-7 (plan §4C.3, A3/G-017) — the window's own descriptors. `idleSince` is the D4 instant the idle
// window is measured from and is null in every other state; `nextCursor` and `prevCursor` are the
// window's two ends, which is what lets a panel page in both directions without inventing a cursor of
// its own. An empty window has neither, rather than carrying the cursor the caller just sent: a cursor
// that names no entry would make the next page look non-empty to a client that only checks presence.
func TestThreadReadReportsIdleSinceAndWindowCursors(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	seedThreadHistory(t, f, scene.runID)

	must(t, f.setThreadState(scene.runID, "running", "active"))
	out := f.getThread(scene, "", 200)
	if got := out["idleSince"]; got != nil {
		t.Fatalf("an active Thread must report no idleSince, got %v", got)
	}
	items := threadItems(t, out)
	if got, want := out.N("prevCursor"), items[0].N("seq"); got != want {
		t.Fatalf("prevCursor = %d, want the window's first seq %d", got, want)
	}
	if got, want := out.N("nextCursor"), items[len(items)-1].N("seq"); got != want {
		t.Fatalf("nextCursor = %d, want the window's last seq %d", got, want)
	}

	// An idle Thread reports the instant the window started, read from the durable row rather than
	// recomputed, so a client can tell how long the agent has been quiet. The two are compared as
	// instants: the durable column is a `timestamptz` and the wire carries it in the API's own time
	// format, and neither spelling is the contract.
	must(t, f.setThreadState(scene.runID, "running", "idle"))
	var durable time.Time
	must(t, f.store.Pool.QueryRow(`SELECT idle_since FROM issue_runs WHERE id=$1`, scene.runID).Scan(&durable))
	reported, e := time.Parse(time.RFC3339, f.getThread(scene, "", 200).S("idleSince"))
	must(t, e)
	if !reported.Equal(durable) {
		t.Fatalf("idleSince = %s, want the durable instant %s", reported, durable)
	}

	// The cursors follow the window, not the Thread: a partial window's ends are its own.
	window := f.getThread(scene, "after=1&limit=2", 200)
	if got, want := window.N("prevCursor"), int64(2); got != want {
		t.Fatalf("prevCursor = %d, want the window's first seq %d", got, want)
	}
	if got, want := window.N("nextCursor"), int64(3); got != want {
		t.Fatalf("nextCursor = %d, want the window's last seq %d", got, want)
	}

	// An empty window carries no cursor at all, whichever direction produced it: a cursor naming no
	// entry would make the next page look non-empty to a client that only checks presence.
	for _, q := range []string{"after=99", "after=5", "before=1", "before=0"} {
		empty := f.getThread(scene, q, 200)
		if n := len(threadItems(t, empty)); n != 0 {
			t.Fatalf("%s: want an empty window got %d entries", q, n)
		}
		if empty["nextCursor"] != nil || empty["prevCursor"] != nil {
			t.Fatalf("%s: an empty window must carry neither cursor, got next=%v prev=%v", q, empty["nextCursor"], empty["prevCursor"])
		}
	}
}

// runResourceFields is the public IssueRun field set: every `issue_runs` column except the six the
// 0018 B-skeleton strips (plan §4C.3), plus the `revision` metadata projection Cloud Revision D5
// adds to every run read (null until a Revision is registered).
var runResourceFields = []string{
	"attempt", "completedAt", "createdAt", "delegatedFromRunId", "deletedAt", "dispatchedAt", "error",
	"executionContextRef", "executorId", "executorType", "externalExecutionId", "failureReason",
	"fireAt", "id", "input", "issueId", "leaseExpiresAt", "maxAttempts", "parentRunId", "queuedAt",
	"rerunOfRunId", "result", "retryOfRunId", "revision", "startedAt", "status", "tenantId", "triggerEvidenceKind",
	"triggerEvidenceRefId", "triggerSummary", "updatedAt", "version", "workflowInvocationRef",
}

// T4C-10 (plan §4C.3) — the Thread read exposes `threadState` and the run resource does not. The
// reader projects the column itself; the alternative shortcut (un-stripping it from the run resource)
// would be a separate contract change, so it is pinned: the run's field set is asserted literally,
// with a run whose `thread_state` and `idle_since` are both set, so a leak cannot hide behind nulls.
func TestThreadReadKeepsTheRunResourceShapeUnchanged(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	must(t, f.setThreadState(scene.runID, "running", "idle"))
	seedThreadHistory(t, f, scene.runID)

	if got := f.getThread(scene, "", 200).S("threadState"); got != "idle" {
		t.Fatalf("want threadState idle got %q", got)
	}

	status, run, e := f.threadRequest("GET", "/api/v1/tenants/"+scene.tenantID+"/issues/"+scene.issueID+"/runs/"+scene.runID, "", "")
	must(t, e)
	if status != 200 {
		t.Fatalf("GET run: want 200 got %d %v", status, run)
	}
	got := make([]string, 0, len(run))
	for k := range run {
		got = append(got, k)
	}
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(runResourceFields) {
		t.Fatalf("the run resource field set must not change with the Thread read\n got %v\nwant %v", got, runResourceFields)
	}
}

// T4C-11 (plan §4C.3) — authorization is the Issue read permission the comments path uses, and the
// scope is the triple tenant + Issue + live run. A caller who cannot see the Issue is refused the
// same way on both paths; a caller who can see the Issue but names a foreign, wrong-Issue, soft
// deleted or otherwise Thread-less run gets a plain not-found, so the read cannot be used to probe
// which runs exist.
func TestThreadReadAuthorizationMatchesComments(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	seedThreadHistory(t, f, scene.runID)
	otherTenant := f.moreTenant()

	// A verified user who is a member of neither tenant: the Thread read must answer exactly what the
	// comments read answers for the same caller, because both are the same membership check.
	stranger, _ := f.addUser(t, "stranger", "Stranger")
	for _, tc := range []struct{ name, path string }{
		{"thread", threadReadPath(scene.tenantID, scene.issueID, scene.runID)},
		{"comments", "/api/v1/tenants/" + scene.tenantID + "/issues/" + scene.issueID + "/comments"},
	} {
		t.Run("non-member "+tc.name, func(t *testing.T) {
			status, out, e := f.threadRequestAs(stranger, "GET", tc.path, "", "")
			must(t, e)
			if status != 403 || out.S("code") != "membership_required" {
				t.Fatalf("%s: want 403 membership_required got %d %v", tc.name, status, out)
			}
		})
	}

	memberStatus, memberOut, e := f.threadRequest("GET", "/api/v1/tenants/"+scene.tenantID+"/issues/"+scene.issueID+"/comments", "", "")
	must(t, e)
	if memberStatus != 200 {
		t.Fatalf("the member must still read the Issue's comments: got %d %v", memberStatus, memberOut)
	}

	unknown := uuid.NewString()
	cases := []struct{ name, tenant, issue, run string }{
		{"cross tenant", otherTenant, scene.issueID, scene.runID},
		{"cross issue", scene.tenantID, f.moreIssue(scene), scene.runID},
		{"unknown run", scene.tenantID, scene.issueID, unknown},
		{"run id is not an id", scene.tenantID, scene.issueID, "not-a-uuid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, out, e := f.threadRequest("GET", threadReadPath(tc.tenant, tc.issue, tc.run), "", "")
			must(t, e)
			if status != 404 || out.S("code") != "not_found" {
				t.Fatalf("%s: want 404 not_found got %d %v", tc.name, status, out)
			}
		})
	}

	t.Run("soft deleted run", func(t *testing.T) {
		_, e := f.store.Pool.Exec(`UPDATE issue_runs SET deleted_at=now() WHERE id=$1`, scene.runID)
		must(t, e)
		// A soft-deleted run is still readable as a resource row, so the not-found is the read's own
		// scope check and not a missing parent.
		if n := f.scalar(`SELECT count(*) FROM issue_runs WHERE id=$1`, scene.runID); n != 1 {
			t.Fatalf("the seeded run must still exist, got %d rows", n)
		}
		status, out, e := f.threadRequest("GET", threadReadPath(scene.tenantID, scene.issueID, scene.runID), "", "")
		must(t, e)
		if status != 404 || out.S("code") != "not_found" {
			t.Fatalf("soft deleted run: want 404 not_found got %d %v", status, out)
		}
	})

	t.Run("a run with no declared session has no Thread", func(t *testing.T) {
		// D-4C-01 materializes `pending` inside StartSession; before that there is genuinely no
		// Thread to read, and an empty `threadState` would put a value outside D4's closed set on the
		// wire. The answer is the same not-found a missing Thread gets (G-017).
		runID := seedDeclaredAgentRun(t, f.store.Pool, scene.seed, "provisioning", "queued", false, false)
		status, out, e := f.threadRequest("GET", threadReadPath(scene.tenantID, scene.issueID, runID), "", "")
		must(t, e)
		if status != 404 || out.S("code") != "not_found" {
			t.Fatalf("undeclared session: want 404 not_found got %d %v", status, out)
		}
	})

	t.Run("a human run has no Thread", func(t *testing.T) {
		// IssueRun D3: phase, the run Workspace and the Thread are agent-only columns
		// (issue_runs_agent_columns), so a team run can never carry a Thread to read.
		runID := uuid.NewString()
		_, e := f.store.Pool.Exec(`INSERT INTO issue_runs(id, tenant_id, issue_id, executor_type, executor_id) VALUES($1,$2,$3,'team',$4)`, runID, scene.tenantID, scene.issueID, uuid.NewString())
		must(t, e)
		status, out, e := f.threadRequest("GET", threadReadPath(scene.tenantID, scene.issueID, runID), "", "")
		must(t, e)
		if status != 404 || out.S("code") != "not_found" {
			t.Fatalf("non-agent run: want 404 not_found got %d %v", status, out)
		}
	})
}

// T4C-12 (plan §4C.3) — a walk that interleaves with real committed appends never skips and never
// repeats. The reader has exactly one tool (after = its highest seen seq, which is why the cursor
// must stay a raw `seq` rather than becoming opaque), and the writers are the production Thread POST.
// The reader stops only when it has read the whole Thread, so a hole in its walk is a failure rather
// than a short read, and the final full window proves the durable outcome is gapless. Once the
// writers have stopped, the same log is walked backwards (`before`) and must reach the identical set:
// both directions are `seq`-range queries over one immutable history, so neither can depend on when
// it ran.
func TestThreadReadPagingIsGapFreeUnderConcurrentAppend(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	f.seedThreadEntry(scene.runID, "system", "user_turn", `{"content":"Begin this task."}`, nil, nil)

	const writers, perWriter, limit = 3, 4, 2
	total := int64(1 + writers*perWriter)

	start := make(chan struct{})
	appended := make(chan struct{}, writers*perWriter)
	statuses := make([][]int, writers)
	errs := make([][]error, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		statuses[w], errs[w] = make([]int, perWriter), make([]error, perWriter)
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for n := 0; n < perWriter; n++ {
				statuses[w][n], _, errs[w][n] = f.threadRequest("POST", threadMessagesPath(scene.tenantID, scene.issueID, scene.runID), threadBody(fmt.Sprintf("turn %d-%d", w, n)), fmt.Sprintf("key-%d-%d", w, n))
				appended <- struct{}{}
			}
		}(w)
	}
	writersDone := make(chan struct{})
	go func() { wg.Wait(); close(writersDone) }()
	close(start)

	var seen []int64
	for cursor := 0; int64(len(seen)) < total; {
		out := f.getThread(scene, fmt.Sprintf("after=%d&limit=%d", cursor, limit), 200)
		page := itemSeqs(t, threadItems(t, out))
		if len(page) == 0 {
			select {
			case <-writersDone:
				// Every append has returned and committed, so a walk that still sees nothing has a
				// hole rather than a lag. Re-read once to be certain before failing.
				again := itemSeqs(t, threadItems(t, f.getThread(scene, fmt.Sprintf("after=%d&limit=%d", cursor, limit), 200)))
				if len(again) == 0 {
					t.Fatalf("forward walk stopped after seq %d with %d of %d entries read", cursor, len(seen), total)
				}
				page = again
			case <-appended:
				continue
			}
		}
		for i := 1; i < len(page); i++ {
			if page[i] != page[i-1]+1 {
				t.Fatalf("a page must be ascending and contiguous: %v", page)
			}
		}
		if page[0] <= int64(cursor) {
			t.Fatalf("after=%d returned seq %d, which is not strictly greater", cursor, page[0])
		}
		seen = append(seen, page...)
		cursor = int(page[len(page)-1])
	}

	for i, seq := range seen {
		if seq != int64(i+1) {
			t.Fatalf("the interleaved walk must be gapless and repeat-free: position %d is seq %d", i, seq)
		}
	}
	for w := range statuses {
		for n := range statuses[w] {
			if errs[w][n] != nil || statuses[w][n] != 201 {
				t.Fatalf("writer %d turn %d: want 201 got %d %v", w, n, statuses[w][n], errs[w][n])
			}
		}
	}
	full := itemSeqs(t, threadItems(t, f.getThread(scene, "after=0&limit=500", 200)))
	if int64(len(full)) != total {
		t.Fatalf("the committed Thread must hold %d entries, got %d", total, len(full))
	}
	for i, seq := range full {
		if seq != int64(i+1) {
			t.Fatalf("the committed Thread must be gapless: position %d is seq %d", i, seq)
		}
	}

	// The same log walked backwards once every writer has stopped. `before` must reach the identical
	// set — the two directions are two questions about one `seq` range, and a backward page that
	// dropped or repeated a row would show up as a different multiset rather than as an error. The
	// walk also pins the property that makes `seq` a cursor instead of an offset: a historical page is
	// selected from the immutable history below the cursor, so the entries a writer appended while the
	// forward walk was running never appear in a page behind it.
	var back []int64
	for cursor := total + 1; int64(len(back)) < total; {
		page := itemSeqs(t, threadItems(t, f.getThread(scene, fmt.Sprintf("before=%d&limit=%d", cursor, limit), 200)))
		if len(page) == 0 {
			t.Fatalf("backward walk stopped at before=%d after %d of %d entries", cursor, len(back), total)
		}
		for i := 1; i < len(page); i++ {
			if page[i] != page[i-1]+1 {
				t.Fatalf("a backward page must be ascending and contiguous: %v", page)
			}
		}
		if last := page[len(page)-1]; last >= cursor {
			t.Fatalf("before=%d returned seq %d, which is not strictly less", cursor, last)
		}
		back = append(page, back...)
		cursor = page[0]
	}
	if fmt.Sprint(back) != fmt.Sprint(full) {
		t.Fatalf("the two directions must reach the same log\nforward  %v\nbackward %v", full, back)
	}
}

// The Thread read is read-only (plan §4R.5: "单次 Store.transact 只读，无写"). The evidence is the
// durable state itself: the entries, the run's Thread state, its row version and `updated_at`, the
// command backlog, the receipt count, the idempotency records and the Issue's activity all have to be
// exactly what they were, because a read that allocated a seq or bumped a version would show up here
// even though it could never show up in the response.
func TestThreadReadHasNoBusinessWrites(t *testing.T) {
	f := setup(t)
	scene := seedThreadScene(t, f)
	must(t, f.setThreadState(scene.runID, "running", "active"))
	seedThreadHistory(t, f, scene.runID)

	type runRow struct {
		threadState string
		idleSince   *time.Time
		version     int64
		updatedAt   time.Time
	}
	readRun := func() runRow {
		var r runRow
		must(t, f.store.Pool.QueryRow(`SELECT thread_state, idle_since, version, updated_at FROM issue_runs WHERE id=$1`, scene.runID).Scan(&r.threadState, &r.idleSince, &r.version, &r.updatedAt))
		return r
	}
	fingerprint := func() string {
		return fmt.Sprintf("entries=%d commands=%d receipts=%d idempotency=%d activities=%d run=%+v",
			f.scalar(`SELECT count(*) FROM thread_entries WHERE run_id=$1`, scene.runID),
			f.scalar(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, scene.runID),
			f.scalar(`SELECT count(*) FROM node_event_receipts`),
			f.scalar(`SELECT count(*) FROM idempotency_records`),
			f.scalar(`SELECT count(*) FROM issue_activities WHERE issue_id=$1`, scene.issueID),
			readRun())
	}

	before := fingerprint()
	// Several reads, including the ones that walk the Thread, the ones that page backwards, and the
	// ones that are refused: a read that moved anything would show up in the fingerprint regardless of
	// its status code.
	f.getThread(scene, "", 200)
	f.getThread(scene, "after=2&limit=1", 200)
	f.getThread(scene, "before=4&limit=2", 200)
	f.getThread(scene, "after=99", 200)
	f.getThread(scene, "after=1&before=4", 400)
	f.getThread(scene, "limit=501", 400)
	f.getThread(scene, "after=nope", 400)
	f.getThread(scene, "before=nope", 400)
	if after := fingerprint(); after != before {
		t.Fatalf("the Thread read must write nothing\nbefore %s\nafter  %s", before, after)
	}
}
