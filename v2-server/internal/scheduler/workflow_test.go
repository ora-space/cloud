package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

const (
	workflowProject = "00000000-0000-0000-0000-000000000456"
	workflowTarget  = `{"kind":"workflow","workflowId":"11111111-1111-4111-8111-111111111111","snapshotId":"22222222-2222-4222-8222-222222222222","input":"整理本周的变更"}`
)

// fakeWorkflowRuns is the run entry as a map of runs by trigger identity, with the bridge's
// refusals injectable.
type fakeWorkflowRuns struct {
	runs                     map[string]*store.WorkflowRun
	creates, starts, cancels int
	createErr, startErr      error
	cancelErr                error
	created                  []string // "<scope user> <workflow>/<snapshot> <project> <input> <name>"
}

func newFakeWorkflowRuns() *fakeWorkflowRuns {
	return &fakeWorkflowRuns{runs: map[string]*store.WorkflowRun{}}
}

func (f *fakeWorkflowRuns) Create(_ context.Context, scope execution.Scope, target WorkflowTarget, projectID, triggerRef, name string) (store.WorkflowRun, error) {
	f.creates++
	if f.createErr != nil {
		return store.WorkflowRun{}, f.createErr
	}
	if _, ok := f.runs[triggerRef]; ok {
		return store.WorkflowRun{}, store.ErrWorkflowRunTriggerTaken
	}
	f.created = append(f.created, fmt.Sprintf("%s %s/%s %s %s %s", scope.UserID, target.WorkflowID, target.SnapshotID, projectID, target.Input, name))
	run := &store.WorkflowRun{ID: fmt.Sprintf("run-%d", len(f.runs)+1), Status: store.WorkflowStatusPending}
	f.runs[triggerRef] = run
	return *run, nil
}

func (f *fakeWorkflowRuns) Find(_ context.Context, _ execution.Scope, triggerRef string) (store.WorkflowRun, error) {
	run, ok := f.runs[triggerRef]
	if !ok {
		return store.WorkflowRun{}, store.ErrWorkflowRunNotFound
	}
	return *run, nil
}

func (f *fakeWorkflowRuns) byID(id string) *store.WorkflowRun {
	for _, run := range f.runs {
		if run.ID == id {
			return run
		}
	}
	return nil
}

func (f *fakeWorkflowRuns) Start(_ context.Context, _ execution.Scope, runID string) (store.WorkflowRun, error) {
	f.starts++
	if f.startErr != nil {
		return store.WorkflowRun{}, f.startErr
	}
	run := f.byID(runID)
	run.Status = store.WorkflowStatusRunning
	return *run, nil
}

func (f *fakeWorkflowRuns) Cancel(_ context.Context, _ execution.Scope, runID string) (store.WorkflowRun, error) {
	f.cancels++
	if f.cancelErr != nil {
		return store.WorkflowRun{}, f.cancelErr
	}
	run := f.byID(runID)
	if run.Status == store.WorkflowStatusPending || run.Status == store.WorkflowStatusRunning {
		run.Status = store.WorkflowStatusCancelled
	}
	return *run, nil
}

type workflowFixture struct {
	*modelFixture
	runs     *fakeWorkflowRuns
	workflow *WorkflowAdapter
}

func newWorkflowFixture(t *testing.T) *workflowFixture {
	t.Helper()
	f := newConversationFixture(t, workflowProject, json.RawMessage(workflowTarget))
	runs := newFakeWorkflowRuns()
	adapter, err := NewWorkflowAdapter(f.records, runs)
	if err != nil {
		t.Fatal(err)
	}
	return &workflowFixture{modelFixture: f, runs: runs, workflow: adapter}
}

func (f *workflowFixture) apply(t *testing.T, event, ref, reason string) store.Occurrence {
	t.Helper()
	o := f.read(t)
	o, err := f.records.ApplyOccurrenceEvidence(context.Background(), f.scope.TenantID, f.scope.UserID, o.ID, o.Version, store.ExecutionEvidence{Event: event, Ref: ref, Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestWorkflowOccurrenceRunsItsOwnRunOnceAndFollowsIt(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx := context.Background()
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	// One run, created from the frozen target in the owner's scope and named after the schedule.
	want := f.scope.UserID + " 11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222 " + workflowProject + " 整理本周的变更 task"
	if f.runs.creates != 1 || f.runs.starts != 1 || len(f.runs.created) != 1 || f.runs.created[0] != want {
		t.Fatalf("creates=%d starts=%d created=%q", f.runs.creates, f.runs.starts, f.runs.created)
	}
	o := f.read(t)
	if o.Status != "running" || o.ExecutionRef != `{"workflowRunId":"run-1"}` || o.Released {
		t.Fatalf("dispatched occurrence = %+v", o)
	}
	if id, ok := WorkflowRunOf(o.ExecutionRef); !ok || id != "run-1" {
		t.Fatalf("WorkflowRunOf = %q %v", id, ok)
	}

	// While the run executes the occurrence keeps its slot.
	if err := f.workflow.Reconcile(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "running" || o.Released {
		t.Fatalf("running occurrence = %+v", o)
	}
	f.runs.runs[f.occ.ID].Status = store.WorkflowStatusSucceeded
	if err := f.workflow.Reconcile(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "succeeded" || !o.Released || f.runs.creates != 1 || f.runs.starts != 1 {
		t.Fatalf("settled occurrence = %+v creates=%d starts=%d", o, f.runs.creates, f.runs.starts)
	}
}

func TestWorkflowOccurrenceRecordsAFailedRun(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx := context.Background()
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	f.runs.runs[f.occ.ID].Status = store.WorkflowStatusFailed
	if err := f.workflow.Reconcile(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "failed" || o.Reason != "workflow_run_failed" || !o.Released {
		t.Fatalf("failed run occurrence = %+v", o)
	}
}

func TestWorkflowStartRefusalWithdrawsTheRun(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{ErrWorkflowNotStartable, "workflow_run_not_startable"},
		{execution.ErrUnavailable, "execution_unavailable"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			f := newWorkflowFixture(t)
			f.runs.startErr = tc.err
			if err := f.workflow.Dispatch(context.Background(), f.scope, f.occ); !errors.Is(err, tc.err) {
				t.Fatal(err)
			}
			o := f.read(t)
			if o.Status != "skipped" || o.Reason != tc.reason || !o.Released || o.ExecutionRef != "" {
				t.Fatalf("refused occurrence = %+v", o)
			}
			// Nothing started, and no pending run is left behind for a list to wait on.
			if f.runs.cancels != 1 || f.runs.runs[f.occ.ID].Status != store.WorkflowStatusCancelled {
				t.Fatalf("cancels=%d run=%+v", f.runs.cancels, f.runs.runs[f.occ.ID])
			}
		})
	}
}

func TestWorkflowTargetGoneIsRefused(t *testing.T) {
	f := newWorkflowFixture(t)
	f.runs.createErr = store.ErrWorkflowSnapshotNotFound
	if err := f.workflow.Dispatch(context.Background(), f.scope, f.occ); !errors.Is(err, store.ErrWorkflowSnapshotNotFound) {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "skipped" || o.Reason != "workflow_unavailable" || !o.Released {
		t.Fatalf("occurrence = %+v", o)
	}
}

func TestWorkflowDispatchFindsTheRunItAlreadyCreated(t *testing.T) {
	f := newWorkflowFixture(t)
	f.runs.runs[f.occ.ID] = &store.WorkflowRun{ID: "earlier", Status: store.WorkflowStatusPending}
	if err := f.workflow.Dispatch(context.Background(), f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "running" || o.ExecutionRef != `{"workflowRunId":"earlier"}` || len(f.runs.created) != 0 {
		t.Fatalf("occurrence = %+v created=%v", o, f.runs.created)
	}
}

func TestWorkflowUnknownCreateOutcomeIsLookedUpNotRetried(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx := context.Background()
	f.runs.createErr = context.DeadlineExceeded
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "reconciling" || o.Reason != "lookup_required" || o.Released {
		t.Fatalf("occurrence = %+v", o)
	}
	// The create never happened: recovery closes the trigger as not started and creates nothing.
	if err := f.workflow.Reconcile(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "skipped" || o.Reason != "execution_not_started" || !o.Released || f.runs.creates != 1 {
		t.Fatalf("occurrence = %+v creates=%d", o, f.runs.creates)
	}
}

func TestWorkflowRecoveryWithdrawsARunThatNeverStarted(t *testing.T) {
	f := newWorkflowFixture(t)
	f.apply(t, "begin", "", "")
	f.apply(t, "reconcile", "", "lookup_required")
	f.runs.runs[f.occ.ID] = &store.WorkflowRun{ID: "orphan", Status: store.WorkflowStatusPending}
	if err := f.workflow.Reconcile(context.Background(), f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "skipped" || o.Reason != "execution_not_started" || !o.Released || f.runs.starts != 0 {
		t.Fatalf("occurrence = %+v starts=%d", o, f.runs.starts)
	}
	if f.runs.runs[f.occ.ID].Status != store.WorkflowStatusCancelled {
		t.Fatalf("orphan run = %+v", f.runs.runs[f.occ.ID])
	}
}

func TestWorkflowRecoveryAdoptsAStartedRun(t *testing.T) {
	f := newWorkflowFixture(t)
	f.apply(t, "begin", "", "")
	f.apply(t, "reconcile", "", "lookup_required")
	f.runs.runs[f.occ.ID] = &store.WorkflowRun{ID: "started", Status: store.WorkflowStatusRunning}
	if err := f.workflow.Reconcile(context.Background(), f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "running" || o.ExecutionRef != `{"workflowRunId":"started"}` || f.runs.starts != 0 {
		t.Fatalf("occurrence = %+v", o)
	}
}

func TestWorkflowRunFailedByRestartIsInterrupted(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx := context.Background()
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	// The process restarted: recovery marks the occurrence, and the bridge failed the run it found executing.
	f.apply(t, "recover", "", "")
	f.runs.runs[f.occ.ID].Status = store.WorkflowStatusFailed
	if err := f.workflow.Reconcile(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "interrupted" || o.Reason != "server_restarted" || !o.Released {
		t.Fatalf("occurrence = %+v", o)
	}
}

func TestWorkflowCancelWaitsForTheRunToStop(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx := context.Background()
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	f.runs.cancelErr = ErrWorkflowStopUnconfirmed
	if err := f.workflow.Cancel(ctx, f.scope, f.occ); !errors.Is(err, ErrWorkflowStopUnconfirmed) {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "cancelling" || o.Released {
		t.Fatalf("unconfirmed cancel = %+v", o)
	}
	// The bridge closes the run once its turn ends; the next reconcile records it.
	f.runs.runs[f.occ.ID].Status = store.WorkflowStatusCancelled
	if err := f.workflow.Reconcile(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "cancelled" || !o.Released {
		t.Fatalf("cancelled occurrence = %+v", o)
	}
}

func TestWorkflowCancelConfirmedSettlesAtOnce(t *testing.T) {
	f := newWorkflowFixture(t)
	ctx := context.Background()
	if err := f.workflow.Dispatch(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if err := f.workflow.Cancel(ctx, f.scope, f.occ); err != nil {
		t.Fatal(err)
	}
	if o := f.read(t); o.Status != "cancelled" || !o.Released || f.runs.cancels != 1 {
		t.Fatalf("occurrence = %+v cancels=%d", o, f.runs.cancels)
	}
}

type spyRunner struct{ calls []string }

func (s *spyRunner) Dispatch(context.Context, execution.Scope, store.Occurrence) error {
	s.calls = append(s.calls, "dispatch")
	return nil
}
func (s *spyRunner) Reconcile(context.Context, execution.Scope, store.Occurrence) error {
	s.calls = append(s.calls, "reconcile")
	return nil
}
func (s *spyRunner) Cancel(context.Context, execution.Scope, store.Occurrence) error {
	s.calls = append(s.calls, "cancel")
	return nil
}

func TestTargetRunnerRoutesByTheFrozenTargetKind(t *testing.T) {
	ctx := context.Background()
	chat, workflow := &spyRunner{}, &spyRunner{}
	occurrence := func(target string) store.Occurrence {
		return store.Occurrence{Snapshot: json.RawMessage(`{"projectId":"p","target":` + target + `}`)}
	}
	runner := TargetRunner{Conversation: chat, Workflow: workflow}
	_ = runner.Dispatch(ctx, execution.Scope{}, occurrence(workflowTarget))
	_ = runner.Reconcile(ctx, execution.Scope{}, occurrence(`{"kind":"chat"}`))
	_ = runner.Cancel(ctx, execution.Scope{}, occurrence(workflowTarget))
	if fmt.Sprint(chat.calls) != "[reconcile]" || fmt.Sprint(workflow.calls) != "[dispatch cancel]" {
		t.Fatalf("chat=%v workflow=%v", chat.calls, workflow.calls)
	}
	// Without a workflow adapter the conversation adapter answers, and refuses the target.
	alone := TargetRunner{Conversation: chat}
	_ = alone.Dispatch(ctx, execution.Scope{}, occurrence(workflowTarget))
	if fmt.Sprint(chat.calls) != "[reconcile dispatch]" {
		t.Fatalf("chat=%v", chat.calls)
	}
}
