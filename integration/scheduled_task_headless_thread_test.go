package integration

import (
	"testing"
)

// markScheduledThreadScene records the admission-ledger origin for the scene's run, which is the only
// fact the headless D3 guards consult to tell a scheduled occurrence from an ordinary run.
func markScheduledThreadScene(t *testing.T, f *fixture, scene threadScene) {
	t.Helper()
	taskID := newTestID()
	agentID := seedScheduledTaskAgent(t, f.store.Pool, scene.seed)
	_, err := f.store.Pool.Exec(`
		INSERT INTO scheduled_tasks(id,tenant_id,issue_id,creator_user_id,agent_id,name,prompt,schedule,timezone,revision,version,enabled)
		VALUES($1,$2,$3,$4,$5,'headless','Sync','{"kind":"daily","hour":0,"minute":0}'::jsonb,'UTC',1,1,false)`,
		taskID, scene.tenantID, scene.issueID, scene.seed.userID, agentID)
	must(t, err)
	_, err = f.store.Pool.Exec(`
		INSERT INTO scheduled_task_occurrences(tenant_id,task_id,scheduled_at,outcome,task_revision,observed_at,run_id)
		VALUES($1,$2,now(),'admitted',1,now(),$3)`, scene.tenantID, taskID, scene.runID)
	must(t, err)
}

// TestScheduledThreadIsClosedInEveryState pins D3 `canAppend = false` for a scheduled run's whole
// lifetime: in every Thread state, including the accepting `pending`, `active` and `idle`, the Thread
// read projects `canAppend: false` and the append endpoint answers `409 thread_closed` without
// recording a user turn or releasing a command.
func TestScheduledThreadIsClosedInEveryState(t *testing.T) {
	for _, tc := range []struct{ phase, state string }{
		{"starting", "pending"},
		{"running", "active"},
		{"running", "idle"},
		{"running", "ending"},
		{"running", "ended"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := setup(t)
			f.bindBusinessHooks()
			scene := seedThreadScene(t, f)
			markScheduledThreadScene(t, f, scene)
			must(t, f.setThreadState(scene.runID, tc.phase, tc.state))

			if page := f.getThread(scene, "", 200); page.B("canAppend") || page.S("threadState") != tc.state {
				t.Fatalf("a scheduled Thread in %s must read canAppend=false, got state=%q canAppend=%v", tc.state, page.S("threadState"), page.B("canAppend"))
			}
			f.postThread(scene, "key-scheduled-"+tc.state, threadBody("no continuation"), 409, "thread_closed")
			if n := f.threadEntries(scene.runID); n != 0 {
				t.Fatalf("a scheduled Thread in %s must not record a user turn, got %d", tc.state, n)
			}
			if n := f.threadCommands(scene.runID); n != 0 {
				t.Fatalf("a scheduled Thread in %s must not release a user command, got %d", tc.state, n)
			}
		})
	}
}
