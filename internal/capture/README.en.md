# internal/capture: a bound on what a child process may write into this process

[中文](README.md) | [English](README.en.md)

`internal/capture` does **one thing**: it keeps what a child process wrote in memory, never more than
the byte count the caller gives it, counts what it drops, and **always reports the write as
complete**. It exists for every path in this repository where a child process's output is decided by
somebody else's repository.

## Why it exists

The output of the Git invocations this repository starts is decided by the remote or by the
repository:

- `git fetch` forwards whatever the remote sends over the sideband to standard error (the `remote: …`
  lines), and the remote decides how much that is.
- `git ls-remote` prints **every ref** the server advertises to standard output, one line per ref.
- The diagnostics of a failed command are the remote's own words, and the remote is not a trusted
  party.

Handing those streams to a `strings.Builder` gives the memory budget to whoever chose the repository,
in a process that is running somebody else's work. That is the reason this package exists: it is a
**memory** problem, not a logging problem.

## Interface and invariants

- `Buffer{Limit}` is an `io.Writer` and may be handed to `exec.Cmd` directly. It keeps the first
  `Limit` bytes; the rest goes to `Dropped`.
- **A write always returns the full length and never returns an error.** This is not a detail but the
  easiest thing to get wrong here. A child's output reaches a `Buffer` like this: `exec` runs
  `io.Copy` in a goroutine over a pipe, and `io.Copy` treats a **short write as an error**. A copy that
  ends early then closes the read end of the pipe, and the child writing into it gets
  `EPIPE`/`SIGPIPE` — so "printed too much" turns into **killing the child**. The earlier implementation
  in `internal/gitserver` returned the truncated length, which is why any `upload-pack` writing more
  than 8 KiB of stderr was killed by its own bound. Returning the length the writer was given, and
  dropping what does not fit, is what makes the bound a property of **this process's memory** rather
  than a new failure mode for the child.
- `Truncated()` / `Dropped`: **whether the stream was cut is part of the result**, not something a
  caller infers from the text. Callers must do different things about it: a log line states the
  truncation in the text (`…(N bytes dropped)` at the end of `String()`), and a **fact** cannot be read
  out of a stream that was cut. Redaction is not one of these decisions: a caller redacts the whole
  capture and only then cuts it to the log's own window, and the bound is far above that window, so
  every byte that can reach a log has been redacted whatever the capture dropped.
- `Len()` reports how much the child wrote in total (kept plus dropped), which is what tells a short
  stream from a cut one.
- A `Limit` of zero keeps nothing — the honest reading of a bound of zero. Callers that want content
  set one.

## What this package does not claim

- It does **not** capture a complete output; that is its purpose. A caller that needs completeness
  owes a bound of its own.
- It does **not** redact: redaction needs the secret, and only the caller holds it (see
  `redactFailure` in `internal/cloneexec`).
- It does **not** stop the child from writing. The bound is about this process's memory only; a
  truncated stream looks like an entirely ordinary write to the child.

## Callers

- `internal/gitserver`: `upload-pack`'s standard error (`stderrLimit`, 8 KiB).
- `internal/cloneexec`: both streams of `runGit` (`captureLimit`, 64 KiB), which is also where "a
  stream that was cut is not a fact" is decided.
- `internal/mirrorpublish`: both streams of `run` (the same 64 KiB).

## Dependencies and tests

- Dependencies: the standard library only (`bytes`, `fmt`).
- `buffer_test.go` covers both shapes of the bound, the values of `Dropped`/`Len()`/`String()`, and —
  the regression case for the `SIGPIPE` defect above — that one write larger than the bound still
  reports the full length and that copying a stream larger than the bound through `io.Copy` finishes
  with the whole stream read and no error. Falsifiable: make `Write` return the truncated length again
  and that case fails with `short write`.
