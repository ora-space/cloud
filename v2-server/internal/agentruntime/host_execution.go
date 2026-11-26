//go:build linux

package agentruntime

import (
	"context"
	"log/slog"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// A session given host tools has its Node execution recorded (Options.RecordHostExecution) before
// its start is sent, and settled once with why it ended. The first reason decided stands: what made
// this driver end the session, which settles it before Node is asked to, what Node reported, or the
// loss of its Node. A server that shuts down
// settles nothing: the next start settles what is still running, as it does after a crash.

const (
	hostExecutionWrite        = 10 * time.Second
	defaultHostExecutionRetry = 5 * time.Second
)

// recordHost records the execution. Its failure means the start must not be sent. The row may exist
// all the same, so from here on the execution counts as recorded.
func (a *agentSession) recordHost() error {
	record := a.project.driver.opts.RecordHostExecution
	if record == nil || a.project.driver.opts.EndHostExecution == nil {
		return execution.ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(a.project.ctx, hostExecutionWrite)
	defer cancel()
	err := record(ctx, a.execution, a.frozen.ID)
	a.mu.Lock()
	a.hostRecorded = true
	decided := a.hostReason != "" // the Node may have been lost while this wrote
	a.mu.Unlock()
	if decided {
		a.settleHost("")
	}
	return err
}

// decideHost keeps reason as why the execution ended, unless one was decided before.
func (a *agentSession) decideHost(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.decideHostLocked(reason)
}

func (a *agentSession) decideHostLocked(reason string) {
	if a.hostReason == "" {
		a.hostReason = reason
	}
}

// settleHost writes that the execution ended, with reason unless one was decided before. The write
// happens before the caller goes on, so nothing that learns of the end from it finds the execution
// running; one that fails is repeated in the background.
//
// A caller that finds another one settling goes on without waiting for its write. Both locks are
// held while the execution is marked and its reason listed, so that caller, and whoever ends the
// session after it, finds the reason in HostExecutionEnds until it is written.
func (a *agentSession) settleHost(reason string) {
	d := a.project.driver
	d.mu.Lock()
	a.mu.Lock()
	a.decideHostLocked(reason)
	if !a.hostRecorded || a.hostSettled || d.closing.Load() {
		a.mu.Unlock()
		d.mu.Unlock()
		return
	}
	a.hostSettled = true
	reason = a.hostReason
	if d.unsettled == nil {
		d.unsettled = map[string]hostExecutionEnd{}
	}
	d.unsettled[a.execution] = hostExecutionEnd{session: a.frozen.ID, reason: reason}
	a.mu.Unlock()
	d.mu.Unlock()
	if a.endHost(reason) == nil {
		return
	}
	retry := d.hostExecutionRetry
	if retry <= 0 {
		retry = defaultHostExecutionRetry
	}
	go func() {
		for {
			time.Sleep(retry)
			if a.project.driver.closing.Load() || a.endHost(reason) == nil {
				return
			}
		}
	}()
}

func (a *agentSession) endHost(reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), hostExecutionWrite)
	defer cancel()
	err := a.project.driver.opts.EndHostExecution(ctx, a.execution, reason)
	if err != nil {
		slog.Warn("record the end of an Agent execution", "execution", a.execution, "reason", reason, "error", err)
		return err
	}
	a.project.driver.mu.Lock()
	delete(a.project.driver.unsettled, a.execution)
	a.project.driver.mu.Unlock()
	return nil
}

// hostExecutionEnd is why an execution of a session ended.
type hostExecutionEnd struct{ session, reason string }

// HostExecutionEnds lists, by execution, why the session's host executions ended whose end is not
// written yet. Whoever ends the session settles them with it: the write repeated here may come later
// and changes nothing then.
func (d *Driver) HostExecutionEnds(sid string) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var ends map[string]string
	for id, end := range d.unsettled {
		if end.session == sid {
			if ends == nil {
				ends = map[string]string{}
			}
			ends[id] = end.reason
		}
	}
	return ends
}

// HostExecutionEnding reports whether why that execution of the session ended was decided, written
// or not. Its credentials are void from then on. An end not written yet counts only for the session
// the execution belongs to: another session asking about the same execution ID learns nothing.
func (d *Driver) HostExecutionEnding(sid, executionID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if end, unwritten := d.unsettled[executionID]; unwritten && end.session == sid {
		return true
	}
	a := d.sessions[sid]
	if a == nil || a.execution != executionID {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hostReason != ""
}
