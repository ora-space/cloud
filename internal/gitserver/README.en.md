# gitserver: Git access to Project repositories

[中文](README.md) | [English](README.en.md)

Serves Project repositories (`/{tenantId}/{projectId}.git`) over smart HTTP only (root decision
`cloud/git-server/0` and the proposed follow-up "Git Server as each Project's code hub").
`internal/core` and PostgreSQL decide every permission; this package holds no authorization state.

| Files | Responsibility |
|---|---|
| `config.go` | Deployment references, listener TLS: the repository listener trusts only the sandbox CA, the service repository listener only the Repo Executor CA, and the ref listener pins Cloud's exact leaf |
| `scope.go` | `RuntimeScope` accepts only the `git-access` SPIFFE purpose; `RequireExecutor` accepts only `spiffe://ora.local/repo-executor` |
| `repository.go` | The four smart HTTP endpoints, canonical IDs, repository creation on first authorized write (staged then renamed), ref reads by exact name or prefix |
| `service.go` | Per-request authorization on both repository listeners (each accepts exactly one context header), per-listener repository configuration, `git http-backend` via CGI, the ref query and health |
| `rules.go` | Per-namespace write rules: create-only, fast-forward, tracking-cas |
| `hook.go` | One-time push tokens, the private Unix-socket hook channel and the `RunPreReceive` hook body |

Two repository listeners:

- **Repository listener** (sandbox network): sandbox git-access certificates; every request carries
  exactly `Ora-Execution-Id`.
- **Service repository listener** (internal service network): the Repo Executor's service
  certificate; every request carries exactly `Ora-Git-Operation-Id`.

The wrong header, no header or both headers are `400`. Core resolves the request into a `GitGrant`:
the whole Project repository to read, plus writable refs or prefixes, each with the rule its
namespace fixes. Reads hide no ref. Any denial is the same `403`, including an absent repository; a
database failure is `503` and never allows. `pre-receive` resolves the same grant again before any
ref is written and checks every ref:

| Namespace | Rule |
|---|---|
| `refs/ora/upstreams/<bindingId>/snapshots/<oid>` | create-only; the OID in the name must equal the target |
| `refs/ora/upstreams/<bindingId>/heads/<branch>` | tracking-cas: the old value equals the grant's expected old; the new value has a snapshot that exists or is created in the same push; non-fast-forwards allowed |
| `refs/heads/workspaces/<workspaceId>/<branch>` | fast-forward: create or fast-forward (the hook checks ancestry inside the quarantine) |
| `refs/ora/deliveries/<deliveryWorkId>` | create-only |

No ref is ever deleted and no ref outside the table is written; one update that no rule admits rejects
the whole push. Today sandbox executions produce only the delivery rule and a sync Operation only its
Binding's snapshot prefix; the internal and external lines each add their own rule sources for
Workspace branches and tracking refs.

Repository policy and the hook path are applied to every git process through `GIT_CONFIG_*`
environment variables rather than stored in repositories, so a restart (new hook directory) or an
older repository cannot weaken them. The repository listener keeps
`receive.denyNonFastForwards=true`; the service repository listener turns it off so a tracking ref can
follow an external force-push, and the hook still refuses non-fast-forwards in every other namespace.
Both keep `receive.denyDeletes=true`. Object transfer and hooks never hold a database transaction.

The ref query is read-only and only for Cloud's pinned identity: `?ref=` returns the commit of one ref
in any namespace; `?prefix=` lists the refs and commits inside one namespace instance (the deliveries,
one Binding's refs, snapshots or heads, or one Workspace).

Tests use the real git client, `git http-backend` and TLS with a policy double for the rule and
listener matrices; `integration/git_server_test.go` and `integration/git_operation_access_test.go`
drive a delivery and an executor sync from real Cloud state through the shipped command as the hook.
Long fetches are not re-authorized mid-transfer; writes are, in pre-receive.
