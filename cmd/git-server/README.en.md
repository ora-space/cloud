# git-server: sandbox Git Server

[中文](README.md) | [English](README.en.md)

This process runs separately from Cloud's business server and composes `internal/gitserver`, Core and
PostgreSQL (proposed ADR `cloud/git-server/0`). One process, two surfaces: the **repository** surface
serves Project repositories to sandboxes and answers Cloud's read-only delivery-ref query; the
**mirror** surface serves published private-repository mirrors to a Workspace's Node. Each has its own
listeners and its own trust model, and neither can reach the other's routes — `internal/gitserver`
records why.

It reads only the Cloud database configuration, its own deployment TLS file references and the mirror
volume; it stores no authorization state. Startup validates migrations; shutdown drains every listener
before closing the pre-receive hook channel. Git also runs this binary as the pre-receive hook
(`-pre-receive`).

The repository surface is on by default, and that default is the fail-closed direction: a deployment
that did not turn it off must supply its material or be refused at startup.
`GIT_SERVER_REPOSITORY_ENABLED=false` is how a deployment with no git-access issuer anywhere says so,
and it relaxes nothing on the surface that remains. The mirror surface is off until `-mirror-root`
names the volume, and once on it refuses to start half-wired: the origin resolver, the identity used to
call it, and this surface's own TLS pair are required together.

| Environment | Default / responsibility |
|---|---|
| `GIT_SERVER_REPOSITORY_ENABLED` | `true`; when `false` this process serves no Project repositories — no listener is opened for them and no hook channel exists |
| `GIT_SERVER_REPOSITORY_ADDR` | `:8445`, smart HTTP for sandboxes; join only the sandbox network |
| `GIT_SERVER_REF_ADDR` | `:8446`, Cloud's read-only delivery-ref query and `/healthz`; join only the default network |
| `GIT_SERVER_REPOSITORY_ROOT` | `/var/lib/ora-git/repositories`, persistent volume of bare repositories |
| `GIT_SERVER_GIT_PROGRAM` | `git`; both surfaces run the same binary, resolved once by this process |
| `GIT_SERVER_CERTIFICATE_FILE` / `PRIVATE_KEY_FILE` | `/etc/ora-git-management/server.pem` / `server.key` |
| `GIT_SERVER_SANDBOX_CA_FILE` | `/etc/ora-git-management/ca.pem`, CA of git-access client certificates |
| `GIT_SERVER_CLOUD_CLIENT_CERT_FILE` | `/etc/ora-git-management/cloud-client.pem`, the one leaf allowed to query refs |
| `GIT_SERVER_HEALTH_SERVER_NAME` | `git-server`, TLS name `-healthcheck` verifies |

| Flag | Responsibility |
|---|---|
| `-config` | Cloud configuration, supplying the database both surfaces decide from |
| `-pre-receive` | Runs this program as the hook; invoked by `git` itself, never passed by hand |
| `-healthcheck` | Calls the ref listener's `/healthz` and verifies the server certificate; it fails outright when the repository surface is off, because that listener does not exist |
| `-mirror-addr` / `-mirror-root` | The mirror listener's address (default `127.0.0.1:18002`) and the mirror volume; naming the volume is what turns the surface on |
| `-origin-url` / `-identity-dir` / `-origin-server-name` | The origin resolver, the identity directory this service calls it with (`client.pem`/`client.key`/`ca.pem`/`origin.pem`), and the name its certificate must carry |
| `-mirror-tls-cert` / `-mirror-tls-key` | The certificate pair the mirror listener presents; both are required, because this surface's only client speaks https |
| `-mirror-timeout` / `-origin-timeout` | Bounds on one clone and on one origin resolution |
| `-max-mirror-bytes` / `-max-volume-bytes` | Bounds on one mirror and on the whole volume; reaching one fails that mirror rather than publishing a partial one |
| `-mirror-grace` | How long an unreferenced mirror is kept before it may be reclaimed |
| `-mirror-reclaim` / `-mirror-reclaim-interval` | Whether the reclaim loop runs, and how often; off by default |

`-healthcheck` calls the ref listener's `/healthz` at its configured host (loopback when unset) and
verifies the server certificate against the deployment CA; it reads no private key. The image needs
`git` with `git http-backend` (Alpine: `git` and `git-daemon`), and startup fails without it — a
mirror-only deployment is not checked for it, because it runs no `http-backend`. Run package tests,
`task check`, `task test:race` and `task build`.
