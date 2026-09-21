# internal/gateway: Public Authentication Boundary

[中文](README.md) | [English](README.en.md)

`internal/gateway` implements the Gateway's login orchestration, PostgreSQL session store, internal credential issuance, browser security policy, and Cloud proxy. It is provider-neutral: only `Authenticator` adapters for [Huawei IDaaS](idaas/README.en.md) and [GitHub](github/README.en.md) understand external protocols; everything else consumes a `VerifiedIdentity` or a resolved `Session`.

## Files and responsibilities

| File | Responsibility |
|---|---|
| `identity.go` | `VerifiedIdentity`, the `Authenticator` interface, and `Normalize` (validates source/subject against Cloud's 128/512-byte limits and truncates the display name to 200 bytes on a rune boundary). |
| `store.go` | `Store`: `CreateAttempt`, `LookupAttempt` (lock-free precheck), `ConsumeAttempt` (`FOR UPDATE` lock, `consumed_at`, and session insert in one transaction), `Resolve`, `Revoke`, `RevokeIdentity`, `Cleanup`. Only SHA-256 digests are stored; validity is decided by `clock_timestamp()`. |
| `login.go` | `Login`: `Start` generates the attempt secret and `state` and derives the PKCE verifier (HMAC with a Gateway-only key); an omitted provider selects the one configured adapter; `Callback` checks the attempt, calls the provider outside any transaction, then consumes the attempt and creates the session. All failures collapse into `ErrLoginFailed`. |
| `tokens.go` | `Issuer`: signs `kind=service,role=gateway` and `kind=user` (`caller` bound to the service `sub`) credentials with two purpose-separated Ed25519 keys, never beyond Cloud's 5-minute ceiling. `LoadPrivateKey` reads PKCS#8 PEM. |
| `security.go` | `NormalizeReturnTo` (rejects absolute, `//`, `/\`, backslash, and control characters), `SameOrigin` (exact `Origin` match or `Sec-Fetch-Site: same-origin`), `CookiePolicy` (`__Host-` session cookie, callback-scoped `SameSite=Lax` attempt cookie). |
| `handler.go` | Gin routes: `POST /auth/login`, `GET /auth/callback/:provider`, `POST /auth/logout`, `ANY /api/v1/*`, `GET /healthz`; stable error shape `{code, params, requestId}`. |
| `proxy.go` | `httputil.ReverseProxy` to the fixed upstream: dial/response-header timeouts, 8 MiB response cap, per-request error callback carried in the context. |
| `ratelimit.go` | Per-client token bucket with a bounded key table; start/callback are limited before any attempt row is written. |
| `cleanup.go` | `RunCleanup`: lifecycle-owned bounded batch cleanup loop. |
| `config.go` | `Config`/`LoadConfig`/`Validate`/`PublicOrigin`: defaults and startup validation of every security bound. |

## Invariants

- Raw session tokens, attempt secrets, OAuth codes, provider tokens, and PKCE verifiers are never persisted, logged, or sent to Cloud.
- External HTTP calls never run inside a database transaction or row lock; a provider success followed by a failed transaction is a failed login, never an inferred session.
- Each attempt creates at most one session; of two concurrent callbacks only one passes the unconsumed check after `FOR UPDATE`.
- `Resolve` never extends a lifetime; unknown, expired, and revoked sessions are uniformly `ErrNotFound`/`401 unauthenticated`.
- A valid session cannot bypass Cloud's decisions about disabled users, membership, or ownership; Cloud's 401/403 pass through unchanged.

## Tests

Unit tests cover `return_to`/origin/cookie policy, identity normalization, credential issuance verified by Cloud's authenticator, configuration bounds, and rate limiting. `integration/gateway_test.go` uses real HTTP, PostgreSQL, the real Cloud router, and a PKCE-verifying fake provider to cover login, proxying, forged headers, cross-site mutations, replay, concurrent consumption, multiple replicas, expiry, revocation, cleanup, provider/Cloud failures, and database constraints.

See the [internal overview](../README.en.md), the [Huawei IDaaS adapter](idaas/README.en.md), the [GitHub adapter](github/README.en.md), and the [Gateway document](../../docs/gateway.md).
