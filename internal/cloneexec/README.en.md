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

## Files

- `executor.go`: `Config`, `Executor`, `New`, `Sweep`, `Execute`, `Job`, `Result`, `Runtime`, `Failure`
  and the seven reason codes.
- `git.go`: the whole construction of a constrained Git invocation — `cloneArguments`, `helperArgument`,
  `gitEnvironment`, `repositoryScope`, `validateBranch`, `validateExecutablePath`, `runGit`,
  `redactFailure`, `headCommit`.
- `server.go`: the mutually authenticated HTTP listener (`Server`, `ServerConfig`, `NewServer`), which
  accepts only the deployment-fixed dispatching identity (`presentedIdentity`), with a 16 KiB request
  body ceiling (`maxJobBody`).
- `client.go`: the caller's side — `Client`, `ClientConfig`, `NewClient`, `Dispatch`,
  `DispatcherRefusal`.
- `storeclient.go`: the `RedemptionClient` interface and `StoreClient` — the `broker-resolver` mutual-TLS
  client, the ten Store reason codes, and `Credential`.
- `executor_test.go`: 18 deployment mutations, argv and environment pinned position by position, the
  refusal paths, destruction on failure, and the sweep.
- `acceptance_test.go`: the end-to-end acceptance run against a real private repository (see "Tests").

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
- Cleanup coverage: the in-job `defer` covers success, failure, timeout, cancellation and panic; outside
  the process, `Sweep` covers a crash. What still has **no direct evidence** is an end-to-end sweep after
  a real hard kill, and the "timeout" and `Interrupted` outcomes of the Node clone state machine — the
  executor does not join that state machine today (see `20261012` D4). The verification obligations say
  so and mark it `Partial` rather than counting it done.

## Dependencies and callers

- Depends on `internal/cloneauth` (verification), `internal/credentialmaterial` (the layout) and `zap`;
  everything else is the standard library. It depends on **neither** `internal/core` nor
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
  a pending task.

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
