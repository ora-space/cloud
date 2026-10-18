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
| `attempt.go`, `usage.go` | Durable HTTP-attempt intent, monotonic timing, protocol usage snapshots and bounded idempotent finalization |
| `config.go`, `health.go` | Deployment references, listener purposes and verified-TLS readiness |

Credential requests require the browser Gateway's service/user credentials. Grant requests require
a dedicated model-access client certificate; data requests require temporary runtime tokens.
Those purposes are not interchangeable. Original keys must never appear in responses or diagnostics.
SSE events flush continuously; error events inside HTTP200 are sanitized too. Revocation interrupts
the upstream and a blocked downstream write. Each watcher has one request owner, cancellation and a join.

Forwarding calls Core `BeginModelAttempt` before `Upstream.Do`. Failure returns the safe
`model_metrics_unavailable` error without dispatching. A row records one gateway dispatch intent,
not proof of remote acceptance, a user turn or an inferred logical request. The gateway never retries
model calls. Finalization uses a detached three-second context and at most three writes of the same
attempt ID and immutable result. Exhausted writes leave `started` and log only the safe ID. A process
crash or failure between registration and dispatch can also leave `started`; it is neither success nor
evidence of zero usage.

Successful JSON and SSE bytes remain unchanged. Observation persists only allowlisted integer
counters, statuses and durations, never content, tool arguments, upstream headers, keys or runtime
tokens. Cumulative snapshots replace prior counters; repeated final frames are not added. Missing,
negative, fractional or browser-unsafe integers remain unknown. OpenAI counters are `prompt_tokens`,
`completion_tokens`, `total_tokens`, `prompt_tokens_details.cached_tokens/cache_write_tokens` and
`completion_tokens_details.reasoning_tokens`; input includes cache. Anthropic counters are
`input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens` and reported
`output_tokens_details.thinking_tokens`; input excludes cache and total remains unknown. Reasoning
is an output detail, never an additional total. This module neither estimates prices nor stores
provider-specific cost extensions.

OpenAI streaming accepts the standard final usage chunk with empty `choices`, and OpenRouter's
documented accounting frame with content-free `delta` and completed `finish_reason` choices.
Unfinished or error choices do not establish final usage. Anthropic combines `message_start` with
cumulative `message_delta` updates. Terminal observation is independent: OpenAI `[DONE]` and Anthropic
`message_stop`. Clean EOF without a terminal preserves forwarding behavior but records `incomplete`.
Complete usage additionally requires a final snapshot with known input/output; otherwise availability
is `partial` or `unavailable`. Provider errors, cancellation, revocation and upstream/downstream
interruptions retain safe outcomes and already observed partial usage. Existing `stream_options`
remain intact, including `include_usage:false`; no option is injected for an unknown compatible provider.

Duration starts immediately before dispatch after registration, using Go's monotonic clock. First
semantic output ignores keepalives, empty role/text and usage-only chunks; it recognizes protocol text,
refusal, tool content, Anthropic thinking and explicit compatible `reasoning/reasoning_content`
strings; their text is never used to estimate token counters. Non-streamed JSON permits observation only after its
complete response arrives, not token-level TTFT. This metric does not include Agent tool time or
reconstruct compaction, permission waits, subagents or SDK logical retries.

Accounting follows [OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create),
[OpenRouter final usage chunks](https://github.com/OpenRouterTeam/docs/blob/main/api_reference/streaming.mdx),
[Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming),
[prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) and
[thinking usage](https://platform.claude.com/docs/en/build-with-claude/thinking-steering-and-cost).

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
cover durable ownership and grant authority. See [model-gateway](../../docs/models/model-gateway.en.md)
and [personal model connections](../../docs/models/model-access.en.md).
`attempt*_test.go` and `usage_test.go` also cover registration before dispatch, repeated snapshots,
unknown versus zero, monotonic timing, finalization after cancellation, failures without redispatch
and byte-preserving forwarding. Their controlled upstreams are synthetic, not evidence of real billing.
