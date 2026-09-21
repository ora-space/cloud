# idaas: Huawei identity adapter

[中文](README.md) | [English](README.en.md)

This module adapts Huawei IDaaS's documented Authorization Code protocol to the Gateway
`Authenticator` interface. It hides authorize parameters, JSON token exchange, userinfo parsing,
provider error classification, and response bounds; callers receive only
`VerifiedIdentity{source: "huawei-corp", subject: uuid, displayName}`.

| File | Responsibility |
| --- | --- |
| `idaas.go` | Fixes endpoints and scope, builds redirects, exchanges codes, and minimizes profiles. |
| `idaas_test.go` | Covers documented JSON requests, identity mapping, fallback, error classes, cancellation, and secret redaction. |

`uuid` is the sole identity key. An optional top-level display field is used only for initial JIT
presentation and falls back to `uuid`; email, employee number, refresh tokens, and the full response
never leave the module. Authorize and token requests send only the documented fields: no PKCE and no
deprecated `display`. Token and userinfo calls require a total timeout, refuse redirects, and must
not run inside database transactions. Oversized or malformed JSON is `ErrProviderRejected`, matching
an explicit provider refusal. See the [Gateway authentication module](../README.en.md) and
[deployment guide](../../../docs/gateway.md).
