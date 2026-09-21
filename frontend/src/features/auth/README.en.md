# auth: browser authentication flow

[中文](README.md) | [English](README.en.md)

## Responsibility

This module treats the Gateway HttpOnly cookie and Cloud `/api/v1/me` as the only login facts. It
owns safe `returnTo` handling, automatic Gateway Login Attempt creation, Huawei sign-in navigation,
current-user probing, and local Cloud logout. It stores no token, interprets no tenant permission,
and does not own the mocked business data.

| File | Responsibility |
| --- | --- |
| `api.ts` | HTTP/TanStack Query adapter for current user, login start, logout, and stable failures. |
| `return-to.ts` | Same-origin return-path rules matching the Gateway. |
| `login-page.tsx` | Automatic-login transition that stops on failure and offers explicit retry. |
| `*.test.ts(x)` | Covers path rules, automatic navigation, failure stopping, disabled users, and logout. |

It depends on the generated `me` client, shared axios adapter, and presentation primitives. Routes
and layouts may depend on this module; lower layers must not depend on screens. Only 401 starts an
external login, 403 must not loop, and external URLs are validated before browser navigation.
