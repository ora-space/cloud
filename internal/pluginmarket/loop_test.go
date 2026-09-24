package pluginmarket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// countingSync records every call and notifies the test; a one-shot failure
// proves the loop survives errors.
type countingSync struct {
	mu    sync.Mutex
	calls int
	fail  bool
	ran   chan struct{}
}

func (c *countingSync) Sync(_ context.Context) error {
	c.mu.Lock()
	c.calls++
	if c.fail {
		c.fail = false
		c.mu.Unlock()
		return errors.New("injected sync failure")
	}
	n := c.calls
	c.mu.Unlock()
	if n <= 3 {
		c.ran <- struct{}{}
	}
	return nil
}

// instantSleep never blocks but still honors cancellation, so the loop runs
// as fast as the test reads from ran and always exits on cancel.
func instantSleep(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// gatedSleep parks the loop until the test releases it, then returns the ctx
// error so cancellation always wins.
func gatedSleep(ctx context.Context, _ time.Duration, released <-chan struct{}) error {
	select {
	case <-released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a sync iteration")
	}
}

func TestRunSyncLoopSyncsImmediatelyThenPerInterval(t *testing.T) {
	counter := &countingSync{ran: make(chan struct{}, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		RunSyncLoop(ctx, counter.Sync, time.Hour, func(c context.Context, _ time.Duration) error {
			return gatedSleep(c, 0, released)
		}, zap.NewNop())
		close(finished)
	}()
	// The first sync happens at startup, before any sleep; the second only
	// after one tick is released.
	waitFor(t, counter.ran)
	select {
	case <-counter.ran:
		t.Fatal("the second sync must wait for a tick")
	default:
	}
	released <- struct{}{}
	waitFor(t, counter.ran)
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop must exit when ctx is canceled")
	}
}

func TestRunSyncLoopSurvivesFailures(t *testing.T) {
	counter := &countingSync{ran: make(chan struct{}, 4), fail: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() {
		RunSyncLoop(ctx, counter.Sync, time.Hour, instantSleep, zap.NewNop())
		close(finished)
	}()
	// The first sync fails, the next tick succeeds; both must happen.
	waitFor(t, counter.ran)
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop must exit when ctx is canceled")
	}
	counter.mu.Lock()
	defer counter.mu.Unlock()
	if counter.calls < 2 {
		t.Fatalf("the loop must keep ticking after a failed sync, calls=%d", counter.calls)
	}
}

func TestRunSyncLoopExitsWhenCancelledDuringSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		RunSyncLoop(ctx, func(context.Context) error { return nil }, time.Hour, func(c context.Context, _ time.Duration) error {
			<-c.Done()
			return c.Err()
		}, zap.NewNop())
		close(finished)
	}()
	cancel()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop must exit when ctx is canceled during the sleep")
	}
}
