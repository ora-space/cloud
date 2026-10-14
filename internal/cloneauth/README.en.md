# internal/cloneauth: issuing and verifying the single-use authorization

[English](README.en.md) | [中文](README.md)

`internal/cloneauth` defines the shape of the **single-use authorization**, plus the two functions that
let Cloud sign one and the Credential Service verify it. It carries out D3 and D10 of
`specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md`: the Store has to be
able to decide **on its own** that an authorization is genuine, while D10 forbids the issuer from
planting anything in the Store at issuance time ("Cloud 只做绑定与签发,不做任何'已用'标记,也不预留消费记录").

The carrier names two purposes today: `OperationClone` (one clone execution fetching its own checkout)
and `OperationMirrorPrepare` (fetching one upstream branch for one Project Operation, so that an
internal mirror can be published). They are **two operations on one carrier**, not a flag on one
operation: they authorize different work, so the binding's fields split into two mutually exclusive
groups along the operation.

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

- `authorization.go`: `OperationClone`, `OperationMirrorPrepare`, `MaxLifetime` (15 minutes), six
  sentinel errors, `Binding`, `validate` with the operation-specific `validateClone` and
  `validateMirrorPrepare`, `Issue`, `Verify`, `split`, `LoadSigningKey`, `LoadVerificationKeys`.
- `capability.go`: the capability vocabulary (`clone`), `CanonicalCapabilities`/`HasCapability`, and
  `permittedCapabilities`, which maps an operation to the one capability set it permits.
- `ref.go`: `ValidateRef` — the branch the mirror purpose pins, which must be `refs/heads/...` and
  nothing else.
- `repository.go`: `CanonicalRepository` — the normalized repository identity.
- `authorization_test.go`: the issue/verify round trip, a 20-case table of incomplete bindings, the
  lifetime boundary, 8 malformed tokens, a tampered payload, another issuer's key, the verifier's clock
  deciding expiry, unusable keys, and key-file round trips with 9 unusable key files.
- `ref_test.go`: 22 ref shapes, the mirror binding's round trip, and the ref being inside the signed
  payload.
- `purpose_test.go`: the two-way negative matrix over the mutually exclusive fields, a mixed or unknown
  payload being refused even under a valid signature, and each purpose carrying its own identity fields.

## Interface

- `Binding`: everything one authorization is bound to — its id, `operation`, tenant and owner, Project,
  workspace, credential id and version, normalized repository identity, and the issuance and expiry
  instants. **Every other field is decided by the operation, and enforced in both directions**:
  - `OperationClone` **requires** `executionId` and the whole Node runtime fence: `nodeId`,
    `nodeInstanceId` (`node_instances.id`) and `nodeIncarnationId` (one per Node process); it
    **forbids** `ref`, `operationId`, `preparationGeneration` and `executorIdentity`.
  - `OperationMirrorPrepare` **requires** `operationId` (the Project Operation), `preparationGeneration`
    (≥ 1), `executorIdentity` (SPIFFE) and `ref` (`refs/heads/...`); it **forbids** `executionId` and
    the whole Node runtime set.
  - Both operations **share** `runtimeGeneration` (`sandbox_instances.generation`) and `controlEpoch`
    (`runtime_controls.control_epoch`): a mirror preparation is fenced by the Workspace's runtime
    generation too, so a late preparation cannot publish under a runtime that has moved on.

  The two groups may not appear together. "Ignore the other purpose's fields" is not enough: a payload
  carrying an execution and a Node runtime *and* an operation id and a preparation attempt reads as
  either work, and the operation degrades into a label. That is not a matter of wording — it is why
  compatibility shortcuts such as filling the execution field with an operation id have to be ruled out
  structurally. The same reasoning is why the clone purpose deliberately does **not** sign the branch
  (it travels in the job's own field) while the mirror purpose must: the published mirror is keyed by
  the commit the ref resolved to and readers compare it against Cloud's rows, so the ref has to be the
  one the issuer pinned.

  The fencing values deliberately do **not** use the Controller lease's `controller_leases.epoch`,
  which identifies the lease holder rather than a Node instance; conflating the two would fence against
  the wrong generation.
- **Who may spend the authorization is decided by the operation, and the two answers come from
  different places.** The clone purpose binds a Node runtime, and Cloud already delivers those values
  to the Node through `runtimeBinding`, so the presenting side can declare them truthfully and the
  Store compares values the **request states**. The mirror purpose has no such premise: C runs outside
  any Node and has no runtime to declare, so it binds `executorIdentity` instead — the Store resolves
  the peer identity from the **verified client certificate**, so the party that may spend it is the one
  the deployment issued a certificate to, not a name written in a request body.
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
- **Purpose confusion is structural, not a check.** An authorization cannot be a clone and a mirror
  preparation at once: the two identity groups are mutually exclusive, missing and mixed fields are
  refused on both issue and verify, and an unknown operation is refused rather than defaulting to
  `clone`. A consumer must therefore compare the operation to decide whether to spend it, and cannot
  infer the purpose from which route the request arrived on.
- **Incomplete means refused**: `validate` runs on both issue and verify. On issue it stops a caller
  from minting a malformed authorization; on verify it stops a signed but malformed payload from
  reaching the Store's checks. The Store therefore never has to decide what an empty tenant or a zero
  generation means. Both sides are necessary: what reaches the Store is bytes someone else produced,
  and a token minted by a build with laxer rules can only be stopped there.
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
  `encoding/json`, `encoding/pem`, `errors`, `fmt`, `os`, `slices`, `strings`, `time`). It depends on
  neither `internal/core` nor any transport.
- Called by: `internal/credentialstore`, which uses `Verify` and `LoadVerificationKeys` to implement
  redemption, and `internal/cloneexec` (the credential executor C), which uses the same two functions to
  refuse an authorization it does not recognise **locally**, before spending anything, and compares
  `binding.Operation != OperationClone` in `matchBinding` — the point where the clone route refuses a
  mirror authorization, before the Store is asked anything.
- The issuing half has two production call sites, both wired by `cmd/server` from one configuration
  section (`clone_dispatch`, Go type `ExecutorDispatchConfig`), because the two share one executor
  endpoint, one dispatcher certificate and one signing key: clone goes through `CloneDispatcher.Execute`
  in `internal/core`, mirror through `MirrorDispatcher.Prepare`.
- **Each capability has its own switch, and neither is on by default**: with
  `CLOUD_CLONE_DISPATCH_ENABLED` `false`, `requireRepositoryAccess` still refuses before dispatch and
  nothing is ever signed — which is also why the signing key file is not a required input, and Cloud
  starts and stays fail-closed without it. `CLOUD_CLONE_DISPATCH_MIRROR_ENABLED` wires mirror preparation
  alone and does not change the clone path, so the mirror chain can be brought up while every
  credential-bearing clone is still refused. **Configured means usable**: `ExecutorDispatch.Open()` fails
  startup when an enabled capability is unusable (no endpoint, no signing key, no certificate, no executor
  identity) rather than degrading at runtime.
- The mirror purpose adds one deployment fact: `CLOUD_CLONE_DISPATCH_EXECUTOR_IDENTITY` must match
  `CREDENTIAL_STORE_BROKER_RESOLVER_IDENTITY` exactly, because the Store compares it not against what the
  payload claims but against the identity it resolves from the **verified client certificate**. A wrong
  spelling does not pass quietly: the redemption is refused and no credential is ever delivered. The
  mirror authorization is additionally bounded by `core.MirrorPreparationLease`, because an authorization
  has no honest reason to outlive the attempt lease it serves.

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

Purpose separation is covered by `purpose_test.go`: 14 one-field-at-a-time cases prove the two purposes
are mutually exclusive in both directions (including filling the execution field with the operation
id), and three further cases sign a payload by hand — bypassing `Issue` and keeping a genuine signature
— to prove that **a valid signature cannot get a mixed or unknown operation past `Verify`**.
`OperationClone` and `OperationMirrorPrepare` each carrying their own identity fields and none of the
other's is what lets a consumer tell the purposes apart by operation. The `cloneexec` side adds
`TestExecuteRefusesAMirrorAuthorizationOnTheCloneRoute`: a mirror authorization that is validly signed
and names the same repository, credential, version and generation as the job must be refused with
`binding_mismatch`, with the Store never asked.
