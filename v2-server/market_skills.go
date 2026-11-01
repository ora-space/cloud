//go:build linux

package main

import (
	"context"
	"errors"
	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/pluginmarket"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"log/slog"
	"os"
	"time"
)

func reconcileSkills(ctx context.Context, db *store.Store, driver *agentruntime.Driver) error {
	if driver != nil {
		online := map[string]bool{}
		for _, id := range driver.OnlineSkillProjects() {
			online[id] = true
		}
		jobs, e := db.SkillJobs(ctx)
		if e != nil {
			return e
		}
		for _, j := range jobs {
			if !online[j.Project] {
				continue
			}
			ok, e := db.StartSkillJob(ctx, j)
			if e != nil {
				return e
			}
			if !ok {
				continue
			}
			e = applySkill(ctx, driver, j)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(e, agentruntime.ErrOfflineSkillNode) {
				continue
			}
			if err := db.FinishSkillJob(ctx, j, skillJobOutcome(e)); err != nil {
				return err
			}
		}
	}
	return db.RefreshSkillStates(ctx)
}

func skillJobOutcome(e error) store.SkillJobOutcome {
	switch {
	case e == nil:
		return store.SkillJobSucceeded
	case errors.Is(e, pluginmarket.ErrTransient), errors.Is(e, execution.ErrUnavailable):
		return store.SkillJobRetry
	default:
		return store.SkillJobFailed
	}
}

func applySkill(ctx context.Context, d *agentruntime.Driver, j store.SkillJob) error {
	remove := j.Desired == "removed"
	version := j.Version
	var file string
	if remove && j.Previous != "" {
		version = j.Previous
	}
	if !remove {
		var creds pluginmarket.Credentials
		if len(j.Credentials) > 0 {
			var e error
			creds, e = pluginmarket.OpenCredentials(j.Credentials, j.Space+"/"+j.Namespace+"/"+j.Retrieval.Endpoint+"/"+j.Retrieval.Bucket)
			if e != nil {
				return e
			}
		}
		var e error
		file, e = pluginmarket.Download(ctx, j.Locator, j.Digest, j.Retrieval, creds)
		if e != nil {
			return e
		}
		defer os.Remove(file)
	}
	return d.ApplySkill(ctx, j.Project, j.Namespace+"/"+j.Identifier, version, file, j.Digest, remove)
}
func runSkillReconciler(ctx context.Context, db *store.Store, d *agentruntime.Driver) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		if e := reconcileSkills(ctx, db, d); e != nil && ctx.Err() == nil {
			slog.Warn("skill reconciliation failed", "error", e)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
