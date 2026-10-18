# git-server: Git Server for Project repositories

[中文](README.md) | [English](README.en.md)

This process runs separately from Cloud's business server and composes `internal/gitserver`, Core
and PostgreSQL (proposed ADR `cloud/git-server/0`). It reads only the Cloud database configuration
and its own deployment TLS file references; it stores no authorization state. Startup validates
migrations; shutdown drains the three listeners before closing the pre-receive hook channel. Git also
runs this binary as the pre-receive hook (`-pre-receive`).

| Environment | Default / responsibility |
|---|---|
| `GIT_SERVER_REPOSITORY_ADDR` | `:8445`, smart HTTP for sandboxes; join only the sandbox network |
| `GIT_SERVER_SERVICE_REPOSITORY_ADDR` | `:8447`, smart HTTP for the Repo Executor; join only the internal service network |
| `GIT_SERVER_REF_ADDR` | `:8446`, Cloud's read-only ref query and `/healthz`; join only the default network |
| `GIT_SERVER_REPOSITORY_ROOT` | `/var/lib/ora-git/repositories`, persistent volume of bare repositories |
| `GIT_SERVER_GIT_PROGRAM` | `git` |
| `GIT_SERVER_CERTIFICATE_FILE` / `PRIVATE_KEY_FILE` | `/etc/ora-git-management/server.pem` / `server.key` |
| `GIT_SERVER_SANDBOX_CA_FILE` | `/etc/ora-git-management/ca.pem`, CA of git-access client certificates |
| `GIT_SERVER_EXECUTOR_CA_FILE` | `/etc/ora-git-management/repo-executor-ca.pem`, CA of Repo Executor service certificates, separate from the sandbox CA |
| `GIT_SERVER_CLOUD_CLIENT_CERT_FILE` | `/etc/ora-git-management/cloud-client.pem`, the one leaf allowed to query refs |
| `GIT_SERVER_HEALTH_SERVER_NAME` | `git-server`, TLS name `-healthcheck` verifies |

`-healthcheck` calls the ref listener's `/healthz` at its configured host (loopback when unset) and verifies the server certificate
against the deployment CA; it reads no private key. The image needs `git` with `git http-backend` (Alpine: `git` and `git-daemon`); startup fails without it.
Run package tests, `task check`, `task test:race` and `task build`.
