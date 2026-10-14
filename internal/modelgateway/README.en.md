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
| `transport.go`, `dns.go` | `NewUpstreamClient(UpstreamConfig)` fixes deployment-owned `DNSConfig`; authenticated HTTPS DNS and numeric public-IP dialing, without environmental proxies or redirects |
| `diagnostics.go` | Finite typed error categories without inspecting upstream text; `Options.Logger` records only the category |
| `config.go`, `health.go` | Deployment references, listener purposes and verified-TLS readiness |

Credential requests require the browser Gateway's service/user credentials. Grant requests require
a dedicated model-access client certificate; data requests require temporary runtime tokens.
Those purposes are not interchangeable. Original keys must never appear in responses or diagnostics.
SSE events flush continuously; error events inside HTTP200 are sanitized too. Revocation interrupts
the upstream and a blocked downstream write. Each watcher has one request owner, cancellation and a join.

Production DNS never falls back to the system resolver. Public numeric IPs bootstrap HTTPS DNS with
normal TLS verification. Every A/AAAA answer must pass address policy before numeric dialing, while
TLS verifies the original model hostname. Only exact deployment-owned development fixture hosts use
system DNS; user connection settings cannot enable this exemption. Both `2001:2::/48` and `198.18.0.0/15` are denied.

Raw keys stay in this service's memory. Ciphertext uses fresh nonces; runtime tokens persist only as
digests. Backups need the database and master-key volume. Startup refuses a key that cannot open
existing records rather than resetting previous encryption identities.

Tests use generated ephemeral secrets and actual TLS/HTTP boundaries for encryption, certificate
purpose, protocol streams, cancellation, safe diagnostics and address policy. The optional credential-free
`transport_network_test.go` probe establishes trusted DNS and verified TLS connectivity only; HTTP 401
does not establish model-call acceptance. Core's PostgreSQL tests
cover durable ownership and grant authority; cluster M4 supplies actual OpenCode/provider evidence.
See [personal model connections](../../docs/model-connections.en.md).
