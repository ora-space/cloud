# internal/cloneexec: the credentialed-clone executor

[English](README.en.md) | [中文](README.md)

`internal/cloneexec` is **component C as a deployment unit of the current product**: it turns one
Cloud-issued single-use authorization into one `git clone` of a private repository, and then makes the
credential that clone used impossible to recover. It carries out D1, D2, D5, D6, D7, D11 and D13 of
`specs/decisions/node/repository/20261010-controlled-node-git-authentication-isolation.md`, together
with the follow-up decision that reassigns C's implementation to the current product,
`specs/decisions/node/repository/20261012-credential-executor-belongs-to-the-current-product.md`.

The approved decisions fix three identities on the machine that runs a clone — M for management, W for
the Agent workload, and C for credential execution — and require the token to exist only under C, in a
directory W cannot traverse, for no longer than the clone. This package is C, built as **its own
process, with its own identity, its own volume and its own listener**, so that boundary is an
operating-system boundary between two processes rather than a rule inside one.

It runs no Agent code and holds no Agent state; between jobs it holds nothing at all. It shares no
filesystem with the Node's workload today, so the in-filesystem W/C exclusivity comes from the
deployment — recorded honestly as such in
`specs/test-cases/node/repository/git-credential-execution-isolation.md` — and is not something this
package can prove about itself.

C carries **two capabilities on two routes** today: `POST /v1/clone-executions` turns an
authorization into one `git clone`, and `POST /v1/mirror-preparations` turns one into one internal
mirror. They **share** the deployment, the verification, the redemption and the way Git is invoked,
and they **share nothing else** — not a route, not a request type, not a result type, not a consumer.
There is deliberately no `kind` switch on one job; see "Two routes, not a switch" below.

## The order is the contract

`Executor.Execute` (`executor.go:317`) is five steps, and the order is itself an invariant:

1. **Verify locally, spending nothing.** Verify the authorization against the deployment's verification
   key, then compare every binding that can be compared for free — the execution, the repository, the
   runtime fencing values (`matchBinding`, `executor.go:439`). A job that fails here has consumed
   nothing.
2. **Redeem as `broker-resolver` at the Credential Service.** This is where the authorization is spent,
   and the Store spends it **durably and exactly once** (`Resolve` in `storeclient.go`).
3. **Materialize.** One directory named for the execution under the materialization root (0700),
   holding one file of mode 0600 (see `internal/credentialmaterial`).
4. **One constrained `git clone`.** Its only credential helper is the deployment-fixed program, and
   that program reads that one file (`cloneArguments` in `git.go`).
5. **Destroy and prove it gone**, on **every** path out of steps 3 and 4 — success, Git failure,
   timeout, cancellation, panic (the `defer` in `Execute`).

The ordering of 3 to 5 keeps the material's lifetime inside the job's: created only after the Store has
decided to deliver, destroyed before the result is returned. A crash is the one case this ordering
cannot cover, which is what the startup sweep is for (`Sweep`, `executor.go:304`) — it runs before the
listener opens.

## Two routes, not a switch

The two capabilities have a route, a request type and a result type each, and they are two exported
methods rather than a switch on one:

| | Clone | Mirror preparation |
| --- | --- | --- |
| Route | `POST /v1/clone-executions` | `POST /v1/mirror-preparations` |
| Request / result | `Job` / `Result` | `MirrorJob` / `MirrorResult` |
| Authorization operation | `clone` | `mirror_prepare` |
| Subject | one execution's checkout on one Node | one attempt of one Project Operation |
| Identity fields | `executionId`, `nodeId`/`nodeInstanceId`/`nodeIncarnationId` | `operationId`/`preparationGeneration`/`executorIdentity` |
| Cloud-side method | `Client.Dispatch` | `Client.PrepareMirror` |

The subject is what forces it: a clone binds "this execution on that Node" and a mirror preparation
binds "this attempt of that Project Operation", and the two sets of identity fields are **disjoint** —
`Binding.validate` requires the three node ids of a clone and **forbids** operation/generation/
executorIdentity, and requires those three of a preparation and **forbids** the node ids. Folding both
into one `kind`-tagged job would leave a decoder deciding which identity fields are meaningful, and the
operation boundary would become a branch inside C instead of a signature Cloud cannot forge (the
2026-10-13 ruling).

**Purpose confusion has to be refused in both directions**, and the rule is a comparison of the
**operation inside the signature**, not of the route the request arrived on:

- a `clone` authorization at the mirror route → 400 `binding_mismatch` (the first check in
  `matchMirrorBinding`), and it happens **before the Store is asked for anything**;
- a `mirror_prepare` authorization at the clone route → 400 `binding_mismatch` (the same check in
  `matchBinding`).

In neither direction is "it was not consumed" taken on C's word: it is shown by redeeming the
authorization afterwards through **the route that does own it** (see "Tests").

## The order of a mirror preparation

`MirrorExecutor.Prepare` (`mirror.go:264`) is the same "the order is the contract", with a publication
instead of a checkout at the end:

1. **Verify locally, spending nothing.** Canonicalize the repository, check
   `operationId`/`preparationGeneration`/`projectId`/credential id, validate the ref with the issuer's
   own `cloneauth.ValidateRef`, verify the signature, then `matchMirrorBinding` — whose first check is
   the operation. A job that fails here has consumed nothing.
2. **Redeem as `broker-resolver`.** `Store.ResolveMirror` decides the purpose first on its own side
   (a mismatch is 403 `authorization_purpose_mismatch`) together with the presenting identity, and only
   then spends the authorization durably and exactly once.
3. **Materialize** under the **authorization's** identifier (`mirrorMaterialIdentity`) rather than the
   operation's: an authorization is spent exactly once, so two attempts of one operation — including an
   attempt Cloud has already retried while this one was still running — cannot collide on one directory.
4. **One constrained fetch**: `init --bare`, fetch the one signed ref, read the commit back out with
   `rev-parse --verify --quiet refs/ora/mirror^{commit}`, assert with `for-each-ref` that the repository
   exposes **exactly** `refs/ora/mirror` (`mirrorpublish.MirrorRef`), and `symbolic-ref HEAD
   refs/ora/mirror`.
5. **Publish and destroy.** `Preparation.Begin`, an already-published commit returns its existing record
   (idempotence by commit), otherwise `Adopt` moves the fetch into the store's staging area and
   `Publish` publishes it. The material's destruction and the fetch directory's removal run on **every**
   way out, and a failed destruction is reported even when the publication succeeded — a preparation
   whose credential may still be on disk is not a successful preparation.

The preparation's timeout is the lesser of the deployment's value and the authorization's remaining
lifetime, so an authorization about to expire cannot let a fetch run past it.

## Files

- `executor.go`: `Config`, `Executor`, `New`, `Sweep`, `Execute`, `Job`, `Result`, `Runtime`, `Failure`
  and the seven reason codes.
- `git.go`: the whole construction of a constrained Git invocation, and the one Git environment
  **both** paths run with — `gitDeployment` (so the two cannot drift apart on which helper runs or
  which CA is trusted), `gitEnvironment`, `repositoryScope`, `validateExecutablePath`, `runGit`,
  `redactFailure`, plus the clone side's `cloneArguments`, `helperArgument`, `validateBranch` and
  `headCommit`.
- `mirror.go`: the second capability — `MirrorJob`, `MirrorResult`, the two reason codes
  (`credential_executor_fetch_failed`/`credential_executor_publish_failed`), `MirrorConfig`,
  `MirrorExecutor`, `NewMirrorExecutor`, `Sweep`, `Prepare`, `matchMirrorBinding`,
  `mirrorMaterialIdentity`.
- `mirrorcontent.go`: the half of a preparation that runs Git — `mirrorArguments` (the clone's
  constraints, pinned the same way, plus a `--bare` fetch), `validateMirrorRef`, `fetch`
  (init/fetch/rev-parse/for-each-ref/symbolic-ref), `gitFact`, `publish`.
- `server.go`: the mutually authenticated HTTP listener (`Server`, `ServerConfig`, `NewServer`) with
  two routes, which accepts only the deployment-fixed dispatching identity
  (`presentedIdentity`/`callerIdentity`), with a 16 KiB request body ceiling (`maxJobBody`). Both
  routes **share one dispatching identity**, which is what makes "the same presenter gets a different
  answer on each route" an observable form of "the rule is the operation, not the route".
- `client.go`: the caller's side — `Client`, `ClientConfig`, `NewClient`, `Dispatch`, `PrepareMirror`,
  `DispatcherRefusal`.
- `storeclient.go`: the `RedemptionClient` interface and `StoreClient` — the `broker-resolver` mutual-TLS
  client, the ten Store reason codes, and `Credential`.
- `executor_test.go`: 18 deployment mutations, argv and environment pinned position by position, the
  refusal paths, destruction on failure, and the sweep.
- `mirror_test.go`: 13 mirror deployment mutations, 20 "refuse before spending" cases, the pinned argv,
  what a publication contains, idempotence by commit, destruction when the fetch fails, and the fetch
  sweep.
- `acceptance_test.go`: the end-to-end clone acceptance run against a real private repository (see
  "Tests").
- `mirror_acceptance_test.go`: the end-to-end mirror preparation against a real private repository, and
  the purpose-confusion matrix in both directions (see "Tests").

## Interface

- `Config`: the materialization root, the checkout root, the directory Git uses as `HOME`, the absolute
  paths of the helper and of Git, an optional CA file, Cloud's verification key set, the
  `RedemptionClient`, a timeout, a clock and a logger. **Every one of them is deployment-fixed**: none
  comes from a job, so no job can point the clone at a trust anchor or a program of its own choosing.
- `New(cfg)`: an incomplete deployment is refused at startup rather than degraded at runtime. The
  materialization and checkout roots must be **different** directories; the helper and Git must be
  absolute paths free of shell metacharacters; the helper must not be writable by C itself; and `HOME`
  must exist and be exactly 0700 — Git reads that directory as `HOME`, so a home another account could
  write to is a `.gitconfig` that account can plant, and `GIT_CONFIG_GLOBAL=/dev/null` neutralizes only
  the file Git is told to read, not a directory Git might fall back to.
- `Execute`: a job's authorization is a **bearer secret**, and this package never prints, logs or writes
  one anywhere; the log gets one line per job carrying an execution id, a repository and a reason code.
  The returned `*Failure` carries a stable code and unwraps to a cause — the code goes to the caller and
  the cause only to the log, because telling a caller whether the forgery it sent was malformed, expired
  or merely unverifiable is helping it iterate on the forgery.
- `Sweep`: `(entries removed, error)`. Run at startup; removes every materialization directory a previous
  run left behind.
- `MirrorConfig` / `NewMirrorExecutor`: the second capability's deployment descriptor and constructor.
  It **reuses** the clone's Git and materialization inputs (the same `gitDeployment` boundary — helper,
  Git, `HOME`, CA, materialization root), because one C identity owns one materialization root and a
  second root would be a second place a token could outlive its job; it **additionally** requires a
  verification key set, a `MirrorRedemptionClient` and a `*mirror.Store`, and refuses to start without
  any of them.
- `MirrorExecutor.Prepare(job)`: `(MirrorResult, error)`, in the order above. All four fields of the
  returned `MirrorResult` are facts Cloud cannot derive from what it sent: the commit, the single ref,
  the canonical upstream identity, and the mirror store's publication generation — the last is what
  lets Cloud tell the mirror it approved from a later republication under the same key.
- `MirrorExecutor.Sweep`: `(entries removed, error)`, removing what a previous run left under the fetch
  root. It does **not** touch the mirror store: an abandoned *staging* directory there is reclaimed by
  the store's own sweep, because only the store can take the key lock that proves a writer is gone.
- `Client.PrepareMirror(job)`: the second method beside `Dispatch`, POSTing to
  `/v1/mirror-preparations` and sharing the `maxJobBody` bound and the `DispatcherRefusal` mapping. It
  is a method rather than a mode on `Dispatch` for the same reason the executor has two handlers: a
  clone names an execution and a Node, a preparation names an Operation and an attempt, and which one
  is being sent is a fact about the call site rather than about a field.
- `Server`/`Client`: HTTPS with JSON. The carrier is plain HTTP under TLS for the same reason the
  Credential Service's is — the platform's service-identity model is "TLS with a pinned client CA and a
  SPIFFE URI in the client certificate", which `net/http` expresses completely — and this endpoint is
  deliberately **not** part of the public contract: the decisions require the authorization to stay off
  the business protocol.

## Invariants

- **Refuse rather than degrade**: any missing required input is a startup failure; a job with no
  authorization is refused; a failed redemption fails the whole clone and never falls back to an
  anonymous or static Git identity. `ReasonCodeUnavailable` (`credential_executor_unavailable`) means
  "this deployment established no boundary", which is a different answer from "this clone was refused"
  and from "the clone itself failed".
- **A failed cleanup is not a success**: `ReasonCodeCleanupFailed` is reported even when the clone
  succeeded — a clone whose credential may still be on disk is not a successful clone
  (`TestExecuteReportsACleanupFailureRatherThanASuccess`).
- **argv and the environment are pinned**: `cloneArguments` fixes, position by position, the helper
  path, `credential.useHttpPath=true` (which is what makes Git report the repository path the helper
  checks against the staged scope), `http.followRedirects=false`, the proxy keys, `core.hooksPath`,
  `core.symlinks`, submodules and LFS; `gitEnvironment` is the clone process's **entire** environment
  (`GIT_CONFIG_GLOBAL`, `GIT_CONFIG_NOSYSTEM`, `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS` and the rest). The
  token never enters argv: it travels only in the `credential` protocol between the helper and Git.
- **Nothing the repository supplies gets run**: no hooks, no LFS filters, no submodules, no following
  symlinks — each of those is a program or a fetch the authorization does not cover.
- **Failures do not echo the secret**: `redactFailure` removes the token from error strings, and
  `Failure` carries no fragment of a credential.
- **The checkout and the materialization root have different permissions on purpose**: the checkout is
  the handoff, and a 0700 materialization root and a 0755 checkout root cannot share one permission
  story — the clone holds no secret and the materialization root holds nothing else.
- **The purpose comes from the signature, not from the route**: each route compares the **operation in
  the authorization** before it asks the Store for anything. So sending a clone's job to the mirror
  route does not buy a mirror, and does not spend the Node's clone either — it buys a 400
  `binding_mismatch`, and the authorization still redeems on the route that owns it.
- **Only the ref the authorization signed is fetched**: the ref travels inside the signed authorization
  and is validated by `cloneauth.ValidateRef`, and the published mirror is asserted with `for-each-ref`
  to expose **exactly** `refs/ora/mirror`, so a reader gets the commit Cloud's rows name rather than the
  commit whoever dispatched would prefer.
- **The fetch directory lives inside the mirror store and is derived, not configured**:
  `fetchRoot = <mirror root>/fetch`, which is what makes the move into the store's staging area a rename
  within one filesystem by construction. A deployment that nested the mirror root and the
  materialization root inside one another is refused at startup by `NewMirrorExecutor` (see the next
  section) — because the startup sweep removes **every** entry under the materialization root.

## Deployment ownership and failure cleanup

- Identity: C is the deployment unit's own **non-root** process identity (follow-up decision D3; the
  production deployment runs it as uid 10002, distinct from Cloud's 10001 and from W's 1000 inside the
  Node). It holds neither Cloud's signing key nor the dispatching identity — only the `broker-resolver`
  redemption identity.
- Volumes: the materialization root, the checkout root and `HOME` are created by the deployment as
  C-exclusive 0700 directories; the helper and the executor binary are root-owned and not writable by C,
  at deployment-fixed paths. `cluster/compose/clone-executor-setup.sh` creates the directories and
  `cluster/scripts/clone-executor-boundary.sh` re-checks this boundary on a running stack.
- The data volume must survive a restart (`clone-executor-data`): lose it and the materialization
  directories left by a crash can no longer be swept.
- The mirror volume is a **different** volume: the mirror root holds published content the internal Git
  Server serves to readers, while the materialization root holds only secrets. So the deployment layer
  (`MirrorExecutor` in `internal/config`) refuses at startup to configure a mirror root that contains
  or is contained by the materialization root or the checkout root — a mirror root nested inside the
  materialization root would be deleted, published content and all, by the startup sweep
  (`credentialmaterial.Sweep` removes **every** entry under the materialization root), and the reverse
  nesting would put credential material on the volume the Git Server serves. Neither is a state a
  deployment should be able to describe, so neither is one it can start in. **Declaring no mirror root
  means having no such capability**: the route answers `credential_executor_unavailable` honestly and
  the clone path is unaffected — whether this machine serves an internal Git Server is a separable
  question.
- Cleanup coverage: the in-job `defer` covers success, failure, timeout, cancellation and panic; outside
  the process, `Sweep` covers a crash. What still has **no direct evidence** is an end-to-end sweep after
  a real hard kill, and the "timeout" and `Interrupted` outcomes of the Node clone state machine — the
  executor does not join that state machine today (see `20261012` D4). The verification obligations say
  so and mark it `Partial` rather than counting it done.

## Dependencies and callers

- Depends on `internal/cloneauth` (verification), `internal/credentialmaterial` (the layout), and, on
  the mirror side, `internal/mirror` (the publication container) and `internal/mirrorpublish` (that one
  fixed internal ref name), plus `zap`; everything else is the standard library. It depends on **neither** `internal/core` nor
  `internal/credentialstore`: the Store's ten refusal codes are **deliberately duplicated** in
  `storeclient.go` rather than imported, because what the executor depends on is that wire vocabulary
  rather than the Store's implementation — the two are separate components with separate binaries, and
  the codes the executor reports have to keep meaning the same thing if the Store is replaced behind its
  endpoints.
- Called by: `cmd/clone-executor` is its deployment entry point, and `cmd/git-credential-ora` is the
  helper it fixes (sharing only `credentialmaterial`, never the job logic). On the Cloud side,
  `CloneDispatcher` in `internal/core` calls `Dispatch` in `client.go`.
- **No production caller is enabled today**: `CLOUD_CLONE_DISPATCH_ENABLED` defaults to `false`, so Cloud
  neither signs nor dispatches and the executor refuses every job. That is the fail-closed default, not
  a pending task. **The mirror path is in the same state**: `Client.PrepareMirror` is driven only by
  this package's acceptance test, Cloud's mirror dispatcher is not wired yet, and
  `internal/mirrorpublish` still has no production caller outside this package.

## Tests

- `executor_test.go` is offline and deterministic (a stand-in script plays Git): `New` refuses each of 18
  deployment mutations (no materialization root, a materialization root that is the checkout root, no
  helper, a helper path with shell metacharacters, no verification key, no service identity, no Store
  endpoint, a `HOME` that is missing, not a directory, or not 0700); `cloneArguments` pins the
  configuration position by position and refuses any job that would make Git run something else; the
  helper argument refuses a path Git could split or execute; `repositoryScope` agrees with the host and
  path Git reports to a credential helper (both sides pass through `credentialmaterial.NormalizeScope`);
  the refusal paths refuse before spending anything; an authorization signed by another key is refused;
  the Store's own refusal code is preserved; a clone succeeds and the credential is proven gone; a failed
  clone still destroys the material; a failed cleanup is not reported as a success; and the sweep removes
  material a previous run left behind.
- `acceptance_test.go` is the **real-chain** acceptance run with no stand-ins: a real private HTTPS
  repository served by `git http-backend` behind HTTP Basic authentication, a real Credential Service
  with a real volume and a real key-encryption key, real mutual TLS between all three parties, the real
  `git-credential-ora` binary (built on the spot), and the production `core.CloneDispatcher`. Its nine
  cases: a private repository clones successfully with the right checkout content, and afterwards the
  materialization root holds zero entries with the token absent from both the materialization and
  checkout roots; the authorization is consumed exactly once and replay is refused; a redemption is
  refused when the authorization names another credential (the cross-tenant case); a redemption under
  another runtime is refused; a redemption after revocation is refused; a superseded version is still
  deliverable inside its retention window while a version that never existed is refused (the version
  conflict); **the default deployment refuses every redemption**; a failed clone still destroys the
  credential; and the startup sweep removes what a crash left behind.
- `mirror_test.go` is offline and deterministic too (a stand-in script plays Git): 13 mirror deployment
  mutations are each refused (no per-mirror limit, no volume limit, a volume limit below the per-mirror
  one, a relative mirror root, no verification key, no Store endpoint, no Git), and `fetchRoot` is
  asserted to be `<mirror root>/fetch`; 20 "refuse before spending" cases each assert the reason code
  **and** `store.calls == 0` **and** zero records in the store;
  `TestMirrorPrepareRefusesACloneAuthorizationWithoutSpendingIt` is the in-package direction of the
  confusion matrix; `mirrorArguments` pins the configuration position by position and refuses an
  unusable target; after a publication the store is readable, the fetch root is empty and the token is
  absent from the materialization root; preparing an already-published commit returns the existing
  record instead of republishing; a failed fetch still destroys the material; and the sweep removes the
  abandoned fetches.
- `mirror_acceptance_test.go` is the **real-chain** acceptance run for a preparation, reusing the same
  real components (a real private HTTPS repository, a real Credential Service with a real volume and a
  real key-encryption key, real mutual TLS, the real `git-credential-ora` built on the spot, and the
  real mirror store) and putting **both executors on one server**, so a single dispatching identity
  presenting one certificate is answered differently by each route — the observable form of "the rule is
  the operation, not the route". Its four cases: a real private repository prepares successfully, the
  published bare repository can be read back by a real `git clone` yielding the README's bytes,
  `ls-remote` advertises `HEAD` and `refs/ora/mirror` at the same commit (`HEAD` is that one ref seen
  through a symbolic ref, not a second ref), and afterwards the authorization cannot prepare a second
  mirror, no token appears under the materialization root and it holds zero entries; **the mirror route
  refuses a clone authorization** (400 `binding_mismatch`, zero records, and that clone authorization
  then still redeems through `Resolve`); **the clone route refuses a mirror authorization** (the same
  400, and that mirror authorization then still redeems through `ResolveMirror`); and a server built
  with `Mirror: nil` answers `credential_executor_unavailable` (500) rather than a routing mistake, and
  spends nothing.
