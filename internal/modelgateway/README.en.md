# modelgateway: personal credentials and runtime model proxy

[中文](README.md) | [English](README.en.md)

This module exclusively encrypts/decrypts upstream model API keys and forwards model requests.
`internal/core` and PostgreSQL own configuration, frozen run bindings, current authorization and
token digests. Controller and Node receive only opaque references and temporary runtime access.

| Files | Responsibility |
|---|---|
| `crypto.go`, `key_verification.go` | AES-256-GCM records, retry fingerprints and existing-key verification |
| `credentials.go` | Gateway-only credential writes with separately verified service/user claims |
| `grants.go` | Purpose-specific model-access mTLS grants, same-token renewal and revocation |
| `forward.go`, `events.go`, `envelopes.go`, `response_io.go` | Restricted protocol/model forwarding, envelope validation, safe errors and blocked-write cancellation |
| `transport.go` | No environmental proxy/redirects; DNS/public-address validation |
| `config.go`, `health.go` | Deployment references, listener purposes and verified-TLS readiness |

Credential requests require the browser Gateway's service/user credentials. Grant requests require
a dedicated model-access client certificate; data requests require temporary runtime tokens.
Those purposes are not interchangeable. Original keys must never appear in responses or diagnostics.
SSE events flush continuously; error events inside HTTP200 are sanitized too. Revocation interrupts
the upstream and a blocked downstream write. Each watcher has one request owner, cancellation and a join.

Raw keys stay in this service's memory. Ciphertext uses fresh nonces; runtime tokens persist only as
digests. Backups need the database and master-key volume. Startup refuses a key that cannot open
existing records rather than resetting previous encryption identities.

Tests use generated ephemeral secrets and actual TLS/HTTP boundaries for encryption, certificate
purpose, protocol streams, cancellation, safe diagnostics and address policy. Core's PostgreSQL tests
cover durable ownership and grant authority; cluster M4 supplies actual OpenCode/provider evidence.
See [personal model connections](../../docs/model-connections.en.md).
