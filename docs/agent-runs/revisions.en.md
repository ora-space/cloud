# Revisions: Delivery, Verified Registration and Resume

English | [中文](revisions.md)

A Revision is what an Agent run saves: the final commit (after the Agent's own commits the Node may add
an internal snapshot commit for remaining uncommitted changes), an incremental bundle relative to this
run's clone base commit, and the session JSONL. A Revision is registered even when nothing changed — it
simply has no bundle.

A Revision is registered only after Cloud itself has checked the objects in object storage, and the run
Workspace is released only after the Revision is registered (or delivery was explicitly given up). The run
lifecycle is in [Agent IssueRuns](issue-runs.en.md).

## Object storage

Cloud owns the object storage configuration, issues scoped upload and download grants, and verifies objects
before registering them; the Node only ever receives one-shot grants and never holds storage credentials.

| Setting (`object_store.*`) | Description |
|---|---|
| `enabled` | When off, no grant is issued; deliveries settle as `skipped` at declaration and the run goes straight to release |
| `endpoint` | The private address Cloud uses to reach storage, for verification and reads |
| `public_endpoint` | Optional; the address signed into upload and download grants when Nodes reach storage at a different address than Cloud |
| `region`, `bucket`, `path_style` | Parameters of the S3-compatible store; MinIO and RustFS need `path_style: true` |
| `access_key_id_file`, `secret_access_key_file` | Credential **file** paths; read into process memory at startup and never present in configuration, environment variables or logs |
| `upload_grant_ttl` | Grant lifetime, 15 minutes by default, between 1 second and 7 days |

At startup Cloud signs one grant against each address as a configuration self-check, without any network
request; a misconfiguration stops the server from starting. `GET /healthz` reports object storage as
`configured` or `unconfigured`; this does not mean the store is online right now and reveals no address or
credential. Signing uses a built-in AWS SigV4 implementation (`internal/objectstore`) with no extra
dependency.

## The delivery flow

```text
session terminal ──▶ Cloud declares a delivery work item (fixed input and object keys)
              │
Controller registers the dispatch ──▶ Node creates the internal snapshot, bundle and session JSONL
              │
Controller ──GrantRevisionUpload──▶ Cloud issues upload grants (held in memory only)
              │
Node uploads the objects ──▶ reports a terminal: revision_delivered / revision_unchanged / revision_failed
              │
Cloud: verify objects outside a transaction ──▶ inside one: re-check, record the verdict, register, advance the run
```

**1. Declaration.** When the session execution has a terminal state the run enters `delivering`, and Cloud
declares a `deliver_revision` work item in the same transaction. Its input is fixed: the base commit (equal to
the run Workspace's `base_commit_id`), the identities of the session and checkout executions, the prior
Revision (copied from the session input), and three locations decided by the **server**:

- the bundle object key `revisions/{tenant}/{run}/{work}/revision.bundle`;
- the session JSONL object key `revisions/{tenant}/{run}/{work}/session.jsonl`;
- the Revision ref `refs/ora/revisions/{run}`.

Object keys include the work item ID, so every retry is a new work item with a new set of keys, and no caller
can name an object another attempt already wrote.

**2. Upload grants.** The Controller calls `GrantRevisionUpload` to obtain grants for the delivery execution,
optionally submitting each object's SHA-256. Each grant covers one object key, one method and one time window:

- every `PUT` is signed with `If-None-Match: *`, so the store only allows the first creation, and even a grant
  still within its lifetime cannot overwrite an object that was already verified;
- when a digest is submitted it is signed into the request as `x-amz-checksum-sha256`, and the store refuses
  an upload whose content does not match;
- a grant is a short-lived bearer credential, **not** an execution input: it only travels in memory and between
  processes and is never written to the database, files, environment variables, command arguments or logs.
  Refreshing a grant never changes the fixed input or object keys.

**3. The result.** The Node reports one of three terminals: `revision_delivered` (changes, with bundle and JSONL),
`revision_unchanged` (no changes, JSONL only) and `revision_failed` (with a reason from a closed set:
`session_not_settled`, `checkout_unavailable`, `snapshot_failed`, `bundle_failed`, `history_unavailable`,
`upload_failed`).

**4. Verification and registration.** A successful declaration is handled in two steps:

- **Outside a transaction**: a short transaction first checks the service identity, lease, execution identity and
  object shapes, and that the object keys, base commit and Revision ref equal the frozen input; then every object
  is checked with a `HEAD` against the private address, comparing its size and the SHA-256 the store kept. A store
  that returns no digest (because it never received a checksum) also fails verification. A network failure
  returns a retryable error, the Node event is not acknowledged, and it can be replayed later.
- **Inside a transaction**: Cloud checks again that the run has not ended, the run Workspace is still ready,
  runtime control still belongs to this run, and the current Node and sandbox are still the delivery's target; it
  then writes the verification verdict (`revision_verifications`), registers the `revisions` row when verification
  passed, and calls the business hook that moves the run to `releasing`. The original Node result, receipt,
  verdict, Revision and business transition commit in one transaction.

When verification fails, Cloud records its own verdict `verification_failed`, keeps the Node's original
declaration, registers no Revision, and retries delivery with backoff; only Cloud can write this reason, and a
Node cannot claim it. Replaying an execution that was already settled never contacts storage again. Delivery retry
and give-up limits are in [Agent IssueRuns](issue-runs.en.md#limits).

## Resuming a prior Revision

When an Agent is `@`-mentioned again on the same Issue, the new run still gets a brand-new run Workspace, but
before the Agent starts the Node restores the output of the Issue's latest Revision into the checkout, so the Agent
continues from where the last run left off.

- **Selection happens once at session start and is fixed**: the candidates are Revisions registered by **other**
  runs of the same tenant and Issue that carry a bundle (their own or one reused from a prior Revision), and the
  most recently registered one wins. A candidate whose Project or repository URL differs from this run is not
  resumed (`project_changed`); a candidate previously marked as not resumable is not resumed either
  (`resume_refused`); there is no fallback to an older candidate. The chosen Revision is written into the session
  input's `priorRevision` and the run's `resumeRevisionId` and never changes afterwards.
- **Download grant**: the session execution obtains a read-only `GET` grant for the prior bundle through
  `GrantRevisionDownload`, for exactly the object key the session input fixed.
- **Failing to restore fails the run**: if restoring is impossible the session ends with `agent_failed`, the run
  fails and the Agent does not start, but delivery still happens. Only the permanent failure "the remote history
  was rewritten and the base commit can no longer be fetched" (`prior_revision_base_unavailable`) marks that
  Revision as not resumable, so the next run starts from scratch.
- **Only the latest one is needed**: a delivery bundle is always made relative to this run's clone base commit,
  and resumed commits build on the prior final commit, so the new bundle naturally contains every commit not yet in
  the remote. A resumed run whose final commit equals the prior one (no new changes) is recorded as `unchanged` and
  reuses the prior bundle.
- **Runs on one Issue are assumed to be serial**, and Cloud does not enforce it: two overlapping runs may pick the
  same prior Revision, and the next run resumes only the one registered later.

## Public reads

The run resource's `revision` field projects the registered Revision's metadata: `id`, `baseCommit`, `finalCommit`,
`changed` (whether this run stored a bundle of its own), `bundleSize`, `historySize`, `priorRevisionId` and
`createdAt`. It is `null` for a run that registered no Revision; `result.deliveryState` says why. Object keys, the
Revision ref and digests are not public, and the public API offers no bundle download. The session JSONL archive can
be read as described in [Session reads](session-reads.en.md).

The proposal to receive deliveries through the Git Server and register Revisions from delivery refs is still under
decision; see [Git Server](../repositories/git-server.en.md#ref-queries).

## Related decisions

- [Cloud owns the object storage configuration and issues scoped upload grants; a Revision is registered only after Cloud verifies the objects](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/0-cloud-owned-object-store-and-verified-revisions.md)
- [A new run on the same Issue first resumes its latest Revision, then starts the Agent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md)
- [Session and delivery inputs gain a prior Revision, plus `GrantRevisionDownload`](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261010-revision-restore-contract.md)
- [A Revision's code changes are delivered only through the Git Server](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261012-git-server-delivery-and-verified-delivery-refs.md) (proposed)
