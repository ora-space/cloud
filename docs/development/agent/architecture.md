# Architecture — for agents

Dense reference. Goal: let you modify this codebase without breaking its invariants. Read this
before `adding-features.md`. Line refs are pointers, not promises — re-check before relying on them.

## Request path

### Public API (`Action == ""`)

```
HTTP → router.New (one generic handler per Route)
     → verify service JWT (Bearer)              [kind=service, role=gateway]
     → verify user JWT (X-Ora-User-Token)       [claims.Caller == service.Subject]
     → strict JSON decode (UseNumber, no trailing tokens, reject null body)
     → body allowlist: unknown_field / invalid_field_type
     → store.Public(ctx, *core.PublicRequest)
        → transact { pg_advisory_xact_lock(67420911) }     ← global write lock
           → identity()     (auto-create user from source+subject)
           → membership()   (tenant active, member active, user active; some paths admin)
           → GET → readPublic()
           → POST/DELETE → idempotency_records (Idempotency-Key header required)
                         → switch dispatch → raw SQL via *sql.Tx
```

### Internal/control API (`Action != ""`)

```
HTTP → router.New → verify service JWT (kind=service, role=node/controller)
     → store.Control(ctx, *core.ControlRequest) → switch on Action
```

Only `/internal/v1/access` and `/internal/v1/admissions` also verify a user token. Everything else
is service-only.

## The four central types

| Type | File | Role |
| --- | --- | --- |
| `core.Object` | `store.go` | `map[string]any`. Accessors `S/N/B/O`. `N` accepts float64/int64/int/json.Number. |
| `core.PublicRequest` | `public.go` | Method/Path/TenantID/…IDs/Key/Limit/After/Query/GroupBy + Body + Identity. |
| `core.Fault` | `store.go` | `{Code, Status, Params}`. `reject(status,code)` / `require(ok,status,code)` panic it; recovered to JSON. |
| `Route` | `router.go` | `{Method, Path, Action, Fields}`. `Fields` = strict body allowlist. |

## Core conventions (follow exactly)

- **All SQL inside `transact`** — one short tx, global advisory lock serializes writes. No
  cross-request state, no prepared statements.
- **`t.list` / `t.one`** wrap SQL in `SELECT row_to_json(resource) FROM (…) resource` and camelCase
  only the **top-level** keys; nested jsonb keys keep their DB names. Aggregate aliases read as
  their SQL name (`SELECT min(position)` → key `min`).
- **Optimistic concurrency** — `version(o, body.N("version"))`: `v<=0` → 428 `version_required`,
  mismatch → 409 `version_conflict`. Every mutating read-then-write bumps `version=version+1`.
- **Soft delete** — `deleted_at=now()`; every query spells `AND deleted_at IS NULL` explicitly. No
  hard erase of business rows (join tables like `issue_labels` are hard-deleted).
- **Idempotency** — POST/DELETE require `Idempotency-Key`; replay returns stored response, changed
  body → 409 `idempotency_conflict`. PUT is version-guarded instead.
- **`updated_at`** is NOT universal — set it explicitly in each `UPDATE …, updated_at=now()`.
- **Tenant scope** — every query filters `tenant_id=$…`. Board reads are tenant-wide (not
  owner-scoped); saved views are owner-scoped.
- **`validText(s,max)`** trims + rejects empty/over-length (400 `invalid_input`).
- **`validID`** = uuid format check; bad id → usually 404 `not_found` (load) or 400 (body field).

## Errors

Envelope `{code, params, requestId}`. Common codes by status:

- **400** `invalid_json` `unknown_field` `invalid_field_type` `invalid_input` `invalid_status`
  `invalid_priority` `invalid_anchor` `idempotency_key_required` …
- **401** `invalid_service_credential` `invalid_user_credential`
- **403** `membership_required` `user_disabled`
- **404** `not_found` `assignee_not_found` `parent_not_found` `user_not_found`
- **409** `version_conflict` `position_conflict` `status_conflict` `label_conflict`
  `system_status_required` `idempotency_conflict`
- **428** `version_required`
- **500** `internal_error`

## Traps (learned the hard way)

1. **Nested issue paths are swallowed** by `case r.IssueID != "" || HasSuffix(path,"/issues")`.
   Dispatch `/comments` `/labels` `/subscribers` **inside** that case; in `readPublic` put nested
   GETs **before** `case r.IssueID != ""`.
2. **OpenAPI `strings.Contains(path, "/issues")`** matches every sub-route — insert new resource
   branches **before** it, and before the hard-coded `status` enum branch in `inputSchema`.
3. **`validField` is closed-typed** — default = string; non-string body fields 400. Add a `case`
   for arrays (`ids`), objects (`filter`/`properties`), integers (`position`).
4. **Contract test enforces `additionalProperties:false`** — every response field must exist in the
   schema. Adding a response field without updating `openapi.go` fails integration with
   "OpenAPI response mismatch".
5. **Gin static segments beat `:param`** — `/issues/batch` is safe (literal `batch` wins over
   `:iid`); `c.Param("iid")` is empty there.
6. **Migrations can't seed runtime entities** — tenants exist only at runtime; seed per-tenant rows
   lazily (`ON CONFLICT DO NOTHING`) on first read/write.
7. **Don't couple Comment to Run.** A comment is not an interaction and not a run. Adding a
   `run_id`/`interaction_id` column to `issue_comments` breaks the frozen cardinality
   (1 comment → 0..N runs) — use `issue_runs.trigger_evidence_*` for provenance instead ([Issue collaboration ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md) D4).

## Auth model (summary — see ../../authentication.md)

- Two independent credentials on public routes: **service** JWT (`Bearer`, role `gateway`) +
  **user** JWT (`X-Ora-User-Token`, `Caller` must equal service `Subject`).
- `identity()` auto-provisions a `users` row from `(source, subject)` on first sight.
- `membership()` enforces active tenant + active membership + active user; admin paths additionally
  require role=admin.

## Where things are

| Concern | File |
| --- | --- |
| Route allowlist + auth + JSON guard | `internal/api/router/router.go` |
| Public dispatch + shared helpers | `internal/core/public.go` |
| Store / transact / Object / Fault / Migrate | `internal/core/store.go` |
| Control (internal) dispatch | `internal/core/control.go` |
| Issue board + collaboration foundation | `internal/core/issues.go`, `issue_{statuses,comments,labels,subscribers,views,runs,activities,context_refs}.go` |
| Projects / workspaces / operations | `internal/core/{project,workspace,operation,node}.go` |
| Workflows (editor document + its collaboration projection) | `internal/core/workflow.go`, `internal/core/workflow_collaboration.go` |
| OpenAPI generator | `internal/contract/openapi.go` |
| Migrations | `internal/core/migrations/NNNN_*.sql` |

## Collaboration — implemented vs. planned

Issue **collaboration** is **partially implemented**. Wave 3A (the "issue-owned foundation"),
Wave 3B-1 (the "collaboration interaction foundation") and Wave 3B-2 (the "workflow interaction
shell") are **IMPLEMENTED + VERIFIED**; the full Issue Detail projection and real Agent/Team/Workflow
execution have not.

**Implemented (Wave 3A, migration `0010` — formerly `0008`/`0007`; renumbered in the workspace integration):**

- Polymorphic `assignee_type/assignee_id` (user/agent/team) + `project_ref` on `issues`.
- Comment threading (`parent_id`) + author ActorRef (`author_type/author_id`, user/agent/team/system).
- `issue_runs` (executor `agent/team/workflow` + opaque external refs), `issue_activities` (append-only
  timeline projection), `issue_context_refs` (reference-not-copy).
- Shared per-issue timeline `seq` (Option-C `GREATEST(MAX,MAX)+1` under the advisory lock).
- API/contract spine for `GET/POST /issues/{iid}/runs`, `GET /issues/{iid}/runs/{rid}`,
  `GET/POST/DELETE /issues/{iid}/context-refs`.

**Implemented (Wave 3B-1, migration `0011` — formerly `0009`/`0008`; renumbered in the workspace integration):**

- The interaction spine table `issue_interactions` (one row per selected `@` target) + the first real
  end-to-end collaboration chain: directory → picker → mention/task → deterministic context → mock
  execution → run lifecycle → activity → reply comment → timeline. No real Agent/Team/Workflow/Runtime.
- The consuming-side **ports** now exist as Go interfaces — `CollaborationDirectory`, `ContextBuilder`,
  `ExecutionDispatcher`, `ExecutionObserver` (canonical list in
  [Issue collaboration ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md) D1;
  do not trust a partial list elsewhere).
- **Fixture/mock adapters** `FixtureCollaborationDirectory` (in-memory, stable UUIDs),
  `DeterministicContextBuilder` (bounded recent comments ≤10, no AI), and `MockExecutionDispatcher`
  (fixed outputs, behaviour all in the adapter) — wired only in development/demo config, **production
  default off**. The rev.-2 `0008_sim_collaboration_catalog.sql` relational catalog is **superseded**:
  no `sim_*` tables are created and no mock domain tables will be.
- `GET /collaboration/targets?q=` (read-only target projection), `GET /issues/{iid}/timeline` (Comment
  + Activity by shared `seq`), `GET /issues/{iid}/interactions`, and `targets[]` on comment create.
  Run lifecycle `queued→dispatched→running→completed(/failed)`; provenance via
  `trigger_evidence_kind`/`ref_id`; agent/team replies land as `author_type='agent'/'team'` comments.

**Dev fixture provider — how to enable.** The fixture adapters are installed by the single composition
point `collab.WireDevelopmentFixtures(store)` (in `internal/collab/collab.go`), which sets the Context /
Dispatcher / Assist ports and *layers* the Directory: `FallbackDirectory` answers `agent`/`team` from the
in-memory fixtures and delegates every other target type to the store's primary directory, which is
database-backed. It deliberately does not touch Forms — workflow form descriptors come from the
`workflows` table, not a fixture. `cmd/server` gates the helper behind the config key
`collaboration.development_fixtures` (env override `CLOUD_COLLABORATION_DEVELOPMENT_FIXTURES=true`),
**explicitly enabled, production default OFF** — the default `configs/config.yaml` ships `false`, and
the config comment + the server startup log both warn that production must leave it off. `cmd/ora-web`
wires the same helper unconditionally (it is a development/demo edge only). The gate is **independent of
authentication**: enabling GitHub Auth never enables these fixtures, and an auth failure never falls
back to a fixture identity. With the flag off the `@` target discovery API serves real tenant members,
**the tenant's real Space Agents, and the tenant's real workflows** — `SpaceAgentDirectory`,
`WorkflowDirectory` and `WorkflowFormDescriptors` are production adapters, wired by `cmd/server` (the
roster-backed Agent directory, layered in front of the workflow-backed one `core.NewStore` installs), so
an agent and a workflow are both @-able in every deployment. With it on, the same API additionally serves
the fixture Agent/Team targets with stable IDs (`internal/collab`:
`BackendAgentID`/`ReviewAgentID`/`PlatformTeamID`).

**Still schema-ready / not API-implemented:**

- `issue_comments.author_type = system` is CHECK-allowed but nothing writes a `system` comment yet.
- `ConversationTarget` remains **not implemented** (left open by the Issue collaboration ADR).

**Implemented (Wave 3B-2, migration `0012` — formerly `0010`/`0009`; renumbered in the workspace integration):**

- Workflow Form Mode end-to-end: `@Workflow` → `GET /collaboration/forms/{formRef}` → dynamic form →
  optional AI Assist → **explicit Confirm** → one `WorkflowRun` and linked `IssueRun` in the same
  transaction. Linked IssueRuns bypass legacy/mock dispatch. Execution advancement and its Timeline
  projection await scheduler integration.
- New ports `FormDescriptorProvider` + `InputAssistProvider` (both nil ⇒ 503). `FormDescriptorProvider`
  is now the production adapter `core.WorkflowFormDescriptors` (a workflow's Start variables projected
  onto renderable fields); only `MockInputAssistProvider` is still a fixture (deterministic, no LLM).
- `issue_interactions.input jsonb` holds the confirmed form values; the run's effective snapshot stays
  in `issue_runs.input`. `ObserveProgress` → `run.progress`; a workflow's human-readable output is a
  `system` activity, never a `workflow`-authored comment.
- `409 workflow_not_available` is **superseded** — a workflow target records a `mode='form'`,
  `runId=NULL` interaction. Workflow *discovery and form descriptors* are now real (see the Workflow
  module); real workflow **execution** and real AI providers remain **blocked on external design**.
- **Wave 3C Issue Detail** full projection + UI; Timeline pagination/truncation; execution logs; PR
  integration; Notification; Realtime/WebSocket; real runtime/LLM/agent/team/workflow execution — all
  **blocked on external design** (Agent/Team/Workflow internal design = UNKNOWN; Issues constrains only
  the consuming-side contract).

Non-`user` actor refs (`agent`/`team`/`system`) are **opaque UUIDs** this wave — shape-validated, not
resolved. `users` remains the only *resolved* human actor. **`workflow` is not an actor at all**: it
exists only on the collaboration-*target* side (`CollaborationTargetRef`), which is why workflow output
lands as a `system`-authored activity rather than a workflow-authored comment (Issue collaboration ADR D2). The target
contracts live in the [Issue collaboration ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md).

### Collaboration interaction model — quick reference (Issue collaboration ADR D2–D6)

Read the ADR before implementing anything collaborative. The short version:

| Rule | Statement |
| --- | --- |
| `@` means | **Collaboration Target Selection** — never execution. |
| Mention text | Markdown `@x` / `mention://type/id` is **display only**, never a routing protocol. |
| `user` target | **Mention Mode** — produces **no** `IssueRun`. `@Human → IssueRun` is forbidden. |
| `agent` target | **Task Mode** — needs an explicit task. `Comment.body != Interaction.task`. |
| `team` target | **Task Mode** — same Issues-facing contract as an agent; no leader/member/delegation assumptions. |
| `workflow` target | **Configure / Form Mode** — dynamic form from a descriptor; never auto-executed. |
| AI Assist | Suggest → Review → Apply → Confirm → Execute. Never auto-executes. |
| Cardinality | `Comment` ≠ `Interaction` ≠ `IssueRun`; 1 comment → 0..N interactions → 0..N runs. **Never add `Comment.run_id`.** |
| Provenance | Use the existing `trigger_evidence_kind`/`trigger_evidence_ref_id` pair. |
| Timeline | `Comment` + `IssueActivity` in one per-issue `seq` namespace; **strictly separate** from Execution Logs. |
| Fixtures | dev/demo config only, production default off, **no mock domain tables**. |

### Workflow interaction — quick reference (Issue collaboration ADR D7–D9)

| Rule | Statement |
| --- | --- |
| Selecting a Workflow | Loads a `FormDescriptor` and renders a form — **never** executes. |
| `FormDescriptor` | An **Issues-facing rendering descriptor** (`formRef/title/description/fields[]`). It is **not** the Workflow schema and binds to no schema technology. |
| Execution boundary | Exactly one: an explicit **Confirm**. Editing, AI Assist, applying suggestions and draft saves create **no** run. |
| AI Assist | `InputAssistProvider` returns a **field-level patch** + suggested refs. Suggest only — never creates a run, never writes a persistent `IssueContextRef`. |
| Form values | A plain `{fieldKey: value}` object. **Never** Workflow-specific columns. |
| Validation | The frontend is UX only; the Issues API **re-validates on Confirm** against the descriptor of the **version being confirmed** — the document the run will execute, not the live draft. A draft that has moved on from that version is refused (`409 workflow_version_fields_changed`) rather than used to judge values the user was shown. |
| Interaction state | No status column: `run_id IS NULL` = unconfirmed, `run_id != NULL` = confirmed. |
| Workflow output | An **`IssueActivity`** (`actor_type='system'` + run/executor in `details`). **`workflow` is not an `ActorRef`.** |
| Execution ports | Reuse `ExecutionDispatcher` / `ExecutionObserver`; no `WorkflowDispatcher`, no second lifecycle. |
| API surface | `/collaboration/forms/{formRef}`, `/issues/{iid}/collaboration/assist` (stateless, keyed by `targetId`), `/issues/{iid}/interactions/{ixid}/confirm`. The collaboration surface **never** grows workflow CRUD — the Workflow module owns `/workflows/*` as its own resource. |
| Extra error | `409 interaction_not_confirmable` — confirm/assist called on a non-`form` interaction. |
