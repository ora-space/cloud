package server

import (
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/scheduler"
)

// NewScheduleCoordinator uses the same execution dependency as page handlers, and for workflow
// targets the same run store and execution bridge as the run routes; without the bridge, workflow
// targets are refused as unsupported. The host owns one instance, calls Recover after public
// execution startup recovery, then drives Tick/Manual, or gives it to a scheduler.Host.
// Construction starts no timer.
func NewScheduleCoordinator(records scheduler.CoordinatorStore, o Options, now func() time.Time) (*scheduler.Coordinator, error) {
	adapter, err := scheduler.NewConversationAdapter(records, o.Execution)
	if err != nil {
		return nil, err
	}
	runner := scheduler.TargetRunner{Conversation: adapter}
	if runs, ok := records.(scheduleWorkflowStore); ok && o.WorkflowRuns != nil {
		workflows, err := scheduler.NewWorkflowAdapter(records, scheduleWorkflowRuns{store: runs, host: o.WorkflowRuns})
		if err != nil {
			return nil, err
		}
		runner.Workflow = workflows
	}
	return scheduler.NewCoordinator(records, runner, now)
}
