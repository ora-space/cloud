# Agent Run residual reclamation: Cloud authority and evidence

English | [中文](residual-reclamation.md)

D4/D5 [bounded startup settlement](startup-settlement.en.md) can finish the business Run while retaining unresolved resources in
`agent_run_residuals`. Cloud continues collecting evidence on that original ledger and only grants system cleanup authority when the
evidence qualifies. Deletion follows the existing owned-delete path. It neither settles the [IssueRun](issue-runs.en.md) again nor reopens its Workspace.

This page describes Cloud automatic reclamation and the step 2 controlled-reclamation implementation contract. It is not evidence of physical termination by the Controller or Sandbox Server,
deployment acceptance, or joint fault acceptance. The
[residual reclamation ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261018-residual-agent-run-reclamation.md)
remains `approved`; this step does not declare the whole ADR implemented.

## Ledger states and one durable responsibility

| `reclaim_state` | Meaning | Authority fields |
|---|---|---|
| `reconciling` | Collect original-identity facts; deletion is not yet authorized | ID, mode and operation are NULL; authorization is `{}` |
| `reclaiming` | One durable responsibility has been granted; continue or await evidence | Fixed `reclaim_id`, mode and nonempty authorization; `owned_delete` binds exactly one operation |
| `cleared` | Disposition under the original responsibility is complete | Keep that responsibility, with both `cleared_at` and disposition |

Automatic reclamation uses `owned_delete` and completes as `released`. Explicit operator commands use `controlled`, allowing
`terminated_data_retained` or external disposition of an existing unfinished intent as `externally_confirmed`. Both modes compete for the
same responsibility and cannot overwrite each other. Failures, timeouts and the existence of operator commands do not escalate automatic authority.

Automatic responsibility stays bound to the same `delete_workspace` operation. Restarts, lease changes, retries and quiesce refusals do not
create another responsibility or operation, and do not revive a failed historical operation. Ordinary deletion, dispatch and permit paths retain
residual guards; an internal system responsibility is not a public deletion flag. The transaction rechecks the ledger, Run, Workspace, Project and
operation tenant, original bindings and generation; a UUID foreign key alone does not establish that scope.

The [migrations](../../internal/core/migrations/README.en.md) retain the ledger and add separate intents, command receipts and per-target termination plans. Defaults initialize existing D4/D5 rows as `reconciling`, without
inferring completion, backfilling D8, or changing old `reason`, `responsibility` or business settlement. A legacy row with non-null `cleared_at`
causes an explicit error and rollback pending compatibility handling (A2).

## Original-identity evidence and deletion eligibility

`reclaim_targets` is a union of original plans and fact targets: original operations/effects, preallocated sandbox ensure IDs, sandboxes, Nodes,
executions, clones, work and data. Late facts may add original targets; they cannot replace an old target with the current generation or drop an
unknown target because a query did not find it.

New facts append to `reclaim_evidence` with logical event keys, facts, sources and database receipt times, instead of changing old
`responsibility.lateFacts`. An identical replay reuses its receipt; new evidence, additional targets or authority transitions increment
`reclaim_version`. Evidence intake, authorization, permits and completion use Cloud's existing serialized transaction boundary. Completion must
therefore reread the full target set and latest facts in its own transaction.

System authorization requires located original identities, closed admission, no conflict, no blocking Project operation or other activity, and
qualifying delivery and execution evidence. Execution evidence includes input closure and an explicit unfinished-execution list for the same
runtime control identity, together with original execution terminals. Per-target termination evidence can also establish physical closure.
A no-target proof applies only when original plans and execution relationships truly do not exist; missing rows, NULL results and elapsed time
do not establish that proof.

Automatic reclamation accepts delivery eligibility only through a genuinely registered and verified Revision for the original `deliver_revision` execution;
every other actual delivery execution needs its own completed verification. There is currently no unified existing durable source for explicit
delivery abandonment. `failed`, `skipped`, business timeouts, absent object storage and missing results do not imply explicit abandonment (A1).
Entries with insufficient evidence remain open.

The continuation guard in the
[late-delivery fact takeover ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261022-late-delivery-fact-takeover.md)
also applies to residual rescue, covering both automatic reconciliation (`reconciling` / `reclaiming` / `cleared` ledgers) and the explicit
controlled takeover of ledgerless D8: if the same Issue already has a later-created Run (ordered by `run.enqueued` activity seq), or the
creation-order evidence for the original Run and the Issue's other Runs is missing or ambiguous, the original delivery's late terminal is stored
as declaration and receipt only. No Revision is registered, and the receipt is treated neither as a verified delivery nor as cleanup authority.
Only when the order evidence is unique and no later-created Run exists is it registered under the original delivery's evidence verification rules;
receiving a fact does not itself acquire reclamation responsibility.

Authorized automatic cleanup follows existing quiesce → terminate → cleanup. A quiesce refusal leaves the original operation in `retry_wait`, retaining
the original authority and guards for evidence collection and retry (A3). It neither restores new execution eligibility, escalates to force-stop,
nor creates another operation.
When the existing Controller requests `blocked` for unconfirmed termination, this system responsibility instead persists claimable `retry_wait`;
actual plans and permits still recheck evidence.

A failed `sandbox_terminate` / `workspace_data_delete` effect bound to this responsibility is a retryable observation, not a disposal conclusion.
After fresh evidence, the same effect may advance to success while retaining both observations. Historical provisioning/execution failures,
external identity bindings and successful conclusions remain protected against contradictions.
After completion, the existing effect-result intake only preserves original bound facts or audits conflicts; it cannot advance the operation,
change its version or grant execution authority again.

Every original sandbox/ensure ID requires a corresponding successful termination result containing both `terminated` and `lateEnsureFenced`.
`terminated_at`, a failed ensure, a 404 or elapsed time alone is insufficient. The current safe target can reuse owned delete; an additional
historical ID without both confirmations remains open, without replacing its identity or dispatching another termination effect (A5).
Data-deletion planning, permits and completion recheck target coverage. One transaction confirms successful data deletion, succeeds the operation,
soft-deletes the Workspace and writes `cleared/released`, without changing the Run's phase/status/result or repeating business settlement activity.

## Deployment commands and two-stage disposition

`cloudctl residual` is a local deployment-operations entrypoint using deployment database credentials. The authenticated subject is the
connection's database `session_user`; an optional operator label is self-reported and cannot replace that identity. Credentials never enter
output or audit. There is no new HTTP route, tenant-admin bypass, Gateway JWT privilege or listening port; public operations on hidden Run
Workspaces continue to return 404.

| Command | Scope and required confirmation |
|---|---|
| `list` / `get` | Tenant is required; intersect Project, Run, Workspace, operation or reclaim filters; details expose only operational state, targets, audit and evidence |
| `terminate` | One exact Run/Workspace, reason, idempotency key and explicit impact confirmation; D8 requires its original failed deletion or explicit missing-delete selection; retains data by default |
| `delete-data` | One root intent with successful physical termination and retained disposition, separate reason/key and data-deletion confirmation; no bare-Workspace deletion |
| `confirm-external` | One existing unfinished root or dependent intent, reason/key, explanation, external fact references for every unfinished target and explicit acceptance of remaining responsibility |

`list` items, actual residual ledger rows from `get`, and the resource output when present expose persisted `stop_evidence` as `stopEvidence`; the resource is returned only when no intent is selected. Without evidence the field is `{}`; legacy D8 list items without a ledger also return `{}`, while a synthetic legacy D8 residual in `get` may omit the field. An unconfirmed-stop record retains `kind=stop_unconfirmed`, `source=cloud`, `observedAt`, and the original-identity `executionId`. This only displays an existing observation: it does not prove physical termination, grant termination, deletion, or ledger-clearance authority, or change reclamation responsibility.

Mutations do not accept tenant/Project batch scope. All command keys share one tenant-scoped receipt space. The same key and normalized input
return the original receipt and current progress; a different action, resource, parent intent, reason or confirmation returns
`idempotency_conflict`. Replay creates neither a new responsibility nor repeated external evidence collection.

Each resource has one `terminate` root intent and at most one dependent `delete_data` intent. Each follows
`registered → terminating → succeeded`. The root owns the sole reclamation responsibility; the dependent does not arbitrate again.
Registration, original-identity checks, admission/execution closure and plan persistence commit in one database transaction, without Controller calls inside it.

Each original sandbox/ensure target retains a fixed effect ID and request. Cloud projects controlled plans through existing
`ListForceStops`, `ConfirmForceStop` and `GetEffectPermit` fields. It adds no RPC/proto and writes no synthetic ordinary force-stop history.
Current Controller lease, original sandbox/generation and control epoch still fence permits and confirmations. Every target requires both
`terminated=true` and `lateEnsureFenced=true`; a no-target proof must also establish closed entrypoints and no planned or in-flight responsibility.
Ordinary force-stop completion that bulk-finishes operations or changes business state is not used for controlled reclamation.

After termination, the root remains succeeded and the ledger, when present, becomes `cleared/terminated_data_retained`, retaining data and the
Workspace. A later explicit delete-data command binds the sole `delete_workspace` operation to that same responsibility and starts at cleanup
through existing operation/effect/permit paths. The first root completion, retained history and `cleared_at` never regress or reset.
Only complete physical `removed=true` evidence succeeds the dependent, soft-deletes the Workspace, appends released disposition and updates
the current disposition. Failure resumes the same operation/effect and never revives a historical failed operation.

External confirmation must cover every unfinished target and its data responsibility; references may be tickets or external fact identifiers.
It transfers responsibility for an existing intent and stops new permits. It does not fabricate `terminated`, `lateEnsureFenced` or `removed`,
permit same-volume reuse or authorize data deletion. Missing intents, succeeded intents, empty explanations and missing targets are rejected.
Late physical facts remain evidence; they cannot silently convert external disposition into system-proven success.

D8 with a historical deletion requires the original done Run, retained Workspace binding and failed/node_unavailable delete operation; it never backfills the D4/D5
ledger. Without a ledger, the root owns targets, evidence, conflicts and the new delete-operation binding. The old failed operation, Run terminal
and settlement remain unchanged. Ordinary scans do not create controlled D8 responsibility. Missing ledger rows or retained disposition cannot
bypass admission, Project-operation or data-reuse guards.

When D8 finished before a deletion was declared, the deployment operator must select the variant with exact Run/Workspace IDs and explicit
`--allow-missing-delete-operation`. Only an original `done/workspace_unavailable` Run with a delivery conclusion, no deletion history,
the retained binding and closed admission qualifies. Root and dependent deletion intents retain `legacyDeleteOperationMissing` provenance.
The default command does not expand admission, invent an original operation, or rewrite the business outcome.

## Existing fact intake and conflicts

Cloud uses existing `runtime_ack`, creation-fact, execution-result and Revision intake. This step adds no RPC, proto or Controller discovery
mechanism. After checking the original control epoch/version, Node, sandbox and generation, `runtime_ack` records input closure and the full
`unfinishedExecutionIds` list on the ledger. Session terminals and delivery results are accepted under their original execution/work/resource
identity. A done Run or cleared ledger is not a reason to reject a consistent late fact. Output rescue uses existing Revision verification;
it does not open admission or start a new session or execution.

Consistent replay preserves the original conclusion. A contradictory terminal first appends a conflict audit and sets `reclaim_conflicted_at`;
the audit and block commit before the stable `409 residual_fact_conflict` is returned (A4). Canonical facts, existing `cleared_at` and disposition
remain intact. Further deletion or reuse is blocked; this step provides no conflict-resolution endpoint.

## Reconciliation, recovery and remaining scope

`ReconcileAgentRunResidualsOnce` scans `reconciling/reclaiming` ledger rows independently of whether the business Run is still `releasing`.
Bounded passes rotate by `reclaim_checked_at NULLS FIRST, created_at, run_id`; a checked time is scheduling metadata, not completion evidence.
Each entry rereads targets, eligibility and original bindings. An existing responsibility resumes its original operation, subject to the current
Controller lease and effect permits. Query failures, insufficient evidence and temporary refusals neither clear the ledger nor expand authority.

If a failed create's original effects have no existing active-discovery path, they continue waiting for verifiable original-identity facts (A6).
Cloud's evidence intake and responsibility scheduling do not establish new Controller discovery capability, or production validation of real
containers, late-ensure fencing or physical data deletion.

| Scope | Cloud boundary |
|---|---|
| Step 2: Cloud deployment CLI, controlled intents, retained data, separate deletion and external confirmation | This page describes the direct Cloud contract; passing tests and continuous Cloud-path evidence require actual execution artifacts |
| Step 3: Controller / Sandbox Server physical contracts and per-target evidence | This page states Cloud evidence requirements, not completed physical acceptance |
| Step 4: cross-repository joint fault and recovery acceptance | Incomplete; single-repository implementation or database tests are not a substitute |
| Taking over D8 historical deletion abandonment and residuals without ledger rows | Explicit operator takeover only; no ledger backfill, automatic retry or historical business-result rewrite; real deployment recovery remains step 4 |

See the [core code map](../../internal/core/README.en.md) for implementation entry points. Test results require their execution artifacts;
this page does not claim migration, concurrency or end-to-end tests have passed.
