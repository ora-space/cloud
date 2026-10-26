# processgroup: end the whole process tree on cancellation

[中文](README.md) | [English](README.en.md)

`Isolate(cmd)` is called before `Start`: `Setpgid` puts the child at the head of a new process group,
`Cancel` becomes `SIGKILL` to the whole group, and `WaitDelay` is set to 5 seconds (ADR
`repo-executor/0` D4).

Why: the default `exec.CommandContext` cancellation kills only the process it started, and Git is
never one process (transport helper, indexer, send-pack). Descendants inherit the pipes and, in the
Repo Executor, hold the redeemed credential in memory; killing only the parent would let the
credential outlive the task.

| File | Responsibility |
|---|---|
| `processgroup_unix.go` | Unix implementation: process group, `SIGKILL` to the group, `WaitDelay` |
| `processgroup_other.go` | other platforms: a documented no-op (the executor ships only as a Linux container) |

Invariants:

- The group is the child's own, so no signal can reach the caller or a sibling task.
- `SIGKILL`: it runs only after the caller decided the work is over, and Git has no shutdown path that
  clears a credential from memory first.
- A group that is already gone (`ESRCH`) yields `os.ErrProcessDone`, keeping the command's own exit
  status.
- `WaitDelay` bounds a descendant that escaped the group but still holds the pipes: after it, the pipes
  are closed and `Wait` returns.
- A command without a context (`Cancel == nil`) is left alone; a caller's `Cancel` and `WaitDelay` are
  replaced.
- No `Pdeathsig`: parent death is handled by the container's PID namespace (`init: true`).

Tests use a real process tree (`sh` starting a background `sleep`, then `wait`): after cancellation the
descendant of an isolated command is gone; in the control without `Isolate` it survives; plus the
configuration and the exit status of a finished command. Dependencies: standard library only.
