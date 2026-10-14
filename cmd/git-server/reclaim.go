package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wanglongan587/cloud/internal/mirror"
	"github.com/wanglongan587/cloud/internal/mirrorpublish"
)

// runMirrorReclaim is the reclaim loop: it reconciles the mirror store against Cloud's own rows and
// removes what nothing references any more.
//
// It runs in this process because the two halves it needs are otherwise in different ones. The sweep
// is against the mirror volume, which only this process and the credential-executing identity C
// mount; the reference set is Cloud's rows, which this process reads for the read authorization and C
// cannot read at all. Moving the loop to C would mean handing the credential-executing identity a
// database connection it has no other use for, and moving the volume to Cloud would put the content
// sandboxes clone inside the process that decides what they may clone — the separation the read path
// exists to keep. So the loop lives where both halves already are.
//
// What it does not do is publish. In this channel a mirror is fetched by C under a single-use
// authorization Cloud signs for one Operation, and a reconciliation pass is not an Operation; the
// per-Operation worker remains the only publisher (see `mirrorpublish.Reconciler.Maintain`). This
// pass therefore only withdraws and sweeps, and it can remove nothing Cloud still references: the
// reference set it sweeps against is Cloud's own answer, not the store's records.
//
// It returns when the context is cancelled, which is what stops it on shutdown.
//
// The logger is taken as the key/value shape the reconciler writes rather than as *zap.Logger, so
// this loop reports into the process's one log stream: `logger.Values` adapts the configured zap
// logger to it (see internal/logger), and nothing here opens a destination of its own.
func runMirrorReclaim(ctx context.Context, pool *sql.DB, mirrors *mirror.Store, interval time.Duration, log mirrorpublish.Logger) {
	reconciler := mirrorpublish.Reconciler{
		Mirrors:  mirrors,
		Bindings: &mirrorpublish.References{Pool: pool},
		Logger:   log,
	}
	for {
		report, err := reconciler.Maintain(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			return
		case err != nil:
			// One failed pass is not a failed loop: Cloud's database may be between connections and
			// the next pass is a fresh question. Nothing is removed by a pass that could not read the
			// references — Maintain returns before the sweep, exactly as it does for Reconcile.
			log.Warn("mirror reclaim pass failed", "error", err.Error())
		default:
			// Logged only when it did something. A reclaim is the event an operator has to be able to
			// find afterwards, and a pass that removed nothing is not one.
			if reclaimWorthReporting(&report) {
				log.Info("mirror reclaim pass",
					"referenced", len(report.Ready), "revoked", len(report.Revoked),
					"destroyed", len(report.Sweep.Destroyed),
					"observedUnreferenced", len(report.Sweep.ObservedUnreferenced),
					"stagingRemoved", report.Sweep.StagingRemoved,
					"orphansRemoved", report.Sweep.OrphansRemoved)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// reclaimWorthReporting decides which passes are announced, and it is a function of the report rather
// than a condition written into the loop because it is the loop's whole outward behavior: a pass it
// does not report is a pass nobody can see, and this is the only thing the loop says.
//
// The first-time observations belong here with the removals. A mirror that has just become
// unreferenced is the beginning of a countdown to its deletion, and if the pass that starts that
// countdown is silent then the only report an operator ever gets is the deletion itself — with no way
// to tell a mirror that spent its grace period from one that was destroyed on the pass after it was
// last read. It also cannot be noisy: the sweep reports an observation once per key, on the pass that
// first noticed it, so a deployment that released ten mirrors logs ten lines and then goes quiet
// again — which is exactly the state the steady-state loop is in.
// It takes the report by pointer only because the report is large and the caller already owns one;
// nothing here writes through it.
func reclaimWorthReporting(report *mirrorpublish.Report) bool {
	return len(report.Revoked) > 0 ||
		len(report.Sweep.Destroyed) > 0 ||
		len(report.Sweep.ObservedUnreferenced) > 0 ||
		report.Sweep.StagingRemoved > 0 ||
		report.Sweep.OrphansRemoved > 0
}
