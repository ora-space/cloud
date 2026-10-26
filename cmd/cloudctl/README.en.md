# cmd/cloudctl: Operational & Deployment CLI

[中文](README.md) | [English](README.en.md)

`cloudctl` is the restricted operational management tool for Ora Cloud. It runs with database operator credentials outside the public API path to manage database migrations, bootstrap initial tenants and administrators, and explicitly dispose of residual resources.

## Commands and responsibilities

### `migrate`
```sh
cloudctl -config <path> -command migrate
```
- Applies all unapplied forward migrations from `internal/core/migrations` in lexicographic order of complete filenames.
- Computes SHA256 checksums of migration scripts and records them in `schema_migrations`.
- Runs within transactional database-level advisory locks (`pg_advisory_xact_lock(67420911)`), making concurrent or repeated execution deterministic and safe.
- Rejects checksum mismatches on previously applied migrations with an error.

### `bootstrap`
```sh
cloudctl -config <path> -command bootstrap -name '<tenant-name>' -source '<idp-source>' -subject '<idp-subject>' -display-name '<display-name>'
```
- Atomically provisions an initial active tenant and its first administrator user in a single database transaction.
- Binds external IdP identity claims (`source` and `subject`) to an internal user record.
- **Note (Huawei IDaaS)**: When `-source` is `huawei-corp`, `-subject` must be the internal `uuid` returned by IDaaS (e.g. `uuid~...`), **never the employee number or W3 username**, or the user will create an unprivileged new user on login due to a subject mismatch.
- Assigns the user the `admin` role in `tenant_memberships`.
- Returns a JSON payload containing `tenantId` and `userId`.

See [External Git line](../../docs/repositories/external-git.en.md) for Git credential submission and reference management.

### `residual`

Place global `-config` before `residual` and action flags after the action. Every action requires `--tenant-id`. Queries and business commands do not need DDL privileges; access to the controlled host and database credentials governs access to this entry point. Core uses PostgreSQL `session_user` as the authenticated actor. Optional `--operator-label` is self-reported and cannot select or override that actor. The CLI accepts no database password flag and reuses existing configuration loading.

```sh
cloudctl -config <path> residual list --tenant-id <tenant> --project-id <project> --limit 100
cloudctl -config <path> residual get --tenant-id <tenant> --workspace-id <workspace>
cloudctl -config <path> residual get --tenant-id <tenant> --intent-id <intent>
```

- `list` intersects optional `--project-id`, `--run-id`, `--workspace-id`, `--operation-id`, and `--reclaim-id` filters, with `--limit` (default 100) and `--after`. It returns `items/nextCursor`; operation filtering can match either the original failed D8 delete or the currently bound deletion operation.
- `get` accepts one root/child `--intent-id`, or an exact `--run-id`, `--workspace-id`, or `--reclaim-id`. Other resource IDs may narrow a resource selector; an intent selector cannot be mixed with resource selectors. Queries return audit history, target evidence, and `openTargets` for external confirmation.

Each `list` item, actual residual ledger rows from `get`, and the resource output when present expose persisted `stop_evidence` as `stopEvidence`; the resource is returned only when no intent is selected. Without evidence the field is an empty object `{}`; legacy D8 list items without a ledger also return `{}`, while a synthetic legacy D8 residual in `get` may omit the field. An existing unconfirmed-stop record retains `kind=stop_unconfirmed`, `source=cloud`, `observedAt`, and the original-identity `executionId`. This field is display-only: it grants no termination, deletion, or ledger-clearance authority and does not change reclamation responsibility.

Termination and data deletion are separate commands, each requiring a reason, idempotency key, and affirmative confirmation. `terminate` requires at least a Run or Workspace; all additional IDs must identify the same original resource. D8 with a historical deletion requires the original failed delete `--operation-id`. Project/tenant selectors cannot authorize batch writes.

A D8 residual that reached `done/workspace_unavailable` before any deletion was created additionally requires explicit `--allow-missing-delete-operation`, both exact `--run-id` and `--workspace-id`, and no `--operation-id`. It cannot select a live Run, an absent delivery conclusion, any deletion history, open admission, or an existing residual ledger. The intent records `legacyDeleteOperationMissing=true` without inventing an operation; the default command still rejects this variant. Termination, fencing, and separately confirmed data deletion retain their evidence requirements.

```sh
cloudctl -config <path> residual terminate --tenant-id <tenant> --run-id <run> --workspace-id <workspace> --reason 'operator abandoned execution' --key <terminate-key> --impact-confirmed
cloudctl -config <path> residual terminate --tenant-id <tenant> --run-id <run> --workspace-id <workspace> --allow-missing-delete-operation --reason 'verified recovery copies retained' --key <missing-delete-key> --impact-confirmed
cloudctl -config <path> residual delete-data --tenant-id <tenant> --intent-id <root-intent> --reason 'delete retained data' --key <new-delete-key> --data-delete-confirmed
```

`terminate` registers termination authorization and retains data by default; acceptance does not mean sandboxes have terminated. The root intent succeeds with `terminated_data_retained` only after every target has genuine termination and late-ensure fencing confirmation. A later `delete-data` accepts only that completed root intent and separately registers a child intent and one fixed deletion operation; `released` requires actual deletion confirmation. It rejects bare Workspace deletion and does not substitute external confirmation for real termination evidence.

An unfinished intent whose disposal cannot be confirmed by the system may explicitly transfer its remaining responsibility. Read `openTargets` from `get` first and supply one `--external-fact` per unfinished target. Keys must be the exact `sandbox:<id>` or `data:<workspace-id>` values; values reference a ticket or another external disposal fact. Duplicate targets, empty references or explanations, and missing responsibility confirmation are rejected; core also checks for omitted targets.

```sh
cloudctl -config <path> residual confirm-external --tenant-id <tenant> --intent-id <unfinished-intent> --reason 'external disposal' --key <external-key> --evidence-note 'host decommissioned; data responsibility accepted' --external-fact 'sandbox:<id>=ticket-123' --external-fact 'data:<workspace-id>=ticket-456' --responsibility-transferred
```

External confirmation accepts only an existing unfinished intent and records a distinct `externally_confirmed` disposition. It manufactures no `terminated`, `lateEnsureFenced`, or `removed` physical evidence and does not authorize reuse of the same volume. Write keys are tenant-wide across actions: identical input returns current progress for the same responsibility; changing the action, target, reason, or confirmation under the same key returns `idempotency_conflict`.

## Boundaries and invariants

- **Deployment boundary**: `cloudctl` is not exposed via HTTP and is not invoked by the server daemon. It requires direct PostgreSQL access; only migration commands need schema migration privileges. Residual management adds no public tenant admin/member permissions, and public HTTP routes continue to reject hidden Run Workspaces.
- **Structured output**: All command results are output as structured JSON to `stdout`, and errors to `stderr`, enabling integration with CI/CD deployment pipelines.
- **Errors and credentials**: Residual commands preserve stable core Fault codes. Unknown infrastructure errors produce only `internal_error`; configuration or connection failures use safe generic codes without connection strings or passwords. Invalid arguments produce `invalid_cli_arguments`; `--impact-confirmed=false` does not confirm an action.

See [cmd overview](../README.en.md), [Database migrations](../../internal/core/migrations/README.en.md), and [Internal credentials](../../docs/identity/authentication.en.md).
