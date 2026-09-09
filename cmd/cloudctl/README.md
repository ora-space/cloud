# cmd/cloudctl: Operational & Deployment CLI

`cloudctl` is the restricted operational management tool for Ora Cloud. It runs with database operator credentials outside the public API path to manage database migrations, provision initial administrative tenants, and register infrastructure secret references.

## Commands and responsibilities

### `migrate`
```sh
cloudctl -config <path> -command migrate
```
- Applies all unapplied forward migrations from `internal/core/migrations` in strict numerical order.
- Computes SHA256 checksums of migration scripts and records them in `schema_migrations`.
- Runs within transactional database-level advisory locks (`pg_advisory_xact_lock(67420911)`), making concurrent or repeated execution deterministic and safe.
- Rejects checksum mismatches on previously applied migrations with an error.

### `bootstrap`
```sh
cloudctl -config <path> -command bootstrap -name '<tenant-name>' -source '<idp-source>' -subject '<idp-subject>' -display-name '<display-name>'
```
- Atomically provisions an initial active tenant and its first administrator user in a single database transaction.
- Binds external IdP identity claims (`source` and `subject`) to an internal user record.
- Assigns the user the `admin` role in `tenant_memberships`.
- Returns a JSON payload containing `tenantId` and `userId`.

### `credential-ref`
```sh
cloudctl -config <path> -command credential-ref -tenant '<tenant-uuid>' -owner '<user-uuid>' -secret-ref 'infra-secret://git/team/account'
```
- Registers an infrastructure credential reference for Git operations.
- Enforces foreign key constraints: the target user must be an active member of the specified tenant.
- **Security invariant**: Only stores the URI/reference string (`secret-ref`). It never accepts, logs, or stores plaintext passwords, tokens, or private keys.

## Boundaries and invariants

- **Deployment boundary**: `cloudctl` is not exposed via HTTP and is not invoked by the server daemon. It requires direct network access to PostgreSQL with schema migration permissions.
- **Structured output**: All command results are output as structured JSON to `stdout`, and errors to `stderr`, enabling integration with CI/CD deployment pipelines.

See [cmd overview](../README.md), [Database migrations](../../internal/core/migrations/README.md), and [Authentication and trust](../../docs/authentication.md).
