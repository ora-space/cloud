# Revisions: Delivery, Verified Registration and Resume

English | [中文](revisions.md)

A Revision is what an Agent run saves: the final commit (after the Agent's own commits the Node may add
an internal snapshot commit for remaining uncommitted changes), the **delivery ref** the Node gets by pushing
that commit to the Project repository, and the session JSONL. Code is delivered only through the
[Git Server](../repositories/git-server.en.md); the session JSONL lives in object storage. A Revision is
registered even when nothing changed, and it pushes and registers its own delivery ref just the same.

A Revision is registered only after Cloud itself has checked the delivery ref and the session JSONL, and the run
Workspace is released only after the Revision is registered (or delivery was explicitly given up). The run
lifecycle is in [Agent IssueRuns](issue-runs.en.md).

The Git delivery and resume described here implement `approved` decisions (see Related decisions at the end).

## Deployment dependencies

A delivery needs both object storage (the session JSONL) and the Git Server (the code). When either is not
configured, the delivery settles as `skipped` at declaration and the run goes straight to release; the reason names
object storage first (`object_store_unconfigured`), then the Git Server (`git_server_unconfigured`). `GET /healthz`
reports each of them as `configured` or `unconfigured`; this does not mean they are online right now and reveals
no address or credential.

### Object storage

Cloud owns the object storage configuration, issues scoped upload grants, and verifies objects before registering
them; the Node only ever receives one-shot grants and never holds storage credentials.

| Setting (`object_store.*`) | Description |
|---|---|
| `enabled` | When off, no grant is issued |
| `endpoint` | The private address Cloud uses to reach storage, for verification and reads |
| `public_endpoint` | Optional; the address signed into upload grants when Nodes reach storage at a different address than Cloud |
| `region`, `bucket`, `path_style` | Parameters of the S3-compatible store; MinIO and RustFS need `path_style: true` |
| `access_key_id_file`, `secret_access_key_file` | Credential **file** paths; read into process memory at startup and never present in configuration, environment variables or logs |
| `upload_grant_ttl` | Grant lifetime, 15 minutes by default, between 1 second and 7 days |

At startup Cloud signs one grant against each address as a configuration self-check, without any network
request; a misconfiguration stops the server from starting. Signing uses a built-in AWS SigV4 implementation
(`internal/objectstore`) with no extra dependency.

### Git Server

Cloud's `git_server.*` settings give it the repository address sandboxes reach (written into clone, session and
delivery inputs), and it verifies delivery refs through the Git Server's ref query listener with its own client
certificate (settings in the [configuration reference](../operations/configuration.en.md#cloud-server), ref queries
in [Git Server](../repositories/git-server.en.md#ref-queries)).

## The delivery flow

```text
session terminal ──▶ Cloud declares a delivery work item (fixed input: JSONL key, repository, delivery ref, Workspace branch)
              │
Controller registers the dispatch ──▶ Node creates the internal snapshot commit and the session JSONL
              │
Node pushes the final commit to the delivery ref ──▶ the Git Server authorizes the push from current state
              │
Controller ──GrantRevisionUpload──▶ Cloud issues the JSONL upload grant (held in memory only)
              │
Node uploads the JSONL ──▶ reports a terminal: revision_delivered / revision_unchanged / revision_failed
              │
Cloud: outside a transaction, check the delivery ref by ref query and the JSONL by HEAD ──▶ inside one: re-check, record the verdict, register, advance the run
```

**1. Declaration.** When the session execution has a terminal state the run enters `delivering`, and Cloud
declares a `deliver_revision` work item in the same transaction. Its input is fixed: the base commit (equal to
the run Workspace's `base_commit_id`), the identities of the session and checkout executions, the prior Revision
(copied from the session input, with only its ID and final commit), and locations decided by the **server**:

- the session JSONL object key `revisions/{tenant}/{run}/{work}/session.jsonl`;
- the repository on the Git Server, `gitRepositoryUrl` (`git_server.repository_base_url` plus `/{tenant}/{project}.git`);
- this attempt's own delivery ref `refs/ora/deliveries/{work}`;
- the run Workspace's branch on the Git Server, `workspaceRef`, taken from the input of the clone execution that made
  the checkout;
- the local ref `refs/ora/revisions/{run}` the Node writes in its checkout, which never appears on the Git Server.

The object key and the delivery ref both include the work item ID, so every retry is a new work item with a new key
and a new delivery ref, and no caller can name an object or ref another attempt already wrote.

**2. Push.** The Node pushes the final commit to the delivery ref and may also fast-forward `workspaceRef` to it. The
Git Server lets it through only while the run is `delivering` and the delivery execution is live; the delivery ref
can only be created, and `workspaceRef` can only be created or fast-forwarded and must be a branch of the Workspace the
certificate names (see [Git Server](../repositories/git-server.en.md#requests-and-authorization)). Cloud verifies the
delivery ref only, not `workspaceRef`.

**3. Upload grant.** The Controller calls `GrantRevisionUpload` to obtain the session JSONL grant for the delivery
execution, optionally submitting its SHA-256; a digest may only name the JSONL key the input fixed, and any other key
is `400 invalid_upload_checksum`. The grant covers one object key, one method and one time window:

- every `PUT` is signed with `If-None-Match: *`, so the store only allows the first creation, and even a grant
  still within its lifetime cannot overwrite an object that was already verified;
- when a digest is submitted it is signed into the request as `x-amz-checksum-sha256`, and the store refuses
  an upload whose content does not match;
- a grant is a short-lived bearer credential, **not** an execution input: it only travels in memory and between
  processes and is never written to the database, files, environment variables, command arguments or logs.
  Refreshing a grant never changes the fixed input or object key.

**4. The result.** The Node reports one of three terminals:

- `revision_delivered`: the final commit equals neither the base commit nor the prior Revision's final commit;
- `revision_unchanged`: the final commit equals the base commit, or the final commit of the resumed prior Revision;
- `revision_failed`: with a reason from the closed set `session_not_settled`, `checkout_unavailable`,
  `snapshot_failed`, `history_unavailable`, `upload_failed`, `push_rejected`, `push_failed`. The former
  `bundle_failed` (value 4) is `reserved` in the wire enum; a result carrying it is refused like any reason outside
  the closed set.

A declared result that does not match whether the run changed anything, or that still carries a bundle, is
`409 result_conflict`.

**5. Verification and registration.** A successful declaration is handled in two steps:

- **Outside a transaction**: a short transaction first checks the service identity, lease, execution identity and the
  JSONL's shape, and that the JSONL key, base commit and local Revision ref equal the frozen input; then the Git
  Server's ref query reads the commit the delivery ref names, which must equal the declared final commit; then the
  JSONL is checked with a `HEAD` against the private address, comparing its size and the SHA-256 the store kept. A
  missing delivery ref, one naming another commit, a mismatched JSONL or a store that returns no digest (because it
  never received a checksum) all fail verification. An unreachable Git Server or store returns a retryable `503`
  (`git_server_unavailable`, `object_store_unavailable`): nothing is taken over, the Node event is not acknowledged,
  and it can be replayed later. A delivery ref can only be created, never moved or deleted, so the commit seen here is
  the one it names from registration on.
- **Inside a transaction**: Cloud checks again that the run has not ended, the run Workspace is still ready,
  runtime control still belongs to this run, and the current Node and sandbox are still the delivery's target; it
  then writes the verification verdict (`revision_verifications`), registers the `revisions` row with its
  `delivery_ref` when verification passed, and calls the business hook that moves the run to `releasing`. The
  original Node result, receipt, verdict, Revision and business transition commit in one transaction.

When verification fails, Cloud records its own verdict `verification_failed`, keeps the Node's original
declaration, registers no Revision, and retries delivery with backoff; only Cloud can write this reason, and a
Node cannot claim it. Replaying an execution that was already settled never contacts the Git Server or storage
again. Delivery retry and give-up limits are in [Agent IssueRuns](issue-runs.en.md#limits).

Each delivery ref belongs to at most one Revision (the partial unique index `revisions_delivery_ref`). Bundle-shaped
Revisions registered before the switch to Git are kept as they are, and a database constraint keeps the two shapes
mutually exclusive (migration `0054`).

### Original facts after delivery give-up

After root D5 moves a Run to `releasing` with `deliveryState=failed` and `revisionId=null`, or root D8 subsequently moves it to `done`,
the registered delivery may still submit its original terminal under the original execution, work, Workspace, Node and sandbox/generation identity.
No residual ledger or operator intent is required. This intake does not relax ordinary `liveDelivery` or dispatch eligibility, change the Run,
historical failed delete or admission, or create reclamation responsibility. The declaration, exact receipt, submission and necessary evidence commit
atomically before the existing takeover success permits ACK. ACK means fact takeover only. Late failures do not retry or resettle business;
success declarations retain the evidence rules above.

Both root D5/D8 intake and existing residual rescue first register a late Revision only when the same tenant/Issue has no later-created Run.
The final registration transaction compares the same-transaction `run.enqueued` activity seq for every Run and serializes with Run creation.
Timestamps, UUIDs, executor, status and soft deletion do not determine this order. Missing or ambiguous evidence, or any later-created Run,
means declaration and receipt only, with no Revision or verified delivery. Identical replays preserve the original decision without external reads,
registration or business hooks; real conflicts and existing ledger conflict audits retain their rules. Normal release proceeds independently;
late takeover does not restart a historical D8 failed delete. See
[late-fact ADR D1–D3](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261022-late-delivery-fact-takeover.md)
(approved; the landing run owns implementation status and joint acceptance).

## Resuming a prior Revision

When an Agent is `@`-mentioned again on the same Issue, the new run still gets a brand-new run Workspace, but
before the Agent starts the Node fetches the delivery ref of the Issue's latest Revision from the Git Server and
restores it into the checkout, so the Agent continues from where the last run left off.

- **Selection happens once at session start and is fixed**: the candidates are Revisions registered by **other**
  runs of the same tenant and Issue that have a delivery ref and a final commit different from their base commit,
  and the most recently registered one wins. A Revision that only holds its base commit would take the new checkout
  back to older code, and a bundle Revision from before the switch to Git has no ref to fetch; neither is a
  candidate. A candidate whose Project or external repository URL differs from this run is not resumed
  (`project_changed`); a candidate previously marked as not resumable is not resumed (`resume_refused`); a candidate
  whose Git Server repository is not the one this checkout was cloned from, or any candidate when no Git Server is
  configured, is not resumed (`repository_changed`); there is no fallback to an older candidate. The chosen Revision
  is written into the session input's `priorRevision` (`revisionId`, `finalCommit`, `baseCommit`,
  `gitRepositoryUrl`, `deliveryRef`) and the run's `resumeRevisionId` and never changes afterwards.
- **Reads are authorized by the Git Server**: a live Agent session can read the whole Project repository, so fetching
  the delivery ref needs no extra grant; object storage issues no download grant for resuming any more.
- **Failing to restore fails the run**: if restoring is impossible the session ends with `agent_failed`, the run
  fails and the Agent does not start, but delivery still happens. Only the permanent failure "the delivery ref cannot
  be fetched" (`prior_revision_base_unavailable`) marks that Revision as not resumable, so the next run starts from
  scratch; a run that already asked to cancel is exempt, because the Git Server refuses a cancelled run's reads with
  the same answer it gives for a missing ref, so the failure says nothing about the Revision.
- **Only the latest one is needed**: resumed commits build on the prior final commit, so the new delivery ref
  naturally contains every earlier commit. A resumed run whose final commit equals the prior one (no new changes) is
  recorded as `unchanged`; it still has its own delivery ref and reuses nothing from the prior.
- **Runs on one Issue are assumed to be serial**, and Cloud does not enforce it: two overlapping runs may pick the
  same prior Revision, and the next run resumes only the one registered later.

The session input also carries a `workspaceDirectory`: up to 100 of the Project's Workspaces as of session start
(newest first; run Workspaces stay listed after deletion because their branches remain on the Git Server), each with
its Workspace ID and kind (`main`, `isolated`, `run`), plus the Issue, run and run status for run Workspaces. A
WorkflowRun's Workspace is not listed: its sessions have no Git Server access and the directory has no fields for it.
The directory is a reference for the Agent in the session only, never refreshed and never used for authorization.

## Public reads

The run resource's `revision` field projects the registered Revision's metadata (fields are defined by
[`api/openapi.json`](../../api/openapi.json)). `changed` is Cloud's own verification verdict: `true` when the run
delivered new commits, `false` when it ended on its base commit or on the prior's final commit; for a Revision
registered before the switch to Git it means "this run stored a bundle of its own". `bundleSize` is `null` for a
Revision delivered over Git. The field is `null` for a run that registered no Revision; `result.deliveryState` says
why. Object keys, the delivery ref, the Revision ref and digests are not public, and the public API offers no code
download. The session JSONL archive can be read as described in [Session reads](session-reads.en.md).

## Related decisions

- [Cloud owns the object storage configuration and issues scoped upload grants; a Revision is registered only after Cloud verifies the objects](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/0-cloud-owned-object-store-and-verified-revisions.md)
- [A new run on the same Issue first resumes its latest Revision, then starts the Agent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md)
- [A Revision's code changes are delivered only through the Git Server: Cloud registers the delivery ref after a ref query, and resuming restores only from the prior's own delivery ref](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261017-git-native-delivery-refs-and-resume.md) (approved; it supersedes the 20261012 Git Server delivery decision)
- [The clone input becomes a Git Server snapshot input; the Git Server derives write rules from the execution kind; delivery and session inputs drop bundles and the download grant](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261016-snapshot-clone-input-and-execution-ref-grants.md) (approved)
