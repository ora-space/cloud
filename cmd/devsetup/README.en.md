# cmd/devsetup: one-shot local development setup

[中文](README.md) | [English](README.en.md)

`cmd/devsetup` (`task setup`) exists only for local development: it turns the repository-root `config.toml` into everything the real Gateway and Cloud need to start on a laptop, so nobody runs `openssl`, assembles environment variables or writes secret files by hand. Production keys and configuration come from the deployment infrastructure; this command plays no part there.

## Responsibilities

Driven by `config.toml` (copied from `config.toml.template`, ignored by Git), it:

- Generates two purpose-separated Ed25519 key pairs under `.local/gateway/` (`-dir` overrides): `gateway-service.key`/`.pem` and `user-identity.key`/`.pem`. The PKCS#8 private keys go to the Gateway (`configs/gateway.yaml`); the PKIX public keys go to Cloud (`auth.keys` in `configs/config.yaml`). It also generates a random 32-byte `gateway-pkce.key`.
- Writes the filled-in external provider's `client_secret` (`[github]` or `[idaas]`, at most one) to `.local/gateway/<provider>-client-secret` (the file the Gateway's matching `client_secret_file` points at) and removes the other, stale one; with both empty the Gateway offers only the development login.
- Writes `.local/dev.env` (`-env` overrides): `CLOUD_DATABASE_DSN`, `GATEWAY_DATABASE_DSN` and `TEST_DATABASE_URL` from `[database].dsn`, `GATEWAY_LOGIN_PROVIDER` from whichever section is filled (`github` / `huawei-idaas` / empty), `GATEWAY_GITHUB_CLIENT_ID` from `[github]`, `GATEWAY_IDAAS_BASE_URL` / `GATEWAY_IDAAS_CLIENT_ID` / `GATEWAY_IDAAS_DISPLAY_NAME_FIELD` from `[idaas]`. `Taskfile.yml` loads it through `dotenv` for every task, so `task dev`, `task migrate`, `task test` and `task check` share one set of values; variables already set in the shell take precedence.
- Applies the database migrations with the same DSN, through the same path as `cloudctl migrate`.

`configs/config.yaml` and `configs/gateway.yaml` remain the authoritative service configuration; `config.toml` only supplies the per-machine values, and only through the environment overrides and secret file the services already accept.

## Boundaries and invariants

- **Strict parsing**: an unknown key in `config.toml` fails with its location; `database.dsn` is required; within `[github]` and `[idaas]`, `client_id` / `client_secret` are set together or left empty together, and the two sections are never both filled (the Gateway has one external provider).
- **Rerunnable**: existing keys are always kept; a pair's private and public key must exist together, otherwise the command refuses and asks for both to be removed, so the Gateway and Cloud never trust drifting keys. The secret file and `.local/dev.env` are rewritten from `config.toml` on every run, so editing `config.toml` and rerunning is enough. Migrations are idempotent.
- **Never prints secrets**: only "generated"/"kept"/"wrote" file names, never private keys, the client secret or the DSN, which may carry a password.
- **Writes only under `.local/`**: that directory and `config.toml` are ignored by Git; every file is `0600`, and nothing generated reaches the repository, logs or the database.
- **Not for deployment**: there is no rotation, custody or distribution.

See the [cmd overview](../README.en.md) and the [Gateway document](../../docs/identity/gateway.en.md).
