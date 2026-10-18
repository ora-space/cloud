# Internal Credentials and Service Trust

English | [中文](authentication.md)

Cloud integrates no external login protocol and issues no credentials. External login (Huawei
IDaaS, GitHub) happens only in the [Gateway](gateway.en.md); Cloud only ever sees internal
credentials signed by the Gateway and mutual-TLS identities between services. This document
describes those internal trust relationships and how Cloud turns a verified credential into a user.

## Trust relationships at a glance

| Caller → callee | Channel | What proves identity |
|---|---|---|
| Gateway → Cloud server (HTTP) | `/api/v1/*` | Two Ed25519 JWTs per request: a service JWT plus a user JWT |
| Gateway → model-gateway (HTTPS) | Writing and clearing personal model keys | The same two JWTs, plus CA verification of model-gateway's certificate |
| Controller → Cloud server (gRPC) | The internal control contract | A TLS 1.3 mutually authenticated Controller certificate (SPIFFE URI) plus `x-ora-controller-id` metadata |
| Node → model-gateway | Short-lived grants for model calls | The dedicated model client certificate injected into the sandbox (see [model-gateway](../models/model-gateway.en.md)) |
| Sandbox, Repo Executor, Cloud → git-server | Git and ref queries | Client certificates issued by separate CAs (see [Git Server](../repositories/git-server.en.md)) |

Every identity only answers "who is calling". What the caller may do is decided by core from current
PostgreSQL state; claims in certificates and JWTs never turn directly into business permissions.

## Dual credentials over HTTP

Every public request carries two independently signed JWTs:

```text
Authorization: Bearer <service JWT>    proves the calling service, e.g. one Gateway instance
X-Ora-User-Token: <user JWT>           proves the end user and binds the service that signed it
```

The two credentials are signed with two private keys of **different purposes** (the Gateway refuses
to start with identical keys), so holding one kind of signing capability cannot forge the other. The
user JWT's `caller` must equal the service JWT's `sub`, which binds the user identity to the service
instance making this call; an intercepted user JWT cannot be replayed through a different service.

**Claims.**

| Claim | service JWT | user JWT |
|---|---|---|
| `iss`, `aud` | Must match the trusted key's issuer and Cloud's configured audience | Same |
| `sub` | Service instance identifier, e.g. `gateway-a` | The user's stable identifier at the external identity source |
| `kind` | `service` | `user` |
| `role` | `gateway` (the only role public endpoints accept) | — |
| `caller` | — | Required; equals the service JWT's `sub` |
| `source` | — | Required; the identity namespace, e.g. `huawei-corp`, `github.com` |
| `displayName` | — | Optional; used only when the user is first created |
| `globalUserId` | — | Optional; the Huawei employee directory association key |
| `iat`, `exp` | Required | Required |

**Verification rules** (`core.Authenticator.Verify`):

- Only `EdDSA` (Ed25519) is accepted. The JWT header must carry a `kid` that matches a configured
  trusted key.
- A trusted key entry pins `kind` and `issuer`, and a service key also pins `role`; the JWT must agree
  on each. A user key therefore cannot sign a service JWT, and a Gateway key cannot sign the
  Controller role.
- `aud` must contain the configured audience. `exp` and `iat` must be present, `iat` must not be in
  the future, and the lifetime (`exp − iat`) must be positive and at most **five minutes**; `nbf` is
  checked when present.
- `sub` must not be empty; a user JWT's `source` and `caller` must not be empty.

The lifetime ceiling means credentials must be minted per request. The Gateway signs 60-second
credentials by default; the browser session's own lifetime is managed by the Gateway in its
database and has nothing to do with the JWT.

**Key configuration.** Cloud holds public keys only:

```yaml
auth:
  audience: ora-cloud
  keys:
    - id: gateway-service-2026          # the kid in the JWT header
      issuer: ora-internal-issuer
      kind: service
      role: gateway
      public_key_file: /run/ora-keys/gateway-service.pem   # PKIX PUBLIC KEY, Ed25519
    - id: user-identity-2026
      issuer: ora-internal-issuer
      kind: user
      public_key_file: /run/ora-keys/user-identity.pem
```

- With no trusted key the server refuses to start; it never falls back to anonymous access.
- To rotate, configure the old and new `kid` side by side, restart the server, and remove the old
  entry once old credentials have expired.
- Private keys are held only by the Gateway (`tokens.service_private_key_file`,
  `tokens.user_private_key_file`, PKCS#8 PEM). JWTs provide no confidentiality; Gateway-to-Cloud
  traffic should still use TLS or a controlled service network.

## The Controller's gRPC identity

The Controller only dials out to Cloud's control listener (`control.grpc_addr`), which accepts TLS 1.3
with mutual authentication only:

- Cloud serves its certificate from `control.certificate_file` and `control.private_key_file` and
  verifies client certificates against `control.client_ca_file`. If any of the four settings is
  missing, the server refuses to start; production has no plaintext fallback.
- `control.controller_identity` is a SPIFFE URI of the form `spiffe://<trust domain>/controller/<id>`.
  The Controller certificate's URI SANs must contain exactly this URI, otherwise the call fails with
  `PERMISSION_DENIED controller_certificate_scope`.
- Every call must also carry one `x-ora-controller-id` metadata value equal to the `<id>` in the URI.
  It becomes the Controller principal for that call, used for lease holding and submission identity;
  metadata can select neither another service identity nor another role.

The service identity only proves "this is the trusted Controller". It does not stand in for runtime
use permissions, exclusive sessions or the various generation checks (see
[Runtime control](../runtime/runtime-control.en.md)). `controlgrpc.NewDevelopment` exists only for
in-process tests; it verifies no certificates and never serves a production listener.

## From credential to user

At the start of every public request's transaction, core resolves the user JWT to a Cloud user:

1. Look up `user_identities` by `(source, sub)`. If nothing is found, create a user and this identity
   mapping **just in time** (JIT); `displayName` is used only here, as the initial display name.
2. When `source = huawei-corp` and a `globalUserId` is present, also resolve the second identity key
   `(huawei-global, <normalized globalUserId>)`:
   - Neither key has a record: create the user and write both mappings.
   - Only one key has a record: bind the other key to the same user. This is how a person an
     administrator pre-added from the employee directory is linked on first login.
   - The two keys point at different users: return `409 identity_conflict`; accounts are **never
     merged automatically**.
3. The user must be `active` and not deleted, otherwise `403 user_disabled`.

Identities match on `(source, subject)` only: accounts are never merged by name, email or employee
number, and employee numbers play no part in authorization. A Huawei employee's `subject` is the
`uuid` returned by IDaaS (of the form `uuid~…`), not the employee number or W3 account; when the first
administrator is provisioned with `cloudctl bootstrap`, `-subject` must be that `uuid` too.

A few read-only entry points (the session list, session archives and model request statistics) use
read-only identity resolution: they recognize only existing identities and aliases, create no user
and bind no alias, and then check membership as usual.

Once the user is resolved, tenant membership and role are decided as described in
[Users, tenants and members](tenancy.en.md).

## What is never done

- External provider tokens, OAuth codes and PKCE verifiers never enter internal JWTs or Cloud.
- Cloud holds no signing private key, runs no certificate authority of its own, and has no public
  user create/update/delete API.
- Secrets such as deployment private keys and repository credentials appear in configuration only as
  file references; processes read their contents into memory only and never log, persist or return
  them.

## Related decisions

- [External identity is normalized by the Gateway; Cloud keeps authorization authority](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md)
- [The IDaaS UUID stays the login key; globalUserId becomes the directory association key](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260923-huawei-global-id-alias.md)
- [Runtime control is authorized by Cloud, delivered by the Controller, and fenced at the Node's real entry points](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md)
