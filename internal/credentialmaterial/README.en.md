# credentialmaterial: one task's credential material directory

[中文](README.md) | [English](README.en.md)

The Repo Executor writes the redeemed plaintext credential into a private directory, and the fixed
helper `git-credential-ora` reads it from there (ADR `repo-executor/0` D3). The two programs must
agree on the layout yet stay apart: the helper is on the path Git executes and links only this
package and repository normalization.

```text
<root>/                 deployment-owned
└── <random>/           one task's material, 0700
    ├── scope           0600, the normalized repository URL the credential may be presented to
    ├── username        0600
    └── password        0600
```

| API | Responsibility |
|---|---|
| `NewScope(repository)` | normalizes with `cloneauth.NormalizeRepository` and splits into `Scope{Host, Path}` |
| `Stage(root, scope, username, password)` | `Mkdir` a 0700 random directory; files `O_CREATE\|O_EXCL` 0600, each fsynced, then the directory and root fsynced; on failure removes what it created |
| `Material.Dir()` / `Material.Destroy()` | the material directory; removes it, confirms with `Lstat` it is gone, fsyncs the root; idempotent |
| `Sweep(root)` | at executor startup removes every entry under the root (crash leftovers) |
| `ReadScope(dir)` / `ReadSecret(dir)` | the helper's side; the scope is read first and the secret only after a match |

Invariants:

- The scope must already be in normalized form; `Stage` refuses a scope the helper would read as a
  different spelling.
- Username and password are non-empty and free of CR, LF and NUL: the helper prints them as lines of
  the credential protocol, and a newline would inject another attribute.
- Every read checks that the directory is a 0700 directory and the file a 0600 regular file (symlinks
  are not followed); otherwise it returns `ErrTampered` and delivers nothing.
- Root and material directory must be absolute, clean paths; errors never contain material content.

Tests cover modes and layout, umask independence, refused input leaving nothing behind, `Destroy`'s
verification and failure report, `Sweep` (including not following symlinks), and tampered modes,
symlinks and scopes. Dependencies: standard library and `internal/cloneauth`.
