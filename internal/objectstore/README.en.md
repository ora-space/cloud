# internal/objectstore: Revision object-store signing and verification

[English](README.en.md) | [中文](README.md)

`internal/objectstore` serves the Revision path alone: it issues the **single-key, single-method,
time-limited** upload grant for one delivery attempt (Cloud Revision D2/D3) and verifies an uploaded
object's **existence, size and SHA-256** by HEAD before Cloud registers a Revision (D4 step 2). It
decides no business semantics — who gets a grant, when, and what a failed verification means all
belong to `internal/core`; this package gets signing and verification right.

Signing is a self-contained AWS Signature Version 4 (SigV4) implementation rather than a new
dependency: the delivery path needs exactly two operations (presign one PUT, sign one HEAD), and the
repository prefers the standard library over a new module for that.

## Files

- `presign.go`: `Config` (endpoint, `public_endpoint`, region, bucket, path style, access key id and
  secret, grant TTL) and `Grant` (URL, method, the headers that must be sent unchanged, expiry);
  `PresignPUT` (signs one `PUT`, carrying `x-amz-sdk-checksum-algorithm: SHA256` and
  `if-none-match: *`), `PresignPUTChecksum` (additionally signs the Node-reported SHA-256 as
  `x-amz-checksum-sha256`, refusing a digest that is not 32 bytes of lowercase hex), and the SigV4
  canonicalization they share: per-segment URI encoding that keeps `/`, query parameters sorted by
  name, `UNSIGNED-PAYLOAD`, and `X-Amz-SignedHeaders` covering every signed header.
- `verify.go`: `Verify` sends `HEAD` with `x-amz-checksum-mode: ENABLED` against the private
  endpoint and compares `Content-Length` with `X-Amz-Checksum-Sha256`. It follows no redirect,
  times out after 30s, and lets no infrastructure detail escape the boundary.
- `presign_test.go`: the offline unit tests described below.

## Dependencies and callers

- Depends on: the standard library only (`net/http`, `net/url`, `crypto/hmac`, `crypto/sha256`,
  `encoding/base64`, `encoding/hex`). The package does not import `internal/core`.
- Called by: `internal/config` (`ObjectStoreConfig.Open` builds the `*Config` and validates it at
  startup with one `PresignPUT`); `cmd/server` (installs it as `store.ObjectStore`); `internal/core`
  (`revision_grants.go` signs grants, `revision.go` verifies objects outside any transaction).
- Credentials enter configuration as **file paths only** (`access_key_id_file`,
  `secret_access_key_file`), read and whitespace-trimmed by `internal/config` into `Config`; the
  values live only inside this process — never persisted, logged, or returned in a grant.

## Invariants

- **A grant is a capability**: one object key, one method, one lifetime. What the URL does not name,
  the Node cannot do. A `PUT` signature covers `host`, `if-none-match` and the checksum headers, so
  the capability cannot be rewritten into a request that overwrites an existing object.
- **An issued grant cannot rewrite an object**: every `PUT` carries `if-none-match: *`. A grant can
  outlive settlement, and create-only semantics stop a second upload at the store, so Cloud's
  external verification cannot be fooled by an object swapped in afterwards.
- **Nothing from the wire is trusted**: `Verify`'s `size`/`sha256` come from the Node's report.
  Shape validation — a digest that is 32 bytes of hex, a non-negative size — belongs to
  `internal/core`'s `validateDeliveryDeclaration` and happens before the probe; `Verify` compares
  facts only. A differing size or digest fails (`false`), and the failure is not distinguished — a
  missing object, a differing digest and a store without a digest are one thing for the delivery
  (D1: the delivery fails and D5 retries it). An unreachable endpoint or a non-`200` answer is a
  transient error, not a definitive failed verification.
- **No checksum is a failure**: the HEAD carries `x-amz-checksum-mode: ENABLED`. A store that
  returns no decodable digest — one that never received a checksum and so cannot vouch for the
  content — fails the verification instead of passing it on existence alone.
- **The grant lifetime is bounded**: a non-positive TTL takes the 15-minute default; a TTL outside
  `[1s, 7d]` is refused outright, so a configuration error cannot masquerade as an unexplained 403 at
  the Node.
- **The signed lifetime is the served lifetime**: both the signature and the expiry are truncated to
  whole seconds, so `Grant.Expires` reports exactly the boundary the store enforces.
- **Credentials do not leak**: a grant returns a URL, a method and headers and no credential; the
  tests assert that no secret appears in the URL.

## Tests

- The unit tests are fully offline and deterministic, against a real `httptest` S3 double. They cover
  the grant's binding to the object key and its local signing, `PresignPUTChecksum`'s binding of the
  checksum header and its refusal of an invalid digest, `public_endpoint` and the second-truncated
  expiry, and the canonical object path.
- Where grant issuance (the delivery execution, the checksum map) and object verification sit in the
  delivery state machine is covered by `internal/core`'s unit tests and
  `integration/agent_run_delivery_test.go`.
- That a real store accepts these signatures is proven against a real S3 (RustFS) by
  `integration/revision_test.go` and `integration/revision_sandbox_test.go`: the signed PUT and the
  HEAD verification, the store's refusal of a wrong checksum and of an expired grant, a stored object
  that a still-live grant cannot overwrite, and an upload through the public endpoint from the sandbox
  network. `task test:revision` (`REQUIRE_S3=1`) and cluster's `task agent:acceptance` run them, and
  the Backend CI workflow runs them against a pinned RustFS.
