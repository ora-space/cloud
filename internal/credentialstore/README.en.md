# credentialstore: independent custody of external Git credentials

[中文](README.md) | [English](README.en.md)

The Credential Store is an independent process holding user-managed HTTPS Git tokens (proposed ADR
"Git credentials in an independent Credential Store" D2 and D3, executor root decision D3). Browsers
submit a token directly with a one-time submission grant Cloud signed; Cloud only reads metadata and
revokes through the management interface; the Repo Executor redeems plaintext with a one-time
execution authorization, at most once per authorization; the KEK operator rewraps DEKs after a key
rotation. Each caller is a distinct identity on a distinct listener, and no identity holds two powers.

| File | Responsibility |
|---|---|
| `keyring.go` | The read-only deployment KEK keyring (`{"primary":"<kekId>","keys":{"<kekId>":"<base64 32 bytes>"}}`); missing or invalid is `ErrKeyringUnavailable` |
| `envelope.go` | Envelope encryption: a random DEK per version, AES-256-GCM over `{"username","token"}` with AAD = `credentialId + "\x00" + version`; the DEK wrapped by the KEK with AES-256-GCM, AAD = `kekId`; `rewrap` changes only the wrapping |
| `files.go` | Volume layout and atomic writes (temp file, fsync, rename, directory fsync); exclusive `O_CREATE\|O_EXCL` claim files |
| `store.go` | Submit, redeem, metadata, revoke, rewrap and the retention sweep; writes are serialized, redemption holds the read lock across "check, claim, decrypt" |
| `identity.go` | The fixed SPIFFE identities; `PeerIdentity` accepts exactly one URI SAN on the verified leaf; `Identities.Validate` refuses overlaps |
| `server.go` | `NewServer`, error codes, strict JSON decoding (16 KiB cap), the mTLS identity gate |
| `service.go` | Handlers of the redemption, management and key mTLS listeners |
| `ingress.go`, `ratelimit.go` | Browser ingress: the single route, CORS, per-remote-IP GCRA rate limit, token validation |
| `config.go` | `CREDENTIAL_STORE_*` environment and TLS 1.3 listener configuration |
| `client.go` | `ManagementClient{Get, Revoke}` for Cloud and `RedemptionClient{Redeem}` for the executor: mTLS, TLS 1.3, no redirects, bounded response bodies, typed errors |

Volume layout (root `0700`; startup is refused if group or others can access it):

- `credentials/<credentialId>.json`: sealed current and retained versions, current version number and its `submissionId`, revocation tombstone;
- `redemptions/<authorizationId>`: one claim file per redeemed execution authorization;
- `submissions/<submissionId>`: one claim file per used submission grant.

Claim files are kept for 24 hours (far longer than any grant lifetime) and then removed by the sweep.

## Four listeners

| Listener | Caller | Routes |
|---|---|---|
| Redemption (mTLS, default `:8461`) | only `spiffe://ora.local/repo-executor` | `POST /v1/redemptions`, body `{"authorization":"<token>"}` → `{"username","token","version"}` |
| Management (mTLS, `:8462`) | only `spiffe://ora.local/cloud/credential-manager` | `GET /v1/credentials/{id}` → `{"credentialId","currentVersion","submissionId","revoked"}` (unknown id: version 0); `POST /v1/credentials/{id}/revoke` → `{"revoked":true}`, idempotent |
| Key (mTLS, `:8463`) | only `spiffe://ora.local/credential-store/kek-operator` | `POST /v1/keys/rewrap` → `{"rewrapped":n}` |
| Browser ingress (HTTPS, no client certificate, `:8460`) | a browser holding a submission grant | `PUT /v1/credentials/{id}/versions`, header `Authorization: Ora-Submission <grant>`, body `{"token","username"?}` → `{"storeVersion":N}` |

Redemption order: verify the authorization with the Store's own clock against Cloud's **execution**
key set (`cloneauth.Verify`; purpose must be `sync`/`publish`/`probe`); its `executorIdentity` must
equal the certificate identity; it must name a credential; the credential exists, is not revoked, and
the version is current or superseded less than an hour ago; **then** create
`redemptions/<authorizationId>` exclusively and durably, and only **then** decrypt. A decryption
failure leaves the claim, so the authorization cannot be redeemed again.

Submission order: verify the grant against the **submission** key set (`cloneauth.VerifySubmission`);
its `credentialId` must equal the path; the credential is not revoked, the tenant matches the first
submission, the `submissionId` is unused and `expectedStoreVersion` is exactly current + 1; claim
`submissions/<submissionId>` exclusively and write the new version; the previous current version
becomes superseded, stops being redeemable after one hour and is destroyed by the sweep. Tokens are
1..8192 bytes of UTF-8 without CR, LF or NUL; the username defaults to `x-access-token` and must not
contain `:`. CORS echoes only configured origins, allows only `PUT` with `Authorization` and
`Content-Type`, and never sets `Access-Control-Allow-Credentials`; the default limit is 10 per minute
per IP with a burst of 5.

Revocation writes the tombstone and drops every sealed version in the same atomic record replacement;
an unknown id also gets a tombstone, so a later submission is refused with `credential_revoked`.

## Error codes

Every refusal is `{"code":"..."}` without internal detail:

| Status | Codes |
|---|---|
| 400 | `invalid_request`, `invalid_authorization` |
| 401 | `client_identity_required`, `invalid_submission`, `submission_expired` |
| 403 | `client_identity_forbidden`, `executor_identity_mismatch`, `submission_mismatch`, `origin_forbidden` |
| 404 | `credential_not_found`, `not_found` (unknown route) |
| 409 | `already_redeemed`, `submission_replayed`, `version_conflict` |
| 410 | `credential_unavailable` (revoked, unknown version or past retention), `credential_revoked` (submission to a revoked credential) |
| 413 | `request_too_large` |
| 429 | `rate_limited` |
| 500 | `store_unavailable` |

The clients map them to `ErrAlreadyRedeemed`, `ErrInvalidAuthorization`, `ErrCredentialNotFound`,
`ErrCredentialUnavailable`, `ErrIdentityRefused` and `ErrRejected`; an unreachable Store, a 5xx or an
unreadable response is `ErrUnavailable`. `*RefusalError` carries the status and code.

Audit lines (zap structured fields) carry only `credentialId`, `version`,
`authorizationId`/`submissionId`, `purpose`, `peer` and `outcome`; tokens, usernames, submission grants
and execution authorizations never reach logs, error strings or management responses. `Secret` and
`Credential` redact themselves when formatted.

All tests run on `t.TempDir` with keys, a CA and certificates generated in process memory, through
real TLS: encryption and AAD, KEK rotation and rewrap, the submission and redemption refusal matrices,
concurrent redemption delivering once, the redemption record existing before decryption, retention,
revocation destroying ciphertext, identity boundaries, and the absence of plaintext from logs.
