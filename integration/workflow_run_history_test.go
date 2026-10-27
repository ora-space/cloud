package integration

import (
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/wanglongan587/cloud/internal/core"
)

// seededRun is one run row written straight into the store: the id the read must return and the
// instant the walk must order it by.
type seededRun struct {
	id        string
	createdAt time.Time
}

// seedRun writes a terminal run row at a chosen instant into the fixture tenant's workflow. The run
// list is a read path, so the acceptance below goes through real HTTP; only the history itself is
// arranged directly, because a hundred real executions would add runtime without adding evidence
// about the walk.
func seedRun(t *testing.T, f *fixture, wid, snapID string, run seededRun, name string) {
	t.Helper()
	seedRunFor(t, f, f.tid, wid, snapID, run, name)
}

// seedRunFor is seedRun for an explicit tenant, so a case can arrange a run the caller may reach
// through a second tenant membership without that run belonging to the history under test.
func seedRunFor(t *testing.T, f *fixture, tid, wid, snapID string, run seededRun, name string) {
	t.Helper()
	_, err := f.store.Pool.ExecContext(t.Context(),
		`INSERT INTO workflow_runs(id,tenant_id,workflow_id,snapshot_id,name,status,created_at,updated_at,started_at,finished_at)
		 VALUES($1,$2,$3,$4,$5,'succeeded',$6,$6,$6,$6)`,
		run.id, tid, wid, snapID, name, run.createdAt)
	must(t, err)
}

// purgeRun soft-deletes a seeded run the way the reclaim path does: the row and its id stay, so only
// the query's own deleted filter can keep the id from resolving as a cursor anchor.
func purgeRun(t *testing.T, f *fixture, runID string) {
	t.Helper()
	_, err := f.store.Pool.ExecContext(t.Context(), `UPDATE workflow_runs SET deleted_at=now() WHERE id=$1`, runID)
	must(t, err)
}

// TestWorkflowRunHistoryWalksNewestFirst pins the run list's order and its cursor.
//
// The list is a paged history, so two things have to hold at once: the newest run comes first, and
// paging reaches every run exactly once. Row ids are random UUIDs, which is why ordering by id
// (what the list used to do) cannot satisfy either — this seeds ids that are deliberately unrelated
// to time, with several runs sharing one instant so the (created_at, id) tiebreaker is exercised.
func TestWorkflowRunHistoryWalksNewestFirst(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	wf := f.call("POST", f.path("/workflows"), core.Object{"name": "History", "graph": runGraph()}, "hist-wf", 200).O("resource")
	wfid := wf.S("id")
	snapID := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "hist-pub", 200).O("resource").S("id")

	const total = 120
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seeded := make([]seededRun, 0, total)
	for i := range total {
		// Two runs per instant: created_at alone is not a total order, so the id must break the tie.
		run := seededRun{id: uuid.NewString(), createdAt: base.Add(time.Duration(i/2) * time.Minute)}
		seedRun(t, f, wfid, snapID, run, "run "+run.id[:8])
		seeded = append(seeded, run)
	}
	// The order the read must produce, computed independently of the endpoint.
	want := slices.Clone(seeded)
	sort.Slice(want, func(i, j int) bool {
		if !want[i].createdAt.Equal(want[j].createdAt) {
			return want[i].createdAt.After(want[j].createdAt)
		}
		return want[i].id > want[j].id
	})

	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		require(t, pages < total, "the walk did not terminate")
		query := "?limit=7"
		if cursor != "" {
			query += "&after=" + cursor
		}
		page := f.call("GET", f.path("/workflows/"+wfid+"/runs")+query, nil, "", 200)
		items := issueItems(page)
		require(t, len(items) > 0, "a page in the middle of the walk must not be empty")
		for _, item := range items {
			got = append(got, item.S("id"))
		}
		if page.S("nextCursor") == "" {
			break
		}
		// The cursor is the page's own last row, so the next request continues strictly below it.
		require(t, page.S("nextCursor") == items[len(items)-1].S("id"), "the cursor must be the last row of the page")
		cursor = page.S("nextCursor")
	}

	gotIDs := make([]string, 0, len(want))
	for _, run := range want {
		gotIDs = append(gotIDs, run.id)
	}
	if !slices.Equal(got, gotIDs) {
		t.Fatalf("the walk returned %d rows in the wrong order or with repeats; want %d newest first", len(got), total)
	}

	// The newest run is on the first page, which is the whole point of ordering server-side: the
	// page the member sees first has to be the one they opened the history for.
	newest := f.call("GET", f.path("/workflows/"+wfid+"/runs")+"?limit=1", nil, "", 200)
	if first := issueItems(newest)[0].S("id"); first != want[0].id {
		t.Fatalf("the first page starts at %s; want the newest run %s", first, want[0].id)
	}

	// A cursor that resolves to no row of this workflow ends the walk with an empty page instead of
	// repeating the history from the top.
	stranger := f.call("GET", f.path("/workflows/"+wfid+"/runs")+"?after="+uuid.NewString(), nil, "", 200)
	if len(issueItems(stranger)) != 0 || stranger.S("nextCursor") != "" {
		t.Fatalf("a cursor outside the workflow must end the walk, got %v", stranger)
	}
	// A cursor that is not a row id at all is refused, not silently treated as the start.
	f.call("GET", f.path("/workflows/"+wfid+"/runs")+"?after=not-a-cursor", nil, "", 400)
}

// TestWorkflowRunHistoryOnlyAnchorsInItsOwnHistory pins which rows a cursor may resolve against.
//
// The cursor is an opaque run UUID, so the endpoint resolves the page boundary from that row's
// created_at. That lookup has to stay inside the history being read: an id naming another workflow's
// run, another tenant's run, or a purged run would otherwise position the page at that row's
// creation instant — a creation-time side channel across tenants, and a way to page a history the
// caller's own rows cannot describe. Each of those cursors ends the walk with an empty page, the
// same answer an unknown cursor gets, while a cursor of this workflow still continues below itself.
func TestWorkflowRunHistoryOnlyAnchorsInItsOwnHistory(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	wfid := createWorkflow(t, f, "Scoped history", "hist-anchor-wf", nil)
	snapID := publishWorkflow(t, f, wfid, "hist-anchor-pub").S("id")
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	older := seededRun{id: uuid.NewString(), createdAt: base}
	newer := seededRun{id: uuid.NewString(), createdAt: base.Add(time.Hour)}
	seedRun(t, f, wfid, snapID, older, "older run")
	seedRun(t, f, wfid, snapID, newer, "newer run")

	// A run of another workflow in the same tenant, at an instant strictly inside this history:
	// resolved without a workflow filter it would anchor between the two runs above and answer the
	// older one; scoped, it anchors nothing.
	otherWid := createWorkflow(t, f, "Other history", "hist-anchor-wf-2", nil)
	otherSnap := publishWorkflow(t, f, otherWid, "hist-anchor-pub-2").S("id")
	otherRun := seededRun{id: uuid.NewString(), createdAt: base.Add(30 * time.Minute)}
	seedRun(t, f, otherWid, otherSnap, otherRun, "other workflow run")

	// A run of a workflow in a second tenant the caller administers: the membership check passes, so
	// only the cursor's own scope can refuse it.
	foreign := f.moreTenant()
	foreignWid := f.call("POST", "/api/v1/tenants/"+foreign+"/workflows", core.Object{
		"name": "Foreign history", "graph": startInputGraph(nil),
	}, "hist-anchor-foreign-wf", 200).O("resource").S("id")
	foreignSnap := f.call("POST", "/api/v1/tenants/"+foreign+"/workflows/"+foreignWid+"/publish", core.Object{}, "hist-anchor-foreign-pub", 200).O("resource").S("id")
	foreignRun := seededRun{id: uuid.NewString(), createdAt: base.Add(45 * time.Minute)}
	seedRunFor(t, f, foreign, foreignWid, foreignSnap, foreignRun, "foreign tenant run")

	// A purged run of this workflow: the row keeps its id, so only the deleted filter can keep it
	// from anchoring a history it is no longer part of.
	purged := seededRun{id: uuid.NewString(), createdAt: base.Add(50 * time.Minute)}
	seedRun(t, f, wfid, snapID, purged, "purged run")
	purgeRun(t, f, purged.id)

	for _, c := range []struct{ name, cursor string }{
		{"another workflow's run", otherRun.id},
		{"another tenant's run", foreignRun.id},
		{"a purged run of this workflow", purged.id},
		{"an unknown run", uuid.NewString()},
	} {
		got := f.call("GET", f.path("/workflows/"+wfid+"/runs")+"?after="+c.cursor, nil, "", 200)
		if items := issueItems(got); len(items) != 0 || got.S("nextCursor") != "" {
			t.Fatalf("%s anchored the history (%d rows): %v", c.name, len(items), got)
		}
	}

	// The same walk still accepts a cursor of this workflow: the page continues strictly below the
	// row it names, so scoping the anchor did not turn valid cursors into empty pages.
	got := issueItems(f.call("GET", f.path("/workflows/"+wfid+"/runs")+"?after="+newer.id, nil, "", 200))
	if len(got) != 1 || got[0].S("id") != older.id {
		t.Fatalf("a cursor of this workflow returned %v; want only the older run %s", got, older.id)
	}
}

// TestWorkflowRowsCarryTheirNewestRun proves a workflow read labels when it last ran.
//
// The label answers "did this workflow work last time" from the list, so it has to be the same run
// the history would show first — not one run on the list and another on the detail page.
func TestWorkflowRowsCarryTheirNewestRun(t *testing.T) {
	f := setup(t)
	seedPersonalModel(t, f)
	never := f.call("POST", f.path("/workflows"), core.Object{"name": "Never ran", "graph": runGraph()}, "lbl-wf-0", 200).O("resource")
	wf := f.call("POST", f.path("/workflows"), core.Object{"name": "Ran twice", "graph": runGraph()}, "lbl-wf", 200).O("resource")
	wfid := wf.S("id")
	snapID := f.call("POST", f.path("/workflows/"+wfid+"/publish"), core.Object{}, "lbl-pub", 200).O("resource").S("id")

	older := seededRun{id: uuid.NewString(), createdAt: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)}
	newer := seededRun{id: uuid.NewString(), createdAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
	// Written newer first so a "last inserted wins" implementation would answer with the older one.
	seedRun(t, f, wfid, snapID, newer, "newer run")
	seedRun(t, f, wfid, snapID, older, "older run")

	rows := issueItems(f.call("GET", f.path("/workflows")+"?limit=100", nil, "", 200))
	byName := map[string]core.Object{}
	for _, row := range rows {
		byName[row.S("name")] = row
	}
	ran, ok := byName["Ran twice"]
	require(t, ok, "the list must contain the workflow that ran")
	latest, ok := ran["latestRun"].(map[string]any)
	require(t, ok, "a workflow that ran must carry its newest run")
	if got := core.Object(latest); got.S("id") != newer.id || got.S("status") != "succeeded" || got.S("snapshotId") != snapID {
		t.Fatalf("latestRun = %v; want the newer run %s", got, newer.id)
	}
	// The label belongs to the list, not to the resource: a run starting must not rewrite every read
	// of a workflow, so the detail read keeps projecting the row alone.
	if detail := f.call("GET", f.path("/workflows/"+wfid), nil, "", 200); detail["latestRun"] != nil {
		t.Fatalf("the detail read must not carry latestRun, got %v", detail["latestRun"])
	}
	// Never having run is null on the list row rather than an absent field or a made-up run.
	empty, ok := byName["Never ran"]
	require(t, ok, "the list must contain the workflow that never ran")
	if _, ok := empty["latestRun"]; !ok {
		t.Fatal("a workflow that never ran must still carry latestRun, as null")
	}
	if empty["latestRun"] != nil {
		t.Fatalf("a workflow that never ran reports %v; want null", empty["latestRun"])
	}
	// The create response is the resource, so it carries no label at all — the same split the detail
	// read above pins.
	if never["latestRun"] != nil {
		t.Fatalf("a create response must not carry latestRun, got %v", never["latestRun"])
	}
}

// require fails the test with msg unless ok, keeping the assertions above readable.
func require(t *testing.T, ok bool, msg string) {
	t.Helper()
	if !ok {
		t.Fatal(msg)
	}
}
