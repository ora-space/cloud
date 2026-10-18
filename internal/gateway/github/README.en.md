# internal/gateway/github: GitHub OAuth Adapter

[中文](README.md) | [English](README.en.md)

`github.Authenticator` implements `gateway.Authenticator`, normalizing the GitHub OAuth App authorization code flow into a `VerifiedIdentity`. It is the only code that understands GitHub's endpoints and user document, and it cannot grant any Ora permission.

## Behavior

- `AuthorizationURL`: carries `client_id`, the fixed `redirect_uri`, the orchestration-generated `state` and `code_challenge` (`S256`), `allow_signup=false` and `prompt=select_account` (forces GitHub's account picker; otherwise GitHub silently reuses the browser's current github.com session and a different account can never sign in); no scope is requested.
- `Exchange`: redeems `code` with `code_verifier`, `client_secret`, and `redirect_uri` at the token endpoint (GitHub enforces the verifier once a challenge was sent), then calls `GET /user` and discards the token immediately.
- Identity normalization: `source` defaults to `github.com` (configure the host for GitHub Enterprise Server), `subject` is the decimal string of the numeric `id`, `displayName` is the non-empty `name` or else `login`. A missing, non-numeric, or non-positive `id` fails the login.
- Failure classification: GitHub error objects (even with HTTP 200), non-`bearer` tokens, non-200 user responses, and responses over 64 KiB or with malformed JSON are `gateway.ErrProviderRejected`; network, timeout, and cancellation errors are infrastructure failures, letting the orchestration distinguish "login failed" from "login unavailable".
- The HTTP client must carry a total timeout; `NewHTTPClient` additionally refuses redirects.

## Invariants

- The GitHub access token, the full user document, and the email never reach the database, logs, errors, JWTs, or the browser.
- `login`, `name`, and email are never identity keys; a renamed account still maps to the same `(github.com, id)`.

See the [authentication boundary (`internal/gateway`)](../README.en.md) and the [Gateway document](../../../docs/identity/gateway.en.md).
