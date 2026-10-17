# cloneauth: one-time grants of the external Git line

[中文](README.md) | [English](README.en.md)

Two kinds of grant that Cloud signs and the executor and the Credential Store verify (ADR
`repo-executor/0` D2; the identity-access Credential Store decision). The format is kept from the
earlier implementation: `base64url(payload) "." base64url(Ed25519 signature)` (RawURLEncoding), the
signature covering the exact payload bytes, so verification never depends on canonical JSON.
Verifiers hold a public-key set for rotation; private keys exist only in Cloud.

| File | Responsibility |
|---|---|
| `token.go` | encoding, signing and verification, strict decoding (unknown fields and trailing values refused), lifetime and clock window, sentinel errors |
| `authorization.go` | `Purpose` (`sync`, `publish`, `probe`), `Authorization`, `Sign` / `Verify` and the per-purpose field rules |
| `submission.go` | `Submission` (purpose `submit`), `SignSubmission` / `VerifySubmission` |
| `repository.go` | `NormalizeRepository` |
| `fields.go` | canonical UUID, full OID, ref and branch name checks |
| `keys.go` | `LoadPrivateKey` (one PKCS#8 block), `LoadPublicKeys` (one or more PKIX blocks) |

Per-purpose fields (fields of other purposes must be empty):

| Purpose | Required | Allowed |
|---|---|---|
| `sync` | `operationId`, `tenantId`, `projectId`, `bindingId`, `repository`, `branch`, `expectedTrackingOid` | credential (a public repository may be anonymous) |
| `publish` | the same IDs, `repository`, `sourceRef`, `sourceCommit`, `targetRef` (`refs/heads/<branch>`), `expectedOldOid`, credential | — |
| `probe` | exactly one of `operationId` and `verificationId`, `tenantId`, `repository`, `service` (`upload-pack`/`receive-pack`), credential | `projectId`, `bindingId`, `targetRef` |

Common fields: `authorizationId`, `executorIdentity` (must be `spiffe://ora.local/repo-executor`),
`issuedAt`, `expiresAt` (UTC, lifetime in `(0, MaxLifetime=15m]`). The credential is `credentialId` +
`credentialVersion > 0`, or both empty. The old purposes `clone`, `mirror_prepare` and `push` are no
longer defined and are refused as unknown.

Invariants:

- `Sign` and `Verify` apply the same field rules; `Sign` refuses invalid input (missing fields,
  forbidden fields, lifetime over the maximum) and `Verify` does not trust the signer.
- `Verify` checks the signature before parsing JSON; `now < expiresAt`, and `issuedAt` may lead the
  verifier's clock by at most `MaxClockSkew` (1 minute) — more is `ErrInvalid`.
- Execution and submission grants use distinct key pairs; even with a reused key, strict decoding and
  the purpose field keep either from verifying as the other.
- `NormalizeRepository`: https only; refuses userinfo, query, fragment, any percent-encoding, empty /
  `.` / `..` segments and non-ASCII; lowercases the host and drops `:443`; keeps the path's case and
  removes one trailing `/` and one `.git`. The result is idempotent, and a signed `repository` must
  equal its normalization.
- Errors are sentinels (`ErrMalformed`, `ErrSignature`, `ErrExpired`, `ErrInvalid`, `ErrKey`,
  `ErrRepository`) and never carry payload or key content.

Tests cover round trips per purpose, every required and forbidden field, wrong keys, tampered
payloads and signatures, expiry, future issuance, lifetimes over the maximum, cross-use of execution
and submission grants, unknown fields, non-UTC instants, key files, and the accept / equality /
reject matrices of `NormalizeRepository`. Dependencies: standard library, `github.com/google/uuid`.
