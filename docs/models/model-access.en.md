# Personal Model Connections, Run Bindings and Short-Lived Grants

English | [中文](model-access.md)

Users can configure their own model service (OpenAI-compatible or Anthropic API) and API key so that the
Agent runs they trigger use that model. The key is kept by the platform, and every run's model requests
are proxied through [model-gateway](model-gateway.en.md): the sandbox running the Agent never receives the
original key, only a short-lived token valid for that run alone.

Declared Agent admission is disabled by default. After host rollout, `plugins.personal_agent_target` opts into one exact Linux target. Cloud freezes package/declaration/adapter identities, configuration revision, model metadata and numeric literals. Controller and Node must prove matching target, contract and protocol before execution. Validated Settings JSON travels as text to avoid rounding integers or decimals; runtime authority exists only in mTLS grants and the live plugin startup channel. An incompatible current Node settles as unavailable; disconnected or stale Nodes are not definitive refusals.

This document covers four things on the Cloud side: connections and the default model, key custody, the
binding fixed for a run, and short-lived grants for model requests.

## Connections and the default model

A personal model connection belongs to **the user personally** and is independent of any tenant: a user can
configure one before joining any space.

| Endpoint | Purpose |
|---|---|
| `GET`, `POST /api/v1/me/model-connections` | List and create connections |
| `GET`, `PUT`, `DELETE /api/v1/me/model-connections/:mcid` | Read, change and delete a connection |
| `PUT`, `DELETE .../model-connections/:mcid/credential` | Write and clear the API key; handled **only** by model-gateway (see below) |
| `GET`, `PUT /api/v1/me/model-default` | Read and set the default model |

- A **connection** has a name, protocol, upstream address, authentication mode, model list and an enabled
  flag. The protocol is `openai-completions` or `anthropic-messages`; the upstream address must be `https`
  without user info, query or fragment; the authentication mode is `bearer`, and the Anthropic protocol may
  also use `x-api-key`. Connection metadata supports neither custom authentication headers nor embedded keys.
- Each entry of the **model list** has `id`, `name`, `contextWindow` and `maxTokens`. Model IDs are opaque
  strings and are kept exactly as given, including IDs that contain `/`.
- Changes use the current `version`; creation and deletion use a per-user `Idempotency-Key`. Reading a
  connection returns only `credentialConfigured`, never the key.
- The **default model** points at one model of one connection; it is the model used when the user triggers an
  OpenCode run.

In enterprise mode, creating or changing a connection (not the key-write path) also passes
[deployment egress admission](../operations/deployment-egress.en.md#updates-revocation-and-health): a short transaction
checks identity, ownership, version and a completed idempotent replay, DNS and address admission run outside that
transaction, and the write transaction rechecks current authoritative state. A DNS refusal returns
`400 invalid_model_url` and writes no new version. A completed legal replay, a pure disable, deletion and clearing the
key skip this admission. Passing it still does not let a later forward skip its own outbound check, and neither DNS nor
external HTTP is held inside a locked transaction.

## Key custody

The original API key only ever enters model-gateway:

- When the browser writes or clears a key through the [Gateway](../identity/gateway.en.md#the-proxy), the
  Gateway forwards those two paths only to the fixed HTTPS model-gateway, and Cloud's HTTP router refuses them
  before reading any body (`404 credential_gateway_required`), so plaintext cannot enter the Cloud process even
  if someone bypasses the Gateway and calls Cloud directly.
- model-gateway encrypts the key with AES-256-GCM, with a random nonce for every write, and hands Cloud an
  **immutable ciphertext record** to store. Cloud core only ever receives ciphertext; it never receives, decrypts
  or returns the original key.
- Replacing a key creates a new ciphertext record and keeps the old reference, so runs already created are
  unaffected. A retry with an idempotency key is associated with the same ciphertext; Cloud compares only opaque
  identifiers, versions and the service key ID, never computing a request fingerprint over the raw key or the
  random ciphertext.
- Writing and clearing both require a user-scoped `Idempotency-Key` and the current resource version. Clearing a
  key and disabling or deleting a connection revoke, in the same transaction, every short-lived grant issued for
  that connection.

## Personal Agent configurations

`/api/v1/me/agent-configurations/:spaceId/:pluginNamespace/:pluginIdentifier` reads, saves and clears the current user's plugin configuration; `POST .../repair` explicitly confirms a changed declaration. The space supplies the selected-release context. Ownership remains one configuration per user and canonical Plugin ID; administrators cannot access another user's values.

Reads return the current declaration alongside the saved configuration. A changed fingerprint retains old values and marks repair as required. Writes check configuration revision, space-selection version and declaration fingerprint; repair submits a complete configuration against the current declaration. Every write requires a user-scoped idempotency key, and a replay under the same key returns the original response with numeric setting literals unchanged. Clear retains a tombstone with an increasing revision, leaves connections intact and does not revoke existing runs. OpenCode stores only plugin options; its model selection remains the original global default resource with its own version.

Settings accept declared scalar fields only, and configuration requests do not carry connection credentials. Connection/model references must belong to the current user and match the declaration's protocols; Claude requires Messages. Saving configuration does not establish connectivity or deployed adapter support.

Declared Agent task readiness and final run admission use the same public gap codes: disabled, deleted or inaccessible connections return `model_connection_unavailable`, a missing key returns `model_credential_required`, a withdrawn selected model returns `model_not_found`, and an unconfigured model selection returns `agent_configuration_required`. The UI directs connection, credential and model gaps to personal model connections, and an unconfigured plugin model selection to the plugin's personal configuration. Internal workflow gap reasons are not exposed directly as these public errors.

## Run bindings

An Agent run of `official/ora-space.opencode` gets a run binding (`run_model_bindings`) generated **in the same
transaction that creates the run**, fixing the initiator, the connection and its version, the protocol, upstream
address, authentication mode, model and ciphertext reference; the initiator's Git author identity is fixed at the
same time.

- When the initiator has no default model, no configured key, or an unusable connection, the whole comment,
  interaction and run roll back together, returning `409 model_default_required`, `model_credential_required` or
  `model_connection_unavailable` respectively. Undeclared legacy OpenCode releases retain this behavior.
- Later changes to the connection or the default model do not change runs already created.
- A run may carry several bindings: a workflow run freezes one per slot its graph references, in the creation
  transaction, and each node's session redeems only the binding of the slot it references (see
  [Workflows](../workflows/workflows.en.md#executor-slots)).
- The session execution's input carries only an opaque `modelBindingId`; neither short-lived tokens nor the
  upstream key ever enter execution inputs or the Controller's control messages.
- When registering a session dispatch, Cloud checks that the current Node declared the model proxy capability in
  its handshake; an older Node without it cannot run a session that uses a personal model
  (`409 model_proxy_required`), and issuing a model grant requires the same capability. How a run settles when the
  Node is confirmed to lack the capability during startup is in [Bounded startup](../agent-runs/startup-settlement.en.md).

For a run using a personal model, only the initiator can append to the Thread, the initiator or a current
administrator can end it, and other members can still read it (see [Threads](../agent-runs/threads.en.md)).

Declared Agent runs additionally validate personal configuration, declaration identity and the deployment's exact target/adapter contract capability, then write private `run_agent_configurations` facts in the same transaction. Missing capability returns `agent_adapter_unavailable`; Claude without a declaration returns `agent_declaration_required`. Deployment composition must supply the capability only after the actual Node/Controller adapter path can consume frozen inputs.

The private snapshot holds package identity, declaration identity, configuration revision and resolved options. Shared run details expose only the plugin, version and configuration revision. A delayed Workspace installation uses the originally frozen release even if the space selection was upgraded or removed. Later configuration edits do not rewrite the snapshot; connection revocation continues through the existing run binding.

## Short-lived grants

The Node in the sandbox requests a short-lived grant from model-gateway using the dedicated model client
certificate (mTLS) injected into the sandbox, which carries the tenant, Workspace and runtime generation. Cloud
core re-checks at issuance, at renewal and on every forwarded request that:

- the registered session execution belongs to this run, and the current Node and runtime occupancy match;
- the run and Thread states still allow model calls;
- the initiator is still a current member with an enabled account, and the connection is still enabled with its key
  in place.

The database stores only the token's SHA-256 digest; a grant lives 15 minutes on the database clock, and renewal
extends the same grant without changing the token. Disabling the account, leaving the space, clearing the key,
disabling or deleting the connection, ending the session or force-stopping the run all make later requests fail;
long streaming forwards are re-checked continuously, and a revocation interrupts both the upstream request and any
blocked downstream write.

A session that is ending, or a cancelled session still waiting to be processed, gets `403 model_session_ending`
once every other check has passed: the Node uses it to wait for the already persisted `EndSession` command and to
keep the end reason the user gave. A wrong scope or a revoked account or key still returns an ordinary refusal and
never masquerades as a normal end. Model access uses the checkout already cloned and never asks again for remote Git
credentials.

## Related decisions

- [Personal Agent configurations resolved per plugin and frozen at run creation](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-personal-agent-configurations.md) (partially implemented; the ADR also governs adapter and end-to-end obligations)

- [Personal model connections are configured by users; the platform keeps the keys and proxies each run's model requests](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/0-personal-model-connections-and-run-scoped-proxy.md)
- [A personal-model Thread is appended only by its initiator; members read it and administrators may end it](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/20261010-personal-model-initiator-controls-input.md)
- [Sandboxes receive a dedicated model client certificate; personal model keys stay in the platform proxy](https://github.com/ora-space/specs/blob/main/decisions/cluster/sandbox-server/20261010-model-client-identity-replaces-shared-model-key-injection.md)
- [Personal slot mappings resolve a workflow's executors; each run pins one model binding per slot](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md) (approved)
- [Model forwarding and Cloud asset downloads share a deployment-owned egress allowlist and explicit resolution](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-deployment-owned-egress-and-explicit-resolution.md) (approved)
