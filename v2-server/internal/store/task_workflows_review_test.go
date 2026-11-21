package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
	"github.com/wanglongan587/cloud/v2-server/internal/taskworkflow"
)

type reviewPort struct {
	taskWorkflowPort
	prepare    func(context.Context, string) error
	startError error
	started    bool
}

func (p *reviewPort) Prepare(ctx context.Context, _, _, id string, _ *string, _ map[string]json.RawMessage) error {
	if p.prepare != nil {
		return p.prepare(ctx, id)
	}
	return nil
}
func (p *reviewPort) Start(ctx context.Context, tenant, user, id string) error {
	p.started = true
	if p.startError != nil {
		return p.startError
	}
	return p.taskWorkflowPort.Start(ctx, tenant, user, id)
}

func reviewWorkflow(t *testing.T) (*squadFixture, string, store.Task, string) {
	t.Helper()
	f := newSquadFixture(t)
	user := f.login(t, "owner", "owner")
	addProject(t, f, user)
	task := createTask(t, f, user, store.TaskInput{Title: "Task", Assignee: &store.TaskAssigneeRef{Type: "human", ID: user}})
	w, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Handling", handlingGraph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil); err != nil {
		t.Fatal(err)
	}
	return f, user, task, w.ID
}

func TestReviewStrandedLinkAfterTransitionConflict(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	port := &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}, prepare: func(ctx context.Context, id string) error {
		_, err := f.db.Exec(ctx, `UPDATE v2_tasks SET status='review',version=version+1 WHERE id=$1`, task.ID)
		return err
	}}
	h := taskworkflow.New(f.store, port)
	defer h.Close()
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "first", WorkflowID: wid}
	id, err := h.Start(f.ctx, in, nil)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("start=%s %v", id, err)
	}
	var link string
	if err := f.db.QueryRow(f.ctx, `SELECT status FROM v2_task_workflow_runs WHERE run_id=$1`, id).Scan(&link); err != nil || link != "failed" {
		t.Fatalf("stranded link=%s %v", link, err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != "review" {
		t.Fatalf("restore=%+v %v", got, err)
	}
	run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("unexecuted run=%+v %v", run, err)
	}
	agent, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "agent", ExecutorID: "test"})
	if err != nil {
		t.Fatalf("Agent still blocked: %v", err)
	}
	if err := f.store.EndTaskRun(f.ctx, agent.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	port.prepare = nil
	in.Key = "next"
	if _, err := h.Start(f.ctx, in, nil); err != nil {
		t.Fatalf("new-key workflow still blocked: %v", err)
	}
}

func TestTaskWorkflowPreflightAndRefusal(t *testing.T) {
	for _, initial := range []string{"pending", "review", "running"} {
		t.Run(initial, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			if _, err := f.db.Exec(f.ctx, `UPDATE v2_tasks SET status=$2 WHERE id=$1`, task.ID, initial); err != nil {
				t.Fatal(err)
			}
			port := &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}}
			h := taskworkflow.New(f.store, port)
			defer h.Close()
			in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "invalid", WorkflowID: wid}
			if _, err := h.Start(f.ctx, in, map[string]json.RawMessage{"branch": json.RawMessage(`12`)}); err == nil {
				t.Fatal("invalid value accepted")
			}
			list, err := f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
			if err != nil || len(list) != 0 {
				t.Fatalf("preflight wrote link: %+v %v", list, err)
			}
			got, _ := f.store.Task(f.ctx, f.tenant, task.ID)
			if got.Status != initial {
				t.Fatalf("preflight moved %s to %s", initial, got.Status)
			}
			bad, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Invalid", `{"nodes":[],"edges":[]}`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.PublishWorkflow(f.ctx, f.tenant, bad.ID, "v1", nil); err != nil {
				t.Fatal(err)
			}
			in.WorkflowID = bad.ID
			if _, err := h.Start(f.ctx, in, nil); !errors.Is(err, taskworkflow.ErrStartGraph) {
				t.Fatalf("invalid graph=%v", err)
			}
			list, err = f.store.TaskWorkflowRuns(f.ctx, f.tenant, task.ID)
			if err != nil || len(list) != 0 {
				t.Fatalf("invalid graph wrote link: %+v %v", list, err)
			}
			in.WorkflowID = wid
			port.startError = errors.New("host refused after admission")
			if _, err := h.Start(f.ctx, in, nil); err == nil {
				t.Fatal("post-admission refusal hidden")
			}
			got, _ = f.store.Task(f.ctx, f.tenant, task.ID)
			if got.Status != initial {
				t.Fatalf("refused start moved %s to %s", initial, got.Status)
			}
			list, err = h.List(f.ctx, f.tenant, task.ID)
			if err != nil || len(list) != 1 || list[0].Status != "failed" || list[0].Failure != port.startError.Error() {
				t.Fatalf("refusal=%+v %v", list, err)
			}
		})
	}
}

func TestTaskWorkflowVersionFenceAndClosedDuringPrepare(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	port := &reviewPort{taskWorkflowPort: taskWorkflowPort{db: f.store}}
	h := taskworkflow.New(f.store, port)
	defer h.Close()
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "run", WorkflowID: wid}
	id, err := h.Start(f.ctx, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := f.store.Task(f.ctx, f.tenant, task.ID)
	if _, err := f.store.SetTaskStatus(f.ctx, storetest.Access(t, f.store, f.tenant, user), task.ID, "cancelled", current.Version); !errors.Is(err, store.ErrWorkflowActive) {
		t.Fatalf("manual close while handling=%v", err)
	}
	if _, err := f.store.SetTaskStatus(f.ctx, storetest.Access(t, f.store, f.tenant, user), task.ID, "review", current.Version); !errors.Is(err, store.ErrWorkflowActive) {
		t.Fatalf("submit for review while handling=%v", err)
	}
	for _, to := range []string{"pending", "running"} {
		current, err = f.store.TransitionTaskStatus(f.ctx, f.tenant, task.ID, current.Status, to)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, "succeeded", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.List(f.ctx, f.tenant, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := f.store.Task(f.ctx, f.tenant, task.ID)
	if got.Status != "running" || got.Version != current.Version {
		t.Fatalf("ABA overwritten: %+v", got)
	}
	port.prepare = func(ctx context.Context, _ string) error {
		_, err := f.db.Exec(ctx, `UPDATE v2_tasks SET status='cancelled',version=version+1 WHERE id=$1`, task.ID)
		return err
	}
	port.started = false
	in.Key = "closed"
	if _, err := h.Start(f.ctx, in, nil); !errors.Is(err, store.ErrInvalidTransition) {
		t.Fatalf("closed during prepare=%v", err)
	}
	if port.started {
		t.Fatal("executed a closed task")
	}
	got, _ = f.store.Task(f.ctx, f.tenant, task.ID)
	if got.Status != "cancelled" {
		t.Fatalf("closed state overwritten: %+v", got)
	}
}

func TestTaskWorkflowRecoveryCancelsAndTenantBacklink(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "pending", WorkflowID: wid}
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	h := taskworkflow.New(f.store, &taskWorkflowPort{db: f.store})
	defer h.Close()
	if err := h.Recover(f.ctx); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.WorkflowRun(f.ctx, f.tenant, id)
	if err != nil || run.Status != "cancelled" {
		t.Fatalf("recovery left executable run=%+v %v", run, err)
	}
	foreign := "00000000-0000-4000-8000-000000000099"
	if got, kind, err := f.store.WorkflowRunTaskLink(f.ctx, f.tenant, id); err != nil || got != task.ID || kind != store.TaskLinkHandle {
		t.Fatalf("own-tenant backlink=%s %s %v", got, kind, err)
	}
	if got, kind, err := f.store.WorkflowRunTaskLink(f.ctx, foreign, id); err != nil || got != "" || kind != "" {
		t.Fatalf("cross-tenant backlink leaked=%s %s %v", got, kind, err)
	}
	if _, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "after", ExecutorID: "test"}); err != nil {
		t.Fatalf("terminal link blocks Agent=%v", err)
	}
}

func TestTaskWorkflowIdleAgentAndCallerKeys(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	agent, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "agent", ExecutorID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_task_runs SET turn_active=false WHERE id=$1`, agent.ID); err != nil {
		t.Fatal(err)
	}
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "same", WorkflowID: wid}
	if _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in); !errors.Is(err, store.ErrRunActive) {
		t.Fatalf("idle Agent ignored=%v", err)
	}
	if err := f.store.EndTaskRun(f.ctx, agent.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CancelWorkflowRun(f.ctx, f.tenant, id); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, id, "cancelled", ""); err != nil {
		t.Fatal(err)
	}
	other := f.login(t, "other", "member")
	current, _ := f.store.Task(f.ctx, f.tenant, task.ID)
	if _, err := f.store.SetTaskAssignee(f.ctx, f.tenant, task.ID, "", &store.TaskAssigneeRef{Type: "human", ID: other}, current.Version); err != nil {
		t.Fatal(err)
	}
	in.UserID = other
	second, fresh, err := f.store.CreateTaskWorkflowRun(f.ctx, in)
	if err != nil || !fresh || second == id {
		t.Fatalf("another caller's key replayed=%s %t %v", second, fresh, err)
	}
}

func TestTaskWorkflowAdmissionLocks(t *testing.T) {
	for _, side := range []string{"agent", "workflow"} {
		t.Run(side, func(t *testing.T) {
			f, user, task, wid := reviewWorkflow(t)
			run, err := f.store.CreateWorkflowRun(f.ctx, f.tenant, wid, "", store.WorkflowRunInput{ProjectID: task.ProjectID})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := f.db.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(f.ctx)
			if _, err := tx.Exec(f.ctx, `SELECT id FROM v2_tasks WHERE id=$1 FOR UPDATE`, task.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if side == "agent" {
					_, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "agent", ExecutorID: "test"})
					done <- err
				} else {
					_, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "workflow", WorkflowID: wid})
					done <- err
				}
			}()
			// Observe the real PostgreSQL lock wait instead of relying on a scheduling race or sleep.
			ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
			defer cancel()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for {
				var blocked bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pg_backend_pid()=ANY(pg_blocking_pids(pid)))`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("admission never waited for task lock")
				case err := <-done:
					t.Fatalf("admission escaped task lock: %v", err)
				case <-tick.C:
				}
			}
			if side == "agent" {
				_, err = tx.Exec(f.ctx, `INSERT INTO v2_task_workflow_runs(tenant_id,task_id,run_id,started_by,start_key) VALUES($1,$2,$3,$4,'opposite')`, f.tenant, task.ID, run.ID, user)
			} else {
				_, err = tx.Exec(f.ctx, `INSERT INTO v2_task_runs(tenant_id,task_id,started_by,start_key,executor_id,executor_name) VALUES($1,$2,$3,'opposite','test','test')`, f.tenant, task.ID, user)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(f.ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, store.ErrRunActive) {
				t.Fatalf("stale admission accepted after opposite reservation: %v", err)
			}
		})
	}
}

func TestTaskWorkflowHistoryDeletedAndReadOnly(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "handle", WorkflowID: wid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, "succeeded", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, id, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	split, err := f.store.CreateWorkflowRun(f.ctx, f.tenant, wid, "", store.WorkflowRunInput{ProjectID: task.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.store.CreateTaskDecomposition(f.ctx, store.NewTaskDecomposition{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "split", WorkflowID: wid, WorkflowRunID: split.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.SettleTaskDecomposition(f.ctx, d.ID, []store.Subtask{{Title: "Child"}}, ""); err != nil {
		t.Fatal(err)
	}
	if got, kind, err := f.store.WorkflowRunTaskLink(f.ctx, f.tenant, split.ID); err != nil || got != task.ID || kind != store.TaskLinkDecomposition {
		t.Fatalf("own-tenant decomposition backlink: %s %s %v", got, kind, err)
	}
	if got, kind, err := f.store.WorkflowRunTaskLink(f.ctx, "00000000-0000-4000-8000-000000000099", split.ID); err != nil || got != "" || kind != "" {
		t.Fatalf("decomposition backlink leaked: %s %s %v", got, kind, err)
	}
	for _, rid := range []string{id, split.ID} {
		if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, rid); err != nil {
			t.Fatal(err)
		}
	}
	h := taskworkflow.New(f.store, &taskWorkflowPort{db: f.store})
	defer h.Close()
	// A historical read must not acquire the task lock even though another writer holds it.
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, `SELECT id FROM v2_tasks WHERE id=$1 FOR UPDATE`, task.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.ctx, time.Second)
	defer cancel()
	list, err := h.List(ctx, f.tenant, task.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("history=%+v %v", list, err)
	}
	for _, r := range list {
		if !r.Deleted {
			t.Fatalf("deleted run still openable: %+v", r)
		}
	}
}

func TestTaskWorkflowSettlementRequiresCurrentLinkIdentity(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	first, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "old", WorkflowID: wid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, first, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, first, store.WorkflowStatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	second, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "revoked", WorkflowID: wid})
	if err != nil {
		t.Fatal(err)
	}
	// An older settled link still has revision 1. Only the current link's identity may satisfy
	// the fence after a status cycle revokes the new admission's ownership.
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_task_workflow_runs SET status_revision=2 WHERE run_id=$1`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, second, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, second, store.WorkflowStatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskRunning {
		t.Fatalf("revoked admission settled through an older link: %+v %v", got, err)
	}
}

func TestTaskWorkflowSettlementDoesNotRepeatAfterAgentRestart(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "settled", WorkflowID: wid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, id, store.WorkflowStatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	agent, err := f.store.CreateTaskRun(f.ctx, store.NewTaskRun{TenantID: f.tenant, TaskID: task.ID, StartedBy: user, StartKey: "agent", ExecutorID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TransitionTaskStatus(f.ctx, f.tenant, task.ID, store.TaskReview, store.TaskRunning); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SettleTaskWorkflow(f.ctx, f.tenant, id, store.WorkflowStatusSucceeded, ""); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskRunning {
		t.Fatalf("duplicate handling settlement overwrote Agent status: %+v %v", got, err)
	}
	if err := f.store.EndTaskRun(f.ctx, agent.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
}

// Settlement locks the task row before the link, mirroring admission and the manual status
// writers (which lock the task and then bump the link revision). Dropping that lock inverts the
// order, so a settlement racing a status write deadlocks in PostgreSQL instead of serializing.
func TestTaskWorkflowSettlementLocksTaskBeforeLink(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "settling", WorkflowID: wid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishWorkflowRun(f.ctx, f.tenant, id, store.WorkflowStatusSucceeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A status write holds the task row first, exactly like writeTaskStatus, then bumps the link.
	tx, err := f.db.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx)
	if _, err := tx.Exec(f.ctx, `SELECT id FROM v2_tasks WHERE tenant_id=$1 AND id=$2 FOR UPDATE`, f.tenant, task.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.store.SettleTaskWorkflow(f.ctx, f.tenant, id, store.WorkflowStatusSucceeded, "") }()
	// Observe the real PostgreSQL lock wait instead of a scheduling race or sleep.
	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pg_backend_pid()=ANY(pg_blocking_pids(pid)))`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("settlement never reached the task lock")
		case err := <-done:
			t.Fatalf("settlement finished while the task row was locked: %v", err)
		case <-tick.C:
		}
	}
	// The concurrent status write bumps the link revision under the task lock, then commits. With
	// the lock inverted this update would wait on the settlement's link row and deadlock.
	if _, err := tx.Exec(f.ctx, `UPDATE v2_task_workflow_runs SET status_revision=status_revision+1 WHERE tenant_id=$1 AND run_id=$2 AND status='running'`, f.tenant, id); err != nil {
		t.Fatalf("status write deadlocked or failed: %v", err)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatalf("status write commit failed: %v", err)
	}
	// The settlement serialized after the commit instead of deadlocking, and the bumped revision
	// fences its task move: the link settles while the task keeps its current status.
	if err := <-done; err != nil {
		t.Fatalf("settlement failed: %v", err)
	}
	var link string
	if err := f.db.QueryRow(f.ctx, `SELECT status FROM v2_task_workflow_runs WHERE run_id=$1`, id).Scan(&link); err != nil || link != store.WorkflowStatusSucceeded {
		t.Fatalf("link=%s %v", link, err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskRunning {
		t.Fatalf("fenced task move inverted: %+v %v", got, err)
	}
}

func TestTaskWorkflowAssigneePolicy(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	member := f.login(t, "member", "member")
	outsider := f.login(t, "outsider", "member")
	squad, err := f.store.CreateSquad(f.ctx, f.tenant, user, store.SquadInput{Name: "Squad"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AddSquadMember(f.ctx, storetest.Access(t, f.store, f.tenant, user), squad.ID, store.SquadMemberInput{Type: store.MemberHuman, UserID: member, Version: squad.Version}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetTaskAssignee(f.ctx, f.tenant, task.ID, "", &store.TaskAssigneeRef{Type: store.AssigneeSquad, ID: squad.ID}, task.Version); err != nil {
		t.Fatal(err)
	}
	in := store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: outsider, Key: "squad", WorkflowID: wid}
	if _, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in); !errors.Is(err, store.ErrTaskNotAssignee) {
		t.Fatalf("squad outsider=%v", err)
	}
	in.UserID = member
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, in)
	if err != nil {
		t.Fatalf("squad member=%v", err)
	}
	if got, kind, err := f.store.WorkflowRunTaskLink(f.ctx, f.tenant, id); err != nil || got != task.ID || kind != store.TaskLinkHandle {
		t.Fatalf("squad member's handling link=%s %s %v", got, kind, err)
	}
	if got, kind, err := f.store.WorkflowRunTaskLink(f.ctx, "00000000-0000-4000-8000-000000000099", id); err != nil || got != "" || kind != "" {
		t.Fatalf("cross-tenant backlink leaked=%s %s %v", got, kind, err)
	}
	// The Agent-assigned case uses the same policy without needing executor configuration here.
	assigned := store.Task{TenantID: f.tenant, Assignee: &store.TaskAssignee{Type: store.AssigneeAgent, ID: "agent"}}
	if err := store.AuthorizeTaskRunner(f.ctx, assigned, outsider, f.store.InSquad); err != nil {
		t.Fatalf("Agent-assigned member=%v", err)
	}
}

func TestInterruptTaskRunInvalidatesWorkflowStatusRevision(t *testing.T) {
	f, user, task, wid := reviewWorkflow(t)
	id, _, err := f.store.CreateTaskWorkflowRun(f.ctx, store.NewTaskWorkflow{TenantID: f.tenant, TaskID: task.ID, UserID: user, Key: "interrupted", WorkflowID: wid})
	if err != nil {
		t.Fatal(err)
	}
	// Model an old overlapping Agent run and handling link to verify the status fence even
	// when interruption is called independently of the boot sequence.
	if _, err := f.db.Exec(f.ctx, `INSERT INTO v2_task_runs(task_id,tenant_id,started_by,start_key,executor_id,executor_name)
		VALUES($1,$2,$3,'agent','test','test')`, task.ID, f.tenant, user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(f.ctx, `UPDATE v2_tasks SET status='running' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := f.db.QueryRow(f.ctx, `SELECT status_revision FROM v2_task_workflow_runs WHERE run_id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.InterruptTaskRuns(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(f.ctx, `SELECT status_revision FROM v2_task_workflow_runs WHERE run_id=$1`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Task(f.ctx, f.tenant, task.ID)
	if err != nil || got.Status != store.TaskPending || after != before+1 {
		t.Fatalf("interruption: status=%s, revision=%d to %d, err=%v", got.Status, before, after, err)
	}
}
