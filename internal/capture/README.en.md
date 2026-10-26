# capture: a bounded buffer for child-process output

[中文](README.md) | [English](README.en.md)

`Buffer{Limit}` is an `io.Writer` that may be handed to `exec.Cmd` directly: it keeps the first `Limit`
bytes and only counts the rest in `Dropped`. The output of the Git processes the Repo Executor starts
is decided by the remote (sideband text, ref advertisements, diagnostics); an unbounded buffer would
hand this process's memory to whoever chose the repository.

| Method | Meaning |
|---|---|
| `Write` | always returns the full length and never an error |
| `Len` | how much the child wrote in total (kept + dropped) |
| `Truncated` | whether the bound was exceeded |
| `Bytes` | the kept raw bytes (check `Truncated` before parsing) |
| `String` | the kept text, ending in `…(N bytes dropped)` when cut |

Invariants:

- **Writes never fail**: a child's output reaches the buffer through `io.Copy`; a short write ends the
  copy, closes the pipe and kills the child with `SIGPIPE`. Reporting the full length and dropping
  what does not fit keeps the bound a property of this process's memory, not a new failure mode for
  the child.
- **Truncation is part of the result**: truncated stdout is not adopted as a fact (ADR
  `repo-executor/0` D4); the caller decides what to do.
- No redaction: only the caller holds the secret, and it redacts the whole capture before cutting it
  to the report window.

Tests: under the bound, exactly at it, one write crossing it, writes after it is full, a zero bound,
and copying a stream larger than the bound through `io.Copy` without error (the `SIGPIPE`
regression). Dependencies: standard library only.
