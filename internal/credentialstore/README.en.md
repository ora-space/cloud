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
sealing, rotation, destruction, redemption under a single-use authorization, and "open only to three
service identities" right.

## Files

- `store.go`: `Config` (`Volume`, `KeyFile`, `Retention`, `AuthorizationPublicKeyFile`, `Now`) and
  `Store` (`Write`/`Rotate`/`Revoke`, plus `Resolver()`, which exists only to hand out the `*Resolver`).
  `Open` is the startup gate D6/D8 require — an unreadable or other-readable volume, a missing or
  unusable KEK, and an unconfigured volume are all refused here, never returning a partially initialized
  Store. `AuthorizationPublicKeyFile` may carry **more than one** public key, which is what lets the
  deployment rotate Cloud's signing key while authorizations signed by the outgoing key are still live;
  the set is read once, at `Open`, so a redemption never depends on the file still being readable.
  `newSecretRef` generates a 24-byte (~192-bit) opaque reference (prefix `cs_`) inside the
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
  the tombstone and `retained.retainUntil` the grace period of a superseded version. `record.version`
  selects the requested version as either `current` or a `previous` still inside its window, and a
  version it cannot select is "this version is unavailable" — the redemption-time version check reads
  the Store's own state rather than the authorization's claim. `authorizationDirectory`
  (`authorizations/`) is a second directory beside `credentials/`, because an authorization has been
  spent whether or not the credential it named still exists.
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
  operation. `resolve` performs a real redemption when the deployment configured a verification key,
  and otherwise still answers 409 `authorization_verification_unavailable` — before reading the body.
- `authorization.go`: the `Resolver` and the redemption path. `ResolveRequest` (the authorization
  carrier plus the presenting side's own account of its Node instance and runtime generation),
  `ResolvedCredential` (the token and its version), and six distinguishable reason errors
  (`ErrAuthorizationUnverifiable`, `ErrAuthorizationInvalid`, `ErrAuthorizationExpired`,
  `ErrAuthorizationConsumed`, `ErrAuthorizationBinding`, `ErrVersionUnavailable`). `Store.Resolver()`
  returns `(*Resolver, bool)`: no verification key means no redemption capability. `Resolver` holds the
  whole **key set** and passes it to `cloneauth.Verify`, so an authorization signed by any mounted key
  is delivered. `claimAuthorization` creates the consumption record with `O_CREATE|O_EXCL`, and that
  create *is* the atomic single-use decision; `markAuthorizationDelivered` moves the result from
  "consumed, not delivered" to "delivered" before the secret leaves the process.
- `client.go`: Cloud's `ingress-writer` client — `ClientConfig` (an `https` endpoint with no query or
  fragment; TLS carrying a client certificate and a trust pool), `Failure` (with `ReasonCode()`), and
  `Write`/`Rotate`/`Revoke`. It deliberately does not retry: a write that timed out may have succeeded,
  and recovery is the caller re-issuing the whole request under the same idempotency key (D4).
- Tests: `store_test.go`, `keystore_test.go`, `server_test.go`, `client_test.go`, `authorization_test.go`,
  `process_test.go` (the cross-process acceptance; see Tests below).

## Dependencies and callers

- Depends on: the standard library (`crypto/aes`, `crypto/cipher`, `crypto/ed25519`, `crypto/hmac`,
  `crypto/sha256`, `crypto/rand`, `crypto/tls`, `encoding/json`, `net/http`, `net/url`) and
  `go.uber.org/zap`; and on `internal/cloneauth`, because the shape of the authorization carrier and its
  signature are defined by the one package the issuer and the redeemer share. The Store holds only the
  public key, so what it verifies is a binding Cloud signed rather than a caller's own account of itself.
  The package does **not** import `internal/core`.
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
- **Capabilities do not overlap**: the `Store` type offers no method that reads a secret back and no
  listing method, and redemption lives on a **separate `Resolver` type** reachable only through
  `Store.Resolver()` — the `write` identity cannot read a secret back, and the `resolve` identity cannot
  write, list or delete. One certificate holding two capabilities is refused at startup, because that
  failure would be invisible at runtime: every call would still authenticate. Putting redemption on
  `Store` would pass the same tests, but it would reduce this boundary to caller discipline.
- **There is no anonymous path**: `NewServer` requires mutual TLS with a client CA; `route` takes the
  identity from the verified TLS state and the capability from the deployment's fixed table, and nothing
  in the URL, headers or body can influence either. Redemption needs **two factors at once, neither
  taken from the caller's assertion**: the `broker-resolver` service identity is enforced by the
  transport and the capability table, and the single-use authorization is verified by the Store itself
  against the deployment-mounted Cloud public key (D3). A deployment that configured no key has no
  `Resolver` at all, so every redemption is refused with `authorization_verification_unavailable` rather
  than degraded (D6).
- **An authorization is single-use, and recorded before it is honoured**: the order is fixed as
  "verify signature and binding → check credential state → write the consumption record → only then hand
  over the secret" (D10). The decision is the `O_CREATE|O_EXCL` creation of the consumption-record file,
  which makes it atomic, lock-free, and independent of process memory: at most one concurrent redemption
  of the same authorization succeeds, and it stays that way across a restart. A failure before delivery
  leaves "consumed, not delivered" behind; the reverse — "delivered with no consumption record" — cannot
  occur, because the record is on disk before delivery. The authorization is itself a bearer secret: the
  body's copy is cleared before the audit line or the failure log, and the consumption record keeps only
  the authorization id, the instant, the presenting Node instance and runtime generation, and the result.
- **The Store owns the expiry decision**: `Resolve` judges expiry with the Store's own clock and does not
  take the payload's account of itself; if the two clocks drift, the Store's clock is what takes effect.
  That it resolves in the Store's favour is deliberate — the Store is the party deciding whether to hand
  over the secret.
- **The runtime fields are a consistency check, not an identity check**: `nodeId`, `nodeInstanceId`,
  `nodeIncarnationId`, `runtimeGeneration` and `controlEpoch` come from the presenting workload's own
  request, so they are compared against the authorization's binding and nothing more. They fence an
  authorization to the runtime it was issued for; they do not attest who is presenting it. The only
  factor the Store verifies independently is the mutual-TLS `broker-resolver` identity, and the
  deployment's fixed three-identity table gives every Node the same one — so the Store cannot tell one
  Node instance from another. Making that cryptographic would need per-Node identities, which the
  identity table does not currently take. An authorization is nonetheless not transferable between
  tenants or credentials, because the binding's credential id, version and tenant come from Cloud's
  signature rather than from the caller.
- **The consumption record fails closed in every failure mode**: a record that exists but cannot be
  parsed counts as spent (`ErrAuthorizationConsumed`) rather than as absent, so a corrupted volume can
  never turn a spent authorization back into a live one; a record that cannot be written at all is an
  error and no secret is delivered; and a crash or power loss between the claim and the delivery leaves
  the conservative reading, `consumed_not_delivered`, which is terminal. `syncDirectory` returns before
  `openToken` is called, so a power loss that loses the record also means the secret never left the
  process — the state "delivered without a record" is unreachable in both directions.
- **Failures are explicit**: the client separates "unreachable" from "refused" into two error types;
  `ErrRevoked`, `ErrConflict`, `ErrNotFound` and `ErrInvalidRequest` each map to a fixed reason code, and
  each of the six redemption refusals has its own — a refused caller has to be able to tell "ask for a
  new authorization" from "go investigate", which a single `resolve_failed` would not allow (D7). No
  response text carries error detail and no log line carries a token, ciphertext, wrapped data key or
  KEK.

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
- `authorization_test.go` covers the redemption path over a real file volume, a real KEK, real mutual
  TLS and real `cloneauth` signatures: delivery of the bound version, a second redemption of the same
  authorization being refused, **a consumed authorization staying refused after the Store is closed and
  reopened** (restart durability), a forged signature, a tampered payload, expiry, binding mismatches
  (credential id, Node instance, generation, epoch and six such fields), an incomplete runtime
  declaration, an unavailable version, a superseded version still inside its window remaining
  redeemable, a revoked credential, unknown and malformed credential ids, cancellation, an
  authorization left unspent when a check fails, no redemption capability without a verification key,
  **exactly one success out of eight concurrent redemptions**, and, across real TLS, delivery, the
  replay refusal code, and audit lines carrying no secret. The failure semantics above have their own
  cases: an unparseable consumption record reads as spent, a volume that cannot record the spend
  delivers nothing, and a secret that cannot be opened leaves `consumed_not_delivered` behind with a
  second attempt refused. Rotation is covered across a real reopen: an authorization from either mounted
  key is delivered while both are present, and a key withdrawn from the file no longer verifies anything
  it signed.
- `process_test.go` is the cross-process acceptance. It builds the real `cmd/credential-store`, writes a
  real configuration file, a real PKI, a real KEK keyring and a real Ed25519 key pair to disk, starts the
  binary as a child process with an environment of its own, and drives it over the real mutually
  authenticated listener: one authorization buys exactly one token, a replay is `authorization_consumed`,
  **the same replay is still refused after the process is restarted**, a fresh authorization still works
  afterwards, the consumption record is in the Service's own volume and carries no token, and neither the
  token nor any authorization appears in the Service's log. Separate cases cover capability separation
  over the real transport (a valid authorization is worthless to the `ingress-writer` identity, and the
  `resolve` identity cannot write), the refusal matrix at the wire (untrusted key, wrong runtime, past
  expiry, not an authorization at all — with a live authorization still redeeming afterwards, proving no
  refusal spent anything), an anonymous client failing the handshake itself, and a two-key deployment
  delivering authorizations signed by either key. What it deliberately does not cover: a real **Cloud
  server** issuing the authorization. There is no Cloud process that does — `Issue` has no production
  caller, because every call site sits behind `requireRepositoryAccess`'s unconditional refusal — so
  issuance here runs Cloud's real code from a real mounted signing key with the test as the caller, and
  that is the whole of the gap.
- End to end: `integration/git_credentials_test.go` (the lifecycle and the handoff),
  `integration/git_credential_ingress_test.go` (the ingress boundary and cross-user/cross-tenant
  isolation), and `integration/git_credential_binding_test.go` (Project binding and the fail-closed
  credentialed clone). They use a test double for `core.CredentialSecretWriter` and so do not depend on
  this service's process; the real agreement between this package and Cloud is covered by the round trip
  in `client_test.go` above. The integration suite runs no real credential-store process, so the real
  Store coverage of the redemption path lives in this package's in-process mutual-TLS tests instead.
- `internal/config/credential_store_test.go` covers that layer's deployment gate: the service is refused
  at startup when its volume, KEK, server certificate, client authority or any of the three identities is
  missing, `ServerTLS` always requires and verifies a client certificate, the redemption capability
  follows its verification key (absent key means absent capability, present key means present, and a
  configured but unusable key is a startup refusal — three states that must not look alike), and
  `CREDENTIAL_STORE_*` and Cloud's `CLOUD_*` prefix never cross.
