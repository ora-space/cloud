# Ora Cloud Architecture Overview

English | [中文](architecture.md)

Ora Cloud is the authoritative server for Ora's multi-user collaboration: it holds all the business state of
tenants, members, Projects, runtime Workspaces, Issues, Agent runs, plugins and workflows, and decides who may do
what and what happens next. Actual execution happens on Nodes inside sandboxes, with the Controller carrying work
between Cloud and the Nodes. This document gives the whole picture; each part is expanded in its own document (see
the [documentation map](README.en.md)).

## Participants

```text
Browser
  │  HTTPS, session cookie
  ▼
Gateway ─────────── personal key writes ──────────▶ model-gateway
  │  /api/v1/* + service JWT + user JWT                  ▲
  ▼                                                      │ model requests (short-lived token)
Cloud server ◀──── mTLS gRPC, dialed out ──── Controller │
  │                                              │       │
  ▼                                              │ Substrate effects / Node protocol
PostgreSQL                                       ▼       │
                                   Sandbox Server ──▶ sandbox: Node + Agent
                                                         ├──▶ git-server (Project repository)
                                                         └──▶ object storage (Revision upload grant)
```

- Browsers talk only to the Gateway, which mints two internal JWTs for every request and forwards it to the Cloud
  server.
- The Controller only dials out to Cloud's gRPC control listener; Cloud never connects to the Controller. The Controller
  drives the Sandbox Server to create and delete sandboxes and holds sessions with the Nodes inside them.
- Sandboxes can reach only model-gateway, git-server's repository listener and the object storage public address; they
  cannot reach Cloud, the Gateway or the database directly.

| Participant | Repository | Responsibility |
|---|---|---|
| Cloud server (`cmd/server`) | This repository | The business authority: the public API, SSE invalidation events, the Controller's gRPC contract, every state machine and background loop |
| Gateway (`cmd/gateway`) | This repository | The browser's only entry point: external login (Huawei IDaaS, GitHub), sessions, the proxy |
| model-gateway (`cmd/model-gateway`) | This repository | Custody of personal model keys and forwarding of model requests |
| git-server (`cmd/git-server`) | This repository | Every Project's code hub, authorized per request from Cloud's state |
| PostgreSQL | — | The only authoritative business store |
| Object storage | — | Revision bundles and session JSONL |
| Controller (`ora-controller`) | desktop | Dials out to Cloud only; claims work, performs Substrate effects, holds Node sessions and relays events and commands |
| Sandbox Server | cluster | The Docker implementation of the Substrate: creates and terminates sandboxes per Workspace and deletes data |
| Node (`ora-node`) | desktop | The executor inside the sandbox: clones, plugin installs, Agent sessions, Revision delivery |

## Principles throughout

1. **PostgreSQL is the only authority.** No other process holds a second copy of business state. Every decision is made
   in a short transaction in `internal/core`, and transactions are serialized by one global advisory lock (see
   [Authoritative persistence and the transaction model](core/persistence.en.md)).
2. **Transactions do database work only.** Calls to HTTP, Git, object storage, Nodes and the Substrate happen outside
   transactions; where needed a flow is split into "transaction → external call → transaction", and the second
   transaction re-checks its preconditions.
3. **Persist intent before any side effect.** Every external action is first registered in Cloud as a durable operation,
   effect or execution with a stable idempotency identity; the Controller performs it and reports evidence, and Cloud
   advances state only when it takes that evidence over. Unknown outcomes are reconciled by stable identity, and a timeout
   is never taken to mean "nothing ran outside".
4. **Generations fence latecomers.** Resource versions, the Controller lease epoch, runtime generations, Node instances
   and runtime control epochs are recorded separately, and every write checks the relevant one; an old participant's
   writes are refused even while it keeps running.
5. **Hints are not state.** SSE events and Controller signals are emitted only after commit, at most once, and are not
   persisted; clients rely on re-reading REST and the Controller on periodic claims, so nothing is lost.
6. **Identity only says who; authorization looks at current state.** Internal JWTs and mTLS certificates only say who is
   calling; what they may do is decided by core from current membership, role, ownership and run state.
7. **Each contract has one source.** The HTTP contract is `api/openapi.json`, generated from `router.Routes` and
   `internal/contract`; the Controller contract is `proto/`. The frontend client and desktop's client are both generated
   from them.

## The path of a request

1. The browser calls the Gateway's `/api/v1/*` with its session cookie. The Gateway checks same-origin and the session, drops
   the credential headers the browser sent, mints a service JWT and a user JWT, and forwards to the fixed Cloud upstream
   (see [Gateway](identity/gateway.en.md)).
2. Cloud's route allowlist matches the route, both credentials are verified and the body is decoded strictly (see
   [HTTP API conventions](core/http-api.en.md)).
3. `Store.Public` opens a transaction: it takes the global lock, resolves the user JWT to a user (creating one just in time
   if needed), checks tenant membership and role, handles idempotent replay and runs the business logic.
4. After the transaction commits, the collected Controller signals and SSE invalidation events are emitted and the response
   is returned.

## The lifecycle of a runtime

When a user creates a Project, Cloud creates the Project, its repository Binding, its main Workspace and a `create_project`
operation in one transaction and answers `202`. Then:

```text
Controller claims the operation ──▶ plans a sandbox_ensure effect ──▶ Sandbox Server creates the sandbox
      ──▶ handshake with the Node, reported as registered ──▶ clone execution registered and dispatched ──▶ Node clones
      ──▶ plugin execution registered and dispatched ──▶ Cloud opens admission, marks the Workspace ready and the operation succeeded in one transaction
```

Every step requires Cloud to have taken over the previous step's evidence; stop, restart and delete first prove the Node is
idle and then confirm the old sandbox really terminated (see [Workspace lifecycle](runtime/workspaces.en.md)). A Project has
only one lifecycle operation at a time. Who may use the runtime, how a page holds exclusive control and how an administrator
force-stops are in [Runtime control](runtime/runtime-control.en.md).

## An Agent run

A user `@`-mentions an Agent in an Issue comment and gives it a task:

```text
comment transaction: create the IssueRun (pin the Agent plugin version, the personal model binding)
  ──▶ dispatch loop: create the run Workspace (a disposable isolated Workspace)
  ──▶ once ready: write the Thread's first prompt and release the session work item ──▶ the Node starts the Agent session
  ──▶ Thread: session records stream back to Cloud one by one, and the user can keep appending messages
  ──▶ session ends: release the delivery work item ──▶ the Node uploads the bundle and session JSONL
  ──▶ Cloud checks the objects outside a transaction and registers the Revision inside one ──▶ deletes the run Workspace
```

The execution result and the saved output are recorded separately; startup, delivery and deletion each have a waiting limit;
an unknown outcome only gets an end request and keeps its residual responsibility (see [Agent IssueRuns](agent-runs/issue-runs.en.md)).

## Domains at a glance

| Domain | Core objects | Documents |
|---|---|---|
| Identity and tenancy | Users, identity mappings, tenants, Spaces, members | [Internal credentials](identity/authentication.en.md), [Gateway](identity/gateway.en.md), [Users, tenants and members](identity/tenancy.en.md) |
| Runtime | Projects, Workspaces, operations, effects, sandboxes, Nodes, runtime control, force-stop | [Workspace lifecycle](runtime/workspaces.en.md), [Runtime control](runtime/runtime-control.en.md), [Controller contract](runtime/controller-integration.en.md) |
| Repositories | Repository Bindings, repository Operations, Project repositories | [Git Server](repositories/git-server.en.md) |
| Collaboration | Issues, status columns, comments, activities, interactions | [Issue board](collaboration/issues.en.md), [Issue collaboration](collaboration/interactions.en.md) |
| Agent runs | IssueRuns, run Workspaces, Threads, Revisions, the residual ledger | [Agent IssueRuns](agent-runs/issue-runs.en.md) and the other documents in that directory |
| Models | Personal model connections, run bindings, short-lived grants, the request ledger | [Personal model connections](models/model-access.en.md), [model-gateway](models/model-gateway.en.md) |
| Plugins | The marketplace catalog, space plugin selections, runtime instances, the Agent roster | [Plugin marketplace](plugins/marketplace.en.md) |
| Workflows | Workflow documents, published versions, WorkflowRuns, asset import | [Workflows](workflows/workflows.en.md) |

## Background loops

The Cloud server process runs a set of periodic loops for the progress and settlement that have no external event to follow:

| Loop | Period | Document |
|---|---|---|
| Plugin marketplace catalog sync | `plugins.sync_interval` (5 minutes by default) | [Plugin marketplace](plugins/marketplace.en.md#catalog-sync) |
| Workflow plugin asset import | `plugins.asset_import_interval` (30 seconds by default) | [Workflows](workflows/workflows.en.md#plugin-asset-import) |
| Agent run dispatch, session start, Thread ending, cancellation, delivery give-up, delete re-declaration, delete give-up | 10 seconds | [Agent IssueRuns](agent-runs/issue-runs.en.md#background-loops) |
| WorkflowRun scheduling | 10 seconds | [Workflows](workflows/workflows.en.md#scheduling) |

They all do a bounded read-only scan outside the global lock and then open one short transaction per object that decides again
and transitions with a compare-and-set, so running a pass twice never has a second effect.

## Authoritative decisions live in specs

This repository's documents describe the current code's behavior and conventions; business rules, trade-offs and rejected
alternatives are recorded in [specs' Cloud decision directory](https://github.com/ora-space/specs/tree/main/decisions/cloud)
(ADRs) and the matching core test cases. When a document and an ADR disagree, check the ADR's status first: `approved` means
approved but not necessarily implemented, and `implemented` means the implementation matches it.
