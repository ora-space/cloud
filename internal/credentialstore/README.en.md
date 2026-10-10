# internal/credentialstore: the independent Credential Service (secret store)

[English](README.en.md) | [中文](README.md)

`internal/credentialstore` implements the **independent** Credential Service that holds user-managed
personal HTTPS Git tokens, together with the `ingress-writer` client Cloud needs to hand a token to it.
It carries out the security boundary described by
`specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md`: the service is a
separate process with its own volume, its own key material and its own trust root, sharing no database
instance, connection, credential or volume with Cloud's business store (D1). Cloud hands the token over
within one request and afterwards keeps only `credential_refs` metadata and an opaque `secret_ref`; the
Cloud process holds no decryption capability (D2).

The package deliberately serves credential storage alone: who a credential belongs to, whether it may be
bound to a Project, and whether a clone is permitted all belong to `internal/core`; this package gets
sealing, rotation, destruction, and "open only to three service identities" right.

## Files

- `store.go`: `Config` (`Volume`, `KeyFile`, `Retention`, `Now`) and `Store` (`Write`/`Rotate`/`Revoke`).
  `Open` is the startup gate D6/D8 require — an unreadable or other-readable volume, a missing or
  unusable KEK, and an unconfigured volume are all refused here, never returning a partially initialized
  Store. `newSecretRef` generates a 24-byte (~192-bit) opaque reference (prefix `cs_`) inside the
  service, so neither the browser, the end user, nor the deployer can choose or predict one. `prune`
  drops superseded versions whose retention window has closed on the writes that already load them;
  there is no background sweeper.
- `keystore.go`: `Keyring` and `LoadKeyring` — the deployment-mounted KEK set (`primary` plus `keys[]`,
  each 32 bytes of base64). Every new wrapping uses `primary` while the wrapping itself records `keyId`,
  so a KEK rotation is a deployment change with no data migration. One DEK per secret:
  `sealToken`/`openToken` use AES-256-GCM with the `credentialID` as additional data (so a sealed blob
  cannot be moved to another credential's record), and the DEK is cleared in a `defer`. `fingerprint` is
  an HMAC-SHA256 keyed with the KEK, which lets a replay be recognized as the same material while
  leaving an attacker who holds the volume unable to test candidate tokens offline. Any unusable KEK is
  reported as `ErrKeyMissing`.
- `record.go`: `record`/`material`/`retained` and `fileStore`. One JSON record per credential at
  `credentials/<id>.json` inside the volume; `validCredentialID` accepts the canonical lowercase UUID
  form and nothing else, and `fileStore.path` re-checks that the id cannot escape the record directory.
  A write stages a sibling temporary file, `Sync`s it, and renames it over the target. `revokedAt` is
  the tombstone and `retained.retainUntil` the grace period of a superseded version.
- `identity.go`: `Capability` (`write`/`resolve`/`keys`), `IdentityConfig` (the three fixed identities)
  and `Identities`. The three must be present, distinct, and shaped `spiffe://<trust domain>/<kind>/<name>`;
  `PresentedIdentity` takes the identity from the URI SANs of a leaf certificate the client CA already
  verified, and a certificate that verified but names no known identity is rejected rather than treated
  as anonymous.
- `server.go`: `Server`, the mutual-TLS, capability-gated HTTP surface. `POST /v1/credentials`,
  `.../{id}/rotate` and `.../{id}/revoke` require `write`; `.../{id}/resolve` requires `resolve`. Bodies
  are decoded strictly (unknown fields, a document that is not a single JSON object, and trailing
  content are refused). `failure` maps a Store error to a fixed reason code and leaves the error text in
  this service's own log; `audit` records operation/credentialId/serviceIdentity/result for every
  operation. `resolve` currently answers 409 `authorization_verification_unavailable` unconditionally.
- `client.go`: Cloud's `ingress-writer` client — `ClientConfig` (an `https` endpoint with no query or
  fragment; TLS carrying a client certificate and a trust pool), `Failure` (with `ReasonCode()`), and
  `Write`/`Rotate`/`Revoke`. It deliberately does not retry: a write that timed out may have succeeded,
  and recovery is the caller re-issuing the whole request under the same idempotency key (D4).
- Tests: `store_test.go`, `keystore_test.go`, `server_test.go`, `client_test.go`.

## Dependencies and callers

- Depends on: the standard library (`crypto/aes`, `crypto/cipher`, `crypto/hmac`, `crypto/sha256`,
  `crypto/rand`, `crypto/tls`, `encoding/json`, `net/http`, `net/url`) and `go.uber.org/zap`. The
  package does **not** import `internal/core`.
- Called by: `internal/config` (`CredentialStoreServiceConfig.Store`/`ServerTLS`/`Identities` build the
  service side, `CredentialIngressConfig.Open` builds Cloud's `*Client`); `cmd/credential-store` (the
  separate process's wiring and lifecycle); `cmd/server` (hands the `*Client` to `internal/core` as
  `store.Secrets`).
- The reverse dependency is an **interface rather than a package**: `internal/core` declares the
  structural interfaces `CredentialSecretWriter` (`Write`/`Rotate`/`Revoke`) and `credentialRefusal`
  (`ReasonCode()`), which `credentialstore.Client` satisfies without `core` importing it — so `core`
  holds no transport.

## Invariants

- **No KEK, no plaintext in the volume**: apart from `fingerprint` (a KEK-keyed HMAC that cannot be
  tested offline), everything a record holds is ciphertext or wrapping metadata, which is what lets the
  volume be backed up under the deployment's own retention policy without carrying the KEK (D8). Opening
  the same volume with a different KEK must fail — never fall back to another key, and never present
  itself as an empty store.
- **One DEK per secret**: a token is sealed with AES-256-GCM under a one-off DEK, and only the DEK is
  wrapped by the KEK; the `credentialID` is additional data, so a sealed blob cannot be relocated. A KEK
  rotation selects by the `keyId` recorded beside each wrapping, so an old KEK must stay in the keyring
  while live wrappings name it.
- **`secret_ref` is generated here**: the write payload has no such field, and supplying one is refused
  by strict decoding. The reference is opaque and unique per credential.
- **A write is idempotent; a conflict is not**: repeating the same id with the same material returns the
  original `secret_ref` and version; the same id with different material is `ErrConflict`, never a silent
  replacement.
- **Rotate writes the new version first**: the superseded version is retained until `retainUntil` rather
  than deleted, so an execution already admitted under it can still settle. Re-supplying the current
  token is a no-op — it advances no version and burns no window the in-flight execution may need.
- **Revoke is a tombstone and is idempotent**: it destroys the ciphertext of `current` and every
  `previous` version and writes `revokedAt`; revoking again, or revoking an id that never existed, both
  succeed. The tombstone is what stops a replayed create from resurrecting a credential Cloud has
  already reported as gone. D4 fixes revocation authority in Cloud's metadata; this is defense in depth.
- **Capabilities do not overlap**: the `Store` type offers no resolve and no listing method at all — the
  `write` identity cannot read a secret back, and the `resolve` identity cannot write, list or delete.
  One certificate holding two capabilities is refused at startup, because that failure would be
  invisible at runtime: every call would still authenticate.
- **There is no anonymous path**: `NewServer` requires mutual TLS with a client CA; `route` takes the
  identity from the verified TLS state and the capability from the deployment's fixed table, and nothing
  in the URL, headers or body can influence either. Until its second factor exists — a single-use
  authorization bound to tenant, owner, Project, workspace, clone execution, credential id and version,
  repository identity, Node instance and runtime generation — `resolve` fails explicitly rather than
  degrading (D6).
- **Failures are explicit**: the client separates "unreachable" from "refused" into two error types;
  `ErrRevoked`, `ErrConflict`, `ErrNotFound` and `ErrInvalidRequest` each map to a fixed reason code; no
  response text carries error detail and no log line carries a token, ciphertext, wrapped data key or
  KEK (D7).

## Tests

- The unit tests are fully offline and deterministic. `store_test.go` covers the opaque, unique
  reference, idempotency per credential id, refusal of invalid input, rotation advancing the version
  while retaining the old one, a no-op rotate of the same token, pruning once the window closes, revoke
  as a terminal tombstone, and "revoked" being distinguishable from "never existed". `keystore_test.go`
  covers the startup gate, a refused malformed keyring, a refused other-readable volume, an exported
  volume never yielding the token (in every base64 and hex spelling), `keyId` persistence with the old
  KEK still usable, and a mandatory failure under a different KEK. `server_test.go` covers identity-table
  validation over a real mutual-TLS connection, capability separation, the refusal of an anonymous or
  unknown peer, idempotency across the transport, the refusal of a caller-supplied `secret_ref`, and
  audit lines carrying no secret. `client_test.go` drives the real `Client` against the real `Server`:
  the full lifecycle round trip (the returned reference is the recorded one, the sealed material is the
  token handed over, a replay seals no second copy, rotation retains the old version, and a replayed
  create after revocation is refused by the tombstone), refusals mapping to stable codes without echoing
  the token, the wrong identity being refused at the capability layer, an unreachable store being an
  explicit failure rather than a refusal, and `NewClient` refusing a deployment that cannot authenticate.
- End to end: `integration/git_credentials_test.go` (the lifecycle and the handoff),
  `integration/git_credential_ingress_test.go` (the ingress boundary and cross-user/cross-tenant
  isolation), and `integration/git_credential_binding_test.go` (Project binding and the fail-closed
  credentialed clone). They use a test double for `core.CredentialSecretWriter` and so do not depend on
  this service's process; the real agreement between this package and Cloud is covered by the round trip
  in `client_test.go` above.
- `internal/config/credential_store_test.go` covers that layer's deployment gate: the service is refused
  at startup when its volume, KEK, server certificate, client authority or any of the three identities is
  missing, `ServerTLS` always requires and verifies a client certificate, and `CREDENTIAL_STORE_*` and
  Cloud's `CLOUD_*` prefix never cross.
