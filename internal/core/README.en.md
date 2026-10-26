# internal/core: authoritative domain and persistence

[中文](README.md) | [English](README.en.md)

This package owns business state, authorization, transactions and recovery decisions. HTTP and gRPC adapters translate requests into Store calls; see the [document map](../../docs/README.en.md) for cross-module contracts.

## Code map

| File group | Package responsibility | Cross-module documentation |
|---|---|---|
| `store.go`, `public.go`, `control.go` | Injected SQL pool, transaction wrapper, dispatch, idempotency and Fault | [Persistence and transactions](../../docs/core/persistence.en.md) |
| `auth.go`, `tenant.go`, `join.go`, `space*.go` | Verified identity resolution, tenant and membership scope | [Identity and tenancy](../../docs/identity/tenancy.en.md) |
| `node*.go`, `runtime_*.go`, `run_workspace.go` | Node facts, control sessions, fencing and run Workspaces | [Runtime control](../../docs/runtime/runtime-control.en.md) |
| `repository_*.go`, `git_*.go` | Bindings, sync/publish Operations, GitGrant and credential metadata | [External Git line](../../docs/repositories/external-git.en.md) |
| `issue*.go`, `collaboration*.go`, `form_descriptor.go` | Issue aggregates, timeline, interactions and consuming collaboration ports | [Issue collaboration](../../docs/collaboration/interactions.en.md) |
| `agent_run_*.go`, `execution*.go`, `thread*.go`, `business_hooks.go` | Run lifecycle, execution registry, Thread and transaction-scoped settlement hooks | [IssueRun](../../docs/agent-runs/issue-runs.en.md), [Controller](../../docs/runtime/controller-integration.en.md) |
| `agent_run_residual.go`, `agent_run_residual_reclaim.go` | Original identity/target coverage and evidence eligibility, one reclamation responsibility and one owned-delete operation, effect/permit and completion checks | [Residual reclamation](../../docs/agent-runs/residual-reclamation.en.md) |
| `agent_run_residual_fact.go`, `agent_run_residual_loop.go` | Appended facts without rewriting old responsibility, replay and durable conflict blocking; ledger scans independent of Run phase and original-operation recovery | [Residual reclamation](../../docs/agent-runs/residual-reclamation.en.md) |
| `agent_run_controlled_reclaim*.go` | Deployment-operator intents and command receipts, one root responsibility and dependent data deletion, per-target termination and external confirmation, and D8 guards without ledger backfill | [Residual reclamation](../../docs/agent-runs/residual-reclamation.en.md) |
| `task_conversation_*.go`, `thread_preview.go` | Ordered task messages, serial same-task Agent handoff and pause barriers, follow-up prompts frozen from Node visible text, configuration-gap hints and a bounded in-process live preview; initial Agent prompts read only the frozen run input | [Thread](../../docs/agent-runs/threads.en.md), [IssueRun](../../docs/agent-runs/issue-runs.en.md) |
| `revision*.go`, `late_delivery.go`, `session_*.go` | Revision registration, original late-delivery facts and creation-order guard, archives, session listing and statistics | [Revision](../../docs/agent-runs/revisions.en.md), [Session reads](../../docs/agent-runs/session-reads.en.md) |
| `model_*.go` | Personal configuration, frozen bindings, grant digests and request observations; no API Key decryption | [Model access](../../docs/models/model-access.en.md) |
| `plugins.go`, `plugin_*.go`, `workflow_asset_imports.go` | Catalog snapshots with Agent declaration copies, space selection, Node install plans with frozen declaration associations and asset import ledger | [Plugin marketplace](../../docs/plugins/marketplace.en.md) |
| `workflow*.go` | Graphs, published snapshots, admission, run identity and scheduler; WorkflowRun creation shared by Issue confirm/manual and direct starts, the `workflow_invocation_ref` link, and the scheduler's projection of WorkflowRun status onto the linked IssueRun | [Workflows](../../docs/workflows/workflows.en.md), [Issue collaboration](../../docs/collaboration/interactions.en.md) |
| `hub.go`, `signals.go` | Post-commit invalidation and control hints; no authoritative state | [HTTP API](../../docs/core/http-api.en.md) |

## Scheduled-run artifact delivery

`scheduled_artifact*.go` persists declarations, signs grants, verifies objects and settles delivery under
the [verdict/artifact ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md).
Revision and artifact deliveries dispatch independently, with at most one unfinished execution of
each kind; both must settle before the run Workspace releases. Upload grants never outlive the run's
fixed deadline. Expired receipts still acknowledge the original execution without HEAD or registration.
Final registration checks the actual database clock after acquiring the lock, rather than the
transaction start before the lock wait. Batch failure or expiry atomically records rejections for
unregistered files in the frozen manifest, preserving existing collection/verification rejections
and registered successes without adding facts on replay.
Object verification and deletion happen outside transactions. Cleanup derives deterministic keys and
deletes objects absent from the index. Settled runs become eligible for another sweep after one minute,
including successful deliveries whose index retention has pruned. `artifacts_gc_at` records the last
sweep, never permanent completion.

Zero-length files may be declared, uploaded and registered; indices and names are deduplicated
independently. The original session manifest and delivery row preserve `omitted_rejections` and
`walk_truncated`, so a bounded refusal list never implies a complete walk. A declaration containing
only refusals or truncation facts settles immediately without an empty upload or deadline wait.

## Store and transaction boundaries

- `NewStore` obtains `*sql.DB` from injected GORM and wires the real Workflow Directory/Forms; it neither connects nor migrates nor creates a global database.
- `transact` takes `pg_advisory_xact_lock(67420911)` inside PostgreSQL, handles domain Faults and post-commit notifications. Hooks must use the supplied transaction, never start a nested Store transaction.
- `transactWorkflowRun` takes the global lock in shared mode, then this run's exclusive lock on a best-effort basis; once it holds that right it takes the linked Issue's lock so Timeline `seq` writes of one Issue never overlap. Lock order is always global → run → Issue → Project.
- Network, Git, filesystem and object-store calls happen outside transactions; callers recheck identity, version and authorization afterwards. Archive reads, publish-source checks and credential confirmation have dedicated entrypoints.
- `Object` is the boundary JSON value. `Fault` and `ErrorCode` convert expected failures and database errors into safe public errors. Domain panics are converted only at the transaction control boundary; unexpected panics must not be swallowed.
- Commands wire Store ports and policy durations. See [collab](../collab/README.en.md) for development ports; simulated results do not replace physical execution evidence.

See [migrations](migrations/README.en.md) for complete migration filenames. Package `dispatcherDB` fixtures migrate one real PostgreSQL template per process and give each fixture an independent database copy; [integration](../../integration/README.en.md) covers HTTP, Git and storage boundaries.

Residual facts retain their original intake: `agent_run_provisioning_fact.go` handles creation facts, `node_execution.go` and `revision.go` handle
original execution terminals and delivery, and `runtime_control_delivery.go` accepts full runtime evidence through existing `runtime_ack`.
`run_workspace.go`, `control.go` and effect/permit paths enforce the original responsibility binding. No new RPC/proto or public system-delete
flag is introduced. Step 2 uses `cloudctl residual` with deployment database credentials and authenticated `session_user`; an operator label is
self-reported. No HTTP route or tenant privilege is added. One root terminate and at most one dependent delete_data intent each follow three
states. Root completion never regresses; deletion binds one operation and preserves the first `cleared_at`. External confirmation requires an
existing unfinished intent and per-target fact references, without fabricating physical success.
Controlled targets use existing `ListForceStops` / `ConfirmForceStop` / `GetEffectPermit` fields, independently of ordinary force-stop history
and business completion. D8 takeover is explicit and uses original identity without ledger backfill. See the dedicated page for automatic
A1–A6 eligibility, two-stage disposition and recovery guards. This describes the Cloud implementation contract, not passing tests;
Controller/Sandbox physical proof and cross-repository fault acceptance remain steps 3 and 4 respectively.

## Database test fixtures

`fixture_template_test.go` is local to this test package. It creates and migrates a `core_template_` database with a 32-character UUID suffix, closes the migration pool and seals the source. Each fixture receives an independent `core_run_` database with a 32-character UUID suffix. The test role requires `CREATEDB`. Ordinary copies belong to `t.Cleanup`; controlled-reclamation matrices retain their per-case resets, and `TestMain` drops their copy before the template. The template accepts no business writes; fresh Stores retain the existing business hooks. Package shutdown reports cumulative migration and copy time.

`fixture_template_isolation_test.go` covers data, DDL, migration history, later copies and parallel copies, and calls the product's `CheckSchema`. Migration/upgrade tests retain integration's empty/older-schema path rather than substituting the latest template. See the [testing guide](../../docs/development/testing.en.md#cleaning-up-leftover-test-databases) for cleanup after a killed process.

This package mirrors integration's small test helper: its `_test.go` code is not importable, and integration already depends on core. Extracting a shared package would add a callback API and change another fixture implementation. Production code does not depend on these global test states.

`scheduled_run_artifact_api.go` authorises the current member, Issue and Run within a short transaction. Lists return registered metadata only; downloads read through the private endpoint outside the transaction, verify size/SHA-256 under a 10 MiB cap, and recheck access/registration before returning, without accepting caller-selected object keys. Registration/pruning emit transactional invalidation hints, and history reads verdict/counts from independent run facts.

Artifact reads map missing membership to the same 404 as an absent resource. History reports expiry only for settled successful/partial deliveries whose registered index is empty, never from occurrence age or failure.
