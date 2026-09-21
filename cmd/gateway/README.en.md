# cmd/gateway: Authentication Gateway Daemon

[中文](README.md) | [English](README.en.md)

`cmd/gateway` is the browser-facing authentication and reverse-proxy entrypoint implementing `specs/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md`. It is limited to configuration, dependency wiring, lifecycle, and exit behavior; login orchestration, session policy, the proxy, and provider adapters live in `internal/gateway`.

## Responsibilities

- **Configuration & logging**: Loads `configs/gateway.yaml` through `gateway.LoadConfig` (overridable with `GATEWAY_*` environment variables), validates that the public origin is HTTPS (loopback development excepted), that the session lifetime never exceeds 90 days and internal credentials never exceed 5 minutes, and initializes the Zap logger.
- **PostgreSQL and migration gate**: Connects through `internal/repository.InitDB` and runs `core.Store.CheckSchema` to verify every migration in `internal/core/migrations` is applied with a matching checksum; it never runs DDL.
- **Key loading**: Reads two PKCS#8 Ed25519 private keys (separate service and user purposes), the PKCE derivation key, and only the selected provider's client secret from files into process memory. An unselected provider needs no usable secret.
- **Boundary assembly**: Selects the Huawei IDaaS or GitHub adapter from `login.provider`, then builds `gateway.Store`, `gateway.Login`, `gateway.Issuer`, the rate limiter, and `gateway.NewHandler` before listening on the configured port.
- **Bounded cleanup**: Starts a lifecycle-owned goroutine that deletes expired/revoked sessions and expired attempts past the audit window in batches, cancelling and waiting for it on shutdown.
- **Graceful shutdown**: Handles `SIGINT`/`SIGTERM`, drains in-flight requests within 10 seconds, then closes the pool and flushes logs.

## Boundaries and invariants

- Only `/api/v1/*` is relayed, and only to the single configured Cloud upstream; `/internal/v1/*` has no route.
- Browser-supplied `Authorization`, `X-Ora-User-Token`, `Cookie`, and `X-Forwarded-*` never reach Cloud; every request gets freshly signed minute-scale service/user JWTs from this replica's keys.
- At runtime it touches only `gateway_login_attempts`, `gateway_sessions`, and `schema_migrations`.
- Huawei intranet deployments use IDaaS's documented Authorization Code flow (confidential client). W3 login creates only a local Cloud session, and logout does not end the W3/IDaaS SSO session.

See the [cmd overview](../README.en.md), [authentication boundary (`internal/gateway`)](../../internal/gateway/README.en.md), and the [Gateway document](../../docs/gateway.md).
