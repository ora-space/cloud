package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

const decompositionGraph = `{"nodes":[{"id":"start","type":"workflow","data":{"kind":"start"}},{"id":"out","type":"workflow","data":{"kind":"output","outputs":[{"name":"tasks","variableSelector":["start","input"]}]}}],"edges":[{"id":"e","source":"start","target":"out"}]}`

// refusedDecomposition is a task with a running decomposition over a pending run, as the
// decomposition host leaves them when the workflow host refuses to start the run.
func refusedDecomposition(t *testing.T, f *squadFixture, user string) (store.Task, store.WorkflowRun, store.TaskDecomposition) {
	t.Helper()
	addProject(t, f, user)
	w, _, err := f.store.CreateWorkflow(f.ctx, f.tenant, "Split", decompositionGraph)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.store.PublishWorkflow(f.ctx, f.tenant, w.ID, "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := createTask(t, f, user, store.TaskInput{Title: "账号体系"})
	input := "账号体系"
	run, err := f.store.CreateWorkflowRun(f.ctx, f.tenant, w.ID, snap.ID, store.WorkflowRunInput{ProjectID: taskProject, Input: &input})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.store.CreateTaskDecomposition(f.ctx, store.NewTaskDecomposition{
		TaskID: task.ID, TenantID: f.tenant, StartedBy: user, StartKey: "click", WorkflowID: w.ID, WorkflowName: run.Name, WorkflowRunID: run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, run, d
}

// Refusing a decomposition cancels its never-started run and fails the decomposition in one
// commit, as RefuseTaskWorkflow does for a handling run: a second refusal changes nothing, and a
// decomposition already settled is left alone with its run.
func TestRefuseTaskDecompositionCancelsTheRunAndFailsTheDecompositionTogether(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "lead", "owner")
	_, run, d := refusedDecomposition(t, f, user)

	settled, done, err := f.store.RefuseTaskDecomposition(f.ctx, d.ID, "节点「起草」需要的技能 x 不可用")
	if err != nil || !done {
		t.Fatalf("refuse = %v done=%v", err, done)
	}
	if settled.Status != store.DecompositionFailed || settled.Failure != "节点「起草」需要的技能 x 不可用" || settled.CreatedCount != 0 || settled.FinishedAt == nil {
		t.Fatalf("refused decomposition = %+v", settled)
	}
	refused, err := f.store.WorkflowRun(f.ctx, f.tenant, run.ID)
	if err != nil || refused.Status != store.WorkflowStatusCancelled || refused.StartedAt != nil {
		t.Fatalf("refused run = %+v (err=%v), want cancelled and never started", refused, err)
	}

	// Again, as a repeated report: nothing changes and the caller learns it did not settle.
	again, done, err := f.store.RefuseTaskDecomposition(f.ctx, d.ID, "另一个原因")
	if err != nil || done || again.Failure != "节点「起草」需要的技能 x 不可用" {
		t.Fatalf("refuse again = %+v done=%v err=%v", again, done, err)
	}
	// The ordinary settlement agrees the decomposition is over.
	if _, done, err := f.store.SettleTaskDecomposition(f.ctx, d.ID, nil, "工作流运行已取消。"); err != nil || done {
		t.Fatalf("settle after refuse = done=%v err=%v", done, err)
	}
	if items, err := f.store.TaskDecompositions(f.ctx, f.tenant, d.TaskID); err != nil || len(items) != 1 || items[0].Failure != "节点「起草」需要的技能 x 不可用" {
		t.Fatalf("decompositions = %+v %v", items, err)
	}
}

// A run that already began executing is not withdrawn by the refusal: stopping it belongs to the
// workflow host, and the decomposition stays running so its real outcome settles it.
func TestRefuseTaskDecompositionLeavesAnExecutingRunAlone(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "lead", "owner")
	_, run, d := refusedDecomposition(t, f, user)
	if _, err := f.store.StartWorkflowRun(f.ctx, f.tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	settled, done, err := f.store.RefuseTaskDecomposition(f.ctx, d.ID, "工作流没有启动，请稍后重试。")
	if !errors.Is(err, store.ErrWorkflowRunState) || done {
		t.Fatalf("refuse an executing run = %v done=%v, want ErrWorkflowRunState", err, done)
	}
	if settled.Status != store.DecompositionRunning {
		t.Fatalf("decomposition = %+v, want it still running", settled)
	}
	live, err := f.store.WorkflowRun(f.ctx, f.tenant, run.ID)
	if err != nil || live.Status != store.WorkflowStatusRunning || live.StartedAt == nil {
		t.Fatalf("run = %+v (err=%v), want it still running", live, err)
	}
}

// A run deleted before the refusal is withdrawn never started either: the decomposition still
// fails, and nothing is left to cancel.
func TestRefuseTaskDecompositionOfADeletedRun(t *testing.T) {
	f := newSquadFixture(t)
	user := f.login(t, "lead", "owner")
	_, run, d := refusedDecomposition(t, f, user)
	if err := f.store.DeleteWorkflowRun(f.ctx, f.tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	settled, done, err := f.store.RefuseTaskDecomposition(f.ctx, d.ID, "工作流没有启动，请稍后重试。")
	if err != nil || !done || settled.Status != store.DecompositionFailed || settled.Failure != "工作流没有启动，请稍后重试。" {
		t.Fatalf("refuse = %+v done=%v err=%v", settled, done, err)
	}
}
