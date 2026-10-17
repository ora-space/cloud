package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// Controller-integration D7, invariant 11: the session input carries the Project's Workspaces newest
// first, at most 100. Run Workspaces stay listed after deletion, with their Issue and run, while a
// deleted isolated Workspace and another Project's Workspaces are not listed.
func TestSessionWorkspaceDirectoryListsTheProjectNewestFirst(t *testing.T) {
	s := commandStore(t)
	scene := seedStartingRun(t, s, sessionStartInput())
	seed := scene.seed
	// Another tenant's Project in the same database, whose Workspaces must never be listed.
	seedDispatchScene(t, s.Pool, false)
	var user, main string
	if err := s.Pool.QueryRow(`SELECT owner_user_id,id FROM workspaces WHERE project_id=$1 AND kind='main'`, seed.project).Scan(&user, &main); err != nil {
		t.Fatal(err)
	}
	isolated, deletedIsolated, deletedRun := uuid.NewString(), uuid.NewString(), uuid.NewString()
	earlierIssue, earlierRun := uuid.NewString(), uuid.NewString()
	tx, err := s.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Workspace columns shared by every row this scene adds.
	const insert = `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref,issue_run_id,deleted_at,created_at)
	 VALUES($1,$2,$3,$3,'verified_request',$4,'isolated',$5,$6,1,'main',$7,$8,now()-$9::interval)`
	exec(`UPDATE workspaces SET created_at=now()-interval '10 hours' WHERE id=$1`, main)
	exec(`UPDATE workspaces SET created_at=now()-interval '1 hour' WHERE id=$1`, scene.ws)
	exec(`INSERT INTO issues(id,tenant_id,creator_user_id,project_ref,title,number) VALUES($1,$2,$3,$4,'Earlier issue',2)`, earlierIssue, seed.tenant, user, seed.project)
	exec(`INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status) VALUES($1,$2,$3,'agent',$4,'failed')`, earlierRun, seed.tenant, earlierIssue, seed.agent)
	exec(insert, deletedRun, seed.tenant, user, seed.project, "deleted", "deleted", earlierRun, "now", "5 hours")
	exec(insert, deletedIsolated, seed.tenant, user, seed.project, "deleted", "deleted", nil, "now", "4 hours")
	exec(insert, isolated, seed.tenant, user, seed.project, "running", "ready", nil, nil, "3 hours")
	exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'Isolated')`, uuid.NewString(), isolated)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	directory := func() []Object {
		t.Helper()
		var out []Object
		if err := s.view(context.Background(), func(tx *transaction) { out = workspaceDirectory(tx, scene.ws) }); err != nil {
			t.Fatal(err)
		}
		return out
	}

	want := []Object{
		{"workspaceId": scene.ws, "kind": "run", "issueId": seed.issue, "issueTitle": "disp issue", "issueRunId": seed.run, "runStatus": "dispatched"},
		{"workspaceId": isolated, "kind": "isolated"},
		{"workspaceId": deletedRun, "kind": "run", "issueId": earlierIssue, "issueTitle": "Earlier issue", "issueRunId": earlierRun, "runStatus": "failed"},
		{"workspaceId": main, "kind": "main"},
	}
	// The complete value: the deleted isolated Workspace and the other Project's are absent.
	if got := directory(); jsonText(got) != jsonText(want) {
		t.Fatalf("directory = %s\nwant %s", jsonText(got), jsonText(want))
	}

	// More than the limit: the newest 100 are listed and the oldest drop out.
	tx, err = s.Pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for i := range workspaceDirectoryLimit {
		id := uuid.NewString()
		exec(insert, id, seed.tenant, user, seed.project, "running", "ready", nil, nil, fmt.Sprintf("%d minutes", 61+i))
		exec(`INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'Bulk')`, uuid.NewString(), id)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := directory()
	if len(got) != workspaceDirectoryLimit || got[0].S("workspaceId") != scene.ws {
		t.Fatalf("directory holds %d listings starting with %v, want %d starting with the session's own", len(got), got[0], workspaceDirectoryLimit)
	}
	for _, listing := range got {
		if id := listing.S("workspaceId"); id == isolated || id == deletedRun || id == main {
			t.Fatalf("an older Workspace %s outlived the limit", id)
		}
	}
}
