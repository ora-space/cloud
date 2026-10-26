# gitrefquery: Cloud's Git Server ref query client

[中文](README.md) | [English](README.en.md)

Cloud's client for the Git Server's read-only ref query. Cloud uses it, outside any database
transaction, to verify that a ref it is about to register (such as a Revision's delivery ref) exists
and names the commit it was told about. The query listener accepts only Cloud's pinned client
certificate; the TLS configuration (pinned Git Server CA, Cloud's identity, no redirects) is built from
the `git_server` section in `internal/config` and injected.

| Files | Responsibility |
|---|---|
| `client.go` | `New` (base must be `https://host[:port]`; needs an HTTP client and a positive timeout); `ResolveGitRef` queries `GET /internal/v1/refs?tenantId=&projectId=&ref=` and returns the commit of a `200` answer carrying a valid one |

Only `404 ref_not_found` is the definitive `core.ErrGitRefNotFound`; any other answer, a transport
failure or a timeout is an error that proves nothing, so callers retry rather than conclude. Every
query is bounded by the timeout and the response body by a size limit. The package implements
`core.GitRefResolver`; `internal/core` does not import it.
