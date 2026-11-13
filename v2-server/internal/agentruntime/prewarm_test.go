//go:build linux

package agentruntime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/node"
)

// startGate holds each Node start until the test releases it, failing it when released with an error.
type startGate struct {
	entered chan string
	release chan error
}

func gateStarts(h *ipcHarness) *startGate {
	g := &startGate{entered: make(chan string, 8), release: make(chan error, 8)}
	start := h.driver.startProject
	h.driver.startProject = func(p *projectRuntime) error {
		g.entered <- p.id
		if err := <-g.release; err != nil {
			return err
		}
		return start(p)
	}
	return g
}

// waitingAcquires reports each acquire that waits for a Node still starting.
func waitingAcquires(h *ipcHarness) chan string {
	waiting := make(chan string, 8)
	h.driver.waiting = func(id string) { waiting <- id }
	return waiting
}

// fakeClock lets a test move idle time forward.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func useFakeClock(h *ipcHarness) *fakeClock {
	c := &fakeClock{now: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)}
	h.driver.now = func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
	return c
}

func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

// runtimeStage keeps the runtime stage a turn's preparation last published.
type runtimeStage struct {
	mu    sync.Mutex
	stage *execution.PreparationStage
}

func (r *runtimeStage) emit(_ context.Context, event execution.Event) error {
	if event.Kind == execution.EventPreparation && len(event.Preparation.Stages) > 0 {
		r.mu.Lock()
		stage := event.Preparation.Stages[0]
		r.stage = &stage
		r.mu.Unlock()
	}
	return nil
}

func (r *runtimeStage) reused(t *testing.T) bool {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stage == nil || r.stage.ID != stageRuntime || r.stage.EndedAt == nil {
		t.Fatalf("runtime stage = %+v", r.stage)
	}
	return r.stage.Reused
}

func (h *ipcHarness) registered(id string) *projectRuntime {
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	return h.driver.projects[id]
}

func expectNoNode(t *testing.T, h *ipcHarness) {
	t.Helper()
	select {
	case <-h.created:
		t.Fatal("an unexpected Node incarnation started")
	default:
	}
}

// chat runs one chat turn on project "project" and returns its runtime stage and the Node that
// served it. A nil f expects the turn to start a new incarnation.
func chat(t *testing.T, h *ipcHarness, f *fakeNodeIPC, id string) (*runtimeStage, *fakeNodeIPC) {
	t.Helper()
	stage := &runtimeStage{}
	done := startIPCRun(h, ipcContext(t), h.session(id, id), id, stage.emit)
	if f == nil {
		f = receiveIPC(t, h.created)
	}
	f.finishTurn(t, nextIPCCommand(t, f, "start_agent_session"), id, "OK")
	if got := receiveIPC(t, done); got.err != nil || got.result.Answer != "OK" {
		t.Fatalf("turn %s = %+v", id, got)
	}
	return stage, f
}

func endSession(t *testing.T, h *ipcHarness, f *fakeNodeIPC, id string) {
	t.Helper()
	ended := make(chan error, 1)
	go func() { ended <- h.driver.End(ipcContext(t), id) }()
	f.terminal(nextIPCCommand(t, f, "end_session"))
	if err := receiveIPC(t, ended); err != nil {
		t.Fatal(err)
	}
}

func TestPrewarmedNodeServesFirstSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	if err := h.driver.Warm(ipcContext(t), "project", h.config); err != nil {
		t.Fatal(err)
	}
	f := receiveIPC(t, h.created)
	if stage, _ := chat(t, h, f, "chat"); !stage.reused(t) {
		t.Fatal("a prewarmed Node was not reported as reused")
	}
	expectNoNode(t, h)
}

// #1026: a failed prewarm changes nothing for the project; its next session cold-starts.
func TestPrewarmFailureLeavesSessionToColdStart(t *testing.T) {
	var installs atomic.Int32
	h := newIPCHarness(t, func(f *fakeNodeIPC, m node.Message) bool {
		if m.Type == "install_plugins" && installs.Add(1) == 1 {
			f.reply(m, "plugins_result", map[string]string{"kind": "plugins_failed"})
			return true
		}
		return false
	})
	if err := h.driver.Warm(ipcContext(t), "project", h.config); err == nil {
		t.Fatal("failed prewarm reported success")
	}
	failed := receiveIPC(t, h.created)
	if failed.stops.Load() == 0 || h.registered("project") != nil {
		t.Fatal("failed prewarm was not stopped and dropped")
	}
	stage, f := chat(t, h, nil, "chat")
	if f.id == failed.id || stage.reused(t) {
		t.Fatal("session did not cold-start its own Node")
	}
}

// #1026: a session created while the Node is still warming waits for that same Node.
func TestSessionDuringPrewarmWaitsForTheSameNode(t *testing.T) {
	h := newIPCHarness(t, nil)
	gate, waiting := gateStarts(h), waitingAcquires(h)
	warmed := make(chan error, 1)
	go func() { warmed <- h.driver.Warm(ipcContext(t), "project", h.config) }()
	receiveIPC(t, gate.entered)
	stage := &runtimeStage{}
	done := startIPCRun(h, ipcContext(t), h.session("chat", "chat"), "chat", stage.emit)
	receiveIPC(t, waiting)
	gate.release <- nil
	if err := receiveIPC(t, warmed); err != nil {
		t.Fatal(err)
	}
	f := receiveIPC(t, h.created)
	f.finishTurn(t, nextIPCCommand(t, f, "start_agent_session"), "chat", "OK")
	if got := receiveIPC(t, done); got.err != nil || got.result.Answer != "OK" {
		t.Fatalf("turn = %+v", got)
	}
	if stage.reused(t) {
		t.Fatal("a session that waited for the Node was reported as reused")
	}
	expectNoNode(t, h)
}

// #1026: when the prewarm a session is waiting for fails, the session cold-starts once itself
// instead of failing the conversation.
func TestSessionDuringFailedPrewarmColdStartsOnce(t *testing.T) {
	h := newIPCHarness(t, nil)
	gate, waiting := gateStarts(h), waitingAcquires(h)
	warmed := make(chan error, 1)
	go func() { warmed <- h.driver.Warm(ipcContext(t), "project", h.config) }()
	receiveIPC(t, gate.entered)
	done := startIPCRun(h, ipcContext(t), h.session("chat", "chat"), "chat", discardIPCEvent)
	receiveIPC(t, waiting)
	gate.release <- errors.New("prewarm failed")
	if err := receiveIPC(t, warmed); err == nil {
		t.Fatal("failed prewarm reported success")
	}
	receiveIPC(t, gate.entered) // the session's own cold start
	gate.release <- nil
	f := receiveIPC(t, h.created)
	f.finishTurn(t, nextIPCCommand(t, f, "start_agent_session"), "chat", "OK")
	if got := receiveIPC(t, done); got.err != nil || got.result.Answer != "OK" {
		t.Fatalf("turn = %+v", got)
	}
}

// A cold start the session asked for itself still fails as before: there is no second attempt.
func TestSessionOwnColdStartFailureIsNotRetried(t *testing.T) {
	h := newIPCHarness(t, nil)
	gate := gateStarts(h)
	done := startIPCRun(h, ipcContext(t), h.session("chat", "chat"), "chat", discardIPCEvent)
	receiveIPC(t, gate.entered)
	gate.release <- errors.New("start failed")
	if got := receiveIPC(t, done); !errors.Is(got.err, execution.ErrUnavailable) {
		t.Fatalf("turn = %+v", got)
	}
	select {
	case <-gate.entered:
		t.Fatal("a session's own failed start was retried")
	default:
	}
}

// #1026: after an idle Node is reclaimed, the next session starts a new one.
func TestReclaimedNodeIsReplacedForNextSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	clock := useFakeClock(h)
	if err := h.driver.Warm(ipcContext(t), "project", h.config); err != nil {
		t.Fatal(err)
	}
	old := receiveIPC(t, h.created)
	clock.advance(DefaultIdleTimeout - time.Second)
	h.driver.reclaim(ipcContext(t))
	if old.stops.Load() != 0 {
		t.Fatal("Node reclaimed before its idle timeout")
	}
	clock.advance(2 * time.Second)
	h.driver.reclaim(ipcContext(t))
	if old.stops.Load() == 0 || h.registered("project") != nil {
		t.Fatal("idle Node was not reclaimed")
	}
	stage, f := chat(t, h, nil, "chat")
	if f.id == old.id || stage.reused(t) {
		t.Fatal("session did not start a new Node after reclaim")
	}
}

func TestReclaimKeepsNodesInUse(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.driver.controlTimeout = 5 * time.Second
	clock := useFakeClock(h)
	if err := h.driver.Warm(ipcContext(t), "project", h.config); err != nil {
		t.Fatal(err)
	}
	f := receiveIPC(t, h.created)
	reclaimAfterTimeout := func() {
		clock.advance(DefaultIdleTimeout + time.Minute)
		h.driver.reclaim(ipcContext(t))
	}

	// A live conversation owns its workspace for as long as it lasts.
	chat(t, h, f, "chat")
	reclaimAfterTimeout()
	if f.stops.Load() != 0 {
		t.Fatal("reclaimed a Node with a live session")
	}
	endSession(t, h, f, "chat")
	h.driver.reclaim(ipcContext(t))
	if f.stops.Load() != 0 {
		t.Fatal("idle time did not restart when the session ended")
	}

	// A caller that found the Node but does not own a workspace yet.
	p := h.registered("project")
	if _, _, err := h.driver.acquire(ipcContext(t), "project", h.config, true); err != nil {
		t.Fatal(err)
	}
	reclaimAfterTimeout()
	if f.stops.Load() != 0 {
		t.Fatal("reclaimed a pinned Node")
	}
	p.unpin()

	// A Node command in flight, such as a skill change.
	replied := make(chan error, 1)
	go func() {
		_, err := p.control(ipcContext(t), "skill", "remove_plugins", "plugins_result", map[string]any{})
		replied <- err
	}()
	command := nextIPCCommand(t, f, "remove_plugins")
	reclaimAfterTimeout()
	if f.stops.Load() != 0 {
		t.Fatal("reclaimed a Node with a command in flight")
	}
	f.reply(command, "plugins_result", map[string]string{"kind": "plugins_completed"})
	if err := receiveIPC(t, replied); err != nil {
		t.Fatal(err)
	}
	reclaimAfterTimeout()
	if f.stops.Load() == 0 {
		t.Fatal("Node idle again was not reclaimed")
	}
}

func TestReclaimKeepsTheDemoNode(t *testing.T) {
	h := newIPCHarness(t, nil)
	clock := useFakeClock(h)
	h.driver.WarmDemo(ipcContext(t), "project", h.config)
	f := receiveIPC(t, h.created)
	p := h.registered("project")
	p.mu.Lock()
	spare := p.spare
	p.mu.Unlock()
	receiveIPC(t, spare.ready)
	clock.advance(24 * time.Hour)
	h.driver.reclaim(ipcContext(t))
	if f.stops.Load() != 0 || h.registered("project") != p {
		t.Fatal("reclaimed the Demo Node")
	}
}

func TestReclaimStopsLeastRecentlyUsedOverTheCap(t *testing.T) {
	h := newIPCHarness(t, nil)
	h.driver.opts.MaxIdle = 2
	clock := useFakeClock(h)
	var nodes []*fakeNodeIPC
	for _, id := range []string{"a", "b", "c"} {
		if err := h.driver.Warm(ipcContext(t), id, h.config); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, receiveIPC(t, h.created))
		clock.advance(time.Minute)
	}
	h.driver.reclaim(ipcContext(t))
	if nodes[0].stops.Load() == 0 || h.registered("a") != nil {
		t.Fatal("least recently used idle Node was not stopped over the cap")
	}
	if nodes[1].stops.Load() != 0 || nodes[2].stops.Load() != 0 {
		t.Fatal("stopped Nodes within the cap")
	}
}

func TestReclaimWithUnconfirmedStopKeepsTheNodeRegistered(t *testing.T) {
	h := newIPCHarness(t, nil)
	clock := useFakeClock(h)
	if err := h.driver.Warm(ipcContext(t), "project", h.config); err != nil {
		t.Fatal(err)
	}
	old := receiveIPC(t, h.created)
	old.stopOK.Store(false)
	clock.advance(DefaultIdleTimeout + time.Minute)
	h.driver.reclaim(ipcContext(t))
	if old.stops.Load() == 0 || h.registered("project") == nil {
		t.Fatal("a Node whose stop is unconfirmed was dropped")
	}
	// No second incarnation may start in its directory until the stop is confirmed.
	got := receiveIPC(t, startIPCRun(h, ipcContext(t), h.session("blocked", "blocked"), "blocked", discardIPCEvent))
	if !errors.Is(got.err, execution.ErrUnavailable) {
		t.Fatalf("turn on an unconfirmed Node = %+v", got)
	}
	expectNoNode(t, h)
	old.stopOK.Store(true)
	h.driver.reclaim(ipcContext(t))
	if h.registered("project") != nil {
		t.Fatal("the next pass did not retry the stop")
	}
	if _, f := chat(t, h, nil, "chat"); f.id == old.id {
		t.Fatal("session reused the stopped Node")
	}
}

func TestIdleNodeCrashIsReplacedForNextSession(t *testing.T) {
	h := newIPCHarness(t, nil)
	if err := h.driver.Warm(ipcContext(t), "project", h.config); err != nil {
		t.Fatal(err)
	}
	old := receiveIPC(t, h.created)
	p := h.registered("project")
	old.Close()
	receiveIPC(t, p.dead)
	if _, f := chat(t, h, nil, "chat"); f.id == old.id || old.stops.Load() == 0 {
		t.Fatal("crashed idle Node was not stopped and replaced")
	}
}

func TestCloseDuringPrewarmStopsTheNode(t *testing.T) {
	h := newIPCHarness(t, nil)
	gate := gateStarts(h)
	warmed := make(chan error, 1)
	go func() { warmed <- h.driver.Warm(ipcContext(t), "project", h.config) }()
	receiveIPC(t, gate.entered)
	closed := make(chan error, 1)
	go func() { closed <- h.driver.Close(ipcContext(t)) }()
	gate.release <- nil
	f := receiveIPC(t, h.created)
	if err := receiveIPC(t, closed); err != nil {
		t.Fatal(err)
	}
	receiveIPC(t, warmed)
	if f.stops.Load() == 0 {
		t.Fatal("Close left the prewarmed Node running")
	}
	if err := h.driver.Warm(ipcContext(t), "other", h.config); !errors.Is(err, execution.ErrUnavailable) {
		t.Fatalf("Warm after Close = %v", err)
	}
	expectNoNode(t, h)
}
