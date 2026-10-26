# gitpreset: acceptance fixtures for the Git Server contract

[中文](README.md) | [English](README.en.md)

The implementation of `cmd/git-server-preset`, for development and acceptance only. It stands in for
the external line (snapshots, Operations) or the internal line (Workspace branches) by writing the
repository volume and the Cloud database directly, deliberately bypassing the Git Server's
authorization, so it is never in the service image and no service process imports it.

| Files | Responsibility |
|---|---|
| `preset.go` | `Snapshot` (a deterministic synthetic history or an existing commit → snapshot ref, tracking ref, the Binding's unfinished sync advanced to `succeeded` — or a new `succeeded` sync — and the current snapshot, recorded in one transaction), `Ref`, `Operation` (a terminal sync state advances the unfinished sync when there is one) |
| `check.go` | `Check`: every ref is in a contract namespace and names a commit; upstream refs belong to the Project's Binding; snapshot refs name their target; tracking refs point at a snapshot; at most one unfinished sync; the current snapshot comes from a succeeded sync of the Binding and its ref on the Git Server names the recorded commit |

Usage errors are `UsageError`, contract problems are returned as `violations`, and both stay distinct
from a command that cannot run (database or git failure). Tests use real PostgreSQL
(`TEST_DATABASE_URL`) and real git.
