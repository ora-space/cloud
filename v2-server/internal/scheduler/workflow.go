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

// WorkflowTarget selects a fixed published snapshot (the default) or the effective release
// at each trigger. The run freezes the resolved snapshot. The project belongs to the schedule.
type WorkflowTarget struct {
	Kind          string `json:"kind"`
	WorkflowID    string `json:"workflowId"`
	SnapshotID    string `json:"snapshotId,omitempty"`
	VersionPolicy string `json:"versionPolicy,omitempty"`
	Input         string `json:"input"`
}

// WorkflowRuns is the workflow run entry the pages use, as the workflow adapter needs it: the run
// store for creating and finding a scheduled trigger's run, and the execution bridge for starting
// and cancelling it. Everything runs in the schedule owner's scope.
type WorkflowRuns interface {
	// Create freezes the resolved published snapshot, project and input into a pending run whose
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
	// ErrWorkflowRoleNotFound is the bridge refusing start because a referenced workspace role is missing.
	ErrWorkflowRoleNotFound = errors.New("workflow role not found")
	// ErrWorkflowSkillNotFound is the bridge refusing start because an enabled Skill is unavailable.
	ErrWorkflowSkillNotFound = errors.New("workflow skill not found")
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
	if target.WorkflowID == "" {
		return config, target, "workflow_required"
	}
	switch target.VersionPolicy {
	case "", "fixed":
		if target.SnapshotID == "" {
			return config, target, "workflow_required"
		}
	case "latest":
		if target.SnapshotID != "" {
			return config, target, "invalid_snapshot"
		}
	default:
		return config, target, "invalid_snapshot"
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
			if reason := workflowStartRejected(err); reason != "" {
				// Nothing started: withdraw the pending run so no list keeps waiting on it, then record
				// the trigger as skipped. The refusal is returned as itself only once both are persisted:
				// the page treats its 400 as final, which it must not be while the run is still pending or
				// the trigger still open. A failed withdrawal or write is returned as itself instead, and
				// Reconcile closes the trigger from the run's state on a later tick.
				if _, cancelErr := a.runs.Cancel(writeCtx, scope, run.ID); cancelErr != nil {
					return fmt.Errorf("withdraw refused workflow run (%v): %w", err, cancelErr)
				}
				if _, applyErr := a.apply(writeCtx, scope, o, "rejected", "", reason); applyErr != nil {
					return fmt.Errorf("record refused workflow run (%v): %w", err, applyErr)
				}
				return err
			}
			_, applyErr := a.apply(writeCtx, scope, o, "reconcile", "", "lookup_required")
			return errors.Join(err, applyErr)
		}
	}
	_, err = a.apply(writeCtx, scope, o, "accepted", encodeWorkflowRef(run.ID), "")
	return err
}

func workflowStartRejected(err error) string {
	switch {
	case errors.Is(err, execution.ErrUnavailable):
		return "execution_unavailable"
	case errors.Is(err, ErrWorkflowSkillNotFound):
		return "workflow_skill_not_found"
	case errors.Is(err, ErrWorkflowRoleNotFound):
		return "workflow_role_not_found"
	case errors.Is(err, ErrWorkflowNotStartable):
		return "workflow_run_not_startable"
	}
	return ""
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
// anything: a run the occurrence never started is withdrawn and the trigger closed as not started
// (pending, or already cancelled without ever beginning), and an accepted run that was deleted
// fails the occurrence.
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
	if errors.Is(err, store.ErrWorkflowRunNotFound) {
		// The accepted run was deleted before its end settled the occurrence; only a finished run
		// can be deleted, so nothing is left to follow and the slot is freed.
		if o, err = a.apply(ctx, scope, o, "failed", o.ExecutionRef, "workflow_run_deleted"); err != nil {
			return err
		}
		return a.release(ctx, scope, o)
	}
	if err != nil {
		return err
	}
	ref := encodeWorkflowRef(run.ID)
	if o.ExecutionRef != "" && o.ExecutionRef != ref {
		return fmt.Errorf("occurrence names another workflow run: %w", execution.ErrConflict)
	}
	switch run.Status {
	case "awaitingInput":
		if o.Status == "starting" || o.Status == "reconciling" {
			o, err = a.apply(ctx, scope, o, "accepted", ref, "")
			if err != nil {
				return err
			}
		}
		_, err = a.apply(ctx, scope, o, "waiting_for_input", ref, "")
		return err
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
		if run.Status == store.WorkflowStatusCancelled && run.StartedAt == nil && o.ExecutionRef == "" {
			// Withdrawn before it began, as a refused start's run is, and the skip it was withdrawn
			// for could not be written (Dispatch's or an earlier Reconcile's): the trigger never
			// ran, so it closes as not started, not as a cancellation of a run it never accepted. The
			// refusal's own reason was in the unwritten record and is gone; the generic one holds.
			_, err := a.apply(ctx, scope, o, "not_accepted", "", "execution_not_started")
			return err
		}
		if o.Status == "starting" || (o.Status == "reconciling" && o.ExecutionRef == "") {
			if o, err = a.apply(ctx, scope, o, "accepted", ref, ""); err != nil {
				return err
			}
		}
		return a.finish(ctx, scope, o, run)
	default:
		// A run still executing after startup recovery was absorbed by the workflow sweep (an
		// interrupted Iteration it resumed): the occurrence follows it as running again, as the
		// awaitingInput branch does, so Cancel works and its later end is recorded for what it is
		// rather than as the restart. A recovered cancelling occurrence loses its cancel intent.
		if o.Status == "starting" || o.Status == "reconciling" {
			_, err = a.apply(ctx, scope, o, "accepted", ref, "")
		}
		return err
	}
}

// finish records the run's result and frees the slot. A run that failed while the occurrence was
// still being recovered after a restart was interrupted by it: the bridge's boot sweep fails every
// run it finds executing, except an interrupted Iteration it absorbs and resumes, whose occurrence
// Reconcile has already returned to running before that run can end.
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

// TargetRunner routes frozen targets to their owning adapters; the conversation adapter
// rejects unsupported kinds when their owning service is not installed.
type TargetRunner struct {
	Conversation Runner
	Workflow     Runner
	Task         Runner
}

func (r TargetRunner) pick(o store.Occurrence) Runner {
	var config struct {
		Target struct {
			Kind string `json:"kind"`
		} `json:"target"`
	}
	if json.Unmarshal(o.Snapshot, &config) == nil {
		if config.Target.Kind == "workflow" && r.Workflow != nil {
			return r.Workflow
		}
		if config.Target.Kind == "task" && r.Task != nil {
			return r.Task
		}
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
