# internal/processgroup: ending a child brings what it started with it

[中文](README.md) | [English](README.en.md)

`internal/processgroup` owns **one thing**: putting the child the caller started at the head of a new
process group of its own, and making the end of a context end **that group** rather than only that
process. It serves D13 of
`specs/decisions/node/repository/20261010-controlled-node-git-authentication-isolation.md` (crash,
timeout and retry cleanup) and the obligation that an ended execution leaves no process holding the
credential.

## Why it exists

The default cancellation of `exec.CommandContext` kills **the one process it started**. No Git
invocation in this repository is one process: `git fetch` starts a transport helper and an indexer,
`git push` starts a send-pack that brings up a receive side, `git upload-pack` starts a packer — and
every one of them **inherits the parent's pipes** and keeps running after the parent is killed.

The consequences come in two layers, and the second is a security one:

- Reliability and resources: a `Wait` can end up waiting for the descendant that inherited the pipes
  (when the writer is a `strings.Builder`, exec runs a copying goroutine for the pipe and `Wait` waits
  for it), so the "timeout" becomes advisory. `reapDelay` only puts a bound on that wait; it does
  **not** end the process, which runs until it decides otherwise.
- Security: in the credential executor the descendant holds the materialized Git Token **in its own
  memory**. The helper hands the token only to the process Git is running, so an execution that has
  ended while its descendant still runs is **a credential outliving the execution** — which is what
  this package is for.

A process group is addressed by the **negated pid**, and a signal to it reaches every member; that is
the whole mechanism. The group is the child's own and not the caller's, so nothing here can produce a
signal that reaches the executor itself or a sibling job.

## Interface and invariants

- `Isolate(command *exec.Cmd)`: makes `command` the head of a new process group (`Setpgid`) and
  replaces its `Cancel` with "kill the whole group". It must be called **before** `Start`: the group
  and the cancellation are both settled as the process is created.
- **A command with no context is returned untouched**: `Isolate` tells `exec.Command` from
  `exec.CommandContext` by `command.Cancel == nil`. Nothing would run the cancellation of a command
  with no context, so moving the child out of the caller's group (a shell, a service manager) would
  only cost it the whole-tree stop signal it should have received, in exchange for a signal nobody
  sends.
- **It replaces a `Cancel` the caller had set**: the two answer the same event, and "the caller wanted
  this process to end differently" has no reading under the credential argument — a child that is
  asked to stop and does not is the shape this package handles.
- **`SIGKILL` rather than `SIGTERM`**: this package runs only on the paths where the caller has
  **already decided the work is over** (a timeout, a cancelled request, a shutdown). A Git process
  asked politely can spend an unbounded time finishing a transfer it was told to abandon, and what is
  being protected is the credential in its descendants' memory, which Git has no shutdown path to
  clear first.
- **A group that is already gone is not an error**: the context can expire in the same instant the
  command exits on its own, and reporting `ESRCH` would turn a completed command into a failure.
  Wrapping `os.ErrProcessDone` is exactly what tells `exec.Cmd.Wait` to keep the command's own exit
  status.
- The package is **split by platform**: implemented on Unix and a **documented no-op** elsewhere
  (`processgroup_other.go`), so that a non-Unix build cannot compile against a guarantee it does not
  have.

## What this package does not claim

- **It does not set `Pdeathsig`.** That promises "the child dies with the parent", and in the
  deployment the container already gives that: the executor is PID 1 of its own container, and when
  PID 1 dies the kernel kills every process in that PID namespace. What this package solves is a
  descendant still running while the parent is **alive and has merely abandoned the work** — a case
  `Pdeathsig` does nothing about. On Linux its semantics are also "delivered when the **thread** that
  created the process exits" rather than "when the process exits", so in a Go program it would read as
  a guarantee it cannot give. A non-containerized deployment (a systemd unit, say) is bounded by the
  executor's own cancellation path — see "Callers" below.
- **It does not cover Windows**: Windows has no process group that can be addressed this way (its
  equivalent is a Job Object), and the three components that launch Git are deployed as Linux
  containers.

## Callers

Four places launch Git and all four need it; missing one leaves a path where a timed-out execution
still has a process holding the token:

- `internal/cloneexec`: `runGit` and `headCommit` (the clone, mirror preparation and push paths all go
  through `runGit`).
- `internal/mirrorpublish`: `run`.
- `internal/gitserver`: `startGit` (upload-pack / receive-pack; no credential on this path, but an
  abandoned request should not leave a packer running either).

The deployment's second guarantee lives in `ora-space/cluster`: the `clone-executor` container gets
`init: true`, so something real carries PID 1's job of reaping zombies.

## Dependencies and tests

- Dependencies: standard library only (`errors`, `os`, `os/exec`, `syscall`).
- `processgroup_unix_test.go` uses a **real process tree** (an `sh` that starts a background process
  and then waits) to measure two things: with `Isolate`, the descendant is **really gone** once the
  context ends (polling `kill(pid, 0)` until `ESRCH`, because a killed child is a zombie until it is
  reaped and a zombie still answers signal 0); and, as a **control**, the same program without
  `Isolate`, where the context kills the process the caller started and the descendant is still
  running. Falsifiable: making `Isolate` a no-op fails the first case and reports the surviving
  descendant's pid.
