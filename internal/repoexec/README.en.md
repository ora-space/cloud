# repoexec: the Repo Executor

[中文](README.md) | [English](README.en.md)

The only Ora program that contacts external Git repositories (proposed ADR `cloud/repo-executor/0`; it fulfils the executor's
side of the Git Server code-hub contract). Stateless: no database, a fresh bare repository in a private temporary directory per
task, and the task directory and credential material destroyed afterwards. It reports observed facts only; Cloud decides the
Operation state.

| File | Responsibility |
|---|---|
| `task.go` | `Task` request, `Result` response (JSON as in the design doc), classifications and error codes |
| `authorize.go` | URL check, `cloneauth.Verify`, field-by-field comparison of task and authorization (purpose included: a probe grant never drives a publish), per-purpose timeout limits |
| `server.go` | mTLS task listener `POST /v1/tasks`: only a client whose single URI SAN is `spiffe://ora.local/cloud/repo-dispatcher`; strict body ≤ 64 KiB; `429 executor_busy` when every slot is taken (the task does not start) |
| `executor.go` | One task's life: verify → egress check → task directory and size monitor → redeem (only with a `credentialId`) → stage material → run → destroy material and directory on every path, panics included (`cleanupFailed` on failure) |
| `git.go` | Environment built from nothing, the hardened configuration (ADR D4 single list), certificate / CA / `Ora-Git-Operation-Id` scoped to the Git Server URL, process-group isolation, bounded capture, redact-then-truncate to 2 KiB |
| `sync.go` | ADR D5: optional base fetch of the tracking ref, hardened external fetch, `forced`, one `--atomic --force-with-lease` push without `+`; `stale info` and hook refusals handled separately |
| `publish.go` | ADR D6: source from the Git Server, fast-forward only, pre `ls-remote`, lease push, post `ls-remote` in its own window |
| `probe.go` | ADR D7: `ls-remote` for `upload-pack`; Go HTTP reading the first pkt-line then hanging up for `receive-pack` |
| `classify.go` | Porcelain and `ls-remote` parsing, stderr → error code, the ADR D8 publish table |
| `egress.go` | https only, port 443, no loopback / private / CGNAT / link-local / ULA / multicast / metadata addresses, internal or dotless names; allowlisted fixtures bypass; re-checked at dial time |
| `workdir.go` | Work root layout (`tasks/`, `credentials/`), startup sweep, 250 ms size monitor |
| `client.go` | Cloud's `Client.Run`: `NotDeliveredError` (request not fully written; the task certainly did not start), `LostError` (sent, no readable result), `RefusedError` (429/403/400; did not start); no timeout shorter than the task's own, no retries |

Git Server access: repository `<GIT_SERVER_URL>/<tenantId>/<projectId>.git`; `http.<GIT_SERVER_URL>/.sslCert|sslKey|sslCAInfo|extraHeader`
apply by URL prefix, so the external remote (another scheme/host/port) never gets the executor certificate or the Operation header,
and the egress policy always denies the Git Server and Credential Store host names. External credentials reach Git only through
`git-credential-ora <material-dir>`, scoped to the normalized repository URL; the task's repository URL is used as entered.

Classes: sync `applied` (replay included), `superseded` (lease `stale info`, with `observedOid`; `applied` if the tracking ref already
holds this commit and its snapshot exists), `failed(git_server_rejected)` (Git Server 403 or pre-receive refusal); publish per the ADR D8
table; a probe that read its advertisement is `applied`. The executor never re-pushes after `uncertain`.

Tests use real git: the real `gitserver` service repository listener (policy double, test binary as the pre-receive hook) and a TLS
external smart HTTP fixture (`git http-backend`, optional basic auth, modes `moved`, `drop`, `hang`, `hang-read`, `redirect`, plus a
refusing pre-receive). After every task they assert an empty work root and no secret in the result or the logs. Run
`go test -race ./internal/repoexec/...`.
