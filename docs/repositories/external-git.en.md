# The External Git Line: Repo Executor, Credential Store, Sync and Publish

English | [中文](external-git.md)

The external Git line is Ora's only path to external Git repositories: the Repo Executor brings an
external repository into the Project repository and explicitly publishes the Project repository back
out; an independent Credential Store holds the tokens; Cloud records only repository Bindings,
repository Operations and credential metadata. This document covers the participants, the sync and
publish flows, the credential lifecycle, and the authorization and failure rules that run through
them. The Git Server's own listeners and ref rules are in [Git Server and Project
Repositories](git-server.en.md).

## Participants

| Participant | Entry point | Responsibility |
|---|---|---|
| Cloud server | `cmd/server`, `internal/repoworker` | Records Bindings and Operations; issues one-time authorizations; leases, renews and settles; verifies through the ref query before a sync may succeed; never touches credential plaintext |
| Repo Executor | `cmd/repo-executor`, `internal/repoexec` | The only process that reaches external Git repositories; stateless, no database; runs each task in a private temporary directory and destroys the directory and credential material afterwards |
| Credential Store | `cmd/credential-store`, `internal/credentialstore` | Keeps HTTPS tokens on an independent volume with envelope encryption; four listeners serve browser submission, executor redemption, Cloud management and KEK operations |
| `git-credential-ora` | `cmd/git-credential-ora` | The only Git credential helper the Repo Executor configures for an external remote; it delivers the credential only when the requested repository equals the staged scope |
| Git Server | `cmd/git-server`, `internal/gitserver` | Smart HTTP and the read-only ref query for Project repositories; the executor writes snapshots and tracking refs through the service repository listener |

External repositories are reached over https only. The Repo Executor joins the internal service
network and one egress network; the Credential Store joins the internal network only. Neither joins
the sandbox network, and a sandbox never obtains the executor certificate or credential plaintext.

## Sync

A sync (`repository_sync`) brings one branch of the external repository into the Project repository.
It is created when a Project is created (`project_created`), when a member asks for one, when a member
proposes a candidate credential for the Binding, and after a publish changes the target branch
(`after_publish`). A Binding has at most one unfinished sync; a repeated manual request returns the
existing Operation.

1. The background worker leases a `pending` Operation while it has a free slot, moves it to `running`
   and issues that execution's one-time authorization in the same transaction. Before entering
   `running` it reads the Binding's tracking ref through the ref query; when the read fails it
   releases the lease and starts nothing. The authorization records that expected old value as
   `expectedTrackingOid`, and the tracking ref may only move from it.
2. The Repo Executor checks that the authorization matches the task field for field, applies its
   egress policy, creates a bare repository under `tasks/`, optionally fetches the tracking ref as a
   base, and fetches from the external remote. It pushes once with `--atomic`: it creates
   `refs/ora/upstreams/<bindingId>/snapshots/<oid>` and moves
   `refs/ora/upstreams/<bindingId>/heads/<branch>` from the expected old value to the new commit.
   Snapshots are create-only and their name must equal the target commit; the tracking ref is
   tracking-cas and allows non-fast-forwards, so an external force-push only moves the tracking ref
   to the new history.
3. After the executor reports `applied`, Cloud still confirms through the ref query that both the
   snapshot ref and the tracking ref point at the reported commit, and only then records `succeeded`
   and makes that commit the Binding's current snapshot. Query failures are retried with backoff
   inside the lease; once the lease has ended nothing is recorded and the sweep replays the work
   idempotently.
4. A successful sync records the `read` capability for the used credential's current version. When a
   candidate credential's sync succeeds and the Binding did not change meanwhile, the candidate is
   adopted.

Unknown outcomes converge by whether the task can possibly have run: an executor refusal (`429
executor_busy`, identity mismatch) or an undelivered request means the task never started and is
recorded `failed`; a connection lost after the request was sent means the sync may have written, so
it is recorded `failed` and Cloud creates at most three automatic replays. A push superseded by
`stale info` is recorded `superseded`, except that a tracking ref already at this commit with the
snapshot present is recorded `applied`.

## Publish

A publish (`repository_publish`) is the only way Ora writes an external repository, and only a user
can create one explicitly. Its fields are immutable after creation: the source ref and commit (a
workspace branch, a delivery ref, or an existing Revision, which contributes the delivery ref and final
commit Cloud verified when registering it; a bundle-shaped Revision without a delivery ref is refused
with `revision_not_publishable`), the target branch, the expected old value
`expectedOldOid`, and the credential reference; `expectedOldOid` must differ from the source commit.

1. Cloud first confirms through the ref query that the source ref exists (refusing with
   `409 source_ref_not_found` when it does not), then creates the Operation; it does not compare the
   source ref's current commit. Whether the source commit is reachable from the source ref is checked
   by the executor, because Cloud reads no Git objects.
2. The credential's current version must already have the `publish` capability. A personal credential
   is approved by its owner; when a tenant administrator publishes with somebody else's personal
   credential, the owner approves on `awaiting_approval` and an administrator cannot approve on their
   behalf. A team credential is published by a tenant administrator directly. The approval binds the
   fields and the credential version for 24 hours; if the credential gains a new version meanwhile,
   the publish is cancelled or refused with `credential_changed`.
3. The executor takes the source from the Git Server service repository listener, runs `ls-remote`
   against the target first, and pushes with a lease only allowing a fast-forward, using
   `expectedOldOid` as the compare value. A publish never forces; a non-fast-forward is refused as
   `not_fast_forward`.
4. After success Cloud requests a sync of the Binding so the current snapshot catches up with the
   published branch; that sync absorbs an already-requested resync.

An uncertain publish enters `uncertain` and is never blindly resent. The first check waits until the
task deadline (after which the executor can no longer push); later checks read the target ref under a
`probe` authorization, from 2 minutes growing exponentially to 1 hour, at most 20 attempts (roughly
16 hours). A target equal to the source commit is `succeeded`, a target still at the expected old
value is `failed(not_applied)`, and any other value is `conflict(moved_after_uncertain)`; a check
that cannot read counts as a failed attempt, and exhaustion records `failed(outcome_unknown)`. A
member may ask for an immediate check.

## Credentials

Cloud keeps only metadata: attribution (`personal` or `team`), name, repository scope, the Store's
current version, proven capabilities and availability. The token goes from the browser straight into
the Credential Store; no Cloud path ever sees it.

1. **Create.** A member creates a personal credential; only a tenant administrator creates a team
   credential. The repository scope must be a normalized https repository URL. Cloud returns a
   submission grant (`submit` purpose, at most 5 minutes) and the Credential Store's browser origin.
2. **Submit.** The browser `PUT`s the token directly to the Credential Store's browser ingress with
   the submission grant. The Store verifies the grant with its own clock and Cloud's submission
   public keys, requires the grant's `credentialId` to equal the path, the version to be exactly the
   current version plus one, and the submission to be unused, then claims it exclusively and writes
   the new version.
3. **Confirm.** The browser then notifies Cloud. Cloud does not trust that report: it asks the Store
   through the management interface for the current version and submission ID, and records the
   credential as `stored` only when they match the pending submission exactly. Once the grant has
   expired, Cloud waits 2 more minutes, asks the Store once, and either adopts the version already
   written or clears the pending record.
4. **Rotate.** Rotation issues a new submission grant for the next version; the old version remains
   redeemable during its retention period, and a new version does not change availability before it
   is confirmed.
5. **Revoke.** Cloud first marks its reference `revoked` and stops issuing new authorizations, then
   calls the Store and retries until it succeeds. The Store's revocation deletes the ciphertext of
   every version; an unknown ID also leaves a revocation mark.
6. **Capabilities.** A new version starts with no capability. A successful sync proves `read`; the
   `publish` capability is proven by a `receive-pack` advertisement probe (`probe` authorization,
   `verified`, `push_denied` or `verification_failed`). A token the external platform refuses loses
   the capability and may mark the credential `invalid`; a personal credential is frozen once its
   owner leaves the tenant.

A credential supports new remote work only when it is `stored`, available, scoped to the Project's
repository and has the required capability; Cloud evaluates this both when creating work and when
issuing the authorization. A Workspace clone never reaches the external repository and uses no
credential: it clones only the Binding's current snapshot (see
[Workspace lifecycle](../runtime/workspaces.en.md#the-clone-step-clones-the-binding-snapshot)), so a
Project with a credential clones exactly like one with a public repository.

## Authorization and least privilege

| Grant | Issuer | Verifier | Purpose |
|---|---|---|---|
| Execution authorization (`sync`, `publish`, `probe`) | Cloud's execution signing key | Repo Executor, Credential Store | Start one task; redeem credential plaintext once |
| Submission grant (`submit`) | Cloud's submission signing key | Credential Store browser ingress | Write one credential version from the browser |

- The two kinds use two different Ed25519 private keys, and Cloud refuses identical keys at startup;
  even a misconfiguration cannot make one stand in for the other because the purpose fields are
  validated strictly.
- A grant is `base64url(payload).base64url(signature)`, and verifiers hold a public-key set so keys
  can rotate. Its lifetime is at most 15 minutes, and an execution authorization's
  `executorIdentity` must be `spiffe://ora.local/repo-executor`.
- Execution authorizations carry fields per purpose: `sync` carries the Operation, tenant, Project,
  Binding, repository, branch and expected old value; `publish` carries the source ref, source
  commit, target ref and expected old value; `probe` carries exactly one of a check ID or an
  Operation ID, the repository and the `upload-pack` / `receive-pack` service. Fields of other
  purposes must be empty, and a `probe` authorization can never drive a publish.
- A credential can be redeemed at most once per execution authorization: the Credential Store
  creates the exclusive claim file before decrypting. The Repo Executor writes plaintext only into a
  `0700` private material directory for one task, and deletes the material and task directory when
  the task ends, failures and panics included.
- The Repo Executor's egress rules come from the deployment-owned `deployment-egress.json` (the same
  file Cloud and model-gateway load): public mode resolves through the deployment DNS, admits https
  only and requires every address to be public; enterprise mode admits only the exact host:port
  targets listed in the file, refusing anything else before DNS. The shared topology always refuses
  cluster ranges and host addresses, so internal services such as the Git Server and the Credential
  Store cannot be external repository targets. Before every external Git subprocess the executor
  resolves and checks the whole address set, then hands the checked numeric addresses to Git through
  `http.curloptResolve`; TLS still verifies the original host name, so a name that changes after the
  check cannot redirect the connection. The probe's `receive-pack` uses the same dialer. A health
  listener reports the schema and digest so a deployment can compare it with Cloud and model-gateway
  after changing the file. An external remote never receives the executor certificate or the
  `Ora-Git-Operation-Id` header. `git-credential-ora` takes the material directory as its argument,
  and delivers a username and password only when Git's request normalizes to exactly the staged
  scope; redirects, submodules and another repository on the same host get nothing.

## Failure and sweeping

The repository worker runs a sweep every 2 seconds, then leases work for its free slots:

| Sweep | Effect |
|---|---|
| Expired leases | `leased` returns to `pending` (no authorization exists, so retrying is safe); a `running` sync is recorded `failed(lease_lost)` and replayed, a publish becomes `uncertain` |
| Expired approvals | A publish not executed within 24 hours is recorded `cancelled(approval_expired)` |
| Unconfirmed submissions | Two minutes after the grant expires, adopt the version the Store already holds or clear the pending record |
| Store revocations | Retry revocations Cloud recorded but the Store has not confirmed |

The default concurrency is 4 tasks, the lease is 1 minute and is renewed every third of it, syncs are
bounded at 30 minutes and publishes and probes at 5 minutes. On a lost lease the worker cancels the
task immediately and the later settlement is fenced out by the lease generation; an Operation ID
enters `running` once, so an executor that lost its lease cannot borrow a later one.

## Configuration

Cloud's `repository.*` settings (environment prefix `CLOUD_REPOSITORY_`) enable the line, and it is
off by default: with it off Projects are still created, but no sync runs and credential creation
answers `503`. The Repo Executor and the Credential Store each read only their own
`REPO_EXECUTOR_*` and `CREDENTIAL_STORE_*` environment variables. All keys, certificates and file
references are in the [configuration
reference](../operations/configuration.en.md#repo-executor-and-credential-store); images, keys,
networks and backups are in [Deployment and
operations](../operations/deployment.en.md).

## Related decisions

The following ADRs are all `approved`; this repository implements the corresponding parts:

- [The Repo Executor is the sole boundary for external Git access](https://github.com/ora-space/specs/blob/main/decisions/cloud/repo-executor/0-repo-executor-sole-external-git-boundary.md) (approved)
- [Git credentials live in an independent Credential Store](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20261016-git-credentials-in-independent-credential-store.md) (approved)
- [Repository Binding and snapshot sync](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/20261016-repository-binding-and-snapshot-sync.md) (approved)
- [Explicit fast-forward publish](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/20261017-explicit-fast-forward-publish.md) (approved)
