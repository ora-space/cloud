# gitserver: sandbox Git access and mirror reads

[中文](README.md) | [English](README.en.md)

One process, two surfaces (proposed ADR `cloud/git-server/0`):

- The **repository** surface serves Project repositories (`/{tenantId}/{projectId}.git`) to sandboxes
  over smart HTTP only, and answers Cloud's read-only delivery-ref query. The caller presents an mTLS
  client certificate issued by the sandbox CA; the certificate names only the runtime target, and
  every request is decided again from current Cloud state. It is the only surface that can write.
- The **mirror** surface serves published private-repository mirrors (`/mirror/v1/<digest>.git`) to a
  Workspace's Node, on a listener of its own. The caller presents no certificate at the transport
  layer at all; its identity is the address its connection arrived from, resolved per request. The
  surface is read-only.

The two share one process, one set of dependencies and one lifecycle, and they do **not** share a
trust boundary: each owns its listeners, its TLS configuration and its handlers, and neither surface's
handler can reach the other's routes — so the address a caller can reach never decides how it is
authorized. A Node cannot present a client certificate (`desktop/apps/ora-node`'s clone policy permits
exactly one `[http]` key, `sslCAInfo`, and no client identity for it exists in the deployment), so
folding this surface into the repository listener would leave the read path unreachable, while relaxing
that listener's `RequireAndVerifyClientCert` would take the transport's guarantee away from every route
on it. The reasoning and the refusal order are in `mirror.go`'s file header.

`internal/core` and PostgreSQL decide every permission; this package holds no authorization state.

| Files | Responsibility |
|---|---|
| `service.go` | Composition of the two surfaces: one resolved git program, one runtime directory, one health answer; refuses to start with neither surface, and answers not-found on a surface it was not given |
| `config.go` | Deployment references and listener TLS: the repository listener requires a client certificate from the sandbox CA; the ref listener pins Cloud's exact leaf; only the surface that will start is validated |
| `scope.go` | `RuntimeScope`: only the `git-access` SPIFFE purpose is accepted; WSS and model-access certificates from the same CA are not |
| `repository.go` | The four smart HTTP endpoints, canonical IDs, repository creation on first authorized write (staged then renamed), exact delivery-ref lookup |
| `hook.go` | One-time push tokens, the private Unix-socket hook channel and the `RunPreReceive` hook body |
| `mirror.go` | The mirror surface's routes, identity resolution, per-request authorization and refusal order; it serves the read protocol only, and mounts no write endpoint |
| `gitrun.go` | The mirror surface's only child process: `git upload-pack --stateless-rpc`, with a closed environment, a bounded stderr and a bounded deadline |
| `origin.go` | The origin-resolution client: calls the resolver over mTLS with this service's identity and pins its certificate, keeping "not a workspace" apart from "could not ask" |
| `identity.go` | The mirror surface's own client identity and its checks (it must assert git-server's service URI and be valid for client authentication) |
| `state.go` | `MirrorStateReader`: composes Cloud's binding with the mirror store's generation and readability into the one snapshot a decision reads |

Every repository request authenticates the runtime target from the certificate and asks Core whether
the declared execution may read (a session restoring a prior Revision) or write (a delivery). Any
denial is the same `403`, including an absent repository; a database failure is `503` and never
allows. A delivery may only create `refs/ora/deliveries/<workId>`: `pre-receive` repeats the full
Core decision immediately before any ref is written and rejects the whole push unless every update
creates exactly that ref with a commit. Replaying the same attempt is up to date and does not fail.

A mirror read is decided in a fixed order: a write or an unknown service is refused first, then the
connection's address is resolved, then Cloud is asked, then the store is opened by the generation the
authorization pinned. Every refusal is one `403` with one body, so a caller cannot use the server to
tell "no such mirror" from "not yours"; a resolver or database that cannot answer is `503` and never
becomes an allow. A mirror republished between the authorization and the open is refused, and the
caller retries, which re-authorizes.

A negotiation body reaches `upload-pack` decoded according to its `Content-Encoding`. A git client
compresses a request body over 1 KiB and declares `gzip`, and crossing that line is normal for an
incremental fetch: a first clone asks for one ref, sends `done` and stays small, while a Workspace
that has committed locally and now fetches the next mirror generation sends its `have` lines in
growing batches until one crosses. Handing the deflate bytes to `upload-pack` as a packet stream is
what the caller sees as a 500 — a server that looks broken rather than a request with a problem. The
repository surface does not have this fault because it delegates to `git http-backend`, which inflates
the body itself. An encoding this surface does not implement (`deflate`, say) and a body that claims
gzip without being it are both refused before git runs (`415` and `400`), for the same reason: passing
them on produces that 500, and a 500 tells the caller to retry what can never work. `MaxRequestBytes`
bounds both the bytes on the wire and the bytes after inflation: the first is what a caller cannot
send, the second what a caller's claimed encoding cannot expand into.

Repository policy and the hook path are applied to every git process through `GIT_CONFIG_*`
environment variables rather than stored in repositories, so a restart (new hook directory) or an
older repository cannot weaken them. The mirror surface's child environment is a closed set built from
nothing, with `GIT_OPTIONAL_LOCKS=0`, so a read can never modify the published tree. Neither path holds
a database transaction or an upstream credential.

Tests use the real git client, `git http-backend` and TLS with a policy double for the boundary matrix
(`service_test.go`, `server_test.go`, `fault_test.go`, `negotiation_test.go`); `surface_test.go` covers
serving only the surfaces a process was given and the routes not crossing between them; Core's
PostgreSQL tests cover the decision, and `integration/git_server_test.go` drives a real delivery from
Cloud state through the shipped command as the hook. `negotiation_test.go` reproduces the incremental
fetch that crosses the compression threshold with a real client (once per protocol version) and
compares the compressed body's answer with the plain one — which shows not merely that the 500 is gone
but that git read the packet stream the client meant. Long fetches are not re-authorized mid-transfer;
writes are, in pre-receive.
