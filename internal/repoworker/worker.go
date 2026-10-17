// Package repoworker drives repository Operations through the Repo Executor (cloud/project sync
// and publish ADRs, executor root ADR D2). It owns everything that crosses a process boundary:
// reading the tracking ref before a sync starts, signing the one-time authorization, calling the
// executor while renewing the lease, and verifying a sync's refs through the ref query before Cloud
// may record success. Every state decision stays in internal/core and is fenced on the lease
// generation; no transaction is ever open while this package waits on the network.
package repoworker

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wanglongan587/cloud/internal/core"
	"github.com/wanglongan587/cloud/internal/repoexec"
)

// Executor is the Repo Executor's task interface as the worker consumes it.
type Executor interface {
	Run(ctx context.Context, task *repoexec.Task) (repoexec.Result, error)
}

// Store is the part of core.Store the worker drives.
type Store interface {
	LeaseRepositoryOperations(ctx context.Context, owner string, limit int, lease time.Duration) ([]core.RepositoryTask, error)
	StartRepositoryOperation(ctx context.Context, id string, gen int64, expectedTracking string, lease, timeout time.Duration) (core.RepositoryStart, error)
	ReleaseRepositoryLease(ctx context.Context, id string, gen int64) error
	RenewRepositoryLease(ctx context.Context, id string, gen int64, lease time.Duration) error
	SettleRepositorySync(ctx context.Context, id string, gen int64, r core.SyncResult) error
	SettleRepositoryPublish(ctx context.Context, id string, gen int64, r core.PublishResult) error
	SweepRepositoryLeasesOnce(ctx context.Context) error
	ExpirePublishApprovalsOnce(ctx context.Context) error
	BeginRepositoryChecks(ctx context.Context, limit int) ([]core.RepositoryCheck, error)
	SettleRepositoryCheck(ctx context.Context, id string, attempt int64, observed string, found bool, failure string) error
	StartCredentialVerifications(ctx context.Context, limit int, lease time.Duration) ([]core.CredentialCheck, error)
	SettleCredentialVerification(ctx context.Context, id string, gen int64, result string) error
	SettleAbandonedGitSubmissionsOnce(ctx context.Context) error
	RevokeGitCredentialsInStoreOnce(ctx context.Context) error
}

// Config wires one worker. Zero durations select the defaults.
type Config struct {
	Store    Store
	Executor Executor
	Refs     core.GitRefReader
	// Key signs execution authorizations. It is distinct from the submission-grant key.
	Key ed25519.PrivateKey
	// Owner names this worker in lease records.
	Owner string
	// Slots bounds concurrent executor tasks; it must not exceed the executor's own slots.
	Slots int
	// Lease is the Operation lease; the worker renews it every third of it.
	Lease time.Duration
	// SyncTimeout and PublishTimeout are the task budgets sent to the executor (executor ADR D4).
	SyncTimeout, PublishTimeout, ProbeTimeout time.Duration
	// Interval is the pause between passes.
	Interval time.Duration
	Log      *zap.Logger
}

// Worker runs passes until its context ends.
type Worker struct {
	cfg   Config
	slots chan struct{}
	tasks sync.WaitGroup
}

// New validates the configuration and applies defaults.
func New(cfg *Config) (*Worker, error) {
	if cfg == nil || cfg.Store == nil || cfg.Executor == nil || cfg.Refs == nil || len(cfg.Key) != ed25519.PrivateKeySize || cfg.Owner == "" {
		return nil, errors.New("repository worker needs a store, executor, ref reader, signing key and owner")
	}
	c := *cfg
	if c.Slots <= 0 {
		c.Slots = 4
	}
	defaults := []struct {
		d   *time.Duration
		def time.Duration
	}{{&c.Lease, time.Minute}, {&c.SyncTimeout, 30 * time.Minute}, {&c.PublishTimeout, 5 * time.Minute}, {&c.ProbeTimeout, 5 * time.Minute}, {&c.Interval, 2 * time.Second}}
	for _, v := range defaults {
		if *v.d <= 0 {
			*v.d = v.def
		}
	}
	if c.Log == nil {
		c.Log = zap.NewNop()
	}
	return &Worker{cfg: c, slots: make(chan struct{}, c.Slots)}, nil
}

// Run performs passes every Interval until ctx ends, then cancels in-flight tasks and waits for
// them to settle. A cancelled sync settles as executor_unreachable (it is replayed); a cancelled
// publish as uncertain — the same outcome a lost worker gets from the lease sweep.
func (w *Worker) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			w.tasks.Wait()
			return
		case <-timer.C:
		}
		w.Pass(ctx)
		timer.Reset(w.cfg.Interval)
	}
}

// Pass runs the sweeps and starts as much work as free slots allow. Tasks run in goroutines the
// worker owns and Run waits for.
func (w *Worker) Pass(ctx context.Context) {
	for name, sweep := range map[string]func(context.Context) error{
		"lease sweep": w.cfg.Store.SweepRepositoryLeasesOnce, "approval expiry": w.cfg.Store.ExpirePublishApprovalsOnce,
		"abandoned submissions": w.cfg.Store.SettleAbandonedGitSubmissionsOnce, "store revocations": w.cfg.Store.RevokeGitCredentialsInStoreOnce,
	} {
		if err := sweep(ctx); err != nil && ctx.Err() == nil {
			w.cfg.Log.Warn("repository worker "+name+" failed", zap.Error(err))
		}
	}
	if free := w.free(); free > 0 {
		tasks, err := w.cfg.Store.LeaseRepositoryOperations(ctx, w.cfg.Owner, free, w.cfg.Lease)
		w.logErr(ctx, "lease", err)
		for _, task := range tasks {
			w.spawn(ctx, func(ctx context.Context) { w.runOperation(ctx, task) })
		}
	}
	if free := w.free(); free > 0 {
		checks, err := w.cfg.Store.BeginRepositoryChecks(ctx, free)
		w.logErr(ctx, "checks", err)
		for _, check := range checks {
			w.spawn(ctx, func(ctx context.Context) { w.runCheck(ctx, &check) })
		}
	}
	if free := w.free(); free > 0 {
		checks, err := w.cfg.Store.StartCredentialVerifications(ctx, free, w.cfg.ProbeTimeout+w.cfg.Lease)
		w.logErr(ctx, "verifications", err)
		for _, check := range checks {
			w.spawn(ctx, func(ctx context.Context) { w.runVerification(ctx, &check) })
		}
	}
}

func (w *Worker) logErr(ctx context.Context, what string, err error) {
	if err != nil && ctx.Err() == nil {
		w.cfg.Log.Warn("repository worker "+what+" failed", zap.Error(err))
	}
}

func (w *Worker) free() int { return cap(w.slots) - len(w.slots) }

// spawn runs fn holding one slot. The slot is taken before the goroutine starts, so a pass never
// starts more tasks than Slots.
func (w *Worker) spawn(ctx context.Context, fn func(context.Context)) {
	w.slots <- struct{}{}
	w.tasks.Add(1)
	go func() {
		defer w.tasks.Done()
		defer func() { <-w.slots }()
		fn(ctx)
	}()
}

// settleContext outlives a shutdown long enough to record what a cancelled task left behind.
func settleContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}
