package core

import "strconv"

// Thread read — the public GET .../runs/{rid}/thread (Thread D5, plan §4C.3). This is the read side
// of the Thread and the authoritative source the panel is rebuilt from.
//
// The full approved read surface (Thread D5, A3/G-017): `after={seq}` and `before={seq}` are mutually
// exclusive decimal `seq` cursors, neither means the tail window, `limit` defaults to 200 and caps at
// 500, every response is ascending by `seq` regardless of direction, and the payload carries
// `threadState`, `idleSince` and the window's `nextCursor`/`prevCursor`.
//
// Cursors are Cloud Thread `seq` values and nothing else: not Node sequences, not SSE event ids, not
// `node_execution_id`. There is no cross-request snapshot guarantee — entries are append-only, so a
// client that re-reads with `after` at its highest seen `seq` neither misses nor repeats one.
//
// Ownership (D6 invariant 7): this file reads only B tables (`thread_entries`, `issue_runs`) and
// writes nothing at all — no entry, no state, no command, no running authority. The whole read is one
// `Store.transact`, so `items` and `threadState` come from a single database snapshot rather than from
// two reads that could straddle a concurrent append.

// ThreadPageLimit is the largest window one Thread read may return. It is Thread D5's own upper bound
// and is exported because three layers must agree on it: the core reader, the transport bound the
// router enforces before dispatch, and the published OpenAPI parameter.
const ThreadPageLimit = 500

// ThreadPageDefault is the window used when the caller sends no `limit`. D5 names the upper bound and
// not the default; 200 is D-4C-02's choice, and it is applied here rather than in the router so the
// core stays the one authority for the read's own contract.
const ThreadPageDefault = 200

// threadRead answers one Thread read on the caller's transaction.
//
// Authorization is the Issue read path's own (Thread D3/D5: "same as comments"): the caller is an
// active tenant member — Public checked membership before dispatching here — and this resolves the
// Issue inside the caller's tenant, so a wrong tenant or Issue is a plain not-found. The run is then
// re-read under the same triple scope, exactly like the Thread POST and `run()`.
func threadRead(t *transaction, r *PublicRequest) Object {
	i := issue(t, r.TenantID, r.IssueID)

	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, i.S("id"))
	// A human run has no Thread: `thread_entries` and `thread_state` belong to agent runs only, and
	// the Thread POST refuses a non-agent run the same way. Answering with an empty Thread would
	// invent a resource rather than report that this run has none.
	require(run != nil && run.S("executorType") == "agent", 404, "not_found")

	// `thread_state` is NULL for a run whose session was never declared: D-4C-01 materializes
	// `pending` inside StartSession, and nothing else writes the column before that. There is
	// genuinely no Thread to read yet, so this is the same not-found a run outside the caller's scope
	// gets — never an empty `threadState`, which would put a value outside D4's closed set
	// (pending|active|idle|ending|ended) on the wire. Which answer a not-yet-materialized Thread
	// should give is part of the unapproved read surface, so it stays registered under G-017.
	state := run.S("threadState")
	require(state != "", 404, "not_found")

	// The two cursors are different questions about the same log — "what comes after this point" and
	// "what comes before it" — so sending both is a client error rather than a third reading. It is
	// the pagination vocabulary's own fault: nothing is wrong with either value on its own.
	require(r.After == "" || r.Before == "", 400, "invalid_pagination")

	limit := r.Limit
	if limit == 0 {
		limit = ThreadPageDefault
	}
	// The transport already refused an out-of-range `limit`, but the bound belongs to this contract
	// and must hold for every caller of the reader, not only the ones that arrive over HTTP.
	require(limit > 0 && limit <= ThreadPageLimit, 400, "invalid_pagination")

	// Every window is a `seq` range query, never an `OFFSET`: `seq` is part of `(run_id, seq)`'s
	// primary key, so it is already a total order and needs no tiebreaker, and an offset would skip
	// or repeat rows as soon as an entry is appended between two reads. Each branch selects by `seq`
	// and the descending branches are re-sorted outward, so the HTTP response is ascending in all
	// three cases and a client needs no case analysis to consume it.
	var rows []Object
	switch {
	case r.After != "":
		// `after=N` is the forward cursor: everything strictly after N, oldest first.
		rows = t.list(`
			SELECT seq, source, kind, record, turn_id, status, created_at
			FROM thread_entries WHERE run_id=$1 AND seq>$2 ORDER BY seq LIMIT $3`,
			run.S("id"), threadCursor(r.After), limit)
	case r.Before != "":
		// `before=N` is the backward cursor: the `limit` entries closest to N from below, i.e. the
		// last page an ascending reader would see before reaching N. The inner query walks backwards
		// to take that page; the outer one restores the response's ascending order.
		rows = t.list(`
			SELECT * FROM (
				SELECT seq, source, kind, record, turn_id, status, created_at
				FROM thread_entries WHERE run_id=$1 AND seq<$2 ORDER BY seq DESC LIMIT $3
			) w ORDER BY seq`,
			run.S("id"), threadCursor(r.Before), limit)
	default:
		// No cursor is the tail: the newest `limit` entries, which is what a panel opening on a live
		// conversation wants. It is the only default that does not make the caller guess how long the
		// Thread has grown.
		rows = t.list(`
			SELECT * FROM (
				SELECT seq, source, kind, record, turn_id, status, created_at
				FROM thread_entries WHERE run_id=$1 ORDER BY seq DESC LIMIT $2
			) w ORDER BY seq`,
			run.S("id"), limit)
	}
	items := make([]Object, 0, len(rows))
	for _, row := range rows {
		items = append(items, threadEntryView(row))
	}
	out := Object{
		"items":       items,
		"threadState": state,
		// `idleSince` is the D4 instant the idle window is measured from, and it is null for every
		// state but `idle` — the writers clear it with the state. It is reported here rather than on
		// the run resource because it is Thread lifecycle, like `threadState`.
		"idleSince": run["idleSince"],
	}
	if state == "ended" && run.S("failureReason") != "" {
		out["failureCode"] = publicSessionFailureCode(run.S("failureReason"))
	}
	uid := identityWithAlias(t, r.Identity).S("id")
	for key, value := range threadModelPermissions(t, run, uid) {
		out[key] = value
	}
	// The window's two ends, so a panel can page in both directions without inventing a cursor: feed
	// `nextCursor` back as `after` and `prevCursor` back as `before`. An empty window has neither.
	if len(items) > 0 {
		out["nextCursor"] = items[len(items)-1].N("seq")
		out["prevCursor"] = items[0].N("seq")
	} else {
		out["nextCursor"] = nil
		out["prevCursor"] = nil
	}
	return out
}

// threadCursor decodes one Thread read cursor. The vocabulary is decimal `seq`, not the UUID
// `page`/`window` use, so this reader must not reuse their helpers: `validID` would reject every
// legal Thread cursor, and relaxing `window`'s UUID check would silently change every other list's
// contract (plan §4C.3). The fault *names* are the shared ones so the error vocabulary stays uniform.
func threadCursor(v string) int64 {
	n, e := strconv.ParseInt(v, 10, 64)
	if e != nil || n < 0 {
		reject(400, "invalid_cursor")
	}
	return n
}

// threadEntryView projects one stored `thread_entries` row onto the public ThreadEntry contract
// (Thread D1/D3). It is what keeps the entry's Node-owned columns (`node_execution_id`,
// `node_sequence`) and its internal `run_id` off the wire: D-022's identity is Cloud-internal.
//
// `turnId` and `status` describe the user-turn lifecycle and exist only for a user turn — 0022's
// `(source = 'user') = (status IS NOT NULL)` CHECK makes that exactly `source = 'user'` — so a node
// or system entry carries neither field rather than carrying a null (plan §4C.3).
func threadEntryView(row Object) Object {
	out := Object{
		"seq":       row["seq"],
		"source":    row["source"],
		"kind":      row["kind"],
		"record":    row["record"],
		"createdAt": row["createdAt"],
	}
	if row.S("source") == "user" {
		out["turnId"] = row["turnId"]
		out["status"] = row["status"]
	}
	return out
}
