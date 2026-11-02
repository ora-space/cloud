# Gateway: External Login, Browser Sessions and the Proxy

English | [中文](gateway.md)

The Gateway (`cmd/gateway`, implemented in `internal/gateway`) is the only service browsers talk to
directly. It performs external login, keeps browser sessions in PostgreSQL, mints two internal JWTs
for every authenticated request, and forwards `/api/v1/*` to the fixed Cloud upstream. When the
deployment configures a frontend build, it also serves the static frontend on the same origin.

The Gateway reads and writes no Cloud business table: users, identity mappings, memberships and
resource authorization all live in Cloud. It owns just two tables, `gateway_login_attempts` and
`gateway_sessions`, and like every other business process it only verifies migrations at startup and
never runs DDL.

## Routes

| Route | Description |
|---|---|
| `GET /auth/providers` | Lists the login methods this deployment registered and its default external provider; the frontend uses it to decide which buttons to show and whether to redirect automatically |
| `POST /auth/login` | Same-origin JSON `{"provider":"github","returnTo":"/path"}`; an omitted `provider` means the deployment's external provider. Creates a login attempt, returns `{"authorizationUrl"}` and sets the attempt cookie |
| `GET /auth/callback/:provider` | The provider's redirect target. On success it creates a session and answers `303` back to the page the user came from; every failure is `401 login_failed` |
| `POST /auth/logout` | After a same-origin check, revokes the current session and clears the cookie; idempotent, returns `204` |
| `ANY /api/v1/*` | Resolves the session, mints credentials and forwards; only the exact `POST /api/v1/join/links/preview` permits session-optional read-only inspection |
| `GET /healthz` | PostgreSQL liveness |
| Other `GET`/`HEAD` | With `web.dist_dir` configured, serves frontend files; unknown client-side routes get `index.html` |

There is no `/internal/*` route at all. Errors have the same shape as Cloud's:
`{"code","params","requestId"}`.

## Login flow

```text
Browser                Gateway                         External provider        PostgreSQL
  │ POST /auth/login    │                                    │                      │
  │────────────────────▶│ same-origin check, rate limit      │                      │
  │                     │ create attempt secret and state, derive the PKCE verifier │
  │                     │──────────────────────────────────────────────────────────▶│ write attempt (digests only)
  │◀── authorizationUrl + attempt cookie                     │                      │
  │ redirect to provider ─────────────────────────────────────▶│                      │
  │◀───────────────────────────────────────── redirect to callback?code&state       │
  │ GET /auth/callback  │ match attempt cookie, state, provider, unexpired, unconsumed │
  │────────────────────▶│ exchange the code outside any transaction ─▶│               │
  │                     │ short transaction: lock attempt, set consumed_at, create session ─▶│
  │◀── 303 returnTo + session cookie (attempt cookie cleared)                       │
```

- **At most one session per attempt.** The callback looks the attempt up first, calls the provider
  outside any transaction, and finally, in a short transaction, locks the attempt with `FOR UPDATE`,
  checks it is still unconsumed, sets `consumed_at` and creates the session. Of two concurrent
  callbacks only one succeeds. A provider success followed by a failed transaction is a failed
  login; a session is never assumed to exist.
- **The PKCE verifier is never stored.** It is derived from the attempt secret and
  `login.pkce_key_file` with a domain-separated HMAC-SHA256. GitHub receives the verifier, so **all
  Gateway replicas must mount the same PKCE key**, or a callback landing on another replica fails.
- **`returnTo` accepts only on-site relative paths**: it starts with a single `/`, its second
  character is neither `/` nor `\`, and it has no control characters. The database also refuses
  absolute paths and the `//` and `/\` forms.
- **Failures converge.** Provider rejections, OAuth errors, oversized or malformed responses and
  missing required fields all return `401 login_failed`; network errors, timeouts, 429 and 5xx return
  `503 login_unavailable`. Raw errors never reach the browser.
- Login start and callback are rate-limited per client address before any attempt is written.

## Sessions and cookies

| Cookie | Production name | Attributes |
|---|---|---|
| Session | `__Host-ora_session` | `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`, no `Domain`; `Max-Age` equals the remaining lifetime in the database |
| Login attempt | `__Secure-ora_login` | `SameSite=Lax` (the provider redirect is a cross-site top-level navigation; `Strict` would keep the cookie from the callback), `Path=/auth/callback`, `Max-Age` equals the attempt lifetime |

In local development (`public.development: true`, loopback HTTP) the names are `ora_session` and
`ora_login`, without `Secure`.

- A session's lifetime is an **absolute deadline**; reads never extend it. The default is 30 days;
  with `login.provider` set to `huawei-idaas` the default is 12 hours, so changes to a corporate
  account are rechecked by IDaaS within a bounded time. The ceiling is 90 days, enforced by a database
  constraint as well.
- Validity is decided only by database state and `clock_timestamp()`. Unknown, expired and revoked
  sessions all return `401 unauthenticated` and clear the cookie.
- Logout writes `revoked_reason=logout`. The database allows only three revocation reasons —
  `logout`, `identity_revoked`, `administrative` — and HTTP exposes only logging yourself out.
- A lifecycle-owned cleanup goroutine runs every `session.cleanup_interval` and deletes at most
  `session.cleanup_batch` rows per batch, only expired or revoked sessions older than
  `session.retention` and expired attempts, keeping an audit window in between.
- A valid Gateway session cannot bypass Cloud: when a user is disabled or a member removed, Cloud
  still answers 401/403 and the Gateway passes that through unchanged.

The database stores only SHA-256 digests of the attempt secret, the `state` and the session token.
Raw tokens, OAuth codes, provider access tokens and PKCE verifiers exist only in the memory of a
single request; they are never persisted, logged or sent to Cloud.

## The proxy

How `ANY /api/v1/*` is handled:

1. Modifying methods require same-origin proof: `Origin` exactly equal to the public origin, or
   `Sec-Fetch-Site: same-origin`; otherwise `403 origin_forbidden`.
2. Resolve the session cookie; failure returns `401 unauthenticated`. The sole exception is the exact `POST /api/v1/join/links/preview`: after origin proof and rate limiting it may use only the service JWT. A valid session adds a caller-bound user JWT. The response is `no-store`; anonymous inspection returns only kind/state and Cloud writes no identity or membership.
3. Drop the browser's `Authorization`, `X-Ora-User-Token`, `Cookie`, `Forwarded`, `X-Forwarded-*` and
   `X-Real-Ip` headers and attach a service JWT and a user JWT freshly signed by this replica (see
   [Internal credentials and service trust](authentication.en.md)).
4. Forward to the fixed `cloud.upstream`; a request cannot choose another target. Request bodies are
   capped at 64 KiB and response bodies at 8 MiB. When Cloud is unreachable or does not finish
   responding within `cloud.timeout`, the answer is `502 upstream_unavailable` and the session is
   unaffected.

Two kinds of request are special:

- **SSE event streams.** Once Cloud answers with `text/event-stream`, that one authorized connection
  is released from `cloud.timeout`, `server.write_timeout` and the response size cap, and frames are
  relayed until either side closes.
- **Personal model keys.** Writing and clearing `.../me/model-connections/:id/credential` is
  forwarded only to the fixed HTTPS model-gateway named by `model_credentials.upstream`, whose
  certificate is verified against the deployment-supplied CA. Cloud is never a fallback for this path;
  without the configuration the answer is `503 model_service_unavailable`. The Gateway holds only
  model-gateway's public CA, never its private key or encryption master key.
- Session analysis reads refuse, before forwarding, any query string `url.ParseQuery` cannot parse
  (`400 invalid_session_filter`), so the proxy's query sanitizing cannot change what a request means.

## Login providers

**Huawei IDaaS (`internal/gateway/idaas`).** Uses IDaaS 2.0 Authorization Code as a confidential
client (`client_secret_post`) without PKCE.

- The default authorize URL is `<idaas.base_url>/saaslogin1/oauth2/v1/authorize` with only `client_id`,
  `response_type=code`, `scope=base.profile`, `state` and the fixed callback. The callback's scheme,
  host and port must match the IDaaS application registration exactly.
- `/saaslogin1/oauth2/v1/token` is called with a form `POST` carrying `client_id`, `client_secret` and
  `code` in the body, never in the URL, so they stay out of access logs; `/saaslogin1/oauth2/v1/userinfo`
  is called the same way.
- The normalized identity is `source=huawei-corp`, `subject=<the trimmed uuid>`, plus the verified
  `globalUserId` (the `globalUserID` spelling is accepted) and the display name from
  `idaas.display_name_field`, falling back to the uuid. Without a `globalUserId` the user can still
  log in, but cannot be linked automatically to a person an administrator pre-added from the employee
  directory.
- The token and userinfo clients have their own total timeout and a 64 KiB response cap, propagate
  cancellation and never follow redirects.
- Deployments can independently override the complete HTTPS authorize, token and userinfo URLs;
  omitted endpoints retain the default paths above. Overrides cannot contain URL credentials, a
  query or a fragment and are never read from browser requests. See the
  [configuration reference](../operations/configuration.en.md#gateway). Overrides do not change the
  form protocol, scope, `huawei-corp` source or UUID mapping, migrate identities, revoke existing
  sessions or grant tenant permissions; targets must preserve the same corporate identity authority.

**GitHub (`internal/gateway/github`).** OAuth App Authorization Code with PKCE S256, no scopes, and
`allow_signup=false` plus `prompt=select_account` on the authorize request. After reading
`GET /user` the access token is discarded at once. The identity is `source=github.com` (the configured
host for GitHub Enterprise Server), `subject=<numeric id>`, display name from name or login.

**Local development (`internal/gateway/devlogin`).** `login.development_provider: true` registers the
`dev` provider: a form served by the Gateway itself where any `source`, `subject` and display name can
be typed in. Attempts, PKCE and sessions then work exactly as with GitHub. Because it can sign in as
anyone, configuration validation requires explicit `public.development: true` and a loopback HTTP
`public.base_url` origin (`localhost`, `127.0.0.1`, or `::1`, with an optional port). HTTPS origins
are rejected even with development mode enabled. Startup logs a warning; production configurations
must not set it.

## Automatic login in the browser

The frontend treats the session cookie and `GET /api/v1/me` as its only authentication facts. A page
that gets `401` goes to a login transition page carrying its current on-site path. When
`GET /auth/providers` lists exactly one external provider (and no `dev`), that page starts
`POST /auth/login` and redirects automatically; otherwise it shows one button per provider. `403`
means the user is disabled in Cloud and no new login is attempted; `5xx` or a failed login start
stops automatic retries and shows an explicit retry button.

The frontend and the Gateway must appear to the browser as one origin: deployments have the Gateway
serve `frontend/dist` through `web.dist_dir`; in local development Vite proxies only `/auth`, `/api`
and `/healthz` to the Gateway and never exposes `/internal`.

## Deployment notes

- All replicas share one PostgreSQL, the PKCE key, the provider client configuration and the client
  secret.
- Give the Gateway a dedicated database role with only `SELECT/INSERT/UPDATE/DELETE` on
  `gateway_login_attempts` and `gateway_sessions` and only `SELECT` on `schema_migrations`.
- `public.base_url` must be an `https` origin (no path, no query); the callback is fixed at
  `<base_url>/auth/callback/<provider>` and never derived from request headers. Only development mode
  allows `http` on loopback.
- The two signing private keys (`tokens.*_private_key_file`) must differ and correspond to the
  `kind: service, role: gateway` and `kind: user` entries in Cloud's `auth.keys`; `tokens.lifetime`
  is at most five minutes.
- Every setting is described in the [configuration reference](../operations/configuration.en.md#gateway).

**IDaaS rollout checklist.**

1. Register a separate Authorization Code application with an exact callback per environment, and
   request `base.profile`, `uuid`, `globalUserId` and the name fields you need; beta and production
   never share an origin. If the default userinfo lacks `uuid`, request it in the console first —
   never fall back to `globalUserId` or the employee number as the login subject.
2. Set `idaas.display_name_field` to the top-level name field actually returned (for example
   `userName` or `cn`).
3. Mount the client secret and a PKCE key of at least 32 bytes as read-only files with identical
   contents on every replica.
4. When provisioning the first administrator, `cloudctl bootstrap -subject` must be that employee's
   `uuid` as returned by IDaaS. Everyone else is pre-added by an administrator from the employee
   directory by `globalUserId` and linked automatically on first login.
5. Verify on beta first — code exchange, userinfo fields, callback, returning to the original path,
   disabled users and local logout — then switch production's `idaas.base_url`, client configuration
   and callback. Never add codes, tokens, userinfo, email addresses or employee numbers to log sampling.

## Related decisions

- [External identity is normalized by the Gateway; Cloud keeps authorization authority](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md)
- [The IDaaS UUID stays the login key; globalUserId becomes the directory association key](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260923-huawei-global-id-alias.md)
