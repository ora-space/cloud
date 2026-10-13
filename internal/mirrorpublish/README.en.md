# internal/mirrorpublish: the mirror writer

[English](README.en.md) | [中文](README.md)

`internal/mirrorpublish` is the writer and reclaimer for the internal Git Server's mirrors. The store
in [`internal/mirror`](../mirror/) answers whether a mirror may be read; this package answers what
should exist, which is a question only Cloud's own rows can answer.

## Responsibilities

- **Authorization** ([`references.go`](references.go)): `References.Binding` is one read of
  PostgreSQL. It returns every live Project with the upstream repository its rows currently name, and
  every mirror key Cloud's rows reference — a Workspace that is not deleted, carries a pinned commit,
  belongs to an active Project, and has a `clone_ready` execution in `clone_executions`. The key is
  derived from those rows and never from a caller.
- **The pass** ([`mirrorpublish.go`](mirrorpublish.go)): `Reconciler.Reconcile` withdraws, publishes
  and sweeps, in that order. `materialize` re-checks the key against Cloud's answer before it touches
  the store, so "no mirror is published that Cloud does not reference" holds for every path into it —
  a process that can write the mirror volume still cannot publish a mirror Cloud has not bound to a
  tenant, a repository and a clone execution (D2).
- **Withdrawal and reclaim**: a stored key whose Project now names a different upstream repository is
  withdrawn durably (D6's first trigger); an unreferenced key whose Project still names the same
  repository is left to `mirror.Store.Sweep`, which reclaims it after the grace period. A pass that
  cannot read Cloud at all changes nothing and returns the error.
- **Fetching** ([`fetch.go`](fetch.go)): `GitFetcher` is the only component that reaches the network.
  It runs one hardened `git fetch` — the same closed protocol set, redirect refusal and fixed
  credential helper the credentialed clone uses — fetches one branch into exactly one ref
  (`refs/heads/ora-mirror`), and reads the commit back out of the repository so the store's
  verification compares two independently obtained values. That ref has to be a branch under
  `refs/heads`: the Node clones with `git clone --branch <name>` and then requires `refs/heads/<name>`
  to exist, `HEAD` to be symbolic to it, and `refs/remotes/origin/<name>` to name the same commit,
  while Git resolves `--branch` only as `refs/heads/<name>` or `refs/tags/<name>` — a single ref
  anywhere else makes every mirrored clone fail with "Remote branch not found". The name is fixed by
  the deployment (`mirrorpublish.MirrorBranch`) because a mirror is keyed by
  `(project, repository, commit)` and not by a ref: two different refs at one commit share a mirror,
  and naming the branch after whichever arrived first would make the second reader's checkout depend
  on the first reader's request.

## Invariants

- **Publishing is authorized by Cloud, not by the filesystem.** There is one entry point,
  `Reconcile`, and it takes Cloud's answer as its first act. `ErrNotAuthorized` is what a key Cloud's
  rows do not name gets.
- **A failed attempt is not a broken mirror.** A fetch that fails leaves the record in the failed
  state, which a reader refuses rather than serves, and the staged content is removed. The next pass
  retries the key.
- **A withdrawal is durable.** A project that returns to an upstream it previously left needs an
  operator action, because revocation is deliberately not something an ordinary pass can undo.

## Not implemented here

The writer has no production caller yet. `Fetcher` and `MaterialSource` are the consuming boundary:
a deployment wires them to the credentialed fetch the executor identity C performs, which redeems a
single-use authorization Cloud signed for one repository. Cloud does not issue mirror authorizations
today, so the writer cannot be deployed or measured end to end, and the credentialed clone it depends
on is fail-closed by design. Cloud's readiness confirmation already has its caller on the read side:
the Git Server asks the same store before it serves, and D7's "Cloud writes the clone input only
after the mirror is ready" is the part still missing.
