package core

import "strings"

// nextTimelineSeq allocates the next per-issue timeline position, shared by issue_comments and
// issue_activities so a single issue never reuses a seq across the two tables.
//
// Implementation: GREATEST(COALESCE(MAX(comments.seq),0), COALESCE(MAX(activities.seq),0)) + 1,
// evaluated inside the advisory-locked transaction — the same race-free-under-the-lock pattern as the
// issue `number` counter.
//
// Concurrency note: the read-then-increment is safe only while every transaction that appends holds
// a lock that orders writers of the same Issue. Ordinary transactions hold the exclusive
// control-plane lock, and every WorkflowRun transaction holds the shared control-plane lock (the two
// exclude each other) plus the Issue lock taken by lockWorkflowRunIssue once it holds its run lock.
// Remove either guarantee and this allocator must first move to a per-issue counter/sequence column
// (atomic UPDATE ... RETURNING); MAX-read-then-increment is not safe under concurrent writes.
func nextTimelineSeq(t *transaction, iid string) int64 {
	o := t.one(`SELECT GREATEST(
		COALESCE((SELECT max(seq) FROM issue_comments WHERE issue_id=$1), 0),
		COALESCE((SELECT max(seq) FROM issue_activities WHERE issue_id=$1), 0)) AS seq`, iid)
	return o.N("seq") + 1
}

// appendActivity records one append-only Timeline entry with the next shared per-issue seq. It is an
// internal projection helper — activities have no public update/delete API and no business state is
// rebuilt from them.
func appendActivity(t *transaction, tid, iid, actorType string, actorID any, action string, details Object) Object {
	require(actorType == "user" || actorType == "agent" || actorType == "team" || actorType == "system", 400, "invalid_actor")
	action = strings.TrimSpace(action)
	require(action != "" && len(action) <= 200, 400, "invalid_input")
	id := newID()
	seq := nextTimelineSeq(t, iid)
	t.exec("INSERT INTO issue_activities(id,tenant_id,issue_id,seq,actor_type,actor_id,action,details) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", id, tid, iid, seq, actorType, actorID, action, jsonText(details))
	return t.one("SELECT * FROM issue_activities WHERE id=$1", id)
}
