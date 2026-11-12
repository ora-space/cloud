# Git Server and Project Repositories

English | [中文](git-server.md)

The Git Server (`cmd/git-server`, implemented in `internal/gitserver`) hosts one bare repository per
Project as that Project's code hub: Agent executions in sandboxes read and commit code through it, and
the Repo Executor brings snapshots of the external repository in through it. It is a separate process
built from the same image as Cloud and stores no authorization state of its own: what each request may
do is decided on the spot by `internal/core` from current PostgreSQL state.

This document first covers the repository Binding and repository Operations recorded on the Cloud
side, then the Git Server's listeners, identities and authorization rules.

## The Cloud side: repository Bindings and Operations

- **Binding (`repository_bindings`).** Exactly one per Project, recording the external repository's
  URL, the branch to sync and an optional credential reference; the Project document's
  `repositoryUrl` and `defaultBranch` are read from it.
- **Repository Operations (`repository_operations`).** Two types: `repository_sync` brings the external
  repository's content into the Project repository, and `repository_publish` publishes the Project
  repository's content back out. States and terminal states are pinned by `CHECK` constraints, and a
  Binding has at most one unfinished sync at a time. The transaction that creates a Project also
  creates its Binding and one `pending` sync triggered by `project_created`.
- **Interaction with Project deletion.** The transaction that moves a Project to `deleting` cancels every
  Operation that has not started running and clears any resync request; Operations that are running or
  awaiting confirmation (`uncertain`) settle normally, and `delete_project` starts only after all of
  them have ended. When the deletion is aborted because a Node refused to stop and the Project returns
  to `active`, the same transaction gives back the cancelled sync; cancelled publishes are not
  restored and must be started again by the user.

The Repo Executor that performs repository Operations is this repository's `cmd/repo-executor`; it
receives Git access by Operation state, a sync writes only this Binding's snapshot prefix and
tracking ref, and a publish only reads its source. The whole line's sync, publish, credential and
settlement rules are in [The external Git line](external-git.en.md). A Workspace's `clone` step clones only the
Binding's current snapshot (see [Workspace lifecycle](../runtime/workspaces.en.md#the-clone-step-clones-the-binding-snapshot)):
Nodes never reach the external repository, and repository credentials take part in no Node work.

## Listeners

| Listener | Default address | Network | Clients |
|---|---|---|---|
| Repository listener | `:8445` | Sandbox network only | Executions inside sandboxes, presenting a git-access client certificate |
| Service repository listener | `:8447` | Internal service network only | The Repo Executor, presenting its service certificate |
| Ref query listener | `:8446` | Default network | Only Cloud's fixed client certificate; also serves `/healthz` |

Each listener trusts exactly one CA: the repository listener trusts only the sandbox CA, the service
repository listener only the Repo Executor CA, and the ref query listener accepts exactly Cloud's one
leaf certificate. Certificates only prove who the caller is; they carry no repository permission.

## Requests and authorization

Repositories are served over smart HTTP at `/{tenantId}/{projectId}.git`. Every request must carry
**exactly one** context header naming the work it serves:

| Listener | Required header | Certificate identity |
|---|---|---|
| Repository listener | `Ora-Execution-Id: <execution ID>` | `spiffe://ora.local/git-access/tenant/<tenantId>/runtime/<workspaceId>/generation/<n>`; the URI must be the only one and its purpose segment must be `git-access` |
| Service repository listener | `Ora-Git-Operation-Id: <Operation ID>` | `spiffe://ora.local/repo-executor` |

A missing header, the wrong header, or both headers return `400`. Core resolves the request into a
grant: the whole Project repository may be read, plus the refs or prefixes that may be written, each
with the rule fixed by its namespace. The repository is the isolation boundary, so reads hide no ref
inside it.

**Grants for Agent session and delivery executions.** The execution must belong to this runtime, be unfinished and not
force-stopped; the runtime must be at its current generation and ready, and the Project `active`; the
user who triggered the run must still be able to use the runtime; the current Node must be the
execution's target Node; and runtime control must be the run's `maintenance` with a binding confirmed
to this sandbox. On top of that:

| Execution | Run phase | Writable refs |
|---|---|---|
| `agent_session` | `starting` or `running`, no cancellation requested, Thread not ending | Branches under `refs/heads/workspaces/<the certificate's workspaceId>/` (fast-forward rule); the prefix comes from the certificate, never from the request |
| `deliver_revision` | `delivering` | Create the delivery ref its input fixed, `refs/ora/deliveries/<delivery work item ID>`; also create or fast-forward the input's `workspaceRef`, provided it belongs to the certificate's Workspace |

Plugin executions are always refused. A WorkflowRun session is checked against the current run, graph node and attempt on every request: only the running attempt may read its Project repository and fast-forward its own Workspace branches. End requests, cancellation, replaced attempts and generations revoke that grant. Whole-run delivery has an independent create-only grant for its frozen delivery ref; see [workflow delivery](../workflows/workflows.en.md#delivery-and-history-archives).

**Grants for clone executions.** The clone execution of a Workspace operation's `clone` step is read-only and
valid only while the operation is `running` in the `clone` step: the execution has no result and is not
force-stopped; the Workspace is at the certificate's generation and not deleted; the Project is `provisioning` or
`active` (a main Workspace clones while its Project is being created); the repository URL in the execution's input
names this tenant and Project; the operation's actor can still use the runtime; no force-stop is in effect; and the
current Node is the execution's target Node. It does not require a ready Workspace or open admission, because cloning
happens before admission opens. The grant ends as soon as the operation leaves the `clone` step or the execution has a
result.

**Grants for the Repo Executor.** The named Operation must be in this tenant and Project, belong to the
Project's Binding and be `running`, and the Project must not be deleted (`provisioning`, `active` and
`deleting` all qualify, since the first sync runs during creation and a sync already running when
deletion starts must still be able to settle):

| Operation | Grant |
|---|---|
| `repository_sync` | Create snapshot refs under this Binding's snapshot prefix and move this Binding's tracking ref from the grant's expected old value to the new commit |
| `repository_publish` | Read-only; a publish only reads its source from the Project repository |

**Outcomes.** Every denial is the same `403`, a missing repository included, so a sandbox cannot probe
whether a repository, execution or Node exists. A database failure returns `503` and never lets the
request through. Authorization runs in a read-only consistent snapshot without core's global lock, so
Git traffic does not serialize with control-plane writes.

Ending the execution, a clone operation leaving its `clone` step, replacing the sandbox, cancelling the run, disabling the triggering user's
account, or cancelling or ending the Operation all take effect on the **next request**, including a
push already inside pre-receive. A long fetch is not re-authorized mid-transfer.

## Ref namespaces and write rules

| Namespace | Rule |
|---|---|
| `refs/ora/upstreams/<bindingId>/snapshots/<oid>` | create-only; the OID in the name must equal the target commit |
| `refs/ora/upstreams/<bindingId>/heads/<branch>` | tracking-cas: the old value must equal the expected old value recorded in the grant; the new value must already exist or be created as a snapshot in the same push; non-fast-forward is allowed |
| `refs/heads/workspaces/<workspaceId>/<branch>` | fast-forward: create or fast-forward |
| `refs/ora/deliveries/<deliveryWorkId>` | create-only |

- `pre-receive` resolves the same grant **again** before any ref is written and checks every ref
  against its rule. No ref can be deleted and no ref outside the table can be written; if any single
  update is not accepted, the whole push is refused.
- Sandbox executions produce the Workspace branch and delivery rules; an external-line sync produces the
  snapshot prefix and this Binding's tracking ref.
- Repository policy and the hook path are applied through `GIT_CONFIG_*` environment variables on every
  git process rather than written into the repository, so neither a restart nor an old repository can
  weaken them. Both repository listeners refuse ref deletion; the repository listener also refuses
  non-fast-forward updates, while the service repository listener allows them so tracking refs can
  follow an external force-push — non-fast-forward updates to every other namespace are still refused
  by the hook.
- A repository is created on its first authorized write (built in a staging location, then renamed).
  Neither object transfer nor the hook holds a database transaction.

## Ref queries

The ref query listener is open only to Cloud's fixed identity and is read-only:

- `GET ...?ref=<ref>`: the commit one ref in any namespace points to;
- `GET ...?prefix=<prefix>`: the refs and commits inside one namespace instance (one delivery, all of one
  Binding's refs or its snapshot or tracking refs, one Workspace).

The Cloud server calls the ref query with the address from its `git_server.*` settings and its own client
certificate (`internal/gitrefquery`, using `?ref=` only): outside any database transaction it checks that a
Revision's delivery ref names the final commit the Node declared, then registers the Revision (see
[Revisions](../agent-runs/revisions.en.md#the-delivery-flow)). Only `404 ref_not_found` counts as the ref being
absent; any other response, a transport failure or a timeout makes verification unavailable for now and it is retried,
never concluded. The query never follows redirects. Without a configured Git Server, clone steps stay `blocked`
(`git_server_unconfigured`), deliveries settle as `skipped`, and Cloud makes no ref query through this client.

This delivery path implements an `approved` Revision decision (see below).

The external Git line calls the ref query through a second client (`internal/gitrefs`) configured by
`repository.git_refs_*`, in three places: after a sync reports `applied` to verify the snapshot and tracking
refs, before creating a publish to confirm the source ref, and when listing a Workspace's collaboration
branches as publish sources (see [The external Git line](external-git.en.md)).

## Operations

- Bare repositories live on the persistent volume named by `GIT_SERVER_REPOSITORY_ROOT`. The image needs
  a `git` with `git http-backend`; without it startup fails.
- Startup verifies migrations first; shutdown drains the three listeners before closing the pre-receive
  hook channel.
- Listener addresses, certificates and CA files are configured as described in
  [`cmd/git-server`](../../cmd/git-server/README.en.md) and the
  [configuration reference](../operations/configuration.en.md#git-server).
- `cmd/git-server-preset` is an acceptance-only contract preset and assertion tool that writes the
  repository volume and the database directly; it ships only in the development image and must never be
  used in production.

## Related decisions

- [Git Server is the Project's code hub: a separate HTTPS service where certificates only authenticate and authorization is decided per request from Cloud's current state](https://github.com/ora-space/specs/blob/main/decisions/cloud/git-server/0-project-code-hub-git-server-over-https.md) (approved)
- [Git Server becomes every Project's code hub: ref namespaces, rule-set grants and the contract for two kinds of writer](https://github.com/ora-space/specs/blob/main/decisions/cloud/git-server/20261016-project-repository-as-code-hub-and-ref-contract.md) (approved)
- [A Workspace's clone step consumes the Binding's current snapshot](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20261016-clone-step-consumes-binding-snapshot.md) (approved)
- [The clone input becomes a Git Server snapshot input; the Git Server derives write rules from the execution kind](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261016-snapshot-clone-input-and-execution-ref-grants.md) (approved)
- [A Revision's code changes are delivered only through the Git Server, and Cloud registers the delivery ref after a ref query](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261017-git-native-delivery-refs-and-resume.md) (approved; supersedes the 20261012 delivery decision)
