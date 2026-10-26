# gitrefs: the client of the Git Server's read-only ref query

[中文](README.md) | [English](README.en.md)

Cloud's client of the Git Server's read-only ref query (Git Server contract D6). Cloud never writes the Git Server: refs are read
outside transactions and what was read is recorded inside them, so this package answers only "which commit does this ref name" and
"which refs lie under this prefix", authenticated with Cloud's pinned client certificate.

| Files | Responsibility |
|---|---|
| `client.go` | `New` (TLS 1.3 mutual authentication, the pinned client identity, a 10s default timeout, no redirects), `ResolveGitRef`, `ListGitRefs`, `get` |

## Invariants

- Only 200 decodes; a 404 carrying `code=ref_not_found` means the ref does not exist; **every other status is an error**, so an
  unavailable server is never mistaken for a missing ref.
- An answer is bounded at 4 MiB (`maxResponse`); a larger one fails rather than being buffered without limit.
- A commit must be a full 40/64-character lowercase hexadecimal object ID; abbreviated, uppercase and empty values are refused
  because they are compared as strings with Git's own output.
- A prefix listing containing a ref outside the prefix, or one naming an invalid commit, is discarded whole.

`NewWithHTTP` serves tests and custom transports; `New` is the only production path.

## Tests

`client_test.go` is driven by `httptest` and needs no real Git Server: the TLS material (a CA and a client pair) is generated into
a temporary directory per test. It covers the URL/CA/certificate rejection branches, the request shape and query parameters, every
status code and answer shape, the size bound, invalid JSON, context cancellation and an unreachable server.
