# idaas: Huawei identity adapter

[中文](README.md) | [English](README.en.md)

This module adapts Huawei IDaaS 2.0 Authorization Code (`client_secret_post`) to the Gateway
`Authenticator` interface. It hides authorize parameters, form token exchange, userinfo parsing,
provider error classification, and response bounds; callers receive only
`VerifiedIdentity{source: "huawei-corp", subject: uuid, displayName}`.

| File | Responsibility |
| --- | --- |
| `idaas.go` | Fixes endpoints and scope, builds redirects, exchanges codes, and minimizes profiles. |
| `idaas_test.go` | Covers client_secret_post form requests, identity mapping, fallback, error classes, cancellation, and secret redaction. |

`uuid` is the sole identity key and is trimmed of leading/trailing whitespace (the documented success
example includes a trailing space); a blank value after trim is rejected. An optional top-level
display field is used only for initial JIT presentation and falls back to `uuid`; email, employee
number, refresh tokens, and the full response never leave the module. Authorize sends only the 2.0
required fields: no PKCE and no `display`. Token exchange posts `grant_type`, `client_id`,
`client_secret`, and `code` in the `application/x-www-form-urlencoded` entity-body (no Basic header,
no credentials on the query string, no PKCE-NULL `code_verifier`); userinfo posts only
`access_token` in the body. Token and userinfo calls require a total timeout, refuse redirects, and
must not run inside database transactions. Oversized or malformed JSON is `ErrProviderRejected`,
matching an explicit provider refusal. See the [Gateway authentication module](../README.en.md) and
[deployment guide](../../../docs/gateway.md).
