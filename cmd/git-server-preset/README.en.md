# git-server-preset: Git Server contract fixtures and assertions

[中文](README.md) | [English](README.en.md)

Acceptance only: it ships in the development (`devsetup`) image used by the cluster development stack
and never in the service image. It writes the Git Server's repository volume and the Cloud database
directly, bypassing the Git Server's authorization, so the internal and external lines can each stand
in for the other and accept end to end on their own (proposed follow-up decision "Git Server as each
Project's code hub", step 1 of its rollout). The logic is in
[`internal/gitpreset`](../../internal/gitpreset/README.en.md); this command only parses arguments,
wires dependencies and sets the exit code.

| Subcommand | Effect |
|---|---|
| `snapshot --project <id> (--commit <oid> \| --synthetic <n>)` | Writes `snapshots/<oid>` and the tracking ref, advances the Binding's unfinished sync (such as the one a new Project starts with) to `succeeded` or records a new `succeeded` sync when there is none, and makes it the current snapshot; `--synthetic` first creates a deterministic history of n commits |
| `ref --project <id> --ref <name> --commit <oid>` | Writes one ref of any namespace (for example a Workspace branch to publish from); the commit must already be in the repository |
| `operation --project <id> --type sync\|publish --state <state> [--error-code <code>]` | Writes one Operation in the given state for waiting, failure and deletion scenarios; terminal states get `finished_at`. A sync moves the Binding's unfinished sync (such as the one written when the Project was created) to the given state and writes a new row only when there is none, so every sync state can be preset; a publish is always a new row |
| `check --project <id>` | Contract assertion: do the Git Server's refs and Cloud's records satisfy the contract |

Each run prints one JSON line: `{"ok": true, ...}` on success; `{"ok": false, "violations": [...]}`
when `check` finds problems; `{"ok": false, "error": "..."}` when a command fails. Exit codes: 0
success, 1 assertion or command failure, 2 usage error.

The database comes from the Cloud configuration (`-config`, or `CLOUD_DATABASE_DSN`) and must be
migrated; the repository volume and git program are the Git Server's `GIT_SERVER_REPOSITORY_ROOT` and
`GIT_SERVER_GIT_PROGRAM`. An absent repository is created the Git Server's atomic way.
