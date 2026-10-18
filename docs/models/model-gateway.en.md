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

Model forwarding connects to addresses users typed in, so every outbound connection has to be kept from being
steered into internal networks:

- Only the deployment-configured, TLS-authenticated DNS-over-HTTPS (RFC 8484) resolver is used, bootstrapped by
  public numeric IPs; a resolution failure **never falls back** to system DNS. Users cannot choose the resolver.
- Only after every A/AAAA answer for the model host passes the address policy does the gateway dial a numeric IP;
  TLS still verifies the original host name. Private, link-local, documentation, translation and special-purpose
  ranges (including `198.18.0.0/15` and `2001:2::/48`) are all refused, and so are answers mixing public and
  private addresses.
- Environment proxies are ignored and redirects are never followed.
- Refusal codes distinguish policy refusal, resolution failure, TLS verification failure, timeout, redirect and an
  unreachable upstream; logs record only the safe category, never the raw URL, resolved addresses, request body,
  authentication headers or the provider's error text.

The Cloud server downloads workflow plugin assets with the same checked client and the same address policy (see
[Workflows](../workflows/workflows.en.md#plugin-asset-import)).

## Forwarding

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
