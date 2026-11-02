package server

import (
	"context"
	"errors"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/scheduler"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// scheduleWorkflowStore is the part of the run store a scheduled workflow needs: creating a run
// under the trigger's identity and finding it again by that identity.
type scheduleWorkflowStore interface {
	CreateWorkflowRun(context.Context, string, string, string, store.WorkflowRunInput) (store.WorkflowRun, error)
	ScheduledWorkflowRun(context.Context, string, string) (store.WorkflowRun, error)
}

// scheduleWorkflowRuns gives the scheduler the workflow run entry the pages use: the same store
// the run routes create runs in, and the same execution bridge that starts and cancels them.
type scheduleWorkflowRuns struct {
	store scheduleWorkflowStore
	host  WorkflowRunHost
}

var _ scheduler.WorkflowRuns = scheduleWorkflowRuns{}

func runScope(scope execution.Scope) WorkflowRunScope {
	return WorkflowRunScope{TenantID: scope.TenantID, UserID: scope.UserID}
}

func (r scheduleWorkflowRuns) Create(ctx context.Context, scope execution.Scope, target scheduler.WorkflowTarget, projectID, triggerRef, name string) (store.WorkflowRun, error) {
	in := store.WorkflowRunInput{ProjectID: projectID, Name: name, TriggerKind: "schedule", TriggerRef: triggerRef}
	if target.Input != "" {
		in.Input = &target.Input
	}
	return r.store.CreateWorkflowRun(ctx, scope.TenantID, target.WorkflowID, target.SnapshotID, in)
}

func (r scheduleWorkflowRuns) Find(ctx context.Context, scope execution.Scope, triggerRef string) (store.WorkflowRun, error) {
	return r.store.ScheduledWorkflowRun(ctx, scope.TenantID, triggerRef)
}

func (r scheduleWorkflowRuns) Start(ctx context.Context, scope execution.Scope, runID string) (store.WorkflowRun, error) {
	run, err := r.host.StartRun(ctx, runScope(scope), runID)
	if errors.Is(err, ErrWorkflowRunNotStartable) {
		return run, errors.Join(scheduler.ErrWorkflowNotStartable, err)
	}
	return run, err
}

func (r scheduleWorkflowRuns) Cancel(ctx context.Context, scope execution.Scope, runID string) (store.WorkflowRun, error) {
	run, err := r.host.CancelRun(ctx, runScope(scope), runID)
	if errors.Is(err, ErrWorkflowRunStopUnconfirmed) {
		return run, errors.Join(scheduler.ErrWorkflowStopUnconfirmed, err)
	}
	return run, err
}
