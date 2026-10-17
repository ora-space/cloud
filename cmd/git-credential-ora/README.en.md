# git-credential-ora: the Repo Executor's fixed Git credential helper

[中文](README.md) | [English](README.en.md)

The executor configures it as the only helper for the external remote (ADR `repo-executor/0` D3):

```text
-c credential.helper= -c "credential.helper=/usr/local/bin/git-credential-ora <material-dir>" -c credential.useHttpPath=true
```

Git runs `git-credential-ora <material-dir> get|store|erase` with the request on stdin.

- `get`: prints `username=` and `password=` only when the request has `protocol=https` and
  `https://<host>/<path>`, normalized with `cloneauth.NormalizeRepository`, **equals** the staged scope.
  Differences in a `.git` suffix, a trailing `/`, host case or `:443` therefore compare equal; any
  other host, port or path (case, sub-path, parent path, `..`, percent-encoding), `http`, a missing
  path or a repeated attribute produces **no output at all**, which the protocol reads as "no
  credential". A redirect, a submodule or another repository on the same host never receives the
  plaintext.
- `store`, `erase`: do nothing and do not read the material.
- The scope is read first and the secret only after a match. Bad arguments or unreadable material exit
  with status 1; stderr names the failure and never contains material or request content.

Tests drive `run` directly: the match / mismatch matrix, a scope with a port, silent `store`/`erase`,
and failures with no output; a real `git credential fill` confirms the request shape Git actually
writes. Dependencies: `internal/credentialmaterial`, `internal/cloneauth`.
