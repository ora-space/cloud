# internal/gateway/devlogin: Development Login Provider

[中文](README.md) | [English](README.en.md)

`devlogin.Authenticator` implements `gateway.Authenticator` while also playing the external provider: instead of GitHub, the browser is sent to a small form served by this package (`GET /auth/dev/authorize`), the developer types the identity they want to be, and the form hands a signed code back to the ordinary `/auth/callback/dev` route. Login attempts, `state`, PKCE, cookies, PostgreSQL sessions and credential issuance are the real ones; only the "who are you" step is faked. It exists so the frontend can be developed and demoed without a GitHub OAuth App.

## Behavior

- `AuthorizationURL`: points at `<public origin>/auth/dev/authorize?state=…&code_challenge=…`, refusing any callback URL other than its own.
- `GET /auth/dev/authorize`: renders the form with `state` and `code_challenge` as hidden fields and `source` / `subject` / `display_name` inputs (defaults `dev` / `developer` / `Developer`). `Cache-Control: no-store`.
- `POST /auth/dev/authorize`: same-origin only (`gateway.SameOrigin`); normalizes the identity with `gateway.Normalize` (source and subject required, Cloud's byte limits), seals `{source, subject, displayName, challenge, expires}` with an HMAC-SHA256 keyed by a random per-process secret, and `303`s to `<callback>?state=…&code=…`. Codes live 5 minutes.
- The form routes' own error responses (missing `state`/`code_challenge`, a cross-site submission, an invalid identity re-rendering the form) are human-facing plain text/HTML and exempt from the JSON fault shape; `/auth/callback/dev` failures remain the uniform fault, the orchestration does not special-case providers.
- `Exchange`: verifies the callback URL, the signature, the expiry and that `S256(code_verifier)` equals the sealed challenge, then returns the typed identity. Every failure is `gateway.ErrProviderRejected`, so the orchestration answers the uniform `401 login_failed`.

## Invariants

- Registered only when `login.development_provider` is set, which `gateway.Config.Validate` accepts solely together with `public.development` (loopback HTTP). `New` additionally refuses any origin that is not loopback `http://`.
- The signing key never leaves process memory and is regenerated at every start; a code from another process, a tampered payload or a foreign verifier is rejected.
- The provider grants nothing: Cloud still creates or resolves the user by `(source, subject)`, and membership and authorization are decided by Cloud exactly as for a GitHub identity. `dev` is the default source so developer identities never collide with `github.com` ones, but any source can be typed, e.g. to sign in as the `cloudctl bootstrap` administrator.
- The Gateway logs a warning at startup when the provider is enabled.

See the [authentication boundary (`internal/gateway`)](../README.en.md) and the [Gateway document](../../../docs/identity/gateway.en.md).
