# repoworker: driving repository Operations through the Repo Executor

[中文](README.md) | [English](README.en.md)

Cloud's worker that drives repository Operations (the cloud sync and publish ADRs, executor root ADR D2). It owns everything that
crosses a process boundary: reading the tracking ref before a sync starts, signing the one-time authorization, calling the executor
while renewing the lease, and verifying a sync through the ref query before Cloud may record success. **Every state decision stays
in `internal/core`**, fenced on the lease generation, and no transaction is ever open while this package waits on the network.

| Files | Responsibility |
|---|---|
| `worker.go` | `Config`/`New` (validation and defaults: Slots 4, Lease 1m, Sync 30m, Publish 5m, Probe 5m, Interval 2s), `Run` (passes every Interval; on exit it cancels in-flight tasks and waits for them to settle), `Pass` (four sweeps, then leasing Operations, checks and credential verifications while free slots allow), `spawn` (takes the slot before starting the goroutine, so a pass never exceeds Slots), `settleContext` (a 15s background timeout outliving shutdown, used to record where a cancelled task went) |
| `operation.go` | `runOperation`, `authorize`, `renew`, `settleSync`, `settleCallFailure`, `settleLost`, `syncVerdict`, `publishVerdict`, `runCheck`, `runVerification` |

## Rules that matter

- **The expected old value is read as late as possible**: a sync reads the tracking ref immediately before `StartRepositoryOperation`
  (sync ADR D2) and never from the snapshot record, which may lag; if it cannot be read the lease is released and nothing starts.
- **Renewal is ownership**: the lease is extended every `Lease/3`; losing it (`core.IsLeaseLost`) cancels the task at once — the sweep
  has already ended that Operation and the Git Server refuses its ID anyway.
- **Success is verified first**: after a sync reports `applied`, both the snapshot ref and the tracking ref must name the reported
  commit before Cloud records `succeeded`. Query failures are retried with backoff inside the lease (1s up to 15s); when the lease
  lapses nothing is recorded and the sweep replays the sync.
- **Settlement follows whether the task could have run**: a refusal (429 `executor_busy`, 403 identity) or an undelivered task
  certainly did not run, so it is `failed`; a lost connection may have run, so a sync is `failed` (its writes are idempotent and it
  is replayed) while a publish becomes `uncertain` and is never resent blindly.
- **An uncertain publish is reconciled by one probe**: `runCheck` reads the target ref once with a `probe` authorization;
  `runVerification` proves a credential version's push capability through the `receive-pack` advertisement, settling `verified`,
  `push_denied` or `verification_failed`.

## Tests

`harness_test.go` supplies fakes for `Store`, `Executor` and `core.GitRefReader` (with UUID fixtures, because the authorization the
worker signs is validated against the same rules the executor applies). The remaining tests drive `runOperation` / `runCheck` /
`runVerification` and `Pass` / `Run` directly: deterministic, with no PostgreSQL, and waiting only on event channels rather than
fixed sleeps. `task test:race` covers the concurrency and lease paths.
