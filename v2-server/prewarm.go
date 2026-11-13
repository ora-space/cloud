//go:build linux

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// nodeIdlePolicy reads how long an idle project Node stays up and how many idle Nodes may stay.
func nodeIdlePolicy(getenv func(string) string) (time.Duration, int, error) {
	timeout, max := agentruntime.DefaultIdleTimeout, agentruntime.DefaultMaxIdle
	if v := getenv("V2_NODE_IDLE_TIMEOUT"); v != "" {
		d, e := time.ParseDuration(v)
		if e != nil || d <= 0 {
			return 0, 0, fmt.Errorf("V2_NODE_IDLE_TIMEOUT must be a positive duration")
		}
		timeout = d
	}
	if v := getenv("V2_NODE_IDLE_MAX"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n <= 0 {
			return 0, 0, fmt.Errorf("V2_NODE_IDLE_MAX must be a positive integer")
		}
		max = n
	}
	return timeout, max, nil
}

func nodeProject(p store.Project) agentruntime.Project {
	return agentruntime.Project{MirrorPath: p.MirrorPath, Commit: p.Commit, SnapshotRef: p.SnapshotRef}
}

// warmTimeout bounds one prewarm; Node itself gives up starting after 20 seconds.
const warmTimeout = time.Minute

// prewarmAtStartup starts Nodes before anyone asks: the Demo first, then the most recently active
// projects up to the idle cap, one at a time so a 4-core demo host is not swamped while it also
// serves the first requests. A restart leaves no Node running, so without this every project's
// first conversation after it would start one. Failures are logged and left to cold start.
func prewarmAtStartup(ctx context.Context, db *store.Store, driver *agentruntime.Driver, demo store.Project, limit int) {
	driver.WarmDemo(ctx, demo.ID, nodeProject(demo))
	recent, e := db.RecentProjects(ctx, limit+1)
	if e != nil {
		slog.Warn("list projects to prewarm", "error", e)
		return
	}
	warmed := 0
	for _, p := range recent {
		if p.ID == demo.ID || warmed == limit {
			continue
		}
		warmed++
		warmProject(ctx, driver, p)
		if ctx.Err() != nil {
			return
		}
	}
}

func warmProject(ctx context.Context, driver *agentruntime.Driver, p store.Project) {
	ctx, cancel := context.WithTimeout(ctx, warmTimeout)
	defer cancel()
	started := time.Now()
	if e := driver.Warm(ctx, p.ID, nodeProject(p)); e != nil {
		if ctx.Err() == nil {
			slog.Warn("prewarm project node", "project", p.ID, "error", e)
		}
		return
	}
	slog.Info("prewarmed project node", "project", p.ID, "duration", time.Since(started).Round(time.Millisecond))
}

// warmCreated prewarms a newly created project's Node in the background; the creation request
// has already succeeded and does not wait.
func warmCreated(ctx context.Context, driver *agentruntime.Driver) func(store.Project) {
	return func(p store.Project) { go warmProject(ctx, driver, p) }
}
