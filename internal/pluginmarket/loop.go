package pluginmarket

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

// SleepFunc blocks until ctx is canceled or d elapses, whichever comes first,
// and returns ctx.Err() when cancellation won. It is the injectable clock seam
// behind the sync loop so tests drive ticks deterministically.
type SleepFunc func(ctx context.Context, d time.Duration) error

// SyncFunc is one bounded catalog refresh attempt; production passes
// (*Syncer).Sync.
type SyncFunc func(ctx context.Context) error

// ContextSleep is the production SleepFunc: a plain timer raced against ctx.
func ContextSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RunSyncLoop runs sync once at startup and then once per interval until ctx
// is canceled. It is owned by the process lifecycle: the caller cancels it on
// shutdown and waits for it to return, exactly like gateway.RunCleanup.
// Failures are logged and retried at the next tick; they never stop the loop,
// and a failed sync leaves the previous catalog snapshot in place.
func RunSyncLoop(ctx context.Context, sync SyncFunc, interval time.Duration, sleep SleepFunc, log *zap.Logger) {
	for {
		if e := sync(ctx); e != nil && !errors.Is(e, context.Canceled) {
			log.Warn("plugin catalog sync failed", zap.Error(e))
		}
		if e := sleep(ctx, interval); e != nil {
			return
		}
	}
}
