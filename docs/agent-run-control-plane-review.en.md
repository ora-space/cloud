# Agent control plane scope and acceptance (cloud#41 continuation)

English | [中文](agent-run-control-plane-review.md)

A's Cloud control loop is implemented and directly exercised with PostgreSQL, Git and RustFS. The user authorized the current proposed ADRs; their status remains unchanged. Cloud #41 is already merged. A new upstream PR submits the subsequent Revision delivery and recovery changes and synchronizes current main. A does not complete cluster#5/#7 or M1–M4.

Sources: [specs#58](https://github.com/ora-space/specs/pull/58), [cluster#5](https://github.com/ora-space/cluster/issues/5), [cluster#7](https://github.com/ora-space/cluster/issues/7), [specs#66](https://github.com/ora-space/specs/pull/66).

## A obligations and evidence

| Obligation | Implementation and direct evidence |
|---|---|
| gRPC, registration and recovery | Frozen plugin/session/delivery input, current Node/sandbox, leases and permits; control/boundary tests; `registeredDelivery` asserts delivery pending recovery |
| Thread and commands | Ordered atomic takeover, receipts, watermark, terminal order, durable command order and idempotent delivery; echo restart/end and Thread hook rollback tests |
| Work and grants | Cloud-chosen per-work keys, new keys on failed retry, unchanged input on refresh; optional v1 `checksums` binds SHA-256 to PUT signatures; retry, checksum and expiry tests |
| Storage and Revision | Private S3 HEAD outside SQL, checking existence, size and stored SHA-256; recheck lease, input and state, then atomically store raw Node evidence, verdict, Revision and hook writes; `TestRevisionS3VerificationAndAtomicTakeover`, network and fencing tests |
| Plugins and run Workspace | Frozen plan, complete per-item results, new execution on failure, blocked unknown result; hidden public start/stop/delete APIs; `TestStartPluginPlanSurvivesSelectionChangesAndExecutionFailures`, `TestRunWorkspaceAndPluginStep` |
| Five hooks and four helpers | Caller-owned transactions; ready/failed, Thread, sessionEnded, delivery and deleted tests directly cover success, failure, duplicates and rollback |
| Go doubles | Real Git push to the delivery ref, sealed JSONL, upload, expired-grant refresh, both doubles restarting after upload, terminal replay and Revision retention after Workspace deletion; simulator delivery and Git snapshot tests |
| Forward migration | Only new 0027; fresh schema and repeated 0023/0024/0026 upgrades preserve historical effects, input, commands, Node results and receipts without inventing Revisions |
| Cluster acceptance | Persistent RustFS, credential files, idempotent bucket creation, private/public endpoints and independent sandbox upload; mandatory PostgreSQL/S3/sandbox acceptance task |

## Contracts and implementation boundaries

`enqueueExecutionWork`, `enqueueThreadCommand`, `createRunWorkspace` and `deleteRunWorkspace` use the caller's transaction. All five named hooks receive the same sql.Tx and must not open another Store transaction or perform external I/O. Delivery settles `saved`/`unchanged` with `revisionId` and metadata, `failed` with a safe reason, or once-only `skipped/object_store_unconfigured` without dispatch. Hook failure rolls back business and control evidence together.

Object mismatch preserves the original Node success declaration and records a separate Cloud `verification_failed` verdict without a Revision. Network failure produces no ACK and remains replayable. Committed replay avoids S3. The temporary `revision_verification_required` rejection is replaced.

Code is delivered through the Git Server and commit/JSONL content semantics belong to Node; Cloud only checks through the Git Server ref query that the delivery ref names the declared commit and HEADs the JSONL, and does not download or interpret them. Real Git doubles prove cumulative restoration, not a real D Agent. A's schema lacks B's delivering/releasing phases: A fences against registered delivery, ended session, nonterminal run, maintenance binding, open admission and current Node. B owns phases through the hooks.

Grants stay in memory, never databases, journals, files, environment or arguments. The checksum map only affects ephemeral signing; send every returned signed header. Health reports database reachability and object store and Git Server configuration, not S3 or Git Server availability or configuration secrets.

## Validation and remaining work

The 2026-09-30 review adds signed `If-None-Match: *` to every PUT so an unexpired grant cannot replace a verified object. A 412 still requires Cloud verification. Controller cancellation leaves the original delivery replayable. Failed plugin items release their maintenance bindings; a late failure from an older revision returns the latest selection to pending without retaining the old error. Recovery tests compare the complete frozen protobuf input through both pending reads and GetDispatch. Direct regressions are `TestRevisionStoredObjectsRejectLiveGrantOverwrite`, `TestPluginFailedResultReleasesMaintenanceForNewSelection`, `TestRevisionUploadReconcilesAnAmbiguousCreatedObject` and the extended restart delivery test.

Object bytes and declarations are frozen durably before the first PUT. Partial-upload restart reuses the original snapshot; artifact copies are removed after terminal evidence is atomically saved. The extended test cancels after a real RustFS PUT, replaces both doubles and changes checkout contents to prove replay still delivers the original snapshot. Historical pending journals with `Result: null` remain replayable, and grants never enter disk state.

Direct acceptance on 2026-09-30 used PostgreSQL 17 and `rustfs/rustfs:latest`, digest `sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff`. Final format/lint, full PostgreSQL+S3, race, build and proto/OpenAPI/frontend drift results are recorded on the PR. Frontend checks cover 53 files and 300 tests.

B still owns phases, Thread projection/API/SSE, Git identity and UI. C still needs production Rust Controller/Node relay, grants, logs/ACK and recovery acceptance. D still needs real Agent/plugin execution and delivery. The desktop companion only pins Cloud, generates protocol and tests compatibility. specs retains these Partial obligations and unchanged ADR status.
