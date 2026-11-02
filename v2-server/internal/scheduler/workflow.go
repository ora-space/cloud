package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// WorkflowTarget is a scheduled workflow: one frozen published snapshot of a workflow and the
// kickoff input it starts with. The project is the schedule's own projectId.
type WorkflowTarget struct {
	Kind       string `json:"kind"`
	WorkflowID string `json:"workflowId"`
	SnapshotID string `json:"snapshotId"`
	Input      string `json:"input"`
}

// WorkflowRuns is the workflow run entry the pages use, as the workflow adapter needs it: the run
// store for creating and finding a scheduled trigger's run, and the execution bridge for starting
// and cancelling it. Everything runs in the schedule owner's scope.
type WorkflowRuns interface {
	// Create freezes the target's snapshot, the project and the input into a pending run whose
	// stable identity is triggerRef; store.ErrWorkflowRunTriggerTaken when that run exists.
	Create(ctx context.Context, scope execution.Scope, target WorkflowTarget, projectID, triggerRef, name string) (store.WorkflowRun, error)
	// Find returns the run triggerRef created; store.ErrWorkflowRunNotFound when there is none.
	Find(ctx context.Context, scope execution.Scope, triggerRef string) (store.WorkflowRun, error)
	// Start begins a pending run; ErrWorkflowNotStartable when the bridge refuses it.
	Start(ctx context.Context, scope execution.Scope, runID string) (store.WorkflowRun, error)
	// Cancel stops the run; ErrWorkflowStopUnconfirmed when execution did not confirm the stop.
	Cancel(ctx context.Context, scope execution.Scope, runID string) (store.WorkflowRun, error)
}

var (
	// ErrWorkflowNotStartable is the bridge refusing to start the run: the graph holds a node kind
	// v2 cannot execute yet, or the input does not satisfy the Start node.
	ErrWorkflowNotStartable = errors.New("workflow run cannot start")
	// ErrWorkflowStopUnconfirmed is a cancellation execution has not confirmed yet.
	ErrWorkflowStopUnconfirmed = errors.New("workflow run stop unconfirmed")
)

// WorkflowAdapter executes scheduled workflow occurrences through the same run entry the pages
// use: it creates the run under the occurrence's identity, starts it, and then follows only that
// run. It copies no graph rule or engine and never drives Node itself.
type WorkflowAdapter struct {
	records RecordStore
	runs    WorkflowRuns
	mu      sync.Mutex
	active  map[string]bool
}

// NewWorkflowAdapter requires the occurrence records and the workflow run entry.
func NewWorkflowAdapter(records RecordStore, runs WorkflowRuns) (*WorkflowAdapter, error) {
	if records == nil || runs == nil {
		return nil, errors.New("schedule workflow adapter requires records and workflow runs")
	}
	return &WorkflowAdapter{records: records, runs: runs, active: make(map[string]bool)}, nil
}

// enter keeps a lookup from overtaking an in-process create and start of the same occurrence.
func (a *WorkflowAdapter) enter(id string) (func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[id] {
		return nil, ErrOperationInProgress
	}
	a.active[id] = true
	return func() { a.mu.Lock(); delete(a.active, id); a.mu.Unlock() }, nil
}

func (a *WorkflowAdapter) get(ctx context.Context, scope execution.Scope, id string) (store.Occurrence, error) {
	return a.records.GetOccurrence(ctx, scope.TenantID, scope.UserID, id)
}

func (a *WorkflowAdapter) apply(ctx context.Context, scope execution.Scope, o store.Occurrence, event, ref, reason string) (store.Occurrence, error) {
	return a.records.ApplyOccurrenceEvidence(ctx, scope.TenantID, scope.UserID, o.ID, o.Version, store.ExecutionEvidence{Event: event, Ref: ref, Reason: reason})
}

// workflowRef is the occurrence's execution reference for a workflow target: the run it created.
type workflowRef struct {
	WorkflowRunID string `json:"workflowRunId"`
}

func encodeWorkflowRef(runID string) string {
	encoded, _ := json.Marshal(workflowRef{WorkflowRunID: runID})
	return string(encoded)
}

// WorkflowRunOf reads the run a workflow occurrence's execution reference names, if it is one.
func WorkflowRunOf(executionRef string) (string, bool) {
	var ref workflowRef
	if json.Unmarshal([]byte(executionRef), &ref) != nil || ref.WorkflowRunID == "" {
		return "", false
	}
	return ref.WorkflowRunID, true
}

// workflowRequest reads the frozen target; a schedule edited after the occurrence was recorded
// does not change what it runs.
func workflowRequest(o store.Occurrence) (store.ScheduleConfig, WorkflowTarget, string) {
	var config store.ScheduleConfig
	if json.Unmarshal(o.Snapshot, &config) != nil {
		return config, WorkflowTarget{}, "invalid_snapshot"
	}
	var target WorkflowTarget
	if json.Unmarshal(config.Target, &target) != nil || target.Kind != "workflow" {
		return config, target, "unsupported_schedule_target"
	}
	if strings.TrimSpace(config.ProjectID) == "" {
		return config, target, "project_required"
	}
	if target.WorkflowID == "" || target.SnapshotID == "" {
		return config, target, "workflow_required"
	}
	return config, target, ""
}

// Dispatch creates the occurrence's run and starts it. The run's identity is the occurrence, so a
// repeated dispatch finds the run it already created instead of creating another.
func (a *WorkflowAdapter) Dispatch(ctx context.Context, scope execution.Scope, occurrence store.Occurrence) error {
	leave, err := a.enter(occurrence.ID)
	if err != nil {
		return err
	}
	defer leave()
	o, err := a.get(ctx, scope, occurrence.ID)
	if err != nil {
		return err
	}
	if o.Status != "recorded" {
		return nil
	}
	o, err = a.apply(ctx, scope, o, "begin", "", "")
	if err != nil || o.Status != "starting" {
		return err
	}
	// Outcomes are persisted with a bounded independent context, so a cancelled caller cannot
	// lose a run that was already created or started.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	config, target, code := workflowRequest(o)
	if code != "" {
		_, err = a.apply(writeCtx, scope, o, "rejected", "", code)
		return err
	}
	run, err := a.runs.Create(ctx, scope, target, config.ProjectID, o.ID, config.Name)
	if errors.Is(err, store.ErrWorkflowRunTriggerTaken) {
		run, err = a.runs.Find(ctx, scope, o.ID)
	}
	if err != nil {
		if reason := workflowTargetUnavailable(err); reason != "" {
			_, applyErr := a.apply(writeCtx, scope, o, "rejected", "", reason)
			return errors.Join(err, applyErr)
		}
		// The run may exist: only its identity can tell, so recovery looks it up.
		_, applyErr := a.apply(writeCtx, scope, o, "reconcile", "", "lookup_required")
		return errors.Join(err, applyErr)
	}
	if run.Status == store.WorkflowStatusPending {
		if _, err = a.runs.Start(ctx, scope, run.ID); err != nil {
			if errors.Is(err, ErrWorkflowNotStartable) || errors.Is(err, execution.ErrUnavailable) {
				// Nothing started: withdraw the pending run so no list keeps waiting on it.
				_, cancelErr := a.runs.Cancel(writeCtx, scope, run.ID)
				reason := "workflow_run_not_startable"
				if errors.Is(err, execution.ErrUnavailable) {
					reason = "execution_unavailable"
				}
				_, applyErr := a.apply(writeCtx, scope, o, "rejected", "", reason)
				return errors.Join(err, cancelErr, applyErr)
			}
			_, applyErr := a.apply(writeCtx, scope, o, "reconcile", "", "lookup_required")
			return errors.Join(err, applyErr)
		}
	}
	_, err = a.apply(writeCtx, scope, o, "accepted", encodeWorkflowRef(run.ID), "")
	return err
}

// workflowTargetUnavailable names a frozen target that can no longer be created: the workflow,
// its snapshot or the project is gone. Creating nothing is then certain.
func workflowTargetUnavailable(err error) string {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrWorkflowSnapshotNotFound),
		errors.Is(err, store.ErrWorkflowDraftSnapshot), errors.Is(err, store.ErrWorkflowNoPublishedSnapshot):
		return "workflow_unavailable"
	case errors.Is(err, store.ErrProjectNotFound):
		return "project_unavailable"
	}
	return ""
}

// Reconcile follows the occurrence's own run, found by its identity, without creating or starting
// anything: a run the occurrence never started is withdrawn and the trigger closed as not started.
// The coordinator calls it on every tick for unreleased occurrences, which is how a finished run
// settles its occurrence and frees the schedule's slot.
func (a *WorkflowAdapter) Reconcile(ctx context.Context, scope execution.Scope, occurrence store.Occurrence) error {
	leave, err := a.enter(occurrence.ID)
	if err != nil {
		return err
	}
	defer leave()
	o, err := a.get(ctx, scope, occurrence.ID)
	if err != nil {
		return err
	}
	if o.Released || o.Status == "recorded" {
		return nil
	}
	if isResult(o.Status) {
		return a.release(ctx, scope, o)
	}
	run, err := a.runs.Find(ctx, scope, o.ID)
	if errors.Is(err, store.ErrWorkflowRunNotFound) && o.ExecutionRef == "" {
		_, err = a.apply(ctx, scope, o, "not_accepted", "", "execution_not_started")
		return err
	}
	if err != nil {
		return err
	}
	ref := encodeWorkflowRef(run.ID)
	if o.ExecutionRef != "" && o.ExecutionRef != ref {
		return fmt.Errorf("occurrence names another workflow run: %w", execution.ErrConflict)
	}
	switch run.Status {
	case store.WorkflowStatusPending:
		if o.ExecutionRef != "" {
			return fmt.Errorf("an accepted workflow run is pending: %w", execution.ErrConflict)
		}
		// Only this adapter starts the run, and it is not doing so now: the start never happened.
		if _, err := a.runs.Cancel(ctx, scope, run.ID); err != nil {
			return err
		}
		_, err = a.apply(ctx, scope, o, "not_accepted", "", "execution_not_started")
		return err
	case store.WorkflowStatusSucceeded, store.WorkflowStatusFailed, store.WorkflowStatusCancelled:
		if o.Status == "starting" || (o.Status == "reconciling" && o.ExecutionRef == "") {
			if o, err = a.apply(ctx, scope, o, "accepted", ref, ""); err != nil {
				return err
			}
		}
		return a.finish(ctx, scope, o, run)
	default:
		if o.Status == "starting" || (o.Status == "reconciling" && o.ExecutionRef == "") {
			_, err = a.apply(ctx, scope, o, "accepted", ref, "")
		}
		return err
	}
}

// finish records the run's result and frees the slot. A run that failed while the occurrence was
// being recovered after a restart was interrupted by it: the bridge fails every run it finds still
// executing when the process starts.
func (a *WorkflowAdapter) finish(ctx context.Context, scope execution.Scope, o store.Occurrence, run store.WorkflowRun) error {
	if !isResult(o.Status) {
		event, reason := "completed", ""
		switch run.Status {
		case store.WorkflowStatusFailed:
			event, reason = "failed", "workflow_run_failed"
			if o.Reason == "server_restarted" {
				event, reason = "interrupted", "server_restarted"
			}
		case store.WorkflowStatusCancelled:
			event = "cancelled"
		}
		var err error
		if o, err = a.apply(ctx, scope, o, event, encodeWorkflowRef(run.ID), reason); err != nil {
			return err
		}
	}
	return a.release(ctx, scope, o)
}

// release frees the schedule's slot once the result is recorded. The bridge already ended every
// node session of the run, so there is no session for the schedule to end.
func (a *WorkflowAdapter) release(ctx context.Context, scope execution.Scope, o store.Occurrence) error {
	if o.Released {
		return nil
	}
	_, err := a.apply(ctx, scope, o, "released", o.ExecutionRef, "")
	return err
}

// Cancel stops the occurrence's run through the bridge. An unconfirmed stop keeps the occurrence
// cancelling; the run's own end settles it on a later Reconcile.
func (a *WorkflowAdapter) Cancel(ctx context.Context, scope execution.Scope, occurrence store.Occurrence) error {
	leave, err := a.enter(occurrence.ID)
	if err != nil {
		return err
	}
	defer leave()
	o, err := a.get(ctx, scope, occurrence.ID)
	if err != nil {
		return err
	}
	if o.Released {
		return nil
	}
	if isResult(o.Status) {
		return a.release(ctx, scope, o)
	}
	if o.Status != "running" && o.Status != "cancelling" {
		return execution.ErrConflict
	}
	runID, ok := WorkflowRunOf(o.ExecutionRef)
	if !ok {
		return execution.ErrConflict
	}
	if o.Status == "running" {
		if o, err = a.apply(ctx, scope, o, "cancel_requested", "", ""); err != nil {
			return err
		}
	}
	run, err := a.runs.Cancel(ctx, scope, runID)
	if err != nil {
		return err
	}
	if run.Status == store.WorkflowStatusPending || run.Status == store.WorkflowStatusRunning {
		return nil
	}
	return a.finish(ctx, scope, o, run)
}

// TargetRunner routes each occurrence to the adapter for its frozen target's kind: conversations
// to the conversation adapter, workflows to the workflow adapter. Without a workflow adapter the
// conversation adapter refuses workflow targets as unsupported.
type TargetRunner struct {
	Conversation Runner
	Workflow     Runner
}

func (r TargetRunner) pick(o store.Occurrence) Runner {
	var config struct {
		Target struct {
			Kind string `json:"kind"`
		} `json:"target"`
	}
	if r.Workflow != nil && json.Unmarshal(o.Snapshot, &config) == nil && config.Target.Kind == "workflow" {
		return r.Workflow
	}
	return r.Conversation
}

// Dispatch delegates to the occurrence's adapter.
func (r TargetRunner) Dispatch(ctx context.Context, scope execution.Scope, o store.Occurrence) error {
	return r.pick(o).Dispatch(ctx, scope, o)
}

// Reconcile delegates to the occurrence's adapter.
func (r TargetRunner) Reconcile(ctx context.Context, scope execution.Scope, o store.Occurrence) error {
	return r.pick(o).Reconcile(ctx, scope, o)
}

// Cancel delegates to the occurrence's adapter.
func (r TargetRunner) Cancel(ctx context.Context, scope execution.Scope, o store.Occurrence) error {
	return r.pick(o).Cancel(ctx, scope, o)
}
