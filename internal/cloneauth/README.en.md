# internal/cloneauth: issuing and verifying the single-use clone authorization

[English](README.en.md) | [中文](README.md)

`internal/cloneauth` defines the shape of the **single-use clone authorization**, plus the two functions
that let Cloud sign one and the Credential Service verify it. It carries out D3 and D10 of
`specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md`: the Store has to be
able to decide **on its own** that an authorization is genuine, while D10 forbids the issuer from
planting anything in the Store at issuance time ("Cloud 只做绑定与签发,不做任何'已用'标记,也不预留消费记录").

## Why this form

Both decisions fix what an authorization binds, how long it lives, and where it is consumed, but
neither fixes its cryptographic form: there is no mention of an HMAC, a signature, a public key, a JWT,
or a handle, and the checks the Store is told to perform are only three — the mutual-TLS peer is
`broker-resolver`, the binding matches the actual request, and it is neither expired nor already
consumed. Checking authenticity is not among them.

That gap has to be closed, because otherwise anything holding the `broker-resolver` certificate could
mint an authorization and redeem **any tenant's** credential, leaving the multi-tenant isolation
required by `20261010-controlled-node-git-authentication-isolation` D14-B item 11 resting on caller
discipline. And registering a pending consumption record at issuance is exactly what D10 forbids. The
only form left is: **Cloud signs with Ed25519 and the Store holds only the public key to verify with**.
A Store whose volume and process are both compromised still cannot mint an authorization, which is what
keeps D10's consumption record meaningful. The key format matches what the deployment already uses: the
PKCS#8 private key and PKIX public key that `openssl genpkey -algorithm ED25519` writes.

## Files

- `authorization.go`: `OperationClone`, `MaxLifetime` (15 minutes), six sentinel errors, `Binding`,
  `Issue`, `Verify`, `split`, `LoadSigningKey`, `LoadVerificationKeys`.
- `authorization_test.go`: the issue/verify round trip, a 20-case table of incomplete bindings, the
  lifetime boundary, 8 malformed tokens, a tampered payload, another issuer's key, the verifier's clock
  deciding expiry, unusable keys, and key-file round trips with 9 unusable key files.

## Interface

- `Binding`: everything one authorization is bound to — its id, `operation=clone`, tenant and owner,
  Project, workspace, clone execution, credential id and version, normalized repository identity, and
  the issuance and expiry instants; plus the **fencing values of the receiving runtime**: `nodeId`,
  `nodeInstanceId` (`node_instances.id`), `nodeIncarnationId` (one per Node process),
  `runtimeGeneration` (`sandbox_instances.generation`) and `controlEpoch`
  (`runtime_controls.control_epoch`). Cloud already delivers these to the Node through
  `runtimeBinding`, so the presenting side can declare them truthfully. It deliberately does **not** use
  the Controller lease's `controller_leases.epoch`, which identifies the lease holder rather than a Node
  instance; conflating the two would fence against the wrong generation.
- `Issue(key, Binding) (string, error)`: the carrier is
  `base64url(payload) + "." + base64url(Ed25519(payload))`. The signature covers the **exact transmitted
  bytes**, so verification depends on no JSON canonicalization: a field-ordering or escaping difference
  can neither make a genuine authorization fail nor, worse, make two different payloads verify as one.
- `Verify(keys, token, now) (Binding, error)`: takes a **set** of public keys and accepts a token that
  verifies under any one of them, which is what makes a signing-key rotation possible without refusing
  the clones already authorized under the outgoing key. The order is fixed as "verify the signature →
  parse → validate the binding → judge expiry". A signature failure and a malformed token are **two**
  errors: a well-formed token that fails verification is the signature of a forgery attempt, not of a
  bug. An empty key set is `ErrKey` rather than a refusal at the first token, so a verifier that was
  configured with nothing fails where the mistake was made.
- `LoadSigningKey`/`LoadVerificationKeys`: Ed25519 only. The wrong algorithm (an ECDSA key, say), the
  wrong half (a public key where a private one belongs), a file that is not PEM, and an absent file all
  report `ErrKey` rather than letting a deployment start with a key that verifies nothing.
  `LoadSigningKey` reads **exactly one** PEM block: a signing key that is one of several is a
  configuration the caller cannot have meant. `LoadVerificationKeys` reads **every** block and refuses
  the whole file if any block is unusable, so a half-rotated file is a startup failure rather than a
  deployment that silently trusts one key fewer than its operator believes.

## Invariants

- **The issuer owns its clock**: `Issue` takes no "now" — the caller supplies `Binding.IssuedAt` and
  `Binding.ExpiresAt`, so issuance is deterministic in tests and the deployment's time source stays in
  one place.
- **The lifetime has a hard ceiling**: D5 requires an authorization to live no longer than one clone
  command timeout. This package cannot see that timeout, so it enforces a ceiling instead: the caller
  picks the real value, and a bug or a misconfiguration cannot turn a single-use authorization into a
  long-lived bearer token.
- **Expiry is judged by the verifier's clock**: `Verify` uses the `now` its caller passes and does not
  take the payload's account of itself. If the issuer's and the Store's clocks drift, the conclusion
  lands on the Store's side — the Store is the party deciding whether to deliver the secret, and that is
  the fail-closed direction.
- **Incomplete means refused**: `validate` runs on both issue and verify. On issue it stops a caller
  from minting a malformed authorization; on verify it stops a signed but malformed payload from
  reaching the Store's checks. The Store therefore never has to decide what an empty tenant or a zero
  generation means.
- **The authorization is a bearer secret**: this package never prints, logs or caches it, and callers
  must keep it out of logs, audits and every durable medium (the Store's consumption record keeps the
  authorization id only, never the carrier).

## Key ownership, rotation and revocation

- **The private half belongs to Cloud and only to Cloud.** It is delivered as a file reference, never as
  a configuration value (`cloud/AGENTS.md` forbids deployment secrets in config files), and it is never
  mounted into the Credential Service. The Store holds public halves only, so a Store whose volume and
  process are both compromised can verify an authorization but cannot mint one — which is the property
  D10's consumption record depends on. There is no in-band key distribution: the deployment places both
  files, and the token carries no key identifier, so a key cannot be selected by the caller.
- **Rotation is the deployment's, and it is possible because the verification file may carry several
  keys.** The overlap is: append the incoming public key to the verification file → restart the
  Credential Service → switch `Issue` to the incoming private key → wait out `MaxLifetime` → remove the
  outgoing public key → restart. Both keys verify for the whole window, so no clone authorized under the
  outgoing key is refused while it is still live.
- **Revocation is removal plus a restart.** The Store reads its verification file once, when it opens,
  so that a redemption never depends on the file still being readable: an operator who unmounts or
  corrupts it does not thereby switch the capability off. The cost of that choice is that withdrawal
  takes effect at the next restart, and `MaxLifetime` is what makes the delay sufficient — every
  authorization an attacker could have minted with a stolen key stops being acceptable within fifteen
  minutes of the theft, whether or not the deployment has noticed it. That bound is why the rotation
  procedure above needs no coordinated cut-over and no key-version negotiation.

## Dependencies and callers

- Depends only on the standard library (`crypto/ed25519`, `crypto/x509`, `encoding/base64`,
  `encoding/json`, `encoding/pem`, `errors`, `fmt`, `os`, `strings`, `time`). It depends on neither
  `internal/core` nor any transport.
- Called by: `internal/credentialstore`, which uses `Verify` and `LoadVerificationKeys` to implement
  redemption, and `internal/cloneexec` (the credential executor C), which uses the same two functions to
  refuse an authorization it does not recognise **locally**, before spending anything.
- The issuing half's production call site is `CloneDispatcher.Execute` in `internal/core`, wired by
  `cmd/server` from the `CloneDispatch` configuration section. **It is not wired by default**:
  `CLOUD_CLONE_DISPATCH_ENABLED` is `false`, so `requireRepositoryAccess` still refuses before dispatch and
  nothing is ever signed — which is also why the signing key file is not a required input, and Cloud starts
  and stays fail-closed without it. **Configured means usable**: `CloneDispatch.Open()` fails startup when
  the section is enabled but unusable (no endpoint, no signing key, no certificate) rather than degrading
  at runtime.

## Tests

`authorization_test.go` is fully offline and deterministic and covers: the binding surviving an
issue/verify round trip field for field (the premise for the Store checking the binding rather than only
the signature), 20 incomplete bindings each refused (an empty tenant, a zero version, a zero generation,
a zero epoch, an expiry not after issuance, a lifetime above the ceiling, and so on), a lifetime exactly
at the ceiling being accepted, 8 malformed tokens, a tampered payload failing verification, a token
signed by another issuer being refused, expiry judged by the passed-in clock (the expiry instant itself
already expired), zero-valued keys being refused, and PKCS#8 private key and PKIX public key PEM round
trips alongside 9 unusable key files (including an ECDSA key of another algorithm). The rotation
behaviour is covered directly: a token signed by either key of a two-key set verifies, a key withdrawn
from the set no longer verifies anything it signed, a mounted file's every block is read, one unusable
block refuses the whole file, and a signing-key file holding more than one block is refused.
