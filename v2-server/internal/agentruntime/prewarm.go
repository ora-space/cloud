//go:build linux

package agentruntime

import (
	"context"
	"sort"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// Warm starts a project's Node ahead of its first session and returns once it is up or has failed.
// A session arriving meanwhile waits for the same Node instead of starting another. A failure is
// only reported: the next session cleans it up and starts its own Node, as without prewarming.
// The Node then idles under the reclaim policy like any other.
func (d *Driver) Warm(ctx context.Context, id string, config Project) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return execution.ErrUnavailable
	}
	p := d.projects[id]
	if p == nil {
		p = d.newProjectLocked(id, config, true)
	}
	d.mu.Unlock()
	select {
	case <-p.ready:
		return p.initErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *Driver) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Driver) idleTimeout() time.Duration {
	if d.opts.IdleTimeout > 0 {
		return d.opts.IdleTimeout
	}
	return DefaultIdleTimeout
}

func (d *Driver) maxIdle() int {
	if d.opts.MaxIdle > 0 {
		return d.opts.MaxIdle
	}
	return DefaultMaxIdle
}

func (d *Driver) reclaimLoop() {
	ticker := time.NewTicker(min(d.idleTimeout()/4, time.Minute))
	defer ticker.Stop()
	for {
		select {
		case <-d.stopReclaim:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			d.reclaim(ctx)
			cancel()
		}
	}
}

// reclaim stops idle Nodes: each one idle for longer than the idle timeout, then the least
// recently used beyond the idle cap. A Node still starting is not counted, so it is never
// stopped before it could serve. A Node whose stop is unconfirmed stays registered, so no second
// incarnation can start in its directory; the next pass, or the next session, retries the stop.
func (d *Driver) reclaim(ctx context.Context) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	type candidate struct {
		p      *projectRuntime
		usedAt time.Time
	}
	var idle []candidate
	var stale []*projectRuntime
	for _, p := range d.projects {
		select {
		case <-p.ready:
		default:
			continue
		}
		if p.initErr != nil {
			stale = append(stale, p)
			continue
		}
		p.mu.Lock()
		switch {
		case p.fatal:
			stale = append(stale, p)
		case !p.warmDemo && p.idleLocked():
			idle = append(idle, candidate{p, p.usedAt})
		}
		p.mu.Unlock()
	}
	d.mu.Unlock()
	sort.Slice(idle, func(i, j int) bool { return idle[i].usedAt.Before(idle[j].usedAt) })
	cutoff := d.clock().Add(-d.idleTimeout())
	excess := len(idle) - d.maxIdle()
	for i, c := range idle {
		if c.p.retire(cutoff, i < excess) {
			stale = append(stale, c.p)
		}
	}
	for _, p := range stale {
		if p.close(ctx) == nil {
			d.dropProject(p)
		}
	}
}

// idleLocked reports that nobody uses the Node: no workspace owner, no Node command in flight
// (installs, clones and skill changes all go through control) and no caller about to own a
// workspace. Caller holds p.mu.
func (p *projectRuntime) idleLocked() bool {
	return len(p.owners) == 0 && len(p.controls) == 0 && p.pins == 0
}

// retire marks an idle Node unusable so it can be stopped: idle since before cutoff, or evicted
// over the cap. It skips a Node that became busy meanwhile, including one applying a skill change.
func (p *projectRuntime) retire(cutoff time.Time, evict bool) bool {
	if !p.skillsMu.TryLock() {
		return false
	}
	defer p.skillsMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fatal || !p.idleLocked() || (!evict && p.usedAt.After(cutoff)) {
		return false
	}
	// Later acquires see fatal and stop it before starting a new incarnation; skill changes see
	// dead and treat the project as offline.
	p.fatal = true
	p.deadOnce.Do(func() { close(p.dead) })
	return true
}
