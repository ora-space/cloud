# internal: Authoritative Cloud Subsystems

[中文](README.md) | [English](README.en.md)

`internal` hosts the private implementation packages for Ora Cloud. Per the repository architectural boundary rules documented in `AGENTS.md`, all core state, policy, translation, and infrastructure adapters remain private under `internal/`.

## Module map

- [core](core/README.en.md) is the authoritative domain core, owning business state machines, transaction boundaries, database advisory locks, and cryptographic authentication.
  - [migrations](core/migrations/README.en.md) contains ordered, forward-only PostgreSQL schema migration scripts and checksum verification.
- [api](api/README.en.md) is the HTTP presentation layer.
  - [router](api/router/README.en.md) binds HTTP routes, verifies two-tier JWT credentials, parses JSON request bodies, and projects domain errors into stable contracts.
- [gateway](gateway/README.en.md) is the public authentication boundary: PostgreSQL-backed Login Attempts and Browser Sessions, provider-neutral login orchestration, internal JWT issuance, cookie/CSRF/redirect protection, and the `/api/v1` proxy.
  - [idaas](gateway/idaas/README.en.md) is the Huawei IDaaS 2.0 (`client_secret_post`) adapter that produces a `VerifiedIdentity` with `source=huawei-corp`.
  - [github](gateway/github/README.en.md) is the GitHub OAuth App adapter that produces a `VerifiedIdentity`.
  - [devlogin](gateway/devlogin/README.en.md) is the development-only provider: a local form where any typed identity signs in, registered solely on loopback development origins.
- [contract](contract/README.en.md) defines OpenAPI 3.0 schema models and contract coverage tests.
- [repository](repository/README.en.md) manages PostgreSQL database connection pools and startup health checks via GORM.
- [objectstore](objectstore/README.en.md) is cloud's own S3-compatible object-store client (SigV4-presigned upload grants, HEAD verification of an object's size and SHA-256), serving Revision delivery, archive reads and workflow asset reads.
- [gitpreset](gitpreset/README.en.md) prepares and checks Git Server contract fixtures for acceptance only, writing the repository volume and database directly; no service process imports it.
- [gitserver](gitserver/README.en.md) serves Git access to Project repositories: the sandbox and Repo Executor repository listeners, per-request ref rule sets from Core, `git http-backend`, per-namespace pre-receive rules and Cloud's ref query.
- [gitrefquery](gitrefquery/README.en.md) is Cloud's client for the Git Server's read-only ref query (`GET /internal/v1/refs`), verifying outside any transaction that a ref Cloud is about to register exists and names the declared commit.
- [repoexec](repoexec/README.en.md) is the only Ora program that contacts external Git repositories: a fresh bare repository per task, sync/publish/probe execution, reporting observed facts while Cloud decides the Operation's state.
- [gitrefs](gitrefs/README.en.md) is the client of the Git Server's read-only ref query (contract D6): TLS 1.3 mutual authentication, bounded answers, and an unavailable server kept distinct from a missing ref.
- [repoworker](repoworker/README.en.md) drives repository Operations: reading the tracking ref, signing the one-time authorization, renewing and losing leases, and settling syncs and publishes against the lease generation.
- [cloneauth](cloneauth/README.en.md) holds the external Git line's one-time grants: Ed25519 signing and strict verification of `sync`/`publish`/`probe` execution authorizations and browser `submit` grants, `NormalizeRepository`, and key loading.
- [credentialmaterial](credentialmaterial/README.en.md) is the Repo Executor's per-task credential material directory (0700 directory, 0600 exclusive files, fsync, verified removal, startup sweep), the layout shared with `git-credential-ora`.
- [processgroup](processgroup/README.en.md) puts a Git child at the head of a new process group, kills the whole group with `SIGKILL` on cancellation and sets `WaitDelay`.
- [capture](capture/README.en.md) is a bounded buffer for child-process output: keeps the first N bytes, counts the rest, and never fails a write.
- [credentialstore](credentialstore/README.en.md) is the independent Credential Store: KEK keyring and envelope encryption, an atomically written credential volume, one-time redemption and submission claims, one-hour version retention and revocation that destroys ciphertext; the redemption, management, key and browser-ingress listeners, plus the clients Cloud and the executor use.
- [config](config/README.en.md) loads and validates application configuration files and environment overrides.
- [logger](logger/README.en.md) provides structured JSON logging via Zap and Lumberjack.
- [simulator](simulator/README.en.md) implements in-process doubles for the Substrate execution engine, Controller, and Workspace Node.
- [controlpb](controlpb/README.en.md) holds the gRPC Go code (server stubs and messages) generated from the Controller internal control contract under [`proto/`](../proto/README.en.md); read-only.
- [controlgrpc](controlgrpc/README.en.md) serves that contract over gRPC: the caller-identity interceptor, `Fault` → status mapping, the lease service; translation only, no business rules.
- [collab](collab/README.en.md) provides explicitly enabled development collaboration fixtures.
- [workflowasset](workflowasset/README.en.md) downloads verified plugin packages and reads workflow documents without database access.
- [egress](egress/README.en.md) owns shared deployment egress parsing, target checks, DNS and dialing.
- [modelgateway](modelgateway/README.en.md) owns personal-key encryption and model request forwarding.

## Architecture

See [Architecture](../docs/architecture.en.md), [Persistence](../docs/core/persistence.en.md) and [AGENTS.md](../AGENTS.md) for dependency direction, ownership and transaction rules. Package READMEs describe local implementation; the [document map](../docs/README.en.md) links cross-module contracts.
