# model-gateway: the Personal Key and Model Proxy Service

English | [中文](model-gateway.md)

model-gateway (`cmd/model-gateway`, implemented in `internal/modelgateway`) is a trusted service that runs
separately from the Cloud server and exclusively owns two things: encrypting and decrypting personal
upstream API keys, and forwarding Agent runs' model requests. Business ownership, configuration freezing,
run permissions and grant digests are still decided by Cloud core and PostgreSQL (see
[Personal model connections](model-access.en.md)), and model-gateway checks with core at every step; the
Controller and the Node only ever receive binding references and short-lived tokens.

## Three listeners

| Listener | Default address | Caller | Authentication |
|---|---|---|---|
| Credential listener | `:8083` | The browser-facing Gateway only | A service JWT and a user JWT freshly signed by the Gateway; handles only writing and clearing keys |
| Grant listener | `:8444` | The Node in a sandbox | Mutual TLS; the client certificate's URI must be `spiffe://ora.local/model-access/tenant/<tenantId>/runtime/<workspaceId>/generation/<n>` |
| Model data listener | `:8443` | The Agent in a sandbox | The short-lived token; forwards only `/runtime/openai/v1/chat/completions` and `/runtime/anthropic/v1/messages` |

The three entry points are not interchangeable: runtime identifiers in request bodies or headers cannot stand in
for the grant listener's certificate, and a short-lived token cannot be used on the credential listener. No
response or error ever contains a real key.

## Startup and shutdown

1. Read Cloud's configuration file (the database and the Gateway's verification keys) and this service's
   deployment environment variables.
2. Verify migrations, load the encryption master key and check the ciphertexts already in the database against
   it: when the master key does not match the encryption identity of existing ciphertexts, **startup fails**; the
   historical records are never reset.
3. Build the checked outbound client and open the three TLS listeners.
4. On shutdown, cancel in-flight model streams first, then wait for every listener to exit.

| Environment variable | Default / purpose |
|---|---|
| `MODEL_GATEWAY_CREDENTIAL_ADDR`, `_RUNTIME_ADDR`, `_GRANT_ADDR` | `:8083`, `:8443`, `:8444` |
| `MODEL_GATEWAY_PUBLIC_ORIGIN` | `https://ora-model-gateway:8443`, the proxy address returned to the Agent in grant responses (`proxyBaseUrl`) |
| `MODEL_GATEWAY_CERTIFICATE_FILE`, `_PRIVATE_KEY_FILE`, `_CA_FILE` | The service certificate and key, and the CA that verifies Node client certificates |
| `MODEL_GATEWAY_MASTER_KEY_FILE`, `_MASTER_KEY_ID` | The encryption master key file and its ID (default `v1`) |
| `MODEL_GATEWAY_DNS_HTTPS_URL`, `_DNS_BOOTSTRAP_IPS` | The outbound resolver, by default `https://cloudflare-dns.com/dns-query` and `1.1.1.1,1.0.0.1` |
| `MODEL_GATEWAY_DEVELOPMENT`, `_DEVELOPMENT_ALLOW_HOSTS` | Development acceptance only: with both set, exactly the listed hosts may use the system DNS and private addresses |

`-healthcheck` verifies HTTPS and the public CA normally and checks only database readiness, without reading any
user key or the master key. **Backups must include both the database and the master key volume**; without either,
stored keys cannot be recovered.

## Outbound policy

Model forwarding and Cloud workflow-asset HTTPS downloads read one
[shared deployment egress file](../operations/deployment-egress.en.md). With no file configured, public mode stays in
effect.

- **Public mode** uses only the deployment-configured, TLS-authenticated DNS-over-HTTPS (RFC 8484) resolver,
  bootstrapped by public numeric IPs; a resolution failure **never falls back** to system DNS. Users cannot choose the
  resolver. Only after every A/AAAA answer for the model host passes the address policy does the gateway dial a numeric
  IP; TLS still verifies the original host name. Private, link-local, documentation, translation and special-purpose
  ranges (including `198.18.0.0/15` and `2001:2::/48`) are all refused, and so are answers mixing public and private
  addresses.
- **Enterprise mode** permits only the exact host:port entries in the file. Unlisted public and private targets are
  refused before DNS; a private address inside an entry is still refused when it hits topology or a reserved range.
  Resolution uses the file's DNS-over-TCP resolver or that target's static addresses, reads neither system DNS nor the
  public DoH settings above. Enterprise mode together with `MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS` refuses to start.
  Application transport does not attempt HTTP/2.
- Neither mode uses environment proxies or follows redirects. Idle connections expire after 60 seconds in both modes.
  Only enterprise connections have a 30-minute hard lifetime; public connections retain their existing unbounded lifetime.
  Enterprise expiry closes an active connection and does not automatically resend a model generation.
- Refusal codes distinguish policy refusal, resolution failure, TLS verification failure, timeout, redirect and an
  unreachable upstream; logs record only the safe category, never the raw URL, resolved addresses, request body,
  authentication headers or the provider's error text.

A connection save that passed enterprise admission does not let a later forward skip its own outbound check. The Cloud
server downloads workflow plugin assets with the same checked client and the same policy (see
[Workflows](../workflows/workflows.en.md#plugin-asset-import)).

## Forwarding

The Claude Code 2.1.303 Messages surface is bounded below; supported beta tokens are defined
once in the [protocol policy](../../internal/modelgateway/anthropic.go). Unknown or duplicate
Anthropic protocol-header values fail. Client diagnostic headers, session identifiers and the
browser-access marker never reach the supplier.

The same pinned CLI also declares `structured-outputs-2025-12-15` and
`thinking-display-updates-2026-08-18` when queries are created through Agent SDK 0.3.303 /
ACP adapter 0.94.0, and `per-turn-control-2026-07-01` on its first request for
`claude-haiku-5-5`. The pinned OpenCode 1.19.3 declares `thinking-binding-controls-2026-08-01`
and `structured-outputs-2025-11-13` when it requests a Claude model through
`@ai-sdk/anthropic`. All of these are included in the bounded whitelist. An upgrade must verify its
observed protocol surface before extending the policy.

| Method and path | Query | Behavior |
|---|---|---|
| POST /runtime/anthropic/v1/messages | None, or exactly beta=true | Forward the frozen model, replace authentication, preserve supported version/beta headers |
| GET /runtime/anthropic/v1/models | None, or exactly limit=1000 | Recheck authorization and return only the bound model without an upstream request |

The listing is a view of the platform's frozen model, not evidence of supplier discovery. It
decrypts no key and records no inference attempt. count_tokens and other queries, paths and
methods are explicitly refused. Refusing optional probes does not relax inference, authorization
or outbound policy.

- Only the model and paths fixed by the grant are forwarded, and the protocol's request envelope is validated;
  successful JSON and SSE bytes are forwarded unchanged, with SSE flushed event by event.
- Error events inside an HTTP 200 are normalized into safe errors too; ACP authentication failures
  (`Authentication required`, `Not logged in`) are classified as `agent_authentication_required`, and other
  providers' raw text is never exposed.
- The grant is re-checked on every forward and throughout long streaming forwards (see
  [Short-lived grants](model-access.en.md#short-lived-grants)); a revocation interrupts both the upstream request and
  any blocked downstream write. Each grant watcher belongs to one request, which waits for it to exit after
  cancellation.
- The gateway **never retries** model calls. Every upstream HTTP attempt is registered before it is sent, and its
  status, duration and usage are written when it finishes; when registration fails the answer is
  `model_metrics_unavailable` and the upstream is never called. Completion uses a separate 3-second context and
  submits the same immutable result at most three times, leaving `started` if it still fails. The statistics'
  definitions are in [Model request statistics](../agent-runs/session-reads.en.md#model-request-statistics).

## Related decisions

- [Personal model connections are configured by users; the platform keeps the keys and proxies each run's model requests](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/0-personal-model-connections-and-run-scoped-proxy.md)
- [Model usage is recorded per gateway HTTP attempt, keeping partial observations and unknown values](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261016-recorded-http-attempt-statistics.md) (approved)
- [Model forwarding and Cloud asset downloads share a deployment-owned egress allowlist and explicit resolution](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-deployment-owned-egress-and-explicit-resolution.md) (approved)
