# internal/credentialmaterial: the on-disk layout of credential materialization

[English](README.en.md) | [中文](README.md)

`internal/credentialmaterial` owns exactly **one** thing: the layout in which the credential-executing
identity materializes one Git token for the duration of one clone, and the answer to "where, and with
what permissions". It carries out D11 (C resolves and materializes on its own) and D13 (cleanup after a
crash, a timeout or a retry) of
`specs/decisions/node/repository/20261010-controlled-node-git-authentication-isolation.md`.

## Why it is a package of its own

**Two** programs have to agree about this layout, and they are deliberately kept apart: the executor
writes and destroys the material, and the deployment-fixed Git credential helper reads it. The helper is
a **separate binary** — it sits on the path Git executes, so the less it can do the better, and it must
never link the executor's job logic — but the moment the two disagree about where a token lives, the
isolation **fails open**. Sharing exactly this much and nothing more is what keeps them consistent
without turning the helper into a second executor.

The layout is the smallest thing that can work:

```
<root>/                     deployment-created, not writable by the workload identity
└── <execution-id>/         created by the credential-executing identity, 0700
    └── token               mode 0600, the plaintext HTTPS Git token
    └── scope               mode 0600, the single host the token may be presented to
```

Every path is derived from a **canonical lowercase UUID**, so a caller can make neither one clone's
material appear under another's name nor a path fragment escape the root. This package reads no
deployment configuration and decides no policy: whether the material may exist at all is the executor's
decision, and this package only makes "where" and "with what permissions" the same answer on both sides.

## Files

- `material.go`: `TokenFile`, `ScopeFile`, `DirectoryMode`/`FileMode`, three sentinel errors,
  `ValidExecutionID`, `ExecutionDirectory`, `TokenPath`, `Stage`, `Read`, `ReadScope`, `Destroy`,
  `Sweep`, plus the internal `writeFile`, `readMaterialFile`, `validateRoot` and `syncDirectory`.
- `material_test.go`: position-by-position assertions on the layout and its modes, the refusal paths,
  proof of destruction, and the sweep.

## Interface and invariants

- `TokenFile = "token"`: the file's content **is** the secret — no username, no header, no JSON envelope.
  A file whose whole content is the secret is the easiest thing to destroy and to verify destroyed, and
  it cannot smuggle a second value past a reader that expects one.
- `ScopeFile = "scope"`: the single host the token may be presented to. It is **not** a secret and it is
  **not** the token's authority — the executor fixes the repository scope from the authorization before
  anything is staged. It exists so the helper can refuse a request for another host without depending on
  Git's redirect and submodule behavior being what the documentation says: `http.followRedirects=false`
  expresses the intent, and this file enforces it at the one place the token is actually handed over.
- `DirectoryMode`/`FileMode` are constants rather than configuration: **the isolation argument depends on
  these two values exactly**, and a deployment that could weaken them could silently remove the boundary.
- `ValidExecutionID`: canonical lowercase UUIDs only (hyphens in fixed positions, no uppercase, no
  braces, no other spellings). `ExecutionDirectory` and `TokenPath` each validate again, and
  `validateRoot` requires an **absolute** root (`ErrUnsafeRoot`) — a relative root is interpreted against
  the process's working directory, which would make "where the root is" a runtime property of the caller
  rather than a property of the deployment.
- `Stage` refuses empty material and an **existing** directory (it never overwrites what someone else
  materialized), while `Read` refuses missing material, material whose permissions were widened, and any
  **non-regular file** (a symlink, device or FIFO is never read — that is precisely how a token would be
  read out of a pipe another process supplies), and treats empty content as `ErrAbsent` rather than as an
  empty token.
- `Destroy` removes the file and then **syncs the directory entry**, so a crash after a reported success
  cannot bring the material back (`syncDirectory(filepath.Clean(root))`). A successful `Destroy` is proof
  the material is gone, not a report that an unlink was issued.
- `Sweep`: removes **every** entry under the root and returns the count; the executor calls it before
  opening its listener, to clear what a crash left behind. It makes no "did this run create that
  directory?" judgement — by definition this run has not created anything yet.
- The three sentinel errors (`ErrInvalidExecutionID`, `ErrUnsafeRoot`, `ErrAbsent`) carry no part of the
  material; callers map them to their own reason codes.

## Dependencies and callers

- Depends only on the standard library (`errors`, `fmt`, `os`, `path/filepath`, `strings`).
- Called by `internal/cloneexec` (the executor: writes and destroys) and `cmd/git-credential-ora` (the
  helper: reads). Neither depends on the other, which is the reason this package exists.

## Tests

`material_test.go` covers: an execution id accepts only canonical lowercase UUIDs; the execution directory
stays directly under the root; `Stage` creates the documented layout with the documented modes (0700/0600)
and refuses empty material and an existing directory; `Read` refuses absent material, widened permissions
and non-regular files; blank content is treated as absent; `Destroy` removes the material and proves it
gone; no token bytes remain anywhere under the root after a `Destroy`; and `Sweep` removes every entry
under the root.
