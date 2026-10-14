# Personal model connections and runtime authorization

[中文](model-connections.md) | [English](model-connections.en.md)

`/api/v1/me/model-connections` manages private connections and model lists; `/api/v1/me/model-default`
selects the current user's default. They are independent of tenant membership, so configuration is
available before joining a space. Updates require the current `version`; creation/deletion use a
user-scoped `Idempotency-Key`. Model IDs are opaque strings and preserve slashes exactly.

Supported protocols are `openai-completions` and `anthropic-messages`. Upstreams must use public HTTPS;
authentication is `bearer`, or `x-api-key` for Anthropic. Metadata requests never accept secrets or
custom authentication headers. Reads expose only `credentialConfigured`. Gateway sends credential
writes directly to independent model-gateway; Cloud HTTP rejects these routes before reading their
bodies. Dedicated Core service methods accept ciphertext only and never receive or decrypt API keys.
Credential PUT writes and DELETE clears both require a user-scoped `Idempotency-Key` and the current
resource version.

Additive migration 0033 stores connection metadata, user defaults, immutable encrypted credentials,
run bindings and temporary token digests. Historical runs remain unchanged. Replacing the current
credential preserves immutable references used by existing runs. For idempotent secret writes,
model-gateway derives a secret-bound opaque credential ID: Core compares only that ID, version and
service key ID, without hashing plaintext or randomized ciphertext.

The transaction creating an `official/ora-space.opencode` run freezes its initiator, connection
version, protocol, URL, authentication mode, model, encrypted credential reference and Git author
identity. Missing defaults, credentials or available connections roll back the comment, interaction
and run together. Other agents retain existing behavior. Controller `AgentSessionSpec.model_binding_id`
carries only an opaque reference; neither temporary grants nor upstream keys enter control messages.

A session can freeze both a personal-model binding and a prior Revision. The published Revision
wire field remains 6; the model reference uses separate field 7. Controller independently checks
model-proxy and Revision-restore capabilities. The two `0033` migrations retain their complete
filenames and SQL, tracked independently by filename and checksum.

model-gateway passes the dedicated verified mTLS certificate's tenant, Workspace and generation to
Core. Core checks registered session execution, current Node, run reservation, run/Thread lifecycle,
tenant membership, user and connection status. PostgreSQL stores only SHA-256 token digests with a
15-minute expiry on its own clock. Renewal extends the same grant without changing its token.
Authorization is checked on each request and throughout streaming. Account disablement, leaving the
tenant, credential clearance, connection disablement/deletion, session ending and force-stop reject
subsequent requests. Clearing a credential or disabling/deleting a connection also durably revokes
existing grants in the same transaction.

Only after certificate scope, execution, account, membership, connection and current Node/runtime
authority all pass does an ending session or pending cancellation return `403 model_session_ending`.
This lets Node await the durable EndSession command and preserve its reason. Invalid scope or revoked
authority keeps the general denial and cannot masquerade as normal shutdown. Model access uses the
already-cloned checkout and does not require fresh remote Git credentials.

Thread reads add `initiatorUserId`, nullable `model{connectionName,modelId,modelName}`, and
`canAppend`/`canEnd`. Only a personal-model run's initiator may append turns; current administrators
may also end it. Other authorized members may still read. Historical and Echo runs retain their
existing permissions.

Model forwarding uses only the model-gateway deployment's DNS-over-HTTPS resolver (RFC 8484).
`MODEL_GATEWAY_DNS_HTTPS_URL` defaults to `https://cloudflare-dns.com/dns-query` and
`MODEL_GATEWAY_DNS_BOOTSTRAP_IPS` to `1.1.1.1,1.0.0.1`; bootstrap entries must be public numeric IPs.
Deployment may select another trusted HTTPS resolver; model users cannot. Resolution failure never
falls back to system DNS. Both resolver and model TLS verify the original hostname; all model
A/AAAA answers pass policy before numeric dialing. Reserved ranges including `198.18.0.0/15` and
`2001:2::/48`, mixed public/private answers and redirects remain forbidden. Only exact development
fixture hosts use Docker DNS; this is never a production fallback.

Safe codes distinguish policy, DNS, TLS, timeout, redirects and unavailability. Logs contain only
classification, without raw URLs, resolved addresses, bodies, headers or provider errors. ACP
failure ends the Node session and revokes access. The known `Authentication required` /
`Not logged in` refusals become `agent_authentication_required`; other provider-authored text
remains private. Thread GET's `failureCode` is allowlisted.
Cancelled `TurnEnded` cannot mark a Thread idle; successful multi-turn sessions still become idle.
Run GET/List's `preparation` projects environment, clone, Agent preparation/start phases, durable
clone attempts and retry waiting. A Workspace operation permits three clones, with 5/10-second
backoff after the first two failures and termination after the third; unknown outcomes stay blocked.
Failed run Workspaces use the existing release path. A failed primary Workspace retains its data
and keeps admission closed. No database schema change is needed.

`internal/core/model_connections_db_test.go` uses real isolated PostgreSQL to cover ownership,
versions, idempotency, disabled accounts, atomic rejection, frozen configuration, runtime generation,
renewal and revocation. Contract/HTTP boundary tests cover strict model shapes and credential-route
ownership. cluster owns the complete real OpenCode acceptance and deployment wiring.
