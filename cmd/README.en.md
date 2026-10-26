# cmd: Command Entrypoints

[中文](README.md) | [English](README.en.md)

`cmd` hosts the command-line and daemon entrypoints for Ora Cloud. Packages in this directory are
strictly limited to runtime configuration loading, dependency injection, process lifecycle wiring,
operating-system signal handling, and process exit codes.

## Module map

- [server](server/README.en.md) is the primary authoritative HTTP API service daemon.
- [gateway](gateway/README.en.md) is the browser-facing authentication and reverse-proxy boundary: IDaaS/GitHub and development providers, PostgreSQL sessions, and internal credential issuance.
- [cloudctl](cloudctl/README.en.md) is the restricted deployment and operations CLI for migrations and tenant bootstrap.
- [devsetup](devsetup/README.en.md) is the development-only one-shot setup (`task setup`): from `config.toml` it generates the Gateway private keys, the public keys Cloud trusts and the PKCE key, writes the selected external provider's client secret file and `.local/dev.env`, and applies migrations.
- [git-server](git-server/README.en.md) serves Project repositories to sandboxes and the Repo Executor over smart HTTP, authorizing every request from current Cloud state, and answers Cloud's read-only ref query.
- [git-server-preset](git-server-preset/README.en.md) prepares and checks Git Server contract fixtures for acceptance only; it ships only in the development image.
- [repo-executor](repo-executor/README.en.md) is the only process that reaches external Git repositories, running sync, explicit publish and read-only probes; it is stateless and has no database, accepting only Cloud's mTLS tasks.
- [git-credential-ora](git-credential-ora/README.en.md) is the Repo Executor's fixed Git credential helper; it delivers the credential only when the requested https repository equals the staged scope.
- [credential-store](credential-store/README.en.md) runs the independent Credential Store: the redemption, management and key mTLS listeners and the browser ingress; it refuses to start without a keyring and periodically sweeps expired versions.
- [simulator](simulator/README.en.md) provides an all-in-one local demo environment backed by in-process Substrate and Git execution doubles.
- [openapi](openapi/README.en.md) compiles and synchronizes the canonical OpenAPI 3.0 specification (`api/openapi.json`) from Go contract definitions.
- [checkformat](checkformat/README.en.md) enforces repository Go formatting standards as a strict failing CI gate.
- [model-gateway](model-gateway/README.en.md) runs personal-key storage and authorized model proxying.
- [demo-issue-board](demo-issue-board/README.en.md) prints a one-shot board demo in an isolated PostgreSQL schema.
- [demo-issue-board-web](demo-issue-board-web/README.en.md) serves the embedded browser board with ephemeral demo data.
- [ora-web](ora-web/README.en.md) serves the frontend through a development edge with a persistent local schema.

## Composition boundary

Service commands load configuration, wire dependencies and own process shutdown; domain policy belongs in [core](../internal/core/README.en.md). Operational tools and development demos have their own data access and migration behavior documented in their module READMEs. See [Architecture](../docs/architecture.en.md), [Deployment](../docs/operations/deployment.en.md) and [AGENTS.md](../AGENTS.md).
