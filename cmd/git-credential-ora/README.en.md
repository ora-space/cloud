# cmd/git-credential-ora: the deployment-fixed Git credential helper

[English](README.en.md) | [中文](README.md)

`cmd/git-credential-ora` is the **deployment-fixed** Git credential helper that hands Git the token the
executor staged for one clone.

It is a Git credential helper in the protocol sense — Git runs it with `get`/`store`/`erase` and writes the
request on standard input — but it is **not** a credential store. It reads one file whose location the
executor chose, refuses to read anything else, and forgets everything the moment it exits.

## Usage

```
git-credential-ora <material-root> <execution-id> <get|store|erase>
```

configured by the executor through `-c credential.helper`. The program's path is fixed by the
**deployment** and its arguments by the **executor**; no part of a clone request reaches it. That split is
the point: the helper is on the path Git executes, so the only thing it must be trusted to do is read a file
it was told to read and print it in the protocol's shape. And every decision about whether the token may be
used at all was already made — the token does not exist on disk unless it was.

## Responsibilities and invariants

- **It reads one file**: the location comes from the executor's arguments and the content is guaranteed by
  `internal/credentialmaterial` (a 0600 regular file inside a 0700 directory). Missing material, widened
  permissions and non-regular files are all refused.
- **The repository scope is enforced at the point of handover**: `get` reads the `scope` file (the
  normalized repository identity, `host` + `path`) first, then reduces the request's `host` and `path`
  through the **same** normalization (`credentialmaterial.NormalizeScope`) and compares them exactly. The
  host is case-insensitive, the path is case-sensitive, and anything that does not match — another
  repository, another repository on the same host, a sub- or super-path, a case-differing path, a
  percent-encoded or query/fragment-carrying path, a request with no `path` — is refused.
  `http.followRedirects=false` and the protocol constraints already say Git will not follow a redirect;
  this makes the refusal happen where the secret would leave, so the guarantee does not rest on Git's
  behavior staying as documented. The request carries a `path` at all because the executor pins
  `credential.useHttpPath=true`; without it Git would report only the host and another repository on that
  host could borrow the scope.
- **`store` and `erase` do nothing**: Git calls `store` after a successful authentication and `erase` after
  a rejected one, and both are opportunities for a helper to persist or discard a credential. This helper
  does neither — the material belongs to one clone and the executor owns its lifetime.
- **The request is consumed before anything is judged**: every operation drains Git's request first. Git
  writes it and waits, so leaving it unread risks a broken pipe on Git's side; and reading it first means a
  malformed request cannot change what this program does, because no field of it is used (apart from the
  two fields the scope check compares, `host` and `path`; unknown keys are ignored rather than refused,
  since the request is Git's own output rather than caller input and a newer Git adding a key must not
  stop authentication).
- **Error messages carry no material**: Git shows a helper's stderr to the user and puts it in its own error
  output, which becomes part of the clone log. So an error names the failure and never the content, and the
  token appears only on the standard output's `password=` line.
- **The username is a fixed label**: `x-access-token`, the form GitHub documents for token authentication.
  It is deliberately **not** read from the material — a username is not a secret, and letting the file carry
  a second value would make the helper's output depend on more than the one thing it delivers.

## Dependencies and callers

- Depends on `internal/credentialmaterial` (the shared layout) and the standard library. It deliberately
  does **not** depend on `internal/cloneexec`: it must not link the executor's job logic.
- Called by Git alone. The executor fixes it, through `-c credential.helper`, as the **only** helper for
  that clone.

See the [cmd entry overview](../README.md), the
[materialization layout](../../internal/credentialmaterial/README.md) and the
[executor package](../../internal/cloneexec/README.md).
