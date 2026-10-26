# Runtime Use Permissions, Exclusive Control and Force-Stop

English | [中文](runtime-control.md)

Members of a tenant share its Projects, but what is inside a runtime Workspace — code, files, running
tasks — belongs to a few people, and only one page may operate it at a time. This document covers
three things: who may use a runtime, how a page acquires and gives back exclusive control, and how an
administrator force-stops a runtime.

Every decision is made in a Cloud PostgreSQL transaction. The Controller only delivers the bindings
Cloud has already decided to the Node, and the Node checks them again at its real execution entry
points, refusing stale control.

## Who may use a runtime

| Person | Sees | May use (read content, operate) |
|---|---|---|
| The runtime's creator (`creator_user_id`) | Full content | Yes, still needs to hold control |
| A current administrator | Full content | Yes, still needs to hold control |
| Any other current member | A safe overview: state, generation, creator and so on, with `canUse=false` | No, `403 runtime_use_forbidden` |

- Only the **current** membership state and role count. Neither the Project owner nor the actor of some
  past operation gains use of the runtime's content from that; once an administrator is demoted, even
  replaying the idempotent response of a request they once made successfully is re-authorized and
  refused first.
- The creator comes from the creating request itself (`creator_evidence=verified_request`); historical
  data was backfilled from a uniquely matching creation operation (`creation_operation`). Historical
  runtimes whose creator cannot be determined (`unknown`) can be used by administrators only.
- There is no per-member sharing grant and no real-time co-editing.

## Exclusive control

Each Workspace has one `runtime_controls` row, and at most one page session holds it at a time. Before
a page performs a write (start, stop, restart, delete, and the executions that follow) it must acquire
control:

| Endpoint | Purpose |
|---|---|
| `GET .../workspaces/:wid/control` | Show occupancy: state, control epoch, holder, expiry. Never returns the session ID |
| `POST .../control/acquire` | Acquire control. Cloud allocates a new `sessionId` returned only to this page and increments `control_epoch` |
| `POST .../control/renew` | Renew; needs the original `sessionId` |
| `POST .../control/release` | Give control back |

**States.**

```text
          acquire                     Node confirms the binding
idle ───────────────▶ acquiring ───────────────────▶ held
 ▲                        │                            │  release / expiry / membership lost / Node closed input
 │                        └──────────────┬─────────────┘
 │                                       ▼
 │      every responsibility settled  draining ──(force-stop)──▶ reconciling
 └──────────────────────────────────────┘
```

- **Acquire**: the caller must be allowed to use the runtime, control must be idle, and the runtime
  must be `ready` or `stopped` with no unfinished activity. When the runtime has a live sandbox the
  state becomes `acquiring` and turns into `held` only after the Node confirms the binding through the
  Controller; a stopped runtime with no sandbox goes straight to `held`. The same user on a second page
  must acquire again; sessions are never shared.
- **Lifetime**: 60 seconds, judged by the PostgreSQL clock; the contract has callers renew every 20
  seconds. Renewal only extends a `held`, still-valid control. The Web frontend's runtime control
  interaction is delivered by a separate ADR (approved) and is not wired in yet.
- **Giving back**: on release, expiry (the page stopped renewing), the holder no longer being a current
  member or losing use of the runtime, or the bound Node going away, control becomes `draining` and new
  writes are closed. A Node reporting its input closed also moves control to `draining`, even if the
  database deadline has not passed yet; a late renewal cannot revive it.
- **Back to idle only once settled**: `draining` returns to `idle` only when the old entry points are
  closed (the Node confirmed its input closed, or the runtime is stopped with no sandbox) and there is no
  unfinished execution, clone, ordinary operation, force-stop or residual responsibility. "The
  connection dropped" or "time passed" alone is never evidence.
- An idempotent replay returns the historical response but never revives withdrawn control. Every state
  change is written to `runtime_control_events`, keeping the holder and the original session.

**Lifecycle operations share the same write boundary.** As soon as a lifecycle operation is
registered it puts control into `maintenance` (recording the operation ID and incrementing the control
epoch), so page sessions cannot operate meanwhile. When the operation ends, control becomes `draining`
and settles by the rules above. After a run Workspace's creation operation succeeds, control becomes a
`maintenance` owned by the run that created it — the reservation records the run id, and both IssueRuns
and WorkflowRuns count — which bounds its sessions and deliveries from then on.

An administrator's ordinary operations cannot take over control someone else holds; force-stop is the
way to interrupt immediately.

## Delivery to the Node: bindings and permits

Cloud records who has control now, but writes happen on the Node, so control has to reach the Node and
be re-confirmed before every execution. This is part of the Controller's RPCs (see
[Controller integration](controller-integration.en.md)):

- **Bindings (`ListBindings`, `AcknowledgeBinding`).** The Controller reads every non-idle control,
  delivers it to the corresponding Node, and reports the Node's acknowledgement back to Cloud: whether
  the control is bound to this sandbox, whether the Node's input is closed, and which executions are
  still unfinished. A binding carries the tenant, Workspace, sandbox, runtime generation, Node host
  incarnation, control epoch and Controller lease epoch; the executing side checks scope against them
  and never accepts a member identity claimed by a caller.
- **Execution permits (`GetExecutionPermit`).** Before dispatching a clone, plugin install or Agent
  session execution, the Controller must obtain a fresh permit for that one execution: control is in
  the `maintenance` of that operation (or run) with a confirmed binding, and the execution belongs to
  that operation, targets the current Node, has no result yet and was not force-stopped. A permit is
  never revived by an idempotent response.
- **Effect permits (`GetEffectPermit`).** Dispatching a Substrate effect likewise requires a permit,
  valid for at most 10 seconds and never beyond the Controller's lease.

The Node checks the binding both before it durably accepts an execution and before its first real
change, refusing stale control; when a reply is lost it still reconciles by the stable execution ID.
An expired Controller lease cannot be extended by a cached permit.

## Force-stop

`POST /api/v1/tenants/:tid/workspaces/:wid/force-stop` lets an administrator terminate a runtime
immediately, even while an ordinary operation is under way or a page is using it.

- Only current administrators may call it. The body must carry `reason`, the current `version` and
  `impactConfirmed: true`, with an `Idempotency-Key`. Callers must state clearly that all work on the
  target will be interrupted and unsaved data may be lost.
- A force-stop is an **independent durable intent** (`runtime_force_stops`). It never reuses or deletes
  an ordinary operation and never fabricates its results. At registration every unterminated sandbox —
  including ones still being allocated — is chosen as a target, admission is closed and control becomes
  `reconciling`. A Workspace has at most one unfinished force-stop at a time (`409 force_stop_pending`);
  meanwhile new lifecycle requests and effect plans are refused.
- The Controller reads the plans through `ListForceStops`, obtains an effect permit and terminates each
  target, and reports with `ConfirmForceStop`. Only evidence with `terminated=true` and
  `lateEnsureFenced=true` is accepted: the executing side must first persist a runtime generation fence
  and a sandbox tombstone so a late creation request cannot start another sandbox.
- Once every target is confirmed, one transaction marks unfinished executions as terminated by the
  force-stop, ends the Node instances, terminates the sandboxes, fails in-flight ordinary operations
  with `administrative_force_stop` (the result references the force-stop; the original input, actor,
  execution identity and any real result are kept), and marks the Workspace stopped. **The data volume
  is kept**, and other runtimes' content is untouched.
- `GET .../force-stop` returns the latest force-stop. A historical runtime whose target cannot be
  determined (no live sandbox and no known creator) returns `409 force_stop_target_unknown`.

## Entry points not yet open

Cloud file, terminal and full Agent product entry points are not open; they must first join the same
use-permission, exclusive-control and execution-permit boundaries. Real plugin execution is already
done by the Node (see [Plugin marketplace](../plugins/marketplace.en.md)). Nodes talk only to the Git Server and use no
repository credential; the external repository is synced by the Repo Executor outside this repository (see
[Workspace lifecycle](workspaces.en.md#public-workspace-endpoints) and [Git Server](../repositories/git-server.en.md)).

## Related decisions

- [Runtime content of shared projects is usable only by its creator and current administrators](https://github.com/ora-space/specs/blob/main/decisions/cloud/workspace/20260927-runtime-workspace-use-permissions.md)
- [Runtime control binds to an exclusive session; hand-over waits for old entry points to close and responsibilities to settle](https://github.com/ora-space/specs/blob/main/decisions/cloud/execution-admission/20260927-exclusive-runtime-control.md)
- [Ordinary stop protects activity; administrative force-stop records independent intent and waits for real termination](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260927-safe-stop-and-administrative-force-stop.md)
- [Runtime control is authorized by Cloud, delivered by the Controller, and fenced at the Node's real entry points](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md)
- [Execution admission follows Space membership](https://github.com/ora-space/specs/blob/main/decisions/cloud/execution-admission/0-access-follows-space-membership.md)
- [Runtime control's frontend interaction and joint fault acceptance are delivered separately](https://github.com/ora-space/specs/blob/main/decisions/cloud/execution-admission/20261013-runtime-control-ui-and-fault-acceptance.md) (approved)
